package main

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	tbl := newSourceTable(maxSourceStateHardCap+128, 0)
	if got := sourceSlotCount(tbl); got != maxSourceStateHardCap {
		t.Fatalf("source table allocated %d slots, want hard cap %d", got, maxSourceStateHardCap)
	}
}

func sourceSlotCount(tbl *sourceTable) int {
	total := 0
	for i := range tbl.shards {
		total += len(tbl.shards[i].entries)
	}
	return total
}

func TestBoundedSourceState(t *testing.T) {
	tbl := newSourceTable(16, 2)
	now := time.Unix(0, 1)
	for i := 1; i <= 200; i++ {
		ip := mustAddr(t, "192.0.2."+itoa(i%250+1))
		_ = tbl.allowRate(ip, now.Add(time.Duration(i)*time.Millisecond), 100, 100, false)
	}
	for i := range tbl.shards {
		sh := &tbl.shards[i]
		if len(sh.entries) > 2 {
			t.Fatalf("shard retained %d slots, want <=2", len(sh.entries))
		}
	}
}

func TestSourceTokenBucket(t *testing.T) {
	tbl := newSourceTable(8, 0)
	ip := mustAddr(t, "203.0.113.7")
	base := time.Unix(100, 0)
	for i := 0; i < 3; i++ {
		if !tbl.allowRate(ip, base, 2, 3, false) {
			t.Fatalf("initial burst request %d denied", i+1)
		}
	}
	if tbl.allowRate(ip, base, 2, 3, false) {
		t.Fatal("request beyond burst unexpectedly allowed")
	}
	if !tbl.allowRate(ip, base.Add(500*time.Millisecond), 2, 3, false) {
		t.Fatal("one token should have refilled after 500ms")
	}
}

func TestInvalidSourceFailsClosedWhenRateLimited(t *testing.T) {
	tbl := newSourceTable(8, 0)
	if tbl.allowRate(netip.Addr{}, time.Unix(100, 0), 5, 12, false) {
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
	if !g.sources.acquireConn(ip) || !g.sources.acquireConn(ip) {
		t.Fatal("first two per-source connections must be accepted")
	}
	if g.sources.acquireConn(ip) {
		t.Fatal("third per-source connection must be rejected")
	}
	g.sources.releaseConn(ip)
	if !g.sources.acquireConn(ip) {
		t.Fatal("released per-source connection must be reusable")
	}
}

func TestUDPDropOversized(t *testing.T) {
	if UDPDropOversized(make([]byte, 512), 1232) {
		t.Fatal("valid-sized UDP packet should not be dropped")
	}
	if !UDPDropOversized(make([]byte, 1233), 1232) {
		t.Fatal("oversized UDP packet should be dropped")
	}
	if !UDPDropOversized(make([]byte, 11), 1232) {
		t.Fatal("truncated DNS packet should be dropped")
	}
}

func TestReadTCPDNSFrameRejectsBeforeAllocation(t *testing.T) {
	var prefix [2]byte
	binary.BigEndian.PutUint16(prefix[:], 5000)
	buf := bytes.NewBuffer(append(prefix[:], make([]byte, 5000)...))
	frame, err := ReadTCPDNSFrame(buf, 4096)
	if !errors.Is(err, errDNSFrameTooLarge) {
		t.Fatalf("ReadTCPDNSFrame error = %v, want %v", err, errDNSFrameTooLarge)
	}
	if frame != nil {
		t.Fatal("oversized frame must not be allocated")
	}
	if got := buf.Len(); got != 5000 {
		t.Fatalf("buffer consumed %d payload bytes, want 0", 5000-got)
	}
}

func TestReadTCPDNSFrameAcceptsBoundedFrame(t *testing.T) {
	payload := make([]byte, 12)
	var prefix [2]byte
	binary.BigEndian.PutUint16(prefix[:], uint16(len(payload)))
	buf := bytes.NewBuffer(append(prefix[:], payload...))
	frame, err := ReadTCPDNSFrame(buf, 4096)
	if err != nil {
		t.Fatalf("ReadTCPDNSFrame: %v", err)
	}
	if len(frame) != len(payload) {
		t.Fatalf("frame length = %d, want %d", len(frame), len(payload))
	}
	if got := buf.Len(); got != 0 {
		t.Fatalf("buffer has %d bytes left, want 0", got)
	}
}

func TestReadTCPDNSQueryClosesOnAbuse(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		_, _ = client.Write(func() []byte {
			var p [2]byte
			binary.BigEndian.PutUint16(p[:], 5000)
			return p[:]
		}())
		close(done)
	}()
	_, err := ReadTCPDNSQuery(server, 4096, NewTCPDNSQueryBudget(16))
	if !errors.Is(err, errDNSFrameTooLarge) {
		t.Fatalf("ReadTCPDNSQuery error = %v, want %v", err, errDNSFrameTooLarge)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client write did not observe immediate connection close")
	}
}

