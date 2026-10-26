// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"errors"
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
// every finding located.
func TestDecodeNewer(t *testing.T) {
	route := `{"kind":"Route","metadata":{"name":"r"},"spec":{"match":{"path":{"prefix":"/"}},"upstreams":[{"name":"orders","weight":1}]}}`
	for _, tc := range []struct {
		name, content string
		paths         []string
		msg           string
	}{
		{"top-level member", `{"format":"ruralz.canonical.v1","resources":[],"schemaLevel":1}`, []string{"schemaLevel"}, "member is not part of ruralz.canonical.v1"},
		{"format v2", `{"format":"ruralz.canonical.v2","resources":[]}`, []string{"format"}, `format "ruralz.canonical.v2" is not served`},
		{"format v2 other shape", `{"format":"ruralz.canonical.v2","resources":{"r":1},"x":[]}`, []string{"format"}, "is not served"},
		{"unknown kind", doc(`{"kind":"Widget","metadata":{"name":"w"},"spec":{}}`), []string{"resources[0].kind"}, `kind "Widget" is unknown to ruralz/v1alpha1`},
		{"resource member", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{},"status":{}}`), []string{"resources[0].status"}, "not part of a ruralz.canonical.v1 resource"},
		{"spec field", doc(strings.Replace(route, `"upstreams"`, `"retryBudget":{"ratio":0.2},"upstreams"`, 1)), []string{"spec.retryBudget"}, "unknown to ruralz/v1alpha1 at schema level 0"},
		{"metadata field", doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{}}`), []string{"metadata.owner"}, "newer release"},
		{"keyed list entry field", doc(strings.Replace(route, `"weight":1`, `"priority":2,"weight":1`, 1)), []string{"spec.upstreams[name=orders].priority"}, "newer release"},
		{"several", doc(`{"kind":"Route","metadata":{"name":"a","owner":"x"},"spec":{"zz":1}}`, `{"kind":"Route","metadata":{"name":"b"},"spec":{"yy":{"a":1}}}`), []string{"metadata.owner", "spec.zz", "spec.yy"}, "newer release"},
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
		}
	}
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
		{"too deep", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"a":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}}`), "nesting deeper than 128", 0},
		{"not an object", `[]`, "not a JSON object", 1},
		{"no format", `{"resources":[]}`, "no format member", 1},
		{"format not a string", `{"format":1,"resources":[]}`, "format is not a string", 11},
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

// TestDecodePrecedence: a newer producer wins over a form problem, so an
// older Node reports RZ-CFG-024 for content it cannot judge.
func TestDecodePrecedence(t *testing.T) {
	err := decodeErr(t, `{"format":"ruralz.canonical.v1","resources":[{"kind":"Route","spec":{}},{"kind":"Widget","metadata":{"name":"w"}}]}`)
	if codeOf(err) != CodeNewer {
		t.Errorf("error = %v, want %s", err, CodeNewer)
	}
	err = decodeErr(t, `{"format": "ruralz.canonical.v1", "resources": [{"kind": "Route", "metadata": {"name": "r", "owner": "x"}}]}`)
	if codeOf(err) != CodeNewer {
		t.Errorf("unknown field with whitespace: error = %v, want %s", err, CodeNewer)
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

// TestNoIndex: a decoder without a schema index refuses to decode.
func TestNoIndex(t *testing.T) {
	_, err := NewDecoder(nil, DecoderOptions{MaxDepth: 1 << 30}).Decode(context.Background(), []byte(doc()), Source{})
	if !errors.Is(err, ErrNoIndex) {
		t.Errorf("error = %v, want ErrNoIndex", err)
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
