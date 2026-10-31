// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// update rewrites the golden files: go test -run Golden -update.
var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// exampleBundle returns the files of a Bundle the golden tests encode:
// shop and shop-json, the base files of the golden corpus's example Bundle
// and of its JSON twin (shopFiles), or a directory under testdata/bundles,
// the minimal Bundle and its twin.
func exampleBundle(t testing.TB, name string) []bundleFile {
	t.Helper()
	switch name {
	case "shop":
		return shopFiles(t, corpus.BundleDir)
	case "shop-json":
		return shopFiles(t, corpus.BundleJSONDir)
	}
	files := dirFiles(t, filepath.Join("testdata", "bundles", name))
	if len(files) == 0 {
		t.Fatalf("no configuration files in testdata/bundles/%s", name)
	}
	return files
}

// TestGoldenExampleBundles pins the canonical bytes and digests of the
// encoder over the parsed trees of the example Bundles: the configuration
// model's example Bundle (shop, the base files of the golden corpus's
// shop-bundle entry with ${VAR} substituted as the render without --env
// does) and a minimal Bundle, each parsed from YAML and from a JSON twin,
// the minimal one's with other member orders, which yield identical
// bytes. The trees are the parsed ones, before stage G materializes
// defaults and normalizes scalars, so the files are named *.parsed.json
// and *.parsed.digest: an internal form of this package, not the
// Revisions of these Bundles. The golden corpus of the pipeline's output,
// the shop-bundle Revision included, belongs to the configuration
// conformance suite (test/conformance/config). Decoding each golden
// document and encoding it again is byte-identical.
func TestGoldenExampleBundles(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{"shop", []string{"shop", "shop-json"}},
		{"minimal", []string{"minimal", "minimal-json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			golden := filepath.Join("testdata", "golden", tc.name+".parsed.json")
			digestFile := filepath.Join("testdata", "golden", tc.name+".parsed.digest")
			for i, dir := range tc.dirs {
				b := loadBundle(t, exampleBundle(t, dir))
				rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), b, Options{Workers: 2})
				if err != nil {
					t.Fatalf("%s: Encode() error = %v", dir, err)
				}
				if *update && i == 0 {
					if err := os.WriteFile(golden, rev.Content, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(digestFile, []byte(rev.Digest.String()+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden) //nolint:gosec // G304: a golden file under testdata.
				if err != nil {
					t.Fatalf("%v (run go test -run Golden -update)", err)
				}
				sameBytes(t, dir, rev.Content, want)
				wantDigest, err := os.ReadFile(digestFile) //nolint:gosec // G304: a golden file under testdata.
				if err != nil {
					t.Fatal(err)
				}
				d, err := revision.Parse(strings.TrimSuffix(string(wantDigest), "\n"))
				if err != nil || d != rev.Digest || d != revision.Sum(want) {
					t.Errorf("%s: digest %s, golden %s (%v)", dir, rev.Digest, wantDigest, err)
				}
			}
			want, err := os.ReadFile(golden) //nolint:gosec // G304: a golden file under testdata.
			if err != nil {
				t.Fatal(err)
			}
			sameBytes(t, "decode then encode", reencode(t, mustDecode(t, want)), want)
		})
	}
}

// TestShopBaseFiles holds the files shopFiles names the example Bundle's
// base to what the pipeline reads: the resources they parse to, from YAML
// and from the JSON twin, are those of the Revision of the corpus entry's
// render without --env, which reads the base files alone, so no file a
// render leaves out is encoded and none it reads is missing.
func TestShopBaseFiles(t *testing.T) {
	content, ok := shopLevel(t).Canonical["-"]
	if !ok {
		t.Fatalf("%s has no canonical form of the render without --env", shopEntry)
	}
	var want []string
	for _, r := range mustDecode(t, content) {
		want = append(want, r.ID.String())
	}
	slices.Sort(want)
	for _, dir := range []string{corpus.BundleDir, corpus.BundleJSONDir} {
		var got []string
		for _, r := range parseFiles(t, shopFiles(t, dir), &tree.FileTable{}) {
			got = append(got, r.ID.String())
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s/%s base files hold\n%q\nthe render without --env holds\n%q", shopEntry, dir, got, want)
		}
	}
}
