package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const maxDoHResponseBytes = 65535

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
		if ip, err := netip.ParseAddr(strings.TrimSpace(x)); err == nil && ip.Zone() == "" {
			return ip.Unmap()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	backendPort := getenvInt("BACKEND_PORT", 18080)
	if backendPort < 1024 || backendPort > 65535 {
		log.Fatalf("BACKEND_PORT must be 1024-65535")
	}
	backendAddr := "127.0.0.1:" + strconv.Itoa(backendPort)
	perIPConnLimit := getenvInt("IP_CONN_LIMIT", 64)
	globalConnLimit := getenvInt("GLOBAL_CONN_LIMIT", 128)
	maxBodyBytes := int64(getenvInt("DOH_MAX_BODY_BYTES", 4096))
	healthTimeout := time.Duration(getenvInt("HEALTH_BACKEND_TIMEOUT_MS", 1000)) * time.Millisecond
	dohIdleTimeoutSeconds := getenvInt("DOH_IDLE_TIMEOUT", 120)
	if perIPConnLimit < 0 {
		log.Fatalf("IP_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}
	if globalConnLimit <= 0 {
		log.Fatalf("GLOBAL_CONN_LIMIT must be > 0")
	}
	if perIPConnLimit > 0 && perIPConnLimit > globalConnLimit {
		log.Fatalf("IP_CONN_LIMIT must be <= GLOBAL_CONN_LIMIT")
	}
	// Upper-bounded to the DNS wire-format maximum message size (also enforced
	// by entrypoint.sh's validate_uint_max). Enforcing it here too means the
	// binary is safe even if it is ever launched without that shell wrapper.
	if maxBodyBytes < 1 || maxBodyBytes > maxDoHResponseBytes {
		log.Fatalf("DOH_MAX_BODY_BYTES must be 1-%d", maxDoHResponseBytes)
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
		Proxy:              nil,
		DisableCompression: true,
		DialContext:        (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:  false,
		// The 0.1 vCPU profile benefits more from a small bounded backend pool
		// than from a large queue of simultaneously active MosDNS goroutines.
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     16,
		// Must expire before MosDNS closes its own idle listener connections,
		// otherwise a reused connection can be reset mid-request and non-replayable
		// POSTs are answered with 502.
		IdleConnTimeout:       backendIdleTimeout(dohIdleTimeoutSeconds),
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 500 * time.Millisecond,
		// Without this a hung MosDNS pins all MaxConnsPerHost slots until each
		// client gives up. Stay below the server WriteTimeout so the 502 from
		// ErrorHandler can still be written.
		ResponseHeaderTimeout: 8 * time.Second,
	}
	var lastProxyErrLogNS int64
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Surface backend failures in the logs, at most once per 10s so a hung
		// backend cannot turn every rejected request into a log line.
		// A client that simply went away is not a backend fault.
		now := time.Now().UnixNano()
		if last := atomic.LoadInt64(&lastProxyErrLogNS); !errors.Is(err, context.Canceled) &&
			now-last >= int64(10*time.Second) &&
			atomic.CompareAndSwapInt64(&lastProxyErrLogNS, last, now) {
			log.Printf("backend request failed (further errors suppressed for 10s): %v", err)
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = bufferDoHResponse

	guard := newPublicGuard(globalConnLimit, perIPConnLimit)
	dohPath := getenv("DOH_PATH", "/dns-query")
	healthPathValue := getenv("HEALTH_PATH", "/health")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPathValue && r.URL.Path != dohPath {
			http.NotFound(w, r)
			return
		}

		ipAddr := clientIPAddr(r)
		if !ipAddr.IsValid() {
			http.Error(w, "client identity unavailable", http.StatusBadRequest)
			return
		}

		if state, ok := r.Context().Value(connGuardKey{}).(*guardedConn); ok && !state.bindSource(ipAddr) {
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}

		if r.URL.Path == healthPathValue {
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
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    8 << 10,
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
	log.Printf("DoH proxy listening on %s -> %s (per-IP conn=%d, global conn=%s, body<=%dB, health=%s)", listenAddr, backendAddr, perIPConnLimit, connLimitDesc, maxBodyBytes, healthPathValue)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// bufferDoHResponse fully reads successful DoH responses before ReverseProxy
// starts writing the response to the client. This matters for chunked/unknown-
// length backend responses: a premature EOF must become a 502, not a seemingly
// successful HTTP 200 containing a truncated DNS message. Successful responses
// are normalized to an explicit Content-Length after validation.
func bufferDoHResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if contentType != "application/dns-message" {
		return fmt.Errorf("backend returned unsupported DoH content type %q", contentType)
	}
	if resp.ContentLength > maxDoHResponseBytes {
		return fmt.Errorf("backend DoH response exceeds %d bytes", maxDoHResponseBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDoHResponseBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("failed to read backend DoH response: %w", err)
	}
	if int64(len(body)) > maxDoHResponseBytes {
		return fmt.Errorf("backend DoH response exceeds %d bytes", maxDoHResponseBytes)
	}
	if resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength {
		return fmt.Errorf("backend DoH response length mismatch: got %d bytes, declared %d", len(body), resp.ContentLength)
	}
	msg := new(dns.Msg)
	if err := msg.Unpack(body); err != nil {
		return fmt.Errorf("invalid backend DoH DNS message: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
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
