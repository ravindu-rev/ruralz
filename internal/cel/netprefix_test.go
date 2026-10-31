// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for the int result of config.cost and for net.prefix
// (docs/architecture/02-configuration-model.md "Allowed places" and
// "Limits").

// TestEvalInt evaluates int places: a typed int, and a dyn result that is
// an int at runtime.
func TestEvalInt(t *testing.T) {
	c := newCompiler(t)
	for _, tc := range []struct {
		site expr.Site
		src  string
		want int64
	}{
		{at(expr.PlaceRateLimitCost), `1`, 1},
		{at(expr.PlaceRateLimitCost), `request.method == "POST" ? 5 : 1`, 1},
		{at(expr.PlaceRateLimitCost), `size(request.path)`, 10},
		{at(expr.PlaceRateLimitCost), `dyn(3)`, 3},
		{at(expr.PlaceRateLimitCost), `-2`, -2},
	} {
		got, err := mustProgram(t, c, tc.site, tc.src).EvalInt(t.Context(), testVars())
		if err != nil || got != tc.want {
			t.Errorf("EvalInt(%q) = %d, %v; want %d", tc.src, got, err, tc.want)
		}
	}
}

// TestEvalIntResultType checks that a dyn result of another type fails at
// runtime with KindResultType, and that EvalBool and EvalString refuse an
// int result.
func TestEvalIntResultType(t *testing.T) {
	c := newCompiler(t)
	for _, src := range []string{`dyn("x")`, `auth.claims.scope`, `dyn(1u)`} {
		_, err := mustProgram(t, c, at(expr.PlaceRateLimitCost), src).EvalInt(t.Context(), testVars())
		var ee *expr.EvalError
		if !errors.As(err, &ee) || ee.Kind != expr.KindResultType || ee.Place != expr.PlaceRateLimitCost {
			t.Errorf("EvalInt(%q) = %v, want KindResultType at the cost place", src, err)
		}
	}
	prg := mustProgram(t, c, at(expr.PlaceTransformRequestBody), `1`)
	if _, err := prg.EvalString(t.Context(), testVars()); err == nil {
		t.Error("EvalString of an int result succeeded")
	}
}

// TestIntPlaceRefusesOtherTypes checks the static rule: an int place takes
// int or dyn, so a string, bool or uint expression is RZ-CFG-014 with the
// conversion hint.
func TestIntPlaceRefusesOtherTypes(t *testing.T) {
	c := newCompiler(t)
	for _, tc := range []struct{ src, typ string }{
		{`"1"`, "string"},
		{`true`, "bool"},
		{`1u`, "uint"},
		{`1.5`, "double"},
	} {
		iss := c.Check(at(expr.PlaceRateLimitCost), tc.src)
		want := "CEL ratelimit config.cost: expression has type " + tc.typ + ", want int"
		if len(iss) != 1 || iss[0].Code != "RZ-CFG-014" || iss[0].Message != want || iss[0].Hint != "convert it with int(...)" {
			t.Errorf("Check(%q) = %+v, want one RZ-CFG-014 %q", tc.src, iss, want)
		}
	}
	for _, src := range []string{`1`, `int(request.headers["x-cost"])`, `auth.claims.cost`} {
		if iss := c.Check(at(expr.PlaceRateLimitCost), src); len(iss) != 0 {
			t.Errorf("Check(%q) = %+v, want none", src, iss)
		}
	}
	// limits[].when is a bool place over the base variables.
	if iss := c.Check(at(expr.PlaceRateLimitWhen), `consumer != null && consumer.tier == "gold"`); len(iss) != 0 {
		t.Errorf("Check of a limits[].when = %+v, want none", iss)
	}
	if iss := c.Check(at(expr.PlaceRateLimitWhen), `response.status == 200`); len(iss) == 0 {
		t.Error("limits[].when accepted response, which onRequestHeaders does not have")
	}
}

// TestNetPrefix checks net.prefix on IPv4, IPv4-mapped and IPv6 addresses
// at the lengths and edges the configuration model gives.
func TestNetPrefix(t *testing.T) {
	c := newCompiler(t)
	for _, tc := range []struct{ src, want string }{
		{`net.prefix("192.0.2.1", 32, 64)`, "192.0.2.1/32"},
		{`net.prefix("192.0.2.77", 24, 64)`, "192.0.2.0/24"},
		{`net.prefix("192.0.2.77", 0, 64)`, "0.0.0.0/0"},
		{`net.prefix("::ffff:192.0.2.77", 24, 64)`, "192.0.2.0/24"},
		{`net.prefix("2001:db8:1:2:3:4:5:6", 32, 64)`, "2001:db8:1:2::/64"},
		{`net.prefix("2001:db8:1:2:3:4:5:6", 32, 128)`, "2001:db8:1:2:3:4:5:6/128"},
		{`net.prefix("2001:db8:1:2:3:4:5:6", 32, 0)`, "::/0"},
		{`net.prefix("2001:DB8::1", 32, 48)`, "2001:db8::/48"},
		{`net.prefix(source.ip, 32, 64)`, "192.0.2.1/32"},
	} {
		got, err := mustProgram(t, c, at(expr.PlaceRateLimitKey), tc.src).EvalString(t.Context(), testVars())
		if err != nil || got != tc.want {
			t.Errorf("EvalString(%q) = %q, %v; want %q", tc.src, got, err, tc.want)
		}
	}
	v := testVars()
	v.Source.IP = netip.MustParseAddr("2001:db8:1:2:aaaa::9")
	got, err := mustProgram(t, c, at(expr.PlaceRateLimitKey), `net.prefix(source.ip, 32, 64)`).EvalString(t.Context(), v)
	if err != nil || got != "2001:db8:1:2::/64" {
		t.Errorf("net.prefix of an IPv6 client = %q, %v; want its /64", got, err)
	}
}

