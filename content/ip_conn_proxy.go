package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type connState struct {
	mu      sync.Mutex
	ip      string
	counted bool
}

type limiter struct {
	mu      sync.Mutex
	max     int
	active  map[string]int
	connMap sync.Map // net.Conn -> *connState
}

func newLimiter(max int) *limiter {
	return &limiter{max: max, active: make(map[string]int)}
}

func (l *limiter) registerConn(c net.Conn) *connState {
	s := &connState{}
	l.connMap.Store(c, s)
	return s
}

func (l *limiter) closeConn(c net.Conn) {
	v, ok := l.connMap.LoadAndDelete(c)
	if !ok {
		return
	}
	s := v.(*connState)
	s.mu.Lock()
	ip, counted := s.ip, s.counted
	s.counted = false
	s.mu.Unlock()
	if counted && ip != "" {
		l.mu.Lock()
		if l.active[ip] > 1 {
			l.active[ip]--
		} else {
			delete(l.active, ip)
		}
		l.mu.Unlock()
	}
}

func (l *limiter) allow(ip string, s *connState) bool {
	if ip == "" {
		ip = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counted {
		return s.ip == ip
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.active[ip] >= l.max {
		return false
	}
	l.active[ip]++
	s.ip = ip
	s.counted = true
	return true
}

type globalConnLimiter struct {
	mu     sync.Mutex
	max    int
	active map[net.Conn]struct{}
}

func newGlobalConnLimiter(max int) *globalConnLimiter {
	return &globalConnLimiter{max: max, active: make(map[net.Conn]struct{})}
}

func (g *globalConnLimiter) add(c net.Conn) bool {
	if g.max <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.active) >= g.max {
		return false
	}
	g.active[c] = struct{}{}
	return true
}

func (g *globalConnLimiter) remove(c net.Conn) {
	if g.max <= 0 {
		return
	}
	g.mu.Lock()
	delete(g.active, c)
	g.mu.Unlock()
}

// rateBucket is a small standard-library-only token bucket. It deliberately
// avoids an external dependency so the tiny Koyeb image remains simple.
type rateBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	buckets  map[string]*rateBucket
	maxPeers int
}

func newRateLimiter(ratePerSecond float64, burst, maxPeers int) *rateLimiter {
	return &rateLimiter{
		rate:     ratePerSecond,
		burst:    float64(burst),
		buckets:  make(map[string]*rateBucket),
		maxPeers: maxPeers,
	}
}

