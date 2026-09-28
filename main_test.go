package main

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testGateway(backend string, limit int, concurrency int) *gateway {
	limiter := newFixedWindowLimiter(limit, 60*time.Second)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	return &gateway{
		backendURL:       backend,
		client:           newHTTPClient(),
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
		_, _ = w.Write([]byte{0x01})
	}))
	defer backend.Close()

	g := testGateway(backend.URL, 2, 10)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
		r.Header.Set("Content-Type", "application/dns-message")
		r.Header.Set("X-Forwarded-For", "192.0.2.10")
		r.ContentLength = 1
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want %d", i+1, w.Code, http.StatusOK)
		}
	}

	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
	r.Header.Set("Content-Type", "application/dns-message")
	r.Header.Set("X-Forwarded-For", "192.0.2.10")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	r = httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
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
		_, _ = w.Write([]byte{0x01, 0x02})
	}))
	defer backend.Close()

	g := testGateway(backend.URL, 100, 10)

	badType := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
	badType.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, badType)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type: got %d", w.Code)
	}

	parameterizedType := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
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
		_, _ = w.Write([]byte{0x01})
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
	valid := base64.RawURLEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0x03})
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
	if isValidDoHGetParameter(valid, maxDNSMessageBytes+1) {
		t.Fatal("validation accepted a configured size above the hard DoH limit")
	}
}

func TestDoHGETParameterValidationAllocs(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})
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
		_, _ = w.Write([]byte{0x01})
	}))
	defer backend.Close()

	g := testGateway(backend.URL, 100, 1)

	var wg sync.WaitGroup
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
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
		r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
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
		_, _ = w.Write([]byte{0x01})
	}))
	defer backend.Close()

	limiter := newFixedWindowLimiterWithMaxKeys(100000, time.Minute, 16)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	g := &gateway{
		backendURL:       backend.URL,
		client:           newHTTPClient(),
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

	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
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
	if defaultMaxActiveRequests != 512 {
		t.Fatalf("default active-request capacity=%d, want 512", defaultMaxActiveRequests)
	}
	if defaultConcurrency != 20 {
		t.Fatalf("default backend processing cap=%d, want 20", defaultConcurrency)
	}
	transport := newHTTPClient().Transport.(*http.Transport)
	if transport.MaxIdleConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxIdleConnsPerHost=%d, want %d", transport.MaxIdleConnsPerHost, defaultConcurrency)
	}
	if transport.MaxConnsPerHost != defaultConcurrency {
		t.Fatalf("MaxConnsPerHost=%d, want %d", transport.MaxConnsPerHost, defaultConcurrency)
	}
}

func TestActiveRequestBurstCapacity(t *testing.T) {
	const burst = 128
	const processingCap = 20

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
		_, _ = w.Write([]byte{0x01})
	}))
	defer backend.Close()

	limiter := newFixedWindowLimiterWithMaxKeys(100000, time.Minute, burst)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	g := &gateway{
		backendURL:       backend.URL,
		client:           newHTTPClient(),
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
			r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
			r.Header.Set("Content-Type", "application/dns-message")
			r.Header.Set("X-Forwarded-For", "198.18.0.1")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			results <- w.Code
		}()
	}

	// Once the burst is admitted, exactly 20 should have
	// reached the MosDNS backend and the remaining requests should be queued.
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

func TestConnectionHeaderTokensAreStripped(t *testing.T) {
	src := make(http.Header)
	src.Add("Connection", "X-Request-Token, X-Another")
	src.Set("X-Request-Token", "secret")
	src.Set("X-Another", "secret2")
	src.Set("X-Keep", "safe")

	dst := make(http.Header)
	copyRequestHeaders(dst, src)
	if dst.Get("X-Request-Token") != "" || dst.Get("X-Another") != "" {
		t.Fatalf("connection-token request headers leaked: %v", dst)
	}
	if dst.Get("X-Keep") != "safe" {
		t.Fatalf("ordinary request header lost: %v", dst)
	}

	dst = make(http.Header)
	copyResponseHeaders(dst, src)
	if dst.Get("X-Request-Token") != "" || dst.Get("X-Another") != "" {
		t.Fatalf("connection-token response headers leaked: %v", dst)
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
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{0x01})
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
	if called {
		t.Fatal("backend was called after request body read failure")
	}
}

// probeReplyIDBytes extracts the transaction ID (the first two wire bytes)
// from a readiness probe's base64url-encoded "dns" query parameter, so a
// test backend can echo it back rather than guessing the gateway's
// internal sequence counter.
func probeReplyIDBytes(t *testing.T, r *http.Request) (byte, byte) {
	dns := r.URL.Query().Get("dns")
	if dns == "" {
		t.Fatal("readiness probe did not send a dns parameter")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(dns)
	if err != nil || len(decoded) < 2 {
		t.Fatalf("readiness probe sent an unparseable dns parameter: %q", dns)
	}
	return decoded[0], decoded[1]
}

func TestReadyEndpointTracksBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idLo, idHi := probeReplyIDBytes(t, r)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{idLo, idHi, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0})
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
		idLo, idHi := probeReplyIDBytes(t, r)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{idLo, idHi, 0x81, 0x82, 0, 1, 0, 0, 0, 0, 0, 0})
	}))
	defer backend.Close()

	g := testGateway(backend.URL, 100, 10)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("SERVFAIL backend: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyEndpointRejectsMismatchedID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeReplyIDBytes(t, r) // validate the probe shape, then ignore the real ID
		w.Header().Set("Content-Type", "application/dns-message")
		// Deliberately echo the wrong transaction ID; the gateway must
		// treat this as not ready even though QR/RCODE look fine.
		_, _ = w.Write([]byte{0xff, 0xff, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0})
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
		switch r.Header.Get("X-Test-Mode") {
		case "oversize":
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(make([]byte, maxDNSMessageBytes+1))
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte{0x01})
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
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
			r.Header.Set("Content-Type", "application/dns-message")
			r.Header.Set("X-Test-Mode", tc.mode)
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
	r := httptest.NewRequest(http.MethodPost, "/dns-query", strings.NewReader("x"))
	r.Header.Set("Content-Type", "application/dns-message")
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("max-size response: got %d, want %d", w.Code, http.StatusOK)
	}
}

func TestGETRejectsPaddedBase64URL(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{0x01})
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

func TestGETValidBase64URL(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 0x03}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dns") != encoded {
			t.Fatalf("backend got dns=%q, want %q", r.URL.Query().Get("dns"), encoded)
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
