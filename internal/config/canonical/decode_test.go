// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// doc wraps resource objects in the canonical envelope.
func doc(resources ...string) string {
	return header + strings.Join(resources, ",") + trailer
}

// mustEncodeBundle encodes b with the hub index.
func mustEncodeBundle(t testing.TB, b *hub.Bundle) []byte {
	t.Helper()
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), b, Options{})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	return rev.Content
}

// decodeErr decodes content with the hub index and returns the error.
func decodeErr(t testing.TB, content string) error {
	t.Helper()
	_, err := NewDecoder(hubIndex(t), DecoderOptions{}).Decode(context.Background(), []byte(content), Source{Name: "c.json"})
	return err
}

// TestRoundTrip: decode then encode is byte-identical, and the decoded
// trees are envelopes with the served apiVersion in front, positions in a
// RoleCanonical file and resources in content order.
func TestRoundTrip(t *testing.T) {
	content := mustEncodeBundle(t, largeBundle(t, 30))
	files := &tree.FileTable{}
	files.Add(tree.File{Path: "ruralz.yaml", Role: tree.RoleBase})
	rs, err := NewDecoder(hubIndex(t), DecoderOptions{}).Decode(context.Background(), content, Source{Name: "lkg/content.json", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "re-encoded", reencode(t, rs), content)
	if len(rs) != 91 {
		t.Fatalf("decoded %d resources, want 91", len(rs))
	}
	first := rs[0]
	if first.ID != (tree.ID{Kind: v1alpha1.KindGateway, Name: "edge"}) || first.APIVersion != "ruralz/v1alpha1" {
		t.Errorf("first resource = %+v", first.ID)
	}
	if m := first.Root.Members[0]; m.Key != "apiVersion" || m.Value.Text != "ruralz/v1alpha1" {
		t.Errorf("first member = %q %q, want apiVersion", m.Key, m.Value.Text)
	}
	if f := files.File(first.Start.File); f.Role != tree.RoleCanonical || f.Path != "lkg/content.json" {
		t.Errorf("file = %+v, want the canonical content file", f)
	}
	if first.Start.Line != 1 || int(first.Start.Column) != len(header)+1 {
		t.Errorf("Start = %+v, want 1:%d", first.Start, len(header)+1)
	}
	timeout, _ := rs[len(rs)-1].Root.At(diag.Path{diag.Field("spec"), diag.Field("timeout")})
	if timeout == nil || timeout.Kind != tree.KindString || timeout.Text != "4s" || timeout.Style != tree.StyleJSON || !timeout.Pos.Known() {
		t.Errorf("spec.timeout = %+v", timeout)
	}
	for i := 1; i < len(rs); i++ {
		a, b := item{id: rs[i-1].ID}, item{id: rs[i].ID}
		if compareItems(&a, &b) >= 0 {
			t.Fatalf("resources %s and %s out of order", rs[i-1].ID, rs[i].ID)
		}
	}
	// Without a FileTable the positions refer to the call's own table.
	rs2, err := NewDecoder(hubIndex(t), DecoderOptions{}).Decode(context.Background(), content, Source{})
	if err != nil || len(rs2) != len(rs) || !rs2[0].Start.Known() {
		t.Errorf("Decode without files = %d resources, %v", len(rs2), err)
	}
}

// TestDecodeNewer: content from a newer producer is RZ-CFG-024, with
// every finding located and, when the resource has a string kind and
// metadata.name, naming the resource with a path from its root. The
// Policy cases put an unknown member below the PolicySpec type dispatch,
// in a closed config of each navigation shape: the config root, a
// set-list element, a nested object, a keyed entry of a nested list and
// an atomic-list entry.
func TestDecodeNewer(t *testing.T) {
	route := `{"kind":"Route","metadata":{"name":"r"},"spec":{"match":{"path":{"prefix":"/"}},"upstreams":[{"name":"orders","weight":1}]}}`
	policy := func(typ, config string) string {
		return doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":` + config + `,"type":"` + typ + `"}}`)
	}
	for _, tc := range []struct {
		name, content string
		// res is the resource every finding names, "" for none.
		res   string
		paths []string
		msg   string
	}{
		{"top-level member", `{"format":"ruralz.canonical.v1","resources":[],"schemaLevel":1}`, "", []string{"schemaLevel"}, "member is not part of ruralz.canonical.v1"},
		{"format v2", `{"format":"ruralz.canonical.v2","resources":[]}`, "", []string{"format"}, `format "ruralz.canonical.v2" is not served`},
		{"format v2 other shape", `{"format":"ruralz.canonical.v2","resources":{"r":1},"x":[]}`, "", []string{"format"}, "is not served"},
		// A format of another JSON type is a format other than
		// ruralz.canonical.v1 too: RZ-CFG-024, one finding.
		{"format number", `{"format":2,"resources":[]}`, "", []string{"format"}, "format 2 is not served by this release, which reads ruralz.canonical.v1"},
		{"format float", `{"format":1.5e3,"resources":[]}`, "", []string{"format"}, "format 1.5e3 is not served"},
		{"format object", `{"format":{"name":"ruralz.canonical","version":2},"resources":[]}`, "", []string{"format"}, "format {...} is not served"},
		{"format array", `{"format":["ruralz.canonical.v1"],"resources":[]}`, "", []string{"format"}, "format [...] is not served"},
		{"format null", `{"format":null,"resources":[]}`, "", []string{"format"}, "format null is not served"},
		{"format bool", `{"format":true,"resources":[]}`, "", []string{"format"}, "format true is not served"},
		{"format empty string", `{"format":"","resources":[]}`, "", []string{"format"}, `format "" is not served`},
		{"unknown kind", doc(`{"kind":"Widget","metadata":{"name":"w"},"spec":{}}`), "Widget/w", []string{"kind"}, `kind "Widget" is unknown to ruralz/v1alpha1`},
		{"unknown kind without a name", doc(`{"kind":"Widget","spec":{}}`), "", []string{"resources[0].kind"}, `kind "Widget" is unknown`},
		{"resource member", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{},"status":{}}`), "Route/r", []string{"status"}, "not part of a ruralz.canonical.v1 resource"},
		{"resource member without a name", doc(`{"kind":"Route","metadata":{},"spec":{},"status":{}}`), "", []string{"resources[0].status"}, "not part of a ruralz.canonical.v1 resource"},
		{"spec field", doc(strings.Replace(route, `"upstreams"`, `"retryBudget":{"ratio":0.2},"upstreams"`, 1)), "Route/r", []string{"spec.retryBudget"}, "unknown to ruralz/v1alpha1 at schema level 0"},
		{"metadata field", doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{}}`), "Route/r", []string{"metadata.owner"}, "newer release"},
		{"keyed list entry field", doc(strings.Replace(route, `"weight":1`, `"priority":2,"weight":1`, 1)), "Route/r", []string{"spec.upstreams[name=orders].priority"}, "newer release"},
		{"several", doc(`{"kind":"Route","metadata":{"name":"a","owner":"x"},"spec":{"zz":1}}`, `{"kind":"Route","metadata":{"name":"b"},"spec":{"yy":{"a":1}}}`), "", []string{"metadata.owner", "spec.zz", "spec.yy"}, "newer release"},
		{"Policy config root (auth.basic)", policy("auth.basic", `{"future":1}`), "Policy/p", []string{"spec.config.future"}, "unknown to ruralz/v1alpha1"},
		{
			"Policy set-list element (auth.mtls subjects)", policy("auth.mtls", `{"caCertificate":{"secretRef":{"key":"k","name":"n","provider":"env"}},"subjects":[{"subject":"CN=a","zz":1}]}`), "Policy/p",
			[]string{`spec.config.subjects[item={"subject":"CN=a","zz":1}].zz`},
			"unknown to ruralz/v1alpha1",
		},
		{"Policy nested object (headers request)", policy("headers", `{"request":{"zz":1}}`), "Policy/p", []string{"spec.config.request.zz"}, "unknown to ruralz/v1alpha1"},
		{"Policy nested keyed entry (headers request.set)", policy("headers", `{"request":{"set":[{"name":"x","value":"v","zz":1}]}}`), "Policy/p", []string{"spec.config.request.set[name=x].zz"}, "unknown to ruralz/v1alpha1"},
		{"Policy atomic-list entry (transform.request set)", policy("transform.request", `{"set":[{"name":"x","target":"header","valueExpression":"'a'","zz":1}]}`), "Policy/p", []string{"spec.config.set[0].zz"}, "unknown to ruralz/v1alpha1"},
	} {
		err := decodeErr(t, tc.content)
		if codeOf(err) != CodeNewer {
			t.Errorf("%s: error = %v, want %s", tc.name, err, CodeNewer)
			continue
		}
		ds := DiagnosticsOf(err)
		if len(ds) != len(tc.paths) {
			t.Errorf("%s: %d diagnostics, want %d: %v", tc.name, len(ds), len(tc.paths), ds)
			continue
		}
		for i, d := range ds {
			if d.Code != CodeNewer || d.Path.String() != tc.paths[i] || !strings.Contains(d.Message, tc.msg) || d.File != "c.json" || d.Line != 1 || d.Column < 1 {
				t.Errorf("%s: diagnostic %d = %+v (path %s), want path %s and %q", tc.name, i, d, d.Path, tc.paths[i], tc.msg)
			}
			if tc.name == "several" {
				continue
			}
			if got := resText(d); got != tc.res {
				t.Errorf("%s: diagnostic %d names resource %q, want %q", tc.name, i, got, tc.res)
			}
		}
	}
}

