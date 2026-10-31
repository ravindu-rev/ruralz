// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/bits"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for the State Store contract: the per-request deadline starting at
// the first blocking call (pack 8.7 rule 2), the stripe it carries,
// RouteMax, the five RZ-STS failures and their result labels, the op label
// mapping against catalog.Ops() and the Capability bits. Deps.MACKey is a
// field only; statestore/keys tests the HMAC it keys.

// t0 returns the fixed start time of the budget tests.
func t0() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

func TestCallTimeout(t *testing.T) {
	ctx := context.Background()
	b := NewRequestBudget(50*time.Millisecond, 3)

	// Starts at the first call, not at construction.
	d, ok := b.CallTimeout(ctx, t0(), 20*time.Millisecond)
	if !ok || d != 20*time.Millisecond {
		t.Fatalf("first call = %v, %v; want the Policy timeout 20ms", d, ok)
	}
	// The smaller of the Policy timeout and what is left of the budget.
	d, ok = b.CallTimeout(ctx, t0().Add(40*time.Millisecond), 20*time.Millisecond)
	if !ok || d != 10*time.Millisecond {
		t.Fatalf("second call = %v, %v; want the 10ms left", d, ok)
	}
	// Spent: RZ-STS-004.
	if d, ok = b.CallTimeout(ctx, t0().Add(50*time.Millisecond), 20*time.Millisecond); ok || d > 0 {
		t.Fatalf("spent budget = %v, %v; want not ok", d, ok)
	}
	if _, ok = b.CallTimeout(ctx, t0().Add(time.Second), 20*time.Millisecond); ok {
		t.Fatal("a later call restarted the budget")
	}
}

func TestCallTimeoutStartsLate(t *testing.T) {
	// A budget built at request start but first used 30ms later still
	// grants its whole length from the first call.
	b := NewRequestBudget(50*time.Millisecond, 0)
	late := t0().Add(30 * time.Millisecond)
	if d, ok := b.CallTimeout(context.Background(), late, time.Second); !ok || d != 50*time.Millisecond {
		t.Fatalf("CallTimeout = %v, %v; want 50ms from the first call", d, ok)
	}
	if d, ok := b.CallTimeout(context.Background(), late.Add(49*time.Millisecond), time.Second); !ok || d != time.Millisecond {
		t.Fatalf("CallTimeout = %v, %v; want 1ms", d, ok)
	}
}

func TestCallTimeoutContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), t0().Add(5*time.Millisecond))
	defer cancel()
	b := NewRequestBudget(50*time.Millisecond, 0)
	if d, ok := b.CallTimeout(ctx, t0(), 20*time.Millisecond); !ok || d != 5*time.Millisecond {
		t.Fatalf("CallTimeout = %v, %v; want the 5ms the context allows", d, ok)
	}
	if _, ok := b.CallTimeout(ctx, t0().Add(5*time.Millisecond), 20*time.Millisecond); ok {
		t.Fatal("CallTimeout past the context deadline succeeded")
	}
}

func TestCallTimeoutNoBudget(t *testing.T) {
	var zero RequestBudget
	if _, ok := zero.CallTimeout(context.Background(), t0(), time.Second); ok {
		t.Fatal("the zero budget (no State Store Policy) granted a call")
	}
	b := NewRequestBudget(time.Second, 0)
	for _, policy := range []time.Duration{0, -time.Millisecond} {
		if _, ok := b.CallTimeout(context.Background(), t0(), policy); ok {
			t.Fatalf("Policy timeout %v granted a call", policy)
		}
	}
}

func TestStripe(t *testing.T) {
	for _, s := range []emit.Stripe{0, 1, 7, 255} {
		b := NewRequestBudget(time.Second, s)
		if b.Stripe() != s {
			t.Fatalf("Stripe = %d, want %d", b.Stripe(), s)
		}
		b.CallTimeout(context.Background(), t0(), time.Millisecond)
		if b.Stripe() != s {
			t.Fatal("a call changed the stripe")
		}
	}
	var zero RequestBudget
	if zero.Stripe() != 0 {
		t.Fatal("zero budget stripe is not 0")
	}
}

