package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxDNSMessageBytes       = 4096
	maxHeaderBytes           = 8 * 1024
	defaultListen            = ":8080"
	defaultBackend           = "http://127.0.0.1:8081/dns-query"
	defaultRateLimit         = 240
	defaultRateWindow        = 60 * time.Second
	defaultRateLimitClients  = 65536
	defaultConcurrency       = 20
	defaultMaxActiveRequests = 512
	defaultUpstreamTO        = 2 * time.Second
	defaultReadyProbeTimeout = 1 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultQueueWait         = 200 * time.Millisecond
	defaultReadTimeout       = 8 * time.Second
	defaultWriteTimeout      = 8 * time.Second
	defaultIdleTimeout       = 20 * time.Second
)

var version = "dev"

var dohGetDecodeBufferPool = sync.Pool{
	New: func() interface{} {
		return new([maxDNSMessageBytes]byte)
	},
}

// dnsMessageBufferPool keeps the common <=4 KiB DoH request/response path
// allocation-light. A one-byte guard is reserved so oversize payloads can be
// detected without a second allocation.
var dnsMessageBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, maxDNSMessageBytes+1)
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
	maxKeys  int
	windowID int64
	counts   map[string]int
	now      func() time.Time
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return newFixedWindowLimiterWithMaxKeys(limit, window, defaultRateLimitClients)
}

