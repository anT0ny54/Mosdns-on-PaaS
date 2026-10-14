package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// regGateway is a gateway with the default limits, the given client (nil
// for the production client) and a 5 s upstream timeout.
func regGateway(t testing.TB, backend string, client *http.Client) *gateway {
	return newTestGateway(t, gatewayOptions{BackendURL: backend, Client: client, UpstreamTimeout: 5 * time.Second})
}

// asyncBodyTransport returns an error immediately and reads the request
// body only later, from another goroutine, as a real transport may do
// (early response, cancellation, failed write).
type asyncBodyTransport struct {
	got chan []byte
}

func (t *asyncBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		b, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		t.got <- b
	}()
	return nil, errors.New("backend down")
}

// The body handed to the transport must not alias the pooled read buffer:
// after the handler returns, other requests reuse the pool, and a transport
// that reads the body late must still see the original bytes.
func TestTransportBodyIsNotAliasedToPool(t *testing.T) {
	tr := &asyncBodyTransport{got: make(chan []byte, 1)}
	g := regGateway(t, "http://127.0.0.1:9/dns-query", &http.Client{Transport: tr})
	want := bytes.Repeat([]byte{0xAB}, 32)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, postReq(bytes.NewReader(want)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	// Churn the pool with different bytes while the transport still holds the body.
	for i := 0; i < 64; i++ {
		b := dnsMessageBufferPool.Get().(*[]byte)
		for j := range (*b)[:cap(*b)] {
			(*b)[:cap(*b)][j] = 0x00
		}
		dnsMessageBufferPool.Put(b)
	}
	select {
	case got := <-tr.got:
		if !bytes.Equal(got, want) {
			t.Fatalf("transport read %x, want %x (body aliased pooled memory)", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport never read the body")
	}
}

func TestServerWriteTimeoutIncludesReadBudget(t *testing.T) {
	// Shipped defaults: 2s body read + 0.1s queue + 2.5s upstream + 2s margin
	// = 6.6s, below the 8s floor, so the floor applies.
	if got := serverWriteTimeout(defaultBodyReadTimeout, defaultQueueWait, defaultUpstreamTO); got != defaultWriteTimeout {
		t.Fatalf("default budget: got %v, want %v", got, defaultWriteTimeout)
	}
	// Raised timeouts must cover body read + queue + upstream + margin:
	// 5s + 5s + 5s + 2s = 17s.
	if got, want := serverWriteTimeout(5*time.Second, 5*time.Second, 5*time.Second), 17*time.Second; got != want {
		t.Fatalf("raised budget: got %v, want %v", got, want)
	}
}

// The backend-latency average must reflect the full exchange (headers and
// body), so a backend that answers headers quickly but stalls or truncates
// the body cannot look healthy in the gauge.
func TestBackendLatencyRecordedAfterBodyRead(t *testing.T) {
	slowBodyBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers out fast; body delayed
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write(make([]byte, 32))
	}))
	defer slowBodyBackend.Close()

	g := regGateway(t, slowBodyBackend.URL+"/dns-query", slowBodyBackend.Client())
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, postReq(bytes.NewReader(make([]byte, 32))))

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

	g2 := regGateway(t, brokenBackend.URL+"/dns-query", brokenBackend.Client())
	rec2 := httptest.NewRecorder()
	g2.ServeHTTP(rec2, postReq(bytes.NewReader(make([]byte, 32))))

	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec2.Code)
	}
	if got := g2.metrics.backendLatencySamples.Load(); got != 0 {
		t.Fatalf("backendLatencySamples = %d, want 0 for a truncated body", got)
	}
}

// A slow (but allowed) POST upload must not eat the deadline reserved for the
// queue wait and the upstream timeout: a 2.5s upload followed by a 2s upstream
// timeout must still end in 504, not a dropped connection.
func TestWriteTimeoutCoversSlowBodyAndRaisedTimeouts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second) // outlives the gateway's 2s upstream timeout
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, 32))
	}))
	defer backend.Close()

	queueWait := 100 * time.Millisecond
	upstreamTimeout := 2 * time.Second
	g := regGateway(t, backend.URL+"/dns-query", backend.Client())
	g.upstreamTimeout = upstreamTimeout
	g.queueWait = queueWait
	g.bodyReadTimeout = 3 * time.Second // the trickled upload below takes ~2.5s

	server := &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           g,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      serverWriteTimeout(g.bodyReadTimeout, queueWait, upstreamTimeout),
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

