package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func clientIPAddr(r *http.Request) netip.Addr {
	// PaaS edge/proxy infrastructure appends the client address to XFF. Trust only
	// the final element of the final header line; if that value is malformed,
	// fall back to the actual peer rather than accepting an attacker-controlled
	// earlier XFF element.
	if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
		x := vals[len(vals)-1]
		if i := strings.LastIndexByte(x, ','); i >= 0 {
			x = x[i+1:]
		}
		if ip, err := netip.ParseAddr(strings.TrimSpace(x)); err == nil {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip
		}
	}
	return netip.Addr{}
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	backendAddr := getenv("BACKEND_ADDR", "127.0.0.1:18080")
	max := getenvInt("IP_CONN_LIMIT", 16)
	ratePerSecond := getenvFloat("DOH_RATE_LIMIT", 1.6666667)
	rateBurst := getenvInt("DOH_RATE_BURST", 16)
	ratePeers := getenvInt("DOH_RATE_MAX_IPS", 4096)
	globalRatePerSecond := getenvFloat("GLOBAL_RATE_LIMIT", 10)
	globalRateBurst := getenvInt("GLOBAL_RATE_BURST", 32)
	healthRatePerSecond := getenvFloat("HEALTH_RATE_LIMIT", 2)
	healthRateBurst := getenvInt("HEALTH_RATE_BURST", 4)
	globalHealthRatePerSecond := getenvFloat("GLOBAL_HEALTH_RATE_LIMIT", 10)
	globalHealthRateBurst := getenvInt("GLOBAL_HEALTH_RATE_BURST", 20)
	globalConnLimit := getenvInt("GLOBAL_CONN_LIMIT", 64)
	maxBodyBytes := int64(getenvInt("DOH_MAX_BODY_BYTES", 4096))
	healthTimeout := time.Duration(getenvInt("HEALTH_BACKEND_TIMEOUT_MS", 1000)) * time.Millisecond
	dohIdleTimeoutSeconds := getenvInt("DOH_IDLE_TIMEOUT", 120)
	if max < 0 {
		log.Fatalf("IP_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}
	if ratePerSecond < 0 {
		log.Fatalf("DOH_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if rateBurst <= 0 {
		log.Fatalf("DOH_RATE_BURST must be > 0")
	}
	if ratePeers < 1 || ratePeers > maxSourceStateHardCap {
		log.Fatalf("DOH_RATE_MAX_IPS must be 1-%d", maxSourceStateHardCap)
	}
	if globalRatePerSecond < 0 {
		log.Fatalf("GLOBAL_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if globalRateBurst <= 0 {
		log.Fatalf("GLOBAL_RATE_BURST must be > 0")
	}
	if healthRatePerSecond < 0 {
		log.Fatalf("HEALTH_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if healthRateBurst <= 0 {
		log.Fatalf("HEALTH_RATE_BURST must be > 0")
	}
	if globalHealthRatePerSecond < 0 {
		log.Fatalf("GLOBAL_HEALTH_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if globalHealthRateBurst <= 0 {
		log.Fatalf("GLOBAL_HEALTH_RATE_BURST must be > 0")
	}
	if globalConnLimit < 0 {
		log.Fatalf("GLOBAL_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}
	// Upper-bounded to the DNS wire-format maximum message size (also enforced
	// by entrypoint.sh's validate_uint_max). Enforcing it here too means the
	// binary is safe even if it is ever launched without that shell wrapper.
	const maxDNSMessageBytes = 65535
	if maxBodyBytes < 1 || maxBodyBytes > maxDNSMessageBytes {
		log.Fatalf("DOH_MAX_BODY_BYTES must be 1-%d", maxDNSMessageBytes)
	}
	if dohIdleTimeoutSeconds < 0 || dohIdleTimeoutSeconds > 3600 || (dohIdleTimeoutSeconds > 0 && dohIdleTimeoutSeconds < 6) {
		log.Fatalf("DOH_IDLE_TIMEOUT must be 0 or 6-3600 seconds (0 uses MosDNS v4.5.3's 10s default)")
	}

	target, err := url.Parse("http://" + backendAddr)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		MaxConnsPerHost:     16,
		// Must expire before MosDNS closes its own idle listener connections,
		// otherwise a reused connection can be reset mid-request and non-replayable
		// POSTs are answered with 502.
		IdleConnTimeout:       backendIdleTimeout(dohIdleTimeoutSeconds),
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Without this a hung MosDNS pins all MaxConnsPerHost slots until each
		// client gives up. Stay below the server WriteTimeout so the 502 from
		// ErrorHandler can still be written.
		ResponseHeaderTimeout: 12 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	guard := newPublicGuard(
		globalConnLimit,
		ratePeers,
		max,
		ratePerSecond,
		float64(rateBurst),
		globalRatePerSecond,
		float64(globalRateBurst),
		healthRatePerSecond,
		float64(healthRateBurst),
		globalHealthRatePerSecond,
		float64(globalHealthRateBurst),
	)
	dohPath := getenv("DOH_PATH", "/dns-query")
	healthPathValue := getenv("HEALTH_PATH", "/health")
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPathValue && r.URL.Path != dohPath {
			http.NotFound(w, r)
			return
		}

		ipAddr := clientIPAddr(r)

		if state, ok := r.Context().Value(connGuardKey{}).(*guardedConn); ok && !state.bindSource(ipAddr) {
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}

		now := time.Now()

		if r.URL.Path == healthPathValue {
			// Keep platform health checks independent from the public DoH request
			// budget. A separate per-IP plus global health budget still prevents
			// /health from becoming an unbounded backend-connect flood.
			if !guard.allowHealth(ipAddr, r.Host, now) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "health rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			// Health checks are infrequent and should never hold a global connection
			// slot open just because the client uses HTTP keep-alive. (The response
			// header is what closes a server-side connection; Request.Close is
			// ignored by net/http servers.)
			w.Header().Set("Connection", "close")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			c, err := net.DialTimeout("tcp", backendAddr, healthTimeout)
			if err != nil {
				http.Error(w, "mosdns unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = c.Close()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}

		// Apply the per-client budget first so a single abusive client is rejected
		// by its own bucket and cannot drain the shared global budget (which would
		// starve well-behaved clients). The global budget applies only to DNS
		// requests; health uses its own isolated limiters. Both run before body
		// buffering or other parsing so rejected floods cost as little CPU and
		// memory as possible.
		if !guard.allowDoH(ipAddr, r.Host, now) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// Strip client-controlled forwarding metadata before MosDNS sees the request.
		// ReverseProxy will append its request RemoteAddr to X-Forwarded-For, so
		// remove inbound forwarding headers and replace RemoteAddr with the already-
		// validated client IP. The backend only needs the trusted XFF value.
		for _, header := range []string{
			"Forwarded",
			"X-Forwarded-For",
			"X-Forwarded-Host",
			"X-Forwarded-Proto",
			"X-Real-IP",
		} {
			r.Header.Del(header)
		}
		if ipAddr.IsValid() {
			r.RemoteAddr = net.JoinHostPort(ipAddr.String(), "0")
		}

		// Reject malformed or oversized requests before they reach MosDNS.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			// DoH GET carries the DNS message in the query string. Reject a request
			// body instead of letting ReverseProxy stream an unused, potentially
			// unbounded body to MosDNS.
			if r.ContentLength != 0 {
				w.Header().Set("Connection", "close")
				http.Error(w, "GET request body not allowed", http.StatusBadRequest)
				return
			}
			dnsParam := r.URL.Query().Get("dns")
			if dnsParam == "" {
				http.Error(w, "missing dns parameter", http.StatusBadRequest)
				return
			}
			if len(dnsParam) > int(maxBodyBytes) {
				http.Error(w, "dns query too large", http.StatusRequestEntityTooLarge)
				return
			}
		} else {
			contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
			if contentType != "application/dns-message" {
				http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
				return
			}
			if r.ContentLength > maxBodyBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if r.ContentLength < 0 {
				// Chunked requests have no trusted Content-Length. Buffer at most
				// one byte beyond the configured cap so an oversized body is
				// rejected before ReverseProxy can stream any of it upstream.
				body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
				if err != nil {
					http.Error(w, "invalid request body", http.StatusBadRequest)
					return
				}
				if int64(len(body)) > maxBodyBytes {
					w.Header().Set("Connection", "close")
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			} else {
				r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			}
		}

		r.Header.Set("Accept", "application/dns-message")
		proxy.ServeHTTP(w, r)
	})

	rawListener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	listener := &guardedListener{Listener: rawListener, guard: guard}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if gc, ok := c.(*guardedConn); ok {
				return context.WithValue(ctx, connGuardKey{}, gc)
			}
			return ctx
		},
	}

	connLimitDesc := "unlimited"
	if globalConnLimit > 0 {
		connLimitDesc = strconv.Itoa(globalConnLimit)
	}
	log.Printf("DoH proxy listening on %s -> %s (per-IP conn=%d, per-IP rate=%g/s burst=%d max-IPs=%d, global rate=%g/s burst=%d, health rate=%g/s burst=%d, global health rate=%g/s burst=%d, global conn=%s, body<=%dB, health=%s)", listenAddr, backendAddr, max, ratePerSecond, rateBurst, ratePeers, globalRatePerSecond, globalRateBurst, healthRatePerSecond, healthRateBurst, globalHealthRatePerSecond, globalHealthRateBurst, connLimitDesc, maxBodyBytes, healthPathValue)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type connGuardKey struct{}

// backendIdleTimeout returns how long the proxy may keep an idle connection to
// MosDNS. MosDNS v4.5.3 normalizes listener idle_timeout <= 0 to its 10s default,
// so zero is treated as 10s here. The proxy then expires pooled connections at
// least 5s earlier, with a 90s upper bound. Explicit 1-5s values are rejected
// during proxy startup because they cannot provide the required margin.
func backendIdleTimeout(mosdnsIdleSeconds int) time.Duration {
	const (
		mosdnsDefaultIdle = 10 * time.Second
		margin            = 5 * time.Second
		maxIdle           = 90 * time.Second
		minIdle           = time.Second
	)
	if mosdnsIdleSeconds <= 0 {
		return mosdnsDefaultIdle - margin
	}
	idle := time.Duration(mosdnsIdleSeconds)*time.Second - margin
	if idle < minIdle {
		idle = minIdle
	}
	if idle > maxIdle {
		idle = maxIdle
	}
	return idle
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func getenvInt(k string, d int) int {
	v, err := strconv.Atoi(getenv(k, strconv.Itoa(d)))
	if err != nil {
		return d
	}
	return v
}

func getenvFloat(k string, d float64) float64 {
	v, err := strconv.ParseFloat(getenv(k, strconv.FormatFloat(d, 'f', -1, 64)), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return d
	}
	return v
}
