// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// fuzzSource returns a Bundle root whose Gateway carries literal, escaped
// so substitution reads it back as authored, in one annotation; ${VALUE}
// alone and inside other text in two more; and ${PORT} as its listener
// port, which substitution re-types when it is an integer. It is written
// verbatim, as an author would write it.
func fuzzSource(t *testing.T, literal string) []byte {
	t.Helper()
	root := resource(t, &tree.FileTable{}, "ruralz.yaml", `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
  annotations:
    shop.example/literal: x
    shop.example/value: "${VALUE}"
    shop.example/mixed: "a-${VALUE:-none}-b $${HOME}"
spec:
  listeners:
    - name: http
      protocol: http
      port: ${PORT}
`).Root
	meta, _ := root.Get("metadata")
	ann, _ := meta.Get("annotations")
	ann.Set("shop.example/literal", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: subst.Escape(literal)})
	var buf bytes.Buffer
	if err := profile.EncodeWith(&buf, []*tree.Node{root}, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzRenderIdempotent is the Render property of the testing strategy:
// rendering a rendered Bundle again yields the same Revision, including
// $${ escapes. For any literal text authored with escapes, any variable
// value substituted into it and any port value, a source Bundle that
// builds renders to a Bundle that loads as file mode loads it, builds the
// source's Revision and renders to the same bytes.
func FuzzRenderIdempotent(f *testing.F) {
	for _, seed := range [][3]string{
		{"plain", "v", "8080"},
		{"costs ${PRICE} and $${X}", "a${b}c $${d}", "8443"},
		{"multi\nline\ttab \"quoted\" \\ back", "\"q\" ${", "0x1F90"},
		{"yes", "null", "1e3"},
		{"", "", "9901"},
		{"é ✓ \u2028 \u0085 \ufeff", "${VALUE}", "08080"},
		{"- not: [a list]", "{a: b}", "true"},
		{"${", "}", "-1"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, literal, value, port string) {
		if !utf8.ValidString(literal) || !utf8.ValidString(value) || !utf8.ValidString(port) {
			t.Skip("the restricted profile reads UTF-8 only")
		}
		src := pipeline(t, []source{{"ruralz.yaml", fuzzSource(t, literal)}}, nil, subst.Map{"VALUE": value, "PORT": port})
		if src.validated == nil {
			t.Skip("the source Bundle does not build")
		}
		first := renderYAML(t, src)
		again := pipeline(t, []source{{"ruralz.yaml", first}}, nil, subst.Map{})
		if again.validated == nil {
			t.Fatalf("the rendered Bundle does not load:\n%s\n%s", diagText(t, again.diags), first)
		}
		if again.validated.Revision.Digest != src.validated.Revision.Digest {
			t.Fatalf("the rendered Bundle builds %s, the source %s:\n%s", again.validated.Revision.Digest.Short(), src.validated.Revision.Digest.Short(), first)
		}
		if second := renderYAML(t, again); !bytes.Equal(second, first) {
			t.Fatalf("the second render differs:\n--- first\n%s--- second\n%s", first, second)
		}
	})
}
