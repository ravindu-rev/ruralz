// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/check"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/geoiptest"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestDecisionRule checks the authz.geoip rule of Security and identity
// "IP filtering and GeoIP" for IPv4 and IPv6 clients: a deny match
// refuses; otherwise a non-empty allow list refuses every address outside
// it, and an empty one admits; an address the database cannot place (no
// record, a record without a country, a private, loopback or unknown
// address) matches neither list; an IPv4-mapped IPv6 address is placed as
// IPv4.
func TestDecisionRule(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	tests := []struct {
		name        string
		allow, deny []string
		addr        string
		want        bool
	}{
		{"allow admits a member, IPv4", []string{"GB", "SE"}, nil, addrGB, true},
		{"allow admits another member, IPv4", []string{"GB", "SE"}, nil, addrSE, true},
		{"allow admits a member, IPv6", []string{"DE"}, nil, addrDE6, true},
		{"allow refuses an outsider, IPv4", []string{"GB", "SE"}, nil, addrUS, false},
		{"allow refuses an outsider, IPv6", []string{"GB", "SE"}, nil, addrJP6, false},
		{"allow refuses a private address", []string{"GB"}, nil, addrPrivate, false},
		{"allow refuses a private IPv6 address", []string{"GB"}, nil, addrPrivate6, false},
		{"allow refuses loopback", []string{"GB"}, nil, addrLoopback, false},
		{"allow refuses an unknown address", []string{"GB"}, nil, addrUnknown, false},
		{"allow refuses a record without a country", []string{"GB"}, nil, addrNoCode, false},
		{"deny refuses a member, IPv4", nil, []string{"CN", "JP"}, addrCN, false},
		{"deny refuses a member, IPv6", nil, []string{"CN", "JP"}, addrJP6, false},
		{"deny alone admits an outsider", nil, []string{"CN", "JP"}, addrGB, true},
		{"deny alone admits a private address", nil, []string{"CN"}, addrPrivate, true},
		{"deny alone admits loopback", nil, []string{"CN"}, addrLoopback, true},
		{"deny alone admits an unknown address", nil, []string{"CN"}, addrUnknown, true},
		{"deny alone admits a record without a country", nil, []string{"CN"}, addrNoCode, true},
		{"deny wins over allow", []string{"GB", "CN"}, []string{"CN"}, addrCN, false},
		{"allow and deny admit the rest of allow", []string{"GB", "CN", "DE"}, []string{"CN"}, addrDE6, true},
		{"allow and deny refuse outside allow", []string{"GB", "CN"}, []string{"CN"}, addrUS, false},
		{"empty allow beside deny admits", []string{}, []string{"CN"}, addrUS, true},
		{"IPv4-mapped client placed as IPv4", []string{"GB"}, nil, "::ffff:" + addrGB, true},
		{"IPv4-mapped client denied as IPv4", nil, []string{"GB"}, "::ffff:" + addrGB, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			f := build(t, fx.db, codes(tt.allow...), codes(tt.deny...), rec)
			r := f.Handle(t.Context(), phase.OnRequestHeaders, from(tt.addr))
			if got := r.Outcome == filter.Continue; got != tt.want {
				t.Fatalf("Handle(%s) admitted = %v, want %v (result %+v)", tt.addr, got, tt.want, r)
			}
			if tt.want && (rec.allows != 1 || len(rec.denies) != 0) {
				t.Errorf("admit recorded %d allows, denies %v; want one allow", rec.allows, rec.denies)
			}
			if !tt.want && (r.Outcome != filter.Respond || rec.allows != 0 || len(rec.denies) != 1 || rec.denies[0] != CodeDenied) {
				t.Errorf("refusal %+v recorded %d allows, denies %v; want Respond and one deny %s", r, rec.allows, rec.denies, CodeDenied)
			}
		})
	}
}

// TestIPv4OnlyDatabase: an ip_version 4 database places IPv4 clients and
// no IPv6 one, which then matches neither list.
func TestIPv4OnlyDatabase(t *testing.T) {
	fx := newFixture(t, dbBytes(t, geoiptest.New(geoiptest.Options{IPVersion: 4}).
		Insert("81.2.69.0/24", geoiptest.Country("GB")).Insert("216.160.83.0/24", geoiptest.Country("US"))))
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Errorf("place(%s) = %q, want GB", addrGB, got)
	}
	if got := fx.placed(t, "::ffff:"+addrUS); got != "US" {
		t.Errorf("place(IPv4-mapped %s) = %q, want US", addrUS, got)
	}
	if got := fx.placed(t, addrJP6); got != "" {
		t.Errorf("place(%s) = %q in an IPv4-only database, want no country", addrJP6, got)
	}
	deny := build(t, fx.db, nil, codes("GB"), nil)
	if r := deny.Handle(t.Context(), phase.OnRequestHeaders, from(addrJP6)); r.Outcome != filter.Continue {
		t.Errorf("deny GB, an IPv6 client: %+v, want Continue", r)
	}
	allow := build(t, fx.db, codes("JP"), nil, nil)
	if r := allow.Handle(t.Context(), phase.OnRequestHeaders, from(addrJP6)); r.Outcome != filter.Respond {
		t.Errorf("allow JP, an IPv6 client the database cannot place: %+v, want Respond", r)
	}
}

