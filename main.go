package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxDNSMessageBytes = 4096
	maxHeaderBytes     = 8 * 1024
	defaultListen      = ":8080"
	defaultBackend     = "http://127.0.0.1:8081/dns-query"
	defaultRateLimit   = 100
	defaultRateWindow  = 60 * time.Second
	defaultConcurrency = 64
	defaultUpstreamTO  = 8 * time.Second
)

var version = "dev"

var dohGetDecodeBufferPool = sync.Pool{
	New: func() interface{} {
		return new([maxDNSMessageBytes]byte)
	},
}

var base64URLDecodeTable = func() [256]uint8 {
	var table [256]uint8
	for i := range table {
		table[i] = 0xff
	}
	for i := byte(0); i < 26; i++ {
		table['A'+i] = i
		table['a'+i] = 26 + i
	}
	for i := byte(0); i < 10; i++ {
		table['0'+i] = 52 + i
	}
	table['-'] = 62
	table['_'] = 63
	return table
}()

// fixedWindowLimiter implements an exact N requests per aligned time window per key.
// It intentionally uses one map per active window so old keys are released without
// a background goroutine or unbounded per-client history.
type fixedWindowLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	windowID int64
	counts   map[string]int
	now      func() time.Time
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	if limit < 1 {
		limit = defaultRateLimit
	}
	if window <= 0 {
		window = defaultRateWindow
	}
	return &fixedWindowLimiter{
		limit:  limit,
		window: window,
		counts: make(map[string]int),
		now:    time.Now,
	}
}

func (l *fixedWindowLimiter) allow(key string) bool {
	now := l.now()
	windowID := now.UnixNano() / l.window.Nanoseconds()

	l.mu.Lock()
	defer l.mu.Unlock()

	if windowID != l.windowID {
		l.windowID = windowID
		l.counts = make(map[string]int)
	}
	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++
	return true
}

func (l *fixedWindowLimiter) retryAfterSeconds() int {
	now := l.now()
	windowNanos := l.window.Nanoseconds()
	if windowNanos <= 0 {
		return 1
	}
	elapsed := now.UnixNano() % windowNanos
	if elapsed < 0 {
		elapsed += windowNanos
	}
	remaining := windowNanos - elapsed
	seconds := int((remaining + int64(time.Second) - 1) / int64(time.Second))
	if seconds < 1 {
		return 1
	}
	return seconds
}

