package fastforward

import (
	"github.com/miekg/dns"
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
