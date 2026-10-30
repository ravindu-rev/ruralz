// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
)

// TestLoopbackHosts checks the hosts of RURALZ_BIND_LOOPBACK
// (docs/architecture/03-data-plane.md "Listeners and protocols"):
// 127.0.0.1 always, and ::1 exactly where a socket can listen on it.
func TestLoopbackHosts(t *testing.T) {
	failing := func(context.Context) error { return errors.New("address family not supported") }
	if got := loopbackHosts(t.Context(), failing); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("loopbackHosts without IPv6 = %v, want [127.0.0.1]", got)
	}
	ok := func(context.Context) error { return nil }
	if got := loopbackHosts(t.Context(), ok); !slices.Equal(got, []string{"127.0.0.1", "::1"}) {
		t.Errorf("loopbackHosts with IPv6 = %v, want [127.0.0.1 ::1]", got)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp6", "[::1]:0")
	hasIPv6 := err == nil
	if hasIPv6 {
		_ = ln.Close()
	}
	got := LoopbackHosts(t.Context())
	if want := 1 + btoi(hasIPv6); len(got) != want || got[0] != LoopbackIPv4 || (hasIPv6 && got[1] != LoopbackIPv6) {
		t.Errorf("LoopbackHosts = %v on a host where ::1 listens: %t", got, hasIPv6)
	}
}

// btoi returns 1 for true.
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestOptionsHosts checks Options.Hosts: each host an IP literal without a
// zone, named once; none binds every interface.
func TestOptionsHosts(t *testing.T) {
	base := Options{Handler: http.NotFoundHandler(), Conns: admission.NewConnLimiter(1)}
	for _, tt := range []struct {
		name  string
		hosts []string
		ok    bool
	}{
		{name: "none", ok: true},
		{name: "loopback pair", hosts: []string{"127.0.0.1", "::1"}, ok: true},
		{name: "host name", hosts: []string{"localhost"}},
		{name: "empty host", hosts: []string{""}},
		{name: "zone", hosts: []string{"fe80::1%lo"}},
		{name: "duplicate", hosts: []string{"127.0.0.1", "127.0.0.1"}},
		{name: "duplicate in another form", hosts: []string{"::1", "0:0:0:0:0:0:0:1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := base
			o.Hosts = tt.hosts
			s, err := New(o)
			if tt.ok {
				if err != nil {
					t.Fatalf("New(Hosts %q) = %v", tt.hosts, err)
				}
				_ = s.Close()
				return
			}
			if !errors.Is(err, ErrOptions) {
				t.Fatalf("New(Hosts %q) = %v, want ErrOptions", tt.hosts, err)
			}
		})
	}
}
