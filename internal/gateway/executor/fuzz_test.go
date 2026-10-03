// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FuzzChain drives random chains (classes, failureModes, when results,
// Phase subscriptions, outcomes including panics, SPI sentinels and
// Consumptive members) through every client Phase and one leg, and checks
// the executor's invariants (spec 04 reqs 39 to 44, R-40, R-42):
//
//   - no panic escapes, and every Filter call follows Enter for its Policy;
//   - a request Phase call never follows a short-circuit;
//   - a Policy that failed open is never called again;
//   - Finish runs exactly for the Finishers that ran, in request order,
//     after every onLog call;
//   - RecordFailure and the failure metric agree, spec.when runtime errors
//     included (spec 03 req 43), and every generated response carries a
//     registered-looking code or the Filter's own;
//   - a security-class Policy is never recorded as failing open.
func FuzzChain(f *testing.F) {
	f.Add([]byte{0x00, 0x11, 0x22, 0x33})
	f.Add([]byte{0x13, 0x07, 0x45, 0x21, 0x98, 0x30, 0x02})
	f.Add([]byte{0x31, 0x1f, 0x05, 0x31, 0x1f, 0x06, 0x31, 0x1f, 0x01, 0x10})
	f.Add([]byte{0x80, 0xff, 0x44, 0x84, 0x0f, 0x03, 0x12, 0x1b, 0x02, 0x55})
	f.Add([]byte("authz-cel-ratelimit-quota-headers-cache"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzOne(t, data)
	})
}

// fuzzPhases are the client Phases a fuzzed Policy may subscribe to.
var fuzzPhases = [...]phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute, phase.OnResponse, phase.OnLog}

