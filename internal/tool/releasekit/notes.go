// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// repoURL is the source repository the notes link to.
const repoURL = "https://github.com/ravindu-rev/ruralz"

// signerIdentity is the certificate identity of the keyless signatures:
// the reusable release-build.yml workflow at the release tag, the job
// that signs and attests (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release pipeline").
const signerIdentity = repoURL + "/.github/workflows/release-build.yml@refs/tags/v"

// oidcIssuer is the OIDC issuer of GitHub Actions.
const oidcIssuer = "https://token.actions.githubusercontent.com"

// rootRef names the start of history as the notes range's start, for the
// first release.
const rootRef = "root"

// Footer tokens the notes read (Conventional Commits footers).
const (
	footerBreaking   = "BREAKING CHANGE"
	footerBreakingH  = "BREAKING-CHANGE"
	footerDeprecated = "Deprecated"
)

// notesTitle is the first line of the notes.
func notesTitle(version string) string { return "# Ruralz " + version }

// notesOptions are the flags of `releasekit notes`.
type notesOptions struct {
	root, version, from, to, budgets, skipped, out string
	// manifest is the release directory's release-manifest.json, whose
	// image digests the verification steps name; empty lists the
	// artifacts without digests.
	manifest string
}

// commit is one commit of the release range.
type commit struct {
	Hash, Subject, Body string
}

// logCommits lists the non-merge commits from..to, oldest first; from ""
// starts at the root.
func logCommits(ctx context.Context, git gitRunner, dir, from, to string) ([]commit, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	out, err := git(ctx, dir, "log", "--no-merges", "--reverse", "--format=%H%x1f%s%x1f%B%x1e", "--end-of-options", rng)
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		body := f[2]
		if _, rest, ok := strings.Cut(body, "\n"); ok {
			body = rest
		} else {
			body = ""
		}
		commits = append(commits, commit{Hash: f[0], Subject: f[1], Body: body})
	}
	return commits, nil
}

// title is a parsed Conventional Commits title.
type title struct {
	Type, Scope, Description string
	Breaking                 bool
}

// parseTitle parses "<type>(<scope>)!: <description>"; ok is false for any
// other subject.
func parseTitle(subject string) (title, bool) {
	m := regexp.MustCompile(`^([a-z]+)(?:\(([a-z0-9][a-z0-9-]*)\))?(!)?: (.+)$`).FindStringSubmatch(subject)
	if m == nil {
		return title{}, false
	}
	return title{Type: m[1], Scope: m[2], Breaking: m[3] == "!", Description: m[4]}, true
}

