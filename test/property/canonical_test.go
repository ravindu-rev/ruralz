// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// FuzzCanonicalForm is the Canonical form row of the Required properties:
// reordering files or map and set lists, or reformatting, keeps the
// Revision, and YAML and JSON give one digest
// (docs/architecture/02-configuration-model.md "Canonical form and
// Revision"). For a Bundle of the golden corpus, or one the fuzzer derives
// from it, every render (without --env, then each Environment of its
// environments file) builds the same Revision, or fails alike, after its
// base documents move to other files under other names, the members of
// every mapping and the entries of every map and set list are shuffled,
// and each file takes another layout: block YAML with comments, blank
// lines and document markers, flow YAML, or one JSON document per file.
// A second variant writes every document as JSON: the Gateway as JSON text
// in ruralz.yaml and each other document, overlays included, as its own
// .json file.
func FuzzCanonicalForm(f *testing.F) {
	addBundleSeeds(f, 1)
	f.Fuzz(func(t *testing.T, archive, envs []byte, seed uint64) {
		tc := tools(t)
		m := newMemBundle(corpus.ParseArchive(archive).Files, envs)
		renders := tc.renders(t, m)
		want := tc.digests(t, m, renders)
		if !slices.ContainsFunc(want, func(d string) bool { return d != "" }) {
			t.Skip("no render of the Bundle builds: the negative corpus covers invalid input")
		}
		src, ok := tc.parseSource(t, m)
		if !ok {
			t.Skip("stages A to C refuse the Bundle")
		}
		r := newRand(seed)
		for _, v := range []struct {
			name  string
			files map[string][]byte
		}{
			{"reordered and reformatted", reshape(t, tc, r, src, false)},
			{"every document as JSON", reshape(t, tc, r, src, true)},
		} {
			got := tc.digests(t, m.withFiles(v.files), renders)
			if !slices.Equal(got, want) {
				t.Fatalf("%s: the renders %q build %q, want %q\n%s", v.name, renders, got, want, dumpFiles(v.files))
			}
		}
	})
}

// reshape returns src's documents as another Bundle of the same
// resources: each document shuffled (shuffleDoc), the base documents
// spread over randomly named files with the Gateway in ruralz.yaml, the
// documents of each overlay over files of that overlay, each file in a
// random layout (styled). With allJSON, ruralz.yaml holds the Gateway as
// JSON text and every other document is its own .json file.
func reshape(t *testing.T, tc *toolchain, r *rand.Rand, src []sourceFile, allJSON bool) map[string][]byte {
	t.Helper()
	var root, base []*tree.Node
	overlays := map[string][]*tree.Node{}
	var overlayNames []string
	for _, sf := range src {
		for _, d := range sf.docs {
			d = d.Clone()
			shuffleDoc(tc, r, d)
			switch {
			case sf.overlay != "":
				if _, ok := overlays[sf.overlay]; !ok {
					overlayNames = append(overlayNames, sf.overlay)
				}
				overlays[sf.overlay] = append(overlays[sf.overlay], d)
			case stringAt(d, "kind") == "Gateway":
				root = append(root, d)
			default:
				base = append(base, d)
			}
		}
	}
	files := map[string][]byte{}
	if allJSON {
		files["ruralz.yaml"] = flowYAML(t, root)
		for i, d := range base {
			files[fmt.Sprintf("json/%03d.json", i)] = jsonOf(t, d)
		}
		for _, name := range overlayNames {
			for i, d := range overlays[name] {
				files[fmt.Sprintf("overlays/%s/%03d.json", name, i)] = jsonOf(t, d)
			}
		}
		return files
	}
	r.Shuffle(len(base), func(i, j int) { base[i], base[j] = base[j], base[i] })
	k := r.IntN(len(base) + 1)
	rootDocs := slices.Concat(root, base[:k])
	r.Shuffle(len(rootDocs), func(i, j int) { rootDocs[i], rootDocs[j] = rootDocs[j], rootDocs[i] })
	files["ruralz.yaml"] = styled(t, r, rootDocs, false)
	spread(t, r, files, "p", base[k:])
	for _, name := range overlayNames {
		spread(t, r, files, "overlays/"+name+"/", overlays[name])
	}
	return files
}

// spread writes docs, in random order, into a random number of files whose
// names start with prefix, each in a random layout; a file holding one
// document may be a .json file.
func spread(t *testing.T, r *rand.Rand, files map[string][]byte, prefix string, docs []*tree.Node) {
	t.Helper()
	if len(docs) == 0 {
		return
	}
	n := 1 + r.IntN(len(docs))
	groups := make([][]*tree.Node, n)
	for _, d := range docs {
		g := r.IntN(n)
		groups[g] = append(groups[g], d)
	}
	for i, g := range groups {
		if len(g) == 0 {
			continue
		}
		name := prefix
		if !strings.HasSuffix(prefix, "/") {
			name += strconv.Itoa(r.IntN(3)) + "/"
		}
		name += pick(r, []string{"a", "routes", "x-y", "z_9", "Mixed.Case"}) + "-" + strconv.Itoa(i)
		switch {
		case len(g) == 1 && r.IntN(3) == 0:
			files[name+".json"] = jsonOf(t, g[0])
		case r.IntN(2) == 0:
			files[name+".yml"] = styled(t, r, g, true)
		default:
			files[name+".yaml"] = styled(t, r, g, true)
		}
	}
}

