// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestKeyedIndex covers the keyed-list lookups in both of their forms: a
// short list is scanned, so is a long list an overlay list of at most
// linearScan entries merges into (at most linearScan scans), and a long
// list merged with a longer overlay list is indexed, and all give the
// same result. $patch: delete removes every base entry with the key (a
// base list may repeat one; stage F reports it), a merge goes to the
// first, entries without a usable key never match, and a list that grows
// past linearScan by appends is indexed with its base entries still
// found.
func TestKeyedIndex(t *testing.T) {
	listener := func(name, port string) string {
		return `{"name":` + name + `,"protocol":"http","port":` + port + `}`
	}
	gw := func(ls ...string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[` + strings.Join(ls, ",") + `]}}`
	}
	filler := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, listener(`"f`+strconv.Itoa(i)+`"`, "1"))
		}
		return out
	}
	// noops are deletes of absent keys, which change nothing and make the
	// overlay list long.
	noops := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, `{"name":"absent`+strconv.Itoa(i)+`","$patch":"delete"}`)
		}
		return out
	}
	for _, size := range []struct {
		name         string
		extra, noops int
	}{{"scanned", 0, 0}, {"long list scanned for a short overlay", linearScan + 1, 0}, {"indexed", linearScan + 1, linearScan + 1}} {
		pad := filler(size.extra)
		cases := []struct {
			name      string
			base, ovr []string
			want      []string
		}{
			{
				"delete removes every entry with the key",
				append([]string{listener(`"a"`, "1"), listener(`"b"`, "2"), listener(`"a"`, "3")}, pad...),
				[]string{`{"name":"a","$patch":"delete"}`},
				append([]string{listener(`"b"`, "2")}, pad...),
			},
			{
				"a merge goes to the first entry with the key",
				append([]string{listener(`"a"`, "1"), listener(`"a"`, "3")}, pad...),
				[]string{`{"name":"a","port":9}`},
				append([]string{listener(`"a"`, "9"), listener(`"a"`, "3")}, pad...),
			},
			{
				"entries without a usable key never match",
				append([]string{`{"protocol":"http","port":1}`, listener(`true`, "2"), listener(`"x"`, "3")}, pad...),
				[]string{`{"name":"x","$patch":"delete"}`, `{"name":"y","port":4}`},
				append(append([]string{`{"protocol":"http","port":1}`, listener(`true`, "2")}, pad...), `{"name":"y","port":4}`),
			},
			{
				"numbers and strings are different keys",
				append([]string{listener(`1`, "1"), listener(`"1"`, "2")}, pad...),
				[]string{`{"name":1,"$patch":"delete"}`},
				append([]string{listener(`"1"`, "2")}, pad...),
			},
		}
		for _, tc := range cases {
			t.Run(size.name+" "+tc.name, func(t *testing.T) {
				out, ds, _ := run(t, []file{{"ruralz.yaml", gw(tc.base...)}}, []file{{"o.yaml", gw(append(noops(size.noops), tc.ovr...)...)}})
				if len(ds) != 0 {
					t.Fatalf("diagnostics %q", texts(ds))
				}
				if g, w := dump(at(t, out[0].Root, "spec.listeners")), compact(t, "["+strings.Join(tc.want, ",")+"]"); g != w {
					t.Errorf("listeners =\n%s\nwant\n%s", g, w)
				}
			})
		}
	}
	t.Run("a list grown past linearScan still finds its base entries", func(t *testing.T) {
		base := []string{listener(`"a"`, "1"), listener(`"b"`, "2")}
		var ovr, want []string
		for i := range 2 * linearScan {
			ovr = append(ovr, `{"name":"n`+strconv.Itoa(i)+`","port":5}`)
		}
		ovr = append(ovr, `{"name":"b","port":7}`, `{"name":"a","$patch":"delete"}`)
		want = []string{listener(`"b"`, "7")}
		for i := range 2 * linearScan {
			want = append(want, `{"name":"n`+strconv.Itoa(i)+`","port":5}`)
		}
		out, ds, _ := run(t, []file{{"ruralz.yaml", gw(base...)}}, []file{{"o.yaml", gw(ovr...)}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics %q", texts(ds))
		}
		if g, w := dump(at(t, out[0].Root, "spec.listeners")), compact(t, "["+strings.Join(want, ",")+"]"); g != w {
			t.Errorf("listeners =\n%s\nwant\n%s", g, w)
		}
	})
	t.Run("a list indexed after a delete skips the removed entry", func(t *testing.T) {
		base := []string{listener(`"a"`, "1"), listener(`"b"`, "2")}
		ovr := []string{`{"name":"a","$patch":"delete"}`}
		want := []string{listener(`"b"`, "7")}
		for i := range 2 * linearScan {
			ovr = append(ovr, `{"name":"n`+strconv.Itoa(i)+`","port":5}`)
			want = append(want, `{"name":"n`+strconv.Itoa(i)+`","port":5}`)
		}
		ovr = append(ovr, `{"name":"b","port":7}`)
		out, ds, _ := run(t, []file{{"ruralz.yaml", gw(base...)}}, []file{{"o.yaml", gw(ovr...)}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics %q", texts(ds))
		}
		if g, w := dump(at(t, out[0].Root, "spec.listeners")), compact(t, "["+strings.Join(want, ",")+"]"); g != w {
			t.Errorf("listeners =\n%s\nwant\n%s", g, w)
		}
	})
	t.Run("a long object finds its members by key", func(t *testing.T) {
		var bl, ol []string
		for i := range 3 * linearScan {
			bl = append(bl, `"k`+strconv.Itoa(i)+`":"a"`)
		}
		ol = append(ol, `"k3":null`, `"k5":"b"`, `"new":"c"`, `"k0":"a"`)
		r := func(labels []string) string {
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{` + strings.Join(labels, ",") + `}},"spec":{}}`
		}
		out, ds, files := run(t, []file{{"ruralz.yaml", r(bl)}}, []file{{"o.yaml", r(ol)}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics %q", texts(ds))
		}
		labels := at(t, out[0].Root, "metadata.labels")
		var want []string
		for i := range 3 * linearScan {
			switch i {
			case 3:
			case 5:
				want = append(want, `"k5":"b"`)
			default:
				want = append(want, `"k`+strconv.Itoa(i)+`":"a"`)
			}
		}
		want = append(want, `"new":"c"`)
		if g, w := dump(labels), compact(t, "{"+strings.Join(want, ",")+"}"); g != w {
			t.Errorf("labels =\n%s\nwant\n%s", g, w)
		}
		if k0, _ := labels.Get("k0"); files.Location(k0.Pos).File != "ruralz.yaml" {
			t.Errorf("an equal overlay scalar replaced the base node: k0 at %v", files.Location(k0.Pos))
		}
		_ = tree.KindMap
	})
}
