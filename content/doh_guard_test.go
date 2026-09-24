package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func (l *stagedListener) Close() error {
	return nil
}

func (l *stagedListener) Addr() net.Addr {
	return stagedAddr("test")
}

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

func TestSourceStateHardCap(t *testing.T) {
	tbl := newSourceTable(maxSourceStateHardCap + 128)
	if got := sourceSlotCount(tbl); got != maxSourceStateHardCap {
		t.Fatalf("source table allocated %d slots, want hard cap %d", got, maxSourceStateHardCap)
	}
}

// allowIP exercises the production key path (rateKey + allowRateKey).
func allowIP(tbl *sourceTable, ip netip.Addr, now time.Time, rate, burst float64) bool {
	return tbl.allowRateKey(rateKey(ip), now, rate, burst, false)
}

func sourceSlotCount(tbl *sourceTable) int {
	total := 0
	for i := range tbl.shards {
		total += len(tbl.shards[i].entries)
	}
	return total
}

func TestBoundedSourceState(t *testing.T) {
	tbl := newSourceTable(16)
	now := time.Unix(0, 1)
	for i := 1; i <= 200; i++ {
		ip := mustAddr(t, "192.0.2."+itoa(i%250+1))
		_ = allowIP(tbl, ip, now.Add(time.Duration(i)*time.Millisecond), 100, 100)
	}
	for i := range tbl.shards {
		sh := &tbl.shards[i]
		if len(sh.entries) > 2 {
			t.Fatalf("shard retained %d slots, want <=2", len(sh.entries))
		}
	}
}

func TestSourceTokenBucket(t *testing.T) {
	tbl := newSourceTable(8)
	ip := mustAddr(t, "203.0.113.7")
	base := time.Unix(100, 0)
	for i := 0; i < 3; i++ {
		if !allowIP(tbl, ip, base, 2, 3) {
			t.Fatalf("initial burst request %d denied", i+1)
		}
	}
	if allowIP(tbl, ip, base, 2, 3) {
		t.Fatal("request beyond burst unexpectedly allowed")
	}
	if !allowIP(tbl, ip, base.Add(500*time.Millisecond), 2, 3) {
		t.Fatal("one token should have refilled after 500ms")
	}
}

func TestInvalidSourceFailsClosedWhenRateLimited(t *testing.T) {
	tbl := newSourceTable(8)
	if allowIP(tbl, netip.Addr{}, time.Unix(100, 0), 5, 12) {
		t.Fatal("invalid source must not bypass a configured per-source rate limit")
	}
}

func TestPerSourceRejectDoesNotConsumeGlobalRate(t *testing.T) {
	g := newPublicGuard(8, 8, 0, 1, 1, 1, 2, 1, 1, 1, 1)
	base := time.Unix(200, 0)
	a := mustAddr(t, "203.0.113.10")
	b := mustAddr(t, "203.0.113.11")
	if !g.allowDoH(a, base) {
		t.Fatal("first source request should be allowed")
	}
	if g.allowDoH(a, base) {
		t.Fatal("second request from source A should be rejected by its own bucket")
	}
	if !g.allowDoH(b, base) {
		t.Fatal("source B should still consume the second global token")
	}
}

func TestGlobalConnectionLimitIsAtomic(t *testing.T) {
	g := newPublicGuard(2, 8, 0, 5, 12, 40, 80, 2, 4, 10, 20)
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
}

func TestPerSourceConnectionLimit(t *testing.T) {
	g := newPublicGuard(8, 8, 2, 5, 12, 40, 80, 2, 4, 10, 20)
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
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var out [20]byte
	i := len(out)
	for v > 0 {
		i--
		out[i] = byte('0' + v%10)
		v /= 10
	}
	return string(out[i:])
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

func TestGuardedListenerClosesOverCapImmediately(t *testing.T) {
	g := newPublicGuard(1, 8, 0, 1, 1, 1, 1, 1, 1, 1, 1)
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
		if got := g.globalCount(); got != 1 {
			t.Fatalf("global connection count = %d, want 1 for admitted socket", got)
		}
		_ = r.c.Close()
	case <-time.After(time.Second):
		t.Fatal("admitted connection was not returned")
	}
	if got := g.globalCount(); got != 0 {
		t.Fatalf("global connection count after close = %d, want 0", got)
	}
}

