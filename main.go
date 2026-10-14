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
	"net/netip"
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
	// maxDNSMessageBytes bounds both DoH requests and MosDNS answers.
	maxDNSMessageBytes       = 4096
	minDNSMessageBytes       = 12
	maxHeaderBytes           = 8 * 1024
	defaultListen            = ":8080"
	defaultBackend           = "http://127.0.0.1:8081/dns-query"
	defaultRateLimit         = 100
	defaultRateWindow        = 60 * time.Second
	defaultRateLimitClients  = 65536
	defaultConcurrency       = 8
	defaultMaxActiveRequests = 16
	defaultUpstreamTO        = 2500 * time.Millisecond
	defaultReadyProbeTimeout = 1 * time.Second
	defaultReadyCacheTTL     = 5 * time.Second
	readyFailCacheTTL        = 1 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultQueueWait         = 100 * time.Millisecond
	defaultReadTimeout       = 5 * time.Second
	// defaultBodyReadTimeout bounds reading one POST body (at most 4 KiB) while
	// the request holds an active slot, so slow uploads cannot pin the slots.
	defaultBodyReadTimeout = 2 * time.Second
	defaultWriteTimeout    = 8 * time.Second
	defaultIdleTimeout     = 20 * time.Second
	// Requests are bounded by body read + QUEUE_WAIT + UPSTREAM_TIMEOUT, so a
	// short drain is enough and stays inside common 10 s stop timeouts.
	defaultShutdownTimeout = 4 * time.Second
	// limiterShrinkThreshold: above this many keys the limiter map is
	// reallocated at window rollover, because a cleared Go map keeps its memory.
	limiterShrinkThreshold = 4096
	// readinessProbeName is answered locally by the "readiness" arbitrary
	// plugin in mosdns.yaml (check-config.sh verifies that both agree), so the
	// probe checks MosDNS itself and never depends on the public upstreams.
	readinessProbeName = "ready.doh-gateway.invalid"
)

var version = "dev"
var dnsMessageBufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, maxDNSMessageBytes+1)
		return &b
	},
}
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

// fixedWindowLimiter expects limit >= 1, window > 0 and maxKeys >= 1; the
// gateway constructor applies the defaults.
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
	return &fixedWindowLimiter{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		counts:  make(map[string]int),
		now:     time.Now,
	}
}

