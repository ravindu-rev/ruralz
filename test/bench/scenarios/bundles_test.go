// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// update rewrites testdata/digests.golden from the scenario Bundles.
var update = flag.Bool("update", false, "rewrite testdata/digests.golden")

// toolchain is the configuration pipeline as ruralz builds it.
var toolchain = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: runtime.GOMAXPROCS(0)})
})

// scenarioRoot returns the scenario Bundles' directory in the repository.
func scenarioRoot(t *testing.T) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, filepath.FromSlash(harness.ScenarioRoot))
}

// bundleDirs returns every Bundle directory under the scenario root.
func bundleDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(scenarioRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(scenarioRoot(t), e.Name(), "ruralz.yaml")); err == nil {
				dirs = append(dirs, e.Name())
			}
		}
	}
	return dirs
}

// profilesOf returns the profiles a Bundle directory has overlays for.
func profilesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(scenarioRoot(t), dir, "overlays"))
	if err != nil {
		t.Fatalf("%s has no overlays: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// render runs the pipeline on dir for profile and returns its digest.
func render(t *testing.T, dir, profile string) string {
	t.Helper()
	p, err := toolchain()
	if err != nil {
		t.Fatal(err)
	}
	root := scenarioRoot(t)
	res, err := p.Run(context.WithoutCancel(t.Context()), pipeline.Request{
		Source:       loader.Source{Path: filepath.Join(root, dir)},
		Environments: loader.Source{Path: filepath.Join(root, "environments.yaml")},
		Environment:  profile,
	})
	if err != nil {
		t.Fatalf("%s for %s: %v", dir, profile, err)
	}
	if res.HasErrors() || res.Validated == nil {
		t.Fatalf("%s for %s does not validate: %v", dir, profile, res.Diagnostics)
	}
	if res.Overlay != profile {
		t.Errorf("%s for %s applied overlay %q, want %q", dir, profile, res.Overlay, profile)
	}
	return res.Validated.Revision.Digest.String()
}

// TestBundlesValidate is the stage 5 check of every scenario Bundle and
// overlay (PBB "Scenarios"): each validates for each host profile, every
// Bundle has the loopback, rh-1 and rh-3 overlays but the functional
// variants' Bundles, which have the loopback one only, and the profiles'
// Revision digests stay pinned in testdata/digests.golden.
func TestBundlesValidate(t *testing.T) {
	var got strings.Builder
	dirs := bundleDirs(t)
	if len(dirs) == 0 {
		t.Fatal("no scenario Bundle")
	}
	for _, dir := range dirs {
		want := Profiles()
		if slices.Contains(FunctionalDirs(), dir) {
			want = []string{harness.ProfileLoopback}
		}
		have := profilesOf(t, dir)
		slices.Sort(want)
		if !slices.Equal(have, want) {
			t.Errorf("%s has overlays %v, want %v", dir, have, want)
		}
		for _, p := range have {
			got.WriteString(p + " " + dir + " " + render(t, dir, p) + "\n")
		}
	}
	golden := filepath.Join("testdata", "digests.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatalf("%v (go test ./test/bench/scenarios -run TestBundlesValidate -update writes it)", err)
	}
	if got.String() != string(want) {
		t.Errorf("the profiles' Revision digests changed (-update rewrites them):\ngot\n%swant\n%s", got.String(), want)
	}
}

// TestEveryDirIsKnown checks the scenario Bundles against the drivers'
// directories: the catalog's lower-case IDs and the named directories,
// nothing else.
func TestEveryDirIsKnown(t *testing.T) {
	known := []string{DirS3, DirO2, DirS2TelemetryOff, DirS2TraceCaps, DirConstants, DirO1Functional, DirO2Functional}
	for _, sc := range harness.Catalog() {
		known = append(known, filepath.Base(sc.Bundle))
	}
	for _, dir := range bundleDirs(t) {
		if !slices.Contains(known, dir) {
			t.Errorf("%s is no scenario's Bundle", dir)
		}
	}
	for _, d := range known {
		if !slices.Contains(bundleDirs(t), d) {
			t.Errorf("no Bundle directory %s", d)
		}
	}
}

// TestCatalogBundles checks that every catalog scenario's Bundle and the
// profiles' environments file exist at the paths the harness reads.
func TestCatalogBundles(t *testing.T) {
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range harness.Catalog() {
		for _, p := range []string{sc.Bundle, sc.Environments} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s: %v", sc.ID, err)
			}
		}
		if sc.Environments != EnvironmentsFile() || sc.Bundle != Path(strings.ToLower(sc.ID)) {
			t.Errorf("%s reads %s and %s, want %s and %s", sc.ID, sc.Bundle, sc.Environments, Path(strings.ToLower(sc.ID)), EnvironmentsFile())
		}
	}
}