// TestClientAddressSource checks that the Filter places source.ip, the
// client address after trusted-proxy and PROXY v2 handling
// (filter.Source.IP), never the TCP peer: a request whose peer is a
// trusted proxy in an allowed country but whose client address is in a
// denied one is refused, and the reverse is admitted.
func TestClientAddressSource(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	f := build(t, fx.db, codes("GB"), nil, nil)
	viaProxy := func(client, peer string) *filtertest.Exchange {
		return &filtertest.Exchange{SourceV: filter.Source{
			IP: netip.MustParseAddr(client), Peer: netip.AddrPortFrom(netip.MustParseAddr(peer), 443), FromHeader: true,
		}}
	}
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, viaProxy(addrUS, addrGB)); r.Outcome != filter.Respond {
		t.Errorf("a US client behind a GB proxy: %+v, want Respond", r)
	}
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, viaProxy(addrGB, addrUS)); r.Outcome != filter.Continue {
		t.Errorf("a GB client behind a US proxy: %+v, want Continue", r)
	}
}

// TestRefusalIs403 checks that a refused country is answered with a 403
// problem document carrying RZ-AUTH-014, with no extra header field and
// no detail ("RZ-AUTH decision codes").
func TestRefusalIs403(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	f := build(t, fx.db, nil, codes("CN"), nil)
	r := f.Handle(t.Context(), phase.OnRequestHeaders, from(addrCN))
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

// TestCannotDecideFailsClosed checks the three requests a Policy cannot
// decide, which fail closed with the authz class default, RZ-AUTH-015,
// whatever the lists hold: one without a valid client address, one on a
// Node without a database (the setting unset, or no read succeeded), and
// one whose lookup fails.
func TestCannotDecideFailsClosed(t *testing.T) {
	good := newFixture(t, worldBytes(t, geoiptest.Options{}))
	unset := NewDatabase(DatabaseOptions{Clock: good.clk})
	unreadable := newFixture(t, nil)
	tests := []struct {
		name string
		db   *Database
		x    *filtertest.Exchange
		want error
	}{
		{"no client address", good.db, &filtertest.Exchange{}, errNoAddress},
		{"setting unset", unset, from(addrGB), errNoDatabase},
		{"no read succeeded", unreadable.db, from(addrGB), errNoDatabase},
		{"no Database at all", nil, from(addrGB), errNoDatabase},
	}
	for _, tt := range tests {
		for _, lists := range [][2][]v1alpha1.CountryCode{{codes("GB"), nil}, {nil, codes("CN")}} {
			t.Run(tt.name, func(t *testing.T) {
				rec := &decisions{}
				f := build(t, tt.db, lists[0], lists[1], rec)
				r := f.Handle(t.Context(), phase.OnRequestHeaders, tt.x)
				if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || !errors.Is(r.Err, tt.want) {
					t.Errorf("Handle = %+v, want CannotDecide RZ-AUTH-015 with %v", r, tt.want)
				}
				if rec.allows != 0 || len(rec.denies) != 1 || rec.denies[0] != "RZ-AUTH-015" {
					t.Errorf("recorded %d allows, denies %v; want one deny RZ-AUTH-015", rec.allows, rec.denies)
				}
			})
		}
	}
	t.Run("lookup fails", func(t *testing.T) {
		// A Reader closed under a published database fails every lookup,
		// a stand-in for a fault the load did not see.
		fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
		f := build(t, fx.db, codes("GB"), nil, nil)
		if err := fx.db.current().reader.Close(); err != nil {
			t.Fatal(err)
		}
		r := f.Handle(t.Context(), phase.OnRequestHeaders, from(addrGB))
		if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || r.Err == nil || !strings.HasPrefix(r.Err.Error(), "authz.geoip: ") {
			t.Errorf("Handle = %+v, want CannotDecide RZ-AUTH-015 with the lookup error", r)
		}
	})
}

// TestOtherPhasesContinue checks that authz.geoip decides only in
// onRequestHeaders, its one registered Phase, and records nothing in any
// other.
func TestOtherPhasesContinue(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	rec := &decisions{}
	f := build(t, fx.db, nil, codes("CN"), rec)
	for p := phase.OnRequestBody; p < phase.Count; p++ {
		if r := f.Handle(t.Context(), p, from(addrCN)); r.Outcome != filter.Continue {
			t.Errorf("Handle(%s) = %+v, want Continue", p, r)
		}
	}
	if rec.allows != 0 || len(rec.denies) != 0 {
		t.Errorf("recorded %d allows, denies %v outside onRequestHeaders", rec.allows, rec.denies)
	}
}

// TestBuildRejectsInvalidConfig checks that Build refuses, with
// RZ-CFG-005, a config of the wrong type, two empty lists and a code that
// is not two upper-case letters, in either list, so a Node refuses what
// validation refuses (the schema's CountryCode pattern, check.Empty).
func TestBuildRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  any
		msg  string
	}{
		{"wrong type", &v1alpha1.AuthzIPConfig{}, "config is"},
		{"nil config", (*v1alpha1.AuthzGeoIPConfig)(nil), "config is"},
		{"both lists absent", &v1alpha1.AuthzGeoIPConfig{}, check.MessageEmpty},
		{"both lists empty", &v1alpha1.AuthzGeoIPConfig{Allow: codes(), Deny: codes()}, check.MessageEmpty},
		{"lower-case allow code", &v1alpha1.AuthzGeoIPConfig{Allow: codes("GB", "de")}, `config.allow entry "de"`},
		{"three-letter deny code", &v1alpha1.AuthzGeoIPConfig{Deny: codes("DEU")}, `config.deny entry "DEU"`},
		{"one letter", &v1alpha1.AuthzGeoIPConfig{Deny: codes("D")}, `config.deny entry "D"`},
		{"empty code", &v1alpha1.AuthzGeoIPConfig{Allow: codes("")}, `config.allow entry ""`},
		{"digits", &v1alpha1.AuthzGeoIPConfig{Allow: codes("1A")}, `config.allow entry "1A"`},
		{"non-ASCII", &v1alpha1.AuthzGeoIPConfig{Allow: codes("Ä")}, "config.allow entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newFixture(t, worldBytes(t, geoiptest.Options{})).db
			f, err := Factory{Database: db}.Build(t.Context(), filter.BuildEnv{Config: tt.cfg})
			if f != nil || err == nil {
				t.Fatalf("Build = %v, %v; want an error", f, err)
			}
			if code, ok := errcode.CodeOf(err); !ok || code != CodeConfig {
				t.Errorf("Build error %v has code %q, want %s", err, code, CodeConfig)
			}
			if !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("Build error %q, want it to name %q", err, tt.msg)
			}
			if db.users != 0 {
				t.Errorf("a refused Build counts %d live users", db.users)
			}
		})
	}
}

