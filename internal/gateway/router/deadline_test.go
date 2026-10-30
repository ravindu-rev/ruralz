// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// deadlineWhen is a match.when program that records the request deadline
// of each evaluation and answers ok, or err.
type deadlineWhen struct {
	ok   bool
	err  error
	seen []time.Time
}

func (*deadlineWhen) Place() expr.PlaceID  { return expr.PlaceRouteMatchWhen }
func (*deadlineWhen) Source() string       { return "deadline" }
func (*deadlineWhen) Refs() expr.Refs      { return expr.Refs{} }
func (*deadlineWhen) Cost() expr.CostRange { return expr.CostRange{} }

func (w *deadlineWhen) EvalBool(_ context.Context, v *expr.Vars) (bool, error) {
	w.seen = append(w.seen, v.Deadline)
	return w.ok, w.err
}

func (*deadlineWhen) EvalString(context.Context, *expr.Vars) (string, error)    { return "", nil }
func (*deadlineWhen) EvalInt(context.Context, *expr.Vars) (int64, error)        { return 0, nil }
func (*deadlineWhen) EvalValue(context.Context, *expr.Vars) (expr.Value, error) { return nil, nil }

// TestWhenRunsUnderCandidateDeadline checks that each candidate's
// match.when runs under that candidate's Route deadline, the request start
// plus its Route timeout, at which a tracked comprehension stops
// (docs/architecture/02-configuration-model.md "Limits"): two candidates
// with their own timeouts each see their own, and on return the activation
// holds the winner's deadline, also a winner without match.when, and none
// when no Route wins or a match.when fails. A zero request start or Route
// timeout sets no deadline.
func TestWhenRunsUnderCandidateDeadline(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	const shortTimeout, longTimeout, plainTimeout = 30 * time.Millisecond, 2 * time.Second, 5 * time.Second
	short, long := &deadlineWhen{}, &deadlineWhen{ok: true}
	r, err := Compile(nil, []Route{
		{Index: 0, Name: "a-short", Match: &v1alpha1.RouteMatch{Path: prefixPath("/w"), When: "short"}, When: short, Timeout: shortTimeout},
		{Index: 1, Name: "b-long", Match: &v1alpha1.RouteMatch{Path: prefixPath("/w"), When: "long"}, When: long, Timeout: longTimeout},
		{Index: 2, Name: "plain", Match: &v1alpha1.RouteMatch{Path: prefixPath("/p")}, Timeout: plainTimeout},
		{Index: 3, Name: "untimed", Match: &v1alpha1.RouteMatch{Path: prefixPath("/u"), When: "untimed"}, When: long},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	match := func(path string, start time.Time) (snapshot.MatchResult, *expr.Vars) {
		t.Helper()
		short.seen, long.seen = nil, nil
		v := &expr.Vars{Request: &expr.Request{Method: http.MethodGet, Path: path, Header: http.Header{}}}
		q := snapshot.MatchRequest{Host: "a.example", Path: path, Method: http.MethodGet, Header: http.Header{}, Vars: v, Start: start}
		return r.Match(t.Context(), &q, nil), v
	}

	res, v := match("/w", start)
	if res.Route != 1 || res.Err != nil {
		t.Fatalf("Match(/w) = %+v, want b-long", res)
	}
	if want := []time.Time{start.Add(shortTimeout)}; !slices.Equal(short.seen, want) {
		t.Errorf("a-short's match.when saw deadlines %v, want its own %v", short.seen, want)
	}
	if want := []time.Time{start.Add(longTimeout)}; !slices.Equal(long.seen, want) {
		t.Errorf("b-long's match.when saw deadlines %v, want its own %v", long.seen, want)
	}
	if !v.Deadline.Equal(start.Add(longTimeout)) {
		t.Errorf("the activation's deadline after Match = %v, want the winner's %v", v.Deadline, start.Add(longTimeout))
	}

	if res, v = match("/p", start); res.Route != 2 || !v.Deadline.Equal(start.Add(plainTimeout)) {
		t.Errorf("Match(/p) = %+v with deadline %v, want plain with %v", res, v.Deadline, start.Add(plainTimeout))
	}

	long.ok = false
	if res, v = match("/w", start); res.Route != -1 || len(long.seen) != 1 || !v.Deadline.IsZero() {
		t.Errorf("Match(/w) with every match.when false = %+v with deadline %v after %d evaluations of b-long, want no Route and no deadline", res, v.Deadline, len(long.seen))
	}
	long.err = errors.New("evaluation failed")
	res, v = match("/w", start)
	if code, _ := errcode.CodeOf(res.Err); code != codeWhen || len(long.seen) != 1 || !v.Deadline.IsZero() {
		t.Errorf("Match(/w) with a failing match.when = %+v with deadline %v, want RZ-RT-006 and no deadline", res, v.Deadline)
	}
	long.ok, long.err = true, nil

	if res, v = match("/w", time.Time{}); res.Route != 1 || !short.seen[0].IsZero() || !long.seen[0].IsZero() || !v.Deadline.IsZero() {
		t.Errorf("Match(/w) without a request start = %+v, deadlines %v %v %v; want b-long and none", res, short.seen, long.seen, v.Deadline)
	}
	if res, v = match("/u", start); res.Route != 3 || !long.seen[0].IsZero() || !v.Deadline.IsZero() {
		t.Errorf("Match(/u) of a Route without a timeout = %+v, deadlines %v %v; want untimed and none", res, long.seen, v.Deadline)
	}
}