func TestRouteMax(t *testing.T) {
	tests := []struct {
		in   []time.Duration
		want time.Duration
	}{
		{nil, 0},
		{[]time.Duration{50 * time.Millisecond}, 50 * time.Millisecond},
		{[]time.Duration{10 * time.Millisecond, 80 * time.Millisecond, 50 * time.Millisecond}, 80 * time.Millisecond},
		{[]time.Duration{-time.Second, 0}, 0},
	}
	for _, tt := range tests {
		if got := RouteMax(tt.in); got != tt.want {
			t.Errorf("RouteMax(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		err     *Error
		failure Failure
		code    string
		result  Result
		label   string
	}{
		{ErrTimeout, FailTimeout, "RZ-STS-001", ResultTimeout, "timeout"},
		{ErrFailed, FailError, "RZ-STS-002", ResultError, "error"},
		{ErrBreakerOpen, FailBreakerOpen, "RZ-STS-003", ResultSkipped, "skipped"},
		{ErrNotAttempted, FailNotAttempted, "RZ-STS-004", ResultSkipped, "skipped"},
		{ErrUnsupported, FailUnsupported, "RZ-STS-005", ResultSkipped, "skipped"},
	}
	results := func() []string {
		f, _ := catalog.Lookup(catalog.StateCallsTotal)
		return f.Labels[1].Values
	}()
	for i, tt := range tests {
		if tt.err.Failure() != tt.failure || tt.err.Code() != tt.code || tt.err.Result() != tt.result {
			t.Errorf("%s: failure %d, code %s, result %d", tt.code, tt.err.Failure(), tt.err.Code(), tt.err.Result())
		}
		if results[tt.err.Result()] != tt.label {
			t.Errorf("%s result label %q, want %q", tt.code, results[tt.err.Result()], tt.label)
		}
		if tt.err.Error() != tt.code+": state store call failed" {
			t.Errorf("Error = %q", tt.err.Error())
		}
		if _, ok := errcode.Lookup(tt.err.Code()); !ok {
			t.Errorf("%s is not registered", tt.err.Code())
		}
		// The sentinels are distinct values usable with errors.Is.
		for j, other := range tests {
			if got := errors.Is(tt.err, other.err); got != (i == j) {
				t.Errorf("errors.Is(%s, %s) = %v", tt.code, other.code, got)
			}
		}
		wrapped := errcode.Wrap(tt.err.Code(), tt.err)
		if !errors.Is(wrapped, tt.err) {
			t.Errorf("%s is lost through errcode.Wrap", tt.code)
		}
	}
	if (&Error{}).Code() != "RZ-STS-005" {
		t.Error("an unclassified failure is not RZ-STS-005")
	}
	// Result values are the emit.StateResult* indexes.
	if ResultOK != emit.StateResultOK || ResultError != emit.StateResultError ||
		ResultTimeout != emit.StateResultTimeout || ResultSkipped != emit.StateResultSkipped {
		t.Error("Result values are not the emit indexes")
	}
	if results[ResultOK] != "ok" {
		t.Errorf("ResultOK labels %q", results[ResultOK])
	}
}

func TestOpLabels(t *testing.T) {
	// catalog.Ops()[op.Label()] == op.String() for every OpKind.
	opsList := []OpKind{OpGCRA, OpQuota, OpCacheGet, OpCacheSet, OpCacheInvalidate, OpRefund}
	ops := catalog.Ops()
	for _, op := range opsList {
		l := op.Label()
		if l < 0 || l >= len(ops) || ops[l] != op.String() || op.String() == "" {
			t.Errorf("%q.Label() = %d (%q)", op.String(), l, at(ops, l))
		}
		// A single-op round trip carries the op's own label.
		if TripSingle.Label(op) != l {
			t.Errorf("TripSingle.Label(%s) = %d, want %d", op, TripSingle.Label(op), l)
		}
	}
	if ops[TripScriptMulti.Label(OpGCRA)] != "script_multi" || ops[TripPipeline.Label(OpCacheGet)] != "pipeline" {
		t.Errorf("trip labels %q, %q", ops[TripScriptMulti.Label(OpGCRA)], ops[TripPipeline.Label(OpCacheGet)])
	}
	// catalog.WriteKinds()[op.WriteLabel()] for the three write kinds.
	kinds := catalog.WriteKinds()
	for _, op := range []OpKind{OpRefund, OpCacheSet, OpCacheInvalidate} {
		w := op.WriteLabel()
		if w < 0 || w >= len(kinds) || kinds[w] != op.String() {
			t.Errorf("%s.WriteLabel() = %d (%q)", op, w, at(kinds, w))
		}
	}
	for _, op := range []OpKind{OpGCRA, OpQuota, OpCacheGet} {
		if op.WriteLabel() != -1 {
			t.Errorf("%s is not a post-commit write but WriteLabel = %d", op, op.WriteLabel())
		}
	}
	for _, unknown := range []OpKind{0, 99} {
		if unknown.String() != "" || unknown.Label() != -1 || unknown.WriteLabel() != -1 || TripSingle.Label(unknown) != -1 {
			t.Errorf("OpKind(%d) has a label", unknown)
		}
	}
}

func at(values []string, i int) string {
	if i < 0 || i >= len(values) {
		return "<out of range>"
	}
	return values[i]
}

func TestDigestOf(t *testing.T) {
	// Stable SHA-256 of the key value (keys and MAC derive from it).
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	d := DigestOf("abc")
	if hex.EncodeToString(d[:]) != want {
		t.Fatalf("DigestOf(abc) = %x", d)
	}
	if DigestOf("abc") != d || DigestOf("abd") == d {
		t.Fatal("DigestOf is not a stable function of its input")
	}
	if DigestOf("") != Digest(sha256.Sum256(nil)) {
		t.Fatal("DigestOf(\"\") is not SHA-256 of nothing")
	}
}

func TestDefaults(t *testing.T) {
	// stateStore.timeout defaults to 50ms.
	if DefaultTimeout != 50*time.Millisecond {
		t.Fatalf("DefaultTimeout = %v", DefaultTimeout)
	}
	l := DefaultLimits()
	if l.InFlight != 8192 || l.BreakerOpenMin >= l.BreakerOpenMax || l.ReconnectBase >= l.ReconnectCap ||
		l.MemoryPollFast >= l.MemoryPollSlow || l.BreakerFailureRatio <= 0 || l.BreakerFailureRatio >= 1 {
		t.Fatalf("DefaultLimits = %+v", l)
	}
	if DefaultLimits() != l {
		t.Fatal("DefaultLimits is not a pure function")
	}
}

// TestCapabilities: Store.Supports answers for one Capability at a time and
// a driver keeps its command sets as one mask (RZ-STS-005), so every
// Capability is exactly one bit and no two share it.
func TestCapabilities(t *testing.T) {
	caps := []Capability{CapScripts, CapVectorSets, CapValkeySearch}
	for i, a := range caps {
		if bits.OnesCount8(uint8(a)) != 1 {
			t.Errorf("capability %d is %08b, not a single bit", i, a)
		}
		for j, b := range caps {
			if i != j && a&b != 0 {
				t.Errorf("capabilities %d and %d share bits (%08b, %08b)", i, j, a, b)
			}
		}
	}
	// A mask holding every capability but one answers membership exactly.
	for i, missing := range caps {
		var mask Capability
		for _, c := range caps {
			if c != missing {
				mask |= c
			}
		}
		for j, c := range caps {
			if got, want := mask&c != 0, j != i; got != want {
				t.Errorf("mask without capability %d: has %d = %v, want %v", i, j, got, want)
			}
		}
	}
}

// TestCost covers the units a call charges and a refund returns: the
// value itself, 0 counting as 1, a negative value refused.
func TestCost(t *testing.T) {
	for _, tc := range []struct {
		in, want int64
		ok       bool
	}{
		{0, 1, true}, {1, 1, true}, {5, 5, true}, {1 << 62, 1 << 62, true}, {-1, 0, false},
	} {
		if got, ok := Cost(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("Cost(%d) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// rfcMillis parses an RFC 3339 time to Unix milliseconds.
func rfcMillis(t *testing.T, s string) int64 {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UnixMilli()
}

// TestQuotaWindowOf covers the window holding an instant: epoch-aligned
// fixed windows, before the epoch too, and UTC calendar months of 28 to
// 31 days, a leap February and a December included.
func TestQuotaWindowOf(t *testing.T) {
	for _, tc := range []struct {
		at         string
		window     time.Duration
		month      bool
		start, end string
	}{
		{"2026-09-26T12:34:56Z", 24 * time.Hour, false, "2026-09-26T00:00:00Z", "2026-09-27T00:00:00Z"},
		{"2026-09-26T12:00:00Z", time.Hour, false, "2026-09-26T12:00:00Z", "2026-09-26T13:00:00Z"},
		{"1969-12-31T23:59:59.999Z", time.Hour, false, "1969-12-31T23:00:00Z", "1970-01-01T00:00:00Z"},
		{"2026-02-14T08:00:00Z", 0, true, "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"2028-02-29T23:59:59.999Z", 0, true, "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"2026-04-30T23:59:59.999Z", time.Hour, true, "2026-04-01T00:00:00Z", "2026-05-01T00:00:00Z"},
		{"2026-12-31T23:00:00Z", 0, true, "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"2027-01-01T00:00:00Z", 0, true, "2027-01-01T00:00:00Z", "2027-02-01T00:00:00Z"},
	} {
		got := QuotaWindowOf(rfcMillis(t, tc.at), tc.window, tc.month)
		if got.Start != rfcMillis(t, tc.start) || got.End != rfcMillis(t, tc.end) {
			t.Errorf("QuotaWindowOf(%s, %v, %v) = [%v, %v), want [%s, %s)", tc.at, tc.window, tc.month,
				time.UnixMilli(got.Start).UTC(), time.UnixMilli(got.End).UTC(), tc.start, tc.end)
		}
	}
}

// TestQuotaCandidates covers the Node's two windows: its own and the one
// before it in its first half, the one after from its middle on, for
// fixed windows and for months, whose middle depends on their length.
func TestQuotaCandidates(t *testing.T) {
	for _, tc := range []struct {
		node   string
		window time.Duration
		month  bool
		a, b   string
	}{
		{"2026-09-26T12:29:59.999Z", time.Hour, false, "2026-09-26T12:00:00Z", "2026-09-26T11:00:00Z"},
		{"2026-09-26T12:30:00Z", time.Hour, false, "2026-09-26T12:00:00Z", "2026-09-26T13:00:00Z"},
		{"2026-02-14T23:59:59.999Z", 0, true, "2026-02-01T00:00:00Z", "2026-01-01T00:00:00Z"},
		{"2026-02-15T00:00:00Z", 0, true, "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"2028-02-15T11:59:59.999Z", 0, true, "2028-02-01T00:00:00Z", "2028-01-01T00:00:00Z"},
		{"2028-02-15T12:00:00Z", 0, true, "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"2026-12-31T00:00:00Z", 0, true, "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"2027-01-01T00:00:00Z", 0, true, "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z"},
	} {
		a, b := QuotaCandidates(rfcMillis(t, tc.node), tc.window, tc.month)
		if a.Start != rfcMillis(t, tc.a) || b.Start != rfcMillis(t, tc.b) {
			t.Errorf("QuotaCandidates(%s, %v, %v) = %v, %v; want starts %s, %s", tc.node, tc.window, tc.month,
				time.UnixMilli(a.Start).UTC(), time.UnixMilli(b.Start).UTC(), tc.a, tc.b)
		}
		if a.End != b.Start && b.End != a.Start {
			t.Errorf("QuotaCandidates(%s): %v and %v are not adjacent", tc.node, a, b)
		}
	}
}

// TestZoneLabel checks the comparison behind the zone label of
// ruralz_state_call_duration_seconds: same when the server's zone equals
// the Node's RURALZ_ZONE, cross when they differ, unknown when either is
// missing; each result is the index of its catalog.Zones() value.
func TestZoneLabel(t *testing.T) {
	for _, tc := range []struct {
		node, server string
		want         string
	}{
		{"eu-west-1a", "eu-west-1a", "same"},
		{"eu-west-1a", "eu-west-1b", "cross"},
		{"", "eu-west-1a", "unknown"},
		{"eu-west-1a", "", "unknown"},
		{"", "", "unknown"},
	} {
		got := ZoneLabel(tc.node, tc.server)
		if zones := catalog.Zones(); got < 0 || got >= len(zones) || zones[got] != tc.want {
			t.Errorf("ZoneLabel(%q, %q) = %d, want the index of %q in %v", tc.node, tc.server, got, tc.want, zones)
		}
	}
	if emit.NumStateZones != len(catalog.Zones()) {
		t.Errorf("emit.NumStateZones = %d, catalog.Zones() has %d", emit.NumStateZones, len(catalog.Zones()))
	}
}