// resText returns the Kind/name a diagnostic names, or "".
func resText(d diag.Diagnostic) string {
	if d.Resource == nil {
		return ""
	}
	return d.Resource.Kind + "/" + d.Resource.Name
}

// TestDecodeForm: anything but exact ruralz.canonical.v1 bytes is
// RZ-CFG-027 "content is not in ruralz.canonical.v1 form".
func TestDecodeForm(t *testing.T) {
	r := `{"kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"4s"}}`
	u := `{"kind":"Upstream","metadata":{"name":"u"},"spec":{}}`
	for _, tc := range []struct {
		name, content, msg string
		col                int
	}{
		{"invalid JSON", `{"format":"ruralz.canonical.v1","resources":[`, "invalid JSON", 46},
		{"trailing value", doc(r) + `{}`, "invalid JSON", len(doc(r)) + 1},
		{"duplicate names", `{"format":"ruralz.canonical.v1","format":"x","resources":[]}`, "invalid JSON: duplicate object member name", 33},
		{"not an object", `[]`, "not a JSON object", 1},
		{"no format", `{"resources":[]}`, "no format member", 1},
		{"no resources", `{"format":"ruralz.canonical.v1"}`, "no resources member", 1},
		{"resources not an array", `{"format":"ruralz.canonical.v1","resources":{}}`, "resources is not an array", 45},
		{"resource not an object", doc(`1`), "resource is not an object", 46},
		{"no kind", doc(`{"metadata":{"name":"r"},"spec":{}}`), "resource has no kind", 46},
		{"kind not a string", doc(`{"kind":7,"metadata":{"name":"r"}}`), "resource has no kind", 46},
		{"environment", doc(`{"kind":"Environment","metadata":{"name":"prod"},"spec":{}}`), "kind Environment never appears", 54},
		{"cluster", doc(`{"kind":"Cluster","metadata":{"name":"c"},"spec":{}}`), "kind Cluster never appears", 54},
		{"no metadata", doc(`{"kind":"Route","spec":{}}`), "no metadata.name", 46},
		{"name not a string", doc(`{"kind":"Route","metadata":{"name":1}}`), "no metadata.name", 46},
		{"apiVersion member", doc(`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{}}`), "a resource carries no apiVersion", 47},
		{"whitespace", strings.Replace(doc(r), `"spec":{`, `"spec": {`, 1), "differs here", 0},
		{"leading whitespace", " " + doc(r), "differs here", 1},
		{"trailing newline", doc(r) + "\n", "differs here", 0},
		{"members reordered", doc(`{"metadata":{"name":"r"},"kind":"Route","spec":{"timeout":"4s"}}`), "differs here", 48},
		{"envelope reordered", `{"resources":[],"format":"ruralz.canonical.v1"}`, "differs here", 3},
		{"resources out of order", doc(r, u), "not in kind and name order", 0},
		{"names out of order", doc(`{"kind":"Route","metadata":{"name":"b"},"spec":{}}`, `{"kind":"Route","metadata":{"name":"a"},"spec":{}}`), "not in kind and name order", 0},
		{"duplicate resource", doc(r, r), "appears twice", 0},
		{"number form", doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"n":1.0}}}`), "differs here", 0},
		{"negative zero", doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"n":-0}}}`), "differs here", 0},
		{"string escape", doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"p":"\u0034s"}}}`), "differs here", 0},
		{"slash escape", doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"p":"\/a"}}}`), "differs here", 0},
		{"uppercase hex", doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"p":"\u001F"}}}`), "differs here", 0},
		{"null", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"timeout":null}}`), "null has no ruralz.canonical.v1 form", 0},
		{"conversion data", doc(`{"kind":"Route","metadata":{"annotations":{"ruralz.io/conversion-data":"x"},"name":"r"},"spec":{}}`), "differs here", 0},
		{"comma", strings.Replace(doc(r, `{"kind":"Route","metadata":{"name":"s"},"spec":{}}`), `},{"kind":"Route","metadata":{"name":"s"}`, `} ,{"kind":"Route","metadata":{"name":"s"}`, 1), "differs here", 0},
	} {
		err := decodeErr(t, tc.content)
		if codeOf(err) != CodeMismatch {
			t.Errorf("%s: error = %v, want %s", tc.name, err, CodeMismatch)
			continue
		}
		ds := DiagnosticsOf(err)
		if len(ds) == 0 || !strings.HasPrefix(ds[0].Message, notCanonical) || !strings.Contains(ds[0].Message, tc.msg) {
			t.Errorf("%s: diagnostics = %v, want %q", tc.name, ds, tc.msg)
			continue
		}
		if tc.col > 0 && (ds[0].Line != 1 || ds[0].Column != tc.col) {
			t.Errorf("%s: at %d:%d, want 1:%d", tc.name, ds[0].Line, ds[0].Column, tc.col)
		}
	}
	// A scan failure keeps the jsonval error in the chain.
	if err := decodeErr(t, `{"format":`); !errors.Is(err, jsonval.ErrSyntax) {
		t.Errorf("invalid JSON: error = %v, want jsonval.ErrSyntax in the chain", err)
	}
}

