// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// validRevision returns the Revision of validBundle with methods on Route
// a, so its canonical form holds a set.
func validRevision(t *testing.T, p *Pipeline) revision.Revision {
	t.Helper()
	files := validBundle()
	files["routes.yaml"] = strings.Replace(files["routes.yaml"], "path: {prefix: /a}", "path: {prefix: /a}\n    methods: [POST, GET]", 1)
	res := run(t, p, bundle(files), Request{})
	if res.Validated == nil {
		t.Fatalf("no Validated: %q", texts(res.Diagnostics))
	}
	return res.Validated.Revision
}

// TestFromCanonicalRoundTrip covers canonical re-entry of a Revision: the
// content verified against its digest runs stages F to M again and yields
// the same Revision, chains, secret uses and CEL sites, with positions in
// the content file.
func TestFromCanonicalRoundTrip(t *testing.T) {
	p := newPipeline(t, Options{})
	src := run(t, p, bundle(validBundle()), Request{})
	rev := src.Validated.Revision
	res, err := p.FromCanonical(t.Context(), CanonicalRequest{Content: rev.Content, Digest: rev.Digest, Name: "lkg/content.json"})
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}
	wantClean(t, "re-entry", res.Diagnostics)
	v := res.Validated
	if v == nil || v.Revision.Digest != rev.Digest || !bytes.Equal(v.Revision.Content, rev.Content) {
		t.Fatalf("Validated %v, want the content's own Revision", v)
	}
	if !slices.Equal(sortedKeys(v.Chains), sortedKeys(src.Validated.Chains)) || len(v.Secrets) != len(src.Validated.Secrets) ||
		len(v.CEL) != len(src.Validated.CEL) || siteKey(v.CEL[0].Site) != siteKey(src.Validated.CEL[0].Site) {
		t.Errorf("re-entry chains %v, %d secrets, %d CEL uses; want those of the source run", sortedKeys(v.Chains), len(v.Secrets), len(v.CEL))
	}
	if loc := v.CEL[0].Loc; loc.File != "lkg/content.json" || loc.Line != 1 {
		t.Errorf("CEL location %+v, want a position in the content file", loc)
	}
	if r := v.Bundle.Resources()[0]; r.Source.File != "" {
		t.Errorf("canonical resource source file %q, want none", r.Source.File)
	}
}

// TestFromCanonicalDecodeFailures covers content the decoder refuses: a
// digest it does not hash to (RZ-CFG-027), content from a newer producer
// (RZ-CFG-024), and content past the source limit (RZ-CFG-001); each
// stops the run without a Revision.
func TestFromCanonicalDecodeFailures(t *testing.T) {
	p := newPipeline(t, Options{})
	rev := validRevision(t, p)
	small := newPipeline(t, Options{Limits: loader.Limits{MaxSourceBytes: 40}})
	tests := []struct {
		name string
		p    *Pipeline
		req  CanonicalRequest
		want string
	}{
		{"digest mismatch", p, CanonicalRequest{Content: rev.Content, Digest: revision.Sum([]byte("other")), Name: "c.json"}, "RZ-CFG-027"},
		{"newer producer", p, CanonicalRequest{Content: []byte(`{"format":"ruralz.canonical.v9","resources":[]}`), Name: "c.json"}, "RZ-CFG-024"},
		{"source limit", small, CanonicalRequest{Content: rev.Content, Name: "c.json"}, "RZ-CFG-001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.p.FromCanonical(t.Context(), tc.req)
			if err != nil {
				t.Fatalf("FromCanonical: %v", err)
			}
			if !res.Stopped || res.Validated != nil || len(res.Diagnostics) == 0 || res.Diagnostics[0].Code != tc.want {
				t.Errorf("Stopped %v, %q; want %s", res.Stopped, texts(res.Diagnostics), tc.want)
			}
		})
	}
	res, _ := small.FromCanonical(t.Context(), CanonicalRequest{Content: rev.Content, Name: "c.json"})
	wantTexts(t, "source limit", res.Diagnostics, "c.json:1:41 error RZ-CFG-001: the canonical content passes the limit of 40 bytes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.FromCanonical(ctx, CanonicalRequest{Content: rev.Content}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v, want context.Canceled", err)
	}
}

