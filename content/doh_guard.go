package main

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxSourceStateHardCap = 4096
	// The connection table is sized off GLOBAL_CONN_LIMIT (validated up to 65535 by
	// entrypoint.sh), not off the rate-state hard cap above: GLOBAL_CONN_LIMIT and
	// DOH_RATE_MAX_IPS are independent knobs, and DOH_RATE_MAX_IPS can be
	// configured lower than GLOBAL_CONN_LIMIT. Reusing maxSourceStateHardCap here
	// would silently start rejecting connections with "too many connections" once
	// more than 4096 distinct source IPs hold a slot, even though the configured
	// global limit allows more. connEntry is small (~40 bytes), so the worst case
	// (65535 entries, ~2.6 MiB) is negligible against the 512 MiB budget.
	maxConnStateHardCap = 65535
	// 32 shards keep the fixed tables bounded while reducing per-request linear scans
	// on the 4096-source default without materially increasing memory usage.
	guardShardCount = 32
)

// sourceEntry is fixed-size state. A hostile rotation of source IPs cannot grow
// a Go map; the table is allocated once and capped at maxSourceStateHardCap.
type sourceEntry struct {
	key          netip.Addr
	lastSeenNS   int64
	dohTokens    float64
	dohLastNS    int64
	healthTokens float64
	healthLastNS int64
}

type sourceShard struct {
	mu      sync.Mutex
	entries []sourceEntry
}

type sourceTable struct {
	shards       [guardShardCount]sourceShard
	activeShards int
}

func newSourceTable(maxPeers int) *sourceTable {
	if maxPeers < 1 {
		maxPeers = 1
	}
	if maxPeers > maxSourceStateHardCap {
		maxPeers = maxSourceStateHardCap
	}
	activeShards := guardShardCount
	if maxPeers < activeShards {
		activeShards = maxPeers
	}
	t := &sourceTable{activeShards: activeShards}
	base, extra := maxPeers/activeShards, maxPeers%activeShards
	for i := 0; i < activeShards; i++ {
		n := base
		if i < extra {
			n++
		}
		t.shards[i].entries = make([]sourceEntry, n)
	}
	return t
}

type connEntry struct {
	key         netip.Addr
	lastSeenNS  int64
	connections int
}

type connShard struct {
	mu      sync.Mutex
	entries []connEntry
}

// connTable keeps per-IP connection accounting physically separate from the
// source-IP rate-state table. This prevents active connection entries from
// consuming rate-bucket capacity and preserves the full configured rate-state
// bound even when source IPs have live connections.
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

func (t *connTable) shardFor(ip netip.Addr) *connShard {
	ip = ip.Unmap()
	return &t.shards[hashAddr(ip)%uint64(t.activeShards)]
}

