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

const (
	maxHealthStateFileBytes = 64 << 10
	maxHealthStateLineBytes = 8 << 10
	maxHealthStateEntries   = 16
)

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
	// DNS messages are bounded by the wire-format maximum. Read the complete
	// response so a header-only or otherwise truncated body cannot score as healthy.
	const maxDNSMessageBytes = 65535
	if resp.ContentLength > maxDNSMessageBytes {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessageBytes+1))
	if err != nil || len(body) > maxDNSMessageBytes {
		return 0, false
	}
	if resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength {
		return 0, false
	}
	if !validProbeDNSResponse(id, body) {
		return 0, false
	}
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms, true
}

// validProbeDNSResponse performs a bounded wire-level validation without pulling
// the full DNS package into the one-shot probe helper. The probe only needs to
// establish that the endpoint returned a complete, wire-structurally valid DNS
// response with one A/IN question. RDATA is treated as opaque bytes; an empty
// NOERROR answer is still a healthy resolver outcome.
func validProbeDNSResponse(id uint16, body []byte) bool {
	if len(body) < 12 || binary.BigEndian.Uint16(body[0:2]) != id {
		return false
	}
	flags := binary.BigEndian.Uint16(body[2:4])
	if flags&0x8000 == 0 || flags&0x7800 != 0 || flags&0x0200 != 0 {
		return false
	}
	if binary.BigEndian.Uint16(body[4:6]) != 1 {
		return false
	}
	rcode := flags & 0x000f
	if rcode != 0 && rcode != 3 {
		return false
	}

	questionEnd, ok := dnsNameEnd(body, 12)
	if !ok || questionEnd+4 > len(body) {
		return false
	}
	// The fixed health query asks for A/IN. Validate those fields so a random
	// DNS-shaped payload cannot pass solely because its header looks plausible.
	if binary.BigEndian.Uint16(body[questionEnd:questionEnd+2]) != 1 ||
		binary.BigEndian.Uint16(body[questionEnd+2:questionEnd+4]) != 1 {
		return false
	}
	offset := questionEnd + 4
	for _, count := range []uint16{
		binary.BigEndian.Uint16(body[6:8]),   // ANCOUNT
		binary.BigEndian.Uint16(body[8:10]),  // NSCOUNT
		binary.BigEndian.Uint16(body[10:12]), // ARCOUNT
	} {
		var ok bool
		offset, ok = dnsRecordsEnd(body, offset, count)
		if !ok {
			return false
		}
	}
	return offset == len(body)
}

// dnsRecordsEnd validates count generic DNS resource records starting at off and
// returns the byte after the final RDATA field. RDATA is opaque because its exact
// structure is type-specific, while RDLength still lets us prove the body is complete.
func dnsRecordsEnd(msg []byte, off int, count uint16) (int, bool) {
	for i := uint16(0); i < count; i++ {
		nameEnd, ok := dnsNameEnd(msg, off)
		if !ok || nameEnd+10 > len(msg) {
			return 0, false
		}
		rdataLen := int(binary.BigEndian.Uint16(msg[nameEnd+8 : nameEnd+10]))
		off = nameEnd + 10
		if off+rdataLen > len(msg) {
			return 0, false
		}
		off += rdataLen
	}
	return off, true
}

// dnsNameEnd returns the first byte after a DNS name field. It validates label
// bounds and compression pointers, but does not allocate or expand the name.
func dnsNameEnd(msg []byte, off int) (int, bool) {
	if off < 0 || off >= len(msg) {
		return 0, false
	}
	seen := make(map[int]struct{}, 4)
	return dnsNameEndSeen(msg, off, seen, 0)
}

func dnsNameEndSeen(msg []byte, off int, seen map[int]struct{}, depth int) (int, bool) {
	if depth > 16 || off < 0 || off >= len(msg) {
		return 0, false
	}
	start := off
	for {
		if off >= len(msg) {
			return 0, false
		}
		length := msg[off]
		switch length & 0xc0 {
		case 0x00:
			off++
			if length == 0 {
				return off, true
			}
			if length > 63 || off+int(length) > len(msg) {
				return 0, false
			}
			off += int(length)
		case 0xc0:
			if off+1 >= len(msg) {
				return 0, false
			}
			pointer := int(length&0x3f)<<8 | int(msg[off+1])
			if pointer >= len(msg) || pointer >= off {
				return 0, false
			}
			if _, ok := seen[pointer]; ok {
				return 0, false
			}
			seen[pointer] = struct{}{}
			if _, ok := dnsNameEndSeen(msg, pointer, seen, depth+1); !ok {
				return 0, false
			}
			return off + 2, true
		default:
			return 0, false
		}
		if off == start {
			return 0, false
		}
	}
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

	if info, err := f.Stat(); err != nil || info.Size() > maxHealthStateFileBytes {
		return make(map[string]state)
	}

	s := bufio.NewScanner(io.LimitReader(f, maxHealthStateFileBytes+1))
	s.Buffer(make([]byte, 1024), maxHealthStateLineBytes)
	for s.Scan() {
		if len(m) >= maxHealthStateEntries {
			break
		}
		p := strings.Split(s.Text(), "\t")
		if len(p) != 3 || p[0] == "" {
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
	// 2000 matches entrypoint.sh's HEALTH_TIMEOUT_MS default and the value
	// documented in README.md; kept in sync so a direct invocation of this
	// binary (outside the shell wrapper) behaves the same as the deployed
	// default instead of silently using a shorter timeout.
	timeout := time.Duration(envIntOrDefault("HEALTH_TIMEOUT_MS", 2000)) * time.Millisecond
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
