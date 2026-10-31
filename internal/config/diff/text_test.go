// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// header returns the first line of d's human form.
func header(d *Diff) string {
	line, _, _ := strings.Cut(string(d.AppendText(nil)), "\n")
	return line
}

// sides is the JSON form's description of the sides.
type sides struct {
	From        map[string]string `json:"from"`
	To          map[string]string `json:"to"`
	Environment *string           `json:"environment"`
}

// TestEnvironmentsAndOrigins: the header and the JSON sides name the one
// Environment either side names, or each side's when they differ (a
// promotion review), and what each side was read from.
func TestEnvironmentsAndOrigins(t *testing.T) {
	from, to := render(t, shop(t, "from")), render(t, shop(t, "to"))
	f, o := from.Revision.Digest, to.Revision.Digest
	cases := []struct {
		name       string
		a, b       Side
		header     string
		env        string
		fromJ, toJ map[string]string
	}{
		{
			"one Environment",
			Side{State: from, Environment: "prod"},
			Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: "prod"},
			"(environment: prod)", "prod",
			map[string]string{"revision": f.Short(), "digest": f.String()},
			map[string]string{"revision": o.Short(), "bundle": "./shop-bundle", "digest": o.String()},
		},
		{
			"promotion",
			Side{State: from, Origin: OriginBundle, Location: "./shop-bundle", Environment: "staging"},
			Side{State: to, Origin: OriginBundle, Location: "./shop-bundle", Environment: "prod"},
			"(environment: staging -> prod)", "",
			map[string]string{"revision": f.Short(), "bundle": "./shop-bundle", "digest": f.String(), "environment": "staging"},
			map[string]string{"revision": o.Short(), "bundle": "./shop-bundle", "digest": o.String(), "environment": "prod"},
		},
		{
			"drift against a Node",
			Side{State: from, Origin: OriginBundle, Location: "./shop-bundle", Environment: "prod"},
			Side{State: to, Origin: OriginAdmin, Location: "https://node-a.shop.example:9901"},
			"(environment: prod)", "prod",
			map[string]string{"revision": f.Short(), "bundle": "./shop-bundle", "digest": f.String()},
			map[string]string{"revision": o.Short(), "admin": "https://node-a.shop.example:9901", "digest": o.String()},
		},
		{
			"saved dumps",
			Side{State: from, Origin: OriginDump, Location: "node-a.json"},
			Side{State: to, Origin: OriginDump, Location: "node-b.json"},
			"", "",
			map[string]string{"revision": f.Short(), "dump": "node-a.json", "digest": f.String()},
			map[string]string{"revision": o.Short(), "dump": "node-b.json", "digest": o.String()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := engine(t, 1).Compare(context.Background(), tc.a, tc.b)
			if err != nil {
				t.Fatal(err)
			}
			want := "ruralz bundle diff: " + f.Short() + " -> " + o.Short()
			if tc.header != "" {
				want += " " + tc.header
			}
			if got := header(d); got != want {
				t.Errorf("header = %q, want %q", got, want)
			}
			var doc sides
			if err := json.Unmarshal(d.AppendJSON(nil), &doc); err != nil {
				t.Fatal(err)
			}
			if got := ""; doc.Environment != nil {
				got = *doc.Environment
				if got != tc.env || got == "" {
					t.Errorf("environment = %q, want %q", got, tc.env)
				}
			} else if tc.env != "" {
				t.Errorf("environment missing, want %q", tc.env)
			}
			if !mapsEqual(doc.From, tc.fromJ) || !mapsEqual(doc.To, tc.toJ) {
				t.Errorf("sides = %v, %v; want %v, %v", doc.From, doc.To, tc.fromJ, tc.toJ)
			}
		})
	}
}

