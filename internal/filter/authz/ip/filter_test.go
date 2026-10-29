// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ip

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu     sync.Mutex
	allows int
	denies []string
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allows++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denies = append(d.denies, code)
}

// build compiles cfg with decisions recorded in rec (nil records nothing).
func build(t *testing.T, cfg *v1alpha1.AuthzIPConfig, rec *decisions) *Filter {
	t.Helper()
	env := filter.BuildEnv{Config: cfg}
	if rec != nil {
		env.Metrics = &emit.PolicyMetrics{Auth: rec}
	}
	f, err := Factory{}.Build(t.Context(), env)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f.(*Filter)
}

// from returns an Exchange whose client address is addr.
func from(addr string) *filtertest.Exchange {
	return &filtertest.Exchange{SourceV: filter.Source{IP: netip.MustParseAddr(addr)}}
}

// TestDecisionRule checks the authz.ip rule: a deny match refuses;
// otherwise a non-empty allow list refuses every address outside it, and
// an empty one admits. A bare address means /32 or /128, and an
// IPv4-mapped IPv6 address, in the list or as the client, matches as
// IPv4.
func TestDecisionRule(t *testing.T) {
	tests := []struct {
		name        string
		allow, deny []string
		addr        string
		want        bool
	}{
		{"allow list admits a member", []string{"10.0.0.0/8"}, nil, "10.1.2.3", true},
		{"allow list refuses an outsider", []string{"10.0.0.0/8"}, nil, "192.0.2.1", false},
		{"deny list refuses a member", nil, []string{"192.0.2.0/24"}, "192.0.2.7", false},
		{"deny list alone admits an outsider", nil, []string{"192.0.2.0/24"}, "198.51.100.1", true},
		{"deny wins over allow", []string{"10.0.0.0/8"}, []string{"10.9.0.0/16"}, "10.9.1.1", false},
		{"allow and deny admit the rest of allow", []string{"10.0.0.0/8"}, []string{"10.9.0.0/16"}, "10.8.1.1", true},
		{"bare IPv4 address is /32", []string{"192.0.2.10"}, nil, "192.0.2.10", true},
		{"bare IPv4 address matches nothing else", []string{"192.0.2.10"}, nil, "192.0.2.11", false},
		{"bare IPv6 address is /128", []string{"2001:db8::1"}, nil, "2001:db8::1", true},
		{"bare IPv6 address matches nothing else", []string{"2001:db8::1"}, nil, "2001:db8::2", false},
		{"IPv6 prefix", []string{"2001:db8::/32"}, nil, "2001:db8:ffff::1", true},
		{"IPv4-mapped client matches IPv4 entry", []string{"10.0.0.0/8"}, nil, "::ffff:10.1.2.3", true},
		{"IPv4-mapped entry matches IPv4 client", []string{"::ffff:10.0.0.0/104"}, nil, "10.1.2.3", true},
		{"IPv4-mapped bare entry matches IPv4 client", nil, []string{"::ffff:192.0.2.1"}, "192.0.2.1", false},
		{"IPv6 deny does not reach IPv4 clients", nil, []string{"::/0"}, "192.0.2.1", true},
		{"IPv6 deny refuses IPv6 clients", nil, []string{"::/0"}, "2001:db8::1", false},
		{"host bits are masked", []string{"10.1.2.3/8"}, nil, "10.200.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			f := build(t, &v1alpha1.AuthzIPConfig{Allow: tt.allow, Deny: tt.deny}, rec)
			r := f.Handle(t.Context(), phase.OnRequestHeaders, from(tt.addr))
			got := r.Outcome == filter.Continue
			if got != tt.want {
				t.Fatalf("Handle(%s) admitted = %v, want %v (result %+v)", tt.addr, got, tt.want, r)
			}
			if tt.want && (rec.allows != 1 || len(rec.denies) != 0) {
				t.Errorf("admit recorded %d allows, denies %v; want one allow", rec.allows, rec.denies)
			}
			if !tt.want && (rec.allows != 0 || len(rec.denies) != 1 || rec.denies[0] != CodeDenied) {
				t.Errorf("refusal recorded %d allows, denies %v; want one deny %s", rec.allows, rec.denies, CodeDenied)
			}
		})
	}
}

// TestRefusalIs403 checks that a refused address is answered with a 403
// problem document carrying RZ-AUTH-013, with no extra header field and
// no detail ("RZ-AUTH decision codes").
func TestRefusalIs403(t *testing.T) {
	f := build(t, &v1alpha1.AuthzIPConfig{Deny: []string{"192.0.2.0/24"}}, nil)
	r := f.Handle(t.Context(), phase.OnRequestHeaders, from("192.0.2.1"))
	if r.Outcome != filter.Respond || r.Response == nil {
		t.Fatalf("Handle = %+v, want Respond", r)
	}
	resp := r.Response
	if resp.Status != 403 || resp.Code != CodeDenied || len(resp.Header) != 0 || resp.Body != nil || resp.Detail != "" {
		t.Errorf("response = %+v, want 403 %s without header, body or detail", resp, CodeDenied)
	}
	if got := errcode.Status(CodeDenied); got != 403 {
		t.Errorf("registered status of %s = %d, want 403", CodeDenied, got)
	}
}

