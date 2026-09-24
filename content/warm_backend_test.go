package cache

import (
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v4/pkg/cache/mem_cache"
)

func newWarmTestBackend(t *testing.T, path string, maxEntries int) *warmBackend {
	t.Helper()
	inner := mem_cache.NewMemCache(maxEntries, 0)
	return newWarmBackend(inner, path, 0, maxEntries, nil).(*warmBackend)
}

func TestWarmBackendKeepsOversizedEntryServiceableWithoutWarmCopy(t *testing.T) {
	w := newWarmTestBackend(t, filepath.Join(t.TempDir(), "cache.dump"), 8)
	defer func() { _ = w.Close() }()

	now := time.Now()
	exp := now.Add(time.Hour)
	small := bytes.Repeat([]byte{1}, maxWarmEntryBytes)
	big := bytes.Repeat([]byte{2}, maxWarmEntryBytes+1)

	w.Store("small", small, now, exp)
	w.Store("big", big, now, exp)

	got, _, _ := w.Get("big")
	if !bytes.Equal(got, big) {
		t.Fatal("oversized value was not kept serviceable in the hot cache")
	}

	w.mu.Lock()
	_, warmPresent := w.entries["big"]
	warmLen := w.order.Len()
	w.mu.Unlock()
	if warmPresent {
		t.Fatal("oversized value must not consume warm-cache metadata/storage")
	}
	if warmLen != 1 {
		t.Fatalf("warm-cache entry count = %d, want 1", warmLen)
	}
}

func TestWarmBackendSnapshotRoundTripPreservesBoundAndLRUOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.dump")
	w := newWarmTestBackend(t, path, 2)
	defer func() { _ = w.Close() }()

	now := time.Now()
	exp := now.Add(time.Hour)
	w.Store("a", []byte("a"), now, exp)
	w.Store("b", []byte("b"), now.Add(time.Second), exp)
	w.Store("c", []byte("c"), now.Add(2*time.Second), exp)
	w.snapshot()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot was not created: %v", err)
	}

	w2 := newWarmTestBackend(t, path, 2)
	defer func() { _ = w2.Close() }()

	w2.mu.Lock()
	gotLen := w2.order.Len()
	front, back := "", ""
	if el := w2.order.Front(); el != nil {
		front = el.Value.(warmNode).Key
	}
	if el := w2.order.Back(); el != nil {
		back = el.Value.(warmNode).Key
	}
	w2.mu.Unlock()

	if gotLen != 2 {
		t.Fatalf("restored warm entry count = %d, want 2", gotLen)
	}
	if front != "c" || back != "b" {
		t.Fatalf("restored LRU order = front %q/back %q, want c/b", front, back)
	}
	if v, _, _ := w2.Get("a"); v != nil {
		t.Fatal("entry beyond the configured warm bound was restored")
	}
}

func TestWarmBackendConcurrentSnapshotsRemainDecodable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.dump")
	w := newWarmTestBackend(t, path, 64)
	defer func() { _ = w.Close() }()

	exp := time.Now().Add(time.Hour)
	for i := 0; i < 32; i++ {
		w.Store(string(rune('a'+i)), bytes.Repeat([]byte{byte(i)}, 256), time.Now(), exp)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.snapshot()
		}()
	}
	w.Store("latest", []byte("latest"), time.Now(), exp)
	wg.Wait()
	w.snapshot()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading final snapshot: %v", err)
	}
	var d warmDisk
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&d); err != nil {
		t.Fatalf("final snapshot is not decodable: %v", err)
	}
	if _, ok := d.Entries["latest"]; !ok {
		t.Fatal("final snapshot did not contain the newest generation")
	}
}