type gateway struct {
	backendURL       string
	client           *http.Client
	limiter          *fixedWindowLimiter
	concurrency      chan struct{}
	upstreamTimeout  time.Duration
	maxRequestBytes  int64
	maxResponseBytes int64
	trustedIPHeader  string
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	case "/readyz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !g.backendReady(r.Context()) {
			writeText(w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	case "/dns-query":
		g.serveDNS(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

func (g *gateway) serveDNS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	clientIP := extractClientIP(r, g.trustedIPHeader)
	if !g.limiter.allow(clientIP) {
		w.Header().Set("Retry-After", strconv.Itoa(g.limiter.retryAfterSeconds()))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limit exceeded\n")
		return
	}

	var body []byte
	var err error
	if r.Method == http.MethodPost {
		if !isStrictDoHContentType(r.Header.Get("Content-Type")) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		body, err = io.ReadAll(io.LimitReader(r.Body, g.maxRequestBytes+1))
		if err != nil {
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
		if int64(len(body)) > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
	} else {
		dnsParam := r.URL.Query().Get("dns")
		if !isValidDoHGetParameter(dnsParam, g.maxRequestBytes) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	select {
	case g.concurrency <- struct{}{}:
		defer func() { <-g.concurrency }()
	default:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "concurrency limit reached\n")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.upstreamTimeout)
	defer cancel()

	outURL := g.backendURL
	if r.Method == http.MethodGet && r.URL.RawQuery != "" {
		outURL += "?" + r.URL.RawQuery
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, outURL, reader)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Del("Host")
	req.Header.Del("Accept-Encoding")
	if r.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/dns-message")
		req.ContentLength = int64(len(body))
	}

	resp, err := g.client.Do(req)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		w.WriteHeader(status)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, g.maxResponseBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if int64(len(respBody)) > g.maxResponseBytes {
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !isStrictDoHContentType(resp.Header.Get("Content-Type")) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if resp.Header.Get("Content-Type") == "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.Header().Set("Content-Type", "application/dns-message")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func isValidDoHGetParameter(value string, maxBytes int64) bool {
	if value == "" || maxBytes <= 0 {
		return false
	}
	if int64(len(value)) > int64(base64.RawURLEncoding.EncodedLen(int(maxBytes))) {
		return false
	}

	fullGroups := len(value) / 4
	remainder := len(value) & 3
	if remainder == 1 {
		return false
	}
	decodedLen := fullGroups * 3
	if remainder == 2 {
		decodedLen++
	} else if remainder == 3 {
		decodedLen += 2
	}
	if decodedLen < 1 || int64(decodedLen) > maxBytes {
		return false
	}

	buf := dohGetDecodeBufferPool.Get().(*[maxDNSMessageBytes]byte)
	defer dohGetDecodeBufferPool.Put(buf)

	out := buf[:decodedLen]
	ti := 0
	oi := 0

	for ; ti+4 <= len(value); ti, oi = ti+4, oi+3 {
		a := base64URLDecodeTable[value[ti]]
		b := base64URLDecodeTable[value[ti+1]]
		c := base64URLDecodeTable[value[ti+2]]
		d := base64URLDecodeTable[value[ti+3]]
		if a == 0xff || b == 0xff || c == 0xff || d == 0xff {
			return false
		}
		out[oi] = a<<2 | b>>4
		out[oi+1] = b<<4 | c>>2
		out[oi+2] = c<<6 | d
	}

	switch remainder {
	case 2:
		a := base64URLDecodeTable[value[ti]]
		b := base64URLDecodeTable[value[ti+1]]
		if a == 0xff || b == 0xff {
			return false
		}
		out[oi] = a<<2 | b>>4
	case 3:
		a := base64URLDecodeTable[value[ti]]
		b := base64URLDecodeTable[value[ti+1]]
		c := base64URLDecodeTable[value[ti+2]]
		if a == 0xff || b == 0xff || c == 0xff {
			return false
		}
		out[oi] = a<<2 | b>>4
		out[oi+1] = b<<4 | c>>2
	}

	return true
}

func isStrictDoHContentType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "application/dns-message")
}

func (g *gateway) backendReady(parent context.Context) bool {
	u, err := url.Parse(g.backendURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return false
		}
	}

	ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func extractClientIP(r *http.Request, preferredHeader string) string {
	if preferredHeader != "" {
		if values := r.Header.Values(preferredHeader); len(values) != 0 {
			parts := strings.Split(strings.Join(values, ","), ",")
			last := strings.TrimSpace(parts[len(parts)-1])
			if ip := net.ParseIP(last); ip != nil {
				return ip.String()
			}
		}
	}
	if raw := r.Header.Get("X-Real-IP"); raw != "" {
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return "unknown"
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func copyRequestHeaders(dst, src http.Header) {
	for k, values := range src {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "X-Forwarded-For") {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, values := range src {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// normalizeListenAddr accepts the numeric PORT value supplied by Koyeb/most
// PaaS platforms (for example, "8080") as well as a normal Go listen address
// such as ":8080" or "127.0.0.1:8080".
func normalizeListenAddr(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return defaultListen
	}
	if port, err := strconv.Atoi(v); err == nil && port >= 1 && port <= 65535 {
		return ":" + strconv.Itoa(port)
	}
	return v
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 500 * time.Millisecond,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport}
}

func main() {
	listen := normalizeListenAddr(envString("PORT", defaultListen))
	backend := envString("MOSDNS_DOH_URL", defaultBackend)
	limit := envInt("RATE_LIMIT", defaultRateLimit)
	window := envDuration("RATE_WINDOW", defaultRateWindow)
	concurrency := envInt("MAX_CONCURRENT_REQUESTS", defaultConcurrency)
	upstreamTimeout := envDuration("UPSTREAM_TIMEOUT", defaultUpstreamTO)
	trustedIPHeader := envString("CLIENT_IP_HEADER", "X-Forwarded-For")

	g := &gateway{
		backendURL:       backend,
		client:           newHTTPClient(),
		limiter:          newFixedWindowLimiter(limit, window),
		concurrency:      make(chan struct{}, concurrency),
		upstreamTimeout:  upstreamTimeout,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  trustedIPHeader,
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           g,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	log.Printf("doh-gateway %s listening on %s, backend=%s, rate=%d/%s, concurrency=%d", version, listen, backend, limit, window, concurrency)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Sprintf("server failed: %v", err))
	}
}
