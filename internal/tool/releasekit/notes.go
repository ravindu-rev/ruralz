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
		from = "`" + escapeMarkdown(in.From) + "`"
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
		"linux `ruralzd` archives add `deploy/systemd/`. `SHA256SUMS` covers every archive, the JSON Schema files " +
		"`ruralz-<apiVersion>-authoring.schema.json` and `ruralz-<apiVersion>-rendered.schema.json`, and " +
		"`" + manifestName + "`, which lists every archive's members and digests; " +
		"each archive's CycloneDX 1.7 SBOM is `<archive>.cdx.json`.\n\n")
	for _, img := range in.Images {
		if len(img.Tags) == 0 {
			fmt.Fprintf(&b, "Image `%s`: no tag is published for a development build.\n", img.Repository)
			continue
		}
		var tags []string
		for _, t := range img.Tags {
			tags = append(tags, "`"+img.Repository+":"+t+"`")
		}
		fmt.Fprintf(&b, "Image %s for %s; deployments should pin it by digest.\n", strings.Join(tags, " and "), strings.Join(img.Platforms, " and "))
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
		b.WriteString("| Gate | Skipped by | Role | Reason |\n|---|---|---|---|\n")
		for _, g := range in.Skipped {
			fmt.Fprintf(&b, "| %s | @%s | %s | %s |\n", escapeMarkdown(g.Gate), escapeMarkdown(g.By), g.Role,
				escapeMarkdown(strings.Join(strings.Fields(g.Reason), " ")))
		}
	}
	return b.String()
}

// writeBudgets renders the budget results of the benchmark records.
func writeBudgets(b *strings.Builder, records []benchRecord, commit string) {
	if len(records) == 0 {
		b.WriteString("No benchmark records were provided for this build.\n")
		return
	}
	b.WriteString("| Scenario | Budget | Value | Result | SLO | Verdict | Hardware | Commit |\n|---|---|---|---|---|---|---|---|\n")
	other := false
	for _, r := range records {
		c := short(r.Identity.Commit)
		if r.Identity.Commit != commit {
			c += " (other commit)"
			other = true
		}
		if len(r.Budgets) == 0 {
			fmt.Fprintf(b, "| %s | none | | | | %s | %s | %s |\n", escapeMarkdown(r.Identity.Scenario),
				escapeMarkdown(r.Validity.Verdict), escapeMarkdown(r.Environment.HardwareProfile), c)
		}
		for _, bg := range r.Budgets {
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", escapeMarkdown(r.Identity.Scenario),
				escapeMarkdown(bg.ID), escapeMarkdown(bg.Value), escapeMarkdown(bg.Result), escapeMarkdown(bg.SLO),
				escapeMarkdown(bg.Verdict), escapeMarkdown(r.Environment.HardwareProfile), c)
		}
	}
	if other {
		b.WriteString("\nRows marked (other commit) were measured on another commit than this release's.\n")
	}
}

// writeVerification renders how to verify the signatures, checksums,
// provenance and SBOM attestations. cosign below 3.1.3 on 3.x, or 2.6.5
// on 2.x, has a verification bypass (GHSA-fx35-mq7g-6g98).
func writeVerification(b *strings.Builder, in notesInput) {
	identity := signerIdentity + in.Version
	archive := ""
	if len(in.Archives) > 0 {
		archive = in.Archives[0].archiveName(in.Version)
	}
	b.WriteString("Every artifact is signed keylessly with Sigstore by the release workflow and logged in Rekor. " +
		"Verify with cosign 3.1.3 or newer on 3.x, or 2.6.5 or newer on 2.x, which fix a verification bypass (GHSA-fx35-mq7g-6g98).\n\n")
	b.WriteString("1. Check the signature of `SHA256SUMS`, then the files you downloaded:\n\n")
	b.WriteString("   ```sh\n")
	fmt.Fprintf(b, "   cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \\\n     --certificate-identity %s \\\n     --certificate-oidc-issuer %s\n", identity, oidcIssuer)
	b.WriteString("   sha256sum --ignore-missing -c SHA256SUMS\n   ```\n\n")
	b.WriteString("2. Check an archive's SLSA Build Level 3 provenance and SBOM attestations:\n\n")
	b.WriteString("   ```sh\n")
	fmt.Fprintf(b, "   gh attestation verify %s --repo ravindu-rev/ruralz\n", archive)
	b.WriteString("   ```\n\n")
	b.WriteString("3. Check an image by digest: its signature, then its provenance and SBOM attestations:\n\n")
	b.WriteString("   ```sh\n")
	for _, img := range in.Images {
		fmt.Fprintf(b, "   cosign verify %s@sha256:<digest> \\\n     --certificate-identity %s \\\n     --certificate-oidc-issuer %s\n", img.Repository, identity, oidcIssuer)
		fmt.Fprintf(b, "   gh attestation verify oci://%s@sha256:<digest> --repo ravindu-rev/ruralz\n", img.Repository)
	}
	b.WriteString("   ```\n")
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
	in := notesInput{Version: o.version, Commit: toCommit, Archives: artifacts(), Images: images(o.version)}
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
