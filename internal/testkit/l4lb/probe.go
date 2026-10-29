// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package l4lb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// probeLoop probes be at once, then every ProbeInterval on a fixed
// schedule from its first probe, until ctx ends (be removed or the
// balancer closed). A probe that overruns its slot is followed at the
// next slot.
func (b *Balancer) probeLoop(ctx context.Context, be *backend) {
	defer b.wg.Done()
	timer := b.clk.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	interval := b.cfg.ProbeInterval
	origin := b.clk.Since(b.base)
	var slot int64
	for {
		r := b.probe(ctx, be)
		now := b.clk.Since(b.base)
		slot = max(slot+1, int64((now-origin)/interval)+1)
		// The next probe is armed before the result is published, so a
		// test that waits for this probe can move a fake clock at once.
		timer.Reset(origin + time.Duration(slot)*interval - now)
		b.record(be, r)
		select {
		case <-timer.C():
		case <-ctx.Done():
			return
		}
	}
}

// probe sends one GET to be's ReadyURL within ProbeTimeout on the
// balancer's clock.
func (b *Balancer) probe(ctx context.Context, be *backend) ProbeResult {
	r := ProbeResult{At: b.clk.Now()}
	pctx, cancel := context.WithCancelCause(ctx)
	t := b.clk.AfterFunc(b.cfg.ProbeTimeout, func() { cancel(ErrProbeTimeout) })
	defer cancel(nil)
	defer t.Stop()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, be.ReadyURL, nil)
	if err != nil {
		r.Err = err
		return r
	}
	resp, err := be.client.Do(req)
	if err != nil {
		if errors.Is(context.Cause(pctx), ErrProbeTimeout) && !errors.Is(err, ErrProbeTimeout) {
			err = fmt.Errorf("%w: %w", ErrProbeTimeout, err)
		}
		r.Err = err
		return r
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	_ = resp.Body.Close()
	r.Status = resp.StatusCode
	if err != nil {
		r.Err = err
		return r
	}
	r.OK = resp.StatusCode == http.StatusOK
	if !r.OK {
		var doc adminapi.Readyz
		if json.Unmarshal(body, &doc) == nil && len(doc.Reasons) > 0 {
			r.Reason = doc.Reasons[0].Reason
		}
	}
	return r
}

// record applies a probe to be's state: a new backend's first success
// marks it healthy; UnhealthyThreshold failures in a row mark a healthy
// one unhealthy; HealthyThreshold successes in a row bring it back.
func (b *Balancer) record(be *backend, r ProbeResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if be.removed || b.closed {
		return
	}
	be.probes++
	be.last = r
	if r.OK {
		be.okRun++
		be.failRun = 0
	} else {
		be.failRun++
		be.okRun = 0
		be.probeFailures++
	}
	from := be.state
	switch be.state {
	case Initial:
		if r.OK {
			b.setHealthyLocked(be, be.joined)
		}
	case Healthy:
		if !r.OK && be.failRun >= b.cfg.UnhealthyThreshold {
			be.state = Unhealthy
		}
	case Unhealthy:
		if r.OK && be.okRun >= b.cfg.HealthyThreshold {
			b.setHealthyLocked(be, true)
		}
	}
	if be.state != from {
		be.transitions++
		t := Transition{Backend: be.Name, From: from, To: be.state, At: r.At, Probe: r}
		if len(b.transitions) < TransitionLogSize {
			b.transitions = append(b.transitions, t)
		} else {
			b.transitions[b.transNext] = t
			b.transNext = (b.transNext + 1) % TransitionLogSize
		}
	}
	b.notifyLocked()
}

// setHealthyLocked marks be healthy. A joining backend (one returning
// from Unhealthy, or added by AddBackend) starts slow when another backend
// already serves: alone it takes every connection whatever its weight,
// and backends configured at Start join together at full weight, so
// their shares never depend on which probe answered first.
func (b *Balancer) setHealthyLocked(be *backend, joining bool) {
	be.state = Healthy
	be.healthySince = b.clk.Since(b.base)
	be.current = 0
	be.slow = false
	if joining && b.cfg.SlowStart > 0 {
		for _, o := range b.backends {
			if o != be && o.state == Healthy {
				be.slow = true
				break
			}
		}
	}
}
