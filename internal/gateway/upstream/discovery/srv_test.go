// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"cmp"
	"encoding/binary"
	"net"
	"slices"
	"strconv"
	"testing"
)

func srv(target string, port, prio, weight uint16) *net.SRV {
	return &net.SRV{Target: target, Port: port, Priority: prio, Weight: weight}
}

// TestSelectSRV covers the SRV rules of 05 req 6: lowest-priority group
// only, weight 0 excluded unless the whole group is 0 (then 1), identity
// target:port without the trailing dot, sorted by identity.
func TestSelectSRV(t *testing.T) {
	type want struct {
		identity string
		weight   uint32
	}
	tests := []struct {
		name string
		recs []*net.SRV
		want []want
	}{
		{"empty", nil, nil},
		{
			"lowest priority group only",
			[]*net.SRV{srv("b.svc.", 80, 10, 5), srv("a.svc.", 80, 20, 5), srv("c.svc.", 81, 10, 7)},
			[]want{{"b.svc:80", 5}, {"c.svc:81", 7}},
		},
		{
			"zero weight excluded when another weighs",
			[]*net.SRV{srv("a.svc.", 80, 1, 0), srv("b.svc.", 80, 1, 3)},
			[]want{{"b.svc:80", 3}},
		},
		{
			"all zero weights become one",
			[]*net.SRV{srv("b.svc.", 80, 1, 0), srv("a.svc.", 80, 1, 0), srv("z.svc.", 80, 2, 9)},
			[]want{{"a.svc:80", 1}, {"b.svc:80", 1}},
		},
		{
			"duplicates merge",
			[]*net.SRV{srv("a.svc.", 80, 1, 2), srv("a.svc", 80, 1, 3), srv("a.svc.", 81, 1, 1)},
			[]want{{"a.svc:80", 5}, {"a.svc:81", 1}},
		},
		{
			"unusable records ignored before the group is chosen",
			[]*net.SRV{nil, srv(".", 80, 0, 1), srv("a.svc.", 0, 0, 1), srv("bad name.", 80, 0, 1), srv("b.svc.", 80, 5, 1)},
			[]want{{"b.svc:80", 1}},
		},
		{
			"ip literal targets",
			[]*net.SRV{srv("10.0.0.2", 80, 0, 1), srv("2001:db8::1", 80, 0, 1)},
			[]want{{"10.0.0.2:80", 1}, {"[2001:db8::1]:80", 1}},
		},
		{"only unusable", []*net.SRV{srv(".", 80, 0, 1)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectSRV(tc.recs)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i].identity != w.identity || got[i].weight != w.weight {
					t.Fatalf("got[%d] = %+v, want %+v", i, got[i], w)
				}
			}
		})
	}
}

// FuzzSRVAnswer is the "SRV answer handling" fuzz target of 05 test 28:
// any answer yields a sorted, duplicate-free list of weighted targets from
// the lowest usable priority, and never panics.
func FuzzSRVAnswer(f *testing.F) {
	f.Add([]byte{0, 1, 0, 80, 0, 10, 0, 5, 1, 1, 0, 81, 0, 10, 0, 0})
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{})
	targets := []string{"a.svc.", "b.svc.", "a.svc", ".", "", "10.0.0.1", "bad name", "c.svc."}
	f.Fuzz(func(t *testing.T, data []byte) {
		var recs []*net.SRV
		for len(data) >= 8 {
			if data[0]%9 == 8 {
				recs = append(recs, nil)
			} else {
				recs = append(recs, &net.SRV{
					Target:   targets[int(data[1])%len(targets)],
					Port:     binary.BigEndian.Uint16(data[2:4]),
					Priority: uint16(data[4] % 4),
					Weight:   binary.BigEndian.Uint16(data[6:8]) % 4,
				})
			}
			data = data[8:]
		}
		got := selectSRV(recs)
		lowest := -1
		for _, r := range recs {
			if usableSRV(r) && (lowest < 0 || int(r.Priority) < lowest) {
				lowest = int(r.Priority)
			}
		}
		if (lowest < 0) != (len(got) == 0) {
			t.Fatalf("usable lowest %d but %d targets", lowest, len(got))
		}
		if !slices.IsSortedFunc(got, func(a, b srvTarget) int { return cmp.Compare(a.identity, b.identity) }) {
			t.Fatalf("not sorted: %+v", got)
		}
		for i, g := range got {
			if i > 0 && got[i-1].identity == g.identity {
				t.Fatalf("duplicate identity %q", g.identity)
			}
			if g.weight == 0 || g.port == 0 {
				t.Fatalf("zero weight or port: %+v", g)
			}
			if g.identity != net.JoinHostPort(g.host, strconv.Itoa(int(g.port))) {
				t.Fatalf("identity %q does not match host %q port %d", g.identity, g.host, g.port)
			}
			found := false
			for _, r := range recs {
				if usableSRV(r) && int(r.Priority) == lowest && r.Port == g.port && trimDot(r.Target) == g.host {
					found = true
				}
			}
			if !found {
				t.Fatalf("target %+v not in the lowest-priority group", g)
			}
		}
	})
}
