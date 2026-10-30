// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package config is the configuration conformance suite
// (docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites" and "Golden tests"): the golden corpus of what the configuration
// pipeline produces, the negative fixtures of every offline RZ-CFG code,
// the JSON subset, loader conformance, hostile-input, CEL cost and overlay
// cases, and the example Bundle tests.
//
// # Paths
//
// Each case runs a Bundle through the runners of the binaries that MUST
// report it (runner). The untagged cases run in process: the CLI's path,
// the ruralz bundle validate and build commands of internal/cli/bundle
// (cliPath), and ruralzd's, the configuration pipeline facade with
// checks.Table(), the CEL compiler and the Node's worker default, in file
// mode (no overlay, ${VAR} from its process environment), and the snapshot
// compiler's serve check after the digest (nodePath). The two report the
// same diagnostics for the stages both run. The corpus's outputs come from
// the facade with the CLI's options, so every comparison with them also
// holds the commands to the facade.
//
// The integration-tagged cases of later steps add the built ruralz and the
// controllable-clock ruralzd build from internal/testkit/binaries as more
// runners: each implements runner for its binary, and the same corpus runs
// through it (runner, binaryRunners).
//
// # Regeneration
//
// Only go test ./test/conformance/config -run Golden -update regenerates
// the corpus: it adds entries for changed examples and the outputs of the
// schema levels the release renders, and refuses to rewrite or delete a
// file the newest release tag holds (corpus.Held).
package config

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/diff"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// update regenerates the corpus (see the package documentation).
var update = flag.Bool("update", false, "regenerate the golden corpus: go test ./test/conformance/config -run Golden -update")

// Documents and directories of the repository the suite reads.
const (
	// configModelDoc is the configuration model: the example Bundle, the
	// REST example Bundle, the worked example, the diagnostics and the
	// diff example.
	configModelDoc = "docs/architecture/02-configuration-model.md"
	// topologiesDoc is Deployment topologies: the quickstart Bundle.
	topologiesDoc = "docs/operations/01-deployment-topologies.md"
	// examplesDir holds the example Bundles beside controlDir.
	examplesDir = "examples"
	// controlDir is the directory beside the Bundles under examples/ that
	// holds the Environment and Cluster files (configuration model "Bundle
	// layout and merge": control/ sits outside the Bundle).
	controlDir = "control"
	// environmentsName is the environments file of examples/control.
	environmentsName = "environments.yaml"
	// bundleRoot is the file that marks a Bundle root.
	bundleRoot = "ruralz.yaml"
	// noEnv names the render without --env, over an empty variable set.
	noEnv = "-"
)

// suite holds what every case shares: the corpus and the pipelines of the
// in-process paths, built once per test binary.
type suite struct {
	// root is the repository root.
	root string
	// corpus is the corpus as the test binary started.
	corpus *corpus.Corpus
	cel    *cel.Compiler
	// cli is the CLI's pipeline: checks.Table(), the CEL compiler and
	// GOMAXPROCS workers, as ruralz bundle runs it.
	cli *pipeline.Pipeline
	// node is the Node's: the same checks and compiler, and the worker
	// default of a Node's activation.
	node *pipeline.Pipeline
	// standIn and standInNode are the two with the test-only stand-ins
	// (standIns): the converter registry that deprecates an apiVersion and
	// a field, and the second served apiVersion.
	standIn, standInNode *pipeline.Pipeline
	// diff compares rendered states as ruralz bundle diff does.
	diff *diff.Engine
}

// sharedSuite builds the suite once: compiling the schema views takes a
// while.
var sharedSuite = sync.OnceValues(newSuite)

func newSuite() (*suite, error) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		return nil, err
	}
	c, err := corpus.Load(filepath.Join(root, filepath.FromSlash(corpus.Dir)))
	if err != nil {
		return nil, err
	}
	compiler, err := cel.New()
	if err != nil {
		return nil, err
	}
	s := &suite{root: root, corpus: c, cel: compiler}
	if s.cli, err = pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: compiler, Workers: runtime.GOMAXPROCS(0)}); err != nil {
		return nil, err
	}
	if s.node, err = pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: compiler}); err != nil {
		return nil, err
	}
	si, err := newStandIns()
	if err != nil {
		return nil, fmt.Errorf("stand-ins: %w", err)
	}
	if s.standIn, err = pipeline.New(si.options(compiler, runtime.GOMAXPROCS(0))); err != nil {
		return nil, err
	}
	if s.standInNode, err = pipeline.New(si.options(compiler, 0)); err != nil {
		return nil, err
	}
	if s.diff, err = diff.New(s.cli.HubIndex(), diff.Options{Workers: runtime.GOMAXPROCS(0), Registry: s.cli.Registry()}); err != nil {
		return nil, err
	}
	return s, nil
}