// TestDecodePrecedence: a newer producer wins over a form problem
// wherever each appears, so an older Node reports RZ-CFG-024 for content
// it cannot judge, and every newer finding is reported, in content
// order. The class: a form problem at the document level (no format, no
// or a non-array resources), at the resource level (no kind, no
// metadata.name, an apiVersion member, a kind the format never holds) or
// in the bytes (whitespace), beside a newer member at the document
// level, at the resource level or in a field, of another resource or of
// the same one: a resource-level form problem of a kind the hub schema
// knows does not hide the newer fields of that resource (no string
// metadata.name, Environment, Cluster). Only a format other than the
// string ruralz.canonical.v1, of any JSON type, stops the judging, with
// that one finding, wherever it lies.
func TestDecodePrecedence(t *testing.T) {
	owner := `{"kind":"Route","metadata":{"name":"b","owner":"x"},"spec":{}}`
	for _, tc := range []struct {
		name, content string
		// paths are the findings' paths, each with the resource it names
		// before a space ("Route/b metadata.owner") when it names one.
		paths []string
	}{
		// Document-level form problems beside a document-level newer member.
		{"no format, newer member", `{"formatVersion":2,"resources":[]}`, []string{"formatVersion"}},
		{"format not a string, newer member", `{"format":1,"resources":[],"level":2}`, []string{"format"}},
		{"format null, newer member", `{"format":null,"level":2,"resources":[]}`, []string{"format"}},
		{"newer member, format not a string", `{"aa":1,"format":{},"resources":[]}`, []string{"format"}},
		{"no resources, newer member", `{"format":"ruralz.canonical.v1","level":2}`, []string{"level"}},
		{"resources not an array, newer member", `{"format":"ruralz.canonical.v1","level":2,"resources":{}}`, []string{"level"}},
		// Document-level form problems beside resource and field findings.
		{"no format, unknown kind", `{"resources":[{"kind":"Widget","metadata":{"name":"w"}}]}`, []string{"Widget/w kind"}},
		{"format not a string, newer field", `{"format":true,"resources":[` + owner + `]}`, []string{"format"}},
		{"newer field, format not a string", `{"resources":[` + owner + `],"format":[]}`, []string{"format"}},
		{"form problems, format not a string", `{"resources":[1,{}],"format":7}`, []string{"format"}},
		// Resource-level form problems beside a newer field elsewhere.
		{"no metadata.name, newer field", doc(`{"kind":"Route","spec":{}}`, owner), []string{"Route/b metadata.owner"}},
		{"no kind, newer field", doc(`{"metadata":{"name":"a"}}`, owner), []string{"Route/b metadata.owner"}},
		{"resource not an object, newer field", doc(`[]`, owner), []string{"Route/b metadata.owner"}},
		{"Environment kind, newer field", doc(`{"kind":"Environment","metadata":{"name":"e"},"spec":{}}`, owner), []string{"Route/b metadata.owner"}},
		{"apiVersion member, newer field in it", doc(`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"b","owner":"x"},"spec":{}}`), []string{"Route/b metadata.owner"}},
		{"no metadata.name, unknown kind", `{"format":"ruralz.canonical.v1","resources":[{"kind":"Route","spec":{}},{"kind":"Widget","metadata":{"name":"w"}}]}`, []string{"Widget/w kind"}},
		// Resource-level form problems beside newer fields of the same
		// resource.
		{"no metadata.name, newer metadata and spec fields", doc(`{"kind":"Route","metadata":{"title":"x"},"spec":{"future":1}}`), []string{"resources[0].metadata.title", "resources[0].spec.future"}},
		{"no metadata.name, generateName", doc(`{"kind":"Route","metadata":{"generateName":"r-"},"spec":{}}`), []string{"resources[0].metadata.generateName"}},
		{"name not a string, newer spec field", doc(`{"kind":"Route","metadata":{"name":5},"spec":{"zz":1}}`), []string{"resources[0].spec.zz"}},
		{"name not a string, newer metadata field", doc(`{"kind":"Route","metadata":{"name":7,"uid":"x"},"spec":{}}`), []string{"resources[0].metadata.uid"}},
		{"Environment kind, newer spec field", doc(`{"kind":"Environment","metadata":{"name":"e"},"spec":{"zz":1}}`), []string{"Environment/e spec.zz"}},
		{"Cluster kind, newer metadata field", doc(`{"kind":"Cluster","metadata":{"name":"c","uid":"x"},"spec":{}}`), []string{"Cluster/c metadata.uid"}},
		{"apiVersion member and no name, newer field", doc(`{"apiVersion":"x","kind":"Route","metadata":{"title":"x"}}`), []string{"resources[0].metadata.title"}},
		// Byte-level form problems beside a newer field.
		{"whitespace, newer field", `{"format": "ruralz.canonical.v1", "resources": [{"kind": "Route", "metadata": {"name": "r", "owner": "x"}}]}`, []string{"Route/r metadata.owner"}},
		// Newer findings of several levels are all reported, in content order.
		{"newer member and field of one resource", doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{},"status":{}}`), []string{"Route/r metadata.owner", "Route/r status"}},
		{"resource member before metadata", doc(`{"aa":1,"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{"zz":1}}`), []string{"Route/r aa", "Route/r metadata.owner", "Route/r spec.zz"}},
		{"newer field and document member", `{"format":"ruralz.canonical.v1","resources":[` + owner + `],"schemaLevel":1}`, []string{"Route/b metadata.owner", "schemaLevel"}},
		{"document member before resources", `{"aa":1,"format":"ruralz.canonical.v1","resources":[` + owner + `]}`, []string{"aa", "Route/b metadata.owner"}},
		// Another format: one finding, nothing else judged.
		{"other format after resources", `{"resources":[{"kind":"Widget","metadata":{"name":"w"}}],"format":"ruralz.canonical.v2","x":1}`, []string{"format"}},
	} {
		err := decodeErr(t, tc.content)
		if codeOf(err) != CodeNewer {
			t.Errorf("%s: error = %v, want %s", tc.name, err, CodeNewer)
			continue
		}
		ds := DiagnosticsOf(err)
		var got []string
		for _, d := range ds {
			p := d.Path.String()
			if r := resText(d); r != "" {
				p = r + " " + p
			}
			got = append(got, p)
		}
		if strings.Join(got, " | ") != strings.Join(tc.paths, " | ") {
			t.Errorf("%s: paths %q, want %q", tc.name, got, tc.paths)
		}
		for i := 1; i < len(ds); i++ {
			if ds[i].Line == ds[i-1].Line && ds[i].Column < ds[i-1].Column {
				t.Errorf("%s: findings out of content order: %v", tc.name, ds)
			}
		}
	}
}

// TestDecodeEnvelopeOnly: a resource without a string kind, or of a kind
// this release does not know, is judged by its envelope alone, since no
// schema says which of its fields exist (package documentation).
func TestDecodeEnvelopeOnly(t *testing.T) {
	for _, tc := range []struct {
		name, content, code, path string
	}{
		{"kind not a string", doc(`{"kind":5,"metadata":{"name":"r","title":"x"},"spec":{"zz":1}}`), CodeMismatch, "resources[0]"},
		{"no kind", doc(`{"metadata":{"name":"r","title":"x"},"spec":{"zz":1}}`), CodeMismatch, "resources[0]"},
		{"unknown kind", doc(`{"kind":"Widget","metadata":{"name":"w","title":"x"},"spec":{"zz":1}}`), CodeNewer, "kind"},
	} {
		err := decodeErr(t, tc.content)
		ds := DiagnosticsOf(err)
		if codeOf(err) != tc.code || len(ds) != 1 || ds[0].Path.String() != tc.path {
			t.Errorf("%s: error = %v, diagnostics %v; want one %s at %s", tc.name, err, ds, tc.code, tc.path)
		}
	}
}

// TestDecodeFormOrder: form findings of several levels are all reported,
// in content order, whatever order the document members come in.
func TestDecodeFormOrder(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		msgs          []string
	}{
		// The two document findings of {} come in member order, format
		// first.
		{"empty document", `{}`, []string{"no format member", "no resources member"}},
		{"no format before a bad resource", `{"resources":[1]}`, []string{"no format member", "resource is not an object"}},
		{"no format, resources not an array", `{"resources":{"a":[1]}}`, []string{"no format member", "resources is not an array"}},
		{"no format and two bad resources", `{"resources":[{"kind":"Route"},{"metadata":{}}]}`, []string{"no format member", "resource has no metadata.name", "resource has no kind"}},
		// The missing name is located at the resource, before its members.
		{"apiVersion member and no name", doc(`{"apiVersion":"x","kind":"Route","metadata":{}}`), []string{"resource has no metadata.name", "a resource carries no apiVersion"}},
	} {
		err := decodeErr(t, tc.content)
		ds := DiagnosticsOf(err)
		if codeOf(err) != CodeMismatch || len(ds) != len(tc.msgs) {
			t.Errorf("%s: error = %v (%d findings), want %d %s findings", tc.name, err, len(ds), len(tc.msgs), CodeMismatch)
			continue
		}
		for i, d := range ds {
			if !strings.Contains(d.Message, tc.msgs[i]) {
				t.Errorf("%s: finding %d = %q, want %q", tc.name, i, d.Message, tc.msgs[i])
			}
		}
	}
}

