// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// update rewrites the golden files: go test -run Golden -update.
var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// TestGoldenExampleBundles pins the canonical bytes and digests of the
// example Bundles (WP-39 "Done when"): the configuration model's example
// Bundle (shop, its base files with the substitution defaults applied)
// and a minimal Bundle, each parsed from YAML and from a JSON twin with
// other member orders, which yield identical bytes (02 reqs 21 to 27, 79).
// The trees are the parsed ones; the pipeline's golden corpus pins the
// output after stage G. Decoding each golden document and encoding it
// again is byte-identical (02 req 26).
func TestGoldenExampleBundles(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{"shop", []string{"shop", "shop-json"}},
		{"minimal", []string{"minimal", "minimal-json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			golden := filepath.Join("testdata", "golden", tc.name+".json")
			digestFile := filepath.Join("testdata", "golden", tc.name+".digest")
			for i, dir := range tc.dirs {
				b := loadBundle(t, filepath.Join("testdata", "bundles", dir))
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
