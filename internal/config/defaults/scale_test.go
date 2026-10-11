// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// scaleCases build resources with one large object or list of n members:
// the shapes where a per-member Get, Select or ItemElem would make stage G
// quadratic (wave 3 note on tree.Node.Get and schemaidx scans).
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
	}
}

// stageAlloc returns the bytes stage G allocates for src, parsed outside
// the count, on this goroutine.
func stageAlloc(t *testing.T, s *Stage, src string) uint64 {
	t.Helper()
	res := envelope(mustParse(t, src))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	h, ds := s.Resource(res, nil)
	runtime.ReadMemStats(&after)
	if h == nil || len(ds) != 0 {
		t.Fatalf("Resource: %q", texts(ds))
	}
	return after.TotalAlloc - before.TotalAlloc
}

// TestScaleLinear checks that stage G stays linear in the size of one
// large object or list. It counts the bytes allocated, not the time, so it
// does not depend on the machine or the race detector: eight times the
// members allocate about eight times the bytes, where a stage that built
// a path, an index or a copy once per member would allocate about 64
// times. BenchmarkScale measures the time of the same shapes.
func TestScaleLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	s := newStage(t)
	const small, factor = 1500, 8
	for name, build := range scaleCases() {
		t.Run(name, func(t *testing.T) {
			stageAlloc(t, s, build(small)) // warm the schema caches
			a := stageAlloc(t, s, build(small))
			b := stageAlloc(t, s, build(small*factor))
			ratio := float64(b) / float64(max(a, 1))
			t.Logf("%d members allocated %d bytes, %d members %d: ratio %.2f", small, a, small*factor, b, ratio)
			if ratio < factor/2 || ratio > 2*factor {
				t.Errorf("%d members allocated %d bytes, %d members %d: ratio %.2f, want about %d", small, a, small*factor, b, ratio, factor)
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