// mapsEqual compares two string maps.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestJSONMemberOrder: the JSON members come in the order the format
// lists them, and the document ends with a newline.
func TestJSONMemberOrder(t *testing.T) {
	d := compare(t, render(t, shop(t, "from")), render(t, shop(t, "to")))
	js := d.AppendJSON(nil)
	order := []string{`"format"`, `"from"`, `"to"`, `"resources"`, `"kind"`, `"name"`, `"change"`, `"source"`, `"impact"`, `"ops"`, `"op"`, `"path"`, `"effective"`, `"route"`, `"phase"`, `"policy"`, `"filterClass"`, `"after"`, `"summary"`, `"changed"`, `"added"`, `"removed"`, `"modified"`, `"routeChainsChanged"`}
	at := 0
	for _, m := range order {
		i := bytes.Index(js[at:], []byte(m))
		if i < 0 {
			t.Fatalf("member %s missing or out of order", m)
		}
		at += i
	}
	if !bytes.HasSuffix(js, []byte("}\n")) || !json.Valid(js) {
		t.Errorf("not one JSON document ending in a newline")
	}
	var buf bytes.Buffer
	if err := d.WriteJSON(&buf); err != nil || !bytes.Equal(buf.Bytes(), js) {
		t.Errorf("WriteJSON() = %v, differs from AppendJSON", err)
	}
	buf.Reset()
	if err := d.WriteText(&buf); err != nil || !bytes.Equal(buf.Bytes(), d.AppendText(nil)) {
		t.Errorf("WriteText() = %v, differs from AppendText", err)
	}
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestWriteErrors: a writer's error is returned.
func TestWriteErrors(t *testing.T) {
	d := &Diff{}
	if err := d.WriteText(failWriter{}); err == nil {
		t.Error("WriteText() error = nil")
	}
	if err := d.WriteJSON(failWriter{}); err == nil {
		t.Error("WriteJSON() error = nil")
	}
}

// TestHumanValues: strings are bare only when they cannot be misread,
// numbers are canonical, containers are RFC 8785 JSON, and nothing
// authored can start a line or reorder the display.
func TestHumanValues(t *testing.T) {
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	cases := []struct {
		n    *tree.Node
		want string
	}{
		{str("5s"), "5s"},
		{str("ghcr.io/acme/x:1.0@sha256:ab"), "ghcr.io/acme/x:1.0@sha256:ab"},
		{str(""), `""`},
		{str("true"), `"true"`},
		{str("null"), `"null"`},
		{str("3.00"), `"3.00"`},
		{str("2026-09-01"), "2026-09-01"},
		{str("a b"), `"a b"`},
		{str("line\nbreak"), `"line\nbreak"`},
		{str("tab\there"), `"tab\there"`},
		{str("abc\u202edef"), `"abc\u202edef"`},
		{str("a\u2028b"), `"a\u2028b"`},
		{str("x\u0085y"), `"x\u0085y"`},
		{str("e\U000E0001"), `"e\U000e0001"`},
		{str("café"), `"café"`},
		{&tree.Node{Kind: tree.KindFloat, Text: "1.50"}, "1.5"},
		{&tree.Node{Kind: tree.KindInt, Text: "10485760"}, "10485760"},
		{&tree.Node{Kind: tree.KindBool, Bool: true}, "true"},
		{&tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "b", Value: str("x\u202e")}, {Key: "a", Value: &tree.Node{Kind: tree.KindInt, Text: "1"}}}}, `{"a":1,"b":"x\u202e"}`},
		{&tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("x"), str("<&>")}}, `["x","<&>"]`},
		{nil, "-"},
	}
	for _, tc := range cases {
		if got := string(appendValue(nil, tc.n)); got != tc.want {
			t.Errorf("appendValue(%v) = %s, want %s", tc.n, got, tc.want)
		}
	}
}

// TestHumanLinesCannotBeForged: paths, names, sources and Environments are
// escaped where they are not printable.
func TestHumanLinesCannotBeForged(t *testing.T) {
	op := Op{Kind: OpAdd, Path: diag.Path{diag.Field("metadata"), diag.Field("labels"), diag.Field("x\n~ spec.admin: forged")}, To: &tree.Node{Kind: tree.KindString, Text: "v"}}
	got := string(appendOp(nil, &op))
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, `metadata.labels["x\n~ spec.admin: forged"]: v`) {
		t.Errorf("appendOp() = %q", got)
	}
	d := &Diff{
		Environment: "prod\u202e",
		Resources:   []Resource{{ID: tree.ID{Kind: "Route", Name: "r"}, Change: Added, Source: "routes/\u202eevil.yaml"}},
		Effective:   []ChainChange{{Route: "r", Phase: phase.OnRequestHeaders, Op: OpAdd, Policy: "p\nx", Class: phase.ClassAuth}},
	}
	text := string(d.AppendText(nil))
	for _, bad := range []string{"\u202e", "p\nx"} {
		if strings.Contains(text, bad) {
			t.Errorf("human form holds %q:\n%s", bad, text)
		}
	}
	for _, want := range []string{`(environment: prod\u202e)`, `routes/\u202eevil.yaml`, `+ p\u000ax (auth, first)`} {
		if !strings.Contains(text, want) {
			t.Errorf("human form lacks %q:\n%s", want, text)
		}
	}
}

