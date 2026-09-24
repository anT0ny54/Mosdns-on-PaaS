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
	primaryUpstreamCount   = 3
	fallbackReserve        = 2 * time.Second
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
	if len(cfg.Upstream) < primaryUpstreamCount+1 {
		return nil, fmt.Errorf("sequential failover requires %d primary upstreams plus 1 emergency fallback", primaryUpstreamCount)
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
// cannot consume the entire query deadline. For a primary attempt, the
// remaining primary-attempt budget is split evenly after reserving a single
// fallbackReserve slice for the independent emergency resolver. The reserve is
// not subtracted again on later primary attempts; as a result a 10s query with
// three primaries gives the primary set about 8s total and keeps 2s available
// for the emergency fallback. A context without a deadline is returned
// unchanged.
func attemptContext(ctx context.Context, remainingPrimary int, reserveFallback bool) (context.Context, context.CancelFunc) {
	reserve := time.Duration(0)
	if reserveFallback {
		reserve = fallbackReserve
	}
	return attemptContextWithReserve(ctx, remainingPrimary, reserve)
}

// attemptContextWithReserve is split out so the failover timing tests can use a
// scaled reserve without waiting through a full 10s production deadline. The
// production path always calls attemptContext, which uses the fixed 2s reserve.
func attemptContextWithReserve(ctx context.Context, remainingPrimary int, reserve time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		if budget := time.Until(deadline); budget > 0 {
			if reserve > 0 {
				if budget <= reserve {
					return context.WithTimeout(ctx, 0)
				}
				budget -= reserve
			}
			if remainingPrimary > 1 {
				return context.WithTimeout(ctx, budget/time.Duration(remainingPrimary))
			}
			return context.WithTimeout(ctx, budget)
		}
	}
	return ctx, func() {}
}

// Exec tries upstreams strictly in configured order. The first three entries
// are the normal policy upstreams; any extra entry is an independent emergency
// fallback and is only reached after those primary attempts fail. A later
// upstream is contacted when the previous exchange fails, times out, or
// returns SERVFAIL. NXDOMAIN remains a valid policy answer and stops the chain,
// avoiding duplicate upstream traffic and preserving bandwidth.
func (f *sequentialForward) Exec(ctx context.Context, qCtx *query_context.Context, next executable_seq.ExecutableChainNode) error {
	return f.execWithAttemptReserve(ctx, qCtx, next, fallbackReserve)
}

// execWithAttemptReserve carries the production reserve as an explicit parameter
// so tests can exercise the same sequential timeout path with a scaled reserve.
func (f *sequentialForward) execWithAttemptReserve(ctx context.Context, qCtx *query_context.Context, next executable_seq.ExecutableChainNode, reserve time.Duration) error {
	q := qCtx.Q()
	if q == nil {
		return errors.New("missing DNS query")
	}

	// If every upstream is temporarily cooling down, still probe the full
	// configured set rather than turning a transient outage into a total outage.
	now := time.Now()
	primaryLimit := primaryUpstreamCount
	if primaryLimit > len(f.upstreams) {
		primaryLimit = len(f.upstreams)
	}
	anyPrimaryAvailable := false
	for i := 0; i < primaryLimit; i++ {
		if !f.upstreams[i].isCoolingDown(now) {
			anyPrimaryAvailable = true
			break
		}
	}

	var lastErr error
	var lastSERVFAIL *dns.Msg
	for i, state := range f.upstreams {
		isFallback := i >= primaryUpstreamCount
		// The emergency resolver is deliberately independent: it is not part of
		// health ordering and must never be skipped by the primary circuit breaker.
		if !isFallback && anyPrimaryAvailable && state.isCoolingDown(now) {
			continue
		}

		remainingPrimary := 0
		if !isFallback {
			primaryLimit := primaryUpstreamCount
			if primaryLimit > len(f.upstreams) {
				primaryLimit = len(f.upstreams)
			}
			for j := i; j < primaryLimit; j++ {
				if !anyPrimaryAvailable || !f.upstreams[j].isCoolingDown(now) {
					remainingPrimary++
				}
			}
			if remainingPrimary == 0 {
				continue
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// ExchangeContext in the patched MosDNS v4.5.3 DoH upstream now
		// propagates this context into its HTTP request. That makes the primary
		// split budget an actual cancellation boundary instead of an orphaned 5s
		// request that can occupy the upstream connection pool after failover.
		// The three primary attempts share one 2s reserve; the emergency attempt
		// receives the whole remaining parent budget instead of another split.
		reserveFallback := !isFallback && len(f.upstreams) > primaryUpstreamCount && reserve > 0
		attemptsLeft := remainingPrimary
		if isFallback {
			attemptsLeft = 1
		}
		var attemptCtx context.Context
		var cancel context.CancelFunc
		if reserveFallback {
			attemptCtx, cancel = attemptContextWithReserve(ctx, attemptsLeft, reserve)
		} else {
			attemptCtx, cancel = attemptContext(ctx, attemptsLeft, false)
		}
		r, err := state.u.ExchangeContext(attemptCtx, q)
		cancel()
		if err == nil && r != nil {
			if shouldFailoverDNSResponse(r) {
				// SERVFAIL is a transport/service failure from the resolver to the
				// caller. Try the next resolver rather than surfacing a transient
				// failure when another healthy endpoint can answer.
				lastSERVFAIL = r
				if !isFallback && ctx.Err() == nil {
					state.recordFailure(time.Now())
				}
				continue
			}
			if !isFallback {
				state.recordSuccess()
			}
			qCtx.SetResponse(r)
			return executable_seq.ExecChainNode(ctx, qCtx, next)
		}

		if err == nil {
			err = errors.New("upstream returned nil response")
		}
		lastErr = err
		if !isFallback && ctx.Err() == nil {
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
