// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// effectiveRow is one row of ruralz bundle render --effective --route
// NAME --output json (docs/reference/01-cli-and-api-surface.md "Bundle
// verbs in detail").
type effectiveRow struct {
	Phase       string `json:"phase"`
	Leg         string `json:"leg"`
	Policy      string `json:"policy"`
	Type        string `json:"type"`
	FilterClass string `json:"filterClass"`
	Slot        string `json:"slot"`
	From        string `json:"from"`
	Reason      string `json:"reason"`
}

// decodeRows decodes the JSON rows of one Route's effective chain.
func decodeRows(t testing.TB, out string) []effectiveRow {
	t.Helper()
	var rows []effectiveRow
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("effective rows %q: %v", out, err)
	}
	return rows
}

// Phases without a span: onLog creates none, and a removed Policy runs in
// no Phase.
const (
	phaseLog  = "onLog"
	phaseNone = "none"
	legClient = "client"
)

// chainKey is one Phase on one leg.
type chainKey struct{ phase, leg string }

func (k chainKey) String() string { return k.phase + "/" + k.leg }

// filterSpan is a ruralz.filter.<policy> span placed on its leg.
type filterSpan struct {
	policy, typ string
	key         chainKey
	start       uint64
	seq         int
}

// serverSpan returns the request's server span: the SERVER span whose
// parent is parentID, the span of the client's traceparent.
func serverSpan(trace []span, parentID string) (span, error) {
	for _, s := range trace {
		if s.Kind == kindServer && s.ParentID == parentID {
			return s, nil
		}
	}
	return span{}, fmt.Errorf("no server span under the client's span %s among %d spans", parentID, len(trace))
}

// filterSpans returns the trace's filter spans under the server span, each
// on its leg: the Upstream of the nearest ruralz.upstream.<name> ancestor,
// or the client leg when the walk up reaches the server span first.
func filterSpans(trace []span, server span) ([]filterSpan, error) {
	byID := make(map[string]span, len(trace))
	for _, s := range trace {
		byID[s.SpanID] = s
	}
	var out []filterSpan
	var errs []error
	for _, s := range trace {
		policy, ok := strings.CutPrefix(s.Name, catalog.SpanFilterPrefix)
		if !ok {
			continue
		}
		leg, err := legOf(byID, s, server.SpanID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, filterSpan{
			policy: policy, typ: s.Attrs[catalog.AttrPolicyType],
			key:   chainKey{phase: s.Attrs[catalog.AttrPhase], leg: leg},
			start: s.Start, seq: s.Seq,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].seq < out[j].seq
	})
	return out, errors.Join(errs...)
}

// legOf walks up from s to its leg.
func legOf(byID map[string]span, s span, serverID string) (string, error) {
	cur := s
	for range len(byID) + 1 {
		parent, ok := byID[cur.ParentID]
		switch {
		case cur.ParentID == serverID:
			return legClient, nil
		case !ok:
			return "", fmt.Errorf("span %s (%s): parent %s is not in the request's trace", s.Name, s.SpanID, cur.ParentID)
		}
		if up, isLeg := strings.CutPrefix(parent.Name, catalog.SpanUpstreamPrefix); isLeg {
			return up, nil
		}
		cur = parent
	}
	return "", fmt.Errorf("span %s (%s): its parents loop", s.Name, s.SpanID)
}

// compareChain checks that the spans of one sampled request show the
// Filter Chain rows print: per Phase and leg the same Policies with their
// types in the printed order, no other filter span, and on the client leg
// the Phases in the printed order. It returns every difference.
func compareChain(rows []effectiveRow, trace []span, parentID string) error {
	server, err := serverSpan(trace, parentID)
	if err != nil {
		return err
	}
	spans, err := filterSpans(trace, server)
	if err != nil {
		return err
	}
	want := map[chainKey][]string{}
	types := map[string]string{}
	var order []chainKey
	for _, r := range rows {
		if r.Phase == phaseLog || r.Phase == phaseNone {
			continue
		}
		k := chainKey{phase: r.Phase, leg: r.Leg}
		if _, seen := want[k]; !seen {
			order = append(order, k)
		}
		want[k] = append(want[k], r.Policy)
		types[r.Policy] = r.Type
	}
	got := map[chainKey][]string{}
	var errs []error
	for _, s := range spans {
		got[s.key] = append(got[s.key], s.policy)
		if wantType, ok := types[s.policy]; ok && s.typ != wantType {
			errs = append(errs, fmt.Errorf("span %s%s at %s: type %q, want %q", catalog.SpanFilterPrefix, s.policy, s.key, s.typ, wantType))
		}
	}
	for _, k := range order {
		if !slices.Equal(got[k], want[k]) {
			errs = append(errs, fmt.Errorf("%s: spans %v, want %v", k, got[k], want[k]))
		}
	}
	for k, policies := range got {
		if _, ok := want[k]; !ok {
			errs = append(errs, fmt.Errorf("%s: spans %v, which the effective chain does not print", k, policies))
		}
	}
	// On the client leg every span of a Phase starts before any span of a
	// later Phase.
	var clientPhases []string
	for _, k := range order {
		if k.leg == legClient {
			clientPhases = append(clientPhases, k.phase)
		}
	}
	var last uint64
	lastPhase := ""
	for _, ph := range clientPhases {
		var first, end uint64
		for _, s := range spans {
			if s.key.leg != legClient || s.key.phase != ph {
				continue
			}
			if first == 0 || s.start < first {
				first = s.start
			}
			end = max(end, s.start)
		}
		if first != 0 && first < last {
			errs = append(errs, fmt.Errorf("client leg: %s starts before %s ended its spans", ph, lastPhase))
		}
		if end != 0 {
			last, lastPhase = end, ph
		}
	}
	return errors.Join(errs...)
}

