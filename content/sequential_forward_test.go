package fastforward

import (
	"context"
	"errors"
	"github.com/IrineSistiana/mosdns/v4/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v4/pkg/upstream"
	"github.com/miekg/dns"
	"sync"
	"testing"
	"time"
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

func TestAttemptContextReservesEmergencyFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	attempt, attemptCancel := attemptContext(ctx, 3, true)
	defer attemptCancel()
	deadline, ok := attempt.Deadline()
	if !ok {
		t.Fatal("attempt context lost its deadline")
	}
	remaining := time.Until(deadline)
	want := (8 * time.Second) / 3
	if remaining < want-150*time.Millisecond || remaining > want+150*time.Millisecond {
		t.Fatalf("first primary attempt budget = %s, want about %s", remaining, want)
	}

	lastPrimary, lastPrimaryCancel := attemptContext(ctx, 1, true)
	defer lastPrimaryCancel()
	lastDeadline, ok := lastPrimary.Deadline()
	if !ok {
		t.Fatal("last primary context lost its deadline")
	}
	lastBudget := time.Until(lastDeadline)
	if lastBudget < 7*time.Second || lastBudget > 9*time.Second {
		t.Fatalf("last primary attempt budget = %s, want about 8s", lastBudget)
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

// Test the actual sequential timeout path with a scaled reserve. The production
// path uses the fixed 2s reserve; this smaller reserve keeps the regression test
// fast while proving three context-respecting stalled primaries cannot consume
// the parent deadline before the emergency resolver is attempted.
func TestSequentialFailoverReachesEmergencyAfterThreeTimeouts(t *testing.T) {
	totalBudget := 1200 * time.Millisecond
	testReserve := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), totalBudget)
	defer cancel()

	stalled := func() *scriptedUpstream {
		return &scriptedUpstream{waitForContext: true}
	}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: stalled()},
			{u: stalled()},
			{u: stalled()},
			{u: emergency},
		},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	start := time.Now()
	if err := f.execWithAttemptReserve(ctx, qctx, nil, testReserve); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	elapsed := time.Since(start)

	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("emergency response not returned: %#v", qctx.R())
	}
	for i, state := range f.upstreams[:3] {
		if got := state.u.(*scriptedUpstream).Calls(); got != 1 {
			t.Fatalf("primary upstream %d call count = %d, want 1", i+1, got)
		}
	}
	if emergency.Calls() != 1 {
		t.Fatalf("emergency upstream call count = %d, want 1", emergency.Calls())
	}
	if elapsed < totalBudget-testReserve-150*time.Millisecond || elapsed >= totalBudget {
		t.Fatalf("failover elapsed = %s, want roughly %s-%s", elapsed, totalBudget-testReserve-150*time.Millisecond, totalBudget)
	}
}

func (s *scriptedUpstream) CloseIdleConnections() {}

func (s *scriptedUpstream) Close() error { return nil }

func (s *scriptedUpstream) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var _ upstream.Upstream = (*scriptedUpstream)(nil)

func TestSequentialFailoverReachesEmergencyAfterThreePrimarySERVFAILs(t *testing.T) {
	servfail := func() *scriptedUpstream {
		return &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}
	}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: servfail()},
			{u: servfail()},
			{u: servfail()},
			{u: emergency},
		},
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("emergency response not returned: %#v", qctx.R())
	}
	for i, state := range f.upstreams {
		if got := state.u.(*scriptedUpstream).Calls(); got != 1 {
			t.Fatalf("upstream %d call count = %d, want 1", i+1, got)
		}
	}
}

func TestSequentialFailoverEmergencyIgnoresCircuitBreaker(t *testing.T) {
	servfail := func() *scriptedUpstream {
		return &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}
	}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: servfail()},
			{u: servfail()},
			{u: servfail()},
			{u: emergency},
		},
	}
	cooldownUntil := time.Now().Add(time.Minute)
	f.upstreams[3].unhealthyUntil = cooldownUntil
	f.upstreams[3].consecutiveFailures = upstreamFailureTrip

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("emergency response not returned: %#v", qctx.R())
	}
	if emergency.Calls() != 1 {
		t.Fatalf("emergency upstream call count = %d, want 1", emergency.Calls())
	}
}

func TestSequentialFailoverProbesAllPrimariesWhenAllAreCooling(t *testing.T) {
	servfail := func() *scriptedUpstream {
		return &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}}
	}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: servfail()},
			{u: servfail()},
			{u: servfail()},
			{u: emergency},
		},
	}
	now := time.Now()
	for _, state := range f.upstreams[:3] {
		state.unhealthyUntil = now.Add(time.Minute)
		state.consecutiveFailures = upstreamFailureTrip
	}

	qctx := query_context.NewContext(&dns.Msg{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := f.Exec(ctx, qctx, nil); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if qctx.R() == nil || qctx.R().Rcode != dns.RcodeSuccess {
		t.Fatalf("emergency response not returned: %#v", qctx.R())
	}
	for i, state := range f.upstreams[:3] {
		if got := state.u.(*scriptedUpstream).Calls(); got != 1 {
			t.Fatalf("cooling primary upstream %d call count = %d, want 1", i+1, got)
		}
	}
	if emergency.Calls() != 1 {
		t.Fatalf("emergency upstream call count = %d, want 1", emergency.Calls())
	}
}

func TestSequentialFailoverDoesNotBypassValidNonSERVFAILResponse(t *testing.T) {
	refused := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeRefused}}}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: refused},
			{u: &scriptedUpstream{err: errors.New("must not be called")}},
			{u: &scriptedUpstream{err: errors.New("must not be called")}},
			{u: emergency},
		},
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
	if emergency.Calls() != 0 {
		t.Fatalf("emergency upstream call count = %d, want 0", emergency.Calls())
	}
}

func TestSequentialFailoverPreservesNXDOMAINAndSkipsEmergency(t *testing.T) {
	nxdomain := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeNameError}}}
	emergency := &scriptedUpstream{response: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}}
	f := &sequentialForward{
		upstreams: []*upstreamState{
			{u: nxdomain},
			{u: &scriptedUpstream{err: errors.New("must not be called")}},
			{u: &scriptedUpstream{err: errors.New("must not be called")}},
			{u: emergency},
		},
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
	if nxdomain.Calls() != 1 {
		t.Fatalf("NXDOMAIN upstream call count = %d, want 1", nxdomain.Calls())
	}
	if emergency.Calls() != 0 {
		t.Fatalf("emergency upstream call count = %d, want 0", emergency.Calls())
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
