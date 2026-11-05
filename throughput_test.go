package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// postDNSQuery sends one DoH POST from a synthetic client address (client i
// of 250) and drains the response. It fails on a transport error, a non-200
// status, or a body that cannot be read or closed.
func postDNSQuery(client *http.Client, target string, i int) error {
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(testDNSBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i%250+1))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status=%d", resp.StatusCode)
	}
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func BenchmarkGatewayParallelPOST(b *testing.B) {
	srv, client, cleanup := throughputHarness(b)
	defer srv.Close()
	defer client.CloseIdleConnections()
	defer cleanup()
	b.ReportAllocs()
	b.ResetTimer()
	var next uint64
	var wg sync.WaitGroup
	// Keep concurrency within the gateway's admission ceiling even on CI
	// hosts where GOMAXPROCS exceeds defaultMaxActiveRequests.
	workers := defaultMaxActiveRequests
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddUint64(&next, 1)
				if i > uint64(b.N) {
					return
				}
				if err := postDNSQuery(client, srv.URL+"/dns-query", int(i)); err != nil {
					b.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestFiveThousandRPSTarget(t *testing.T) {
	if testing.Short() || !getenvBool("REQUIRE_5000_RPS") {
		t.Skip("set REQUIRE_5000_RPS=1 to enforce the 5,000 RPS throughput gate")
	}
	const warmup = 1000
	const requests = 10000
	const target = 5000.0
	srv, client, cleanup := throughputHarness(t)
	defer srv.Close()
	defer client.CloseIdleConnections()
	defer cleanup()
	send := func(n int) int64 {
		var failures int64
		// Stay within the active-request ceiling: more in-flight requests than
		// defaultMaxActiveRequests are rejected with 503 by design.
		workers := defaultMaxActiveRequests
		jobs := make(chan int)
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range jobs {
					if err := postDNSQuery(client, srv.URL+"/dns-query", i); err != nil {
						atomic.AddInt64(&failures, 1)
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

func throughputHarness(tb testing.TB) (*httptest.Server, *http.Client, func()) {
	backend := okBackend()
	// 16 workers share 8 processing slots; the long QueueWait keeps a slow or
	// heavily loaded host from turning normal queueing into 503s.
	g := newTestGateway(tb, gatewayOptions{
		BackendURL:       backend.URL,
		RateLimit:        int(^uint(0) >> 1),
		RateLimitClients: 256, // 250 synthetic client IPs are used below.
		QueueWait:        5 * time.Second,
		TrustedIPHeader:  "X-Forwarded-For",
	})
	server := httptest.NewServer(g)
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     64,
		DisableCompression:  true,
	}, Timeout: 5 * time.Second}
	return server, client, backend.Close
}

func getenvBool(key string) bool {
	v := envString(key, "")
	return v == "1" || v == "true"
}
