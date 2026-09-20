package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

type probeURLKey struct{}

// The probe helper is a one-shot process. Keep the transport deliberately
// single-use so it does not retain idle sockets between its three probes.
var probeClient = newProbeClient()
var probeID uint32

func newProbeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:              nil,
			ForceAttemptHTTP2:  true,
			DisableCompression: true,
			DisableKeepAlives:  true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if network == "tcp" {
					if _, port, err := net.SplitHostPort(addr); err == nil {
						if rawURL, _ := ctx.Value(probeURLKey{}).(string); rawURL != "" {
							if ip := pinnedIPForURL(rawURL); ip != "" {
								addr = net.JoinHostPort(ip, port)
							}
						}
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// A health probe must not redirect to another origin.
			return http.ErrUseLastResponse
		},
	}
}

func pinnedIPForURL(rawURL string) string {
	// Pin only the exact built-in endpoint URLs. A custom endpoint may share a
	// built-in hostname but use a different URL/path; runtime MosDNS resolves
	// that custom URL normally, so the health probe must use the same behavior.
	switch rawURL {
	case "https://root.hagezi.org/dns-query":
		return validPinnedIP(os.Getenv("UPSTREAM_0_IP"))
	case "https://wurzn.hagezi.org/dns-query":
		return validPinnedIP(os.Getenv("UPSTREAM_1_IP"))
	case "https://juuri.hagezi.org/dns-query":
		return validPinnedIP(os.Getenv("UPSTREAM_2_IP"))
	default:
		return ""
	}
}

func validPinnedIP(value string) string {
	ip := net.ParseIP(value)
	if ip != nil && ip.To4() != nil {
		return value
	}
	return ""
}

func probe(url string, timeout time.Duration) (int64, bool) {
	id := uint16(atomic.AddUint32(&probeID, 1))
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(dnsQuery(id)))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = context.WithValue(ctx, probeURLKey{}, url)
	resp, err := probeClient.Do(req.WithContext(ctx))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if contentType != "application/dns-message" {
		return 0, false
	}
	// DNS messages are bounded by the wire-format maximum. Only the 12-byte
	// header is needed for validation; drain the remainder without retaining
	// the full response so health probes stay allocation-light and memory-bounded.
	const maxDNSMessageBytes = 65535
	if resp.ContentLength > maxDNSMessageBytes {
		return 0, false
	}
	var header [12]byte
	if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
		return 0, false
	}
	remaining := int64(maxDNSMessageBytes - len(header))
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, remaining+1))
	if err != nil || n > remaining {
		return 0, false
	}
	flags := binary.BigEndian.Uint16(header[2:4])
	if binary.BigEndian.Uint16(header[0:2]) != id || flags&0x8000 == 0 {
		return 0, false
	}
	// The probe sends one standard recursive DNS question. Require the normal
	// query opcode and an echoed question count so a generic HTTP/DNS-shaped
	// response cannot be mistaken for a healthy resolver.
	if flags&0x7800 != 0 || flags&0x0200 != 0 || binary.BigEndian.Uint16(header[4:6]) != 1 {
		return 0, false
	}
	// Treat only NOERROR and NXDOMAIN as healthy probe outcomes. Unknown or
	// reserved RCODEs must not be accepted as evidence that the resolver is
	// servicing DNS normally.
	rcode := flags & 0x000f
	if rcode != 0 && rcode != 3 {
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

func envIntOrDefault(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
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
		if err1 == nil && err2 == nil && e > 0 && !math.IsNaN(e) && !math.IsInf(e, 0) && fails >= 0 {
			m[p[0]] = state{ewma: e, failures: fails}
		}
	}
	if err := s.Err(); err != nil {
		return make(map[string]state)
	}
	return m
}

func saveState(path string, m map[string]state) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return
		}
	}

	keys := make([]string, 0, len(m))
	for url := range m {
		keys = append(keys, url)
	}
	sort.Strings(keys)

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	w := bufio.NewWriterSize(f, 4<<10)
	writeOK := true
	for _, url := range keys {
		st := m[url]
		if _, err := fmt.Fprintf(w, "%s\t%.3f\t%d\n", url, st.ewma, st.failures); err != nil {
			writeOK = false
			break
		}
	}
	flushErr := w.Flush()
	closeErr := f.Close()
	if writeOK && flushErr == nil && closeErr == nil {
		if err := os.Rename(tmp, path); err == nil {
			return
		}
	}
	_ = os.Remove(tmp)
}