// TestDecodeIdentity: every finding about a resource whose kind and
// metadata.name are strings names that resource and has a path from its
// root, whatever level raised it (a resource member, a kind the format
// never holds or does not know, a field, the re-encoding, the order of
// the resources); a finding about a resource without them, or about the
// document, names none and has a path from the document root (TQ
// "Conformance suites": code and resource identity wherever raised).
func TestDecodeIdentity(t *testing.T) {
	r := `{"kind":"Route","metadata":{"name":"r"},"spec":{}}`
	for _, tc := range []struct {
		name, content, code string
		// want is the first finding: its resource ("" for none), then its
		// path.
		res, path string
	}{
		{"resource member", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{},"status":{}}`), CodeNewer, "Route/r", "status"},
		{"unknown kind", doc(`{"kind":"Widget","metadata":{"name":"w"},"spec":{}}`), CodeNewer, "Widget/w", "kind"},
		{"field", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"zz":1}}`), CodeNewer, "Route/r", "spec.zz"},
		{"document member", `{"format":"ruralz.canonical.v1","resources":[` + r + `],"x":1}`, CodeNewer, "", "x"},
		{"apiVersion member", doc(`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{}}`), CodeMismatch, "Route/r", "apiVersion"},
		{"Environment kind", doc(`{"kind":"Environment","metadata":{"name":"prod"},"spec":{}}`), CodeMismatch, "Environment/prod", "kind"},
		{"Cluster kind", doc(`{"kind":"Cluster","metadata":{"name":"c"},"spec":{}}`), CodeMismatch, "Cluster/c", "kind"},
		{"no metadata.name", doc(`{"kind":"Route","metadata":{},"spec":{}}`), CodeMismatch, "", "resources[0]"},
		{"name not a string", doc(`{"apiVersion":"x","kind":"Route","metadata":{"name":1},"spec":{}}`), CodeMismatch, "", "resources[0]"},
		{"no kind", doc(`{"metadata":{"name":"r"},"spec":{}}`), CodeMismatch, "", "resources[0]"},
		{"not an object", doc(`true`), CodeMismatch, "", "resources[0]"},
		{"no format", `{"resources":[]}`, CodeMismatch, "", ""},
		{"format not a string", `{"format":1,"resources":[]}`, CodeNewer, "", "format"},
		{"null value", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"timeout":null}}`), CodeMismatch, "Route/r", "spec.timeout"},
		{"duplicate resource", doc(r, r), CodeMismatch, "Route/r", ""},
		{"whitespace", strings.Replace(doc(r), `"spec":{`, `"spec": {`, 1), CodeMismatch, "Route/r", ""},
	} {
		err := decodeErr(t, tc.content)
		ds := DiagnosticsOf(err)
		if codeOf(err) != tc.code || len(ds) == 0 {
			t.Errorf("%s: error = %v, want %s", tc.name, err, tc.code)
			continue
		}
		if got := resText(ds[0]); got != tc.res || ds[0].Path.String() != tc.path {
			t.Errorf("%s: finding names %q at %q, want %q at %q", tc.name, got, ds[0].Path, tc.res, tc.path)
		}
	}
}

// TestDecodeNoRangeRule: an integer-looking literal above 2^53 is the RFC
// 8785 form of a double; canonical decode accepts it.
func TestDecodeNoRangeRule(t *testing.T) {
	content := doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"maximum":123456789012345680000,"minimum":-9007199254740992}}}`)
	rs := mustDecode(t, []byte(content))
	sameBytes(t, "re-encoded", reencode(t, rs), []byte(content))
	n, _ := rs[0].Root.At(diag.Path{diag.Field("spec"), diag.Field("configSchema"), diag.Field("maximum")})
	if n == nil || n.Kind != tree.KindInt || n.Text != "123456789012345680000" {
		t.Errorf("maximum = %+v", n)
	}
}