// TestEnvironments checks the environments file holds exactly the three
// profiles, each selecting the overlay of its name.
func TestEnvironments(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scenarioRoot(t), "environments.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`(?m)^  name: (\S+)$`).FindAllStringSubmatch(string(b), -1)
	var got []string
	for _, m := range names {
		got = append(got, m[1])
	}
	if !slices.Equal(got, Profiles()) {
		t.Errorf("environments = %v, want %v", got, Profiles())
	}
	if strings.Contains(string(b), "overlay:") || strings.Contains(string(b), "variables:") {
		t.Error("an Environment names an overlay or variables: each must select the overlay of its name, with no variable")
	}
}

// TestOverlaysAreFixed checks what keeps a profile's digests equal across
// runs: no overlay or base uses a ${VAR}, the loopback overlays name only
// the loopback profile's fixed service ports and secret root, and the
// RH-1 and RH-3 overlays only paths under their fixed secret root.
func TestOverlaysAreFixed(t *testing.T) {
	sv := harness.LoopbackServices()
	var ports []string
	for _, a := range []string{sv.Mock, sv.IDP, sv.Sink, sv.StateStore, sv.FaultProxy} {
		ports = append(ports, a[strings.LastIndexByte(a, ':')+1:])
	}
	addr := regexp.MustCompile(`127\.0\.0\.1:(\d+)`)
	secret := regexp.MustCompile(`name: (/[^\s}]+)`)
	for _, dir := range bundleDirs(t) {
		base, err := os.ReadFile(filepath.Join(scenarioRoot(t), dir, "ruralz.yaml")) //nolint:gosec // G304: a scenario Bundle of the repository
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(base), "${") {
			t.Errorf("%s's base uses a ${VAR}", dir)
		}
		for _, p := range profilesOf(t, dir) {
			b, err := os.ReadFile(filepath.Join(scenarioRoot(t), dir, "overlays", p, "bench.yaml")) //nolint:gosec // G304: a scenario overlay of the repository
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "${") {
				t.Errorf("%s/%s uses a ${VAR}", dir, p)
			}
			wantRoot := RHSecretRoot
			if p == harness.ProfileLoopback {
				wantRoot = harness.LoopbackSecretRoot
				for _, m := range addr.FindAllStringSubmatch(string(b), -1) {
					if !slices.Contains(ports, m[1]) {
						t.Errorf("%s/%s names 127.0.0.1:%s, not a loopback service port %v", dir, p, m[1], ports)
					}
				}
			}
			for _, m := range secret.FindAllStringSubmatch(string(b), -1) {
				if !strings.HasPrefix(m[1], wantRoot+"/") {
					t.Errorf("%s/%s names secret %s outside %s", dir, p, m[1], wantRoot)
				}
			}
		}
	}
}

// TestS2MatchesDocument keeps the S2 Bundle equal byte for byte to the S2
// block of docs/architecture/12-performance-budgets-and-benchmarking.md
// "Scenarios", the Bundle's base.
func TestS2MatchesDocument(t *testing.T) {
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join(root, "docs", "architecture", "12-performance-budgets-and-benchmarking.md")) //nolint:gosec // G304: the repository's own document
	if err != nil {
		t.Fatal(err)
	}
	block, err := S2Block(doc)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join(scenarioRoot(t), "s2", "ruralz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle) != block {
		t.Errorf("test/bench/scenarios/s2/ruralz.yaml differs from the document's S2 block:\n--- document\n%s--- Bundle\n%s", block, bundle)
	}
}