// rowsFixture is the partner-orders chain as the CLI prints it.
func rowsFixture() []effectiveRow {
	return []effectiveRow{
		{Phase: "onRequestHeaders", Leg: legClient, Policy: "apikey-partner", Type: "auth.api-key"},
		{Phase: "onRequestHeaders", Leg: legClient, Policy: "ratelimit-global", Type: "ratelimit"},
		{Phase: "onRequestHeaders", Leg: legClient, Policy: "quota-partner", Type: "quota"},
		{Phase: "onUpstreamRequest", Leg: "orders", Policy: "upstream-oauth", Type: "auth.upstream-oauth2"},
		{Phase: "onUpstreamRequest", Leg: "orders", Policy: "headers-internal", Type: "headers"},
		{Phase: "onResponse", Leg: legClient, Policy: "headers-security", Type: "headers"},
		{Phase: phaseLog, Leg: legClient, Policy: "quota-partner", Type: "quota"},
		{Phase: phaseNone, Leg: phaseNone, Policy: "jwt-default", Type: "auth.jwt"},
	}
}

// traceFixture builds the spans a request running rowsFixture exports:
// the server span under the client's span "c1", a route match span, the
// client-leg filter spans under the server span and the upstream ones
// under the orders leg span, in start order.
func traceFixture() []span {
	var out []span
	add := func(id, parent, name string, kind spanKind, start uint64, attrs map[string]string) {
		out = append(out, span{TraceID: "t1", SpanID: id, ParentID: parent, Name: name, Kind: kind, Start: start, Attrs: attrs, Seq: len(out)})
	}
	filter := func(phase, typ string) map[string]string {
		return map[string]string{catalog.AttrPhase: phase, catalog.AttrPolicyType: typ}
	}
	add("s1", "c1", "GET partner-orders", kindServer, 1, nil)
	add("m1", "s1", catalog.SpanRouteMatch, kindInternal, 2, nil)
	add("f1", "s1", catalog.SpanFilterPrefix+"apikey-partner", kindInternal, 3, filter("onRequestHeaders", "auth.api-key"))
	add("f2", "s1", catalog.SpanFilterPrefix+"ratelimit-global", kindInternal, 4, filter("onRequestHeaders", "ratelimit"))
	add("f3", "s1", catalog.SpanFilterPrefix+"quota-partner", kindInternal, 5, filter("onRequestHeaders", "quota"))
	add("u1", "s1", catalog.SpanUpstreamPrefix+"orders", kindClient, 6, nil)
	add("f4", "u1", catalog.SpanFilterPrefix+"upstream-oauth", kindInternal, 7, filter("onUpstreamRequest", "auth.upstream-oauth2"))
	add("f5", "u1", catalog.SpanFilterPrefix+"headers-internal", kindInternal, 8, filter("onUpstreamRequest", "headers"))
	add("f6", "s1", catalog.SpanFilterPrefix+"headers-security", kindInternal, 9, filter("onResponse", "headers"))
	return out
}

func TestCompareChain(t *testing.T) {
	tests := []struct {
		name    string
		edit    func([]span) []span
		parent  string
		wantErr string
	}{
		{"the spans show the chain", func(s []span) []span { return s }, "c1", ""},
		{"spans written in another order", func(s []span) []span {
			slices.Reverse(s)
			return s
		}, "c1", ""},
		{"no server span under the client's span", func(s []span) []span { return s }, "other", "no server span"},
		{"a missing span", func(s []span) []span {
			return slices.DeleteFunc(s, func(x span) bool { return x.SpanID == "f2" })
		}, "c1", "onRequestHeaders/client: spans [apikey-partner quota-partner], want [apikey-partner ratelimit-global quota-partner]"},
		{"two Policies swapped", func(s []span) []span {
			s[2].Start, s[3].Start = s[3].Start, s[2].Start
			return s
		}, "c1", "want [apikey-partner ratelimit-global quota-partner]"},
		{"an upstream span on the client leg", func(s []span) []span {
			s[6].ParentID = "s1"
			return s
		}, "c1", "onUpstreamRequest/client"},
		{"a span the chain does not print", func(s []span) []span {
			return append(s, span{
				TraceID: "t1", SpanID: "x", ParentID: "s1", Name: catalog.SpanFilterPrefix + "cors-default", Start: 4,
				Attrs: map[string]string{catalog.AttrPhase: "onRequestHeaders", catalog.AttrPolicyType: "cors"}, Seq: 99,
			})
		}, "c1", "onRequestHeaders/client: spans [apikey-partner ratelimit-global cors-default quota-partner]"},
		{"a wrong Policy type", func(s []span) []span {
			s[7].Attrs = map[string]string{catalog.AttrPhase: "onUpstreamRequest", catalog.AttrPolicyType: "transform.request"}
			return s
		}, "c1", `type "transform.request", want "headers"`},
		{"a client Phase out of order", func(s []span) []span {
			s[8].Start = 2
			return s
		}, "c1", "client leg: onResponse starts before onRequestHeaders"},
		{"a span whose parent is missing", func(s []span) []span {
			s[6].ParentID = "gone"
			return s
		}, "c1", "is not in the request's trace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := compareChain(rowsFixture(), tt.edit(traceFixture()), tt.parent)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("compareChain = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("compareChain = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRows(t *testing.T) {
	rows := decodeRows(t, `[{"phase":"onRequestHeaders","leg":"client","policy":"jwt-default","type":"auth.jwt","filterClass":"auth","slot":"auth","from":"Gateway","reason":"slot auth, inherited"}]`)
	if len(rows) != 1 || rows[0].Policy != "jwt-default" || rows[0].Reason != "slot auth, inherited" {
		t.Errorf("decodeRows = %+v", rows)
	}
}