// TestBuildRefusesWhatCheckReports: the offline check and Build apply one
// rule to the lists, so every config Check reports Build refuses with the
// same code, and a config Check passes Build accepts.
func TestBuildRefusesWhatCheckReports(t *testing.T) {
	for _, cfg := range []*v1alpha1.AuthzGeoIPConfig{
		{},
		{Allow: codes()},
		{Allow: codes(), Deny: codes()},
		{Allow: codes("GB")},
		{Deny: codes("CN")},
		{Allow: codes(), Deny: codes("CN")},
	} {
		var found []diag.Diagnostic
		check.Check(&hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "geo"}, Config: cfg}, nil,
			func(d diag.Diagnostic) { found = append(found, d) })
		f, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: cfg})
		if f != nil {
			_ = f.(*Filter).Close()
		}
		code, _ := errcode.CodeOf(err)
		switch {
		case len(found) > 0 && (err == nil || code != found[0].Code):
			t.Errorf("config %+v: Check reports %s, Build = %v", cfg, found[0].Code, err)
		case len(found) == 0 && err != nil:
			t.Errorf("config %+v: Check passes, Build = %v", cfg, err)
		}
	}
}

// TestBuildStopsOnEndedContext checks that a canceled snapshot compile
// stops Build with the context's error, which carries no RZ code.
func TestBuildStopsOnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Factory{}.Build(ctx, filter.BuildEnv{Config: &v1alpha1.AuthzGeoIPConfig{Allow: codes("GB")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build = %v, want context.Canceled", err)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Errorf("Build error %v carries an RZ code", err)
	}
}

// TestAdmitAllocatesNothing checks that authz.geoip allocates nothing for
// an admitted request in onRequestHeaders, IPv4, IPv6 or IPv4-mapped, and
// for an address the database cannot place.
func TestAdmitAllocatesNothing(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	rec := &decisions{}
	f := build(t, fx.db, nil, codes("CN", "JP"), rec)
	ctx := t.Context()
	for _, addr := range []string{addrGB, addrDE6, "::ffff:" + addrGB, addrPrivate, addrNoCode} {
		x := from(addr)
		if n := testing.AllocsPerRun(100, func() { f.Handle(ctx, phase.OnRequestHeaders, x) }); n != 0 {
			t.Errorf("Handle(%s) allocates %v times per run, want 0", addr, n)
		}
	}
}

// TestConcurrentRequestsDuringReloads: requests decide concurrently while
// the database is replaced under them, each on the database it read, and
// never on a mix (the race detector checks the sharing).
func TestConcurrentRequestsDuringReloads(t *testing.T) {
	gb := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB")))
	cn := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN")))
	fx := newFixture(t, gb)
	f := build(t, fx.db, codes("GB", "CN"), nil, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			x := from(addrGB)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if r := f.Handle(t.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
					t.Errorf("Handle during a reload = %+v, want Continue", r)
					return
				}
			}
		})
	}
	for i := range 20 {
		data := gb
		if i%2 == 0 {
			data = cn
		}
		replace(t, fx.path, data, t0.Add(-time.Duration(i+2)*time.Hour))
		fx.poll(t)
	}
	close(stop)
	wg.Wait()
}
