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
	"io"
	"time"
)

const sequentialForwardPluginType = "sequential_forward"

func init() {
	coremain.RegNewPluginFunc(sequentialForwardPluginType, initSequentialForward, func() interface{} { return new(Args) })
}

var _ coremain.ExecutablePlugin = (*sequentialForward)(nil)

type sequentialForward struct {
	*coremain.BP
	upstreams []upstream.Upstream
	closers   []io.Closer
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
			for _, closer := range f.closers {
				_ = closer.Close()
			}
			return nil, fmt.Errorf("failed to init upstream #%d: %w", i, err)
		}
		f.upstreams = append(f.upstreams, u)
		f.closers = append(f.closers, u)
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
// contacted only when the previous exchange returns an error (including a
// per-attempt timeout). A valid DNS response (including NXDOMAIN or SERVFAIL)
// is considered a response and stops the chain, which avoids duplicate
// upstream traffic and preserves bandwidth.
func (f *sequentialForward) Exec(ctx context.Context, qCtx *query_context.Context, next executable_seq.ExecutableChainNode) error {
	q := qCtx.Q()
	if q == nil {
		return errors.New("missing DNS query")
	}

	var lastErr error
	for i, u := range f.upstreams {
		if err := ctx.Err(); err != nil {
			return err
		}
		// ExchangeContext must not retain or modify q, so reuse the same query
		// object across fallback attempts and avoid an allocation/copy per hop.
		attemptCtx, cancel := attemptContext(ctx, len(f.upstreams)-i)
		r, err := u.ExchangeContext(attemptCtx, q)
		cancel()
		if err == nil {
			if r == nil {
				lastErr = errors.New("upstream returned nil response")
				continue
			}
			qCtx.SetResponse(r)
			return executable_seq.ExecChainNode(ctx, qCtx, next)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("all upstreams failed")
	}
	return lastErr
}

func (f *sequentialForward) Shutdown() error {
	var firstErr error
	for _, closer := range f.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
