package cache

import (
	"bufio"
	"container/list"
	"encoding/gob"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	cachepkg "github.com/IrineSistiana/mosdns/v4/pkg/cache"
	"go.uber.org/zap"
)

type warmEntry struct {
	Value          []byte
	StoredTime     time.Time
	ExpirationTime time.Time
}

type warmNode struct {
	Key   string
	Entry warmEntry
}

const maxWarmEntryBytes = 8 * 1024

// Keep startup recovery bounded even if the on-disk snapshot is corrupted or
// unexpectedly replaced. The limit leaves room for gob/map/order metadata around
// the default 8192 x 8 KiB warm-entry budget without permitting a huge decode.
const maxWarmSnapshotBytes = 96 << 20

type warmDisk struct {
	Entries map[string]warmEntry
	// Order preserves the exact LRU order (newest first). It is optional so
	// snapshots written by older versions remain readable.
	Order []string
}

type warmBackend struct {
	inner      cachepkg.Backend
	path       string
	interval   time.Duration
	maxEntries int
	logger     *zap.Logger

	lifecycle           sync.RWMutex
	mu                  sync.Mutex
	snapshotMu          sync.Mutex
	entries             map[string]*list.Element
	order               *list.List // newest first; oldest entry is Back
	stop                chan struct{}
	done                chan struct{}
	closed              bool
	generation          uint64
	persistedGeneration uint64
}

func newWarmBackend(inner cachepkg.Backend, path string, intervalSeconds int, maxEntries int, logger *zap.Logger) cachepkg.Backend {
	if path == "" {
		return inner
	}

	w := &warmBackend{
		inner:      inner,
		path:       path,
		interval:   time.Duration(intervalSeconds) * time.Second,
		maxEntries: maxEntries,
		logger:     logger,
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	w.load()
	if w.interval > 0 {
		go w.loop()
	} else {
		close(w.done)
	}
	return w
}

func (w *warmBackend) Get(key string) ([]byte, time.Time, time.Time) {
	w.lifecycle.RLock()
	defer w.lifecycle.RUnlock()
	if w.closed {
		return nil, time.Time{}, time.Time{}
	}

	// Keep the hot path lock-free with respect to the warm metadata. The inner
	// cache is already concurrency-safe; warm metadata is only needed after an
	// inner-cache miss or when the process is rebuilding its hot cache from disk.
	if v, st, exp := w.inner.Get(key); v != nil {
		return v, st, exp
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// A concurrent Store may have filled the inner cache while this request was
	// waiting for the warm metadata lock. Prefer the current hot-cache value.
	if v, st, exp := w.inner.Get(key); v != nil {
		return v, st, exp
	}

	el, ok := w.entries[key]
	if !ok {
		return nil, time.Time{}, time.Time{}
	}
	n := el.Value.(warmNode)
	e := n.Entry
	if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(time.Now()) {
		w.removeElementLocked(el)
		return nil, time.Time{}, time.Time{}
	}
	w.inner.Store(key, e.Value, e.StoredTime, e.ExpirationTime)
	v, st, exp := w.inner.Get(key)
	if v == nil {
		w.removeElementLocked(el)
		return nil, time.Time{}, time.Time{}
	}
	// A warm restore is a real access for the purposes of future snapshots.
	n.Entry.StoredTime = st
	n.Entry.ExpirationTime = exp
	el.Value = n
	w.touchLocked(el)
	return v, st, exp
}

func (w *warmBackend) Store(key string, v []byte, storedTime, expirationTime time.Time) {
	w.lifecycle.RLock()
	defer w.lifecycle.RUnlock()
	if w.closed {
		return
	}
	now := time.Now()
	// Match mem_cache semantics: an already-expired entry is a no-op.
	if expirationTime.IsZero() || !expirationTime.After(now) {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if el, ok := w.entries[key]; ok {
		if len(v) > maxWarmEntryBytes {
			w.inner.Store(key, v, storedTime, expirationTime)
			w.removeElementLocked(el)
			return
		}
		w.inner.Store(key, v, storedTime, expirationTime)
		w.generation++
		// The inner mem-cache copies the value, and the warm wrapper owns its
		// own copy so the snapshot cannot alias caller-owned memory. Avoid an
		// extra inner.Get on every cache fill.
		el.Value = warmNode{
			Key: key,
			Entry: warmEntry{
				Value:          append([]byte(nil), v...),
				StoredTime:     storedTime,
				ExpirationTime: expirationTime,
			},
		}
		w.order.MoveToFront(el)
		return
	}

	if len(v) > maxWarmEntryBytes {
		w.inner.Store(key, v, storedTime, expirationTime)
		return
	}
	if w.maxEntries > 0 && w.order.Len() >= w.maxEntries {
		w.removeElementLocked(w.order.Back())
	}
	w.inner.Store(key, v, storedTime, expirationTime)
	w.generation++
	el := w.order.PushFront(warmNode{
		Key: key,
		Entry: warmEntry{
			Value:          append([]byte(nil), v...),
			StoredTime:     storedTime,
			ExpirationTime: expirationTime,
		},
	})
	w.entries[key] = el
}

func (w *warmBackend) Len() int {
	w.lifecycle.RLock()
	defer w.lifecycle.RUnlock()
	return w.inner.Len()
}

func (w *warmBackend) Close() error {
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true
	close(w.stop)
	<-w.done
	if w.interval > 0 {
		w.snapshot()
	}
	return w.inner.Close()
}

func (w *warmBackend) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.snapshot()
		case <-w.stop:
			return
		}
	}
}

