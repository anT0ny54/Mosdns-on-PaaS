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
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxDNSMessageBytes = 4096
	// minDNSMessageBytes is the size of a bare DNS header; anything shorter
	// cannot be a DNS message, so it is rejected before reaching MosDNS.
	minDNSMessageBytes       = 12
	maxHeaderBytes           = 8 * 1024
	defaultListen            = ":8080"
	defaultBackend           = "http://127.0.0.1:8081/dns-query"
	defaultRateLimit         = 240
	defaultRateWindow        = 60 * time.Second
	defaultRateLimitClients  = 65536
	defaultConcurrency       = 20
	defaultMaxActiveRequests = 512
	defaultUpstreamTO        = 3 * time.Second // must stay above mosdns.yaml servers[0].timeout
	defaultReadyProbeTimeout = 1 * time.Second
	defaultReadyCacheTTL     = 5 * time.Second
	readyFailCacheTTL        = 1 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultQueueWait         = 200 * time.Millisecond
	defaultReadTimeout       = 5 * time.Second
	defaultWriteTimeout      = 8 * time.Second
	defaultIdleTimeout       = 20 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

var version = "dev"

// dnsMessageBufferPool keeps the common <=4 KiB DoH request/response path
// allocation-light. A one-byte guard is reserved so oversize payloads can be
// detected without a second allocation. It stores *[]byte rather than []byte:
// putting a bare slice into a sync.Pool boxes the slice header into a fresh
// heap allocation on every Put.
var dnsMessageBufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, maxDNSMessageBytes+1)
		return &b
	},
}