// TestS2Block covers the S2 block's extraction: the yaml block after the
// sentence introducing it, and the errors for a document without one.
func TestS2Block(t *testing.T) {
	doc := "intro\n\nThe S2 Bundle ([Configuration model](x) fields only) carries the scenario rules in comments:\n\n```yaml\na: 1\nb: 2\n```\n\nafter\n```yaml\nc: 3\n```\n"
	got, err := S2Block([]byte(doc))
	if err != nil || got != "a: 1\nb: 2\n" {
		t.Errorf("S2Block = %q, %v, want the first block after the sentence", got, err)
	}
	for name, bad := range map[string]string{
		"no sentence": "```yaml\na: 1\n```\n",
		"no block":    "The S2 Bundle (fields) carries the scenario rules in comments:\n\ntext\n",
		"unclosed":    "The S2 Bundle (fields) carries the scenario rules in comments:\n\n```yaml\na: 1\n",
	} {
		if _, err := S2Block([]byte(bad)); err == nil {
			t.Errorf("S2Block(%s) = nil error", name)
		}
	}
}

// TestConstantsAPIKey checks the constants Bundle's Consumer holds the
// SHA-256 hash of the key the Quota denial cache run sends.
func TestConstantsAPIKey(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scenarioRoot(t), DirConstants, "ruralz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ConstantsAPIKey))
	if want := `hash: "sha256:` + hex.EncodeToString(sum[:]) + `"`; !strings.Contains(string(b), want) {
		t.Errorf("the constants Bundle holds no %s", want)
	}
}

// TestRouteTimeout covers the field the activations run swaps in S2's
// render: ruralz bundle render quotes durations, so the run, which looked
// only for a bare "timeout: 5s", found none and never started; a bare
// duration still matches, and a render without exactly one 5 s timeout is
// refused.
func TestRouteTimeout(t *testing.T) {
	for _, c := range []struct {
		name, render, from, to string
		wantErr                bool
	}{
		{"quoted", "spec:\n  timeout: \"2s\"\n---\nspec:\n  timeout: \"5s\"\n", `timeout: "5s"`, `timeout: "6s"`, false},
		{"bare", "spec:\n  timeout: 5s   # (target)\n", "timeout: 5s", "timeout: 6s", false},
		{"none", "spec:\n  timeout: \"2s\"\n", "", "", true},
		{"two", "timeout: \"5s\"\ntimeout: \"5s\"\n", "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			from, to, err := routeTimeout([]byte(c.render))
			if (err != nil) != c.wantErr || from != c.from || to != c.to {
				t.Errorf("routeTimeout(%q) = %q, %q, %v; want %q, %q, error %v", c.render, from, to, err, c.from, c.to, c.wantErr)
			}
		})
	}
}

// TestDriverItems checks the drivers against the Latency job's plan:
// every driver item of the rotation has a driver here, and no driver lacks
// an item.
func TestDriverItems(t *testing.T) {
	var items []string
	for slot := 1; slot <= harness.Slots; slot++ {
		for _, it := range harness.SlotItems(slot) {
			if it.Kind == harness.KindDriver {
				items = append(items, it.ID)
			}
		}
	}
	if !slices.Equal(items, DriverItems()) {
		t.Errorf("rotation driver items %v, want DriverItems() %v", items, DriverItems())
	}
	drivers := Drivers(Full())
	for _, id := range items {
		if drivers[id] == nil {
			t.Errorf("no driver for %s", id)
		}
	}
	if len(drivers) != len(items) {
		t.Errorf("%d drivers for %d items", len(drivers), len(items))
	}
}

// TestDigestsPinBothLabs checks the golden file covers every Bundle and
// profile, so a new Bundle cannot skip the pin.
func TestDigestsPinBothLabs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "digests.golden"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	n := 0
	for _, dir := range bundleDirs(t) {
		n += len(profilesOf(t, dir))
	}
	if len(lines) != n {
		t.Errorf("digests.golden pins %d renders, want %d", len(lines), n)
	}
	for i, l := range lines {
		if f := strings.Fields(l); len(f) != 3 || !strings.HasPrefix(f[2], "sha256:") {
			t.Errorf("digests.golden line %s: %q", strconv.Itoa(i+1), l)
		}
	}
}
