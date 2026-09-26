package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stagedListener struct {
	mu          sync.Mutex
	conns       []net.Conn
	idx         int
	allowSecond <-chan struct{}
}

func (l *stagedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	idx := l.idx
	if idx >= len(l.conns) {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	c := l.conns[idx]
	l.idx++
	wait := l.allowSecond != nil && idx == 1
	l.mu.Unlock()
	if wait {
		<-l.allowSecond
	}
	return c, nil
}

func (l *stagedListener) Close() error   { return nil }
func (l *stagedListener) Addr() net.Addr { return stagedAddr("test") }

type stagedAddr string

func (a stagedAddr) Network() string { return "test" }
func (a stagedAddr) String() string  { return string(a) }

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	ip, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return ip
}

func connSlotCount(g *publicGuard) int {
	total := 0
	for i := range g.connections.shards {
		total += len(g.connections.shards[i].entries)
	}
	return total
}

func connCount(g *publicGuard, ip netip.Addr) int {
	ip = ip.Unmap()
	sh := g.connections.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e := findConnLocked(sh, ip); e != nil {
		return e.connections
	}
	return 0
}

func TestConnectionStateHasBoundedSpareSlots(t *testing.T) {
	g := newPublicGuard(128, 16)
	if got := connSlotCount(g); got != 256 {
		t.Fatalf("connection table allocated %d slots, want 256", got)
	}
}

func TestConnectionStateHardCap(t *testing.T) {
	g := newPublicGuard(maxConnStateHardCap+128, 16)
	if got := connSlotCount(g); got != maxConnStateHardCap {
		t.Fatalf("connection table allocated %d slots, want hard cap %d", got, maxConnStateHardCap)
	}
	g = newPublicGuard(maxConnStateHardCap/2+1, 16)
	if got := connSlotCount(g); got != maxConnStateHardCap {
		t.Fatalf("2x connection table must stop at hard cap, got %d", got)
	}
}

func TestGlobalConnectionLimitIsAtomic(t *testing.T) {
	g := newPublicGuard(2, 0)
	if !g.acquireGlobalConn() || !g.acquireGlobalConn() {
		t.Fatal("first two global slots must be accepted")
	}
	if g.acquireGlobalConn() {
		t.Fatal("third global slot must be rejected")
	}
	g.releaseGlobalConn()
	if !g.acquireGlobalConn() {
		t.Fatal("released global slot must be reusable")
	}
	g.releaseGlobalConn()
	g.releaseGlobalConn()
	if got := atomic.LoadInt64(&g.globalConn); got != 0 {
		t.Fatalf("global connection count = %d, want 0", got)
	}
}

func TestPerSourceConnectionRejectsInvalidSource(t *testing.T) {
	g := newPublicGuard(8, 0)
	if g.connections.acquireConn(netip.Addr{}) {
		t.Fatal("invalid source must never be accepted")
	}
}

func TestPerSourceConnectionCanonicalizesIPv4MappedAddress(t *testing.T) {
	g := newPublicGuard(8, 1)
	v4 := mustAddr(t, "192.0.2.25")
	mapped := mustAddr(t, "::ffff:192.0.2.25")

	if g.connections.shardFor(mapped) != g.connections.shardFor(v4) {
		t.Fatal("IPv4 and IPv4-mapped forms must select the same connection shard")
	}
	if !g.connections.acquireConn(mapped) {
		t.Fatal("IPv4-mapped source should acquire the per-source slot")
	}
	if g.connections.acquireConn(v4) {
		t.Fatal("IPv4 and IPv4-mapped forms must share the same per-source slot")
	}
	g.connections.releaseConn(v4)
	if !g.connections.acquireConn(v4) {
		t.Fatal("released canonical slot must be reusable")
	}
	g.connections.releaseConn(mapped)
	g.connections.releaseConn(v4)
	if got := connCount(g, v4); got != 0 {
		t.Fatalf("canonical source connection count = %d, want 0", got)
	}
}

func TestPerSourceConnectionLimit(t *testing.T) {
	g := newPublicGuard(8, 2)
	ip := mustAddr(t, "198.51.100.9")
	if !g.connections.acquireConn(ip) || !g.connections.acquireConn(ip) {
		t.Fatal("first two per-source connections must be accepted")
	}
	if g.connections.acquireConn(ip) {
		t.Fatal("third per-source connection must be rejected")
	}
	g.connections.releaseConn(ip)
	if !g.connections.acquireConn(ip) {
		t.Fatal("released per-source connection must be reusable")
	}
	g.connections.releaseConn(ip)
	g.connections.releaseConn(ip)
}