func (l *fixedWindowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	windowID := l.now().UnixNano() / l.window.Nanoseconds()
	if windowID != l.windowID {
		l.windowID = windowID
		if len(l.counts) > limiterShrinkThreshold {
			l.counts = make(map[string]int)
		} else {
			clear(l.counts)
		}
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
	windowNanos := l.window.Nanoseconds()
	elapsed := l.now().UnixNano() % windowNanos
	if elapsed < 0 {
		elapsed += windowNanos
	}
	seconds := int((windowNanos - elapsed + int64(time.Second) - 1) / int64(time.Second))
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

// gatewayOptions configures newGateway. Zero values select the defaults,
// except ReadyCacheTTL, where zero disables the readiness cache.
type gatewayOptions struct {
	BackendURL       string
	Client           *http.Client
	ReadyClient      *http.Client
	RateLimit        int
	RateWindow       time.Duration
	RateLimitClients int
	MaxActive        int
	Processing       int
	QueueWait        time.Duration
	UpstreamTimeout  time.Duration
	BodyReadTimeout  time.Duration
	TrustedIPHeader  string
	ReadyCacheTTL    time.Duration
}

type gateway struct {
	client          *http.Client
	readyClient     *http.Client
	limiter         *fixedWindowLimiter
	activeSlots     chan struct{}
	processingSlots chan struct{}
	queueWait       time.Duration
	upstreamTimeout time.Duration
	bodyReadTimeout time.Duration
	trustedIPHeader string
	metrics         gatewayMetrics
	probeSeq        atomic.Uint64
	backendBase     *url.URL
	backendPlain    string
	backendSimple   bool
	readyCacheTTL   time.Duration
	readyMu         sync.Mutex
	readyAt         time.Time
	readyOK         bool
	readyInflight   chan struct{}
}

func orInt(v, def int) int {
	if v < 1 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// newGateway validates the backend URL once and applies every default, so
// the request path needs no lazy parsing or defensive fallbacks.
func newGateway(o gatewayOptions) (*gateway, error) {
	u, err := url.Parse(o.BackendURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid backend URL %q: want an absolute http(s) URL", o.BackendURL)
	}
	processing := orInt(o.Processing, defaultConcurrency)
	g := &gateway{
		client:          o.Client,
		readyClient:     o.ReadyClient,
		limiter:         newFixedWindowLimiterWithMaxKeys(orInt(o.RateLimit, defaultRateLimit), orDuration(o.RateWindow, defaultRateWindow), orInt(o.RateLimitClients, defaultRateLimitClients)),
		activeSlots:     make(chan struct{}, orInt(o.MaxActive, defaultMaxActiveRequests)),
		processingSlots: make(chan struct{}, processing),
		queueWait:       orDuration(o.QueueWait, defaultQueueWait),
		upstreamTimeout: orDuration(o.UpstreamTimeout, defaultUpstreamTO),
		bodyReadTimeout: orDuration(o.BodyReadTimeout, defaultBodyReadTimeout),
		trustedIPHeader: o.TrustedIPHeader,
		backendBase:     u,
		backendPlain:    u.String(),
		backendSimple:   u.RawQuery == "" && !u.ForceQuery && u.Fragment == "",
		readyCacheTTL:   o.ReadyCacheTTL,
	}
	if g.client == nil {
		g.client = newHTTPClient(processing)
	}
	if g.readyClient == nil {
		g.readyClient = newHTTPClient(1)
	}
	return g, nil
}

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

// Unwrap lets http.ResponseController reach the connection (read deadlines).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

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

// ready returns the (cached) readiness result. Concurrent callers share one
// in-flight probe; a waiter whose own client goes away stops waiting. A probe
// aborted by its caller's cancellation is not cached, and the waiters retry.
func (g *gateway) ready(ctx context.Context) bool {
	for {
		g.readyMu.Lock()
		if g.readyCacheTTL > 0 && !g.readyAt.IsZero() {
			ttl := g.readyCacheTTL
			if !g.readyOK && ttl > readyFailCacheTTL {
				ttl = readyFailCacheTTL
			}
			if time.Since(g.readyAt) < ttl {
				ok := g.readyOK
				g.readyMu.Unlock()
				return ok
			}
		}
		if g.readyInflight == nil {
			done := make(chan struct{})
			g.readyInflight = done
			g.readyMu.Unlock()
			ok := g.backendReady(ctx)
			g.readyMu.Lock()
			if ctx.Err() == nil {
				g.readyAt = time.Now()
				g.readyOK = ok
			}
			g.readyInflight = nil
			close(done)
			g.readyMu.Unlock()
			return ok
		}
		inflight := g.readyInflight
		g.readyMu.Unlock()
		select {
		case <-inflight:
			if g.readyCacheTTL <= 0 {
				// No cache to read the shared result from: probe again.
				continue
			}
		case <-ctx.Done():
			return false
		}
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() { g.countStatus(rec.status) }()
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		writeText(w, http.StatusOK, "ok\n")
	case "/readyz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		if !g.ready(r.Context()) {
			if r.Context().Err() != nil {
				// The client is gone: nobody to answer, nothing to count.
				return
			}
			writeText(w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		writeText(w, http.StatusOK, "ok\n")
	case "/metrics":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		g.serveMetrics(w)
	case "/dns-query":
		g.serveDNS(w, r)
	default:
		// Plain text like every other gateway-generated error; http.NotFound
		// would emit Go's HTML error page despite the nosniff header.
		writeText(w, http.StatusNotFound, "not found\n")
	}
}

func (g *gateway) serveDNS(w http.ResponseWriter, r *http.Request) {
	g.metrics.requestsTotal.Add(1)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, "GET, POST")
		return
	}
	clientIP := extractClientIP(r, g.trustedIPHeader)
	if !g.limiter.allow(rateLimitKey(clientIP)) {
		g.metrics.rateLimitedTotal.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(g.limiter.retryAfterSeconds()))
		writeText(w, http.StatusTooManyRequests, "rate limit exceeded\n")
		return
	}
	select {
	case g.activeSlots <- struct{}{}:
		defer func() { <-g.activeSlots }()
	default:
		g.metrics.activeRejectedTotal.Add(1)
		w.Header().Set("Retry-After", "1")
		writeText(w, http.StatusServiceUnavailable, "active request limit reached\n")
		return
	}
	var body []byte
	var dnsParam string
	if r.Method == http.MethodPost {
		if !isStrictDoHContentType(r.Header.Get("Content-Type")) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > maxDNSMessageBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		// A DNS message is at most 4 KiB: do not let a slow upload hold the
		// active slot for the whole server ReadTimeout. Not every
		// ResponseWriter supports deadlines (tests), so the error is ignored.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(g.bodyReadTimeout))
		// Read into a pooled buffer, then keep an exact-size copy (DNS queries
		// are typically < 100 bytes) and recycle the buffer at once, so the
		// transport never references pooled memory.
		buf := dnsMessageBufferPool.Get().(*[]byte)
		raw, err := readBoundedBody(r.Body, (*buf)[:0], maxDNSMessageBytes)
		if err == nil {
			body = bytes.Clone(raw)
		}
		dnsMessageBufferPool.Put(buf)
		if err == nil {
			// Clear the short deadline again: once the body is consumed the
			// server keeps a background read on the connection, and a
			// deadline expiring there (during the upstream wait) would cancel
			// the request context. The server sets fresh deadlines for the
			// next request on this connection.
			_ = rc.SetReadDeadline(time.Time{})
		}
		if err != nil {
			if isTimeoutError(err) {
				w.Header().Set("Connection", "close")
			}
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
		if len(body) > maxDNSMessageBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if len(body) < minDNSMessageBytes {
			writeText(w, http.StatusBadRequest, "invalid request body\n")
			return
		}
	} else {
		dnsParam = r.URL.Query().Get("dns")
		if !isValidDoHGetParameter(dnsParam) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	queueStart := time.Now()
	select {
	case g.processingSlots <- struct{}{}:
	default:
		queueTimer := time.NewTimer(g.queueWait)
		select {
		case g.processingSlots <- struct{}{}:
			queueTimer.Stop()
		case <-queueTimer.C:
			g.metrics.queueTimeoutTotal.Add(1)
			w.Header().Set("Retry-After", "1")
			writeText(w, http.StatusServiceUnavailable, "server busy\n")
			return
		case <-r.Context().Done():
			queueTimer.Stop()
			return
		}
	}
	g.metrics.queueWaitSamples.Add(1)
	g.metrics.queueWaitNanos.Add(uint64(time.Since(queueStart)))
	// The slot bounds work against MosDNS, not the client's download speed, so it
	// is released as soon as the answer has been read (before writing it to the
	// client); a client that reads slowly then cannot starve other requests.
	slotHeld := true
	releaseSlot := func() {
		if slotHeld {
			slotHeld = false
			<-g.processingSlots
		}
	}
	defer releaseSlot()
	ctx, cancel := context.WithTimeout(r.Context(), g.upstreamTimeout)
	defer cancel()
	var reqBody io.Reader
	if body != nil {
		// bytes.Reader lets net/http set ContentLength and a GetBody for
		// stale-connection retries; body is owned by this request alone.
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, g.backendRequestURL(dnsParam), reqBody)
	if err != nil {
		g.badUpstreamRequest(w)
		return
	}
	req.Header.Set("Accept", "application/dns-message")
	if clientIP.IsValid() {
		// Only a real address: MosDNS answers 400 to an unparseable
		// X-Forwarded-For when get_user_ip_from_header is enabled.
		req.Header.Set("X-Forwarded-For", clientIP.String())
	}
	if r.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/dns-message")
	}
	backendStart := time.Now()
	resp, err := g.client.Do(req)
	if err != nil {
		g.failBackend(w, r, err)
		return
	}
	defer resp.Body.Close()
	respBuf := dnsMessageBufferPool.Get().(*[]byte)
	defer dnsMessageBufferPool.Put(respBuf)
	respBody, err := readBoundedBody(resp.Body, (*respBuf)[:0], maxDNSMessageBytes)
	if err != nil {
		g.failBackend(w, r, err)
		return
	}
	// Take the elapsed time as soon as the body has been read, but only
	// record it once the response has passed every check below: a timeout,
	// an oversize body or an invalid/non-2xx answer must not feed the
	// backend-latency metrics.
	backendElapsed := time.Since(backendStart)
	if len(respBody) > maxDNSMessageBytes {
		g.metrics.backendErrorTotal.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// A 2xx answer must be a plausible DNS message: enforce the same
		// 12-byte minimum as on the request side, not just the content type.
		if !isStrictDoHContentType(resp.Header.Get("Content-Type")) || len(respBody) < minDNSMessageBytes {
			g.metrics.backendErrorTotal.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		g.metrics.backendLatencySamples.Add(1)
		g.metrics.backendLatencyNanos.Add(uint64(backendElapsed))
	} else {
		g.metrics.backendErrorTotal.Add(1)
		if resp.StatusCode < 400 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}
	// Answer fully read: free the processing slot before writing to the
	// client. The deferred Close stays as the safety net for early returns.
	_ = resp.Body.Close()
	releaseSlot()
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// badUpstreamRequest answers a request the gateway could not turn into a valid
// backend request: a backend error and a 502 with a short text body.
func (g *gateway) badUpstreamRequest(w http.ResponseWriter) {
	g.metrics.backendErrorTotal.Add(1)
	writeText(w, http.StatusBadGateway, "bad upstream request\n")
}

func (g *gateway) failBackend(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	g.metrics.backendErrorTotal.Add(1)
	status := http.StatusBadGateway
	if isTimeoutError(err) {
		status = http.StatusGatewayTimeout
	}
	w.WriteHeader(status)
}

func (g *gateway) backendRequestURL(dns string) string {
	if dns == "" {
		return g.backendPlain
	}
	if g.backendSimple {
		// dns is always validated base64url (or generated by the readiness probe), so it needs no escaping.
		return g.backendPlain + "?dns=" + dns
	}
	u := *g.backendBase
	values := u.Query()
	values.Set("dns", dns)
	u.RawQuery = values.Encode()
	return u.String()
}

// readBoundedBody reads r into buf (which must have capacity maxBytes+1) and
// returns at most maxBytes+1 bytes; a result longer than maxBytes means the
// body was too large.
func readBoundedBody(r io.Reader, buf []byte, maxBytes int) ([]byte, error) {
	if cap(buf) != maxBytes+1 {
		return nil, errors.New("invalid body buffer")
	}
	buf = buf[:0]
	for len(buf) < cap(buf) {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
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
	return buf, nil
}

func isValidDoHGetParameter(value string) bool {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(maxDNSMessageBytes) {
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
	if decodedLen < minDNSMessageBytes || decodedLen > maxDNSMessageBytes {
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

// probeFailed records a failed readiness probe and returns false. A probe
// aborted because the caller went away (parent canceled) says nothing about
// backend health, so it is not counted.
func (g *gateway) probeFailed(parent context.Context) bool {
	if parent.Err() == nil {
		g.metrics.readyFailTotal.Add(1)
	}
	return false
}

func (g *gateway) backendReady(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, defaultReadyProbeTimeout)
	defer cancel()
	query, wantID := buildReadinessDNSQuery(g.probeSeq.Add(1))
	question := query[12:]
	probeURL := g.backendRequestURL(base64.RawURLEncoding.EncodeToString(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return g.probeFailed(parent)
	}
	req.Header.Set("Accept", "application/dns-message")
	resp, err := g.readyClient.Do(req)
	if err != nil {
		return g.probeFailed(parent)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isStrictDoHContentType(resp.Header.Get("Content-Type")) {
		return g.probeFailed(parent)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessageBytes+1))
	if err != nil || len(buf) > maxDNSMessageBytes || !isUsableDNSProbeResponse(buf, wantID, question) {
		return g.probeFailed(parent)
	}
	g.metrics.readyPassTotal.Add(1)
	return true
}

// buildReadinessDNSQuery builds "A readinessProbeName" with the given ID.
func buildReadinessDNSQuery(seq uint64) ([]byte, uint16) {
	id := uint16(seq)
	query := make([]byte, 12, 64)
	query[0] = byte(id)
	query[1] = byte(id >> 8)
	query[2] = 0x01
	query[5] = 0x01
	for _, part := range strings.Split(readinessProbeName, ".") {
		query = append(query, byte(len(part)))
		query = append(query, part...)
	}
	query = append(query, 0)
	query = append(query, 0, 1)
	query = append(query, 0, 1)
	return query, id
}

// isUsableDNSProbeResponse accepts only the local answer: matching ID, QR set,
// NOERROR, one echoed question and at least one answer record. NXDOMAIN means
// the readiness rule is missing and the query leaked to the upstreams.
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
	if msg[3]&0x0f != 0 {
		return false
	}
	if msg[4] != 0 || msg[5] != 1 {
		return false
	}
	if msg[6] == 0 && msg[7] == 0 {
		return false
	}
	return bytes.Equal(msg[12:12+len(question)], question)
}

func (g *gateway) serveMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	m := &g.metrics
	var b strings.Builder
	b.Grow(4096)
	fmt.Fprintf(&b, "# HELP doh_gateway_build_info Gateway build information.\n# TYPE doh_gateway_build_info gauge\ndoh_gateway_build_info{version=%q} 1\n", version)
	fmt.Fprintf(&b, "# HELP doh_gateway_requests_total Total DoH requests received.\n# TYPE doh_gateway_requests_total counter\ndoh_gateway_requests_total %d\n", m.requestsTotal.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_rate_limited_total Requests rejected by the per-client rate limiter.\n# TYPE doh_gateway_rate_limited_total counter\ndoh_gateway_rate_limited_total %d\n", m.rateLimitedTotal.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_active_rejected_total Requests rejected by the active request ceiling.\n# TYPE doh_gateway_active_rejected_total counter\ndoh_gateway_active_rejected_total %d\n", m.activeRejectedTotal.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_queue_timeout_total Requests that exhausted queue wait.\n# TYPE doh_gateway_queue_timeout_total counter\ndoh_gateway_queue_timeout_total %d\n", m.queueTimeoutTotal.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_backend_error_total Backend/protocol errors observed by the gateway.\n# TYPE doh_gateway_backend_error_total counter\ndoh_gateway_backend_error_total %d\n", m.backendErrorTotal.Load())
	fmt.Fprintf(&b, `# HELP doh_gateway_responses_total Responses grouped by status class.
# TYPE doh_gateway_responses_total counter
doh_gateway_responses_total{class="2xx"} %d
doh_gateway_responses_total{class="3xx"} %d
doh_gateway_responses_total{class="4xx"} %d
doh_gateway_responses_total{class="5xx"} %d
`, m.responses2xxTotal.Load(), m.responses3xxTotal.Load(), m.responses4xxTotal.Load(), m.responses5xxTotal.Load())
	fmt.Fprintf(&b, `# HELP doh_gateway_ready_probe_total Readiness probes by result.
# TYPE doh_gateway_ready_probe_total counter
doh_gateway_ready_probe_total{result="ready"} %d
doh_gateway_ready_probe_total{result="not_ready"} %d
`, m.readyPassTotal.Load(), m.readyFailTotal.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_queue_wait_seconds_total Total processing-slot wait time.\n# TYPE doh_gateway_queue_wait_seconds_total counter\ndoh_gateway_queue_wait_seconds_total %.6f\n", float64(m.queueWaitNanos.Load())/float64(time.Second))
	fmt.Fprintf(&b, "# HELP doh_gateway_queue_wait_samples_total Requests that obtained a processing slot.\n# TYPE doh_gateway_queue_wait_samples_total counter\ndoh_gateway_queue_wait_samples_total %d\n", m.queueWaitSamples.Load())
	fmt.Fprintf(&b, "# HELP doh_gateway_backend_latency_seconds_total Total time of valid MosDNS answers (request sent until the 2xx DNS body was fully read and validated).\n# TYPE doh_gateway_backend_latency_seconds_total counter\ndoh_gateway_backend_latency_seconds_total %.6f\n", float64(m.backendLatencyNanos.Load())/float64(time.Second))
	fmt.Fprintf(&b, "# HELP doh_gateway_backend_latency_samples_total Valid MosDNS answers included in the latency total.\n# TYPE doh_gateway_backend_latency_samples_total counter\ndoh_gateway_backend_latency_samples_total %d\n", m.backendLatencySamples.Load())
	_, _ = io.WriteString(w, b.String())
}

// extractClientIP returns the client address, or the zero Addr if none can
// be determined. With a trusted header, the last entry of its last value is
// used (the address the trusted proxy appended), then X-Real-IP.
func extractClientIP(r *http.Request, preferredHeader string) netip.Addr {
	if preferredHeader != "" {
		if values := r.Header.Values(preferredHeader); len(values) != 0 {
			value := values[len(values)-1]
			if comma := strings.LastIndexByte(value, ','); comma >= 0 {
				value = value[comma+1:]
			}
			if ip, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
				return ip.Unmap()
			}
		}
		if raw := r.Header.Get("X-Real-IP"); raw != "" {
			if ip, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil {
				return ip.Unmap()
			}
		}
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// rateLimitKey keys IPv4 clients by address and IPv6 clients by /64.
func rateLimitKey(ip netip.Addr) string {
	if !ip.IsValid() {
		return "unknown"
	}
	if ip.Is4() {
		return ip.String()
	}
	p, err := ip.WithZone("").Prefix(64)
	if err != nil {
		return ip.String()
	}
	return p.Addr().String()
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// copyResponseHeaders relays backend headers except hop-by-hop headers,
// Content-Length (recomputed) and X-Content-Type-Options (set by the gateway).
func copyResponseHeaders(dst, src http.Header) {
	connectionTokens := connectionHeaderTokens(src)
	for k, values := range src {
		if isHopByHopHeader(k) || connectionTokens[strings.ToLower(k)] ||
			strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "X-Content-Type-Options") {
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

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

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
		log.Printf("ignoring invalid %s=%q (want an integer >= 1); using default %d", key, v, fallback)
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
		log.Printf("ignoring invalid %s=%q (want a positive duration such as 100ms or 2.5s); using default %s", key, v, fallback)
		return fallback
	}
	return d
}

// newHTTPClient returns a client for the loopback MosDNS backend. Request
// deadlines come from the per-request context, so no transport-level
// timeouts are set. The idle timeout (15 s) stays below MosDNS's listener
// idle_timeout (20 s) so the client always closes an idle connection first.
func newHTTPClient(maxConns int) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     15 * time.Second,
		DisableCompression:  true,
	}
	return &http.Client{Transport: transport, CheckRedirect: noRedirect}
}

// serverWriteTimeout keeps the server's write deadline above the longest time
// a request may legitimately spend: reading its body, waiting for a slot, and
// waiting for MosDNS. The body-read budget is included explicitly because the
// write deadline starts when the request headers are read.
func serverWriteTimeout(bodyReadTimeout, queueWait, upstreamTimeout time.Duration) time.Duration {
	if need := bodyReadTimeout + queueWait + upstreamTimeout + 2*time.Second; need > defaultWriteTimeout {
		return need
	}
	return defaultWriteTimeout
}

func main() {
	listen := normalizeListenAddr(os.Getenv("PORT"))
	backend := envString("MOSDNS_DOH_URL", defaultBackend)
	limit := envInt("RATE_LIMIT", defaultRateLimit)
	window := envDuration("RATE_WINDOW", defaultRateWindow)
	processing := envInt("MAX_CONCURRENT_REQUESTS", defaultConcurrency)
	maxActive := envInt("MAX_ACTIVE_REQUESTS", defaultMaxActiveRequests)
	queueWait := envDuration("QUEUE_WAIT", defaultQueueWait)
	upstreamTimeout := envDuration("UPSTREAM_TIMEOUT", defaultUpstreamTO)
	trustedIPHeader := envString("CLIENT_IP_HEADER", "none")
	if strings.EqualFold(trustedIPHeader, "none") {
		trustedIPHeader = ""
	}
	if processing > maxActive {
		log.Printf("warning: MAX_CONCURRENT_REQUESTS (%d) exceeds MAX_ACTIVE_REQUESTS (%d); requests are rejected before they can queue, so QUEUE_WAIT never applies", processing, maxActive)
	}
	if upstreamTimeout <= 2*time.Second {
		log.Printf("warning: UPSTREAM_TIMEOUT (%s) is not above the MosDNS server timeout (2s); slow lookups end in 504 instead of a SERVFAIL answer", upstreamTimeout)
	}
	g, err := newGateway(gatewayOptions{
		BackendURL:       backend,
		RateLimit:        limit,
		RateWindow:       window,
		RateLimitClients: envInt("RATE_LIMIT_CLIENTS", defaultRateLimitClients),
		MaxActive:        maxActive,
		Processing:       processing,
		QueueWait:        queueWait,
		UpstreamTimeout:  upstreamTimeout,
		TrustedIPHeader:  trustedIPHeader,
		ReadyCacheTTL:    defaultReadyCacheTTL,
	})
	if err != nil {
		log.Fatalf("MOSDNS_DOH_URL: %v", err)
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           g,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      serverWriteTimeout(defaultBodyReadTimeout, queueWait, upstreamTimeout),
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Printf("doh-gateway %s listening on %s, backend=%s, rate=%d/%s, active=%d, processing=%d", version, listen, backend, limit, window, maxActive, processing)
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		// Shutdown is only called in the other branch, so any return here
		// is a startup or listener failure.
		log.Printf("server failed: %v", err)
		os.Exit(1)
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
