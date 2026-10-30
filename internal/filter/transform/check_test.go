// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// want is one expected finding: "path: message", prefixed "schema " when
// the schema reports it itself.
type want = string

// findingTexts returns the findings of cfg as want strings.
func findingTexts(t *testing.T, cfg any) []string {
	t.Helper()
	s := specOf(t, cfg)
	var out []string
	for _, f := range findings(&s) {
		text := f.path.String() + ": " + f.message
		if f.schema {
			text = "schema " + text
		}
		out = append(out, text)
	}
	return out
}

// TestFindings covers every RZ-CFG-005 finding of a transform config, in
// field order, and which of them the schema reports itself.
func TestFindings(t *testing.T) {
	many := &v1alpha1.TransformRequestConfig{}
	for range 33 {
		many.Remove = append(many.Remove, reqRemove("header", "x-a"))
	}
	long := strings.Repeat("a", MaxPatternBytes+1)
	wide := strings.Repeat("é", MaxPatternBytes/2+1) // over 1 KiB, under 1,024 characters
	wideLong := strings.Repeat("é", MaxPatternBytes+1)
	tests := []struct {
		name string
		cfg  any
		want []want
	}{
		{"empty request", &v1alpha1.TransformRequestConfig{}, nil},
		{"empty response", &v1alpha1.TransformResponseConfig{}, nil},
		{"valid request", &v1alpha1.TransformRequestConfig{
			Body: "request.body", ContentType: ptr("application/soap+xml; charset=utf-8; action=\"urn:a;b\""),
			Set: []v1alpha1.TransformRequestSet{
				reqSet("header", "x-a", `"a"`), reqSet("query", "page", `"2"`), reqSet("path", "", `"/v2"`), reqSet("body", `a\.b.*.c`, "1"),
			},
			Remove:   []v1alpha1.TransformRequestRemove{reqRemove("header", "x-b"), reqRemove("query", "q"), reqRemove("body", "internal")},
			ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus"), arrayOp("append", "a", "b.c"), arrayOp("delete", "items.0", "")},
			Replace:  []v1alpha1.Replace{pattern("customer.email", "^[^@]+", "***"), literally("", "(", ")"), pattern("", "a(?P<x>b)", "${x}")},
		}, nil},
		{"entry count", many, []want{": 33 entries across the set, remove, arrayOps and replace lists, more than 32"}},
		{"content types", &v1alpha1.TransformResponseConfig{ContentType: ptr("text")}, []want{`contentType: "text" is not an RFC 9110 media type`}},
		{"empty content type", &v1alpha1.TransformRequestConfig{ContentType: ptr("")}, []want{`contentType: "" is not an RFC 9110 media type`}},
		{"header names", &v1alpha1.TransformRequestConfig{
			Set:    []v1alpha1.TransformRequestSet{reqSet("header", "x a", `"v"`), reqSet("header", "host", `"v"`), reqSet("header", "", `"v"`)},
			Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "traceparent"), reqRemove("header", strings.Repeat("x", 257))},
		}, []want{
			`set[0].name: field name is not an RFC 9110 token: byte ' ' at offset 1`,
			`set[1].name: header host is managed by the Node (host)`,
			`set[2].name: field name is empty`,
			`remove[0].name: header traceparent is managed by the Node (trace context)`,
			`remove[1].name: field name is too long: 257 bytes, the limit is 256`,
		}},
		{"body fields in a body rewrite", &v1alpha1.TransformResponseConfig{
			Set:    []v1alpha1.TransformResponseSet{respSet("header", "Content-Encoding", `"gzip"`), respSet("header", "content-type", `"text/plain"`)},
			Remove: []v1alpha1.TransformResponseRemove{respRemove("header", "Digest"), respRemove("header", "content-md5"), respRemove("header", "Repr-Digest"), respRemove("header", "content-digest"), respRemove("body", "a")},
		}, []want{
			`set[0].name: "Content-Encoding" is a field the Node drops when a Policy rewrites the body, as this one does`,
			`remove[0].name: "Digest" is a field the Node drops when a Policy rewrites the body, as this one does`,
			`remove[1].name: "content-md5" is a field the Node drops when a Policy rewrites the body, as this one does`,
			`remove[2].name: "Repr-Digest" is a field the Node drops when a Policy rewrites the body, as this one does`,
			`remove[3].name: "content-digest" is a field the Node drops when a Policy rewrites the body, as this one does`,
		}},
		{"body fields without a body rewrite", &v1alpha1.TransformResponseConfig{
			Remove: []v1alpha1.TransformResponseRemove{respRemove("header", "content-encoding"), respRemove("header", "digest")},
		}, nil},
		{"query names", &v1alpha1.TransformRequestConfig{
			Set:    []v1alpha1.TransformRequestSet{reqSet("query", "page[size]", `"1"`)},
			Remove: []v1alpha1.TransformRequestRemove{reqRemove("query", "")},
		}, []want{
			`set[0].name: query field name is not an RFC 9110 token: byte '[' at offset 4`,
			`remove[0].name: query field name is empty`,
		}},
		{"query names are not managed names", &v1alpha1.TransformRequestConfig{
			Set: []v1alpha1.TransformRequestSet{reqSet("query", "host", `"1"`), reqSet("query", "content-length", `"1"`)},
		}, nil},
		{"dot paths", &v1alpha1.TransformRequestConfig{
			Set:      []v1alpha1.TransformRequestSet{reqSet("body", "a..b", "1")},
			Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", `a\b`), reqRemove("body", ""), reqRemove("body", "items.a*")},
			ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "a.", "b"), arrayOp("append", "a", `b\`)},
			Replace:  []v1alpha1.Replace{pattern(".a", "x", "y")},
		}, []want{
			`set[0].name: empty dot path segment at byte 2`,
			`remove[0].name: invalid dot path escape at byte 1`,
			`remove[1].name: empty dot path segment`,
			`remove[2].name: unescaped * inside a dot path segment at byte 6`,
			`arrayOps[0].from: empty dot path segment at byte 2`,
			`arrayOps[1].to: invalid dot path escape at byte 1`,
			`replace[0].path: empty dot path segment at byte 0`,
		}},
		{"path target", &v1alpha1.TransformRequestConfig{
			Set:    []v1alpha1.TransformRequestSet{reqSet("path", "p", `"/a"`), reqSet("path", "", `"/b"`)},
			Remove: []v1alpha1.TransformRequestRemove{reqRemove("path", "")},
		}, []want{
			`set[0].name: target path takes no name`,
			`set[1].target: a second set entry with target path; a Policy rewrites the path once`,
			`remove[0].target: target path cannot be removed; a set entry rewrites it`,
		}},
		{"empty valueExpression", &v1alpha1.TransformRequestConfig{
			Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", "")},
		}, []want{`set[0].valueExpression: valueExpression is empty`}},
		{"array operations", &v1alpha1.TransformResponseConfig{
			ArrayOps: []v1alpha1.ArrayOp{
				arrayOp("move", "", ""), arrayOp("append", "a", ""), arrayOp("move", "a", "b.*"), arrayOp("append", "a.*", `b.\*`),
				arrayOp("delete", "", "x"), arrayOp("copy", "a", "b"),
			},
		}, []want{
			`arrayOps[0].from: move needs from and to`,
			`arrayOps[0].to: move needs from and to`,
			`arrayOps[1].to: append needs from and to`,
			`arrayOps[2].to: to holds a * segment; an operation writes one place`,
			`arrayOps[4].from: delete needs from`,
			`arrayOps[4].to: delete takes no to`,
			`schema arrayOps[5].op: op "copy" is not move, append or delete`,
		}},
		{"patterns", &v1alpha1.TransformRequestConfig{
			Replace: []v1alpha1.Replace{pattern("", long, ""), pattern("", wide, ""), pattern("", wideLong, ""), pattern("", "a(b", ""), literally("", "a(b", ""), literally("", long, "")},
		}, []want{
			`schema replace[0].pattern: pattern is 1025 bytes, more than 1024`,
			`replace[1].pattern: pattern is 1026 bytes, more than 1024`,
			`schema replace[2].pattern: pattern is 2050 bytes, more than 1024`,
			"replace[3].pattern: pattern is not valid RE2: missing closing ): `a(b`",
			`schema replace[5].pattern: pattern is 1025 bytes, more than 1024`,
		}},
		{"unknown request targets", &v1alpha1.TransformRequestConfig{
			Set:    []v1alpha1.TransformRequestSet{reqSet("cookie", "a", `"b"`)},
			Remove: []v1alpha1.TransformRequestRemove{reqRemove("cookie", "a")},
		}, []want{
			`schema set[0].target: target "cookie" is not header, query, body or path`,
			`schema remove[0].target: target "cookie" is not header, query or body`,
		}},
		{"response targets", &v1alpha1.TransformResponseConfig{
			Set:    []v1alpha1.TransformResponseSet{respSet("query", "a", `"b"`), respSet("path", "", `"/b"`)},
			Remove: []v1alpha1.TransformResponseRemove{respRemove("query", "a")},
		}, []want{
			`schema set[0].target: target "query" is not header or body`,
			`schema set[1].target: target "path" is not header or body`,
			`schema remove[0].target: target "query" is not header or body`,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingTexts(t, tc.cfg); !slices.Equal(got, tc.want) {
				t.Errorf("findings\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// TestCheckMediaType covers the contentType rule: an RFC 9110 media type
// that a field value may hold.
func TestCheckMediaType(t *testing.T) {
	for _, ok := range []string{
		"application/json", "text/xml", "application/problem+json", "text/plain;charset=utf-8",
		"text/plain ; charset=utf-8", "text/plain;;charset=utf-8", "text/plain;", `multipart/form-data; boundary="a b;c\"d"`,
		"a/b; x=y; z=\"\"",
	} {
		if err := checkMediaType(ok); err != nil {
			t.Errorf("checkMediaType(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "text", "text/", "/json", "te xt/plain", "text/plain; charset", "text/plain; =utf-8", "text/plain; charset=",
		"text/plain; charset=\"utf-8", "text/plain; charset=utf 8", "text/plain x", " text/plain", "text/plain ",
		"text/plain\x00", "text/plain; a=\"\x01\"", "text/plain; a=\"\\\x01\"", "text/plain,text/html",
		strings.Repeat("a", 8<<10) + "/b",
	} {
		if err := checkMediaType(bad); err == nil {
			t.Errorf("checkMediaType(%q) = nil, want an error", bad)
		}
	}
}

// TestReport covers the offline check: each finding but the schema's is
// RZ-CFG-005 at its path under spec.config with its location, a second
// path entry with the first as its related location, and a resource
// without a decoded transform config yields nothing.
func TestReport(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set:    []v1alpha1.TransformRequestSet{reqSet("path", "", `"/a"`), reqSet("cookie", "a", `"b"`), reqSet("path", "", `"/b"`)},
		Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "host")},
	}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "transform-test"}, Config: cfg}
	loc := func(p diag.Path) diag.Location { return diag.Location{File: "p.yaml", Line: len(p), Column: 1} }
	var got diag.List
	Report(r, RequestSpec(cfg), loc, func(d diag.Diagnostic) { got = append(got, d) })
	type row struct {
		code, path, message string
		line                int
		related             []diag.Related
	}
	var rows []row
	for _, d := range got {
		rows = append(rows, row{d.Code, d.Path.String(), d.Message, d.Location.Line, d.Related})
	}
	wantRows := []row{
		{CodeConfig, "spec.config.set[2].target", "a second set entry with target path; a Policy rewrites the path once", 5,
			[]diag.Related{{Location: diag.Location{File: "p.yaml", Line: 5, Column: 1}, Message: "first entry at"}}},
		{CodeConfig, "spec.config.remove[0].name", "header host is managed by the Node (host)", 5, nil},
	}
	if len(rows) != len(wantRows) {
		t.Fatalf("Report = %+v, want %+v", rows, wantRows)
	}
	for i := range rows {
		g, w := rows[i], wantRows[i]
		if g.code != w.code || g.path != w.path || g.message != w.message || g.line != w.line || !slices.Equal(g.related, w.related) {
			t.Errorf("diagnostic %d = %+v, want %+v", i, g, w)
		}
	}
	if got[0].Resource == nil || got[0].Resource.Name != "transform-test" {
		t.Errorf("Resource = %+v", got[0].Resource)
	}
	var unlocated diag.List
	Report(r, RequestSpec(cfg), nil, func(d diag.Diagnostic) { unlocated = append(unlocated, d) })
	if len(unlocated) != 2 || unlocated[0].Related != nil || unlocated[0].Location != (diag.Location{}) {
		t.Errorf("Report without loc = %+v", unlocated)
	}
	Report(nil, RequestSpec(cfg), loc, func(diag.Diagnostic) { t.Error("a nil resource reported") })
	Report(r, RequestSpec(cfg), loc, nil)
}

// schemaDiagnostics validates the JSON Policy document src against the
// embedded rendered schema (stage F) and returns its findings.
func schemaDiagnostics(t *testing.T, src string) diag.List {
	t.Helper()
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatalf("schemaidx.Embedded: %v", err)
	}
	set, err := schemaview.Embedded(idx)
	if err != nil {
		t.Fatalf("schemaview.Embedded: %v", err)
	}
	files := &tree.FileTable{}
	file := files.Add(tree.File{Path: "policy.json"})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), file, "policy.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) != 0 || len(docs) != 1 {
		t.Fatalf("profile.Parse = %d documents, %v, %v", len(docs), ds, err)
	}
	res := &tree.Resource{
		ID:         tree.ID{Kind: v1alpha1.KindPolicy, Name: "transform-test"},
		APIVersion: "ruralz/v1alpha1", Root: docs[0].Root, Start: docs[0].Start,
	}
	out, ok := set.Validate(res, files)
	if !ok {
		t.Fatal("ruralz/v1alpha1 is not served")
	}
	return out
}

// TestSchemaReportsWhatCheckLeavesOut covers the findings Report leaves to
// the schema (stage F): a target or op outside its enum and a pattern over
// 1,024 characters; and the members the schema requires per target or op,
// which Report checks again for a present but empty member.
func TestSchemaReportsWhatCheckLeavesOut(t *testing.T) {
	long := strings.Repeat("a", MaxPatternBytes+1)
	tests := []struct {
		name, typ, config, at string
	}{
		{"unknown set target", "transform.request", `{"set": [{"target": "cookie", "name": "a", "valueExpression": "1"}]}`, "spec.config.set[0].target"},
		{"response query target", "transform.response", `{"set": [{"target": "query", "name": "a", "valueExpression": "1"}]}`, "spec.config.set[0].target"},
		{"unknown op", "transform.request", `{"arrayOps": [{"op": "copy", "from": "a", "to": "b"}]}`, "spec.config.arrayOps[0].op"},
		{"long pattern", "transform.response", `{"replace": [{"pattern": "` + long + `"}]}`, "spec.config.replace[0].pattern"},
		{"header without name", "transform.request", `{"set": [{"target": "header", "valueExpression": "1"}]}`, "spec.config.set[0]"},
		{"move without to", "transform.response", `{"arrayOps": [{"op": "move", "from": "a"}]}`, "spec.config.arrayOps[0]"},
		{"delete without from", "transform.request", `{"arrayOps": [{"op": "delete"}]}`, "spec.config.arrayOps[0]"},
		{"empty valueExpression", "transform.request", `{"set": [{"target": "path", "valueExpression": ""}]}`, "spec.config.set[0].valueExpression"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "transform-test"},` +
				`"spec": {"type": "` + tc.typ + `", "config": ` + tc.config + `}}`
			ds := schemaDiagnostics(t, src)
			found := false
			for _, d := range ds {
				if d.Code == CodeConfig && strings.HasPrefix(d.Path.String(), tc.at) {
					found = true
				}
			}
			if !found {
				t.Errorf("schema findings %v, want RZ-CFG-005 at %s", ds, tc.at)
			}
		})
	}
	valid := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "transform-test"},` +
		`"spec": {"type": "transform.request", "config": {"set": [{"target": "path", "valueExpression": "\"/a\""}],` +
		`"arrayOps": [{"op": "delete", "from": "a.0"}], "replace": [{"pattern": "a", "literal": true}]}}}`
	if ds := schemaDiagnostics(t, valid); len(ds) != 0 {
		t.Errorf("schema findings of a valid config: %v", ds)
	}
}