func (w *warmBackend) load() {
	f, err := os.Open(w.path)
	if err != nil {
		if !os.IsNotExist(err) {
			w.warn("failed to open warm cache", zap.Error(err))
		}
		return
	}
	defer f.Close()

	if info, err := f.Stat(); err != nil {
		w.warn("failed to stat warm cache", zap.Error(err))
		return
	} else if info.Size() > maxWarmSnapshotBytes {
		w.warn("warm cache snapshot is too large", zap.Int64("bytes", info.Size()), zap.Int64("max_bytes", maxWarmSnapshotBytes))
		w.generation++
		return
	}

	var d warmDisk
	if err := gob.NewDecoder(bufio.NewReader(io.LimitReader(f, maxWarmSnapshotBytes+1))).Decode(&d); err != nil {
		w.warn("failed to decode warm cache; starting empty", zap.Error(err))
		w.generation++
		return
	}

	now := time.Now()
	addNode := func(k string, e warmEntry) (warmNode, bool) {
		if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(now) {
			return warmNode{}, false
		}
		if len(e.Value) == 0 || len(e.Value) > maxWarmEntryBytes {
			return warmNode{}, false
		}
		return warmNode{Key: k, Entry: e}, true
	}

	// New snapshots carry the exact list order. Walk it directly so a
	// restart restores the same LRU state rather than reconstructing it from
	// StoredTime (which is not an access-time signal).
	seenCap := len(d.Order)
	if w.maxEntries > 0 && seenCap > w.maxEntries {
		seenCap = w.maxEntries
	}
	seen := make(map[string]struct{}, seenCap)
	nodeCap := len(d.Entries)
	if w.maxEntries > 0 && nodeCap > w.maxEntries {
		nodeCap = w.maxEntries
	}
	nodes := make([]warmNode, 0, nodeCap)
	for _, k := range d.Order {
		if _, ok := seen[k]; ok {
			continue
		}
		e, ok := d.Entries[k]
		if !ok {
			continue
		}
		n, ok := addNode(k, e)
		if !ok {
			continue
		}
		seen[k] = struct{}{}
		nodes = append(nodes, n)
	}

	dirty := len(d.Order) == 0
	// Backward compatibility for snapshots written before Order was added.
	// Those snapshots never had exact access order, so StoredTime is the best
	// available reconstruction.
	if len(d.Order) == 0 {
		for k, e := range d.Entries {
			n, ok := addNode(k, e)
			if ok {
				nodes = append(nodes, n)
			}
		}
		sort.Slice(nodes, func(i, j int) bool {
			if !nodes[i].Entry.StoredTime.Equal(nodes[j].Entry.StoredTime) {
				return nodes[i].Entry.StoredTime.After(nodes[j].Entry.StoredTime)
			}
			return nodes[i].Key < nodes[j].Key
		})
	}
	if len(nodes) != len(d.Entries) {
		dirty = true
	}
	if w.maxEntries > 0 && len(nodes) > w.maxEntries {
		dirty = true
		nodes = nodes[:w.maxEntries]
	}

	// Keep the warm LRU order newest-first while loading the inner cache oldest
	// first, so the newest warm entries are also the most recently inserted hot
	// entries after restart.
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		w.inner.Store(n.Key, n.Entry.Value, n.Entry.StoredTime, n.Entry.ExpirationTime)
	}
	for _, n := range nodes {
		el := w.order.PushBack(n)
		w.entries[n.Key] = el
	}
	w.pruneLocked(now)
	if dirty {
		w.generation++
	}
	w.info("loaded warm cache", zap.Int("entries", w.order.Len()))
}