func fuzzOne(t *testing.T, data []byte) {
	h := newHarness(t)
	next := func() byte {
		if len(data) == 0 {
			return 0
		}
		b := data[0]
		data = data[1:]
		return b
	}
	n := int(next()%7) + 1
	var ps []*snapshot.Policy
	probes := map[string]*probe{}
	finishers := map[string]bool{}
	for i := range n {
		name := fmt.Sprintf("p%d", i)
		b := next()
		class := phase.Class(b % uint8(phase.NumClasses))
		mode := v1alpha1.FailureModeClosed
		if b&0x80 != 0 {
			mode = v1alpha1.FailureModeOpen
		}
		subs := next()
		var phases []phase.Phase
		script := map[phase.Phase]result{}
		for j, ph := range fuzzPhases {
			if subs&(1<<j) == 0 {
				continue
			}
			phases = append(phases, ph)
			script[ph] = fuzzResult(next())
		}
		if len(phases) == 0 {
			phases = []phase.Phase{phase.OnRequestHeaders}
		}
		s := spec{name: name, class: class, mode: mode, phases: phases}
		switch w := next() % 5; w {
		case 1:
			s.when = &constProgram{ok: true}
		case 2:
			s.when = &constProgram{ok: false}
		case 3:
			s.when = &constProgram{err: errors.New("cel: null")}
		case 4:
			s.when = &constProgram{panic: true}
		}
		switch {
		case subs&0x80 != 0 && class == phase.ClassAdmission && slices.Contains(phases, phase.OnRequestHeaders):
			c := h.consumer(name)
			c.script = script
			switch next() % 4 {
			case 1:
				c.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
					return filter.Deny(429, "RZ-RL-001", nil), true
				}
			case 2:
				c.complete = func(filter.Exchange, *statestore.Call) filter.Result {
					return filter.Undecided("RZ-STS-001", statestore.ErrTimeout)
				}
			}
			s.f = &finishConsumer{consumer: c}
			finishers[name] = true
		case subs&0x40 != 0:
			s.f = &finishFilter{fakeFilter{name: name, ev: h.ev, script: script, st: h.st, t: t}}
			finishers[name] = true
		default:
			s.f = h.filter(name, script)
		}
		p := policy(s)
		m, pr := newProbe()
		p.Metrics = m
		probes[name] = pr
		ps = append(ps, p)
	}
	ch := chain(ps, nil)

	ctx := context.Background()
	r := h.e.Begin(h.st, ch, h.store)
	var short *filter.Response
	shortAt := -1
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		if short = r.Request(ctx, ph); short != nil {
			shortAt = len(h.ev.list())
			break
		}
	}
	kind := ResponseUpstream
	if short != nil {
		kind = ResponseGenerated
		if short.Status == 0 {
			t.Fatalf("short-circuit without a status: %+v", short)
		}
	}
	repl := r.Response(ctx, kind)
	if repl != nil && (repl.Status < 400 || repl.Code == "") {
		t.Fatalf("replacement %+v", repl)
	}
	logStart := len(h.ev.list())
	r.Log(ctx)
	r.Release()

	evs := h.ev.list()
	// No request-Phase call after the short-circuit.
	if shortAt >= 0 {
		for _, e := range evs[shortAt:] {
			for _, ph := range []string{".onRequestHeaders", ".onRequestBody", ".onRoute", ".prepare", ".complete"} {
				if strings.HasSuffix(e, ph) {
					t.Fatalf("%s after the short-circuit: %v", e, evs)
				}
			}
		}
	}
	// Failed-open Policies are not called after their failure.
	for _, fr := range h.st.fails {
		if fr.mode != v1alpha1.FailureModeOpen || fr.ph == phase.OnLog {
			continue
		}
		last := -1
		for i, e := range evs {
			if strings.HasPrefix(e, fr.policy+".") && !strings.HasSuffix(e, ".finish") && !strings.HasSuffix(e, ".undo") {
				last = i
			}
		}
		if last >= 0 && !strings.HasSuffix(evs[last], fr.ph.String()) && !strings.HasSuffix(evs[last], ".complete") && !strings.HasSuffix(evs[last], ".prepare") {
			t.Fatalf("%s called after failing open in %s: %v", fr.policy, fr.ph, evs)
		}
	}
	// Finish: exactly the Finishers that ran, request order, after onLog.
	var wantFin []string
	for _, p := range ps {
		if decided, skip := h.st.When(p); decided && !skip && finishers[p.Name] {
			wantFin = append(wantFin, p.Name+".finish")
		}
	}
	gotFin := filterSuffix(evs, ".finish")
	if !slices.Equal(gotFin, wantFin) {
		t.Fatalf("finish %v, want %v (events %v)", gotFin, wantFin, evs)
	}
	for _, e := range evs[logStart:] {
		if strings.HasSuffix(e, ".finish") {
			break
		}
		if !strings.HasSuffix(e, ".onLog") {
			t.Fatalf("%s during onLog", e)
		}
	}
	// Failure records and metrics agree.
	var metricFails int64
	for _, pr := range probes {
		for ph := range phase.Count {
			metricFails += pr.failures(ph, 0) + pr.failures(ph, 1)
		}
	}
	if int64(len(h.st.fails)) != metricFails {
		t.Fatalf("RecordFailure %d, metric %d", len(h.st.fails), metricFails)
	}
	classes := map[string]phase.Class{}
	for _, p := range ps {
		classes[p.Name] = p.Class
	}
	for _, fr := range h.st.fails {
		if fr.mode == v1alpha1.FailureModeOpen && closedOnlyClass(classes[fr.policy]) {
			t.Fatalf("%s (%s) failed open in %s", fr.policy, classes[fr.policy], fr.ph)
		}
	}
}

// fuzzResult maps a byte to a scripted Result.
func fuzzResult(b byte) result {
	switch b % 9 {
	case 0, 1, 2:
		return nil // Continue
	case 3:
		return respondWith(403, "RZ-AUTH-010")
	case 4:
		return undecided("")
	case 5:
		return undecided("RZ-STS-002")
	case 6:
		return outcome(filter.Retry)
	case 7:
		return panics
	default:
		return failWith(filter.ErrBudget)
	}
}

// finishConsumer is a Consumptive Finisher.
type finishConsumer struct{ *consumer }

func (f *finishConsumer) Finish(context.Context, filter.Exchange) { f.ev.add(f.name + ".finish") }