func TestTCPDNSQueryBudget(t *testing.T) {
	b := NewTCPDNSQueryBudget(2)
	if !b.Allow() || !b.Allow() {
		t.Fatal("first two queries must be accepted")
	}
	if b.Allow() {
		t.Fatal("query over connection budget must be rejected")
	}
}

func TestTCPDNSQueryBudgetConcurrent(t *testing.T) {
	const maxQueries = 128
	const attempts = 2000
	b := NewTCPDNSQueryBudget(maxQueries)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != maxQueries {
		t.Fatalf("concurrent budget accepted %d queries, want %d", got, maxQueries)
	}
}

func TestReadTCPDNSQueryClosesOnQueryBudget(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	budget := NewTCPDNSQueryBudget(1)
	if !budget.Allow() {
		t.Fatal("setup query budget was unexpectedly denied")
	}
	result := make(chan error, 1)
	go func() {
		_, err := ReadTCPDNSQuery(server, 4096, budget)
		result <- err
	}()

	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := client.Read(buf[:]); err == nil {
		t.Fatal("query-budget abuse left TCP connection open")
	}
	select {
	case err := <-result:
		if !errors.Is(err, errDNSQueryLimit) {
			t.Fatalf("ReadTCPDNSQuery error = %v, want %v", err, errDNSQueryLimit)
		}
	case <-time.After(time.Second):
		t.Fatal("query-budget guard did not return")
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

func TestClientIPAddrUsesKoyebFinalXFFValue(t *testing.T) {
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

func TestGuardedConnBindsSourceOnlyOnce(t *testing.T) {
	g := newPublicGuard(8, 8, 1, 1, 1, 1, 1, 1, 1, 1, 1)
	server, client := net.Pipe()
	defer client.Close()
	c := &guardedConn{Conn: server, guard: g}
	a := mustAddr(t, "192.0.2.1")
	b := mustAddr(t, "192.0.2.2")
	if !c.bindSource(a) {
		t.Fatal("initial source binding failed")
	}
	if c.bindSource(b) {
		t.Fatal("already-bound connection changed source identity")
	}
	shA := g.sources.shardFor(a)
	shA.mu.Lock()
	eA := findSourceLocked(shA, a)
	countA := 0
	if eA != nil {
		countA = eA.connections
	}
	shA.mu.Unlock()
	if countA != 1 {
		t.Fatalf("source A connection count = %d, want 1", countA)
	}
	shB := g.sources.shardFor(b)
	shB.mu.Lock()
	eB := findSourceLocked(shB, b)
	countB := 0
	if eB != nil {
		countB = eB.connections
	}
	shB.mu.Unlock()
	if countB != 0 {
		t.Fatalf("source B connection count = %d, want 0", countB)
	}
	_ = c.Close()
	shA.mu.Lock()
	eA = findSourceLocked(shA, a)
	countA = 0
	if eA != nil {
		countA = eA.connections
	}
	shA.mu.Unlock()
	if countA != 0 {
		t.Fatalf("source A connection count after close = %d, want 0", countA)
	}
}
