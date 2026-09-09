package cache

import (
	"encoding/gob"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/IrineSistiana/mosdns/v4/pkg/cache"
	"go.uber.org/zap"
)

type warmRecord struct {
	Value          []byte
	StoredTime     time.Time
	ExpirationTime time.Time
}

type warmBackend struct {
	base     cache.Backend
	path     string
	interval time.Duration
	log      *zap.Logger

	mu      sync.RWMutex
	records map[string]warmRecord
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newWarmBackend(base cache.Backend, path string, intervalSeconds int, log *zap.Logger) *warmBackend {
	w := &warmBackend{
		base:    base,
		path:    path,
		log:     log,
		records: make(map[string]warmRecord),
	}
	if path == "" {
		return w
	}
	w.load()
	if intervalSeconds > 0 {
		w.interval = time.Duration(intervalSeconds) * time.Second
		w.stop = make(chan struct{})
		w.done = make(chan struct{})
		go w.dumpLoop()
	}
	return w
}

func (w *warmBackend) Get(key string) ([]byte, time.Time, time.Time) {
	if v, stored, exp := w.base.Get(key); v != nil {
		return v, stored, exp
	}

	now := time.Now()
	w.mu.RLock()
	r, ok := w.records[key]
	w.mu.RUnlock()
	if !ok || !r.ExpirationTime.After(now) {
		if ok {
			w.mu.Lock()
			delete(w.records, key)
			w.mu.Unlock()
		}
		return nil, time.Time{}, time.Time{}
	}

	v := append([]byte(nil), r.Value...)
	// Rehydrate the RAM cache after a warm-disk hit.
	w.base.Store(key, v, r.StoredTime, r.ExpirationTime)
	return v, r.StoredTime, r.ExpirationTime
}

func (w *warmBackend) Store(key string, v []byte, storedTime, expirationTime time.Time) {
	w.base.Store(key, v, storedTime, expirationTime)
	if w.path == "" || v == nil {
		return
	}
	w.mu.Lock()
	w.records[key] = warmRecord{
		Value:          append([]byte(nil), v...),
		StoredTime:     storedTime,
		ExpirationTime: expirationTime,
	}
	w.mu.Unlock()
}

func (w *warmBackend) Len() int { return w.base.Len() }

func (w *warmBackend) Close() error {
	w.once.Do(func() {
		if w.stop != nil {
			close(w.stop)
			<-w.done
		}
		w.dump()
	})
	return w.base.Close()
}

func (w *warmBackend) dumpLoop() {
	ticker := time.NewTicker(w.interval)
	defer func() {
		ticker.Stop()
		close(w.done)
	}()
	for {
		select {
		case <-ticker.C:
			w.dump()
		case <-w.stop:
			return
		}
	}
}

func (w *warmBackend) load() {
	f, err := os.Open(w.path)
	if err != nil {
		if !os.IsNotExist(err) && w.log != nil {
			w.log.Warn("warm cache load failed", zap.String("file", w.path), zap.Error(err))
		}
		return
	}
	defer f.Close()

	var records map[string]warmRecord
	if err := gob.NewDecoder(f).Decode(&records); err != nil {
		if w.log != nil {
			w.log.Warn("warm cache load failed; starting with empty disk cache", zap.String("file", w.path), zap.Error(err))
		}
		return
	}

	now := time.Now()
	for key, r := range records {
		if r.ExpirationTime.After(now) && len(r.Value) > 0 {
			w.records[key] = r
		}
	}
	if w.log != nil && len(w.records) > 0 {
		w.log.Info("warm cache loaded", zap.String("file", w.path), zap.Int("entries", len(w.records)))
	}
}

func (w *warmBackend) dump() {
	if w.path == "" {
		return
	}

	now := time.Now()
	w.mu.RLock()
	records := make(map[string]warmRecord, len(w.records))
	for key, r := range w.records {
		if r.ExpirationTime.After(now) && len(r.Value) > 0 {
			records[key] = warmRecord{
				Value:          append([]byte(nil), r.Value...),
				StoredTime:     r.StoredTime,
				ExpirationTime: r.ExpirationTime,
			}
		}
	}
	w.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0755); err != nil {
		if w.log != nil {
			w.log.Warn("warm cache mkdir failed", zap.String("file", w.path), zap.Error(err))
		}
		return
	}

	tmp := w.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		if w.log != nil {
			w.log.Warn("warm cache dump create failed", zap.String("file", w.path), zap.Error(err))
		}
		return
	}
	encErr := gob.NewEncoder(f).Encode(records)
	syncErr := f.Sync()
	closeErr := f.Close()
	if encErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if w.log != nil {
			w.log.Warn("warm cache dump failed", zap.String("file", w.path), zap.Errors("errors", []error{encErr, syncErr, closeErr}))
		}
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		_ = os.Remove(tmp)
		if w.log != nil {
			w.log.Warn("warm cache dump rename failed", zap.String("file", w.path), zap.Error(err))
		}
		return
	}
	if w.log != nil {
		w.log.Info("warm cache dumped", zap.String("file", w.path), zap.Int("entries", len(records)))
	}
}
