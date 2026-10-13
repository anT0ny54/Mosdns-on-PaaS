package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var errRegBackendDown = errors.New("backend down")

// holdingErrorTransport returns an error immediately but closes the request
// body only after a delay, mimicking a transport that finishes with the body
// asynchronously (early upstream response, cancellation, or a failed write).
type holdingErrorTransport struct {
	delay      time.Duration
	closeOnce  sync.Once
	bodyClosed chan struct{}
}

func (t *holdingErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	go func() {
		time.Sleep(t.delay)
		_ = req.Body.Close()
		t.closeOnce.Do(func() { close(t.bodyClosed) })
	}()
	return nil, errRegBackendDown
}

func newRegTestGateway(backend string, client *http.Client) *gateway {
	return &gateway{
		backendURL:       backend,
		client:           client,
		limiter:          newFixedWindowLimiterWithMaxKeys(1000, time.Minute, 1000),
		activeSlots:      make(chan struct{}, 16),
		processingSlots:  make(chan struct{}, 8),
		queueWait:        100 * time.Millisecond,
		upstreamTimeout:  5 * time.Second,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
	}
}

func newRegDoHPostRequest(t *testing.T, msg []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dns-query", bytes.NewReader(msg))
	req.Header.Set("Content-Type", "application/dns-message")
	req.RemoteAddr = "127.0.0.1:4242"
	return req
}

// Regression test for the pooled-buffer/transport-error race: the handler
// must not return (making the pooled POST body available to another request)
// before the transport has closed the outgoing request body.
func TestPooledBodyHeldUntilTransportCloses(t *testing.T) {
	tr := &holdingErrorTransport{delay: 150 * time.Millisecond, bodyClosed: make(chan struct{})}
	g := newRegTestGateway("http://127.0.0.1:9/dns-query", &http.Client{Transport: tr})

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(rec, newRegDoHPostRequest(t, make([]byte, 32)))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("handler returned before the transport closed the request body; the pooled buffer could be reused while the transport still references it")
	case <-time.After(60 * time.Millisecond):
	}

	select {
	case <-tr.bodyClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("transport never closed the request body")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after the transport closed the request body")
	}

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestServerWriteTimeoutIncludesReadBudget(t *testing.T) {
	// Shipped defaults: the full budget (5s read + queue + upstream + 2s
	// margin = 9.6s) exceeds the 8s floor, so the floor is NOT returned; the
	// whole budget must be covered.
	if got, want := serverWriteTimeout(defaultReadTimeout, defaultQueueWait, defaultUpstreamTO),
		defaultReadTimeout+defaultQueueWait+defaultUpstreamTO+2*time.Second; got != want {
		t.Fatalf("default budget: got %v, want %v", got, want)
	}
	// Raised queue/upstream timeouts must also cover the body-read budget:
	// 5s upload + 5s queue + 5s upstream + 2s margin = 17s. The old formula
	// (queue+upstream+2s = 12s) would drop the connection on this budget.
	if got, want := serverWriteTimeout(5*time.Second, 5*time.Second, 5*time.Second), 17*time.Second; got != want {
		t.Fatalf("raised budget: got %v, want %v", got, want)
	}
}

// The backend-latency average must reflect the full exchange (headers and
// body), so a backend that answers headers quickly but stalls or truncates
// the body cannot look healthy in the gauge.
func TestBackendLatencyRecordedAfterBodyRead(t *testing.T) {
	okBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers out fast; body delayed
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write(make([]byte, 32))
	}))
	defer okBackend.Close()

	g := newRegTestGateway(okBackend.URL+"/dns-query", okBackend.Client())
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, newRegDoHPostRequest(t, make([]byte, 32)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := g.metrics.backendLatencySamples.Load(); got != 1 {
		t.Fatalf("backendLatencySamples = %d, want 1", got)
	}
	if got := g.metrics.backendLatencyNanos.Load(); got < uint64(100*time.Millisecond) {
		t.Fatalf("backendLatencyNanos = %d, want >= 100ms (must include body transfer time)", got)
	}

	brokenBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 16)) // fewer bytes than promised: truncated body
	}))
	defer brokenBackend.Close()

	g2 := newRegTestGateway(brokenBackend.URL+"/dns-query", brokenBackend.Client())
	rec2 := httptest.NewRecorder()
	g2.ServeHTTP(rec2, newRegDoHPostRequest(t, make([]byte, 32)))

	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec2.Code)
	}
	if got := g2.metrics.backendLatencySamples.Load(); got != 0 {
		t.Fatalf("backendLatencySamples = %d, want 0 for a truncated body", got)
	}
}

// A slow POST upload must not eat the deadline reserved for the queue wait and
// the upstream timeout: with the OLD formula (queue+upstream+2s = ~4.1s) a
// 2.5s upload followed by a 2s upstream timeout would be cut off before the
// gateway could write 504. The new formula (5s read + 0.1s queue + 2s upstream
// + 2s margin ~= 9.1s) delivers the 504.
func TestWriteTimeoutCoversSlowBodyAndRaisedTimeouts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second) // outlives the gateway's 2s upstream timeout
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, 32))
	}))
	defer backend.Close()

	queueWait := 100 * time.Millisecond
	upstreamTimeout := 2 * time.Second
	g := newRegTestGateway(backend.URL+"/dns-query", backend.Client())
	g.upstreamTimeout = upstreamTimeout
	g.queueWait = queueWait

	server := &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           g,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      serverWriteTimeout(defaultReadTimeout, queueWait, upstreamTimeout),
	}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer server.Close()
	go func() { _ = server.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := make([]byte, 32)
	fmt.Fprintf(conn, "POST /dns-query HTTP/1.1\r\nHost: %s\r\nContent-Type: application/dns-message\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", ln.Addr().String(), len(msg))
	// Trickle the body over ~2.5s (well under the 5s ReadTimeout, but more
	// than the 2s margin the old write-deadline formula assumed).
	for written := 0; written < len(msg); {
		n, werr := conn.Write(msg[written : written+4])
		if werr != nil {
			t.Fatalf("body write: %v", werr)
		}
		written += n
		time.Sleep(320 * time.Millisecond)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("response: %v (connection dropped before the gateway could write 504?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusGatewayTimeout)
	}
}
