package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxUDPPacketBytes = 1232
	defaultMaxTCPDNSFrame    = 4096
	defaultMaxTCPDNSQueries  = 16
	maxSourceStateHardCap    = 512
	guardShardCount          = 8
)

var (
	errDNSFrameTooLarge = errors.New("dns tcp frame too large")
	errDNSFrameInvalid  = errors.New("invalid dns tcp frame length")
	errDNSQueryLimit    = errors.New("dns tcp query limit exceeded")
)

// sourceEntry is fixed-size state. A hostile rotation of source IPs cannot grow
// a Go map; the table is allocated once and capped at maxSourceStateHardCap.
type sourceEntry struct {
	ip           netip.Addr
	lastSeenNS   int64
	dohTokens    float64
	dohLastNS    int64
	healthTokens float64
	healthLastNS int64
	connections  int
}

type sourceShard struct {
	mu      sync.Mutex
	entries []sourceEntry
}

type sourceTable struct {
	shards       [guardShardCount]sourceShard
	activeShards int
	perConn      int
}

func newSourceTable(maxPeers, perConn int) *sourceTable {
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
	t := &sourceTable{activeShards: activeShards, perConn: perConn}
	for i := 0; i < maxPeers; i++ {
		s := i % activeShards
		t.shards[s].entries = append(t.shards[s].entries, sourceEntry{})
	}
	return t
}

func hashIP(ip netip.Addr) uint64 {
	b := ip.As16()
	var h uint64 = 1469598103934665603
	for _, v := range b {
		h ^= uint64(v)
		h *= 1099511628211
	}
	return h
}

func (t *sourceTable) shardFor(ip netip.Addr) *sourceShard {
	return &t.shards[hashIP(ip)%uint64(t.activeShards)]
}

func findSourceLocked(sh *sourceShard, ip netip.Addr) *sourceEntry {
	for i := range sh.entries {
		if sh.entries[i].ip == ip {
			return &sh.entries[i]
		}
	}
	return nil
}

func findFreeSourceLocked(sh *sourceShard) *sourceEntry {
	var oldest *sourceEntry
	for i := range sh.entries {
		e := &sh.entries[i]
		if !e.ip.IsValid() {
			return e
		}
		if e.connections == 0 && (oldest == nil || e.lastSeenNS < oldest.lastSeenNS) {
			oldest = e
		}
	}
	return oldest
}

func (t *sourceTable) findOrCreateLocked(sh *sourceShard, ip netip.Addr, nowNS int64) *sourceEntry {
	if e := findSourceLocked(sh, ip); e != nil {
		e.lastSeenNS = nowNS
		return e
	}
	e := findFreeSourceLocked(sh)
	if e == nil {
		// This source shard is full of active connections. Fail closed rather than
		// allocating attacker-controlled state or evicting live accounting.
		return nil
	}
	*e = sourceEntry{ip: ip, lastSeenNS: nowNS}
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

func (t *sourceTable) allowRate(ip netip.Addr, now time.Time, rate, burst float64, health bool) bool {
	if rate <= 0 || burst <= 0 {
		return true
	}
	if !ip.IsValid() {
		return false
	}
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := t.findOrCreateLocked(sh, ip, now.UnixNano())
	if e == nil {
		return false
	}
	nowNS := now.UnixNano()
	if health {
		return refill(&e.healthTokens, &e.healthLastNS, rate, burst, nowNS)
	}
	return refill(&e.dohTokens, &e.dohLastNS, rate, burst, nowNS)
}

func (t *sourceTable) acquireConn(ip netip.Addr) bool {
	if t.perConn <= 0 || !ip.IsValid() {
		return true
	}
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := t.findOrCreateLocked(sh, ip, time.Now().UnixNano())
	if e == nil || e.connections >= t.perConn {
		return false
	}
	e.connections++
	return true
}

func (t *sourceTable) releaseConn(ip netip.Addr) {
	if t.perConn <= 0 || !ip.IsValid() {
		return
	}
	sh := t.shardFor(ip)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e := findSourceLocked(sh, ip); e != nil {
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
		sources:         newSourceTable(sourceStateLimit, perSourceConn),
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
	if !g.sources.allowRate(ip, now, g.dohRate, g.dohBurst, false) {
		return false
	}
	return g.globalDoH.allow(now)
}

func (g *publicGuard) allowHealth(ip netip.Addr, now time.Time) bool {
	if !g.sources.allowRate(ip, now, g.healthRate, g.healthBurst, true) {
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
	if c.guard.sources.perConn <= 0 {
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
	if !c.guard.sources.acquireConn(ip) {
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
			c.guard.sources.releaseConn(c.source)
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

// UDPDropOversized performs the size gate for a raw DNS UDP packet. Callers
// should silently discard packets when it returns true.
func UDPDropOversized(packet []byte, maxBytes int) bool {
	if maxBytes <= 0 {
		maxBytes = defaultMaxUDPPacketBytes
	}
	return len(packet) > maxBytes || len(packet) < 12
}

// ReadTCPDNSQuery enforces both the per-connection query budget and the DNS
// length prefix. Any abuse error closes the TCP connection immediately.
func ReadTCPDNSQuery(c net.Conn, maxBytes int, budget *TCPDNSQueryBudget) ([]byte, error) {
	if budget != nil && !budget.Allow() {
		_ = c.Close()
		return nil, errDNSQueryLimit
	}
	frame, err := ReadTCPDNSFrame(c, maxBytes)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return frame, nil
}

// ReadTCPDNSFrame validates the two-byte DNS-over-TCP length prefix before any
// frame allocation or DNS parsing. Callers should close the TCP connection when
// an error is returned. (ReadTCPDNSQuery above is the abuse-aware wrapper most
// callers on a real TCP connection want; this lower-level function is exported
// separately so it stays directly unit-testable against a plain io.Reader.)
func ReadTCPDNSFrame(r io.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > 65535 {
		maxBytes = defaultMaxTCPDNSFrame
	}
	var prefix [2]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	frameLen := int(binary.BigEndian.Uint16(prefix[:]))
	if frameLen < 12 {
		return nil, errDNSFrameInvalid
	}
	if frameLen > maxBytes {
		return nil, errDNSFrameTooLarge
	}
	frame := make([]byte, frameLen)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

type TCPDNSQueryBudget struct {
	max   int64
	count int64
}

func NewTCPDNSQueryBudget(maxQueries int) *TCPDNSQueryBudget {
	if maxQueries <= 0 {
		maxQueries = defaultMaxTCPDNSQueries
	}
	return &TCPDNSQueryBudget{max: int64(maxQueries)}
}

func (b *TCPDNSQueryBudget) Allow() bool {
	if b == nil || b.max <= 0 {
		return true
	}
	for {
		count := atomic.LoadInt64(&b.count)
		if count >= b.max {
			return false
		}
		if atomic.CompareAndSwapInt64(&b.count, count, count+1) {
			return true
		}
	}
}
