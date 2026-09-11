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

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

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
	// Koyeb's public proxy supplies X-Forwarded-For. Use the first address,
	// which is the original client when the proxy appends rather than replaces.
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		parts := strings.Split(x, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	if x := r.Header.Get("X-Real-IP"); net.ParseIP(strings.TrimSpace(x)) != nil {
		return strings.TrimSpace(x)
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
	ratePeers := getenvInt("DOH_RATE_MAX_IPS", 4096)
	debugRequests := getenvBool("DEBUG_DOH_REQUESTS", false)
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

	target, err := url.Parse("http://" + backendAddr)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       180 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	connLim := newLimiter(max)
	rateLim := newRateLimiter(ratePerSecond, rateBurst, ratePeers)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		var lw *loggingResponseWriter
		if debugRequests {
			lw = &loggingResponseWriter{ResponseWriter: w}
			w = lw
			defer func() {
				status := lw.status
				if status == 0 {
					status = http.StatusOK
				}
				log.Printf("DOH DEBUG method=%s path=%s status=%d duration=%s client=%s", r.Method, r.URL.Path, status, time.Since(started).Round(time.Millisecond), clientIP(r))
			}()
		}
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

		// Keep RFC 8484 GET requests intact. MosDNS handles the dns=base64url
		// GET form itself. Preserve method, query, body and Content-Type.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("dns") == "" {
			http.Error(w, "missing dns parameter", http.StatusBadRequest)
			return
		}

		if !rateLim.allow(ip, time.Now()) {
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
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       300 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			state := connLim.registerConn(c)
			return context.WithValue(ctx, connStateKey{}, state)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				connLim.closeConn(c)
			}
		},
	}

	if max == 0 {
		log.Printf("DoH compatibility proxy listening on %s -> %s (per-IP connection cap=unlimited, rate=%g/s burst=%d max-IPs=%d, health=%s)", listenAddr, backendAddr, ratePerSecond, rateBurst, ratePeers, healthPath)
	} else {
		log.Printf("DoH compatibility proxy listening on %s -> %s (per-IP connection cap=%d, rate=%g/s burst=%d max-IPs=%d, health=%s)", listenAddr, backendAddr, max, ratePerSecond, rateBurst, ratePeers, healthPath)
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

func getenvBool(k string, d bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return d
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return d
	}
}

func getenvFloat(k string, d float64) float64 {
	v, err := strconv.ParseFloat(getenv(k, strconv.FormatFloat(d, 'f', -1, 64)), 64)
	if err != nil {
		return d
	}
	return v
}
