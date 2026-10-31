// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// spanDocs are resources whose stage F work is dominated by one large
// part: valid listeners, listeners with a type error each, unknown
// members, a Policy dispatched with many members, a set of strings, a
// typed map of type errors, and one long string and one long member name.
func spanDocs(n int) map[string]string {
	listeners := func(port string) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`{"name": "l` + strconv.Itoa(i) + `", "protocol": "http", "port": ` + port + `}`)
		}
		return envelope("Gateway", `{"listeners": [`+b.String()+`]}`)
	}
	var hosts strings.Builder
	for i := range n {
		if i > 0 {
			hosts.WriteString(", ")
		}
		hosts.WriteString(`"h` + strconv.Itoa(i) + `.example"`)
	}
	long := strings.Repeat("a", 1<<20)
	return map[string]string{
		"valid listeners":   listeners("80"),
		"listener types":    listeners(`"x"`),
		"unknown members":   largeRoute(n),
		"policy dispatch":   largePolicy(n),
		"set of strings":    envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": [`+hosts.String()+`]}]}`),
		"typed-map errors":  largeLabels(n),
		"long string":       `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "annotations": {"a": "` + long + `"}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
		"long member name":  envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+long+`": 1}`),
		"long quoted names": envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+strings.Repeat("a ", 1<<19)+`": 1, "`+strings.Repeat("a ", 1<<19)+`b": 2}`),
	}
}

// widest returns the largest member count of an object at or below n and
// the longest text stage F reads as one piece there: a member name, a
// scalar's text or a list element's canonical JSON.
func widest(n *tree.Node) (members, text int) {
	if n == nil {
		return 0, 0
	}
	members, text = len(n.Members), len(n.Text)
	for _, m := range n.Members {
		mm, mt := widest(m.Value)
		members, text = max(members, mm), max(text, mt, len(m.Key))
	}
	for _, it := range n.Items {
		mm, mt := widest(it)
		members, text = max(members, mm), max(text, mt, len(schemaidx.AppendItemJSON(nil, it)))
	}
	return members, text
}

// TestLongestSpan bounds the work stage F does without a yield check, by
// counting work units, never the clock. A check is due every 256 nodes
// inside a unit; stage F checks at every unit it charges, but some spans
// of its work cannot be interrupted (Options.Yield) and are charged at
// once after they finish. The longest is the jsonschema/v6 Validate call,
// one span over the whole instance: its charge must be exactly the
// instance's nodes plus the bytes of its member names and strings, at
// most the resource's nodes plus its bytes (the budget the Options.Yield
// doc states). Every other span must stay within one object's members,
// three times over (schemaidx.Walk dispatches an object twice and scans a
// keyed entry for its key), plus one text, escaped and with its numbers:
// a dispatch, a path segment, a canonical JSON or a comparison. Between
// two Yield calls stage F charges at most yieldEvery-1 units plus one
// span.
func TestLongestSpan(t *testing.T) {
	v := testView(t)
	for name, src := range spanDocs(20_000) {
		t.Run(name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", src)
			var charges []int
			since, maxGap, yields := 0, 0, 0
			m := newMapper(v, res, files, MaxDiagnostics, Options{Yield: func() {
				maxGap = max(maxGap, since)
				since = 0
				yields++
			}})
			m.work.observe = func(k int) {
				charges = append(charges, k)
				since += k
			}
			m.run()
			_ = m.result()
			nodes := countNodes(res.Root)
			keys, strs := textBytes(res.Root)
			validator := nodes + keys + strs
			i := slices.Index(charges, slices.Max(charges))
			if longest := charges[i]; longest != validator {
				t.Errorf("longest span charged %d units, want the validator's %d (%d nodes, %d member-name and %d string bytes)", longest, validator, nodes, keys, strs)
			}
			if validator > nodes+len(src) {
				t.Errorf("validator span %d units, want at most %d nodes plus %d document bytes", validator, nodes, len(src))
			}
			others := slices.Delete(slices.Clone(charges), i, i+1)
			members, text := widest(res.Root)
			if budget := 3*members + 1 + 8*text; len(others) > 0 && slices.Max(others) > budget {
				t.Errorf("another span charged %d units, want at most %d (%d members, a %d-byte text)", slices.Max(others), budget, members, text)
			}
			if maxGap > yieldEvery-1+charges[i] {
				t.Errorf("%d units between two Yield calls, want at most %d", maxGap, yieldEvery-1+charges[i])
			}
			if yields == 0 {
				t.Error("no Yield call")
			}
			t.Logf("%d nodes, %d bytes: validator span %d units, next longest %d, %d yields", nodes, len(src), validator, slices.Max(others), yields)
		})
	}
}