func TestPerSourceConnectionLimitSupports64ConcurrentConnections(t *testing.T) {
	const (
		limit = 64
		extra = 1
	)
	g := newPublicGuard(128, limit)
	ip := mustAddr(t, "198.51.100.64")

	start := make(chan struct{})
	results := make(chan bool, limit+extra)
	var ready sync.WaitGroup
	ready.Add(limit + extra)
	for i := 0; i < limit+extra; i++ {
		go func() {
			ready.Done()
			<-start
			results <- g.connections.acquireConn(ip)
		}()
	}
	ready.Wait()
	close(start)

	accepted := 0
	for i := 0; i < limit+extra; i++ {
		if <-results {
			accepted++
		}
	}
	if accepted != limit {
		t.Fatalf("concurrent per-source accepts = %d, want exactly %d", accepted, limit)
	}
	if got := connCount(g, ip); got != limit {
		t.Fatalf("source connection count = %d, want %d", got, limit)
	}

	for i := 0; i < limit; i++ {
		g.connections.releaseConn(ip)
	}
	if got := connCount(g, ip); got != 0 {
		t.Fatalf("source connection count after release = %d, want 0", got)
	}
}

func TestGlobalConnectionCeilingSupports128ConnectionsAcrossMultipleSources(t *testing.T) {
	const (
		globalLimit = 128
		sourceLimit = 64
		sources     = 4
	)
	g := newPublicGuard(globalLimit, sourceLimit)
	ips := make([]netip.Addr, sources)
	for i := range ips {
		ips[i] = mustAddr(t, "198.51.100."+strconv.Itoa(10+i))
	}

	start := make(chan struct{})
	results := make(chan bool, globalLimit)
	var ready sync.WaitGroup
	ready.Add(globalLimit)
	for i := 0; i < globalLimit; i++ {
		ip := ips[i%len(ips)]
		go func(ip netip.Addr) {
			ready.Done()
			<-start
			if !g.acquireGlobalConn() {
				results <- false
				return
			}
			if !g.connections.acquireConn(ip) {
				g.releaseGlobalConn()
				results <- false
				return
			}
			results <- true
		}(ip)
	}
	ready.Wait()
	close(start)

	accepted := 0
	for i := 0; i < globalLimit; i++ {
		if <-results {
			accepted++
		}
	}
	if accepted != globalLimit {
		t.Fatalf("concurrent global accepts = %d, want exactly %d", accepted, globalLimit)
	}
	if got := atomic.LoadInt64(&g.globalConn); got != globalLimit {
		t.Fatalf("global connection count = %d, want %d", got, globalLimit)
	}
	for i, ip := range ips {
		if got := connCount(g, ip); got != globalLimit/sources {
			t.Fatalf("source %d connection count = %d, want %d", i, got, globalLimit/sources)
		}
	}
	if g.acquireGlobalConn() {
		t.Fatal("129th global connection must be rejected while 128 are active")
	}

	for _, ip := range ips {
		for i := 0; i < globalLimit/sources; i++ {
			g.connections.releaseConn(ip)
			g.releaseGlobalConn()
		}
	}
	if got := atomic.LoadInt64(&g.globalConn); got != 0 {
		t.Fatalf("global connection count after release = %d, want 0", got)
	}
}

func TestKeepAliveReusesOneGuardedConnectionForRepeatedDoHRequests(t *testing.T) {
	const requests = 100
	const dohBody = "\x00\x07\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
	g := newPublicGuard(128, 64)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() failed: %v", err)
	}
	listener := &guardedListener{Listener: raw, guard: g}

	var mu sync.Mutex
	var firstConn *guardedConn
	var handlerErr error
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, ok := r.Context().Value(connGuardKey{}).(*guardedConn)
		if !ok || state == nil {
			mu.Lock()
			handlerErr = errors.New("missing guarded connection in request context")
			mu.Unlock()
			http.Error(w, "guard missing", http.StatusInternalServerError)
			return
		}
		ip := clientIPAddr(r)
		if !state.bindSource(ip) {
			mu.Lock()
			handlerErr = errors.New("keep-alive request lost its per-source connection slot")
			mu.Unlock()
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
		mu.Lock()
		if firstConn == nil {
			firstConn = state
		} else if firstConn != state && handlerErr == nil {
			handlerErr = errors.New("repeated keep-alive requests used different guarded connections")
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte(dohBody))
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if gc, ok := c.(*guardedConn); ok {
				return context.WithValue(ctx, connGuardKey{}, gc)
			}
			return ctx
		},
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Server.Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop after Close()")
		}
	})

	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		DisableKeepAlives:     false,
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       1,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	body := []byte(dohBody)

	for i := 0; i < requests; i++ {
		req, err := http.NewRequest(http.MethodPost, "http://"+raw.Addr().String()+"/dns-query", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest() failed: %v", err)
		}
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("X-Forwarded-For", "198.51.100.200")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("keep-alive DoH request %d failed: %v", i+1, err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading DoH response %d failed: %v", i+1, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DoH response %d status = %d, want 200", i+1, resp.StatusCode)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("DoH response %d body changed", i+1)
		}
		if got := atomic.LoadInt64(&g.globalConn); got != 1 {
			t.Fatalf("global connection count after request %d = %d, want 1", i+1, got)
		}
		if got := connCount(g, mustAddr(t, "198.51.100.200")); got != 1 {
			t.Fatalf("source connection count after request %d = %d, want 1", i+1, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if handlerErr != nil {
		t.Fatal(handlerErr)
	}
	if firstConn == nil {
		t.Fatal("no guarded connection observed")
	}
}

func TestClientIPAddrUsesTrustedFinalXFFValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/health", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Add("X-Forwarded-For", "10.0.0.1, 203.0.113.10")
	r.Header.Add("X-Forwarded-For", "198.51.100.7")
	want := mustAddr(t, "198.51.100.7")
	if got := clientIPAddr(r); got != want {
		t.Fatalf("clientIPAddr = %v, want %v", got, want)
	}
}

