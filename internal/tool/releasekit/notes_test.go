// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseTitle(t *testing.T) {
	cases := map[string]title{
		"feat(cli): add the version command": {Type: "feat", Scope: "cli", Description: "add the version command"},
		"fix: handle empty bodies":           {Type: "fix", Description: "handle empty bodies"},
		"feat(config)!: drop the old field":  {Type: "feat", Scope: "config", Breaking: true, Description: "drop the old field"},
		"build(deps): raise x to v1.2.0":     {Type: "build", Scope: "deps", Description: "raise x to v1.2.0"},
	}
	for subject, want := range cases {
		got, ok := parseTitle(subject)
		if !ok || got != want {
			t.Errorf("parseTitle(%q) = %+v %v, want %+v", subject, got, ok, want)
		}
	}
	for _, subject := range []string{"Merge pull request #1", "feat(CLI): x", "feat:no space", "fix(cli) : x"} {
		if _, ok := parseTitle(subject); ok {
			t.Errorf("parseTitle(%q) accepted", subject)
		}
	}
}

// TestFooters: Conventional Commits footers, values continuing over the
// following lines until a blank line or the next footer.
func TestFooters(t *testing.T) {
	body := "Some body text.\n\n" +
		"* feat: squashed commit\n\n" +
		"BREAKING CHANGE: the --old flag is now --new\n" +
		"and scripts must change.\n" +
		"Refs: OQ-release-versioning-and-compatibility-6\n" +
		"BREAKING-CHANGE: second one\n" +
		"Deprecated: --legacy-output; removed in 0.4.0\n" +
		"Signed-off-by: A <a@example.com>\n\n" +
		"Deprecated: \n"
	if got, want := footers(body, footerBreaking, footerBreakingH), []string{
		"the --old flag is now --new and scripts must change.", "second one",
	}; !slices.Equal(got, want) {
		t.Errorf("breaking footers = %q, want %q", got, want)
	}
	if got, want := footers(body, footerDeprecated), []string{"--legacy-output; removed in 0.4.0"}; !slices.Equal(got, want) {
		t.Errorf("deprecated footers = %q, want %q", got, want)
	}
	if got := footers("BREAKING CHANGES: not a token match\n", footerBreaking); len(got) != 0 {
		t.Errorf("footers matched another token: %q", got)
	}
}

