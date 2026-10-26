// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stageG returns stage G over the embedded schemas and the Policy type
// registry, as the pipeline builds it.
func stageG(t *testing.T) *defaults.Stage {
	t.Helper()
	set, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	s, err := defaults.New(defaults.Options{Schemas: set, Registry: registry.New()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// parseResources parses every source of a testdata directory with the
// restricted profile into envelope trees, as stages A to F hand them to
// stage G, registering each file in files with RoleBase.
func parseResources(t *testing.T, dir string, files *tree.FileTable) []*tree.Resource {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	var rs []*tree.Resource
	for _, name := range names {
		f, ok := profile.FormatOf(name)
		if !ok {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: a file under testdata.
		if err != nil {
			t.Fatal(err)
		}
		id := files.Add(tree.File{Path: filepath.ToSlash(name), Role: tree.RoleBase})
		docs, ds, err := profile.Parse(context.Background(), src, id, name, f, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("Parse(%s) = %v, %v", name, ds, err)
		}
		for _, d := range docs {
			k, _ := d.Root.Get("kind")
			meta, _ := d.Root.Get("metadata")
			n, _ := meta.Get("name")
			av, _ := d.Root.Get("apiVersion")
			rs = append(rs, &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: n.Text}, APIVersion: av.Text, Root: d.Root, Start: d.Start})
		}
	}
	return rs
}

// TestStageGRoundTrip runs the real stage G on Decode output, as
// pipeline.FromResources does with a Last-Known-Good or handover candidate
// (Materialize(Decode(x)) == Decode(x)), not only a synthetic
// materialization. For the example Bundle, its JSON twin and the minimal
// Bundle: stage G, then Encode, gives a Revision; Verify of it, with one
// worker and with four, into a file table whose first file is a base file,
// re-encodes to the same bytes, and stage G again on the decoded trees
// gives a Bundle whose resources carry no source file (their positions name
// the tree.RoleCanonical file Decode registered, which also exempts them
// from the integer range rule) and which encodes to the same bytes and
// digest.
func TestStageGRoundTrip(t *testing.T) {
	ctx := context.Background()
	g := stageG(t)
	enc := NewEncoder(hubIndex(t))
	for _, dir := range []string{"shop", "shop-json", "minimal"} {
		files := &tree.FileTable{}
		b, ds, err := g.Run(ctx, parseResources(t, filepath.Join("testdata", "bundles", dir), files), files, 2)
		if err != nil || len(ds) > 0 {
			t.Fatalf("%s: stage G: %v, %v", dir, ds, err)
		}
		rev, err := enc.Encode(ctx, b, Options{Workers: 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int{1, 4} {
			lkg := &tree.FileTable{}
			lkg.Add(tree.File{Path: "base.yaml", Role: tree.RoleBase})
			rs, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w}).Verify(ctx, rev.Content, rev.Digest, Source{Name: "lkg.json", Files: lkg})
			if err != nil {
				t.Fatalf("%s, workers %d: Verify: %v", dir, w, err)
			}
			again, err := enc.EncodeResources(ctx, rs, Options{Workers: w})
			if err != nil {
				t.Fatal(err)
			}
			sameBytes(t, dir+": decode then encode", again.Content, rev.Content)
			ptrs := make([]*tree.Resource, len(rs))
			for i := range rs {
				ptrs[i] = &rs[i]
				if role := lkg.File(rs[i].Start.File).Role; role != tree.RoleCanonical {
					t.Errorf("%s, workers %d: %s starts in a file of role %v", dir, w, rs[i].ID, role)
				}
			}
			b2, ds, err := g.Run(ctx, ptrs, lkg, w)
			if err != nil || len(ds) > 0 {
				t.Fatalf("%s, workers %d: stage G on the decoded trees: %v, %v", dir, w, ds, err)
			}
			for _, r := range b2.Resources() {
				if r.Source.File != "" {
					t.Errorf("%s, workers %d: %s has source file %q, want none for canonical content", dir, w, r.ID, r.Source.File)
				}
			}
			rev2, err := enc.Encode(ctx, b2, Options{Workers: w})
			if err != nil {
				t.Fatal(err)
			}
			sameBytes(t, dir+": stage G on the decoded trees", rev2.Content, rev.Content)
			if rev2.Digest != rev.Digest {
				t.Errorf("%s, workers %d: digest %s, want %s", dir, w, rev2.Digest, rev.Digest)
			}
		}
	}
}