func hashAddr(ip netip.Addr) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range ip.As16() {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

// rateKey returns the canonical source address. The source IP is the only
// identity component by design; Host is never part of rate-limit state.
func rateKey(ip netip.Addr) netip.Addr {
	if !ip.IsValid() {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func (t *sourceTable) shardForKey(key netip.Addr) *sourceShard {
	return &t.shards[hashAddr(key)%uint64(t.activeShards)]
}

func findSourceLocked(sh *sourceShard, key netip.Addr) *sourceEntry {
	for i := range sh.entries {
		if sh.entries[i].key == key {
			return &sh.entries[i]
		}
	}
	return nil
}

func findFreeSourceLocked(sh *sourceShard) *sourceEntry {
	var oldest *sourceEntry
	for i := range sh.entries {
		e := &sh.entries[i]
		if !e.key.IsValid() {
			return e
		}
		if oldest == nil || e.lastSeenNS < oldest.lastSeenNS {
			oldest = e
		}
	}
	return oldest
}

func (t *sourceTable) findOrCreateLocked(sh *sourceShard, key netip.Addr, nowNS int64) *sourceEntry {
	if e := findSourceLocked(sh, key); e != nil {
		e.lastSeenNS = nowNS
		return e
	}
	e := findFreeSourceLocked(sh)
	if e == nil {
		return nil
	}
	*e = sourceEntry{key: key, lastSeenNS: nowNS}
	return e
}

func refill(tokens *float64, lastNS *int64, rate, burst float64, nowNS int64) bool {
	if rate <= 0 || burst <= 0 {
		return true
	}
	if *lastNS == 0 {
		*tokens = burst
		*lastNS = nowNS
	}
	elapsedNS := nowNS - *lastNS
	if elapsedNS > 0 {
		*tokens += float64(elapsedNS) / float64(time.Second) * rate
		if *tokens > burst {
			*tokens = burst
		}
		*lastNS = nowNS
	}
	if *tokens < 1 {
		return false
	}
	*tokens -= 1
	return true
}

func (t *sourceTable) allowRateKey(key netip.Addr, now time.Time, rate, burst float64, health bool) bool {
	if !key.IsValid() {
		return false
	}
	if rate <= 0 || burst <= 0 {
		return true
	}
	sh := t.shardForKey(key)
	nowNS := now.UnixNano()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := t.findOrCreateLocked(sh, key, nowNS)
	if e == nil {
		return false
	}
	if health {
		return refill(&e.healthTokens, &e.healthLastNS, rate, burst, nowNS)
	}
	return refill(&e.dohTokens, &e.dohLastNS, rate, burst, nowNS)
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
	var oldest *connEntry
	for i := range sh.entries {
		e := &sh.entries[i]
		if !e.key.IsValid() {
			return e
		}
		if e.connections == 0 && (oldest == nil || e.lastSeenNS < oldest.lastSeenNS) {
			oldest = e
		}
	}
	return oldest
}

func (t *connTable) acquireConn(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	if t.perConn <= 0 {
		return true
	}
	ip = ip.Unmap()
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	key := ip
	e := findConnLocked(sh, key)
	if e == nil {
		e = findFreeConnLocked(sh)
		if e == nil {
			// This source shard is full of active connections. Fail closed rather
			// than allocating attacker-controlled state or evicting live accounting.
			return false
		}
		*e = connEntry{key: key}
	}
	e.lastSeenNS = time.Now().UnixNano()
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
	if e := findConnLocked(sh, ip); e != nil {
		if e.connections > 0 {
			e.connections--
		}
		e.lastSeenNS = time.Now().UnixNano()
	}
}

// publicGuard is the active in-process anti-abuse guard for the HTTP DoH proxy.
// Source-IP state uses a bounded sharded table instead of a map-based limiter.
type publicGuard struct {
	globalConnLimit int64
	globalConn      int64
	sources         *sourceTable
	connections     *connTable
	globalDoH       tokenBucket
	globalHealth    tokenBucket
	dohRate         float64
	dohBurst        float64
	healthRate      float64
	healthBurst     float64
}

func newPublicGuard(globalConnLimit, sourceStateLimit, perSourceConn int, dohRate, dohBurst, globalRate, globalBurst, healthRate, healthBurst, globalHealthRate, globalHealthBurst float64) *publicGuard {
	// The connection table must hold at least one entry per source IP that can
	// simultaneously occupy a global connection slot, and every entry also
	// serves as active per-IP accounting: size it from whichever of
	// GLOBAL_CONN_LIMIT and DOH_RATE_MAX_IPS is larger, so raising
	// GLOBAL_CONN_LIMIT beyond DOH_RATE_MAX_IPS's default/configured value can
	// never starve legitimate connections once the fixed table fills up.
	// globalConnLimit <= 0 means "unlimited" (see acquireGlobalConn), so size
	// for the same hard cap newConnTable itself enforces.
	connStateLimit := sourceStateLimit
	switch {
	case globalConnLimit <= 0:
		connStateLimit = maxConnStateHardCap
	case globalConnLimit > connStateLimit:
		connStateLimit = globalConnLimit
	}
	return &publicGuard{
		globalConnLimit: int64(globalConnLimit),
		sources:         newSourceTable(sourceStateLimit),
		connections:     newConnTable(connStateLimit, perSourceConn),
		globalDoH:       newTokenBucket(globalRate, globalBurst),
		globalHealth:    newTokenBucket(globalHealthRate, globalHealthBurst),
		dohRate:         dohRate,
		dohBurst:        dohBurst,
		healthRate:      healthRate,
		healthBurst:     healthBurst,
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

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	lastNS int64
}

func newTokenBucket(rate, burst float64) tokenBucket {
	return tokenBucket{rate: rate, burst: burst}
}

func (b *tokenBucket) allow(now time.Time) bool {
	if b.rate <= 0 || b.burst <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return refill(&b.tokens, &b.lastNS, b.rate, b.burst, now.UnixNano())
}

func (g *publicGuard) allowDoH(ip netip.Addr, now time.Time) bool {
	if !g.sources.allowRateKey(rateKey(ip), now, g.dohRate, g.dohBurst, false) {
		return false
	}
	return g.globalDoH.allow(now)
}

func (g *publicGuard) allowHealth(ip netip.Addr, now time.Time) bool {
	if !g.sources.allowRateKey(rateKey(ip), now, g.healthRate, g.healthBurst, true) {
		return false
	}
	return g.globalHealth.allow(now)
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
// The old slot is released before the new one is acquired, and the two shard
// locks are never held together, so the move cannot deadlock or double-count.
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
		// Never take a slot on a connection whose Close has already run; it
		// would never be released.
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
		// first relevant request in the handler. This avoids counting a shared
		// edge/proxy IP as every client.
		return &guardedConn{Conn: c, guard: l.guard}, nil
	}
}