// TestClassifyCommits sorts commits into the notes sections.
func TestClassifyCommits(t *testing.T) {
	commits := []commit{
		{Hash: "a1", Subject: "feat(cli): add the version command"},
		{Hash: "a2", Subject: "fix(gateway): keep the Host header"},
		{Hash: "a3", Subject: "perf(gateway): cut an allocation"},
		{Hash: "a4", Subject: "build(deps): raise example.com/m to v1.2.0"},
		{Hash: "a5", Subject: "docs(data-plane): explain the Drain"},
		{Hash: "a6", Subject: "build(tools): add releasekit"},
		{Hash: "a7", Subject: "feat(config)!: drop the old field"},
		{Hash: "a8", Subject: "refactor(cli): split a file", Body: "BREAKING CHANGE: the CLI exit code 4 is gone\n\nDeprecated: the --flat flag of ruralz bundle render; removed in 0.4.0\n"},
		{Hash: "a9", Subject: "Merge branch 'x'"},
	}
	var in notesInput
	classifyCommits(commits, &in)
	hashes := func(items []noteItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.Hash)
		}
		return out
	}
	for name, c := range map[string]struct{ got, want []string }{
		"features":     {hashes(in.Features), []string{"a1", "a7"}},
		"fixes":        {hashes(in.Fixes), []string{"a2"}},
		"performance":  {hashes(in.Performance), []string{"a3"}},
		"dependencies": {hashes(in.Dependencies), []string{"a4"}},
		"deprecations": {hashes(in.Deprecations), []string{"a8"}},
		"upgrades":     {hashes(in.Upgrades), []string{"a7", "a8"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if in.Upgrades[0].Text != "drop the old field" || in.Upgrades[1].Text != "the CLI exit code 4 is gone" {
		t.Errorf("upgrade notes = %+v", in.Upgrades)
	}
	if in.Deprecations[0].Text != "the --flat flag of ruralz bundle render; removed in 0.4.0" {
		t.Errorf("deprecations = %+v", in.Deprecations)
	}
}

// TestCodeSpan: a code span keeps its text verbatim, backslashes and
// underscores included, and a fence longer than any backtick run inside.
func TestCodeSpan(t *testing.T) {
	for in, want := range map[string]string{
		"ruralz_0.1.0_bench.tar.gz": "`ruralz_0.1.0_bench.tar.gz`",
		`a\b`:                       "`a\\b`",
		"a`b":                       "``a`b``",
		"`edge":                     "`` `edge ``",
	} {
		if got := codeSpan(in); got != want {
			t.Errorf("codeSpan(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeMarkdown(t *testing.T) {
	if got, want := escapeMarkdown("keep <script> *bold* [x](y) a|b `c` ~d_e\\"), `keep \<script\> \*bold\* \[x\](y) a\|b \`+"`"+`c\`+"`"+` \~d\_e\\`; got != want {
		t.Errorf("escapeMarkdown = %q, want %q", got, want)
	}
}

// sampleIndex and sampleManifests are the image digests of the sample
// notes.
const sampleIndex = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sampleImages(version string) []imagePlan {
	return []imagePlan{images(version)[0].withDigests(sampleIndex, []platformManifest{
		{Platform: "linux/amd64", Digest: "sha256:" + strings.Repeat("b", 64)},
		{Platform: "linux/arm64", Digest: "sha256:" + strings.Repeat("c", 64)},
	})}
}

func sampleNotes(version string) notesInput {
	in := notesInput{
		Version: version, Commit: fixtureCommit, From: "v0.0.9",
		Archives: artifacts(), Images: sampleImages(version), Bench: benchArchiveName(version),
		Features: []noteItem{{Scope: "cli", Text: "add the version command", Hash: fixtureCommit}},
		Schema:   []string{"`ruralz/v1alpha1`: no schema changes."},
		Records: []benchRecord{
			{Budgets: []benchBudget{{ID: "PB-2", Value: "<= 1ms (target)", Result: "0.84ms", SLO: "SLO-GW-2", Verdict: "pass"}}, Summary: &benchSummary{Of: "comparison", Verdict: "pass"}},
			{Budgets: []benchBudget{{ID: "PB-3", Value: "<= 150us (target)", Result: "160us", Verdict: "fail"}}, Summary: &benchSummary{Of: "comparison", Verdict: "none"}},
		},
		Skipped: []skippedGate{{Gate: "Benchmarks", By: "octocat", Role: "maintain", Reason: "RH-1 is\nbeing rebuilt"}},
	}
	in.Records[0].Identity.Scenario, in.Records[0].Identity.Commit, in.Records[0].Environment.HardwareProfile = "S2", fixtureCommit, "RH-1"
	in.Records[0].Identity.Baseline = "v0.0.9"
	in.Records[1].Identity.Scenario, in.Records[1].Identity.Commit = "S1", strings.Repeat("e", 40)
	return in
}

// TestRenderNotes: every section of the Release artifacts row, in order,
// with the verification steps naming the signer and the cosign floor.
func TestRenderNotes(t *testing.T) {
	got := renderNotes(sampleNotes("0.1.0"))
	headings := []string{
		"# Ruralz 0.1.0\n", "## Artifacts", "## Features", "## Fixes", "## Performance", "## Dependency updates",
		"## Deprecations", "## Upgrade notes", "## Schema, Plugin ABI and Control Stream", "## Budget results", "## Verification", "## Skipped gates",
	}
	last := -1
	for _, h := range headings {
		i := strings.Index(got, h)
		if i <= last {
			t.Errorf("heading %q missing or out of order", h)
		}
		last = i
	}
	for _, want := range []string{
		"Release 0.1.0 of Ruralz, built from commit [`0123456789ab`]",
		"changes since `v0.0.9`",
		"| `ruralzd_0.1.0_darwin_arm64.tar.gz` | darwin/arm64 | Development only |",
		"| `ruralz_0.1.0_windows_amd64.zip` | windows/amd64 | Production |",
		"Image `ghcr.io/ravindu-rev/ruralzd:0.1.0` and `ghcr.io/ravindu-rev/ruralzd:0.1` for linux/amd64 and linux/arm64; deployments should pin it by digest.",
		"- **cli:** add the version command ([`0123456789ab`](https://github.com/ravindu-rev/ruralz/commit/" + fixtureCommit + "))",
		"## Fixes\n\nNone.\n",
		"- `ruralz/v1alpha1`: no schema changes.",
		"| Scenario | Summary | A/B verdict | Budget | Value | Result | SLO | Verdict | Hardware | Commit |",
		`| S2 | comparison | pass against v0.0.9 | PB-2 | \<= 1ms (target) | 0.84ms | SLO-GW-2 | pass | RH-1 | 0123456789ab |`,
		`| S1 | comparison | none | PB-3 | \<= 150us (target) | 160us |  | fail |  | eeeeeeeeeeee (other commit) |`,
		"Rows marked (other commit) were measured on another commit",
		"cosign 3.1.3 or newer on 3.x, or 2.6.5 or newer on 2.x",
		"GHSA-fx35-mq7g-6g98",
		"--certificate-identity https://github.com/ravindu-rev/ruralz/.github/workflows/release-build.yml@refs/tags/v0.1.0",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		"sha256sum --ignore-missing -c SHA256SUMS",
		"`release-manifest.json`, which lists every archive's members and digests and the image digests",
		"`ruralz_0.1.0_bench.tar.gz` holds the `ruralz.bench.v1` records",
		"The image index is `ghcr.io/ravindu-rev/ruralzd@" + sampleIndex + "`; its SBOM is `ruralzd_0.1.0_image.cdx.json`.",
		"cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json --new-bundle-format \\\n",
		"gh attestation verify ruralzd_0.1.0_linux_amd64.tar.gz --repo ravindu-rev/ruralz \\\n     --signer-workflow ravindu-rev/ruralz/.github/workflows/release-build.yml\n",
		"--signer-workflow ravindu-rev/ruralz/.github/workflows/release-build.yml \\\n     --predicate-type https://cyclonedx.org/bom\n",
		"cosign verify ghcr.io/ravindu-rev/ruralzd@" + sampleIndex + " \\\n",
		"cosign verify ghcr.io/ravindu-rev/ruralzd@" + sampleIndex + " --new-bundle-format \\\n",
		"gh attestation verify oci://ghcr.io/ravindu-rev/ruralzd@" + sampleIndex + " --repo ravindu-rev/ruralz",
		"xattr -d com.apple.quarantine ruralzd",
		"A download with `curl` never carries the flag.",
		"| Benchmarks | Within 5% p99 and 3% alloc/op of the previous release (target)",
		"For 0.1.0 no maintainer skips this gate: the release waits for RH-1 and passing budgets | @octocat | maintain | RH-1 is being rebuilt |",
		"[release gates](https://github.com/ravindu-rev/ruralz/blob/v0.1.0/docs/engineering/03-testing-and-quality-strategy.md#release-gates)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q", want)
		}
	}
}

// TestRenderNotesKinds: a candidate names its one image tag; a development
// build publishes none; no records and no skipped gate say so.
func TestRenderNotesKinds(t *testing.T) {
	rc := sampleNotes("0.1.0-rc.1")
	rc.Records, rc.Skipped, rc.From = nil, nil, ""
	got := renderNotes(rc)
	for _, want := range []string{
		"Release candidate 0.1.0-rc.1 of Ruralz", "changes since the first commit.",
		"Image `ghcr.io/ravindu-rev/ruralzd:0.1.0-rc.1` for", "No benchmark summary records were provided for this build.",
		"None: no maintainer skipped a release gate.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("candidate notes lack %q", want)
		}
	}
	if dev := renderNotes(sampleNotes("0.0.0-dev")); !strings.Contains(dev, "no tag is published for a development build") {
		t.Error("development notes name an image tag")
	}
	// A dry run without Docker records no image, so the notes give no
	// image digest to verify.
	bare := sampleNotes("0.0.0-dev")
	bare.Images = images("0.0.0-dev")
	if got := renderNotes(bare); !strings.Contains(got, "3. This build recorded no image.") || strings.Contains(got, "cosign verify ghcr.io") {
		t.Errorf("notes without an image:\n%s", got)
	}
}

func schemaDoc(kinds, defs string) string {
	return `{"properties":{"kind":{"enum":[` + kinds + `]}},"$defs":{` + defs + `}}`
}

// TestSchemaChanges lists kinds, definitions, properties, enum values,
// defaults, schema levels and required members.
func TestSchemaChanges(t *testing.T) {
	from := schemaDoc(`"Gateway","Route"`,
		`"Mode":{"enum":["a","b"]},`+
			`"Old":{"properties":{}},`+
			`"RouteSpec":{"properties":{"timeout":{"type":"string"},"gone":{},"retries":{"default":2},"mode":{"enum":["x"]},"level":{"x-ruralz-since":1}},"required":["timeout"]}`)
	to := schemaDoc(`"Gateway","Route","Upstream"`,
		`"Mode":{"enum":["a","c"]},`+
			`"New":{"x-ruralz-since":2},`+
			`"RouteSpec":{"properties":{"timeout":{"type":"string"},"retries":{"default":3},"mode":{"enum":["x","y"]},"level":{"x-ruralz-since":2},"added":{"x-ruralz-since":2}},"required":["added"]}`)
	got, err := schemaChanges([]byte(from), []byte(to))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Kind `Upstream` added",
		"`Mode`: value \"c\" added",
		"`Mode`: value \"b\" removed",
		"Definition `New` added at schema level 2",
		"Definition `Old` removed",
		"`RouteSpec.added` added at schema level 2",
		"`RouteSpec.gone` removed",
		"`RouteSpec.level`: schema level changed from 1 to 2",
		"`RouteSpec.mode`: value \"y\" added",
		"`RouteSpec.retries`: default changed from 2 to 3",
		"`RouteSpec`: \"added\" is now required",
		"`RouteSpec`: \"timeout\" is no longer required",
	}
	if !slices.Equal(got, want) {
		t.Errorf("schemaChanges =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if same, _ := schemaChanges([]byte(from), []byte(from)); len(same) != 0 {
		t.Errorf("identical schemas: %q", same)
	}
	if _, err := schemaChanges([]byte("{"), []byte(to)); err == nil {
		t.Error("bad JSON accepted")
	}
}

// TestReadBudgets reads ruralz.bench.v1 summary records, nested or not,
// sorted, and skips the run records; a missing directory holds none;
// another JSON file, a record of another kind and a summary record without
// its summary are errors.
func TestReadBudgets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "nightly", "s2.json"), `{"format":"ruralz.bench.v1","identity":{"scenario":"S2","kind":"summary","commit":"c"},"budgets":[{"id":"PB-2","value":"v","result":"r","slo":"","verdict":"pass"}],"summary":{"of":"comparison","verdict":"pass"}}`)
	writeFile(t, filepath.Join(dir, "s1.json"), `{"format":"ruralz.bench.v1","identity":{"scenario":"S1","kind":"summary"},"budgets":[],"summary":{"of":"ladder","verdict":"none"}}`)
	writeFile(t, filepath.Join(dir, "runs", "s2-head-1.json"), `{"format":"ruralz.bench.v1","identity":{"scenario":"S2","kind":"macro"},"budgets":[{"id":"PB-2","value":"v","result":"r1","slo":"","verdict":"pass"}]}`)
	writeFile(t, filepath.Join(dir, "runs", "cel.json"), `{"format":"ruralz.bench.v1","identity":{"scenario":"CEL","kind":"component"},"budgets":[]}`)
	writeFile(t, filepath.Join(dir, "s1.hlog"), "histogram")
	got, err := readBudgets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Identity.Scenario != "S1" || got[0].Summary.Of != "ladder" ||
		got[1].file != "nightly/s2.json" || got[1].Budgets[0].ID != "PB-2" || got[1].Budgets[0].Result != "r" {
		t.Errorf("records = %+v", got)
	}
	if none, err := readBudgets(filepath.Join(dir, "absent")); err != nil || none != nil {
		t.Errorf("missing directory: %v %v", none, err)
	}
	for name, doc := range map[string]string{
		"foreign.json":      `{"format":"other"}`,
		"kind.json":         `{"format":"ruralz.bench.v1","identity":{"scenario":"S1","kind":"trend"},"budgets":[]}`,
		"bare-summary.json": `{"format":"ruralz.bench.v1","identity":{"scenario":"S1","kind":"summary"},"budgets":[]}`,
	} {
		bad := t.TempDir()
		writeFile(t, filepath.Join(bad, name), doc)
		if _, err := readBudgets(bad); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestReadSkipped validates the skipped-gates document.
func TestReadSkipped(t *testing.T) {
	dir := t.TempDir()
	good := `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Benchmarks","by":"octo-cat","role":"maintain","reason":"RH-1 down"}]}`
	cases := map[string]string{
		"unknown gate":   `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Compatibility","by":"o","role":"admin","reason":"r"}]}`,
		"twice":          `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Chaos","by":"o","role":"admin","reason":"r"},{"gate":"Chaos","by":"p","role":"admin","reason":"s"}]}`,
		"login":          `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Chaos","by":"-bad","role":"admin","reason":"r"}]}`,
		"role":           `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Chaos","by":"o","role":"write","reason":"r"}]}`,
		"reason":         `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Chaos","by":"o","role":"admin","reason":"  "}]}`,
		"format":         `{"format":"ruralz.skippedgates.v2","skipped":[]}`,
		"unknown member": `{"format":"ruralz.skippedgates.v1","skipped":[],"note":"x"}`,
	}
	p := filepath.Join(dir, "skipped.json")
	writeFile(t, p, good)
	got, err := readSkipped(p)
	if err != nil || len(got) != 1 || got[0].By != "octo-cat" {
		t.Errorf("readSkipped(good) = %+v, %v", got, err)
	}
	for name, content := range cases {
		writeFile(t, p, content)
		if _, err := readSkipped(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckRef(t *testing.T) {
	for _, ref := range []string{"v0.1.0", "HEAD", "origin/main", "HEAD^", fixtureCommit} {
		if err := checkRef(ref); err != nil {
			t.Errorf("checkRef(%q) = %v", ref, err)
		}
	}
	for _, ref := range []string{"", "-x", "--output=x", "a b"} {
		if err := checkRef(ref); err == nil {
			t.Errorf("checkRef(%q) = nil", ref)
		}
	}
}

// gitIn runs git in dir with a fixed identity and dates.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-C", dir, "-c", "user.name=Release", "-c", "user.email=release@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "core.autocrlf=false",
	}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...) //nolint:gosec // Test runs git with fixed arguments in a temporary repository.
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-10-01T12:00:00Z", "GIT_COMMITTER_DATE=2026-10-01T12:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRunNotesOnRepository renders the notes of a real Git range: by
// default the commits after the previous release tag, skipping a release
// candidate's tag, with schema changes against it; and the first release
// from the root.
func TestRunNotesOnRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main")
	schemaPath := filepath.Join(dir, "api", "schema", "ruralz", "v1alpha1", "authoring.schema.json")
	writeFile(t, schemaPath, schemaDoc(`"Gateway","Route"`, `"RouteSpec":{"properties":{"timeout":{}}}`))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "--quiet", "-m", "feat(config): add the schema")
	gitIn(t, dir, "tag", "v0.1.0")
	for _, msg := range []string{
		"fix(gateway): handle <script> in hosts",
		"perf(gateway): cut an allocation",
		"build(deps): raise example.com/m to v1.2.0",
		"docs(data-plane): explain the Drain",
	} {
		gitIn(t, dir, "commit", "--quiet", "--allow-empty", "-m", msg)
		if strings.HasPrefix(msg, "perf") {
			gitIn(t, dir, "tag", "v0.2.0-rc.1")
		}
	}
	writeFile(t, schemaPath, schemaDoc(`"Gateway","Route","Upstream"`, `"RouteSpec":{"properties":{"timeout":{},"retries":{}}}`))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "--quiet", "-m", "feat(cli)!: rename the output flag\n\nBREAKING CHANGE: --old is now --new\nand scripts must change.\n\nDeprecated: --legacy-output; removed in 0.4.0")

	out := filepath.Join(t.TempDir(), "NOTES.md")
	skipped := filepath.Join(t.TempDir(), "skipped.json")
	writeFile(t, skipped, `{"format":"ruralz.skippedgates.v1","skipped":[{"gate":"Chaos","by":"octocat","role":"admin","reason":"flaky runner"}]}`)
	o := notesOptions{root: dir, version: "0.2.0", from: "", to: "HEAD", budgets: filepath.Join(dir, "absent"), skipped: skipped, out: out}
	if err := runNotes(t.Context(), o, runGit); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out) //nolint:gosec // The test's own output.
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# Ruralz 0.2.0", "changes since `v0.1.0`",
		"**gateway:** handle \\<script\\> in hosts", "**gateway:** cut an allocation", "**deps:** raise example.com/m to v1.2.0",
		"**cli:** rename the output flag", "**cli:** --old is now --new and scripts must change.",
		"**cli:** --legacy-output; removed in 0.4.0",
		"`ruralz/v1alpha1`: Kind `Upstream` added.", "`ruralz/v1alpha1`: `RouteSpec.retries` added.",
		"`ruralz.plugin.v1`: not served by this release.", "`ruralz.control.v1`: not served by this release.",
		"passed on the candidate commit | @octocat | admin | flaky runner |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"explain the Drain", "add the schema"} {
		if strings.Contains(got, absent) {
			t.Errorf("notes include %q", absent)
		}
	}

	o.from, o.skipped, o.version = rootRef, "", "0.1.0"
	if err := runNotes(t.Context(), o, runGit); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(out) //nolint:gosec // The test's own output.
	if first := string(data); !strings.Contains(first, "`ruralz/v1alpha1` is served for the first time, with the kinds `Gateway`, `Route`, `Upstream`.") ||
		!strings.Contains(first, "add the schema") || !strings.Contains(first, "changes since the first commit.") {
		t.Errorf("first-release notes:\n%s", first)
	}

	// Without a previous release tag the default range starts at the root.
	o.from, o.to = "", "v0.1.0"
	if err := runNotes(t.Context(), o, runGit); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(out) //nolint:gosec // The test's own output.
	if first := string(data); !strings.Contains(first, "changes since the first commit.") || !strings.Contains(first, "add the schema") {
		t.Errorf("default range of the first release:\n%s", first)
	}
}

// TestRunNotesErrors: bad flags and failing git are errors.
func TestRunNotesErrors(t *testing.T) {
	// The stub resolves any ref and fails every other git command.
	failing := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return []byte(fixtureCommit), nil
		}
		return nil, errors.New("git failed")
	}
	base := notesOptions{root: ".", version: "0.1.0", from: rootRef, to: "HEAD", out: filepath.Join(t.TempDir(), "n.md")}
	cases := map[string]func(*notesOptions){
		"version": func(o *notesOptions) { o.version = "0.1" },
		"to":      func(o *notesOptions) { o.to = "--all" },
		"from":    func(o *notesOptions) { o.from = "-x" },
		"git":     func(*notesOptions) {},
	}
	for name, change := range cases {
		o := base
		change(&o)
		if err := runNotes(t.Context(), o, failing); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestRunNotesManifest: with -manifest the notes name the image digests and
// the records archive the manifest records; a manifest of another version
// or commit is an error.
func TestRunNotesManifest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main")
	writeFile(t, filepath.Join(dir, "api", "schema", "ruralz", "v1alpha1", "authoring.schema.json"), schemaDoc(`"Gateway"`, `"GatewaySpec":{}`))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "--quiet", "-m", "feat(config): add the schema")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	m := &manifest{Format: manifestFormat, Version: "0.2.0", Commit: head, Images: sampleImages("0.2.0"), Bench: benchInfo{Name: benchArchiveName("0.2.0")}}
	data, err := encodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), manifestName)
	writeFile(t, manifestPath, string(data))
	out := filepath.Join(t.TempDir(), notesName)
	o := notesOptions{root: dir, version: "0.2.0", from: rootRef, to: "HEAD", manifest: manifestPath, out: out}
	if err := runNotes(t.Context(), o, runGit); err != nil {
		t.Fatal(err)
	}
	got := string(readBack(t, out))
	for _, want := range []string{
		"cosign verify ghcr.io/ravindu-rev/ruralzd@" + sampleIndex,
		"The image index is `ghcr.io/ravindu-rev/ruralzd@" + sampleIndex + "`",
		"`ruralz_0.2.0_bench.tar.gz` holds",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
	for name, change := range map[string]func(*manifest){
		"version": func(m *manifest) { m.Version = "0.2.1" },
		"commit":  func(m *manifest) { m.Commit = strings.Repeat("a", 40) },
	} {
		other := *m
		change(&other)
		data, err := encodeManifest(&other)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, manifestPath, string(data))
		if err := runNotes(t.Context(), o, runGit); err == nil || !strings.Contains(err.Error(), "describes") {
			t.Errorf("a manifest of another %s: %v, want an error", name, err)
		}
	}
}