// Only a complete, valid 2xx DNS answer may feed the backend-latency gauge.
// Every rejected or relayed-error response must leave the sample count at 0
// while still being counted as a backend error.
func TestBackendLatencyNotRecordedForRejectedResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		size        int
		wantStatus  int
	}{
		{"oversize body", http.StatusOK, "application/dns-message", maxDNSMessageBytes + 1, http.StatusBadGateway},
		{"wrong content type", http.StatusOK, "text/plain", 32, http.StatusBadGateway},
		{"body shorter than DNS header", http.StatusOK, "application/dns-message", 4, http.StatusBadGateway},
		{"backend 5xx", http.StatusServiceUnavailable, "application/dns-message", 32, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = w.Write(make([]byte, tc.size))
			}))
			defer backend.Close()

			g := regGateway(t, backend.URL+"/dns-query", backend.Client())
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, postReq(bytes.NewReader(make([]byte, 32))))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := g.metrics.backendLatencySamples.Load(); got != 0 {
				t.Fatalf("backendLatencySamples = %d, want 0", got)
			}
			if got := g.metrics.backendLatencyNanos.Load(); got != 0 {
				t.Fatalf("backendLatencyNanos = %d, want 0", got)
			}
			if got := g.metrics.backendErrorTotal.Load(); got != 1 {
				t.Fatalf("backendErrorTotal = %d, want 1", got)
			}
		})
	}
}

// getBodyTransport checks the retry body the transport can request through
// GetBody: it must carry the same bytes as the original POST body and be
// obtainable more than once.
type getBodyTransport struct {
	t    *testing.T
	want []byte
}

func (tr *getBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody == nil {
		tr.t.Error("POST request has no GetBody; stale-connection retries would be lost")
	} else {
		for i := 0; i < 2; i++ {
			rc, err := req.GetBody()
			if err != nil {
				tr.t.Errorf("GetBody #%d: %v", i+1, err)
				continue
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Equal(got, tr.want) {
				tr.t.Errorf("GetBody #%d returned %x, want %x", i+1, got, tr.want)
			}
		}
	}
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(make([]byte, 32))),
		Request:    req,
	}, nil
}

func TestPOSTRetryBodyMatchesOriginal(t *testing.T) {
	msg := []byte("0123456789abcdef")
	g := regGateway(t, "http://127.0.0.1:9/dns-query", &http.Client{Transport: &getBodyTransport{t: t, want: msg}})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, postReq(bytes.NewReader(msg)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// A /readyz probe aborted because the client disconnected says nothing about
// backend health: it must not count as not_ready and no response is written.
func TestReadyProbeCanceledByClientIsNotCounted(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer backend.Close()
	defer close(release)
	g := regGateway(t, backend.URL, nil)
	g.readyCacheTTL = defaultReadyCacheTTL

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readyz did not return after the client went away")
	}
	if got := g.metrics.readyFailTotal.Load(); got != 0 {
		t.Fatalf("canceled probe counted as %d not_ready probes, want 0", got)
	}
	if got := g.metrics.responses5xxTotal.Load(); got != 0 {
		t.Fatalf("canceled probe counted %d 5xx responses, want 0", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("response written for a disconnected client: %q", rec.Body.String())
	}
	if !g.readyAt.IsZero() {
		t.Fatal("canceled probe poisoned the readiness cache")
	}
}

func startRegServer(t *testing.T, g *gateway) net.Listener {
	t.Helper()
	server := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      serverWriteTimeout(g.bodyReadTimeout, g.queueWait, g.upstreamTimeout),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(ln) }()
	return ln
}

// A POST body that is not complete within the body-read budget is rejected
// and its active slot freed, instead of being held for the whole ReadTimeout.
func TestSlowPOSTBodyIsCutOffAndFreesSlot(t *testing.T) {
	g := regGateway(t, "http://127.0.0.1:9/dns-query", nil)
	g.bodyReadTimeout = 200 * time.Millisecond
	ln := startRegServer(t, g)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	start := time.Now()
	fmt.Fprintf(conn, "POST /dns-query HTTP/1.1\r\nHost: x\r\nContent-Type: application/dns-message\r\nContent-Length: 64\r\n\r\nabcd")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("slow body held the request for %s", d)
	}
	deadline := time.Now().Add(time.Second)
	for len(g.activeSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(g.activeSlots); n != 0 {
		t.Fatalf("active slots held after cut-off: %d", n)
	}
}

// The short body deadline is cleared once the body is read: a backend that
// answers after the body-read budget has passed must still be relayed (the
// server's background read must not cancel the request).
func TestBodyDeadlineDoesNotCancelSlowBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(make([]byte, 32))
	}))
	defer backend.Close()
	g := regGateway(t, backend.URL+"/dns-query", nil)
	g.bodyReadTimeout = 100 * time.Millisecond
	ln := startRegServer(t, g)
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 2; i++ { // second request reuses the keep-alive connection
		resp, err := client.Post("http://"+ln.Addr().String()+"/dns-query", "application/dns-message", bytes.NewReader(make([]byte, 32)))
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
}