// TestResourceColumns: the identity column is at least 48 characters and
// the source column at least 29, each widened to its longest cell plus two
// spaces; no line ends in a space.
func TestResourceColumns(t *testing.T) {
	long := strings.Repeat("n", 63)
	d := &Diff{Resources: []Resource{
		{ID: tree.ID{Kind: "Route", Name: "a"}, Change: Modified, Source: "routes/a.yaml", Impact: schemaidx.ImpactTraffic},
		{ID: tree.ID{Kind: "AIProvider", Name: long}, Change: Added, Source: "ai/providers-with-a-rather-long-name.yaml"},
	}}
	text := string(d.AppendText(nil))
	lines := strings.Split(text, "\n")
	idW := len("+ AIProvider/"+long) + 2
	srcW := len("ai/providers-with-a-rather-long-name.yaml") + 2
	want := "~ Route/a" + strings.Repeat(" ", idW-len("~ Route/a")) + "routes/a.yaml" + strings.Repeat(" ", srcW-len("routes/a.yaml")) + "[traffic]"
	if lines[2] != want {
		t.Errorf("line = %q\nwant %q", lines[2], want)
	}
	if want := "+ AIProvider/" + long + "  ai/providers-with-a-rather-long-name.yaml"; lines[3] != want {
		t.Errorf("line = %q\nwant %q", lines[3], want)
	}
	for _, l := range lines {
		if strings.HasSuffix(l, " ") {
			t.Errorf("line %q ends in a space", l)
		}
	}
}

// TestSummaryLine pins the summary's counts, plurals and impact.
func TestSummaryLine(t *testing.T) {
	cases := []struct {
		s    Summary
		want string
	}{
		{Summary{}, "0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none\n"},
		{Summary{Changed: 1, Modified: 1, RouteChainsChanged: 1, Impact: schemaidx.ImpactAI}, "1 resource changed: 0 added, 0 removed, 1 modified; 1 Route chain changed; impact: ai\n"},
		{Summary{Changed: 5, Added: 1, Removed: 1, Modified: 3, RouteChainsChanged: 2, Impact: schemaidx.ImpactAI | schemaidx.ImpactSecurity | schemaidx.ImpactMetadata}, "5 resources changed: 1 added, 1 removed, 3 modified; 2 Route chains changed; impact: ai, metadata, security\n"},
	}
	for _, tc := range cases {
		if got := string(appendSummary(nil, &tc.s)); got != tc.want {
			t.Errorf("appendSummary(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestImpliedValues: a set element or an entry holding only its key shows
// no value; any other added or removed value does.
func TestImpliedValues(t *testing.T) {
	entry := func(members ...string) *tree.Node {
		n := &tree.Node{Kind: tree.KindMap}
		for _, m := range members {
			n.Members = append(n.Members, tree.Member{Key: m, Value: &tree.Node{Kind: tree.KindString, Text: "x"}})
		}
		return n
	}
	cases := []struct {
		op   Op
		want string
	}{
		{Op{Kind: OpAdd, Path: diag.Path{diag.Field("spec"), diag.Keyed("name", "x")}, To: entry("name")}, "    + spec[name=x]\n"},
		{Op{Kind: OpRemove, Path: diag.Path{diag.Field("spec"), diag.Keyed("name", "x")}, From: entry("name", "slot")}, `    - spec[name=x]: {"name":"x","slot":"x"}` + "\n"},
		{Op{Kind: OpAdd, Path: diag.Path{diag.Field("spec"), diag.Item("GET")}, To: &tree.Node{Kind: tree.KindString, Text: "GET"}}, "    + spec[item=GET]\n"},
		{Op{Kind: OpRemove, Path: diag.Path{diag.Field("spec"), diag.Field("timeout")}, From: &tree.Node{Kind: tree.KindString, Text: "5s"}}, "    - spec.timeout: 5s\n"},
		{Op{Kind: OpMove, Path: diag.Path{diag.Field("spec"), diag.Keyed("name", "x")}, FromIndex: 0, ToIndex: 4, Flags: FlagCapabilityGrant}, "    > spec[name=x]: position 1 -> 5  (Capability grant)\n"},
	}
	for _, tc := range cases {
		if got := string(appendOp(nil, &tc.op)); got != tc.want {
			t.Errorf("appendOp(%+v) = %q, want %q", tc.op, got, tc.want)
		}
	}
}