func newFixedWindowLimiterWithMaxKeys(limit int, window time.Duration, maxKeys int) *fixedWindowLimiter {
	if limit < 1 {
		limit = defaultRateLimit
	}
	if window <= 0 {
		window = defaultRateWindow
	}
	if maxKeys < 1 {
		maxKeys = defaultRateLimitClients
	}
	return &fixedWindowLimiter{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		counts:  make(map[string]int),
		now:     time.Now,
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
	count, exists := l.counts[key]
	if !exists && len(l.counts) >= l.maxKeys {
		return false
	}
	if count >= l.limit {
		return false
	}
	l.counts[key] = count + 1
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

type gatewayMetrics struct {
	requestsTotal         atomic.Uint64
	rateLimitedTotal      atomic.Uint64
	activeRejectedTotal   atomic.Uint64
	queueTimeoutTotal     atomic.Uint64
	backendErrorTotal     atomic.Uint64
	responses2xxTotal     atomic.Uint64
	responses4xxTotal     atomic.Uint64
	responses5xxTotal     atomic.Uint64
	readyPassTotal        atomic.Uint64
	readyFailTotal        atomic.Uint64
	queueWaitSamples      atomic.Uint64
	queueWaitNanos        atomic.Uint64
	backendLatencySamples atomic.Uint64
	backendLatencyNanos   atomic.Uint64
}

type gateway struct {
	backendURL       string
	client           *http.Client
	limiter          *fixedWindowLimiter
	activeSlots      chan struct{}
	processingSlots  chan struct{}
	queueWait        time.Duration
	upstreamTimeout  time.Duration
	maxRequestBytes  int64
	maxResponseBytes int64
	trustedIPHeader  string
	readyClient      *http.Client
	metrics          gatewayMetrics
	probeSeq         atomic.Uint64
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
	case "/metrics":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		g.serveMetrics(w)
		return
	case "/dns-query":
		g.serveDNS(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

func (g *gateway) serveDNS(w http.ResponseWriter, r *http.Request) {
	g.metrics.requestsTotal.Add(1)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	clientIP := extractClientIP(r, g.trustedIPHeader)
	if !g.limiter.allow(clientIP) {
		g.metrics.rateLimitedTotal.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(g.limiter.retryAfterSeconds()))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limit exceeded\n")
		return
	}

	// Admit bursts up to a hard active-request ceiling, but do not execute all
	// admitted requests at once. Waiting handlers are cheap in Go and prevent a
	// burst from being turned into immediate 503 responses.
	select {
	case g.activeSlots <- struct{}{}:
		defer func() { <-g.activeSlots }()
	default:
		g.metrics.activeRejectedTotal.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "active request limit reached\n")
		return
	}

	queueStart := time.Now()
	queueTimer := time.NewTimer(g.queueWait)
	defer queueTimer.Stop()
	select {
	case g.processingSlots <- struct{}{}:
		wait := time.Since(queueStart)
		g.metrics.queueWaitSamples.Add(1)
		g.metrics.queueWaitNanos.Add(uint64(wait))
		defer func() { <-g.processingSlots }()
	case <-queueTimer.C:
		g.metrics.queueTimeoutTotal.Add(1)
		w.Header().Set("Retry-After", "1")
		writeText(w, http.StatusServiceUnavailable, "server busy\n")
		return
	case <-r.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.upstreamTimeout)
	defer cancel()

	var body []byte
	var err error
	var bodyBuf []byte
	var dnsParam string
	if r.Method == http.MethodPost {
		if !isStrictDoHContentType(r.Header.Get("Content-Type")) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		bodyBuf = dnsMessageBufferPool.Get().([]byte)
		bodyBuf = bodyBuf[:0]
		defer func() {
			dnsMessageBufferPool.Put(bodyBuf[:cap(bodyBuf)])
		}()
		body, err = readBoundedBody(r.Body, bodyBuf, int(g.maxRequestBytes))
		if err != nil {
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
		if int64(len(body)) > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
	} else {
		dnsParam = r.URL.Query().Get("dns")
		if !isValidDoHGetParameter(dnsParam, g.maxRequestBytes) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	// Forward exactly the "dns" value that was just validated, not the raw
	// query string. r.URL.Query().Get only inspects the first "dns" value,
	// so blindly re-appending r.URL.RawQuery would let a second/duplicate
	// "dns" parameter (or arbitrary extra parameters) reach the upstream
	// completely unvalidated.
	outURL := g.backendURL
	if r.Method == http.MethodGet {
		outURL += "?dns=" + dnsParam
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
	req.Header.Del("Accept-Encoding")
	if r.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/dns-message")
		req.ContentLength = int64(len(body))
	}

	backendStart := time.Now()
	resp, err := g.client.Do(req)
	g.metrics.backendLatencySamples.Add(1)
	g.metrics.backendLatencyNanos.Add(uint64(time.Since(backendStart)))
	if err != nil {
		g.metrics.backendErrorTotal.Add(1)
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		w.WriteHeader(status)
		return
	}
	defer resp.Body.Close()

	respBuf := dnsMessageBufferPool.Get().([]byte)
	respBuf = respBuf[:0]
	defer func() {
		dnsMessageBufferPool.Put(respBuf[:cap(respBuf)])
	}()
	respBody, err := readBoundedBody(resp.Body, respBuf, int(g.maxResponseBytes))
	if err != nil {
		g.metrics.backendErrorTotal.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if int64(len(respBody)) > g.maxResponseBytes {
		g.metrics.backendErrorTotal.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !isStrictDoHContentType(resp.Header.Get("Content-Type")) {
			g.metrics.backendErrorTotal.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	} else if resp.StatusCode >= 400 {
		g.metrics.backendErrorTotal.Add(1)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		g.metrics.responses2xxTotal.Add(1)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		g.metrics.responses4xxTotal.Add(1)
	case resp.StatusCode >= 500:
		g.metrics.responses5xxTotal.Add(1)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if resp.Header.Get("Content-Type") == "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.Header().Set("Content-Type", "application/dns-message")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func readBoundedBody(r io.Reader, buf []byte, maxBytes int) ([]byte, error) {
	if maxBytes < 1 || cap(buf) < maxBytes+1 {
		return nil, errors.New("invalid body buffer")
	}
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			return buf, nil
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		if n > 0 {
			buf = buf[:len(buf)+n]
			if len(buf) > maxBytes {
				return buf, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func isValidDoHGetParameter(value string, maxBytes int64) bool {
	if value == "" || maxBytes <= 0 || maxBytes > maxDNSMessageBytes {
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
	ctx, cancel := context.WithTimeout(parent, defaultReadyProbeTimeout)
	defer cancel()

	query := buildReadinessDNSQuery(g.probeSeq.Add(1))
	encoded := base64.RawURLEncoding.EncodeToString(query)
	probeURL := g.backendURL
	if u, err := url.Parse(g.backendURL); err != nil || u.Hostname() == "" {
		g.metrics.readyFailTotal.Add(1)
		return false
	} else {
		values := u.Query()
		values.Set("dns", encoded)
		u.RawQuery = values.Encode()
		probeURL = u.String()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		g.metrics.readyFailTotal.Add(1)
		return false
	}
	req.Header.Set("Accept", "application/dns-message")

	client := g.readyClient
	if client == nil {
		client = g.client
	}
	if client == nil {
		g.metrics.readyFailTotal.Add(1)
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		g.metrics.readyFailTotal.Add(1)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isStrictDoHContentType(resp.Header.Get("Content-Type")) {
		g.metrics.readyFailTotal.Add(1)
		return false
	}

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessageBytes+1))
	if err != nil || len(buf) > maxDNSMessageBytes || !isUsableDNSProbeResponse(buf) {
		g.metrics.readyFailTotal.Add(1)
		return false
	}

	g.metrics.readyPassTotal.Add(1)
	return true
}

func buildReadinessDNSQuery(seq uint64) []byte {
	label := "ready-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(seq, 36)
	query := make([]byte, 12, 64)
	query[0] = byte(seq)
	query[1] = byte(seq >> 8)
	query[2] = 0x01 // RD
	query[5] = 0x01 // QDCOUNT = 1

	for _, part := range []string{label, "example", "com"} {
		query = append(query, byte(len(part)))
		query = append(query, part...)
	}
	query = append(query, 0)
	query = append(query, 0, 1) // QTYPE A
	query = append(query, 0, 1) // QCLASS IN
	return query
}

func isUsableDNSProbeResponse(msg []byte) bool {
	if len(msg) < 12 {
		return false
	}
	if msg[2]&0x80 == 0 {
		return false
	}
	rcode := msg[3] & 0x0f
	return rcode == 0 || rcode == 3
}

func (g *gateway) serveMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	queueSamples := g.metrics.queueWaitSamples.Load()
	queueNanos := g.metrics.queueWaitNanos.Load()
	backendSamples := g.metrics.backendLatencySamples.Load()
	backendNanos := g.metrics.backendLatencyNanos.Load()
	avgQueueSeconds := 0.0
	if queueSamples != 0 {
		avgQueueSeconds = float64(queueNanos) / float64(queueSamples) / float64(time.Second)
	}
	avgBackendSeconds := 0.0
	if backendSamples != 0 {
		avgBackendSeconds = float64(backendNanos) / float64(backendSamples) / float64(time.Second)
	}

	_, _ = io.WriteString(w, "# HELP doh_gateway_requests_total Total DoH requests received.\n# TYPE doh_gateway_requests_total counter\ndoh_gateway_requests_total "+strconv.FormatUint(g.metrics.requestsTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_rate_limited_total Requests rejected by the per-client rate limiter.\n# TYPE doh_gateway_rate_limited_total counter\ndoh_gateway_rate_limited_total "+strconv.FormatUint(g.metrics.rateLimitedTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_active_rejected_total Requests rejected by the active request ceiling.\n# TYPE doh_gateway_active_rejected_total counter\ndoh_gateway_active_rejected_total "+strconv.FormatUint(g.metrics.activeRejectedTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_queue_timeout_total Requests that exhausted queue wait.\n# TYPE doh_gateway_queue_timeout_total counter\ndoh_gateway_queue_timeout_total "+strconv.FormatUint(g.metrics.queueTimeoutTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_backend_error_total Backend/protocol errors observed by the gateway.\n# TYPE doh_gateway_backend_error_total counter\ndoh_gateway_backend_error_total "+strconv.FormatUint(g.metrics.backendErrorTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, `# HELP doh_gateway_responses_total Responses grouped by status class.
# TYPE doh_gateway_responses_total counter
doh_gateway_responses_total{class="2xx"} `+strconv.FormatUint(g.metrics.responses2xxTotal.Load(), 10)+`
doh_gateway_responses_total{class="4xx"} `+strconv.FormatUint(g.metrics.responses4xxTotal.Load(), 10)+`
doh_gateway_responses_total{class="5xx"} `+strconv.FormatUint(g.metrics.responses5xxTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, `# HELP doh_gateway_ready_probe_total Readiness probes by result.
# TYPE doh_gateway_ready_probe_total counter
doh_gateway_ready_probe_total{result="ready"} `+strconv.FormatUint(g.metrics.readyPassTotal.Load(), 10)+`
doh_gateway_ready_probe_total{result="not_ready"} `+strconv.FormatUint(g.metrics.readyFailTotal.Load(), 10)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_queue_wait_seconds_total Total processing-slot wait time.\n# TYPE doh_gateway_queue_wait_seconds_total counter\ndoh_gateway_queue_wait_seconds_total "+strconv.FormatFloat(float64(queueNanos)/float64(time.Second), 'f', 6, 64)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_queue_wait_seconds_avg Average processing-slot wait time for admitted requests.\n# TYPE doh_gateway_queue_wait_seconds_avg gauge\ndoh_gateway_queue_wait_seconds_avg "+strconv.FormatFloat(avgQueueSeconds, 'f', 6, 64)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_backend_latency_seconds_total Total gateway-to-MosDNS request time.\n# TYPE doh_gateway_backend_latency_seconds_total counter\ndoh_gateway_backend_latency_seconds_total "+strconv.FormatFloat(float64(backendNanos)/float64(time.Second), 'f', 6, 64)+"\n")
	_, _ = io.WriteString(w, "# HELP doh_gateway_backend_latency_seconds_avg Average gateway-to-MosDNS request time.\n# TYPE doh_gateway_backend_latency_seconds_avg gauge\ndoh_gateway_backend_latency_seconds_avg "+strconv.FormatFloat(avgBackendSeconds, 'f', 6, 64)+"\n")
}

func extractClientIP(r *http.Request, preferredHeader string) string {
	if preferredHeader != "" {
		if values := r.Header.Values(preferredHeader); len(values) != 0 {
			value := values[len(values)-1]
			if comma := strings.LastIndexByte(value, ','); comma >= 0 {
				value = value[comma+1:]
			}
			if ip := net.ParseIP(strings.TrimSpace(value)); ip != nil {
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
	connectionTokens := connectionHeaderTokens(src)
	for k, values := range src {
		if isHopByHopHeader(k) || connectionTokens[strings.ToLower(k)] || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "X-Forwarded-For") {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	connectionTokens := connectionHeaderTokens(src)
	for k, values := range src {
		if isHopByHopHeader(k) || connectionTokens[strings.ToLower(k)] || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func connectionHeaderTokens(h http.Header) map[string]bool {
	tokens := make(map[string]bool)
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens[strings.ToLower(token)] = true
			}
		}
	}
	return tokens
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
	if port, err := strconv.Atoi(v); err == nil {
		if port >= 1 && port <= 65535 {
			return ":" + strconv.Itoa(port)
		}
		return defaultListen
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
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   defaultConcurrency,
		MaxConnsPerHost:       defaultConcurrency,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport}
}

func newReadinessClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       1,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   500 * time.Millisecond,
		ResponseHeaderTimeout: 750 * time.Millisecond,
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
	processingConcurrency := envInt("MAX_CONCURRENT_REQUESTS", defaultConcurrency)
	maxActiveRequests := envInt("MAX_ACTIVE_REQUESTS", defaultMaxActiveRequests)
	maxRateLimitClients := envInt("RATE_LIMIT_CLIENTS", defaultRateLimitClients)
	queueWait := envDuration("QUEUE_WAIT", defaultQueueWait)
	upstreamTimeout := envDuration("UPSTREAM_TIMEOUT", defaultUpstreamTO)
	trustedIPHeader := envString("CLIENT_IP_HEADER", "X-Forwarded-For")

	g := &gateway{
		backendURL:       backend,
		client:           newHTTPClient(),
		limiter:          newFixedWindowLimiterWithMaxKeys(limit, window, maxRateLimitClients),
		activeSlots:      make(chan struct{}, maxActiveRequests),
		processingSlots:  make(chan struct{}, processingConcurrency),
		queueWait:        queueWait,
		upstreamTimeout:  upstreamTimeout,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  trustedIPHeader,
		readyClient:      newReadinessClient(),
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           g,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	log.Printf("doh-gateway %s listening on %s, backend=%s, rate=%d/%s, active=%d, processing=%d", version, listen, backend, limit, window, maxActiveRequests, processingConcurrency)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}