// TestFromResourcesRecomputedBytes covers RZ-CFG-027 for content in
// canonical form whose recomputed canonical bytes differ: a default it
// leaves out, or a set out of order, at the first byte that differs,
// naming the resource that holds it.
func TestFromResourcesRecomputedBytes(t *testing.T) {
	p := newPipeline(t, Options{})
	rev := validRevision(t, p)
	content := string(rev.Content)
	tests := []struct {
		name, old, new, resource string
	}{
		{"default left out", `,"weight":1}],"protocol":"http"}},{"kind":"Upstream","metadata":{"name":"secure"}`, `}],"protocol":"http"}},{"kind":"Upstream","metadata":{"name":"secure"}`, "Upstream/orders"},
		{"set out of order", `"methods":["GET","POST"]`, `"methods":["POST","GET"]`, "Route/a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(content, tc.old) {
				t.Fatalf("content lacks %s: %s", tc.old, content)
			}
			changed := strings.Replace(content, tc.old, tc.new, 1)
			res, err := p.FromCanonical(t.Context(), CanonicalRequest{Content: []byte(changed), Name: "c.json"})
			if err != nil {
				t.Fatalf("FromCanonical: %v", err)
			}
			at := strings.Index(changed, tc.new) + firstDiff(tc.old, tc.new) + 1
			if res.Validated != nil || len(res.Diagnostics) != 1 {
				t.Fatalf("Validated %v, %q; want one RZ-CFG-027", res.Validated, texts(res.Diagnostics))
			}
			d := res.Diagnostics[0]
			if d.Code != CodeMismatch || d.File != "c.json" || d.Line != 1 || d.Column != at ||
				d.Resource == nil || d.Resource.Kind+"/"+d.Resource.Name != tc.resource {
				t.Errorf("diagnostic %s, want RZ-CFG-027 at c.json:1:%d on %s", d.AppendText(nil), at, tc.resource)
			}
		})
	}
}

// firstDiff returns the index of the first byte where a and b differ.
func firstDiff(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

// TestFromResourcesRunsStages covers canonical re-entry running every
// stage from F: content in canonical form whose Route names an Upstream
// the content lacks is RZ-CFG-009 at its position in the content.
func TestFromResourcesRunsStages(t *testing.T) {
	p := newPipeline(t, Options{})
	content := string(validRevision(t, p).Content)
	start := strings.Index(content, `{"kind":"Upstream","metadata":{"name":"orders"}`)
	end := strings.Index(content, `{"kind":"Upstream","metadata":{"name":"secure"}`)
	changed := content[:start] + content[end:]
	res, err := p.FromCanonical(t.Context(), CanonicalRequest{Content: []byte(changed), Name: "c.json"})
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}
	if got := codes(res.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-009"}) || res.Diagnostics[0].File != "c.json" {
		t.Errorf("diagnostics %q, want one RZ-CFG-009 in c.json", texts(res.Diagnostics))
	}
}

// TestFromResourcesDump covers a /config/dump document as a diff source:
// the resources ReadDump decodes re-enter with positions counted from the
// start of the document, the Revision is the dump's, and a recomputed
// difference is located in the document.
func TestFromResourcesDump(t *testing.T) {
	p := newPipeline(t, Options{})
	rev := validRevision(t, p)
	read := func(rev revision.Revision) (*Result, []byte) {
		t.Helper()
		body, ok := canonical.AppendDump(nil, rev, revision.Digest{})
		if !ok {
			t.Fatal("AppendDump refused the Revision")
		}
		files := &tree.FileTable{}
		dump, rs, err := p.Decoder().ReadDump(t.Context(), bytes.NewReader(body), 0, canonical.Source{Name: "dump.json", Files: files})
		if err != nil {
			t.Fatalf("ReadDump: %v", err)
		}
		res, err := p.FromResources(t.Context(), Canonical{Resources: rs, Files: files, Content: dump.Content, Text: dump.Body}, false)
		if err != nil {
			t.Fatalf("FromResources: %v", err)
		}
		return res, body
	}
	res, _ := read(rev)
	wantClean(t, "dump", res.Diagnostics)
	if res.Validated == nil || res.Validated.Revision.Digest != rev.Digest {
		t.Fatalf("Validated %v, want the dump's Revision %s", res.Validated, rev.Digest)
	}
	changed := []byte(strings.Replace(string(rev.Content), `"methods":["GET","POST"]`, `"methods":["POST","GET"]`, 1))
	res, body := read(revision.Revision{Digest: revision.Sum(changed), Content: changed})
	at := bytes.Index(body, []byte(`"methods":["POST"`)) + len(`"methods":["`) + 1
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != CodeMismatch || res.Diagnostics[0].File != "dump.json" || res.Diagnostics[0].Column != at {
		t.Errorf("diagnostics %q, want RZ-CFG-027 at dump.json:1:%d", texts(res.Diagnostics), at)
	}
}

// TestFromResourcesCanceled covers a canceled context.
func TestFromResourcesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newPipeline(t, Options{}).FromResources(ctx, Canonical{}, false); !errors.Is(err, context.Canceled) {
		t.Errorf("FromResources = %v, want context.Canceled", err)
	}
}