func (w *warmBackend) pruneLocked(now time.Time) {
	for el := w.order.Back(); el != nil; {
		prev := el.Prev()
		n := el.Value.(warmNode)
		e := n.Entry
		if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(now) {
			w.removeElementLocked(el)
		}
		el = prev
	}
}

func (w *warmBackend) touchLocked(el *list.Element) {
	if el == nil || el == w.order.Front() {
		return
	}
	w.order.MoveToFront(el)
	w.generation++
}

func (w *warmBackend) removeElementLocked(el *list.Element) {
	if el == nil {
		return
	}
	n, ok := el.Value.(warmNode)
	if !ok {
		w.order.Remove(el)
		w.generation++
		return
	}
	delete(w.entries, n.Key)
	w.order.Remove(el)
	w.generation++
}

func (w *warmBackend) snapshot() {
	if w.path == "" {
		return
	}
	// Loop and shutdown snapshots can otherwise overlap. Serializing them is
	// required so an older, slower snapshot cannot rename over a newer one.
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.mu.Lock()
	w.pruneLocked(time.Now())
	if w.generation == w.persistedGeneration {
		w.mu.Unlock()
		return
	}
	snapshotGeneration := w.generation
	entries := make(map[string]warmEntry, w.order.Len())
	order := make([]string, 0, w.order.Len())
	for el := w.order.Front(); el != nil; el = el.Next() {
		n := el.Value.(warmNode)
		// warmEntry values are immutable after Store replaces the whole node, so
		// the slice backing array remains safe to encode after releasing w.mu.
		// Avoid copying every cached response on each snapshot.
		entries[n.Key] = n.Entry
		order = append(order, n.Key)
	}
	w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0755); err != nil {
		w.warn("failed to create warm cache directory", zap.Error(err))
		return
	}
	tmp := w.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		w.warn("failed to create warm cache snapshot", zap.Error(err))
		return
	}
	bw := bufio.NewWriterSize(f, 32<<10)
	encErr := gob.NewEncoder(bw).Encode(warmDisk{Entries: entries, Order: order})
	if encErr == nil {
		encErr = bw.Flush()
	}
	if encErr == nil {
		encErr = f.Sync()
	}
	closeErr := f.Close()
	if encErr == nil {
		encErr = closeErr
	}
	if encErr != nil {
		_ = os.Remove(tmp)
		w.warn("failed to write warm cache snapshot", zap.Error(encErr))
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		_ = os.Remove(tmp)
		w.warn("failed to install warm cache snapshot", zap.Error(err))
		return
	}

	w.mu.Lock()
	if w.generation == snapshotGeneration {
		w.persistedGeneration = snapshotGeneration
	}
	w.mu.Unlock()
	w.info("saved warm cache", zap.Int("entries", len(entries)))
}

func (w *warmBackend) warn(msg string, fields ...zap.Field) {
	if w.logger != nil {
		w.logger.Warn(msg, fields...)
	}
}

func (w *warmBackend) info(msg string, fields ...zap.Field) {
	if w.logger != nil {
		w.logger.Info(msg, fields...)
	}
}
