// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestNoCascade covers the one-failure-one-diagnostic rule of Normalize:
// the set duplicate check skips the elements the visit reported, so two
// nulls in a set give no third diagnostic "duplicate element null" and two
// equal strings that are not UTF-8 give none whose message holds the raw
// bytes, and the keyed-list checks skip a reported entry or key likewise.
// For every kind of failure an element or entry can have, two equal
// failing elements of a set, or two failing entries or keys of a map
// list, give exactly one diagnostic each, in authored and canonical
// content, with messages that are valid UTF-8; a duplicate that
// normalization reveals is still reported beside a failing element.
func TestNoCascade(t *testing.T) {
	hosts := route(`{"match": {"hosts": ["a.example", "b.example", "c.example"]}}`)
	upstreams := route(`{"match": {}, "upstreams": [{"name": "a"}, {"name": "b"}]}`)
	subjects := policyDoc("auth.mtls", `{"caCertificate": {"secretRef": {"provider": "env", "name": "X"}}, "subjects": [{"subject": "CN=a"}, {"subject": "CN=a"}]}`)
	// each edits the nodes at the two paths alike.
	each := func(edit func(n *tree.Node)) func(t *testing.T, root *tree.Node, paths []string) {
		return func(t *testing.T, root *tree.Node, paths []string) {
			for _, p := range paths {
				edit(at(t, root, p))
			}
		}
	}
	tests := []struct {
		name  string
		src   string
		paths []string
		edit  func(t *testing.T, root *tree.Node, paths []string)
		// want is the message start of each diagnostic, in order.
		want []string
	}{
		{"set: two nulls", route(`{"match": {"hosts": ["a.example", null, null]}}`), nil, nil, []string{"null is not allowed", "null is not allowed"}},
		{
			"set: two nil nodes", hosts, nil,
			func(t *testing.T, root *tree.Node, _ []string) {
				l := at(t, root, "spec.match.hosts")
				l.Items[1], l.Items[2] = nil, nil
			},
			[]string{"null is not allowed", "null is not allowed"},
		},
		{
			"set: two equal strings that are not UTF-8", hosts,
			[]string{"spec.match.hosts[0]", "spec.match.hosts[1]"},
			each(func(n *tree.Node) { n.Text = notUTF8 }),
			[]string{"string is not valid UTF-8", "string is not valid UTF-8"},
		},
		{
			"set: two equal numbers out of range", hosts,
			[]string{"spec.match.hosts[0]", "spec.match.hosts[1]"},
			each(func(n *tree.Node) { n.Kind, n.Text = tree.KindFloat, "1e400" }),
			[]string{"number 1e400 is outside the range", "number 1e400 is outside the range"},
		},
		{
			"set: two elements of no JSON kind", hosts,
			[]string{"spec.match.hosts[0]", "spec.match.hosts[1]"},
			each(func(n *tree.Node) { n.Kind = tree.Kind(99) }),
			[]string{"node kind 99 has no JSON value", "node kind 99 has no JSON value"},
		},
		{
			"set: two equal objects holding a null", policyDoc("auth.mtls", `{"caCertificate": {"secretRef": {"provider": "env", "name": "X"}}, "subjects": [{"subject": null}, {"subject": null}]}`),
			nil, nil,
			[]string{"null is not allowed", "null is not allowed"},
		},
		{
			"set: two equal objects holding text that is not UTF-8", subjects,
			[]string{"spec.config.subjects[0].subject", "spec.config.subjects[1].subject"},
			each(func(n *tree.Node) { n.Text = notUTF8 }),
			[]string{"string is not valid UTF-8", "string is not valid UTF-8"},
		},
		{
			"set: two equal objects holding a member name that is not UTF-8", subjects,
			[]string{"spec.config.subjects[0]", "spec.config.subjects[1]"},
			each(func(n *tree.Node) { n.Members[0].Key = notUTF8 }),
			[]string{"member name is not valid UTF-8", "member name is not valid UTF-8"},
		},
		{
			"set: a revealed duplicate beside a null", route(`{"match": {"hosts": [null, "A.example", "a.example"]}}`), nil, nil,
			[]string{"null is not allowed", `duplicate element "a.example"`},
		},
		{
			"map list: two equal keys that are not UTF-8", upstreams,
			[]string{"spec.upstreams[0].name", "spec.upstreams[1].name"},
			each(func(n *tree.Node) { n.Text = notUTF8 }),
			[]string{"string is not valid UTF-8", "string is not valid UTF-8"},
		},
		{
			"map list: two keys of no JSON kind", upstreams,
			[]string{"spec.upstreams[0].name", "spec.upstreams[1].name"},
			each(func(n *tree.Node) { n.Kind = tree.Kind(99) }),
			[]string{"node kind 99 has no JSON value", "node kind 99 has no JSON value"},
		},
		{
			"map list: two entries of no JSON kind", upstreams,
			[]string{"spec.upstreams[0]", "spec.upstreams[1]"},
			each(func(n *tree.Node) { n.Kind = tree.Kind(99) }),
			[]string{"node kind 99 has no JSON value", "node kind 99 has no JSON value"},
		},
		{
			"map list: two string entries that are not UTF-8", upstreams,
			[]string{"spec.upstreams[0]", "spec.upstreams[1]"},
			each(func(n *tree.Node) { *n = tree.Node{Kind: tree.KindString, Text: notUTF8, Pos: n.Pos} }),
			[]string{"string is not valid UTF-8", "string is not valid UTF-8"},
		},
		{
			"map list: two equal keys out of range", upstreams,
			[]string{"spec.upstreams[0].name", "spec.upstreams[1].name"},
			each(func(n *tree.Node) { n.Kind, n.Text = tree.KindFloat, "1e400" }),
			[]string{"number 1e400 is outside the range", "number 1e400 is outside the range"},
		},
		{"map list: two null entries", route(`{"match": {}, "upstreams": [null, null]}`), nil, nil, []string{"null is not allowed", "null is not allowed"}},
	}
	for _, tc := range tests {
		for _, role := range roles() {
			// Canonical content is exempt from the number range rule,
			// so 1e400 is valid there.
			if strings.Contains(tc.name, "out of range") && role.role == tree.RoleCanonical {
				continue
			}
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				if tc.edit != nil {
					tc.edit(t, res.Root, tc.paths)
				}
				h, ds := newStage(t).Resource(res, f.files)
				var got []string
				for _, d := range ds {
					got = append(got, d.Message)
					if !utf8.ValidString(d.Message) {
						t.Errorf("message is not UTF-8: %q", d.Message)
					}
				}
				ok := h == nil && len(got) == len(tc.want)
				for i := range got {
					ok = ok && strings.HasPrefix(got[i], tc.want[i])
				}
				if !ok {
					t.Errorf("Resource = %v,\n%q\nwant one diagnostic each, starting\n%q", h != nil, texts(ds), tc.want)
				}
			})
		}
	}
}

