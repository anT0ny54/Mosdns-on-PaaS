package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type result struct {
	idx int
	ok  bool
	ms  int64
}

func dnsQuery(id uint16) []byte {
	b := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(b[0:2], id)
	binary.BigEndian.PutUint16(b[2:4], 0x0100)
	binary.BigEndian.PutUint16(b[4:6], 1)
	b = append(b, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1)
	return b
}

func probe(url string, timeout time.Duration) (int64, bool) {
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return 0, false
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	body := dnsQuery(id)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(data) < 12 {
		return 0, false
	}
	if binary.BigEndian.Uint16(data[0:2]) != id {
		return 0, false
	}
	if binary.BigEndian.Uint16(data[6:8]) == 0 {
		return 0, false
	}
	return time.Since(start).Milliseconds(), true
}

func main() {
	timeout := 1500 * time.Millisecond
	if v := os.Getenv("HEALTH_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}
	urls := os.Args[1:]
	if len(urls) == 0 {
		os.Exit(2)
	}
	out := make([]result, len(urls))
	done := make(chan result, len(urls))
	for i, u := range urls {
		go func(i int, u string) {
			ms, ok := probe(u, timeout)
			done <- result{idx: i, ok: ok, ms: ms}
		}(i, u)
	}
	for range urls {
		r := <-done
		out[r.idx] = r
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ok != out[j].ok {
			return out[i].ok
		}
		if !out[i].ok {
			return out[i].idx < out[j].idx
		}
		return out[i].ms < out[j].ms
	})
	for _, r := range out {
		score := r.ms
		if !r.ok {
			score = 10000
		}
		fmt.Printf("%d\t%s\t%d\t%s\t%d\n", r.idx, urls[r.idx], r.ms, strings.TrimSpace(strconv.FormatBool(r.ok)), score)
	}
}