// footers returns the values of the footers named key in a commit body. A
// footer is a line "<token>: <value>" whose value runs on over the next
// lines until a blank line or another footer.
func footers(body string, keys ...string) []string {
	token := regexp.MustCompile(`^(BREAKING CHANGE|[A-Za-z][A-Za-z0-9-]*): (.*)$`)
	var out []string
	var cur *strings.Builder
	flush := func() {
		if cur != nil {
			out = append(out, strings.TrimSpace(cur.String()))
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if m := token.FindStringSubmatch(line); m != nil {
			flush()
			if slices.Contains(keys, m[1]) {
				cur = &strings.Builder{}
				cur.WriteString(m[2])
			}
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if cur != nil {
			cur.WriteString(" " + strings.TrimSpace(line))
		}
	}
	flush()
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

// noteItem is one line of a notes section.
type noteItem struct {
	Scope, Text, Hash string
}

// notesInput is everything the notes render.
type notesInput struct {
	Version, Commit, From string
	Features, Fixes, Performance, Dependencies,
	Deprecations, Upgrades []noteItem
	Schema   []string
	Records  []benchRecord
	Skipped  []skippedGate
	Archives []artifact
	Images   []imagePlan
	// Bench names the benchmark records archive.
	Bench string
}

// classify sorts the commits of the range into the notes sections:
// features (feat), fixes (fix), performance (perf), dependency updates
// (build(deps)), deprecations (Deprecated footers) and upgrade notes
// (breaking titles and BREAKING CHANGE footers).
func classifyCommits(commits []commit, in *notesInput) {
	for _, c := range commits {
		t, ok := parseTitle(c.Subject)
		if ok {
			item := noteItem{Scope: t.Scope, Text: t.Description, Hash: c.Hash}
			switch {
			case t.Type == "feat":
				in.Features = append(in.Features, item)
			case t.Type == "fix":
				in.Fixes = append(in.Fixes, item)
			case t.Type == "perf":
				in.Performance = append(in.Performance, item)
			case t.Type == "build" && t.Scope == "deps":
				in.Dependencies = append(in.Dependencies, item)
			}
		}
		breaking := footers(c.Body, footerBreaking, footerBreakingH)
		for _, b := range breaking {
			in.Upgrades = append(in.Upgrades, noteItem{Scope: t.Scope, Text: b, Hash: c.Hash})
		}
		if ok && t.Breaking && len(breaking) == 0 {
			in.Upgrades = append(in.Upgrades, noteItem{Scope: t.Scope, Text: t.Description, Hash: c.Hash})
		}
		for _, d := range footers(c.Body, footerDeprecated) {
			in.Deprecations = append(in.Deprecations, noteItem{Scope: t.Scope, Text: d, Hash: c.Hash})
		}
	}
}

// escapeMarkdown escapes the characters that would let commit text add
// markup, links or HTML to the notes.
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_[]<>|~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// codeSpan renders s as a Markdown code span, where backslash escapes do
// not apply: the fence is one backtick longer than the longest run of
// backticks in s, with a space inside it when s starts or ends with one.
func codeSpan(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// short is the 12-character form of a commit.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// writeItems renders a section's items, or "None.".
func writeItems(b *strings.Builder, items []noteItem) {
	if len(items) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, it := range items {
		b.WriteString("- ")
		if it.Scope != "" {
			fmt.Fprintf(b, "**%s:** ", escapeMarkdown(it.Scope))
		}
		fmt.Fprintf(b, "%s ([`%s`](%s/commit/%s))\n", escapeMarkdown(it.Text), short(it.Hash), repoURL, it.Hash)
	}
}

// renderNotes renders the release notes in Markdown. The sections follow
// the Release artifacts row of
// docs/engineering/04-release-versioning-and-compatibility.md: features,
// fixes, deprecations, upgrade notes, schema, ABI and Control Stream
// additions, budget results, verification steps and skipped gates.
func renderNotes(in notesInput) string {
	var b strings.Builder
	b.WriteString(notesTitle(in.Version) + "\n\n")
	kind := map[string]string{kindRelease: "Release", kindCandidate: "Release candidate", kindDev: "Development build"}[versionKind(in.Version)]
	from := "the first commit"
	if in.From != "" {
		from = codeSpan(in.From)
	}
	fmt.Fprintf(&b, "%s %s of Ruralz, built from commit [`%s`](%s/commit/%s); changes since %s.\n",
		kind, in.Version, short(in.Commit), repoURL, in.Commit, from)

	b.WriteString("\n## Artifacts\n\n")
	b.WriteString("| Archive | Platform | Support |\n|---|---|---|\n")
	for _, a := range in.Archives {
		support := "Production"
		if a.Support == supportDevelopment {
			support = "Development only"
		}
		fmt.Fprintf(&b, "| `%s` | %s/%s | %s |\n", a.archiveName(in.Version), a.OS, a.Arch, support)
	}
	b.WriteString("\nEach archive holds the binary, `LICENSE`, `NOTICE`, `README.md` and its own `THIRD_PARTY_LICENSES`; " +
		"linux `ruralzd` archives add `deploy/systemd/`. Each archive's CycloneDX 1.7 SBOM is `<archive>.cdx.json`. " +
		codeSpan(in.Bench) + " holds the `ruralz.bench.v1` records, raw histograms and CPU profiles of the release's Latency and soak jobs. " +
		"`SHA256SUMS`, signed as `" + signatureName + "`, covers every other file of the release: the archives and their SBOMs, the image SBOM, " +
		"the JSON Schema files `ruralz-<apiVersion>-authoring.schema.json` and `ruralz-<apiVersion>-rendered.schema.json`, " +
		"`" + manifestName + "`, which lists every archive's members and digests and the image digests, `" + notesName + "` and the benchmark records.\n\n")
	for _, img := range in.Images {
		digest := ""
		if img.recorded() {
			digest = fmt.Sprintf(" The image index is %s; its SBOM is %s.", codeSpan(img.Repository+"@"+img.Digest), codeSpan(img.SBOM))
		}
		if len(img.Tags) == 0 {
			fmt.Fprintf(&b, "Image `%s`: no tag is published for a development build.%s\n", img.Repository, digest)
			continue
		}
		var tags []string
		for _, t := range img.Tags {
			tags = append(tags, "`"+img.Repository+":"+t+"`")
		}
		fmt.Fprintf(&b, "Image %s for %s; deployments should pin it by digest.%s\n", strings.Join(tags, " and "), strings.Join(img.Platforms, " and "), digest)
	}

	for _, s := range []struct {
		heading string
		items   []noteItem
	}{
		{"Features", in.Features},
		{"Fixes", in.Fixes},
		{"Performance", in.Performance},
		{"Dependency updates", in.Dependencies},
		{"Deprecations", in.Deprecations},
		{"Upgrade notes", in.Upgrades},
	} {
		fmt.Fprintf(&b, "\n## %s\n\n", s.heading)
		writeItems(&b, s.items)
	}

	b.WriteString("\n## Schema, Plugin ABI and Control Stream\n\n")
	for _, line := range in.Schema {
		b.WriteString("- " + line + "\n")
	}

	b.WriteString("\n## Budget results\n\n")
	writeBudgets(&b, in.Records, in.Commit)

	b.WriteString("\n## Verification\n\n")
	writeVerification(&b, in)

	b.WriteString("\n## Skipped gates\n\n")
	if len(in.Skipped) == 0 {
		b.WriteString("None: no maintainer skipped a release gate.\n")
	} else {
		b.WriteString("A maintainer skipped these [release gates](" + repoURL + "/blob/" + tagOf(in) +
			"/docs/engineering/03-testing-and-quality-strategy.md#release-gates), each with the reason below.\n\n")
		b.WriteString("| Gate | Evidence | Skipped by | Role | Reason |\n|---|---|---|---|---|\n")
		evidence := map[string]string{}
		for _, row := range releaseGateRows() {
			evidence[row[0]] = row[1]
		}
		for _, g := range in.Skipped {
			fmt.Fprintf(&b, "| %s | %s | @%s | %s | %s |\n", escapeMarkdown(g.Gate), escapeMarkdown(evidence[g.Gate]), escapeMarkdown(g.By), g.Role,
				escapeMarkdown(strings.Join(strings.Fields(g.Reason), " ")))
		}
	}
	return b.String()
}

// tagOf is the release tag of a release or candidate, else the commit, for
// links into the repository as it was released.
func tagOf(in notesInput) string {
	if versionKind(in.Version) == kindDev {
		return in.Commit
	}
	return "v" + in.Version
}

// writeBudgets renders every budget with its pooled result from the
// benchmark summary records: per scenario, what the summary covers
// (comparison, ladder or soak) and its A/B verdict against its baseline.
func writeBudgets(b *strings.Builder, records []benchRecord, commit string) {
	if len(records) == 0 {
		b.WriteString("No benchmark summary records were provided for this build.\n")
		return
	}
	b.WriteString("| Scenario | Summary | A/B verdict | Budget | Value | Result | SLO | Verdict | Hardware | Commit |\n" +
		"|---|---|---|---|---|---|---|---|---|---|\n")
	other := false
	for _, r := range records {
		c := short(r.Identity.Commit)
		if r.Identity.Commit != commit {
			c += " (other commit)"
			other = true
		}
		ab := escapeMarkdown(r.Summary.Verdict)
		if r.Identity.Baseline != "" {
			ab += " against " + escapeMarkdown(r.Identity.Baseline)
		}
		lead := fmt.Sprintf("| %s | %s | %s |", escapeMarkdown(r.Identity.Scenario), escapeMarkdown(r.Summary.Of), ab)
		if len(r.Budgets) == 0 {
			fmt.Fprintf(b, "%s none | | | | %s | %s | %s |\n", lead,
				escapeMarkdown(r.Validity.Verdict), escapeMarkdown(r.Environment.HardwareProfile), c)
		}
		for _, bg := range r.Budgets {
			fmt.Fprintf(b, "%s %s | %s | %s | %s | %s | %s | %s |\n", lead,
				escapeMarkdown(bg.ID), escapeMarkdown(bg.Value), escapeMarkdown(bg.Result), escapeMarkdown(bg.SLO),
				escapeMarkdown(bg.Verdict), escapeMarkdown(r.Environment.HardwareProfile), c)
		}
	}
	if other {
		b.WriteString("\nRows marked (other commit) were measured on another commit than this release's.\n")
	}
}

// signerWorkflow is the reusable workflow whose OIDC identity signs every
// artifact and attestation, as gh attestation verify names it.
const signerWorkflow = "ravindu-rev/ruralz/.github/workflows/release-build.yml"

// sbomPredicate is the predicate type of the CycloneDX SBOM attestations.
const sbomPredicate = "https://cyclonedx.org/bom"

// cosign2Flag is the flag cosign 2.x needs to read the Sigstore bundles
// cosign 3 writes, for SHA256SUMS and for the image's OCI referrers.
// release-verify.yml runs every command of the Verification section's sh
// blocks with cosign 3.x, except the blocks that pass this flag, which
// are the 2.x variants.
const cosign2Flag = "--new-bundle-format"

// writeVerification renders how to verify the signatures, checksums,
// provenance and SBOM attestations, as commands release-verify.yml runs
// verbatim (cosign2Flag). cosign below 3.1.3 on 3.x, or 2.6.5 on 2.x, has a
// verification bypass (GHSA-fx35-mq7g-6g98).
func writeVerification(b *strings.Builder, in notesInput) {
	identity := signerIdentity + in.Version
	archive := ""
	if len(in.Archives) > 0 {
		archive = in.Archives[0].archiveName(in.Version)
	}
	signer := fmt.Sprintf(" \\\n     --certificate-identity %s \\\n     --certificate-oidc-issuer %s\n", identity, oidcIssuer)
	attest := func(subject, predicate string) {
		fmt.Fprintf(b, "   gh attestation verify %s --repo ravindu-rev/ruralz \\\n     --signer-workflow %s", subject, signerWorkflow)
		if predicate != "" {
			fmt.Fprintf(b, " \\\n     --predicate-type %s", predicate)
		}
		b.WriteString("\n")
	}
	b.WriteString("Every artifact is signed keylessly with Sigstore by the reusable workflow `release-build.yml` at `refs/tags/v" + in.Version +
		"` and logged in Rekor; its SLSA Build Level 3 provenance and CycloneDX SBOMs are GitHub artifact attestations of the same workflow. " +
		"Verify with the GitHub CLI and cosign 3.1.3 or newer on 3.x, or 2.6.5 or newer on 2.x, which fix a verification bypass (GHSA-fx35-mq7g-6g98).\n\n")
	b.WriteString("1. Check the signature of `SHA256SUMS`, then the files you downloaded (on macOS, `shasum -a 256 --ignore-missing -c SHA256SUMS`):\n\n")
	b.WriteString("   ```sh\n")
	b.WriteString("   cosign verify-blob SHA256SUMS --bundle " + signatureName + signer)
	b.WriteString("   sha256sum --ignore-missing -c SHA256SUMS\n   ```\n\n")
	b.WriteString("   cosign 2.x reads the Sigstore bundle only with `" + cosign2Flag + "`:\n\n")
	b.WriteString("   ```sh\n")
	b.WriteString("   cosign verify-blob SHA256SUMS --bundle " + signatureName + " " + cosign2Flag + signer)
	b.WriteString("   ```\n\n")
	b.WriteString("2. Check an archive's provenance, then its SBOM attestation:\n\n")
	b.WriteString("   ```sh\n")
	attest(archive, "")
	attest(archive, sbomPredicate)
	b.WriteString("   ```\n\n")
	for _, img := range in.Images {
		if !img.recorded() {
			b.WriteString("3. This build recorded no image.\n\n")
			continue
		}
		ref := img.Repository + "@" + img.Digest
		b.WriteString("3. Check the image by digest: its signature, then its provenance and SBOM attestations:\n\n")
		b.WriteString("   ```sh\n")
		b.WriteString("   cosign verify " + ref + signer)
		attest("oci://"+ref, "")
		attest("oci://"+ref, sbomPredicate)
		b.WriteString("   ```\n\n")
		b.WriteString("   cosign 2.x reads the image's signature only with `" + cosign2Flag + "`:\n\n")
		b.WriteString("   ```sh\n")
		b.WriteString("   cosign verify " + ref + " " + cosign2Flag + signer)
		b.WriteString("   ```\n\n")
	}
	b.WriteString("macOS sets a quarantine flag on a file a browser downloads, which stops an unnotarized binary; " +
		"Sigstore signs the release instead of Apple notarization. After the checks above, clear the flag from the unpacked binary with " +
		"`xattr -d com.apple.quarantine ruralzd` (or `ruralz`). A download with `curl` never carries the flag.\n")
}

// schemaNotes summarizes the schema, Plugin ABI and Control Stream changes
// of the range from..to (from "" for the first release).
func schemaNotes(ctx context.Context, git gitRunner, dir, from, to string) ([]string, error) {
	versions := func(ref string) ([]string, error) {
		out, err := git(ctx, dir, "ls-tree", "--name-only", "--end-of-options", ref, "api/schema/ruralz/")
		if err != nil {
			return nil, err
		}
		var vs []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				vs = append(vs, path.Base(l))
			}
		}
		slices.Sort(vs)
		return vs, nil
	}
	show := func(ref, p string) ([]byte, error) {
		return git(ctx, dir, "show", "--end-of-options", ref+":"+p)
	}
	toVersions, err := versions(to)
	if err != nil {
		return nil, err
	}
	var fromVersions []string
	if from != "" {
		if fromVersions, err = versions(from); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, v := range toVersions {
		p := "api/schema/ruralz/" + v + "/authoring.schema.json"
		newer, err := show(to, p)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(fromVersions, v) {
			doc, err := parseSchema(newer)
			if err != nil {
				return nil, fmt.Errorf("%s at %s: %w", p, to, err)
			}
			out = append(out, fmt.Sprintf("`ruralz/%s` is served for the first time, with the kinds %s.", v, strings.Join(quoted(schemaKinds(doc)), ", ")))
			continue
		}
		older, err := show(from, p)
		if err != nil {
			return nil, err
		}
		changes, err := schemaChanges(older, newer)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(changes) == 0 {
			out = append(out, fmt.Sprintf("`ruralz/%s`: no schema changes.", v))
		}
		for _, c := range changes {
			out = append(out, fmt.Sprintf("`ruralz/%s`: %s.", v, c))
		}
	}
	for _, v := range fromVersions {
		if !slices.Contains(toVersions, v) {
			out = append(out, fmt.Sprintf("`ruralz/%s` is no longer served.", v))
		}
	}
	for _, s := range []struct{ name, dir string }{
		{"ruralz.plugin.v1", "api/proto/ruralz/plugin/v1/"},
		{"ruralz.control.v1", "api/proto/ruralz/control/v1/"},
	} {
		files, err := git(ctx, dir, "ls-tree", "-r", "--name-only", "--end-of-options", to, s.dir)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(files)) == "" {
			out = append(out, fmt.Sprintf("`%s`: not served by this release.", s.name))
			continue
		}
		changed := files
		if from != "" {
			if changed, err = git(ctx, dir, "diff", "--name-only", from, to, "--", s.dir); err != nil {
				return nil, err
			}
		}
		names := strings.Fields(string(changed))
		if len(names) == 0 {
			out = append(out, fmt.Sprintf("`%s`: no changes.", s.name))
			continue
		}
		out = append(out, fmt.Sprintf("`%s`: changed definitions in %s.", s.name, strings.Join(quoted(names), ", ")))
	}
	return out, nil
}

// quoted wraps each string in backticks.
func quoted(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
}

// previousRelease names the release tag the notes start from when none is
// given: the newest vX.Y.Z tag before the release commit, never a release
// candidate's (an X.Y.Z release lists every change since the previous
// release, the candidates' included), or root when there is none.
func previousRelease(ctx context.Context, git gitRunner, dir, toCommit string) string {
	out, err := git(ctx, dir, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "--exclude", "v*-*", toCommit+"^")
	if tag := strings.TrimSpace(string(out)); err == nil && checkRef(tag) == nil {
		return tag
	}
	return rootRef
}

// runNotes writes the release notes of version for the range from..to; an
// empty from is the previous release.
func runNotes(ctx context.Context, o notesOptions, git gitRunner) error {
	if err := checkVersion(o.version); err != nil {
		return err
	}
	if o.out == "" {
		return fmt.Errorf("-out is required")
	}
	toCommit, err := resolveCommit(ctx, git, o.root, o.to)
	if err != nil {
		return err
	}
	if o.from == "" {
		o.from = previousRelease(ctx, git, o.root, toCommit)
	}
	fromCommit := ""
	if o.from != rootRef {
		if fromCommit, err = resolveCommit(ctx, git, o.root, o.from); err != nil {
			return err
		}
	}
	commits, err := logCommits(ctx, git, o.root, fromCommit, toCommit)
	if err != nil {
		return err
	}
	in := notesInput{Version: o.version, Commit: toCommit, Archives: artifacts(), Images: images(o.version), Bench: benchArchiveName(o.version)}
	if o.manifest != "" {
		m, err := readManifest(o.manifest)
		if err != nil {
			return err
		}
		if m.Version != o.version || m.Commit != toCommit {
			return fmt.Errorf("%s describes %s at %s, not %s at %s", o.manifest, m.Version, m.Commit, o.version, toCommit)
		}
		in.Images, in.Bench = m.Images, m.Bench.Name
	}
	if fromCommit != "" {
		in.From = o.from
	}
	classifyCommits(commits, &in)
	if in.Schema, err = schemaNotes(ctx, git, o.root, fromCommit, toCommit); err != nil {
		return err
	}
	if o.budgets != "" {
		if in.Records, err = readBudgets(o.budgets); err != nil {
			return err
		}
	}
	if o.skipped != "" {
		if in.Skipped, err = readSkipped(o.skipped); err != nil {
			return err
		}
	}
	return os.WriteFile(o.out, []byte(renderNotes(in)), 0o600)
}