// TestDecodeLeavesValuesToStageF pins where the newer-producer check
// stops: it names unknown members, kinds and fields, not values, so a
// value outside an enum, a dispatch value no branch matches (a Policy
// type this release does not register) and an undeclared member of an
// open Policy config pass Decode and are stage F's (RZ-CFG-005) when
// FromResources re-runs the pipeline. Whether canonical content should
// report them as RZ-CFG-024 instead is an open question, not decided
// here.
func TestDecodeLeavesValuesToStageF(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"unknown Policy type", doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"x":1},"type":"future.type"}}`)},
		{"open Policy config member", doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"allowOrigins":["*"],"futureField":1},"type":"cors"}}`)},
		{"enum value", doc(`{"kind":"Upstream","metadata":{"name":"u"},"spec":{"endpoints":[{"address":"a:1"}],"protocol":"http4"}}`)},
		{"nested enum value", doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"driver":"future"}}}`)},
	} {
		if err := decodeErr(t, tc.content); err != nil {
			t.Errorf("%s: error = %v, want none (stage F judges values)", tc.name, err)
		}
	}
}

// TestVerify: verification checks the digest first, then the form.
func TestVerify(t *testing.T) {
	ctx := context.Background()
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	content := encode(t, gatewayDoc, routeDoc)
	want := revision.Sum(content)
	rs, err := dec.Verify(ctx, content, want, Source{Name: "lkg.json"})
	if err != nil || len(rs) != 2 {
		t.Fatalf("Verify() = %d, %v", len(rs), err)
	}
	flipped := []byte(strings.Replace(string(content), `"edge"`, `"edgf"`, 1))
	_, err = dec.Verify(ctx, flipped, want, Source{Name: "lkg.json"})
	wantMsg := "content hashes to " + revision.Sum(flipped).String() + ", want " + want.String()
	if codeOf(err) != CodeMismatch || err.Error() != "RZ-CFG-027: "+wantMsg {
		t.Errorf("flipped byte: error = %v, want %q", err, wantMsg)
	}
	if ds := DiagnosticsOf(err); len(ds) != 1 || ds[0].File != "lkg.json" || ds[0].Message != wantMsg {
		t.Errorf("flipped byte: diagnostics = %v", ds)
	}
	if _, err := dec.Verify(ctx, content, revision.Digest{}, Source{}); codeOf(err) != CodeMismatch {
		t.Errorf("zero digest: error = %v", err)
	}
	// Hash first: reformatted content with its own digest fails the form.
	spaced := []byte(strings.Replace(string(content), `"spec":{`, `"spec": {`, 1))
	if _, err := dec.Verify(ctx, spaced, revision.Sum(spaced), Source{}); codeOf(err) != CodeMismatch || !strings.Contains(err.Error(), notCanonical) {
		t.Errorf("reformatted: error = %v", err)
	}
}

// TestDecodeWorkers: results and errors do not depend on the worker
// count.
func TestDecodeWorkers(t *testing.T) {
	content := mustEncodeBundle(t, largeBundle(t, 100))
	bad := strings.Replace(string(content), `"name":"route-57"},"spec":{`, `"name":"route-57"},"spec":{"aa":1,`, 1)
	bad = strings.Replace(bad, `"name":"route-9"},"spec":{`, `"name":"route-9"},"spec":{"ab":1,`, 1)
	form := strings.Replace(string(content), `"name":"route-42"},"spec":{"match"`, `"name":"route-42"},"spec":{ "match"`, 1)
	if form == string(content) {
		t.Fatal("form mutation did not apply")
	}
	var first []diag.Diagnostic
	for _, w := range []int{0, 1, 2, 5, 64} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w})
		rs, err := dec.Decode(context.Background(), content, Source{})
		if err != nil || len(rs) != 301 {
			t.Fatalf("workers %d: %d resources, %v", w, len(rs), err)
		}
		_, err = dec.Decode(context.Background(), []byte(bad), Source{})
		ds := DiagnosticsOf(err)
		if codeOf(err) != CodeNewer || len(ds) != 2 {
			t.Fatalf("workers %d: error = %v", w, err)
		}
		if first == nil {
			first = ds
		} else if ds[0].Path.String() != first[0].Path.String() || ds[0].Resource.Name != first[0].Resource.Name || ds[1].Resource.Name != first[1].Resource.Name {
			t.Errorf("workers %d: diagnostics %v, want %v", w, ds, first)
		}
		_, err = dec.Decode(context.Background(), []byte(form), Source{})
		if codeOf(err) != CodeMismatch {
			t.Errorf("workers %d: form error = %v", w, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := dec.Decode(ctx, content, Source{}); !errors.Is(err, context.Canceled) {
			t.Errorf("workers %d: canceled decode error = %v", w, err)
		}
	}
	if first[0].Resource.Name != "route-57" || first[1].Resource.Name != "route-9" {
		t.Errorf("findings in content order: %v", first)
	}
}

// TestNoIndex: a decoder without a schema index fails every call with
// ErrNoIndex before it reads, hashes or decodes anything (NewDecoder), so
// Verify reports no RZ-CFG-027 for a bad hash, whatever the content: a
// valid Revision with its digest or a wrong one, a valid dump or a body
// that is not one. The reader of ReadDump is not read.
func TestNoIndex(t *testing.T) {
	content := []byte(doc())
	good := revision.Sum(content)
	dump := dumpOf(t, nil, revision.Revision{Content: content, Digest: good}, revision.Digest{})
	d := NewDecoder(nil, DecoderOptions{MaxDepth: 1 << 30})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Decode", func() error { _, err := d.Decode(ctx, content, Source{}); return err }},
		{"Decode of invalid JSON", func() error { _, err := d.Decode(ctx, []byte("x"), Source{}); return err }},
		{"Verify, good hash", func() error { _, err := d.Verify(ctx, content, good, Source{}); return err }},
		{"Verify, bad hash", func() error { _, err := d.Verify(ctx, []byte("x"), revision.Digest{}, Source{}); return err }},
		{"ReadDump, valid dump", func() error {
			r := bytes.NewReader(dump)
			_, _, err := d.ReadDump(ctx, r, 0, Source{})
			if r.Len() != len(dump) {
				t.Errorf("ReadDump, valid dump: %d bytes read", len(dump)-r.Len())
			}
			return err
		}},
		{"ReadDump, not a dump", func() error { _, _, err := d.ReadDump(ctx, strings.NewReader("[]"), 0, Source{}); return err }},
		{"ReadDump, over the limit", func() error { _, _, err := d.ReadDump(ctx, bytes.NewReader(dump), 1, Source{}); return err }},
	} {
		if err := tc.call(); !errors.Is(err, ErrNoIndex) {
			t.Errorf("%s: error = %v, want ErrNoIndex", tc.name, err)
		}
	}
}

// TestPositionKeepsFile: a line or column past what tree.Pos holds
// (content over 2 GiB) leaves the position unknown but keeps its file, so
// stage G reads the role of the canonical file the decode registered, not
// of whatever file has ID 0: the number range exemption of canonical
// content and hub.Source.File read the canonical file, not a base file of
// the caller's table.
func TestPositionKeepsFile(t *testing.T) {
	var over int64 = math.MaxInt32 + 1
	big := int(over)
	if int64(big) != over {
		t.Skip("int holds no position past 2^31")
	}
	const file tree.FileID = 3
	for _, tc := range []struct {
		name string
		p    jsonval.Position
		want tree.Pos
	}{
		{"known", jsonval.Position{Line: 1, Column: 7}, tree.Pos{File: file, Line: 1, Column: 7}},
		{"at the limit", jsonval.Position{Line: math.MaxInt32, Column: math.MaxInt32}, tree.Pos{File: file, Line: math.MaxInt32, Column: math.MaxInt32}},
		{"column past the limit", jsonval.Position{Line: 1, Column: big}, tree.Pos{File: file}},
		{"line past the limit", jsonval.Position{Line: big, Column: 1}, tree.Pos{File: file}},
	} {
		got := posOf(file, tc.p)
		if got != tc.want {
			t.Errorf("%s: posOf = %+v, want %+v", tc.name, got, tc.want)
		}
		files := &tree.FileTable{}
		files.Add(tree.File{Path: "base.yaml", Role: tree.RoleBase})
		in := newInput([]byte(doc()), 0, len(doc()), Source{Name: "lkg.json", Files: files})
		if p := posOf(in.file, tc.p); files.File(p.File).Role != tree.RoleCanonical {
			t.Errorf("%s: the position names file %d of role %v, want the canonical file", tc.name, p.File, files.File(p.File).Role)
		}
	}
}

// TestDiagnosticsOf covers the error helpers.
func TestDiagnosticsOf(t *testing.T) {
	err := decodeErr(t, doc(`{"kind":"Route","metadata":{"name":"a","x":1,"y":2},"spec":{}}`))
	if got := err.Error(); got != "RZ-CFG-024: Route/a metadata.x: field is unknown to ruralz/v1alpha1 at schema level 0; the content comes from a newer release (and 1 more)" {
		t.Errorf("Error() = %q", got)
	}
	var f *Findings
	if !errors.As(err, &f) || f.Unwrap() != nil || len(f.Diagnostics) != 2 {
		t.Errorf("Findings = %+v", f)
	}
	if ds := DiagnosticsOf(errcode.Errorf(CodeMismatch, "boom")); len(ds) != 1 || ds[0].Code != CodeMismatch || ds[0].Message != "RZ-CFG-027: boom" {
		t.Errorf("DiagnosticsOf(errcode) = %v", ds)
	}
	if ds := DiagnosticsOf(errors.New("plain")); ds != nil {
		t.Errorf("DiagnosticsOf(plain) = %v", ds)
	}
	for _, tc := range []struct {
		f    Findings
		want string
	}{
		{Findings{}, "canonical: no findings"},
		{Findings{Err: errors.New("cause")}, "cause"},
		{Findings{Diagnostics: diag.List{{Message: "m"}}}, "m"},
	} {
		if got := tc.f.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// TestNewerWalkCachesElem: the key-aware element of a list entry is read
// once per entry, not once per finding below it (scale_test's quadratic
// case). The white-box half reports two findings below one keyed entry
// whose key changes between them: both name the key read first. The
// allocation half decodes one set element (auth.mtls subjects) holding k
// and 2k unknown members: its canonical JSON is built once, so twice the
// findings allocate about twice the bytes, where building it per finding
// would allocate four times.
func TestNewerWalkCachesElem(t *testing.T) {
	idx := hubIndex(t)
	content := []byte(doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"a","zz":1}]}}`))
	d := NewDecoder(idx, DecoderOptions{})
	in := newInput(content, 0, len(content), Source{})
	rn := jsonTree(t, `{"kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"a","zz":1}]}}`)
	spec, _ := rn.Get("spec")
	ups, _ := spec.Get("upstreams")
	entry := ups.Items[0]
	rs, _ := idx.Resource("Route")
	specS, ok := rs.Select(rn).Property("spec")
	if !ok {
		t.Fatal("no spec schema")
	}
	upsS, ok := specS.Select(spec).Property("upstreams")
	if !ok {
		t.Fatal("no upstreams schema")
	}
	var who ident
	who.reset(0, true, "Route", "r")
	out := found{limit: 10}
	w := newerWalk{d: d, in: in}
	w.start(&who, &out)
	w.frames = append(w.frames, frame{name: "spec"}, frame{name: "upstreams"}, frame{list: upsS, item: entry, i: 0})
	zz := &entry.Members[1]
	w.report(zz, "first")
	name, _ := entry.Get("name")
	name.Text = "b"
	w.report(zz, "second")
	if len(out.newer) != 2 || out.newer[0].Path.String() != "spec.upstreams[name=a].zz" || out.newer[1].Path.String() != "spec.upstreams[name=a].zz" {
		t.Errorf("findings %v, want both at spec.upstreams[name=a].zz", out.newer)
	}

	if testing.Short() {
		return
	}
	set := func(k int) []byte {
		return []byte(doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"caCertificate":{"secretRef":{"key":"k","name":"n","provider":"env"}},"subjects":[{` + members(k, "x") + `,"subject":"CN=a"}]},"type":"auth.mtls"}}`))
	}
	dec := NewDecoder(idx, DecoderOptions{Workers: 1, MaxDiagnostics: 1 << 20})
	const k = 3000
	small, large := allocOf(dec, set(k)), allocOf(dec, set(2*k))
	if ratio := float64(large) / float64(small); ratio > 2.6 {
		t.Errorf("2x the findings below one set element allocate %.2fx the bytes (%d vs %d), want about 2x", ratio, large, small)
	}
}

// TestEnvelopeOrderKeys: the document findings of {} carry distinct,
// increasing content-order keys, format before resources, so their order
// never rests on the stability of the keeper's sort.
func TestEnvelopeOrderKeys(t *testing.T) {
	d := NewDecoder(hubIndex(t), DecoderOptions{})
	in := newInput([]byte(`{}`), 0, 2, Source{})
	r := newRun(d.maxDiags)
	d.envelope(&tree.Node{Kind: tree.KindMap}, in, r, nil)
	es := r.form.entries
	if len(es) != 2 || es[0].o.compare(es[1].o) >= 0 || !strings.Contains(es[0].d.Message, "no format") || !strings.Contains(es[1].d.Message, "no resources") {
		t.Errorf("entries %+v, want no format then no resources, keys increasing", es)
	}
}