// isBase64URLChar marks the unpadded URL-safe base64 alphabet (A-Z a-z 0-9 - _).
var isBase64URLChar = func() [256]bool {
	var table [256]bool
	for c := 'A'; c <= 'Z'; c++ {
		table[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		table[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		table[c] = true
	}
	table['-'] = true
	table['_'] = true
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
	responses3xxTotal     atomic.Uint64
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

	// backendURL is parsed once, on first use (see backendRequestURL).
	backendOnce  sync.Once
	backendBase  *url.URL // nil when backendURL is malformed
	backendPlain string   // backendBase.String(), used for POST

	// /readyz result cache. readyCacheTTL <= 0 disables caching (used by tests).
	readyCacheTTL time.Duration
	readyMu       sync.Mutex
	readyAt       time.Time
	readyOK       bool
}

// statusRecorder remembers the status code written by a handler so every
// response, including ones generated by the gateway itself, can be counted.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (g *gateway) countStatus(status int) {
	switch {
	case status >= 200 && status < 300:
		g.metrics.responses2xxTotal.Add(1)
	case status >= 300 && status < 400:
		g.metrics.responses3xxTotal.Add(1)
	case status >= 400 && status < 500:
		g.metrics.responses4xxTotal.Add(1)
	case status >= 500:
		g.metrics.responses5xxTotal.Add(1)
	}
}

// ready reports backend readiness. Probes are serialized and their result is
// cached briefly so that public /readyz traffic cannot be turned into an
// unbounded stream of uncached upstream DNS queries.
func (g *gateway) ready(ctx context.Context) bool {
	if g.readyCacheTTL <= 0 {
		return g.backendReady(ctx)
	}
	g.readyMu.Lock()
	defer g.readyMu.Unlock()

	if !g.readyAt.IsZero() {
		ttl := g.readyCacheTTL
		if !g.readyOK && ttl > readyFailCacheTTL {
			ttl = readyFailCacheTTL
		}
		if time.Since(g.readyAt) < ttl {
			return g.readyOK
		}
	}
	// Probe with a detached context so one client disconnecting cannot poison
	// the cached result for everyone else.
	ok := g.backendReady(context.Background())
	g.readyAt = time.Now()
	g.readyOK = ok
	return ok
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Count every response the gateway serves, whichever handler produced it.
	defer func() { g.countStatus(rec.status) }()

	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	case "/readyz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !g.ready(r.Context()) {
			writeText(w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	case "/metrics":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		g.serveMetrics(w)
	case "/dns-query":
		g.serveDNS(w, r)
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
	if !g.limiter.allow(rateLimitKey(clientIP)) {
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

	// Read and validate the request BEFORE taking a processing slot, so a
	// client that trickles its body cannot occupy one of the few backend
	// slots while it is still sending.
	var body []byte
	var err error
	var dnsParam string
	releaseBody := true
	if r.Method == http.MethodPost {
		if !isStrictDoHContentType(r.Header.Get("Content-Type")) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		bodyBuf := dnsMessageBufferPool.Get().(*[]byte)
		defer func() {
			// If the transport failed mid-request it may still be reading the
			// buffer, so it must not go back into the pool.
			if releaseBody {
				dnsMessageBufferPool.Put(bodyBuf)
			}
		}()
		body, err = readBoundedBody(r.Body, (*bodyBuf)[:0], int(g.maxRequestBytes))
		if err != nil {
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
		if int64(len(body)) > g.maxRequestBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if len(body) < minDNSMessageBytes {
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
	} else {
		dnsParam = r.URL.Query().Get("dns")
		if !isValidDoHGetParameter(dnsParam, g.maxRequestBytes) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
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

	// Forward exactly the "dns" value that was just validated, not the raw
	// query string, so a duplicate "dns" parameter or arbitrary extra
	// parameters can never reach the upstream unvalidated. A query string
	// already present in MOSDNS_DOH_URL is preserved and merged with.
	outURL, ok := g.backendRequestURL(dnsParam)
	if !ok {
		g.metrics.backendErrorTotal.Add(1)
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, outURL, reader)
	if err != nil {
		g.metrics.backendErrorTotal.Add(1)
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return
	}
	// Only the headers MosDNS needs are sent; client headers (cookies,
	// authorization, ...) are deliberately not forwarded.
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("X-Forwarded-For", clientIP)
	if r.Method == http.MethodPost {
		// ContentLength and GetBody were already derived from the
		// *bytes.Reader by NewRequestWithContext; setting ContentLength
		// again would only duplicate that value.
		req.Header.Set("Content-Type", "application/dns-message")
	}

	backendStart := time.Now()
	resp, err := g.client.Do(req)
	g.metrics.backendLatencySamples.Add(1)
	g.metrics.backendLatencyNanos.Add(uint64(time.Since(backendStart)))
	if err != nil {
		releaseBody = false
		if r.Context().Err() != nil {
			// The client went away; that is not a backend fault and nobody
			// is left to read a response.
			return
		}
		g.metrics.backendErrorTotal.Add(1)
		status := http.StatusBadGateway
		if isTimeoutError(err) {
			status = http.StatusGatewayTimeout
		}
		w.WriteHeader(status)
		return
	}
	defer resp.Body.Close()

	respBuf := dnsMessageBufferPool.Get().(*[]byte)
	defer dnsMessageBufferPool.Put(respBuf)
	respBody, err := readBoundedBody(resp.Body, (*respBuf)[:0], int(g.maxResponseBytes))
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
	} else {
		g.metrics.backendErrorTotal.Add(1)
		if resp.StatusCode < 400 {
			// Redirects are never followed and a 1xx/3xx is not a DNS answer;
			// do not relay a Location header from the loopback backend.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// backendRequestURL returns the MosDNS URL to call. For a GET, dns is the
// already-validated "dns" value and is merged into any query string that
// MOSDNS_DOH_URL already carries; for a POST dns is empty and the configured
// URL is used as is. ok is false when MOSDNS_DOH_URL is not an absolute URL
// with a host. The URL is parsed once rather than on every request.
func (g *gateway) backendRequestURL(dns string) (string, bool) {
	g.backendOnce.Do(func() {
		if u, err := url.Parse(g.backendURL); err == nil && u.Host != "" {
			g.backendBase = u
			g.backendPlain = u.String()
		}
	})
	if g.backendBase == nil {
		return "", false
	}
	if dns == "" {
		return g.backendPlain, true
	}
	u := *g.backendBase
	values := u.Query()
	values.Set("dns", dns)
	u.RawQuery = values.Encode()
	return u.String(), true
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

// isValidDoHGetParameter validates an unpadded URL-safe base64 "dns" value whose
// decoded size is between minDNSMessageBytes and maxBytes. It only inspects the
// alphabet and length; the payload itself is never decoded or allocated.
func isValidDoHGetParameter(value string, maxBytes int64) bool {
	if value == "" || maxBytes <= 0 || maxBytes > maxDNSMessageBytes {
		return false
	}
	if int64(len(value)) > int64(base64.RawURLEncoding.EncodedLen(int(maxBytes))) {
		return false
	}

	remainder := len(value) & 3
	if remainder == 1 {
		return false
	}
	decodedLen := len(value) / 4 * 3
	if remainder == 2 {
		decodedLen++
	} else if remainder == 3 {
		decodedLen += 2
	}
	if decodedLen < minDNSMessageBytes || int64(decodedLen) > maxBytes {
		return false
	}

	for i := 0; i < len(value); i++ {
		if !isBase64URLChar[value[i]] {
			return false
		}
	}
	return true
}

func isStrictDoHContentType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "application/dns-message")
}

func (g *gateway) backendReady(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, defaultReadyProbeTimeout)
	defer cancel()

	query, wantID := buildReadinessDNSQuery(g.probeSeq.Add(1))
	question := query[12:]
	probeURL, ok := g.backendRequestURL(base64.RawURLEncoding.EncodeToString(query))
	if !ok {
		g.metrics.readyFailTotal.Add(1)
		return false
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
	if err != nil || len(buf) > maxDNSMessageBytes || !isUsableDNSProbeResponse(buf, wantID, question) {
		g.metrics.readyFailTotal.Add(1)
		return false
	}

	g.metrics.readyPassTotal.Add(1)
	return true
}

func buildReadinessDNSQuery(seq uint64) ([]byte, uint16) {
	label := "ready-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(seq, 36)
	id := uint16(seq)
	query := make([]byte, 12, 64)
	query[0] = byte(id)
	query[1] = byte(id >> 8)
	query[2] = 0x01 // RD
	query[5] = 0x01 // QDCOUNT = 1

	for _, part := range []string{label, "example", "com"} {
		query = append(query, byte(len(part)))
		query = append(query, part...)
	}
	query = append(query, 0)
	query = append(query, 0, 1) // QTYPE A
	query = append(query, 0, 1) // QCLASS IN
	return query, id
}

// isUsableDNSProbeResponse reports whether msg is a usable reply to the
// probe that embedded wantID: it must echo that transaction ID (so a
// mismatched or stray response can't be mistaken for the live probe's
// result), be marked as a response, carry a non-failure RCODE, and actually
// echo the probe's question section — a bare 12-byte header with a matching
// ID is not proof that the backend answered this probe.
func isUsableDNSProbeResponse(msg []byte, wantID uint16, question []byte) bool {
	if len(msg) < 12+len(question) {
		return false
	}
	gotID := uint16(msg[0]) | uint16(msg[1])<<8
	if gotID != wantID {
		return false
	}
	if msg[2]&0x80 == 0 {
		return false
	}
	rcode := msg[3] & 0x0f
	if rcode != 0 && rcode != 3 {
		return false
	}
	// QDCOUNT must be exactly 1, matching the probe query.
	if msg[4] != 0 || msg[5] != 1 {
		return false
	}
	return bytes.Equal(msg[12:12+len(question)], question)
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

	// Render into one buffer and flush with a single write: /metrics is
	// scraped on a timer, and each WriteString would otherwise become a
	// separate response chunk on the wire.
	var b strings.Builder
	b.Grow(2048)

	b.WriteString("# HELP doh_gateway_requests_total Total DoH requests received.\n# TYPE doh_gateway_requests_total counter\ndoh_gateway_requests_total " + strconv.FormatUint(g.metrics.requestsTotal.Load(), 10) + "\n")
	b.WriteString("# HELP doh_gateway_rate_limited_total Requests rejected by the per-client rate limiter.\n# TYPE doh_gateway_rate_limited_total counter\ndoh_gateway_rate_limited_total " + strconv.FormatUint(g.metrics.rateLimitedTotal.Load(), 10) + "\n")
	b.WriteString("# HELP doh_gateway_active_rejected_total Requests rejected by the active request ceiling.\n# TYPE doh_gateway_active_rejected_total counter\ndoh_gateway_active_rejected_total " + strconv.FormatUint(g.metrics.activeRejectedTotal.Load(), 10) + "\n")
	b.WriteString("# HELP doh_gateway_queue_timeout_total Requests that exhausted queue wait.\n# TYPE doh_gateway_queue_timeout_total counter\ndoh_gateway_queue_timeout_total " + strconv.FormatUint(g.metrics.queueTimeoutTotal.Load(), 10) + "\n")
	b.WriteString("# HELP doh_gateway_backend_error_total Backend/protocol errors observed by the gateway.\n# TYPE doh_gateway_backend_error_total counter\ndoh_gateway_backend_error_total " + strconv.FormatUint(g.metrics.backendErrorTotal.Load(), 10) + "\n")
	b.WriteString(`# HELP doh_gateway_responses_total Responses grouped by status class.
# TYPE doh_gateway_responses_total counter
doh_gateway_responses_total{class="2xx"} ` + strconv.FormatUint(g.metrics.responses2xxTotal.Load(), 10) + `
doh_gateway_responses_total{class="3xx"} ` + strconv.FormatUint(g.metrics.responses3xxTotal.Load(), 10) + `
doh_gateway_responses_total{class="4xx"} ` + strconv.FormatUint(g.metrics.responses4xxTotal.Load(), 10) + `
doh_gateway_responses_total{class="5xx"} ` + strconv.FormatUint(g.metrics.responses5xxTotal.Load(), 10) + "\n")
	b.WriteString(`# HELP doh_gateway_ready_probe_total Readiness probes by result.
# TYPE doh_gateway_ready_probe_total counter
doh_gateway_ready_probe_total{result="ready"} ` + strconv.FormatUint(g.metrics.readyPassTotal.Load(), 10) + `
doh_gateway_ready_probe_total{result="not_ready"} ` + strconv.FormatUint(g.metrics.readyFailTotal.Load(), 10) + "\n")
	b.WriteString("# HELP doh_gateway_queue_wait_seconds_total Total processing-slot wait time.\n# TYPE doh_gateway_queue_wait_seconds_total counter\ndoh_gateway_queue_wait_seconds_total " + strconv.FormatFloat(float64(queueNanos)/float64(time.Second), 'f', 6, 64) + "\n")
	b.WriteString("# HELP doh_gateway_queue_wait_samples_total Requests that obtained a processing slot.\n# TYPE doh_gateway_queue_wait_samples_total counter\ndoh_gateway_queue_wait_samples_total " + strconv.FormatUint(queueSamples, 10) + "\n")
	b.WriteString("# HELP doh_gateway_queue_wait_seconds_avg Average processing-slot wait time for admitted requests.\n# TYPE doh_gateway_queue_wait_seconds_avg gauge\ndoh_gateway_queue_wait_seconds_avg " + strconv.FormatFloat(avgQueueSeconds, 'f', 6, 64) + "\n")
	b.WriteString("# HELP doh_gateway_backend_latency_seconds_total Total gateway-to-MosDNS request time.\n# TYPE doh_gateway_backend_latency_seconds_total counter\ndoh_gateway_backend_latency_seconds_total " + strconv.FormatFloat(float64(backendNanos)/float64(time.Second), 'f', 6, 64) + "\n")
	b.WriteString("# HELP doh_gateway_backend_latency_samples_total Requests sent to MosDNS.\n# TYPE doh_gateway_backend_latency_samples_total counter\ndoh_gateway_backend_latency_samples_total " + strconv.FormatUint(backendSamples, 10) + "\n")
	b.WriteString("# HELP doh_gateway_backend_latency_seconds_avg Average gateway-to-MosDNS request time.\n# TYPE doh_gateway_backend_latency_seconds_avg gauge\ndoh_gateway_backend_latency_seconds_avg " + strconv.FormatFloat(avgBackendSeconds, 'f', 6, 64) + "\n")

	_, _ = io.WriteString(w, b.String())
}

// extractClientIP returns the client address. When preferredHeader is set the
// deployment is assumed to sit behind a trusted reverse proxy, so that header
// (last hop) and then X-Real-IP are honoured; when it is empty neither header
// is trusted and only the socket peer address is used.
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
		if raw := r.Header.Get("X-Real-IP"); raw != "" {
			if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
				return ip.String()
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return "unknown"
}

// rateLimitKey maps a client address to its rate-limit bucket. IPv6 clients are
// grouped by /64 because a single subscriber normally controls a whole /64 and
// could otherwise mint unlimited keys (and exhaust the key table).
func rateLimitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String()
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
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
	values := h.Values("Connection")
	if len(values) == 0 {
		// Fast path: skip the map allocation in the common case where the
		// backend sent no Connection header. Reading a nil map is valid, so
		// copyResponseHeaders needs no special casing.
		return nil
	}
	tokens := make(map[string]bool)
	for _, value := range values {
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

// isTimeoutError reports whether err is a deadline/timeout failure. The
// transport's own ResponseHeaderTimeout is not context.DeadlineExceeded, so the
// net.Error interface has to be consulted as well.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// noRedirect makes the clients hand back 3xx responses instead of following
// them: the backend is a fixed loopback listener and must never steer the
// gateway (or a re-sent POST body) somewhere else.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
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

// newHTTPClient builds the gateway-to-MosDNS client. maxConns must match the
// processing concurrency so the transport never becomes a second, hidden queue.
func newHTTPClient(maxConns int, responseHeaderTimeout time.Duration) *http.Client {
	if maxConns < 1 {
		maxConns = defaultConcurrency
	}
	transport := &http.Transport{
		// The backend is a fixed in-container MosDNS listener; ambient
		// HTTP(S)_PROXY variables must never reroute it.
		Proxy:                 nil,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   maxConns,
		MaxConnsPerHost:       maxConns,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport, CheckRedirect: noRedirect}
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
	return &http.Client{Transport: transport, CheckRedirect: noRedirect}
}

func main() {
	listen := normalizeListenAddr(envString("PORT", defaultListen))
	backend := envString("MOSDNS_DOH_URL", defaultBackend)
	// Fail fast: a malformed backend URL would otherwise only surface as a 502
	// on every request.
	if u, err := url.Parse(backend); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Fatalf("invalid MOSDNS_DOH_URL %q: want an absolute http(s) URL", backend)
	}
	limit := envInt("RATE_LIMIT", defaultRateLimit)
	window := envDuration("RATE_WINDOW", defaultRateWindow)
	processingConcurrency := envInt("MAX_CONCURRENT_REQUESTS", defaultConcurrency)
	maxActiveRequests := envInt("MAX_ACTIVE_REQUESTS", defaultMaxActiveRequests)
	maxRateLimitClients := envInt("RATE_LIMIT_CLIENTS", defaultRateLimitClients)
	queueWait := envDuration("QUEUE_WAIT", defaultQueueWait)
	upstreamTimeout := envDuration("UPSTREAM_TIMEOUT", defaultUpstreamTO)
	// Client-supplied IP headers are spoofable, so they are trusted only on
	// explicit opt-in: set CLIENT_IP_HEADER to the header your trusted reverse
	// proxy sets (for example X-Forwarded-For). The default "none" ignores
	// X-Forwarded-For / X-Real-IP and keys rate limits on the socket peer.
	trustedIPHeader := envString("CLIENT_IP_HEADER", "none")
	if strings.EqualFold(trustedIPHeader, "none") {
		trustedIPHeader = ""
	}

	g := &gateway{
		backendURL:       backend,
		client:           newHTTPClient(processingConcurrency, upstreamTimeout),
		limiter:          newFixedWindowLimiterWithMaxKeys(limit, window, maxRateLimitClients),
		activeSlots:      make(chan struct{}, maxActiveRequests),
		processingSlots:  make(chan struct{}, processingConcurrency),
		queueWait:        queueWait,
		upstreamTimeout:  upstreamTimeout,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  trustedIPHeader,
		readyClient:      newReadinessClient(),
		readyCacheTTL:    defaultReadyCacheTTL,
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

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server failed: %v", err)
			os.Exit(1)
		}
	case <-sigCtx.Done():
		stop()
		log.Printf("shutdown signal received, draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown incomplete: %v", err)
			_ = server.Close()
		}
	}
}
