package cache

import (
	"bufio"
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
	entries map[string]warmEntry
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
		entries:    make(map[string]warmEntry),
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

	w.mu.RLock()
	e, ok := w.entries[key]
	if ok && (!e.ExpirationTime.IsZero() && !e.ExpirationTime.After(time.Now())) {
		ok = false
	}
	w.mu.RUnlock()
	if !ok {
		return nil, time.Time{}, time.Time{}
	}

	// Backend.Get promises a value that the caller will not modify. The inner
	// backend copies the value on Store, so there is no need for another
	// allocation here.
	w.inner.Store(key, e.Value, e.StoredTime, e.ExpirationTime)
	return e.Value, e.StoredTime, e.ExpirationTime
}

func (w *warmBackend) Store(key string, v []byte, storedTime, expirationTime time.Time) {
	if expirationTime.IsZero() || expirationTime.After(time.Now()) {
		w.inner.Store(key, v, storedTime, expirationTime)
		cp := append([]byte(nil), v...)
		w.mu.Lock()
		w.pruneLocked(time.Now())
		if _, exists := w.entries[key]; !exists && w.maxEntries > 0 && len(w.entries) >= w.maxEntries {
			w.evictOneLocked()
		}
		w.entries[key] = warmEntry{Value: cp, StoredTime: storedTime, ExpirationTime: expirationTime}
		w.mu.Unlock()
	}
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
		if e.ExpirationTime.IsZero() || e.ExpirationTime.After(now) {
			w.entries[k] = e
			w.inner.Store(k, e.Value, e.StoredTime, e.ExpirationTime)
		}
	}
	w.mu.Lock()
	w.pruneLocked(now)
	w.mu.Unlock()
	w.info("loaded warm cache", zap.Int("entries", len(w.entries)))
}

func (w *warmBackend) evictOneLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range w.entries {
		if e.ExpirationTime.IsZero() || e.ExpirationTime.After(time.Now()) {
			if oldestKey == "" || e.StoredTime.Before(oldest) {
				oldestKey = k
				oldest = e.StoredTime
			}
		}
	}
	if oldestKey != "" {
		delete(w.entries, oldestKey)
	}
}

func (w *warmBackend) pruneLocked(now time.Time) {
	for k, e := range w.entries {
		if !e.ExpirationTime.IsZero() && !e.ExpirationTime.After(now) {
			delete(w.entries, k)
		}
	}
	for w.maxEntries > 0 && len(w.entries) > w.maxEntries {
		w.evictOneLocked()
	}
}

func (w *warmBackend) snapshot() {
	if w.path == "" {
		return
	}

	// Take a shallow snapshot. warmEntry values are immutable after Store, so
	// retaining their byte-slice references is safe and avoids duplicating the
	// entire cache during every disk dump.
	w.mu.RLock()
	now := time.Now()
	entries := make(map[string]warmEntry, len(w.entries))
	for k, e := range w.entries {
		if e.ExpirationTime.IsZero() || e.ExpirationTime.After(now) {
			entries[k] = e
		}
	}
	w.mu.RUnlock()

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
	bw := bufio.NewWriter(f)
	encErr := gob.NewEncoder(bw).Encode(warmDisk{Entries: entries})
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