// TestNetPrefixErrors checks that an address that does not parse, or a
// length outside 0 to 32 or 0 to 128, is a runtime error whose text never
// holds the argument.
func TestNetPrefixErrors(t *testing.T) {
	c := newCompiler(t)
	for _, src := range []string{
		`net.prefix("not-an-address", 32, 64)`,
		`net.prefix("", 32, 64)`,
		`net.prefix("192.0.2.1/24", 32, 64)`,
		`net.prefix("192.0.2.1", 33, 64)`,
		`net.prefix("192.0.2.1", -1, 64)`,
		`net.prefix("2001:db8::1", 32, 129)`,
		`net.prefix("2001:db8::1", 32, -1)`,
	} {
		_, err := mustProgram(t, c, at(expr.PlaceRateLimitKey), src).EvalString(t.Context(), testVars())
		var ee *expr.EvalError
		if !errors.As(err, &ee) {
			t.Errorf("EvalString(%q) = %v, want an EvalError", src, err)
			continue
		}
		if strings.Contains(err.Error(), "192.0.2.1") || strings.Contains(err.Error(), "not-an-address") {
			t.Errorf("EvalString(%q) error %q holds the argument", src, err)
		}
	}
	// Out-of-range lengths are refused only for the address's own family.
	got, err := mustProgram(t, c, at(expr.PlaceRateLimitKey), `net.prefix("192.0.2.1", 32, 200)`).EvalString(t.Context(), testVars())
	if err != nil || got != "192.0.2.1/32" {
		t.Errorf("IPv4 with an unused IPv6 length out of range = %q, %v; want 192.0.2.1/32", got, err)
	}
}

// TestNetPrefixInEveryPlace checks that net.prefix compiles in every CEL
// place, with the result type each place takes.
func TestNetPrefixInEveryPlace(t *testing.T) {
	c := newCompiler(t)
	const call = `net.prefix("192.0.2.1", 24, 64)`
	for _, p := range expr.Places() {
		src := call
		switch p.Result {
		case expr.ResultBool:
			src = call + ` == "192.0.2.0/24"`
		case expr.ResultInt:
			src = `size(` + call + `)`
		case expr.ResultString, expr.ResultDyn:
		}
		site := at(p.ID)
		if p.ID == expr.PlaceStepPathExpression || p.ID == expr.PlaceStepWhen {
			site = sequential(p.ID)
		}
		if iss := c.Check(site, src); len(iss) != 0 {
			t.Errorf("Check(%s, %q) = %+v, want none", p.ID, src, iss)
		}
	}
}

// TestNetPrefixEstimate checks the static estimate: a call costs 1 plus
// 0.1 unit per input byte, and its result is sized at its input plus 4
// bytes, so concatenating two results of a 256-byte nominal address costs
// ceil(0.1 × 2 × 260) = 52.
func TestNetPrefixEstimate(t *testing.T) {
	c := newCompiler(t)
	cost := func(src string) uint64 {
		t.Helper()
		return mustProgram(t, c, at(expr.PlaceRateLimitKey), src).Cost().Max
	}
	ip := cost(`source.ip`)
	one := cost(`net.prefix(source.ip, 32, 64)`)
	if got := one - ip; got != 27 {
		t.Errorf("net.prefix call over a 256-byte address costs %d, want 1 + 26 = 27", got)
	}
	two := cost(`net.prefix(source.ip, 32, 64) + net.prefix(source.ip, 32, 64)`)
	if got := two - 2*one; got != 52 {
		t.Errorf("concatenation of two results costs %d, want 52 from results of 260 bytes", got)
	}
}

// TestNetPrefixRuntimeCost checks the runtime charge: 1 plus 0.1 unit per
// byte of the address, rounded up.
func TestNetPrefixRuntimeCost(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want uint64
	}{
		{"", 1},
		{"192.0.2.1", 2},
		{"2001:db8:1:2:3:4:5:6", 3},
		{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", 5},
	} {
		got := trackNetPrefix([]ref.Val{types.String(tc.addr), types.Int(32), types.Int(64)}, nil)
		if got == nil || *got != tc.want {
			t.Errorf("runtime cost of net.prefix(%q) = %v, want %d", tc.addr, got, tc.want)
		}
	}
	// A tracked program charges the call: under the cost limit it
	// evaluates.
	c := newCompiler(t)
	prg := mustProgram(t, c, at(expr.PlaceRateLimitKey), `net.prefix(source.ip, 32, 64)`)
	if !prg.tracked {
		t.Fatal("net.prefix over an unbounded source.ip is untracked")
	}
	if got, err := prg.EvalString(t.Context(), testVars()); err != nil || got != "192.0.2.1/32" {
		t.Errorf("tracked net.prefix = %q, %v", got, err)
	}
}