// styled writes docs as one YAML stream in a random layout: block style
// with comments, blank lines and an opening document marker, flow style
// (each document as JSON text), or a document by document mix.
func styled(t *testing.T, r *rand.Rand, docs []*tree.Node, mayDecorate bool) []byte {
	t.Helper()
	switch r.IntN(3) {
	case 0:
		return flowYAML(t, docs)
	case 1:
		var parts [][]byte
		for _, d := range docs {
			if r.IntN(2) == 0 {
				parts = append(parts, yamlSafe(jsonOf(t, d)))
			} else {
				parts = append(parts, decorate(r, yamlOf(t, []*tree.Node{d}), mayDecorate))
			}
		}
		return bytes.Join(parts, []byte("---\n"))
	default:
		return decorate(r, yamlOf(t, docs), mayDecorate)
	}
}

// flowYAML writes docs as a YAML stream of flow documents: each one JSON
// text, which the restricted profile reads as YAML (yamlSafe).
func flowYAML(t *testing.T, docs []*tree.Node) []byte {
	t.Helper()
	var parts [][]byte
	for _, d := range docs {
		parts = append(parts, yamlSafe(jsonOf(t, d)))
	}
	return bytes.Join(parts, []byte("---\n"))
}

// decorate adds comments, blank lines and an opening document marker to a
// block YAML stream the profile encoder wrote, whose every line is whole:
// a comment after any line, a comment line or a blank line between lines.
func decorate(r *rand.Rand, text []byte, on bool) []byte {
	if !on && r.IntN(2) == 0 {
		return text
	}
	var b bytes.Buffer
	if r.IntN(2) == 0 && !bytes.HasPrefix(text, []byte("---")) {
		b.WriteString("--- # starts the stream\n")
	}
	for line := range bytes.Lines(text) {
		switch r.IntN(8) {
		case 0:
			b.WriteString("# a comment line\n")
		case 1:
			b.WriteString("\n")
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		b.Write(line)
		if r.IntN(4) == 0 {
			b.WriteString("   # trailing: comment, with ${NOT} a \"value\"")
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// shuffleDoc shuffles the members of every mapping of a document and the
// entries of every map and set list its apiVersion's schema declares
// (x-ruralz-list), leaving orderedMap, atomic and untyped lists in order.
func shuffleDoc(tc *toolchain, r *rand.Rand, doc *tree.Node) {
	idx, ok := tc.pipe.Schemas().Index(stringAt(doc, "apiVersion"))
	if !ok || idx.Walk(stringAt(doc, "kind"), doc, func(c *schemaidx.Cursor) error {
		shuffleNode(r, c.Node, c.Info.Node)
		return nil
	}) != nil {
		shuffleMembers(r, doc)
	}
}

// shuffleNode shuffles one node in place: a mapping's members, and a map
// or set list's entries by its schema.
func shuffleNode(r *rand.Rand, n *tree.Node, schema *schemaidx.Node) {
	switch n.Kind {
	case tree.KindMap:
		r.Shuffle(len(n.Members), func(i, j int) { n.Members[i], n.Members[j] = n.Members[j], n.Members[i] })
	case tree.KindList:
		if schema == nil {
			return
		}
		if lt, _ := schema.List(); lt == schemaidx.ListMap || lt == schemaidx.ListSet {
			r.Shuffle(len(n.Items), func(i, j int) { n.Items[i], n.Items[j] = n.Items[j], n.Items[i] })
		}
	default:
	}
}

// shuffleMembers shuffles the members of every mapping under n.
func shuffleMembers(r *rand.Rand, n *tree.Node) {
	switch n.Kind {
	case tree.KindMap:
		shuffleNode(r, n, nil)
		for _, m := range n.Members {
			shuffleMembers(r, m.Value)
		}
	case tree.KindList:
		for _, it := range n.Items {
			shuffleMembers(r, it)
		}
	default:
	}
}

// stringAt returns the string member key of n, or "".
func stringAt(n *tree.Node, key string) string {
	if n == nil || n.Kind != tree.KindMap {
		return ""
	}
	if v, ok := n.Get(key); ok && v.Kind == tree.KindString {
		return v.Text
	}
	return ""
}

// dumpFiles returns files as text for a failure message, by sorted path.
func dumpFiles(files map[string][]byte) string {
	var names []string
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "-- %s --\n%s", name, files[name])
	}
	return b.String()
}