// mustSuite returns the shared suite, failing t when it does not build.
func mustSuite(t testing.TB) *suite {
	t.Helper()
	s, err := sharedSuite()
	if err != nil {
		t.Fatalf("build the conformance suite: %v", err)
	}
	return s
}

// path returns the absolute path of a slash path relative to the
// repository root.
func (s *suite) path(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

// corpusPath returns the absolute path of a slash path relative to the
// corpus directory.
func (s *suite) corpusPath(rel string) string { return s.path(corpus.Dir + "/" + rel) }

// levels returns the schema levels this release renders, the levels inside
// the skew window whose outputs every entry pins: the hub schema's own
// level, the one level the canonical encoder renders at
// (canonical.Options.Level).
func (s *suite) levels() []int { return []int{s.cli.HubIndex().Level()} }

// readEnvironments returns the Environments of an environments file in file
// order, failing t on any finding: the corpus and examples/control hold
// only files that pass.
func (s *suite) readEnvironments(t testing.TB, p *pipeline.Pipeline, file string) []string {
	t.Helper()
	envs, err := p.ReadEnvironments(context.Background(), environmentsSource(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if ds := envs.Diagnostics(); len(ds) > 0 {
		t.Fatalf("%s:\n%s", file, texts(ds))
	}
	for _, name := range envs.FileOrder() {
		if strings.ContainsAny(name, "./\\") || name == noEnv {
			t.Fatalf("%s: Environment %q cannot name output files", file, name)
		}
	}
	return envs.FileOrder()
}

// renders returns the renders of an entry with the environments file
// envFile ("" for none): without --env first, then each Environment in
// file order (docs/engineering/03-testing-and-quality-strategy.md "Golden
// tests").
func (s *suite) renders(t testing.TB, envFile string) []string {
	t.Helper()
	out := []string{noEnv}
	if envFile != "" {
		out = append(out, s.readEnvironments(t, s.cli, envFile)...)
	}
	return out
}

// entryBase splits an entry name into its base and its number k: an
// example's later entry is <example>-<k>, k counting from 1, and the first
// one is <example> with k 0 (docs/engineering/03-testing-and-quality-
// strategy.md "Golden tests").
func entryBase(name string) (base string, k int) {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return name, 0
	}
	digits := name[i+1:]
	if digits == "" || digits[0] == '0' || len(digits) > 6 || strings.Trim(digits, "0123456789") != "" {
		return name, 0
	}
	k, err := strconv.Atoi(digits)
	if err != nil {
		return name, 0
	}
	return name[:i], k
}

// diffSuffix ends the base name of a diff entry, <example>-diff, whose to/
// holds that example's Bundle.
const diffSuffix = "-diff"

// toLocation returns the location a corpus diff names its TO side by, as
// ruralz bundle diff names a source Bundle: "./<example>", the base name of
// the entry, less the diff suffix for a diff entry, so the configuration
// model's diff example reads "./shop-bundle".
func toLocation(entry string) string {
	base, _ := entryBase(entry)
	return "./" + strings.TrimSuffix(base, diffSuffix)
}

// goldenName returns the name of a golden entry without golden/.
func goldenName(e *corpus.Entry) string { return strings.TrimPrefix(e.Name, corpus.GoldenDir+"/") }

// golden returns the golden entries, sorted by name.
func (s *suite) golden() []*corpus.Entry {
	var out []*corpus.Entry
	for i := range s.corpus.Entries {
		if e := &s.corpus.Entries[i]; e.Kind == corpus.Golden {
			out = append(out, e)
		}
	}
	return out
}

// newest returns the highest-numbered golden entry of base among es, nil
// when there is none.
func newest(es []*corpus.Entry, base string) *corpus.Entry {
	var best *corpus.Entry
	bestK := -1
	for _, e := range es {
		if b, k := entryBase(goldenName(e)); b == base && k > bestK {
			best, bestK = e, k
		}
	}
	return best
}

// nextName returns the name of the entry that follows the newest entry of
// base among es: base itself when there is none, else <base>-<k+1>.
func nextName(es []*corpus.Entry, base string) string {
	e := newest(es, base)
	if e == nil {
		return base
	}
	_, k := entryBase(goldenName(e))
	return base + "-" + strconv.Itoa(k+1)
}

// readTree returns every regular file under dir by its slash path relative
// to dir, hidden files and directories included: a frozen copy holds
// everything its source directory holds.
func readTree(t testing.TB, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: a file of the repository under test.
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return out
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
