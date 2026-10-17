package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
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

// newTestGateway builds a gateway through the production constructor and
// freezes the limiter clock so rate-limit windows never roll over mid-test.
func newTestGateway(t testing.TB, o gatewayOptions) *gateway {
	t.Helper()
	g, err := newGateway(o)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	g.limiter.now = func() time.Time { return time.Unix(120, 0) }
	return g
}

// postReq builds a DoH POST request with the required content type.
func postReq(body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/dns-query", body)
	r.Header.Set("Content-Type", "application/dns-message")
	return r
}

// okBackend answers every query with testDNSBody.
func okBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
}

func testGateway(t testing.TB, backend string, limit int, concurrency int) *gateway {
	t.Helper()
	return newTestGateway(t, gatewayOptions{
		BackendURL:      backend,
		RateLimit:       limit,
		Processing:      concurrency,
		MaxActive:       5000,
		UpstreamTimeout: 2 * time.Second,
		TrustedIPHeader: "X-Forwarded-For",
	})
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
	backend := okBackend()
	defer backend.Close()
	g := testGateway(t, backend.URL, 2, 10)
	for i := 0; i < 2; i++ {
		r := postReq(strings.NewReader(testDNSBody))
		r.Header.Set("X-Forwarded-For", "192.0.2.10")
		r.ContentLength = int64(len(testDNSBody))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want %d", i+1, w.Code, http.StatusOK)
		}
	}
	r := postReq(strings.NewReader(testDNSBody))
	r.Header.Set("X-Forwarded-For", "192.0.2.10")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	r = postReq(strings.NewReader(testDNSBody))
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
	if l.admit("198.51.100.1") != 0 {
		t.Fatal("first client denied")
	}
	if l.admit("198.51.100.2") != 0 {
		t.Fatal("second client denied")
	}
	if l.admit("198.51.100.3") == 0 {
		t.Fatal("third client exceeded key cap but was allowed")
	}
	if l.admit("198.51.100.1") != 0 {
		t.Fatal("existing client denied after key cap reached")
	}
}
func TestDoHContentTypeAndSize(t *testing.T) {
	backend := okBackend()
	defer backend.Close()
	g := testGateway(t, backend.URL, 100, 10)
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
	tooLarge := postReq(strings.NewReader(strings.Repeat("a", maxDNSMessageBytes+1)))
	tooLarge.ContentLength = maxDNSMessageBytes + 1
	w = httptest.NewRecorder()
	g.ServeHTTP(w, tooLarge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: got %d", w.Code)
	}
}
func TestDoHGETParameterValidation(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString([]byte(testDNSBody))
	if !isValidDoHGetParameter(valid) {
		t.Fatal("valid raw base64url parameter rejected")
	}
	if isValidDoHGetParameter("AQ==") {
		t.Fatal("padded base64url parameter accepted")
	}
	if isValidDoHGetParameter(strings.Repeat("A", base64.RawURLEncoding.EncodedLen(maxDNSMessageBytes)+1)) {
		t.Fatal("oversize base64url parameter accepted")
	}
	if isValidDoHGetParameter("AAAA") {
		t.Fatal("3-byte payload below the 12-byte DNS header accepted")
	}
	if isValidDoHGetParameter(strings.Repeat("A", 15)+"=") || isValidDoHGetParameter("AAAAAAAAAAAAAAA!") {
		t.Fatal("invalid alphabet accepted")
	}
	if !isValidDoHGetParameter(base64.RawURLEncoding.EncodeToString(make([]byte, maxDNSMessageBytes))) {
		t.Fatal("max-size payload rejected")
	}
}
func TestDoHGETParameterValidationAllocs(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString([]byte(testDNSBody + testDNSBody))
	if !isValidDoHGetParameter(valid) {
		t.Fatal("valid raw base64url parameter rejected")
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if !isValidDoHGetParameter(valid) {
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
	g := testGateway(t, backend.URL, 100, 1)
	var wg sync.WaitGroup
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := postReq(strings.NewReader(testDNSBody))
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
		r := postReq(strings.NewReader(testDNSBody))
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
	backend := okBackend()
	defer backend.Close()
	g := newTestGateway(t, gatewayOptions{
		BackendURL:       backend.URL,
		RateLimit:        100000,
		RateLimitClients: 16,
		MaxActive:        1,
		Processing:       1,
		QueueWait:        time.Second,
		UpstreamTimeout:  2 * time.Second,
		TrustedIPHeader:  "X-Forwarded-For",
	})
	g.activeSlots <- struct{}{}
	defer func() { <-g.activeSlots }()
	r := postReq(strings.NewReader(testDNSBody))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}
func TestBackendPoolSizing(t *testing.T) {
	transport := newHTTPClient(defaultConcurrency).Transport.(*http.Transport)
	if transport.MaxIdleConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxIdleConnsPerHost=%d, want %d", transport.MaxIdleConnsPerHost, defaultConcurrency)
	}
	if transport.MaxConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxConnsPerHost=%d, want %d", transport.MaxConnsPerHost, defaultConcurrency)
	}
	transport = newHTTPClient(200).Transport.(*http.Transport)
	if transport.MaxIdleConns != 200 || transport.MaxIdleConnsPerHost != 200 || transport.MaxConnsPerHost != 200 {
		t.Fatalf("pool sizes=%d/%d/%d, want 200/200/200", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost)
	}
	if g := testGateway(t, "http://127.0.0.1:1", 100, 3); cap(g.processingSlots) != 3 || g.client.Transport.(*http.Transport).MaxConnsPerHost != 3 {
		t.Fatal("constructor did not size the backend pool from MAX_CONCURRENT_REQUESTS")
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
	g := newTestGateway(t, gatewayOptions{
		BackendURL:       backend.URL,
		RateLimit:        100000,
		RateLimitClients: burst,
		MaxActive:        burst,
		Processing:       processingCap,
		QueueWait:        10 * time.Second,
		UpstreamTimeout:  5 * time.Second,
		TrustedIPHeader:  "X-Forwarded-For",
	})
	var wg sync.WaitGroup
	results := make(chan int, burst)
	wg.Add(burst)
	for i := 0; i < burst; i++ {
		go func() {
			defer wg.Done()
			r := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
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
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	got := g.clientIP(r).String()
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
	g := testGateway(t, backend.URL, 100, 10)
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
	resp := make([]byte, 12, len(decoded)+16)
	copy(resp, decoded[:2])
	resp[2] = 0x81
	resp[3] = 0x80 | rcode
	resp[5] = 0x01
	resp = append(resp, decoded[12:]...)
	if rcode == 0 {
		// One answer: A 127.0.0.1, name compressed to the question.
		resp[7] = 0x01
		resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4, 127, 0, 0, 1)
	}
	return resp
}
func TestReadyEndpointTracksBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 0))
	}))
	g := testGateway(t, backend.URL, 100, 10)
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
func TestMetricsEndpoint(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	g.metrics.requestsTotal.Add(7)
	g.metrics.queueWaitSamples.Add(2)
	g.metrics.queueWaitNanos.Add(uint64(150 * time.Millisecond))
	g.metrics.backendLatencySamples.Add(5)
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
		"doh_gateway_queue_wait_seconds_total 0.150000",
		"doh_gateway_queue_wait_samples_total 2",
		"doh_gateway_backend_latency_samples_total 5",
		"doh_gateway_build_info{version=",
		`doh_gateway_responses_total{class="2xx"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "3xx") {
		t.Fatal("metrics still expose the always-zero 3xx series")
	}
}
func TestMaxSizeResponseAccepted(t *testing.T) {
	boundaryBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, maxDNSMessageBytes))
	}))
	defer boundaryBackend.Close()
	g := testGateway(t, boundaryBackend.URL, 100, 10)
	w := httptest.NewRecorder()
	r := postReq(strings.NewReader(testDNSBody))
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("max-size response: got %d, want %d", w.Code, http.StatusOK)
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
	g := testGateway(t, backend.URL+"?static=yes", 100, 10)
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
	g := testGateway(t, backend.URL, 100, 10)
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
	if l.admit("198.51.100.1") != 0 {
		t.Fatal("first request denied")
	}
	if l.admit("198.51.100.1") == 0 {
		t.Fatal("second request allowed in same window")
	}
	l.now = func() time.Time { return time.Unix(180, 0) }
	if l.admit("198.51.100.1") != 0 {
		t.Fatal("request denied after window reset")
	}
}
func TestFixedWindowRetryAfter(t *testing.T) {
	l := newFixedWindowLimiter(1, time.Minute)
	l.now = func() time.Time { return time.Unix(125, 0) }
	if got := l.admit("198.51.100.20"); got != 0 {
		t.Fatalf("first request denied (retry-after=%d)", got)
	}
	if got := l.admit("198.51.100.20"); got != 55 {
		t.Fatalf("retry-after=%d, want 55", got)
	}
	l.now = func() time.Time { return time.Unix(179, 0) }
	if got := l.admit("198.51.100.20"); got != 1 {
		t.Fatalf("retry-after=%d, want 1", got)
	}
	// A new client rejected by the key cap gets the same window-based value.
	capped := newFixedWindowLimiterWithMaxKeys(10, time.Minute, 1)
	capped.now = func() time.Time { return time.Unix(125, 0) }
	capped.admit("198.51.100.1")
	if got := capped.admit("198.51.100.2"); got != 55 {
		t.Fatalf("key-cap retry-after=%d, want 55", got)
	}
}
func TestSlowBodyDoesNotHoldProcessingSlot(t *testing.T) {
	backend := okBackend()
	defer backend.Close()
	g := testGateway(t, backend.URL, 100, 1)
	pr, pw := io.Pipe()
	slow := postReq(pr)
	slow.Header.Set("X-Forwarded-For", "192.0.2.50")
	slowDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, slow)
		slowDone <- w.Code
	}()
	fast := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, backend.URL, 100, 10)
	r := postReq(strings.NewReader(testDNSBody[:11]))
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
	g := testGateway(t, backend.URL, 100, 10)
	r := postReq(strings.NewReader(testDNSBody))
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
	// MosDNS does not use the client address, so the gateway sends none.
	if got.Get("X-Forwarded-For") != "" || got.Get("Accept") != "application/dns-message" {
		t.Fatalf("unexpected upstream headers: %v", got)
	}
}
func TestRateLimitKeyGroupsIPv6By64(t *testing.T) {
	key := func(s string) string { return rateLimitKey(netip.MustParseAddr(s)) }
	if a, b := key("2001:db8:1:2::1"), key("2001:db8:1:2:ffff::9"); a != b {
		t.Fatalf("same /64 produced different keys: %q vs %q", a, b)
	}
	if key("2001:db8:1:2::1") == key("2001:db8:1:3::1") {
		t.Fatal("different /64 networks share a key")
	}
	if got := key("198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("IPv4 key changed: %q", got)
	}
	if got := rateLimitKey(netip.Addr{}); got != "unknown" {
		t.Fatalf("fallback key changed: %q", got)
	}
}
func TestClientIPHeaderTrust(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Real-IP", "203.0.113.8")
	// With no CLIENT_IP_HEADER configured, the header is never consulted.
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	g.trustedIPHeader = ""
	if got := g.clientIP(r).String(); got != "192.0.2.1" {
		t.Fatalf("with header trust disabled got %q, want the socket peer", got)
	}
	// Only the configured header is trusted: X-Real-IP is client-controlled
	// unless the proxy strips it, so a missing or invalid trusted value falls
	// back to the socket peer.
	for _, xff := range []string{"", "not-an-ip"} {
		r = httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		r.Header.Set("X-Real-IP", "203.0.113.8")
		if got := g.clientIP(r).String(); got != "192.0.2.1" {
			t.Fatalf("XFF=%q: got %q, want the socket peer (X-Real-IP must not be trusted)", xff, got)
		}
	}
}

// The trusted header is honored only when the socket peer is inside one of
// the trusted proxy ranges; a direct client with a spoofed header falls back
// to its own address.
func TestClientIPRequiresTrustedProxy(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	trusted, err := netip.ParsePrefix("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	g.trustProxyCIDRs = []netip.Prefix{trusted.Masked()}
	g.trustedIPHeader = "X-Forwarded-For"

	r := httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := g.clientIP(r).String(); got != "192.0.2.1" {
		t.Fatalf("direct client with spoofed header got %q, want its socket peer", got)
	}

	r = httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := g.clientIP(r).String(); got != "203.0.113.7" {
		t.Fatalf("trusted proxy peer got %q, want the header address", got)
	}

	// An invalid header value from a trusted peer still falls back.
	r = httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := g.clientIP(r).String(); got != "10.1.2.3" {
		t.Fatalf("invalid header value got %q, want the proxy's own address", got)
	}

	// IPv6 proxies are matched with an IPv6 prefix.
	g.trustProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}
	r = httptest.NewRequest(http.MethodGet, "/dns-query?dns=AA", nil)
	r.RemoteAddr = "[2001:db8:1::1]:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := g.clientIP(r).String(); got != "203.0.113.7" {
		t.Fatalf("IPv6 trusted proxy got %q, want the header address", got)
	}
}

func TestParseTrustedProxyPrefixes(t *testing.T) {
	if prefixes, err := parseTrustedProxyPrefixes(""); err != nil || prefixes != nil {
		t.Fatalf("empty list: %v %v", prefixes, err)
	}
	prefixes, err := parseTrustedProxyPrefixes(" 10.0.0.0/8, 2001:db8::/32 ,")
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 2 || prefixes[0].String() != "10.0.0.0/8" || prefixes[1].String() != "2001:db8::/32" {
		t.Fatalf("got %v", prefixes)
	}
	if _, err := parseTrustedProxyPrefixes("10.0.0.0/8,bogus"); err == nil {
		t.Fatal("invalid CIDR accepted")
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
	g := testGateway(t, backend.URL, 100, 10)
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
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	bad := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader(testDNSBody))
	bad.Header.Set("Content-Type", "text/plain")
	g.ServeHTTP(httptest.NewRecorder(), bad)
	if got := g.metrics.responses4xxTotal.Load(); got != 1 {
		t.Fatalf("4xx count=%d, want 1", got)
	}
	dead := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, backend.URL, 100, 10)
	r := postReq(strings.NewReader(testDNSBody))
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
func TestUpstreamTimeoutMapsTo504(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(testDNSBody))
	}))
	defer backend.Close()
	g := testGateway(t, backend.URL, 100, 10)
	g.upstreamTimeout = 50 * time.Millisecond
	r := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, backend.URL, 100, 10)
	ctx, cancel := context.WithCancel(context.Background())
	r := postReq(strings.NewReader(testDNSBody)).WithContext(ctx)
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
	g := testGateway(t, backend.URL, 100, 1)
	g.queueWait = 50 * time.Millisecond
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r := postReq(strings.NewReader(testDNSBody))
		g.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	r := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
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
	g := testGateway(t, backend.URL, 1, 10)
	var w *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		r := postReq(strings.NewReader(testDNSBody))
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
	g := testGateway(t, backend.URL, 100, 10)
	r := postReq(strings.NewReader(testDNSBody))
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
	if l.admit("198.51.100.1") != 0 {
		t.Fatal("first request denied")
	}
}
func TestBackendRequestURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		dns     string
		want    string
	}{
		{"plain without dns", "http://127.0.0.1:8081/dns-query", "", "http://127.0.0.1:8081/dns-query"},
		{"plain with dns", "http://127.0.0.1:8081/dns-query", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA"},
		{"existing query is kept", "http://127.0.0.1:8081/dns-query?static=yes", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA&static=yes"},
		{"existing dns is replaced", "http://127.0.0.1:8081/dns-query?dns=old&static=yes", "AAAA", "http://127.0.0.1:8081/dns-query?dns=AAAA&static=yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGateway(t, gatewayOptions{BackendURL: tc.backend})
			if got := g.backendRequestURL(tc.dns); got != tc.want {
				t.Fatalf("backendRequestURL(%q)=%q want %q", tc.dns, got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "127.0.0.1:8081", "ftp://127.0.0.1/dns-query", "http:///dns-query"} {
		if _, err := newGateway(gatewayOptions{BackendURL: bad}); err == nil {
			t.Errorf("newGateway accepted invalid backend %q", bad)
		}
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
	g := testGateway(t, backend.URL, 100, 1)
	bw := &blockingWriter{h: make(http.Header), inWrite: make(chan struct{}), release: make(chan struct{})}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		r := postReq(strings.NewReader(testDNSBody))
		g.ServeHTTP(bw, r)
	}()
	select {
	case <-bw.inWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("slow client's response was never written")
	}
	r := postReq(strings.NewReader(testDNSBody))
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

func TestGETInvalidParameterRejected(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:1", 100, 10)
	for _, q := range []string{"dns=not-base64!", "dns=AQ%3D%3D", "dns=", "other=1"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dns-query?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want %d", q, w.Code, http.StatusBadRequest)
		}
	}
}
func TestReadyEndpointRejectsUnusableAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, r *http.Request) []byte
	}{
		// NXDOMAIN means the local readiness rule is missing and the probe
		// leaked to the public upstreams.
		{"nxdomain", func(t *testing.T, r *http.Request) []byte { return probeResponse(t, r, 3) }},
		{"servfail", func(t *testing.T, r *http.Request) []byte { return probeResponse(t, r, 2) }},
		{"bare header without question", func(t *testing.T, r *http.Request) []byte {
			resp := probeResponse(t, r, 0)
			if len(resp) >= 12 {
				resp = resp[:12]
			}
			return resp
		}},
		{"mismatched id", func(t *testing.T, r *http.Request) []byte {
			resp := probeResponse(t, r, 0)
			if len(resp) >= 2 {
				resp[0], resp[1] = 0xff, 0xff
			}
			return resp
		}},
		{"noerror without answer", func(t *testing.T, r *http.Request) []byte {
			resp := probeResponse(t, r, 0)
			resp[7] = 0
			return resp
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dns-message")
				_, _ = w.Write(tc.build(t, r))
			}))
			defer backend.Close()
			g := testGateway(t, backend.URL, 100, 10)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
			}
		})
	}
}
func TestReadinessQueryAsksForTheLocalName(t *testing.T) {
	q, id := buildReadinessDNSQuery(0x1234)
	if id != 0x1234 || q[0] != 0x34 || q[1] != 0x12 {
		t.Fatalf("id not encoded: %x", q[:2])
	}
	var labels []string
	for i := 12; q[i] != 0; i += int(q[i]) + 1 {
		labels = append(labels, string(q[i+1:i+1+int(q[i])]))
	}
	if got := strings.Join(labels, "."); got != readinessProbeName {
		t.Fatalf("probe asks for %q, want %q", got, readinessProbeName)
	}
}
func TestConcurrentReadyzShareOneProbe(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(probeResponse(t, r, 0))
	}))
	defer backend.Close()
	g := testGateway(t, backend.URL, 100, 10)
	g.readyCacheTTL = time.Minute
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			codes <- w.Code
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for i := 0; i < n; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("readyz got %d", c)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("backend probed %d times, want 1 shared probe", got)
	}
}
func TestLimiterReleasesMemoryAfterSpike(t *testing.T) {
	l := newFixedWindowLimiter(1, time.Minute)
	l.now = func() time.Time { return time.Unix(120, 0) }
	for i := 0; i <= limiterShrinkThreshold; i++ {
		l.admit("k" + strconv.Itoa(i))
	}
	before := l.counts
	l.now = func() time.Time { return time.Unix(180, 0) }
	l.admit("fresh")
	if len(l.counts) != 1 {
		t.Fatalf("counts after rollover=%d, want 1", len(l.counts))
	}
	before["probe"] = 1
	if _, shared := l.counts["probe"]; shared {
		t.Fatal("limiter kept the oversized map after a spike")
	}
}
func TestBackendNosniffHeaderIsNotDuplicated(t *testing.T) {
	src := http.Header{"X-Content-Type-Options": {"nosniff"}, "Cache-Control": {"max-age=30"}}
	dst := http.Header{"X-Content-Type-Options": {"nosniff"}}
	copyResponseHeaders(dst, src)
	if got := dst.Values("X-Content-Type-Options"); len(got) != 1 {
		t.Fatalf("X-Content-Type-Options=%v, want one value", got)
	}
	if dst.Get("Cache-Control") != "max-age=30" {
		t.Fatal("Cache-Control from MosDNS not relayed")
	}
}
func TestIPv4MappedClientIsUnmapped(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:1", 100, 1)
	r := httptest.NewRequest(http.MethodGet, "/dns-query", nil)
	r.Header.Set("X-Forwarded-For", "::ffff:198.51.100.9")
	got := g.clientIP(r)
	if got.String() != "198.51.100.9" || rateLimitKey(got) != "198.51.100.9" {
		t.Fatalf("got %s / %s", got, rateLimitKey(got))
	}
}
