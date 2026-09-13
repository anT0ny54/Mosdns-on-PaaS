package main

import (
	"bufio"
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
	"sync"
	"time"
)

type result struct {
	idx      int
	ok       bool
	ms       int64
	ewma     float64
	failures int
	score    int64
	url      string
}

type state struct {
	ewma     float64
	failures int
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
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(data) < 12 || binary.BigEndian.Uint16(data[0:2]) != id || binary.BigEndian.Uint16(data[6:8]) == 0 {
		return 0, false
	}
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms, true
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 && n <= 1 {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func loadState(path string) map[string]state {
	m := make(map[string]state)
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Split(s.Text(), "\t")
		if len(p) != 3 {
			continue
		}
		e, err1 := strconv.ParseFloat(p[1], 64)
		fails, err2 := strconv.Atoi(p[2])
		if err1 == nil && err2 == nil && e > 0 && fails >= 0 {
			m[p[0]] = state{ewma: e, failures: fails}
		}
	}
	return m
}

func saveState(path string, m map[string]state) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for url, st := range m {
		fmt.Fprintf(w, "%s\t%.3f\t%d\n", url, st.ewma, st.failures)
	}
	_ = w.Flush()
	_ = f.Close()
	_ = os.Rename(tmp, path)
}

func main() {
	timeout := 1200 * time.Millisecond
	if v := os.Getenv("HEALTH_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}
	alpha := envFloat("HEALTH_EWMA_ALPHA", 0.35)
	failurePenalty := envInt64("HEALTH_FAILURE_PENALTY_MS", 1500)
	switchPct := envFloat("HEALTH_SWITCH_MARGIN_PCT", 0.20)
	switchMs := envInt64("HEALTH_SWITCH_MARGIN_MS", 25)
	statePath := os.Getenv("HEALTH_STATE_FILE")
	if statePath == "" {
		statePath = "/tmp/mosdns-upstream-state.tsv"
	}
	active := os.Getenv("HEALTH_ACTIVE_UPSTREAM")
	mode := os.Getenv("HAGEZI_UPSTREAM")
	urls := os.Args[1:]
	if len(urls) == 0 {
		os.Exit(2)
	}

	st := loadState(statePath)
	out := make([]result, len(urls))
	done := make(chan result, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			ms, ok := probe(u, timeout)
			done <- result{idx: i, ok: ok, ms: ms, url: u}
		}(i, u)
	}
	wg.Wait()
	close(done)
	for r := range done {
		s := st[r.url]
		if r.ok {
			if s.ewma <= 0 {
				s.ewma = float64(r.ms)
			} else {
				s.ewma = alpha*float64(r.ms) + (1-alpha)*s.ewma
			}
			s.failures = 0
		} else {
			if s.ewma <= 0 {
				s.ewma = float64(timeout.Milliseconds())
			}
			s.failures++
		}
		score := int64(s.ewma) + int64(s.failures)*failurePenalty
		if !r.ok || s.failures > 0 {
			score += failurePenalty
		}
		if score > 10000 {
			score = 10000
		}
		r.ewma, r.failures, r.score = s.ewma, s.failures, score
		st[r.url] = s
		out[r.idx] = r
	}
	saveState(statePath, st)

	// Stable health order: healthy first, then lowest smoothed score.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ok != out[j].ok {
			return out[i].ok
		}
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].idx < out[j].idx
	})

	// Hysteresis: don't churn the active upstream for a small/temporary win.
	// Switch only if the active endpoint is failed, or the best alternative is
	// both materially faster and at least HEALTH_SWITCH_MARGIN_MS better.
	if active != "" && len(out) > 1 {
		activePos, bestPos := -1, -1
		for i := range out {
			if out[i].url == active {
				activePos = i
			}
			if bestPos < 0 && out[i].ok {
				bestPos = i
			}
		}
		if activePos >= 0 && bestPos >= 0 && out[activePos].ok && bestPos != activePos {
			best := out[bestPos]
			cur := out[activePos]
			improvement := float64(cur.score-best.score) / float64(max64(cur.score, 1))
			absolute := cur.score - best.score
			if improvement < switchPct || absolute < switchMs {
				// Keep active first; retain the health ordering for the remainder.
				keep := out[activePos]
				copy(out[1:activePos+1], out[0:activePos])
				out[0] = keep
			}
		}
	}

	// Random mode only randomizes near-equal healthy candidates. This preserves
	// health awareness while avoiding deterministic pinning when requested.
	if mode == "random" && len(out) > 1 && out[0].ok {
		best := out[0].score
		cutoff := int64(float64(best) * 1.10)
		if cutoff < best+10 {
			cutoff = best + 10
		}
		candidates := 0
		for i := range out {
			if out[i].ok && out[i].score <= cutoff {
				candidates++
			}
		}
		if candidates > 1 {
			// A tiny deterministic rotation is enough; the outer process already
			// rotates every interval, so no crypto RNG is needed here.
			shift := int(time.Now().UnixNano() % int64(candidates))
			if shift > 0 {
				tmp := append([]result(nil), out[:candidates]...)
				for i := 0; i < candidates; i++ {
					out[i] = tmp[(i+shift)%candidates]
				}
			}
		}
	}

	for _, r := range out {
		ms := r.ms
		if !r.ok {
			ms = 0
		}
		fmt.Printf("%d\t%s\t%d\t%s\t%d\t%.0f\t%d\n", r.idx, r.url, ms, strconv.FormatBool(r.ok), r.score, r.ewma, r.failures)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
