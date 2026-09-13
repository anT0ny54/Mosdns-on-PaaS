package cache

import (
	"bufio"
	"container/list"
	"encoding/gob"
	"os"
	"path/filepath"
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

type warmDisk struct {
	Entries map[string]warmEntry
}

type warmBackend struct {
	inner      cachepkg.Backend
	path       string
	interval   time.Duration
	maxEntries int
	logger     *zap.Logger

	mu      sync.RWMutex
	entries map[string]*list.Element
	order   *list.List // newest first; oldest entry is Back
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newWarmBackend(inner cachepkg.Backend, path string, intervalSeconds int, maxEntries int, logger *zap.Logger) cachepkg.Backend {
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
	if path == "" {
		close(w.done)
		return w
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
	if v, st, exp := w.inner.Get(key); v != nil {
		return v, st, exp
	}

	w.mu.Lock()
	el, ok := w.entries[key]
	if !ok {
		w.mu.Unlock()
		return nil, time.Time{}, time.Time{}
	}
	n := el.Value.(warmNode)
	e := n.Entry
	if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(time.Now()) {
		w.removeElementLocked(el)
		w.mu.Unlock()
		return nil, time.Time{}, time.Time{}
	}
	w.order.MoveToFront(el)
	w.mu.Unlock()

	w.inner.Store(key, e.Value, e.StoredTime, e.ExpirationTime)
	return e.Value, e.StoredTime, e.ExpirationTime
}

func (w *warmBackend) Store(key string, v []byte, storedTime, expirationTime time.Time) {
	if !expirationTime.IsZero() && !expirationTime.After(time.Now()) {
		return
	}

	w.inner.Store(key, v, storedTime, expirationTime)
	cp := append([]byte(nil), v...)

	w.mu.Lock()
	defer w.mu.Unlock()

	if el, ok := w.entries[key]; ok {
		el.Value = warmNode{Key: key, Entry: warmEntry{Value: cp, StoredTime: storedTime, ExpirationTime: expirationTime}}
		w.order.MoveToFront(el)
		return
	}

	if w.maxEntries > 0 && w.order.Len() >= w.maxEntries {
		w.removeElementLocked(w.order.Back())
	}
	el := w.order.PushFront(warmNode{Key: key, Entry: warmEntry{Value: cp, StoredTime: storedTime, ExpirationTime: expirationTime}})
	w.entries[key] = el
}

func (w *warmBackend) Len() int { return w.inner.Len() }

func (w *warmBackend) Close() error {
	w.once.Do(func() {
		close(w.stop)
		<-w.done
		w.snapshot()
		_ = w.inner.Close()
	})
	return nil
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

	var d warmDisk
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&d); err != nil {
		w.warn("failed to decode warm cache; starting empty", zap.Error(err))
		return
	}

	now := time.Now()
	for k, e := range d.Entries {
		if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(now) {
			continue
		}
		if _, exists := w.entries[k]; exists {
			continue
		}
		if w.maxEntries > 0 && w.order.Len() >= w.maxEntries {
			w.removeElementLocked(w.order.Back())
		}
		cp := append([]byte(nil), e.Value...)
		el := w.order.PushBack(warmNode{Key: k, Entry: warmEntry{Value: cp, StoredTime: e.StoredTime, ExpirationTime: e.ExpirationTime}})
		w.entries[k] = el
		w.inner.Store(k, cp, e.StoredTime, e.ExpirationTime)
	}
	w.pruneLocked(now)
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

func (w *warmBackend) removeElementLocked(el *list.Element) {
	if el == nil {
		return
	}
	n, ok := el.Value.(warmNode)
	if !ok {
		w.order.Remove(el)
		return
	}
	delete(w.entries, n.Key)
	w.order.Remove(el)
}

func (w *warmBackend) snapshot() {
	if w.path == "" {
		return
	}

	w.mu.Lock()
	w.pruneLocked(time.Now())
	entries := make(map[string]warmEntry, w.order.Len())
	for el := w.order.Front(); el != nil; el = el.Next() {
		n := el.Value.(warmNode)
		e := n.Entry
		e.Value = append([]byte(nil), e.Value...)
		entries[n.Key] = e
	}
	w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0755); err != nil {
		w.warn("failed to create warm cache directory", zap.Error(err))
		return
	}
	tmp := w.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		w.warn("failed to create warm cache snapshot", zap.Error(err))
		return
	}
	bw := bufio.NewWriterSize(f, 32<<10)
	encErr := gob.NewEncoder(bw).Encode(warmDisk{Entries: entries})
	if encErr == nil {
		encErr = bw.Flush()
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
