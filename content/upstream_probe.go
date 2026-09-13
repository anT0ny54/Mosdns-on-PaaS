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

type candidate struct {
	url      string
	ok       bool
	latency  int64
	ewma     float64
	failures int
	score    int64
}

type state struct {
	ewma     float64
	failures int
}

func dnsQuery(id uint16) []byte {
	q := make([]byte, 12, 32)
	binary.BigEndian.PutUint16(q[0:2], id)
	binary.BigEndian.PutUint16(q[2:4], 0x0100)
	binary.BigEndian.PutUint16(q[4:6], 1)
	return append(q, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		3, 'c', 'o', 'm', 0, 0, 1, 0, 1)
}

func probe(url string, timeout time.Duration) (int64, bool) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, false
	}
	id := binary.BigEndian.Uint16(b[:])

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(dnsQuery(id)))
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || resp.StatusCode != http.StatusOK || len(body) < 12 {
		return 0, false
	}
	if binary.BigEndian.Uint16(body[:2]) != id {
		return 0, false
	}
	return max64(time.Since(start).Milliseconds(), 1), true
}

func getFloat(name string, def float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil || v <= 0 || v > 1 {
		return def
	}
	return v
}

func getInt(name string, def int64) int64 {
	v, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

func load(path string) map[string]state {
	out := make(map[string]state)
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		p := strings.Split(line, "\t")
		if len(p) != 3 {
			continue
		}
		e, e1 := strconv.ParseFloat(p[1], 64)
		f, e2 := strconv.Atoi(p[2])
		if e1 == nil && e2 == nil && e > 0 && f >= 0 {
			out[p[0]] = state{ewma: e, failures: f}
		}
	}
	return out
}

func save(path string, m map[string]state) {
	tmp := path + ".tmp"
	var b strings.Builder
	for u, s := range m {
		fmt.Fprintf(&b, "%s\t%.3f\t%d\n", u, s.ewma, s.failures)
	}
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	timeout := time.Duration(getInt("HEALTH_TIMEOUT_MS", 1200)) * time.Millisecond
	alpha := getFloat("HEALTH_EWMA_ALPHA", 0.35)
	penalty := getInt("HEALTH_FAILURE_PENALTY_MS", 1500)
	statePath := os.Getenv("HEALTH_STATE_FILE")
	if statePath == "" {
		statePath = "/tmp/mosdns-upstream-state.tsv"
	}

	st := load(statePath)
	urls := os.Args[1:]
	type rawResult struct {
		url     string
		latency int64
		ok      bool
	}
	ch := make(chan rawResult, len(urls))

	for _, u := range urls {
		go func(u string) {
			latency, ok := probe(u, timeout)
			ch <- rawResult{url: u, latency: latency, ok: ok}
		}(u)
	}

	out := make([]candidate, 0, len(urls))
	for range urls {
		r := <-ch
		s := st[r.url]
		if r.ok {
			if s.ewma <= 0 {
				s.ewma = float64(r.latency)
			} else {
				s.ewma = alpha*float64(r.latency) + (1-alpha)*s.ewma
			}
			s.failures = 0
		} else {
			if s.ewma <= 0 {
				s.ewma = float64(timeout.Milliseconds())
			}
			s.failures++
		}
		score := int64(s.ewma) + int64(s.failures)*penalty
		if !r.ok {
			score += penalty
		}
		st[r.url] = s
		out = append(out, candidate{url: r.url, ok: r.ok, latency: r.latency, ewma: s.ewma, failures: s.failures, score: min64(score, 10000)})
	}
	save(statePath, st)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ok != out[j].ok {
			return out[i].ok
		}
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].url < out[j].url
	})

	// "random" means randomize only healthy near-equal endpoints.
	if os.Getenv("HAGEZI_UPSTREAM") == "random" && len(out) > 1 && out[0].ok {
		best := out[0].score
		cutoff := best + max64(10, int64(float64(best)*0.10))
		n := 0
		for n < len(out) && out[n].ok && out[n].score <= cutoff {
			n++
		}
		if n > 1 {
			shift := int(time.Now().UnixNano() % int64(n))
			tmp := append([]candidate(nil), out[:n]...)
			for i := 0; i < n; i++ {
				out[i] = tmp[(i+shift)%n]
			}
		}
	}

	for _, c := range out {
		fmt.Printf("%s\t%d\t%s\t%d\t%.0f\t%d\n",
			c.url, c.latency, strconv.FormatBool(c.ok), c.score, c.ewma, c.failures)
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
