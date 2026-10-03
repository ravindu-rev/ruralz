// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"errors"
	"net/netip"
	"testing"
)

// FuzzFetchAllow is the spec 06 section 6.4 target for RURALZ_FETCH_ALLOW
// (requirement 85). Oracle: no panic; an error wraps ErrAllowEntry and
// stays short; an accepted value round-trips through String and never
// changes the default deny table for addresses outside its prefixes.
func FuzzFetchAllow(f *testing.F) {
	for _, s := range []string{
		"",
		"127.0.0.1",
		"127.0.0.0/8,::1",
		"env-proxy",
		"10.0.0.0/8, 192.168.0.0/16 ,env-proxy",
		"::ffff:127.0.0.0/104",
		"fe80::1%eth0",
		"127.0.0.1,,",
		"0.0.0.0/0,::/0",
		"64:ff9b::/96",
		"1.2.3.4/33",
	} {
		f.Add(s)
	}
	probes := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("::1"),
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("64:ff9b::7f00:1"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("2001:db8::1"),
	}
	f.Fuzz(func(t *testing.T, v string) {
		g, err := ParseAllow(v)
		if err != nil {
			if !errors.Is(err, ErrAllowEntry) {
				t.Fatalf("error %v does not wrap ErrAllowEntry", err)
			}
			if len(err.Error()) > 512 {
				t.Fatalf("error message of %d bytes", len(err.Error()))
			}
			return
		}
		again, err := ParseAllow(g.String())
		if err != nil {
			t.Fatalf("String() %q does not parse: %v", g.String(), err)
		}
		if again.String() != g.String() || again.EnvProxy() != g.EnvProxy() {
			t.Fatalf("round trip %q -> %q", g.String(), again.String())
		}
		for _, a := range probes {
			if g.Allowed(a) != again.Allowed(a) {
				t.Fatalf("round trip changed the decision for %s", a)
			}
			if !Denied(a) && !g.Allowed(a) {
				t.Fatalf("allow entries denied %s, which the default table admits", a)
			}
			if Denied(a) && g.Allowed(a) && !g.inAllow(a.Unmap()) {
				t.Fatalf("%s admitted without an allow entry", a)
			}
		}
	})
}