// TestNoClientAddressFailsClosed checks that a request without a valid
// client address cannot be placed on either list, so the Policy cannot
// decide and fails closed with the authz class default, RZ-AUTH-015,
// whatever the lists hold.
func TestNoClientAddressFailsClosed(t *testing.T) {
	for _, cfg := range []*v1alpha1.AuthzIPConfig{
		{Allow: []string{"0.0.0.0/0", "::/0"}},
		{Deny: []string{"192.0.2.0/24"}},
	} {
		rec := &decisions{}
		f := build(t, cfg, rec)
		r := f.Handle(t.Context(), phase.OnRequestHeaders, &filtertest.Exchange{})
		if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || !errors.Is(r.Err, errNoAddress) {
			t.Errorf("Handle without address = %+v, want CannotDecide RZ-AUTH-015 with errNoAddress", r)
		}
		if rec.allows != 0 || len(rec.denies) != 1 || rec.denies[0] != "RZ-AUTH-015" {
			t.Errorf("recorded %d allows, denies %v; want one deny RZ-AUTH-015", rec.allows, rec.denies)
		}
	}
}

// TestOtherPhasesContinue checks that authz.ip decides only in
// onRequestHeaders, its one registered Phase, and records nothing in any
// other.
func TestOtherPhasesContinue(t *testing.T) {
	rec := &decisions{}
	f := build(t, &v1alpha1.AuthzIPConfig{Deny: []string{"0.0.0.0/0"}}, rec)
	for p := phase.OnRequestBody; p < phase.Count; p++ {
		if r := f.Handle(t.Context(), p, from("192.0.2.1")); r.Outcome != filter.Continue {
			t.Errorf("Handle(%s) = %+v, want Continue", p, r)
		}
	}
	if rec.allows != 0 || len(rec.denies) != 0 {
		t.Errorf("recorded %d allows, denies %v outside onRequestHeaders", rec.allows, rec.denies)
	}
}

// TestBuildRejectsInvalidConfig checks that Build refuses, with
// RZ-CFG-005, a config of the wrong type, two empty lists and an entry
// that is neither a CIDR nor an IP address (zones included), in either
// list.
func TestBuildRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  any
	}{
		{"wrong type", &v1alpha1.CORSConfig{}},
		{"nil config", (*v1alpha1.AuthzIPConfig)(nil)},
		{"both lists empty", &v1alpha1.AuthzIPConfig{}},
		{"malformed allow entry", &v1alpha1.AuthzIPConfig{Allow: []string{"10.0.0.0/8", "10.0.0.0/33"}}},
		{"malformed deny entry", &v1alpha1.AuthzIPConfig{Deny: []string{"not-an-address"}}},
		{"zone", &v1alpha1.AuthzIPConfig{Allow: []string{"fe80::1%eth0"}}},
		{"empty entry", &v1alpha1.AuthzIPConfig{Deny: []string{""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: tt.cfg})
			if f != nil || err == nil {
				t.Fatalf("Build = %v, %v; want an error", f, err)
			}
			if code, ok := errcode.CodeOf(err); !ok || code != CodeConfig {
				t.Errorf("Build error %v has code %q, want %s", err, code, CodeConfig)
			}
		})
	}
}

// TestBuildStopsOnEndedContext checks that a canceled snapshot compile
// stops Build with the context's error, which carries no RZ code.
func TestBuildStopsOnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Factory{}.Build(ctx, filter.BuildEnv{Config: &v1alpha1.AuthzIPConfig{Allow: []string{"10.0.0.0/8"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build = %v, want context.Canceled", err)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Errorf("Build error %v carries an RZ code", err)
	}
}

// TestAdmitAllocatesNothing checks the per-stage budget of authz.ip in
// onRequestHeaders: 0 Ruralz-owned allocations for an admitted request
// (docs/architecture/12-performance-budgets-and-benchmarking.md).
func TestAdmitAllocatesNothing(t *testing.T) {
	rec := &decisions{}
	f := build(t, &v1alpha1.AuthzIPConfig{Allow: []string{"10.0.0.0/8", "2001:db8::/32"}, Deny: []string{"10.9.0.0/16"}}, rec)
	ctx := t.Context()
	for _, addr := range []string{"10.1.2.3", "2001:db8::1", "::ffff:10.1.2.3"} {
		x := from(addr)
		if n := testing.AllocsPerRun(100, func() { f.Handle(ctx, phase.OnRequestHeaders, x) }); n != 0 {
			t.Errorf("Handle(%s) allocates %v times per run, want 0", addr, n)
		}
	}
}

// BenchmarkAdmit measures an admitted request against a 64-entry allow
// list (budget: 2 µs at p50, 0 allocations).
func BenchmarkAdmit(b *testing.B) {
	allow := make([]string, 0, 64)
	for i := range 64 {
		allow = append(allow, netip.AddrFrom4([4]byte{10, byte(i), 0, 0}).String()+"/16")
	}
	f, err := Factory{}.Build(b.Context(), filter.BuildEnv{Config: &v1alpha1.AuthzIPConfig{Allow: allow}})
	if err != nil {
		b.Fatal(err)
	}
	x := from("10.63.1.1")
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		f.Handle(ctx, phase.OnRequestHeaders, x)
	}
}
