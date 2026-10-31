// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// examplesDir is the repository's examples/ directory, read in place: the
// golden corpus of test/conformance/config holds the outputs, so this
// package keeps no copy of an example Bundle or of its outputs
// (docs/engineering/03-testing-and-quality-strategy.md "Golden tests").
const examplesDir = "../../../examples"

// TestExampleBundlesRunEveryStage runs every example Bundle through every
// stage, without --env and for each Environment of examples/control: no
// diagnostic, warnings included, a Revision, the same digest for one and
// many workers, and canonical re-entry of the Revision to the same digest.
func TestExampleBundlesRunEveryStage(t *testing.T) {
	envs := loader.Source{Path: filepath.Join(examplesDir, "control", "environments.yaml")}
	for _, name := range []string{"shop-bundle", "shop-rest-bundle", "quickstart"} {
		dir := filepath.Join(examplesDir, name)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("example %s: %v", name, err)
		}
		for _, env := range []string{"", "staging", "prod"} {
			t.Run(name+"/"+env, func(t *testing.T) {
				req := Request{Source: loader.Source{Path: dir}, Environment: env}
				if env != "" {
					req.Environments = envs
				}
				var digests []string
				for _, workers := range []int{1, 4} {
					p := newPipeline(t, Options{Workers: workers})
					res, err := p.Run(t.Context(), req)
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					wantClean(t, "run", res.Diagnostics)
					if res.Validated == nil {
						t.Fatal("no Validated for a clean run")
					}
					digests = append(digests, res.Validated.Revision.Digest.String())
					back, err := p.FromCanonical(t.Context(), CanonicalRequest{
						Content: res.Validated.Revision.Content, Digest: res.Validated.Revision.Digest, Name: "lkg.json",
					})
					if err != nil {
						t.Fatalf("FromCanonical: %v", err)
					}
					wantClean(t, "canonical re-entry", back.Diagnostics)
					if back.Validated == nil || back.Validated.Revision.Digest != res.Validated.Revision.Digest {
						t.Errorf("canonical re-entry: Validated %v, want digest %s", back.Validated, res.Validated.Revision.Digest)
					}
				}
				if digests[0] != digests[1] {
					t.Errorf("digest with 1 worker %s, with 4 workers %s", digests[0], digests[1])
				}
			})
		}
	}
}
