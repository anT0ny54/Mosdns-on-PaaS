package fastforward

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v4/pkg/query_context"
	"github.com/miekg/dns"
)

func TestUpstreamStateTripsAndRecovers(t *testing.T) {
	var s upstreamState
	base := time.Unix(1000, 0)

	if s.isCoolingDown(base) {
		t.Fatal("new upstream must not start in cooldown")
	}
	s.recordFailure(base)
	if s.isCoolingDown(base) {
		t.Fatal("one failure must not trip the circuit")
	}
	s.recordFailure(base.Add(time.Second))
	if !s.isCoolingDown(base.Add(time.Second)) {
		t.Fatal("second consecutive failure must trip the circuit")
	}
	if s.isCoolingDown(base.Add(upstreamFailureBackoff + 2*time.Second)) {
		t.Fatal("cooldown must expire")
	}

	s.recordSuccess()
	if s.isCoolingDown(base.Add(upstreamFailureBackoff + 2*time.Second)) {
		t.Fatal("a successful response must clear the circuit")
	}
}

func TestAttemptContextSplitsRemainingBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()

	attempt, attemptCancel := attemptContext(ctx, 3)
	defer attemptCancel()
	deadline, ok := attempt.Deadline()
	if !ok {
		t.Fatal("attempt context lost its deadline")
	}
	remaining := time.Until(deadline)
	want := 3 * time.Second
	if remaining < want-150*time.Millisecond || remaining > want+150*time.Millisecond {
		t.Fatalf("first attempt budget = %s, want about %s", remaining, want)
	}

	last, lastCancel := attemptContext(ctx, 1)
	defer lastCancel()
	lastDeadline, ok := last.Deadline()
	if !ok {
		t.Fatal("last attempt context lost its deadline")
	}
	lastBudget := time.Until(lastDeadline)
	if lastBudget < 8*time.Second || lastBudget > 9*time.Second {
		t.Fatalf("last attempt budget = %s, want about 9s", lastBudget)
	}
}

type scriptedUpstream struct {
	mu             sync.Mutex
	calls          int
	response       *dns.Msg
	err            error
	waitForContext bool
}

func (s *scriptedUpstream) ExchangeContext(ctx context.Context, _ *dns.Msg) (*dns.Msg, error) {
	s.mu.Lock()
	s.calls++
	wait := s.waitForContext
	s.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return s.response, s.err
	}
	return s.response, nil
}

func (s *scriptedUpstream) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Test the actual sequential timeout path with equal-share deadlines. The
// regression test is scaled down so two stalled upstreams cannot consume the
// parent deadline before the third configured upstream is attempted.
func TestSequentialFailoverReachesThirdUpstreamAfterTwoTimeouts(t *testing.T) {
	totalBudget := 1200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), totalBudget)
	defer cancel()

	stalled := func() *scriptedUpstream {
		return &scriptedUpstream{waitForContext: true}
	}
	third := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: stalled()}, {u: stalled()}, {u: third}},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("third upstream response not returned: %#v", qctx.R())
	}
	for i, state := range f.upstreams[:2] {
		if got := state.u.(*scriptedUpstream).Calls(); got != 1 {
			t.Fatalf("timed-out upstream %d call count = %d, want 1", i+1, got)
		}
	}
	if third.Calls() != 1 {
		t.Fatalf("third upstream call count = %d, want 1", third.Calls())
	}
}

func TestSequentialFailoverReachesThirdUpstreamAfterTwoSERVFAILs(t *testing.T) {
	servfail := func() *scriptedUpstream {
		return &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}
	}
	third := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: servfail()}, {u: servfail()}, {u: third}},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("third upstream response not returned: %#v", qctx.R())
	}
	if got := f.upstreams[0].u.(*scriptedUpstream).Calls(); got != 1 {
		t.Fatalf("first upstream call count = %d, want 1", got)
	}
	if got := f.upstreams[1].u.(*scriptedUpstream).Calls(); got != 1 {
		t.Fatalf("second upstream call count = %d, want 1", got)
	}
	if third.Calls() != 1 {
		t.Fatalf("third upstream call count = %d, want 1", third.Calls())
	}
}

func TestSequentialShutdownToleratesUpstreamsWithoutClose(t *testing.T) {
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: &scriptedUpstream{}}, {u: &scriptedUpstream{}}, {u: &scriptedUpstream{}}},
	}
	if err := f.Shutdown(); err != nil {
		t.Fatalf("Shutdown returned error for Upstream without Close: %v", err)
	}
}

func TestSequentialFailoverPreservesLastSERVFAILWithoutFourthFallback(t *testing.T) {
	servfail := func() *scriptedUpstream {
		return &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}
	}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: servfail()}, {u: servfail()}, {u: servfail()}},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeServerFailure {
		t.Fatalf("last SERVFAIL response was not preserved: %#v", qctx.R())
	}
}

func TestSequentialFailoverSkipsCoolingUpstreamWhenAnotherIsHealthy(t *testing.T) {
	first := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	second := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	third := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: first}, {u: second}, {u: third}},
	}
	f.upstreams[0].unhealthyUntil = time.Now().Add(time.Minute)
	f.upstreams[0].consecutiveFailures = upstreamFailureTrip

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("healthy response not returned: %#v", qctx.R())
	}
	if first.Calls() != 0 {
		t.Fatalf("cooling upstream call count = %d, want 0", first.Calls())
	}
	if second.Calls() != 1 {
		t.Fatalf("healthy upstream call count = %d, want 1", second.Calls())
	}
	if third.Calls() != 0 {
		t.Fatalf("unused upstream call count = %d, want 0", third.Calls())
	}
}

func TestSequentialFailoverDoesNotBypassValidNonSERVFAILResponse(t *testing.T) {
	refused := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeRefused}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: refused}, {u: &scriptedUpstream{err: errors.New("must not be called")}}, {u: &scriptedUpstream{err: errors.New("must not be called")}}},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeRefused {
		t.Fatalf("valid REFUSED response was not preserved: %#v", qctx.R())
	}
}

func TestSequentialFailoverPreservesNXDOMAIN(t *testing.T) {
	nxdomain := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeNameError}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{{u: nxdomain}, {u: &scriptedUpstream{err: errors.New("must not be called")}}, {u: &scriptedUpstream{err: errors.New("must not be called")}}},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeNameError {
		t.Fatalf("NXDOMAIN response was not preserved: %#v", qctx.R())
	}
}

func TestShouldFailoverDNSResponse(t *testing.T) {
	cases := []struct {
		name  string
		rcode int
		want  bool
	}{
		{name: "success", rcode: 0, want: false},
		{name: "nxdomain", rcode: 3, want: false},
		{name: "servfail", rcode: 2, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFailoverDNSResponse(&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: tc.rcode}}); got != tc.want {
				t.Fatalf("shouldFailoverDNSResponse(rcode=%d) = %v, want %v", tc.rcode, got, tc.want)
			}
		})
	}
	if shouldFailoverDNSResponse(nil) {
		t.Fatal("nil response must not trigger failover")
	}
}
