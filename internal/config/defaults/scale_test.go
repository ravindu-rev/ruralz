// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// scaleCases build resources with one large object or list of n members:
// the shapes where a per-member Get, Select or ItemElem would make stage G
// quadratic (wave 3 note on tree.Node.Get and schemaidx scans), and where
// one encoding/json call over the whole value would keep a worker from
// yielding (01 req 53), typed, free (an open config, a plugin config, a
// Plugin configSchema, both any-valued) or a JSONSchemaDocument.
func scaleCases() map[string]func(n int) string {
	list := func(n int, item func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = item(n - i)
		}
		return strings.Join(parts, ", ")
	}
	return map[string]func(n int) string{
		"set of strings": func(n int) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {}, "tags": [` +
				list(n, func(i int) string { return `"T` + strconv.Itoa(i) + `"` }) + `]}}`
		},
		"map list": func(n int) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [` +
				list(n, func(i int) string { return `{"address": "h` + strconv.Itoa(i) + `:80"}` }) + `]}}`
		},
		"host set": func(n int) string {
			return route(`{"match": {"hosts": [` + list(n, func(i int) string { return `"H` + strconv.Itoa(i) + `.Example"` }) + `]}}`)
		},
		"labels": func(n int) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {` +
				list(n, func(i int) string { return `"k` + strconv.Itoa(i) + `": "v"` }) + `}}, "spec": {"match": {}}}`
		},
		"open config members": func(n int) string {
			return policyDoc("quota", `{"consumerQuota": "c", `+list(n, func(i int) string { return `"u` + strconv.Itoa(i) + `": 1` })+`}`)
		},
		"json schema document": func(n int) string {
			return policyDoc("validation.json-schema", `{"schema": {"properties": {`+
				list(n, func(i int) string {
					return `"p` + strconv.Itoa(i) + `": {"type": "integer", "maximum": ` + strconv.Itoa(i) + `}`
				})+`}}}`)
		},
		"atomic list of objects": func(n int) string {
			return policyDoc("ratelimit", `{"limits": [`+list(n, func(i int) string { return `{"requests": ` + strconv.Itoa(i) + `, "window": "60s"}` })+`]}`)
		},
		"plugin config": func(n int) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": {"x": [` +
				list(n, func(i int) string { return `{"a": ` + strconv.Itoa(i) + `, "b": ["s", true]}` }) + `]}}}`
		},
		"plugin config schema": func(n int) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"configSchema": {"type": "object", "properties": {` +
				list(n, func(i int) string {
					return `"p` + strconv.Itoa(i) + `": {"type": "integer", "enum": [` + strconv.Itoa(i) + `]}`
				}) + `}}}}`
		},
	}
}

// stageCost returns the bytes stage G allocates for src, parsed outside
// the count, on this goroutine, and the work units it counts.
func stageCost(t *testing.T, s *Stage, src string) (bytes uint64, units int) {
	t.Helper()
	res := envelope(mustParse(t, src))
	w := newWork()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	h, ds := s.resource(res, nil, w)
	runtime.ReadMemStats(&after)
	if h == nil || len(ds) != 0 {
		t.Fatalf("Resource: %q", texts(ds))
	}
	return after.TotalAlloc - before.TotalAlloc, w.total
}

// TestScaleLinear checks that stage G stays linear in the size of one
// large object or list, by two measures that do not depend on the machine
// or the race detector, for eight times the members:
//
//   - The work units the worker counts (nodes visited, sort comparisons
//     and what each lookup scans, lookup.go) grow about eight times (a
//     sort adds a logarithm). A per-member Get, Select or ItemElem on the
//     large value, which allocates nothing, would grow them about 64
//     times; TestLookupsCounted keeps every such lookup on the counter.
//   - The bytes allocated grow about eight times, where a stage that
//     built a path, an index or a copy once per member would allocate
//     about 64 times.
//
// BenchmarkScale measures the time of the same shapes.
func TestScaleLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	s := newStage(t)
	const small, factor = 1500, 8
	for name, build := range scaleCases() {
		t.Run(name, func(t *testing.T) {
			stageCost(t, s, build(small)) // warm the schema caches
			a, ua := stageCost(t, s, build(small))
			b, ub := stageCost(t, s, build(small*factor))
			for _, m := range []struct {
				what       string
				small, big uint64
			}{
				{"work units", uint64(ua), uint64(ub)}, //nolint:gosec // G115: unit counts are positive.
				{"bytes allocated", a, b},
			} {
				ratio := float64(m.big) / float64(max(m.small, 1))
				t.Logf("%s: %d members %d, %d members %d: ratio %.2f", m.what, small, m.small, small*factor, m.big, ratio)
				if ratio < factor/2 || ratio > 2*factor {
					t.Errorf("%s: %d members %d, %d members %d: ratio %.2f, want about %d", m.what, small, m.small, small*factor, m.big, ratio, factor)
				}
			}
		})
	}
}

// TestScaleResults checks the results of the large cases: sorted, lower
// cased and complete.
func TestScaleResults(t *testing.T) {
	s := newStage(t)
	const n = 3000
	h, ds := s.Resource(envelope(mustParse(t, scaleCases()["host set"](n))), nil)
	if h == nil || len(ds) != 0 {
		t.Fatalf("Resource: %q", texts(ds))
	}
	hosts := at(t, h.Tree, "spec.match.hosts")
	if len(hosts.Items) != n {
		t.Fatalf("hosts = %d, want %d", len(hosts.Items), n)
	}
	for i := 1; i < n; i++ {
		if hosts.Items[i-1].Text >= hosts.Items[i].Text || hosts.Items[i].Text != strings.ToLower(hosts.Items[i].Text) {
			t.Fatalf("hosts not sorted lower case at %d: %q %q", i, hosts.Items[i-1].Text, hosts.Items[i].Text)
		}
	}
	h, _ = s.Resource(envelope(mustParse(t, scaleCases()["map list"](n))), nil)
	eps := at(t, h.Tree, "spec.endpoints")
	for i, e := range eps.Items {
		if w := at(t, e, "weight"); w.Text != "1" || w.Style != tree.StyleDefaulted {
			t.Fatalf("endpoint %d weight = %s", i, dump(w))
		}
		if i > 0 && at(t, eps.Items[i-1], "address").Text >= at(t, e, "address").Text {
			t.Fatalf("endpoints not sorted at %d", i)
		}
	}
}

// errorScaleCases build resources with one large list element whose
// members each give stage G an RZ-CFG-005 (02 req 16 and 19): an open
// JWTIssuer, the entry of the map list issuers keyed by issuer, with n
// null or out-of-range members named to sort before the key member, so
// tree.Node.Get scans them all to name the entry. These are the shapes
// where naming the element once per diagnostic cost the diagnostics times
// the element: quadratic below the cap, 10,001 times the element above it
// (WP-37 review round 3).
func errorScaleCases() map[string]func(n int) string {
	issuer := func(n int, value string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(`, "A` + fmt.Sprintf("%07d", i) + `": ` + value)
		}
		return policyDoc("auth.jwt", `{"issuers": [{"issuer": "i", "jwksUrl": "https://x.example/j", "audiences": ["a"]`+b.String()+`}]}`)
	}
	return map[string]func(n int) string{
		"jwt issuer nulls":                 func(n int) string { return issuer(n, "null") },
		"jwt issuer integers out of range": func(n int) string { return issuer(n, "9007199254740993") },
	}
}

// failCost returns the bytes run allocates on this goroutine, the work
// units w counts and the diagnostics run returns, for a run that must
// fail with RZ-CFG-005 diagnostics only.
func failCost(t *testing.T, run func(w *work) (failed bool, ds diag.List)) (bytes uint64, units, diags int) {
	t.Helper()
	w := newWork()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	failed, ds := run(w)
	runtime.ReadMemStats(&after)
	if !failed || len(ds) == 0 {
		t.Fatalf("run passed: failed %v, %d diagnostics", failed, len(ds))
	}
	for _, d := range ds {
		if d.Code != CodeSchema {
			t.Fatalf("diagnostic %s, want only %s", d.AppendText(nil), CodeSchema)
		}
	}
	return after.TotalAlloc - before.TotalAlloc, w.total, len(ds)
}

// linearPair checks that eight times the members cost about eight times
// the work units and bytes, and at most perByteUnits units and
// perByteAlloc bytes per source byte. When the cap holds both sizes'
// diagnostics at MaxDiagnostics+1, the cost of the diagnostics stops
// growing, so only the upper bound of the ratio applies.
func linearPair(t *testing.T, small, big int, build func(n int) string, cost func(n int) (uint64, int, int)) {
	t.Helper()
	cost(small) // warm the schema caches
	a, ua, da := cost(small)
	b, ub, db := cost(big)
	keep := DefaultMaxDiagnostics + 1
	if da != min(small, keep) || db != min(big, keep) {
		t.Errorf("diagnostics: %d members %d, %d members %d; want %d and %d", small, da, big, db, min(small, keep), min(big, keep))
	}
	factor := float64(big) / float64(small)
	lower := factor / 2
	if db < big {
		lower = 1
	}
	for _, m := range []struct {
		what       string
		small, big uint64
	}{
		{"work units", uint64(ua), uint64(ub)}, //nolint:gosec // G115: unit counts are positive.
		{"bytes allocated", a, b},
	} {
		ratio := float64(m.big) / float64(max(m.small, 1))
		t.Logf("%s: %d members %d, %d members %d: ratio %.2f", m.what, small, m.small, big, m.big, ratio)
		if ratio < lower || ratio > 2*factor {
			t.Errorf("%s: %d members %d, %d members %d: ratio %.2f, want %.0f to %.0f", m.what, small, m.small, big, m.big, ratio, lower, 2*factor)
		}
	}
	for _, c := range []struct {
		n, units int
		bytes    uint64
	}{{small, ua, a}, {big, ub, b}} {
		src := len(build(c.n))
		if c.units > perByteUnits*src || c.bytes > uint64(perByteAlloc*src) {
			t.Errorf("%d members: %d work units and %d bytes for %d source bytes, want at most %d units and %d bytes per byte",
				c.n, c.units, c.bytes, src, perByteUnits, perByteAlloc)
		}
	}
}

// perByteUnits and perByteAlloc bound the work units and the bytes
// allocated of a failing resource per byte of its JSON source in
// TestScaleLinearErrors: the members' visit, sort and diagnostics are
// linear, a few units and under a hundred bytes per source byte; naming
// the element once per diagnostic cost hundreds of units, or for a set
// element its text, per byte.
const (
	perByteUnits = 8
	perByteAlloc = 256
)

// TestScaleLinearErrors is TestScaleLinear for inputs with diagnostics
// (WP-37 review round 3; 01 req 53 and 54, 02 req 78): one RZ-CFG-005 per
// member of a large list element costs about eight times the work units
// and bytes for eight times the members below the cap (1,000 and 8,000
// diagnostics), at most eight times above it (12,000 and 96,000 members,
// 10,001 diagnostics each), and a bounded number of units and bytes per
// source byte at every size, so a diagnostic costs the depth of its path,
// not the size of the element above it. The set twin names each diagnostic's set element by its RFC
// 8785 text, on a test schema, since every set of ruralz/v1alpha1 holds
// strings or closed objects.
func TestScaleLinearErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	s := newStage(t)
	pairs := [][2]int{{1000, 8000}, {12_000, 96_000}}
	for name, build := range errorScaleCases() {
		for _, p := range pairs {
			t.Run(name+"/"+strconv.Itoa(p[0]), func(t *testing.T) {
				cost := func(n int) (uint64, int, int) {
					res := envelope(mustParse(t, build(n)))
					return failCost(t, func(w *work) (bool, diag.List) {
						h, ds := s.resource(res, nil, w)
						return h == nil, ds
					})
				}
				linearPair(t, p[0], p[1], build, cost)
			})
		}
	}
	idx := setSchema(t)
	for _, p := range pairs {
		t.Run("set element nulls/"+strconv.Itoa(p[0]), func(t *testing.T) {
			cost := func(n int) (uint64, int, int) {
				res := envelope(mustParse(t, setElementNulls(n)))
				return failCost(t, func(w *work) (bool, diag.List) {
					ds, prior := normalize(idx, res, nil, w)
					return prior != nil, ds
				})
			}
			linearPair(t, p[0], p[1], setElementNulls, cost)
		})
	}
}

// setSchemaJSON is a rendered schema of one kind, Thing, whose spec.s is
// a set of open objects with a Duration d and a set of strings t: the set
// twin of the keyed JWTIssuer cases.
const setSchemaJSON = `{
 "$schema": "https://json-schema.org/draft/2020-12/schema",
 "type": "object",
 "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
 "required": ["apiVersion", "kind", "metadata", "spec"],
 "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
 "$defs": {
  "Thing": {"type": "object", "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"const": "Thing"},
   "metadata": {"type": "object"}, "spec": {"$ref": "#/$defs/ThingSpec"}}},
  "ThingSpec": {"type": "object", "properties": {"s": {"type": "array", "items": {"$ref": "#/$defs/Element"}, "x-ruralz-list": {"type": "set"}}}},
  "Element": {"type": "object", "properties": {"d": {"$ref": "#/$defs/Duration"},
   "t": {"type": "array", "items": {"type": "string"}, "x-ruralz-list": {"type": "set"}}}},
  "Duration": {"type": "string"}
 }
}`

// setSchema indexes setSchemaJSON.
func setSchema(t testing.TB) *schemaidx.Index {
	t.Helper()
	idx, err := schemaidx.Load("test/v1", []byte(setSchemaJSON))
	if err != nil {
		t.Fatalf("schemaidx.Load: %v", err)
	}
	return idx
}

// setElementNulls returns a Thing whose set spec.s holds one element of
// n null members.
func setElementNulls(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`, "A` + fmt.Sprintf("%07d", i) + `": null`)
	}
	return `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {}, "spec": {"s": [{"k": "v"` + b.String() + `}]}}`
}
