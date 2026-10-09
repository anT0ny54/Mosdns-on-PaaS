package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testDNSBody = "0123456789ab"

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return newFixedWindowLimiterWithMaxKeys(limit, window, defaultRateLimitClients)
}
func testGateway(backend string, limit int, concurrency int) *gateway {
	limiter := newFixedWindowLimiter(limit, 60*time.Second)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	return &gateway{
		backendURL:       backend,
		client:           newHTTPClient(concurrency, 2*time.Second),
		limiter:          limiter,
		activeSlots:      make(chan struct{}, 5000),
		processingSlots:  make(chan struct{}, concurrency),
		queueWait:        defaultQueueWait,
		upstreamTimeout:  2 * time.Second,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  "X-Forwarded-For",
	}
}
func TestNormalizeListenAddr(t *testing.T) {
	tests := map[string]string{
		"8080":           ":8080",
		":8080":          ":8080",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"[::]:8080":      "[::]:8080",
		"":               defaultListen,
		"0":              defaultListen,
		"65536":          defaultListen,
	}
	for in, want := range tests {
		if got := normalizeListenAddr(in); got != want {
			t.Errorf("normalizeListenAddr(%q)=%q, want %q", in, got, want)
		}
	}
}
func TestRateLimitPerClient(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 2, 10)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		r.Header.Set("X-Forwarded-For", "192.0.2.10")
		r.ContentLength = int64(len(testDNSBody))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want %d", i+1, w.Code, http.StatusOK)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	r.Header.Set("X-Forwarded-For", "192.0.2.10")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	r = httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	r.Header.Set("X-Forwarded-For", "192.0.2.11")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("second client got %d, want %d", w.Code, http.StatusOK)
	}
}
func TestRateLimiterClientCap(t *testing.T) {
	l := newFixedWindowLimiterWithMaxKeys(10, time.Minute, 2)
	l.now = func() time.Time { return time.Unix(120, 0) }
	if !l.allow("198.51.100.1") {
		t.Fatal("first client denied")
	}
	if !l.allow("198.51.100.2") {
		t.Fatal("second client denied")
	}
	if l.allow("198.51.100.3") {
		t.Fatal("third client exceeded key cap but was allowed")
	}
	if !l.allow("198.51.100.1") {
		t.Fatal("existing client denied after key cap reached")
	}
}
func TestDoHContentTypeAndSize(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	badType := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	badType.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, badType)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type: got %d", w.Code)
	}
	parameterizedType := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	parameterizedType.Header.Set("Content-Type", "application/dns-message; charset=utf-8")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, parameterizedType)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("parameterized content type: got %d, want %d", w.Code, http.StatusUnsupportedMediaType)
	}
	tooLarge := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(strings.Repeat("a", maxDNSMessageBytes+1)))
	tooLarge.Header.Set("Content-Type", "application/dns-message")
	tooLarge.ContentLength = maxDNSMessageBytes + 1
	w = httptest.NewRecorder()
	g.ServeHTTP(w, tooLarge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: got %d", w.Code)
	}
}
func TestGETBase64Validation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	invalid := httptest.NewRequest(http.MethodGet, "/dns-query?dns=not-base64!", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, invalid)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid GET: got %d", w.Code)
	}
}
func TestDoHGETParameterValidation(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString([]byte(testDNSBody))
	if !isValidDoHGetParameter(valid, maxDNSMessageBytes) {
		t.Fatal("valid raw base64url parameter rejected")
	}
	if isValidDoHGetParameter("AQ==", maxDNSMessageBytes) {
		t.Fatal("padded base64url parameter accepted")
	}
	if isValidDoHGetParameter(strings.Repeat("A", base64.RawURLEncoding.EncodedLen(maxDNSMessageBytes)+1), maxDNSMessageBytes) {
		t.Fatal("oversize base64url parameter accepted")
	}
	if isValidDoHGetParameter("AAAA", 1) {
		t.Fatal("decoded payload above max size accepted")
	}
	if isValidDoHGetParameter("AAAA", maxDNSMessageBytes) {
		t.Fatal("3-byte payload below the 12-byte DNS header accepted")
	}
	if isValidDoHGetParameter(strings.Repeat("A", 15)+"=", maxDNSMessageBytes) || isValidDoHGetParameter("AAAAAAAAAAAAAAA!", maxDNSMessageBytes) {
		t.Fatal("invalid alphabet accepted")
	}
	if isValidDoHGetParameter(valid, maxDNSMessageBytes+1) {
		t.Fatal("validation accepted a configured size above the hard DoH limit")
	}
}
func TestDoHGETParameterValidationAllocs(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString([]byte(testDNSBody + testDNSBody))
	if !isValidDoHGetParameter(valid, maxDNSMessageBytes) {
		t.Fatal("valid raw base64url parameter rejected")
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if !isValidDoHGetParameter(valid, maxDNSMessageBytes) {
			t.Fatal("valid raw base64url parameter rejected during allocation check")
		}
	})
	if allocs != 0 {
		t.Fatalf("validation allocated %.2f times per call, want 0", allocs)
	}
}
func TestProcessingConcurrencyQueues(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 1)
	var wg sync.WaitGroup
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("first request: got %d, want %d", w.Code, http.StatusOK)
		}
		close(firstDone)
	}()
	<-entered
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("queued request: got %d, want %d", w.Code, http.StatusOK)
		}
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("second request completed before processing slot was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-firstDone
	<-secondDone
	wg.Wait()
}
func TestActiveRequestCap(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	limiter := newFixedWindowLimiterWithMaxKeys(100000, time.Minute, 16)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	g := &gateway{
		backendURL:       backend.URL,
		client:           newHTTPClient(1, 2*time.Second),
		limiter:          limiter,
		activeSlots:      make(chan struct{}, 1),
		processingSlots:  make(chan struct{}, 1),
		upstreamTimeout:  2 * time.Second,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  "X-Forwarded-For",
	}
	g.activeSlots <- struct{}{}
	defer func() { <-g.activeSlots }()
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestDefaultCapacityConfiguration(t *testing.T) {
	if defaultRateLimitClients != 65536 {
		t.Fatalf("default rate-limit client guard=%d, want 65536", defaultRateLimitClients)
	}
	if defaultMaxActiveRequests != 16 {
		t.Fatalf("default active-request capacity=%d, want 16", defaultMaxActiveRequests)
	}
	if defaultConcurrency != 8 {
		t.Fatalf("default backend processing cap=%d, want 8", defaultConcurrency)
	}
	transport := newHTTPClient(defaultConcurrency, defaultUpstreamTO).Transport.(*http.Transport)
	if transport.MaxIdleConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxIdleConnsPerHost=%d, want %d", transport.MaxIdleConnsPerHost, defaultConcurrency)
	}
	if transport.MaxConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxConnsPerHost=%d, want %d", transport.MaxConnsPerHost, defaultConcurrency)
	}
	if transport.ResponseHeaderTimeout != defaultUpstreamTO {
		t.Fatalf("ResponseHeaderTimeout=%s, want %s", transport.ResponseHeaderTimeout, defaultUpstreamTO)
	}
	transport = newHTTPClient(50, time.Second).Transport.(*http.Transport)
	if transport.MaxConnsPerHost != 50 || transport.MaxIdleConnsPerHost != 50 {
		t.Fatalf("transport conns=%d/%d, want 50/50", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}
}
func TestActiveRequestBurstCapacity(t *testing.T) {
	const burst = 128
	const processingCap = 8
	backendRelease := make(chan struct{})
	backendEntered := make(chan struct{}, burst)
	var backendMu sync.Mutex
	backendActive := 0
	backendPeak := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendMu.Lock()
		backendActive++
		if backendActive > backendPeak {
			backendPeak = backendActive
		}
		backendMu.Unlock()
		backendEntered <- struct{}{}
		<-backendRelease
		backendMu.Lock()
		backendActive--
		backendMu.Unlock()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	limiter := newFixedWindowLimiterWithMaxKeys(100000, time.Minute, burst)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	g := &gateway{
		backendURL:       backend.URL,
		client:           newHTTPClient(processingCap, 5*time.Second),
		limiter:          limiter,
		activeSlots:      make(chan struct{}, burst),
		processingSlots:  make(chan struct{}, processingCap),
		queueWait:        10 * time.Second,
		upstreamTimeout:  5 * time.Second,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  "X-Forwarded-For",
	}
	var wg sync.WaitGroup
	results := make(chan int, burst)
	wg.Add(burst)
	for i := 0; i < burst; i++ {
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
			r.Header.Set("Content-Type", "application/dns-message")
			r.Header.Set("X-Forwarded-For", "198.18.0.1")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			results <- w.Code
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(g.activeSlots) < burst && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(g.activeSlots); got != burst {
		t.Fatalf("active slots reached %d, want %d", got, burst)
	}
	for i := 0; i < processingCap; i++ {
		select {
		case <-backendEntered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d backend requests entered; want %d", i, processingCap)
		}
	}
	backendMu.Lock()
	peak := backendPeak
	backendMu.Unlock()
	if peak > processingCap {
		t.Fatalf("backend peak concurrency=%d, want <= %d", peak, processingCap)
	}
	close(backendRelease)
	wg.Wait()
	close(results)
	ok := 0
	for code := range results {
		if code == http.StatusOK {
			ok++
		}
	}
	if ok != burst {
		t.Fatalf("successful requests=%d, want %d", ok, burst)
	}
	if got := len(g.activeSlots); got != 0 {
		t.Fatalf("active slots after completion=%d, want 0", got)
	}
	backendMu.Lock()
	finalActive := backendActive
	backendMu.Unlock()
	if finalActive != 0 {
		t.Fatalf("backend active requests after completion=%d, want 0", finalActive)
	}
}
func TestHealthEndpoint(t *testing.T) {
	g := testGateway("http://127.0.0.1:1", 100, 1)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if string(body) != "ok\n" {
		t.Fatalf("body=%q", body)
	}
}

type failingReader struct {
	readErr error
}

func (r failingReader) Read([]byte) (int, error) { return 0, r.readErr }
func (r failingReader) Close() error             { return nil }
func TestResponseConnectionHeaderTokensAreStripped(t *testing.T) {
	src := make(http.Header)
	src.Add("Connection", "X-Request-Token, X-Another")
	src.Set("X-Request-Token", "secret")
	src.Set("X-Another", "secret2")
	src.Set("X-Keep", "safe")
	dst := make(http.Header)
	copyResponseHeaders(dst, src)
	if dst.Get("X-Request-Token") != "" || dst.Get("X-Another") != "" {
		t.Fatalf("connection-token response headers leaked: %v", dst)
	}
	if dst.Get("X-Keep") != "safe" {
		t.Fatalf("ordinary response header lost: %v", dst)
	}
}
func TestKoyebClientIPUsesLastXForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Forwarded-For", "198.51.100.10, 203.0.113.10")
	got := extractClientIP(r, "X-Forwarded-For")
	if got != "203.0.113.10" {
		t.Fatalf("got %q, want last X-Forwarded-For address", got)
	}
}
func TestPOSTReadErrorRejected(t *testing.T) {
	var called atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", nil)
	r.Body = failingReader{readErr: errors.New("synthetic read failure")}
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want %d", w.Code, http.StatusBadRequest)
	}
	if called.Load() {
		t.Fatal("backend was called after request body read failure")
	}
}
func probeResponse(t *testing.T, r *http.Request, rcode byte) []byte {
	dns := r.URL.Query().Get("dns")
	if dns == "" {
		t.Errorf("readiness probe did not send a dns parameter")
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(dns)
	if err != nil || len(decoded) <= 12 {
		t.Errorf("readiness probe sent an unparseable dns parameter: %q", dns)
		return nil
	}
	resp := make([]byte, 12, len(decoded))
	copy(resp, decoded[:2])
	resp[2] = 0x81
	resp[3] = 0x80 | rcode
	resp[5] = 0x01
	resp = append(resp, decoded[12:]...)
	return resp
}
func TestReadyEndpointTracksBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 0))
	}))
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("live backend: got %d", w.Code)
	}
	backend.Close()
	w = httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead backend: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestReadyEndpointRejectsServfail(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 2))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("SERVFAIL backend: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestReadyEndpointRejectsBareHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := probeResponse(t, r, 0)
		if len(resp) >= 12 {
			resp = resp[:12]
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(resp)
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("question-less backend: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestReadyEndpointRejectsMismatchedID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := probeResponse(t, r, 0)
		if len(resp) >= 2 {
			resp[0], resp[1] = 0xff, 0xff
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(resp)
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("mismatched-ID backend: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestMetricsEndpoint(t *testing.T) {
	g := testGateway("http://127.0.0.1:1", 100, 1)
	g.metrics.requestsTotal.Add(7)
	g.metrics.queueWaitSamples.Add(2)
	g.metrics.queueWaitNanos.Add(uint64(150 * time.Millisecond))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	for _, want := range []string{
		"doh_gateway_requests_total 7",
		"doh_gateway_queue_timeout_total",
		"doh_gateway_ready_probe_total{result=\"ready\"}",
		"doh_gateway_queue_wait_seconds_avg 0.075000",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in %s", want, body)
		}
	}
}
func TestResponseBoundsAndContentType(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mode") {
		case "oversize":
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(make([]byte, maxDNSMessageBytes+1))
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(testDNSBody))
		}
	}))
	defer backend.Close()
	for _, tc := range []struct {
		name string
		mode string
		want int
	}{
		{name: "oversize", mode: "oversize", want: http.StatusBadGateway},
		{name: "wrong content type", mode: "wrong-type", want: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGateway(backend.URL, 100, 10)
			g.backendURL = backend.URL + "?mode=" + tc.mode
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
			r.Header.Set("Content-Type", "application/dns-message")
			g.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
	boundaryBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, maxDNSMessageBytes))
	}))
	defer boundaryBackend.Close()
	g := testGateway(boundaryBackend.URL, 100, 10)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("max-size response: got %d, want %d", w.Code, http.StatusOK)
	}
}
func TestGETRejectsPaddedBase64URL(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AQ%3D%3D", nil)
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want %d", w.Code, http.StatusBadRequest)
	}
}
func TestGETPreservesExistingBackendQuery(t *testing.T) {
	payload := []byte(testDNSBody)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("static") != "yes" {
			t.Errorf("backend lost its static query param: %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("dns") != encoded {
			t.Errorf("backend got dns=%q, want %q", r.URL.Query().Get("dns"), encoded)
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(payload)
	}))
	defer backend.Close()
	g := testGateway(backend.URL+"?static=yes", 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
}
func TestGETValidBase64URL(t *testing.T) {
	payload := []byte(testDNSBody)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dns") != encoded {
			t.Errorf("backend got dns=%q, want %q", r.URL.Query().Get("dns"), encoded)
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(payload)
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dns-query?dns="+encoded, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if got := w.Body.Bytes(); string(got) != string(payload) {
		t.Fatalf("body=%x, want %x", got, payload)
	}
}
func TestFixedWindowResets(t *testing.T) {
	l := newFixedWindowLimiter(1, time.Minute)
	l.now = func() time.Time { return time.Unix(120, 0) }
	if !l.allow("198.51.100.1") {
		t.Fatal("first request denied")
	}
	if l.allow("198.51.100.1") {
		t.Fatal("second request allowed in same window")
	}
	l.now = func() time.Time { return time.Unix(180, 0) }
	if !l.allow("198.51.100.1") {
		t.Fatal("request denied after window reset")
	}
}
func TestFixedWindowRetryAfter(t *testing.T) {
	l := newFixedWindowLimiter(1, time.Minute)
	l.now = func() time.Time { return time.Unix(125, 0) }
	if !l.allow("198.51.100.20") {
		t.Fatal("first request denied")
	}
	if l.allow("198.51.100.20") {
		t.Fatal("second request allowed in same window")
	}
	if got := l.retryAfterSeconds(); got != 55 {
		t.Fatalf("retry-after=%d, want 55", got)
	}
	l.now = func() time.Time { return time.Unix(179, 0) }
	if got := l.retryAfterSeconds(); got != 1 {
		t.Fatalf("retry-after=%d, want 1", got)
	}
}
func okBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
}
func TestSlowBodyDoesNotHoldProcessingSlot(t *testing.T) {
	backend := okBackend()
	defer backend.Close()
	g := testGateway(backend.URL, 100, 1)
	pr, pw := io.Pipe()
	slow := httptest.NewRequest(http.MethodPost, "/dns-query", pr)
	slow.Header.Set("Content-Type", "application/dns-message")
	slow.Header.Set("X-Forwarded-For", "192.0.2.50")
	slowDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, slow)
		slowDone <- w.Code
	}()
	fast := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	fast.Header.Set("Content-Type", "application/dns-message")
	fast.Header.Set("X-Forwarded-For", "192.0.2.51")
	fastDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, fast)
		fastDone <- w.Code
	}()
	select {
	case code := <-fastDone:
		if code != http.StatusOK {
			t.Fatalf("fast request got %d, want %d", code, http.StatusOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast request blocked behind a client that is still sending its body")
	}
	if _, err := pw.Write([]byte(testDNSBody)); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	select {
	case code := <-slowDone:
		if code != http.StatusOK {
			t.Fatalf("slow request got %d, want %d", code, http.StatusOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow request never completed")
	}
}
func TestPOSTShortBodyRejected(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody[:11]))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want %d", w.Code, http.StatusBadRequest)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("backend was called for a body shorter than a DNS header")
	}
}
func TestClientHeadersAreNotForwarded(t *testing.T) {
	seen := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	r.Header.Set("Cookie", "session=secret")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	got := <-seen
	if got.Get("Cookie") != "" || got.Get("Authorization") != "" {
		t.Fatalf("client credentials leaked upstream: %v", got)
	}
	if got.Get("X-Forwarded-For") != "203.0.113.5" || got.Get("Accept") != "application/dns-message" {
		t.Fatalf("unexpected upstream headers: %v", got)
	}
}
func TestRateLimitKeyGroupsIPv6By64(t *testing.T) {
	if a, b := rateLimitKey("2001:db8:1:2::1"), rateLimitKey("2001:db8:1:2:ffff::9"); a != b {
		t.Fatalf("same /64 produced different keys: %q vs %q", a, b)
	}
	if rateLimitKey("2001:db8:1:2::1") == rateLimitKey("2001:db8:1:3::1") {
		t.Fatal("different /64 networks share a key")
	}
	if got := rateLimitKey("198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("IPv4 key changed: %q", got)
	}
	if got := rateLimitKey("unknown"); got != "unknown" {
		t.Fatalf("fallback key changed: %q", got)
	}
}
func TestClientIPHeaderTrust(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Real-IP", "203.0.113.8")
	if got := extractClientIP(r, ""); got != "192.0.2.1" {
		t.Fatalf("with header trust disabled got %q, want the socket peer", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Real-IP", "203.0.113.8")
	if got := extractClientIP(r, "X-Forwarded-For"); got != "203.0.113.8" {
		t.Fatalf("X-Real-IP fallback got %q", got)
	}
}
func TestClientIPHeaderInvalidFallsBackToRealIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	r.Header.Set("X-Real-IP", "203.0.113.8")
	if got := extractClientIP(r, "X-Forwarded-For"); got != "203.0.113.8" {
		t.Fatalf("invalid preferred header did not fall back to X-Real-IP: got %q", got)
	}
}
func TestReadyEndpointCachesResult(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 0))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	g.readyCacheTTL = time.Minute
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("probe %d: got %d", i+1, w.Code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("backend probed %d times, want 1 (cached)", got)
	}
}
func TestMetricsCountGatewayGeneratedResponses(t *testing.T) {
	g := testGateway("http://127.0.0.1:1", 100, 1)
	bad := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	bad.Header.Set("Content-Type", "text/plain")
	g.ServeHTTP(httptest.NewRecorder(), bad)
	if got := g.metrics.responses4xxTotal.Load(); got != 1 {
		t.Fatalf("4xx count=%d, want 1", got)
	}
	dead := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	dead.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, dead)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("dead backend: got %d, want %d", w.Code, http.StatusBadGateway)
	}
	if got := g.metrics.responses5xxTotal.Load(); got != 1 {
		t.Fatalf("5xx count=%d, want 1", got)
	}
}
func TestBackendRedirectIsNotFollowed(t *testing.T) {
	var followed int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followed, 1)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer target.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("redirecting backend: got %d, want %d", w.Code, http.StatusBadGateway)
	}
	if w.Header().Get("Location") != "" {
		t.Fatalf("Location header relayed to the client: %v", w.Header())
	}
	if atomic.LoadInt32(&followed) != 0 {
		t.Fatal("gateway followed a backend redirect")
	}
	if got := g.metrics.backendErrorTotal.Load(); got != 1 {
		t.Fatalf("backend errors=%d, want 1", got)
	}
}
func TestTransportHeaderTimeoutMapsTo504(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	g.client = newHTTPClient(10, 50*time.Millisecond)
	g.upstreamTimeout = 5 * time.Second
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d, want %d", w.Code, http.StatusGatewayTimeout)
	}
}
func TestClientDisconnectIsNotABackendError(t *testing.T) {
	started := make(chan struct{})
	backendRelease := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-backendRelease:
		}
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(w, r)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the client went away")
	}
	close(backendRelease)
	if got := g.metrics.backendErrorTotal.Load(); got != 0 {
		t.Fatalf("client disconnect counted as %d backend errors, want 0", got)
	}
}
func TestMetricsExposeSampleCounters(t *testing.T) {
	g := testGateway("http://127.0.0.1:1", 100, 1)
	g.metrics.queueWaitSamples.Add(3)
	g.metrics.backendLatencySamples.Add(5)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		"doh_gateway_queue_wait_samples_total 3",
		"doh_gateway_backend_latency_samples_total 5",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in %s", want, body)
		}
	}
}
func TestQueueTimeoutReturns503(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 1)
	g.queueWait = 50 * time.Millisecond
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		g.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After=%q, want %q", got, "1")
	}
	if got := g.metrics.queueTimeoutTotal.Load(); got != 1 {
		t.Errorf("queue timeouts=%d, want 1", got)
	}
	close(release)
	<-firstDone
}
func TestWrongMethodAdvertisesAllowedMethods(t *testing.T) {
	g := testGateway("http://127.0.0.1:1", 100, 1)
	for _, tc := range []struct {
		path  string
		allow string
	}{
		{"/healthz", "GET, HEAD"},
		{"/readyz", "GET, HEAD"},
		{"/metrics", "GET, HEAD"},
		{"/dns-query", "GET, POST"},
	} {
		method := http.MethodPost
		if tc.path == "/dns-query" {
			method = http.MethodPut
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(method, tc.path, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want %d", tc.path, w.Code, http.StatusMethodNotAllowed)
		}
		if got := w.Header().Get("Allow"); got != tc.allow {
			t.Fatalf("%s: Allow=%q, want %q", tc.path, got, tc.allow)
		}
	}
}
func TestRateLimitedResponseIsPlainText(t *testing.T) {
	backend := okBackend()
	defer backend.Close()
	g := testGateway(backend.URL, 1, 10)
	var w *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		w = httptest.NewRecorder()
		g.ServeHTTP(w, r)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type=%q, want text/plain", got)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}
func TestRelayedResponseSetsContentLength(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, 3000))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Content-Length"); got != "3000" {
		t.Fatalf("Content-Length=%q, want 3000", got)
	}
	if w.Body.Len() != 3000 {
		t.Fatalf("body length=%d, want 3000", w.Body.Len())
	}
}
func TestLimiterReadsClockUnderLock(t *testing.T) {
	l := newFixedWindowLimiter(1, time.Minute)
	l.now = func() time.Time {
		if l.mu.TryLock() {
			l.mu.Unlock()
			t.Error("limiter clock was read without holding the mutex")
		}
		return time.Unix(120, 0)
	}
	if !l.allow("198.51.100.1") {
		t.Fatal("first request denied")
	}
}
func TestBackendRequestURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		dns     string
		want    string
		wantOK  bool
	}{
		{"plain without dns", "http://127.0.0.1:8081/dns-query", "", "http://127.0.0.1:8081/dns-query", true},
		{"plain with dns", "http://127.0.0.1:8081/dns-query", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA", true},
		{"existing query is kept", "http://127.0.0.1:8081/dns-query?static=yes", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA&static=yes", true},
		{"existing dns is replaced", "http://127.0.0.1:8081/dns-query?dns=old&static=yes", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA&static=yes", true},
		{"empty backend", "", "AAAA", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &gateway{backendURL: tc.backend}
			got, ok := g.backendRequestURL(tc.dns)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("backendRequestURL(%q)=%q,%v want %q,%v", tc.dns, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
func TestHTTPClientIdlePoolFollowsConcurrency(t *testing.T) {
	transport := newHTTPClient(200, time.Second).Transport.(*http.Transport)
	if transport.MaxIdleConns != 200 || transport.MaxIdleConnsPerHost != 200 || transport.MaxConnsPerHost != 200 {
		t.Fatalf("pool sizes=%d/%d/%d, want 200/200/200", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost)
	}
}
func TestServerWriteTimeoutCoversUpstreamTimeout(t *testing.T) {
	if got := serverWriteTimeout(defaultQueueWait, defaultUpstreamTO); got != defaultWriteTimeout {
		t.Fatalf("default write timeout changed: got %s, want %s", got, defaultWriteTimeout)
	}
	if got := serverWriteTimeout(defaultQueueWait, 10*time.Second); got <= defaultQueueWait+10*time.Second {
		t.Fatalf("write timeout %s does not exceed queue wait plus upstream timeout", got)
	}
}

// blockingWriter simulates a client that is not reading: the first body Write
// blocks until release is closed.
type blockingWriter struct {
	h       http.Header
	inWrite chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Header() http.Header { return b.h }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.inWrite) })
	<-b.release
	return len(p), nil
}
func TestSlowReadingClientDoesNotHoldProcessingSlot(t *testing.T) {
	backend := okBackend()
	defer backend.Close()
	g := testGateway(backend.URL, 100, 1)
	bw := &blockingWriter{h: make(http.Header), inWrite: make(chan struct{}), release: make(chan struct{})}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
		r.Header.Set("Content-Type", "application/dns-message")
		g.ServeHTTP(bw, r)
	}()
	select {
	case <-bw.inWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("slow client's response was never written")
	}
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("request behind a slow-reading client got %d, want %d", w.Code, http.StatusOK)
	}
	close(bw.release)
	<-slowDone
	if got := len(g.processingSlots); got != 0 {
		t.Fatalf("processing slots held after completion=%d, want 0", got)
	}
}

func TestShortBackendResponseRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{0x01})
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	r.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("short 2xx backend body: got %d, want %d", w.Code, http.StatusBadGateway)
	}
	if got := g.metrics.backendErrorTotal.Load(); got != 1 {
		t.Fatalf("backend errors=%d, want 1", got)
	}
}
func TestReadyEndpointHonorsRequestContext(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 0))
	}))
	defer backend.Close()
	g := testGateway(backend.URL, 100, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	}()
	time.Sleep(200 * time.Millisecond) // let the probe reach the backend
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readyz did not return after the client went away")
	}
	close(release)
	if !g.readyAt.IsZero() {
		t.Fatal("canceled probe poisoned the readiness cache")
	}
}