func (g *publicGuard) globalCount() int64 {
	return atomic.LoadInt64(&g.globalConn)
}

func TestSourceRateBucketsArePerIP(t *testing.T) {
	g := newPublicGuard(8, 32, 0, 1, 1, 100, 100, 1, 1, 10, 10)
	now := time.Unix(300, 0)
	ip := mustAddr(t, "192.0.2.44")
	otherIP := mustAddr(t, "192.0.2.45")

	if !g.allowDoH(ip, now) {
		t.Fatal("first request should be admitted")
	}
	if g.allowDoH(ip, now) {
		t.Fatal("same source IP must share one rate bucket regardless of Host")
	}
	if !g.allowDoH(otherIP, now) {
		t.Fatal("different source IP should have an independent bucket")
	}
}

func connCount(g *publicGuard, ip netip.Addr) int {
	sh := g.connections.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e := findConnLocked(sh, ipKey(ip)); e != nil {
		return e.connections
	}
	return 0
}

func TestGuardedConnRebindsSharedEdgeConnection(t *testing.T) {
	g := newPublicGuard(8, 8, 1, 1, 1, 1, 1, 1, 1, 1, 1)
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
	// A pooled edge connection may carry another client next: the slot moves.
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
	if got := connCount(g, a); got != 0 {
		t.Fatalf("source A connection count after post-close bind = %d, want 0", got)
	}
}

func TestGuardedConnRebindRespectsPerSourceLimit(t *testing.T) {
	g := newPublicGuard(8, 8, 1, 1, 1, 1, 1, 1, 1, 1, 1)
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

func TestRateStateCapacityIsIndependentFromConnectionState(t *testing.T) {
	g := newPublicGuard(8, maxSourceStateHardCap, 16, 1000, 1000, 1000, 1000, 1, 1, 10, 10)
	ip := mustAddr(t, "192.0.2.1")
	if !g.connections.acquireConn(ip) {
		t.Fatal("failed to allocate active per-IP connection state")
	}

	// The source-rate table is independently preallocated to the hard cap. The
	// connection table may also hold the active client, but it must not consume
	// any of the rate-state slots.
	if got := sourceSlotCount(g.sources); got != maxSourceStateHardCap {
		t.Fatalf("rate table slots = %d, want %d", got, maxSourceStateHardCap)
	}
	connSlots := 0
	for i := range g.connections.shards {
		connSlots += len(g.connections.shards[i].entries)
	}
	if connSlots != maxSourceStateHardCap {
		t.Fatalf("connection table slots = %d, want %d", connSlots, maxSourceStateHardCap)
	}

	now := time.Unix(400, 0)
	for i := 0; i < maxSourceStateHardCap; i++ {
		ip := mustAddr(t, "198.18."+itoa(i/256)+"."+itoa(i%256))
		if !g.sources.allowRateKey(rateKey(ip), now, 1000, 1000, false) {
			t.Fatalf("rate bucket %d was rejected", i)
		}
	}

	rateBuckets := 0
	for i := range g.sources.shards {
		sh := &g.sources.shards[i]
		sh.mu.Lock()
		for _, e := range sh.entries {
			if e.key != "" {
				rateBuckets++
			}
		}
		sh.mu.Unlock()
	}
	if rateBuckets < maxSourceStateHardCap-16 {
		t.Fatalf("rate buckets retained = %d, unexpectedly low for a full 4096-slot rate table", rateBuckets)
	}
}