// TestShownJSON covers the set duplicate's rendering of an element: its
// clipped RFC 8785 text, or a placeholder below a secret field and for
// text that is not UTF-8, so a message never holds raw bytes.
func TestShownJSON(t *testing.T) {
	z := &normalizer{res: &tree.Resource{}, w: newWork()}
	z.stack = append(z.stack, step{})
	long := `"` + strings.Repeat("a", 100) + `"`
	for _, tc := range []struct{ canon, want string }{
		{`"a.example"`, `"a.example"`},
		{`{"subject":"CN=a"}`, `{"subject":"CN=a"}`},
		{long, clip(long)},
		{`"caf` + "\xe9" + `"`, "value"},
		{long + "\xff", "value"},
	} {
		if got := z.shownJSON([]byte(tc.canon)); got != tc.want || !utf8.ValidString(got) {
			t.Errorf("shownJSON(%q) = %q, want %q", tc.canon, got, tc.want)
		}
	}
	secret := gateway(`{"stateStore": {"driver": "redis", "url": {"secretRef": {"provider": "env", "name": "x"}}}}`)
	idx := hubIndex(t)
	res := envelope(mustParse(t, secret))
	sel, _ := idx.Resource("Gateway")
	path := []string{"spec", "stateStore", "url"}
	n := res.Root
	z.stack = stack{{node: n, schema: sel}}
	for _, name := range path {
		n, _ = n.Get(name)
		sel = memberSchema(sel.Select(nullMembers(z.stack[len(z.stack)-1].node)), name)
		z.stack = append(z.stack, step{node: n, schema: sel, name: name})
	}
	if !slices.ContainsFunc(z.stack, func(st step) bool { return st.schema.Keywords().Secret }) {
		t.Fatal("no secret field on the path")
	}
	if got := z.shownJSON([]byte(`"a"`)); got != "value" {
		t.Errorf("shownJSON below a secret field = %q, want the placeholder", got)
	}
}
