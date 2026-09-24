/*
 * Copyright (C) 2020-2022, IrineSistiana
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */

package fastforward

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/IrineSistiana/mosdns/v4/coremain"
	"github.com/IrineSistiana/mosdns/v4/pkg/executable_seq"
	"github.com/IrineSistiana/mosdns/v4/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v4/pkg/upstream"
	"github.com/miekg/dns"
	"sync"
	"time"
)

const sequentialForwardPluginType = "sequential_forward"

func init() {
	coremain.RegNewPluginFunc(sequentialForwardPluginType, initSequentialForward, func() interface{} { return new(Args) })
}

var _ coremain.ExecutablePlugin = (*sequentialForward)(nil)

const (
	upstreamFailureTrip    = 2
	upstreamFailureBackoff = 15 * time.Second
)

type upstreamState struct {
	u upstream.Upstream

	mu                  sync.Mutex
	consecutiveFailures int
	unhealthyUntil      time.Time
}

func (s *upstreamState) isCoolingDown(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.unhealthyUntil.IsZero() && now.Before(s.unhealthyUntil)
}

func (s *upstreamState) recordSuccess() {
	s.mu.Lock()
	s.consecutiveFailures = 0
	s.unhealthyUntil = time.Time{}
	s.mu.Unlock()
}

func (s *upstreamState) recordFailure(now time.Time) {
	s.mu.Lock()
	if s.consecutiveFailures < upstreamFailureTrip {
		s.consecutiveFailures++
	}
	if s.consecutiveFailures >= upstreamFailureTrip {
		s.unhealthyUntil = now.Add(upstreamFailureBackoff)
	}
	s.mu.Unlock()
}

type sequentialForward struct {
	*coremain.BP
	upstreams []*upstreamState
}

func shouldFailoverDNSResponse(r *dns.Msg) bool {
	return r != nil && r.Rcode == dns.RcodeServerFailure
}

func initSequentialForward(bp *coremain.BP, args interface{}) (coremain.Plugin, error) {
	cfg := args.(*Args)
	if len(cfg.Upstream) == 0 {
		return nil, errors.New("no upstream is configured")
	}

	return newSequentialForward(bp, cfg)
}

func newSequentialForward(bp *coremain.BP, args *Args) (*sequentialForward, error) {
	f := &sequentialForward{BP: bp}
	for i, c := range args.Upstream {
		if c == nil || c.Addr == "" {
			return nil, fmt.Errorf("upstream #%d, missing server addr", i)
		}

		opt := &upstream.Opt{
			DialAddr:       c.DialAddr,
			Socks5:         c.Socks5,
			SoMark:         c.SoMark,
			BindToDevice:   c.BindToDevice,
			IdleTimeout:    time.Duration(c.IdleTimeout) * time.Second,
			MaxConns:       c.MaxConns,
			EnablePipeline: c.EnablePipeline,
			EnableHTTP3:    c.EnableHTTP3,
			Bootstrap:      c.Bootstrap,
			TLSConfig: &tls.Config{
				InsecureSkipVerify: c.InsecureSkipVerify,
				ClientSessionCache: tls.NewLRUClientSessionCache(64),
			},
			Logger: bp.L(),
		}

		u, err := upstream.NewUpstream(c.Addr, opt)
		if err != nil {
			for _, state := range f.upstreams {
				_ = state.u.Close()
			}
			return nil, fmt.Errorf("failed to init upstream #%d: %w", i, err)
		}
		f.upstreams = append(f.upstreams, &upstreamState{u: u})
	}
	return f, nil
}

// attemptContext bounds a single upstream attempt so one stalled upstream
// cannot consume the entire query deadline and leave the remaining fallbacks
// with no time to run. The time left is split evenly across the upstreams that
// still have to be tried; the last upstream keeps whatever budget remains. A
// context without a deadline is returned unchanged.
func attemptContext(ctx context.Context, remaining int) (context.Context, context.CancelFunc) {
	if remaining > 1 {
		if deadline, ok := ctx.Deadline(); ok {
			if budget := time.Until(deadline); budget > 0 {
				return context.WithTimeout(ctx, budget/time.Duration(remaining))
			}
		}
	}
	return ctx, func() {}
}

// Exec tries upstreams strictly in configured order. A later upstream is
// contacted when the previous exchange fails, times out, or returns SERVFAIL.
// NXDOMAIN remains a valid policy answer and stops the chain, avoiding
// duplicate upstream traffic and preserving bandwidth.
func (f *sequentialForward) Exec(ctx context.Context, qCtx *query_context.Context, next executable_seq.ExecutableChainNode) error {
	q := qCtx.Q()
	if q == nil {
		return errors.New("missing DNS query")
	}

	// If every upstream is temporarily cooling down, still probe the full
	// configured set rather than turning a transient outage into a total outage.
	now := time.Now()
	anyAvailable := false
	for _, state := range f.upstreams {
		if !state.isCoolingDown(now) {
			anyAvailable = true
			break
		}
	}

	var lastErr error
	var lastSERVFAIL *dns.Msg
	for i, state := range f.upstreams {
		if anyAvailable && state.isCoolingDown(now) {
			continue
		}

		remaining := 0
		for j := i; j < len(f.upstreams); j++ {
			if !anyAvailable || !f.upstreams[j].isCoolingDown(now) {
				remaining++
			}
		}
		if remaining == 0 {
			continue
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// ExchangeContext in the patched MosDNS v4.5.3 DoH upstream now
		// propagates this context into its HTTP request. That makes the split
		// budget an actual cancellation boundary instead of an orphaned 5s
		// request that can occupy the upstream connection pool after failover.
		attemptCtx, cancel := attemptContext(ctx, remaining)
		r, err := state.u.ExchangeContext(attemptCtx, q)
		cancel()
		if err == nil && r != nil {
			if shouldFailoverDNSResponse(r) {
				// SERVFAIL is a transport/service failure from the resolver to the
				// caller. Try the next resolver rather than surfacing a transient
				// failure when another healthy endpoint can answer.
				lastSERVFAIL = r
				if ctx.Err() == nil {
					state.recordFailure(time.Now())
				}
				continue
			}
			state.recordSuccess()
			qCtx.SetResponse(r)
			return executable_seq.ExecChainNode(ctx, qCtx, next)
		}

		if err == nil {
			err = errors.New("upstream returned nil response")
		}
		lastErr = err
		if ctx.Err() == nil {
			state.recordFailure(time.Now())
		}
	}

	if lastSERVFAIL != nil {
		qCtx.SetResponse(lastSERVFAIL)
		return executable_seq.ExecChainNode(ctx, qCtx, next)
	}
	if lastErr == nil {
		lastErr = errors.New("all upstreams failed")
	}
	return lastErr
}

func (f *sequentialForward) Shutdown() error {
	var firstErr error
	for _, state := range f.upstreams {
		if err := state.u.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
