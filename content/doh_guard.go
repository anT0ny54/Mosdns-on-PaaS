package main

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxSourceStateHardCap = 4096
	guardShardCount       = 8
)

// sourceEntry is fixed-size state. A hostile rotation of source IPs cannot grow
// a Go map; the table is allocated once and capped at maxSourceStateHardCap.
type sourceEntry struct {
	key          string
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
	for i := 0; i < maxPeers; i++ {
		s := i % activeShards
		t.shards[s].entries = append(t.shards[s].entries, sourceEntry{})
	}
	return t
}

type connEntry struct {
	key         string
	lastSeenNS  int64
	connections int
}

type connShard struct {
	mu      sync.Mutex
	entries []connEntry
}

// connTable keeps per-IP connection accounting physically separate from the
// IP+Host rate-state table. This prevents active connection entries from
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
	if maxPeers > maxSourceStateHardCap {
		maxPeers = maxSourceStateHardCap
	}
	activeShards := guardShardCount
	if maxPeers < activeShards {
		activeShards = maxPeers
	}
	t := &connTable{activeShards: activeShards, perConn: perConn}
	for i := 0; i < maxPeers; i++ {
		s := i % activeShards
		t.shards[s].entries = append(t.shards[s].entries, connEntry{})
	}
	return t
}

func (t *connTable) shardFor(ip netip.Addr) *connShard {
	return &t.shards[hashKey(ipKey(ip))%uint64(t.activeShards)]
}

func hashKey(key string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

func ipKey(ip netip.Addr) string {
	return ip.String()
}

func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func rateKey(ip netip.Addr, host string) string {
	return ipKey(ip) + "\x00" + canonicalHost(host)
}

func (t *sourceTable) shardForKey(key string) *sourceShard {
	return &t.shards[hashKey(key)%uint64(t.activeShards)]
}

func (t *sourceTable) shardFor(ip netip.Addr) *sourceShard {
	return t.shardForKey(ipKey(ip))
}

func findSourceLocked(sh *sourceShard, key string) *sourceEntry {
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
		if e.key == "" {
			return e
		}
		if oldest == nil || e.lastSeenNS < oldest.lastSeenNS {
			oldest = e
		}
	}
	return oldest
}

func (t *sourceTable) findOrCreateLocked(sh *sourceShard, key string, nowNS int64) *sourceEntry {
	if e := findSourceLocked(sh, key); e != nil {
		e.lastSeenNS = nowNS
		return e
	}
	e := findFreeSourceLocked(sh)
	if e == nil {
		// This source shard is exhausted. Fail closed rather than allocating
		// attacker-controlled state beyond the fixed bound.
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

func (t *sourceTable) allowRateKey(key string, now time.Time, rate, burst float64, health bool) bool {
	if rate <= 0 || burst <= 0 {
		return true
	}
	if key == "" {
		return false
	}
	sh := t.shardForKey(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := t.findOrCreateLocked(sh, key, now.UnixNano())
	if e == nil {
		return false
	}
	nowNS := now.UnixNano()
	if health {
		return refill(&e.healthTokens, &e.healthLastNS, rate, burst, nowNS)
	}
	return refill(&e.dohTokens, &e.dohLastNS, rate, burst, nowNS)
}

func (t *sourceTable) allowRate(ip netip.Addr, now time.Time, rate, burst float64, health bool) bool {
	if !ip.IsValid() {
		return false
	}
	return t.allowRateKey(ipKey(ip), now, rate, burst, health)
}

func findConnLocked(sh *connShard, key string) *connEntry {
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
		if e.key == "" {
			return e
		}
		if e.connections == 0 && (oldest == nil || e.lastSeenNS < oldest.lastSeenNS) {
			oldest = e
		}
	}
	return oldest
}

func (t *connTable) acquireConn(ip netip.Addr) bool {
	if t.perConn <= 0 || !ip.IsValid() {
		return true
	}
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	key := ipKey(ip)
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
	if e == nil || e.connections >= t.perConn {
		return false
	}
	e.connections++
	return true
}

func (t *connTable) releaseConn(ip netip.Addr) {
	if t.perConn <= 0 || !ip.IsValid() {
		return
	}
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e := findConnLocked(sh, ipKey(ip)); e != nil {
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
	return &publicGuard{
		globalConnLimit: int64(globalConnLimit),
		sources:         newSourceTable(sourceStateLimit),
		connections:     newConnTable(sourceStateLimit, perSourceConn),
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

func (g *publicGuard) allowDoH(ip netip.Addr, host string, now time.Time) bool {
	if !g.sources.allowRateKey(rateKey(ip, host), now, g.dohRate, g.dohBurst, false) {
		return false
	}
	return g.globalDoH.allow(now)
}

func (g *publicGuard) allowHealth(ip netip.Addr, host string, now time.Time) bool {
	if !g.sources.allowRateKey(rateKey(ip, host), now, g.healthRate, g.healthBurst, true) {
		return false
	}
	return g.globalHealth.allow(now)
}

type guardedConn struct {
	net.Conn
	guard    *publicGuard
	mu       sync.Mutex
	source   netip.Addr
	hasSlot  bool
	closeOne sync.Once
}

func (c *guardedConn) bindSource(ip netip.Addr) bool {
	if c.guard.connections.perConn <= 0 {
		return true
	}
	if !ip.IsValid() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasSlot {
		// The source identity is fixed on the first relevant request. A later
		// change would make connection accounting migrate between buckets and
		// could race with bounded-table eviction. Treat it as an abuse/malformed
		// forwarding condition instead of moving a live slot.
		return c.source == ip
	}
	if !c.guard.connections.acquireConn(ip) {
		return false
	}
	c.source = ip
	c.hasSlot = true
	return true
}

func (c *guardedConn) Close() error {
	c.closeOne.Do(func() {
		c.mu.Lock()
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
		// The real client IP for a public Koyeb HTTP service is available only in
		// the trusted X-Forwarded-For header, which HTTP parsing exposes later.
		// Keep the global cap at accept-time and bind the per-source slot on the
		// first relevant request in the handler. This avoids counting a shared
		// edge/proxy IP as every client.
		return &guardedConn{Conn: c, guard: l.guard}, nil
	}
}
