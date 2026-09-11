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
	healthPath := getenv("HEALTH_PATH", "/health")
	healthTimeout := time.Duration(getenvInt("HEALTH_BACKEND_TIMEOUT_MS", 1000)) * time.Millisecond
	if max < 0 {
		log.Fatalf("IP_CONN_LIMIT must be >= 0 (0 = unlimited)")
	}

	target, err := url.Parse("http://" + backendAddr)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Never inherit HTTP(S)_PROXY from the Koyeb/container environment.
	// The public DoH path must go directly to the private MosDNS listener;
	// an ambient proxy can otherwise create an unexpected network path.
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

	lim := newLimiter(max)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only expose the configured DoH path plus the health endpoint.
		// Rejecting arbitrary paths reduces accidental proxy use and prevents
		// this public listener from becoming a generic HTTP forwarder.
		if r.URL.Path != healthPath && r.URL.Path != getenv("DOH_PATH", "/dns-query") {
			http.NotFound(w, r)
			return
		}

		// Health is successful only when the private MosDNS listener is reachable.
		// This prevents Koyeb from marking an instance healthy while MosDNS is down.
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
		v := r.Context().Value(connStateKey{})
		state, _ := v.(*connState)
		if state == nil {
			http.Error(w, "internal limiter error", http.StatusInternalServerError)
			return
		}
		ip := clientIP(r)
		if !lim.allow(ip, state) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "per-IP connection limit exceeded", http.StatusTooManyRequests)
			return
		}
		// IMPORTANT: keep RFC 8484 GET requests intact. MosDNS v4.5.3's HTTP
		// listener implements the DoH GET form (dns=base64url) itself. Rewriting
		// Firefox GET requests to POST here can break provider validation and is
		// unnecessary. Only normalize the Accept header; preserve method, query,
		// body, and Content-Type exactly as supplied by Firefox/Cromite.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("dns") == "" {
			http.Error(w, "missing dns parameter", http.StatusBadRequest)
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
			state := lim.registerConn(c)
			return context.WithValue(ctx, connStateKey{}, state)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				lim.closeConn(c)
			}
		},
	}

	if max == 0 {
		log.Printf("DoH compatibility proxy listening on %s -> %s (per-IP connection cap=unlimited, health=%s)", listenAddr, backendAddr, healthPath)
	} else {
		log.Printf("DoH compatibility proxy listening on %s -> %s (per-IP connection cap=%d, health=%s)", listenAddr, backendAddr, max, healthPath)
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