func (r *rateLimiter) allow(ip string, now time.Time) bool {
	if r.rate <= 0 || r.burst <= 0 {
		return true
	}
	if ip == "" {
		ip = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[ip]
	if b == nil {
		if len(r.buckets) >= r.maxPeers {
			// The map is only a bounded anti-abuse cache. If it fills, evict one
			// old bucket; active clients immediately get a fresh burst allowance.
			var oldestIP string
			var oldest time.Time
			for k, v := range r.buckets {
				if oldestIP == "" || v.last.Before(oldest) {
					oldestIP, oldest = k, v.last
				}
			}
			if oldestIP != "" {
				delete(r.buckets, oldestIP)
			}
		}
		b = &rateBucket{tokens: r.burst, last: now}
		r.buckets[ip] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.rate
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	// Koyeb's edge normally provides the original client in X-Real-IP.
	// Prefer it because it is a single address and cannot be confused with
	// a client-supplied comma-separated chain. Fall back to the first valid
	// X-Forwarded-For address for deployments where only XFF is provided.
	if x := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(x) != nil {
		return x
	}
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		for _, part := range strings.Split(x, ",") {
			ip := strings.TrimSpace(part)
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	return "unknown"
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	backendAddr := getenv("BACKEND_ADDR", "127.0.0.1:18080")
	max := getenvInt("IP_CONN_LIMIT", 0)
	ratePerSecond := getenvFloat("DOH_RATE_LIMIT", 30)
	rateBurst := getenvInt("DOH_RATE_BURST", 60)
	ratePeers := getenvInt("DOH_RATE_MAX_IPS", 512)
	globalRatePerSecond := getenvFloat("GLOBAL_RATE_LIMIT", 40)
	globalRateBurst := getenvInt("GLOBAL_RATE_BURST", 80)
	globalConnLimit := getenvInt("GLOBAL_CONN_LIMIT", 128)
	maxBodyBytes := int64(getenvInt("DOH_MAX_BODY_BYTES", 4096))
	healthPath := getenv("HEALTH_PATH", "/health")
	healthTimeout := time.Duration(getenvInt("HEALTH_BACKEND_TIMEOUT_MS", 1000)) * time.Millisecond
	if max < 0 {
		log.Fatalf("IP_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}
	if ratePerSecond < 0 {
		log.Fatalf("DOH_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if rateBurst < 0 {
		log.Fatalf("DOH_RATE_BURST must be >= 0")
	}
	if ratePeers < 1 {
		log.Fatalf("DOH_RATE_MAX_IPS must be >= 1")
	}
	if globalRatePerSecond < 0 {
		log.Fatalf("GLOBAL_RATE_LIMIT must be >= 0 (0 = unlimited)")
	}
	if globalRateBurst < 0 {
		log.Fatalf("GLOBAL_RATE_BURST must be >= 0")
	}
	if globalConnLimit < 0 {
		log.Fatalf("GLOBAL_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}
	if maxBodyBytes < 1 {
		log.Fatalf("DOH_MAX_BODY_BYTES must be >= 1")
	}

	target, err := url.Parse("http://" + backendAddr)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          12,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       12,
		IdleConnTimeout:       180 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	connLim := newLimiter(max)
	globalConnLim := newGlobalConnLimiter(globalConnLimit)
	rateLim := newRateLimiter(ratePerSecond, rateBurst, ratePeers)
	globalRateLim := newRateLimiter(globalRatePerSecond, globalRateBurst, 1)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dohPath := getenv("DOH_PATH", "/dns-query")
		if r.URL.Path != healthPath && r.URL.Path != dohPath {
			http.NotFound(w, r)
			return
		}

		if r.URL.Path == healthPath {
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

		state := r.Context().Value(connStateKey{})
		connStateValue, _ := state.(*connState)
		if connStateValue == nil {
			http.Error(w, "internal limiter error", http.StatusInternalServerError)
			return
		}
		ip := clientIP(r)

		// Keep the connection cap disabled by default for Firefox/Fennec DoH.
		// The abuse control is request-rate based instead of connection based.
		if max > 0 && !connLim.allow(ip, connStateValue) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}

		// Only RFC 8484 GET/POST DoH requests are accepted. Reject malformed
		// or oversized requests before they reach MosDNS.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
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
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}

		now := time.Now()
		if !globalRateLim.allow("global", now) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "service rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		if !rateLim.allow(ip, now) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		r.Header.Set("Accept", "application/dns-message")
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       300 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			state := connLim.registerConn(c)
			return context.WithValue(ctx, connStateKey{}, state)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				if !globalConnLim.add(c) {
					_ = c.Close()
				}
			case http.StateClosed:
				globalConnLim.remove(c)
				connLim.closeConn(c)
			}
		},
	}

	if max == 0 {
		log.Printf("DoH proxy listening on %s -> %s (per-IP conn=unlimited, per-IP rate=%g/s burst=%d max-IPs=%d, global rate=%g/s burst=%d, global conn=%d, body<=%dB, health=%s)", listenAddr, backendAddr, ratePerSecond, rateBurst, ratePeers, globalRatePerSecond, globalRateBurst, globalConnLimit, maxBodyBytes, healthPath)
	} else {
		log.Printf("DoH proxy listening on %s -> %s (per-IP conn=%d, per-IP rate=%g/s burst=%d max-IPs=%d, global rate=%g/s burst=%d, global conn=%d, body<=%dB, health=%s)", listenAddr, backendAddr, max, ratePerSecond, rateBurst, ratePeers, globalRatePerSecond, globalRateBurst, globalConnLimit, maxBodyBytes, healthPath)
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type connStateKey struct{}

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
	if err != nil {
		return d
	}
	return v
}
