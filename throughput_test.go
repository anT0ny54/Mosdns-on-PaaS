package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkGateway5000RPS is a sustained-throughput regression benchmark for
// the 5,000 RPS target. It uses a local in-process backend so the result
// measures gateway/admission/HTTP overhead rather than public DNS latency.
func BenchmarkGateway5000RPS(b *testing.B) {
	_, srv, client, cleanup := throughputHarness()
	defer srv.Close()
	defer client.CloseIdleConnections()
	defer cleanup()

	b.ReportAllocs()
	b.ResetTimer()
	var next uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddUint64(&next, 1)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/dns-query", strings.NewReader(testDNSBody))
			req.Header.Set("Content-Type", "application/dns-message")
			req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(int(i%250)+1))
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				b.Fatalf("status=%d", resp.StatusCode)
			}
			resp.Body.Close()
		}
	})
}

// TestFiveThousandRPSTarget performs a short, warm sustained run when the
// environment explicitly requests a hard throughput gate. Keeping the gate
// opt-in avoids making ordinary unit-test environments depend on CPU speed.
func TestFiveThousandRPSTarget(t *testing.T) {
	if testing.Short() || getenvBool("REQUIRE_5000_RPS") == false {
		t.Skip("set REQUIRE_5000_RPS=1 to enforce the 5,000 RPS throughput gate")
	}

	const warmup = 1000
	const requests = 10000
	const target = 5000.0

	_, srv, client, cleanup := throughputHarness()
	defer srv.Close()
	defer client.CloseIdleConnections()
	defer cleanup()

	send := func(n int) int64 {
		var failures int64
		workers := 64
		jobs := make(chan int)
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range jobs {
					req, _ := http.NewRequest(http.MethodPost, srv.URL+"/dns-query", strings.NewReader(testDNSBody))
					req.Header.Set("Content-Type", "application/dns-message")
					req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i%250+1))
					resp, err := client.Do(req)
					if err != nil || resp.StatusCode != http.StatusOK {
						atomic.AddInt64(&failures, 1)
					}
					if resp != nil {
						resp.Body.Close()
					}
				}
			}()
		}
		for i := 0; i < n; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		return failures
	}

	if failures := send(warmup); failures != 0 {
		t.Fatalf("warmup failures=%d", failures)
	}
	start := time.Now()
	if failures := send(requests); failures != 0 {
		t.Fatalf("throughput-run failures=%d", failures)
	}
	d := time.Since(start)
	rps := float64(requests) / d.Seconds()
	t.Logf("sustained throughput=%.0f RPS over %s", rps, d.Round(time.Millisecond))
	if rps < target {
		t.Fatalf("throughput %.0f RPS is below target %.0f RPS", rps, target)
	}
}

func throughputHarness() (*gateway, *httptest.Server, *http.Client, func()) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write([]byte{0x01})
	}))
	limiter := newFixedWindowLimiterWithMaxKeys(100000, time.Minute, defaultRateLimitClients)
	limiter.now = func() time.Time { return time.Unix(120, 0) }
	g := &gateway{
		backendURL:       backend.URL,
		client:           newHTTPClient(defaultConcurrency, defaultUpstreamTO),
		limiter:          limiter,
		activeSlots:      make(chan struct{}, defaultMaxActiveRequests),
		processingSlots:  make(chan struct{}, defaultConcurrency),
		queueWait:        defaultQueueWait,
		upstreamTimeout:  defaultUpstreamTO,
		maxRequestBytes:  maxDNSMessageBytes,
		maxResponseBytes: maxDNSMessageBytes,
		trustedIPHeader:  "X-Forwarded-For",
	}
	server := httptest.NewServer(g)
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     64,
		DisableCompression:  true,
	}, Timeout: 5 * time.Second}
	return g, server, client, backend.Close
}

func getenvBool(key string) bool {
	return envString(key, "") == "1" || envString(key, "") == "true"
}
