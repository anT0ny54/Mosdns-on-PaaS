package main

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

const (
	// The connection table is bounded independently from request volume. A hostile
	// rotation of source IPs cannot grow a Go map because state is preallocated.
	maxConnStateHardCap = 65535
	// Keep contention low without allocating a large number of locks for this
	// deliberately small connection table.
	guardShardCount = 32
)

type connEntry struct {
	key         netip.Addr
	connections int
}

type connShard struct {
	mu      sync.Mutex
	entries []connEntry
}

// connTable keeps per-source concurrent connection accounting in a fixed-size,
// sharded table. It intentionally has up to 2x the global connection ceiling in
// metadata slots (still hard-capped at 65535) to reduce same-shard collisions when
// many source IPs are active without making attacker-controlled state unbounded.
type connTable struct {
	shards       [guardShardCount]connShard
	activeShards int
	perConn      int
}

func newConnTable(maxPeers, perConn int) *connTable {
	if maxPeers < 1 {
		maxPeers = 1
	}
	if maxPeers > maxConnStateHardCap {
		maxPeers = maxConnStateHardCap
	}
	activeShards := guardShardCount
	if maxPeers < activeShards {
		activeShards = maxPeers
	}
	t := &connTable{activeShards: activeShards, perConn: perConn}
	base, extra := maxPeers/activeShards, maxPeers%activeShards
	for i := 0; i < activeShards; i++ {
		n := base
		if i < extra {
			n++
		}
		t.shards[i].entries = make([]connEntry, n)
	}
	return t
}

func hashAddr(ip netip.Addr) uint64 {
	// FNV-1a over the canonical 16-byte representation is deterministic and
	// allocation-free. The source IP is already canonicalized by the callers.
	var h uint64 = 1469598103934665603
	for _, b := range ip.As16() {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

func (t *connTable) shardFor(ip netip.Addr) *connShard {
	ip = ip.Unmap()
	return &t.shards[hashAddr(ip)%uint64(t.activeShards)]
}

func findConnLocked(sh *connShard, key netip.Addr) *connEntry {
	for i := range sh.entries {
		if sh.entries[i].key == key {
			return &sh.entries[i]
		}
	}
	return nil
}

func findFreeConnLocked(sh *connShard) *connEntry {
	for i := range sh.entries {
		e := &sh.entries[i]
		if !e.key.IsValid() || e.connections == 0 {
			return e
		}
	}
	return nil
}

func (t *connTable) acquireConn(ip netip.Addr) bool {
	if !ip.IsValid() || t.perConn <= 0 {
		return t.perConn <= 0 && ip.IsValid()
	}
	ip = ip.Unmap()
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := findConnLocked(sh, ip)
	if e == nil {
		e = findFreeConnLocked(sh)
		if e == nil {
			// The global listener ceiling and this fixed table are both full of
			// active connections. Fail closed instead of allocating attacker state.
			return false
		}
		*e = connEntry{key: ip}
	}
	if e.connections >= t.perConn {
		return false
	}
	e.connections++
	return true
}

func (t *connTable) releaseConn(ip netip.Addr) {
	if t.perConn <= 0 || !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e := findConnLocked(sh, ip); e != nil && e.connections > 0 {
		e.connections--
		if e.connections == 0 {
			// Leave the key in place for a hot-source cache hit, but the slot is
			// immediately reusable by another source when this shard fills.
		}
	}
}

// publicGuard protects the proxy with bounded socket concurrency. Request-rate
// token buckets are intentionally absent: they add per-request locking/state and
// turn normal shared-IP burst traffic into 429s. Connection ceilings still bound
// goroutines, backend work, and attacker-controlled socket state.
type publicGuard struct {
	globalConnLimit int64
	globalConn      int64
	connections     *connTable
}

func newPublicGuard(globalConnLimit, perSourceConn int) *publicGuard {
	connStateLimit := globalConnLimit
	if connStateLimit > 0 && connStateLimit < maxConnStateHardCap {
		// A modest amount of spare metadata reduces false rejects from multiple
		// active source IPs hashing into the same shard.
		connStateLimit *= 2
		if connStateLimit > maxConnStateHardCap {
			connStateLimit = maxConnStateHardCap
		}
	}
	if connStateLimit <= 0 || connStateLimit > maxConnStateHardCap {
		connStateLimit = maxConnStateHardCap
	}
	return &publicGuard{
		globalConnLimit: int64(globalConnLimit),
		connections:     newConnTable(connStateLimit, perSourceConn),
	}
}

func (g *publicGuard) acquireGlobalConn() bool {
	if g.globalConnLimit <= 0 {
		return true
	}
	for {
		current := atomic.LoadInt64(&g.globalConn)
		if current >= g.globalConnLimit {
			return false
		}
		if atomic.CompareAndSwapInt64(&g.globalConn, current, current+1) {
			return true
		}
	}
}

func (g *publicGuard) releaseGlobalConn() {
	if g.globalConnLimit > 0 {
		atomic.AddInt64(&g.globalConn, -1)
	}
}

type guardedConn struct {
	net.Conn
	guard   *publicGuard
	mu      sync.Mutex
	source  netip.Addr
	hasSlot bool
	closed  bool
	once    sync.Once
}

// bindSource charges this connection's per-IP slot to ip. A PaaS edge may reuse
// one keep-alive connection for requests from different clients, so a changed
// identity moves the slot to the new client instead of rejecting the request.
func (c *guardedConn) bindSource(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	if c.guard.connections.perConn <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if c.hasSlot {
		if c.source == ip {
			return true
		}
		c.guard.connections.releaseConn(c.source)
		c.hasSlot = false
		c.source = netip.Addr{}
	}
	if !c.guard.connections.acquireConn(ip) {
		return false
	}
	c.source = ip
	c.hasSlot = true
	return true
}

func (c *guardedConn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		if c.hasSlot {
			c.guard.connections.releaseConn(c.source)
			c.hasSlot = false
			c.source = netip.Addr{}
		}
		c.mu.Unlock()
		c.guard.releaseGlobalConn()
	})
	return c.Conn.Close()
}

type guardedListener struct {
	net.Listener
	guard *publicGuard
}

func (l *guardedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.guard.acquireGlobalConn() {
			// Over-cap sockets are closed immediately at accept time, never queued
			// behind an application semaphore.
			_ = c.Close()
			continue
		}
		// The real client IP for a public PaaS HTTP service is available only in
		// the trusted X-Forwarded-For header, which HTTP parsing exposes later.
		// Keep the global cap at accept-time and bind the per-source slot on the
		// first relevant request in the handler.
		return &guardedConn{Conn: c, guard: l.guard}, nil
	}
}