func scoreState(s state, failurePenalty int64) int64 {
	var score int64
	switch {
	case math.IsNaN(s.ewma) || s.ewma <= 0:
		score = 0
	case s.ewma >= 10000:
		score = 10000
	default:
		score = int64(s.ewma)
	}

	if score < 10000 && s.failures > 0 && failurePenalty > 0 {
		room := 10000 - score
		penalty := int64(s.failures)
		if penalty > room/failurePenalty {
			return 10000
		}
		score += penalty * failurePenalty
	}
	return score
}

func main() {
	timeout := time.Duration(envIntOrDefault("HEALTH_TIMEOUT_MS", 1200)) * time.Millisecond
	failsToSwitch := envIntOrDefault("HEALTH_FAILS_TO_SWITCH", 2)
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
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			ms, ok := probe(u, timeout)
			out[i] = result{idx: i, ok: ok, ms: ms, url: u}
		}(i, u)
	}
	wg.Wait()

	// Update the shared state map serially after all probes complete. This keeps
	// the allocation-free per-result slice optimization while avoiding concurrent
	// map reads/writes (and preserving deterministic state updates by input order).
	for i := range out {
		r := &out[i]
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
		score := scoreState(s, failurePenalty)
		r.ewma, r.failures, r.score = s.ewma, s.failures, score
		st[r.url] = s
	}
	currentState := make(map[string]state, len(urls))
	for _, u := range urls {
		currentState[u] = st[u]
	}
	saveState(statePath, currentState)

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
	keptActive := false
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
		if activePos >= 0 && bestPos >= 0 && bestPos != activePos {
			best := out[bestPos]
			cur := out[activePos]
			if !cur.ok {
				// A transient failure must not be enough to replace the active
				// upstream. Preserve it until the configured consecutive-failure
				// threshold is reached; this also prevents random mode from
				// bypassing the same hysteresis rule below.
				if cur.failures < failsToSwitch {
					keptActive = true
					keep := out[activePos]
					copy(out[1:activePos+1], out[0:activePos])
					out[0] = keep
				}
			} else {
				improvement := float64(cur.score-best.score) / float64(max64(cur.score, 1))
				absolute := cur.score - best.score
				if improvement < switchPct || absolute < switchMs {
					// Keep active first; retain the health ordering for the remainder.
					keptActive = true
					keep := out[activePos]
					copy(out[1:activePos+1], out[0:activePos])
					out[0] = keep
				}
			}
		}
	}

	// Random mode only randomizes near-equal healthy candidates. This preserves
	// health awareness while avoiding deterministic pinning when requested.
	if mode == "random" && !keptActive && len(out) > 1 && out[0].ok {
		best := out[0].score
		cutoff := int64(float64(best) * 1.10)
		if cutoff < best+10 {
			cutoff = best + 10
		}
		activeScore, activeHealthy := int64(0), false
		if active != "" {
			for i := range out {
				if out[i].url == active {
					activeScore, activeHealthy = out[i].score, out[i].ok
					break
				}
			}
		}
		if activeHealthy && activeScore != best {
			// When active is not the best candidate, every randomized candidate
			// must still beat it by both configured switch margins. If active is
			// already best, use the normal near-best cutoff so random mode can
			// actually rotate among otherwise equivalent healthy endpoints.
			marginCutoff := activeScore - switchMs
			pctCutoff := int64(float64(activeScore) * (1 - switchPct))
			if pctCutoff < marginCutoff {
				cutoff = pctCutoff
			} else {
				cutoff = marginCutoff
			}
		}
		candidates := 0
		for i := range out {
			if out[i].ok && out[i].score <= cutoff {
				candidates++
				continue
			}
			if out[i].ok {
				break
			}
		}
		if candidates > 1 {
			// A tiny time-based rotation is enough; no crypto RNG is needed here.
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