func TestClientIPAddrUnmapsIPv4MappedAddress(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/health", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "::ffff:192.0.2.10")
	want := mustAddr(t, "192.0.2.10")
	if got := clientIPAddr(r); got != want {
		t.Fatalf("clientIPAddr = %v, want %v", got, want)
	}
}

func TestGuardedListenerClosesOverCapImmediately(t *testing.T) {
	g := newPublicGuard(1, 0)
	if !g.acquireGlobalConn() {
		t.Fatal("failed to occupy the only global slot")
	}
	server1, client1 := net.Pipe()
	server2, client2 := net.Pipe()
	defer client1.Close()
	defer client2.Close()
	allowSecond := make(chan struct{})
	l := &stagedListener{conns: []net.Conn{server1, server2}, allowSecond: allowSecond}
	gl := &guardedListener{Listener: l, guard: g}
	type acceptResult struct {
		c   net.Conn
		err error
	}
	result := make(chan acceptResult, 1)
	go func() {
		c, err := gl.Accept()
		result <- acceptResult{c: c, err: err}
	}()

	_ = client1.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := client1.Read(buf[:]); err == nil {
		t.Fatal("over-cap connection remained open")
	}

	g.releaseGlobalConn()
	close(allowSecond)

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("guardedListener.Accept() error = %v", r.err)
		}
		if r.c == nil {
			t.Fatal("guardedListener.Accept() returned nil connection")
		}
		if got := atomic.LoadInt64(&g.globalConn); got != 1 {
			t.Fatalf("global connection count = %d, want 1 for admitted socket", got)
		}
		_ = r.c.Close()
	case <-time.After(time.Second):
		t.Fatal("admitted connection was not returned")
	}
	if got := atomic.LoadInt64(&g.globalConn); got != 0 {
		t.Fatalf("global connection count after close = %d, want 0", got)
	}
}

func TestGuardedConnRebindsSharedEdgeConnection(t *testing.T) {
	g := newPublicGuard(8, 1)
	server, client := net.Pipe()
	defer client.Close()
	c := &guardedConn{Conn: server, guard: g}
	a := mustAddr(t, "192.0.2.1")
	b := mustAddr(t, "192.0.2.2")
	if !c.bindSource(a) {
		t.Fatal("initial source binding failed")
	}
	if !c.bindSource(a) {
		t.Fatal("re-binding the same source must be a no-op")
	}
	if got := connCount(g, a); got != 1 {
		t.Fatalf("source A connection count = %d, want 1", got)
	}
	if !c.bindSource(b) {
		t.Fatal("shared connection was rejected when its client changed")
	}
	if got := connCount(g, a); got != 0 {
		t.Fatalf("source A connection count after rebind = %d, want 0", got)
	}
	if got := connCount(g, b); got != 1 {
		t.Fatalf("source B connection count after rebind = %d, want 1", got)
	}
	_ = c.Close()
	if got := connCount(g, b); got != 0 {
		t.Fatalf("source B connection count after close = %d, want 0", got)
	}
	if c.bindSource(a) {
		t.Fatal("a closed connection must not take a new slot")
	}
}

func TestGuardedConnRebindRespectsPerSourceLimit(t *testing.T) {
	g := newPublicGuard(8, 1)
	s1, c1 := net.Pipe()
	s2, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	conn1 := &guardedConn{Conn: s1, guard: g}
	conn2 := &guardedConn{Conn: s2, guard: g}
	a := mustAddr(t, "192.0.2.1")
	b := mustAddr(t, "192.0.2.2")
	if !conn1.bindSource(a) || !conn2.bindSource(b) {
		t.Fatal("initial bindings failed")
	}
	if conn2.bindSource(a) {
		t.Fatal("rebinding onto a source already at its connection limit must fail")
	}
	if got := connCount(g, a); got != 1 {
		t.Fatalf("source A connection count = %d, want 1", got)
	}
	if got := connCount(g, b); got != 0 {
		t.Fatalf("source B slot must have been released by the failed move, got %d", got)
	}
	_ = conn1.Close()
	_ = conn2.Close()
}
