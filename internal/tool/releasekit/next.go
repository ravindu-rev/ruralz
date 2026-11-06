// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// releaseVersion is a release's version, MAJOR.MINOR.PATCH, which its tag
// vMAJOR.MINOR.PATCH names.
type releaseVersion struct {
	Major, Minor, Patch int
}

// parseReleaseTag parses a release tag, vX.Y.Z without leading zeros; ok is
// false for any other name, a pre-release or build metadata included.
func parseReleaseTag(tag string) (releaseVersion, bool) {
	m := regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`).FindStringSubmatch(tag)
	if m == nil {
		return releaseVersion{}, false
	}
	var v releaseVersion
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// String is the version without the leading v.
func (v releaseVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// tag is the version's release tag.
func (v releaseVersion) tag() string { return "v" + v.String() }

// line is the version's release line, X.Y.
func (v releaseVersion) line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// compareVersions orders release versions.
func compareVersions(a, b releaseVersion) int {
	return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
}

// change is how far a commit moves the version; of a range of commits,
// the highest change counts.
type change int

// Changes, lowest first.
const (
	changeNone change = iota
	changePatch
	changeMinor
	changeBreaking
)

// String names the change for the log.
func (c change) String() string {
	switch c {
	case changePatch:
		return "fix, performance or dependency"
	case changeMinor:
		return "feature"
	case changeBreaking:
		return "breaking"
	default:
		return "no releasing"
	}
}

// commitChange classifies one non-merge commit by its Conventional Commits
// title and footers (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release pipeline"): a breaking change is a title with "!" or a
// BREAKING CHANGE (or BREAKING-CHANGE) footer, in a commit of any type; a
// feature is feat; a patch is a fix (fix), a performance change (perf) or
// a dependency update (build(deps), Dependabot's included), each of which
// changes what ships. Every other commit changes nothing: docs, test, ci
// (Dependabot's ci(deps) action updates included), build of any other
// scope, chore, refactor, revert, and a title that is no Conventional
// Commits title.
func commitChange(c commit) change {
	t, ok := parseTitle(c.Subject)
	switch {
	case ok && t.Breaking, len(footers(c.Body, footerBreaking, footerBreakingH)) > 0:
		return changeBreaking
	case !ok:
		return changeNone
	case t.Type == "feat":
		return changeMinor
	case t.Type == "fix", t.Type == "perf", t.Type == "build" && t.Scope == "deps":
		return changePatch
	default:
		return changeNone
	}
}

// firstVersion is the version of the first release.
func firstVersion() releaseVersion { return releaseVersion{Minor: 1} }

// bump returns the version after v that a change makes: a breaking change
// moves the major from 1.0.0 on and, while the major is 0, the minor, as a
// feature does; a fix, performance change or dependency update moves the
// patch.
func (v releaseVersion) bump(c change) releaseVersion {
	switch {
	case c == changeBreaking && v.Major > 0:
		return releaseVersion{Major: v.Major + 1}
	case c == changeBreaking, c == changeMinor:
		return releaseVersion{Major: v.Major, Minor: v.Minor + 1}
	case c == changePatch:
		return releaseVersion{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	default:
		return v
	}
}

// footerReleaseAs is the footer whose value, X.Y.Z, raises the next
// version to X.Y.Z, as for 1.0.0.
const footerReleaseAs = "Release-As"

// releaseAs is one Release-As footer of the range.
type releaseAs struct {
	commit commit
	value  string
}

// versionPlan is what the non-merge commits since the previous release
// make of the next version.
type versionPlan struct {
	// next is the version due; ok is false when the commits release
	// nothing.
	next releaseVersion
	ok   bool
	// top is the commits' highest change and first the oldest commit
	// making it.
	top   change
	first commit
	// footer is the Release-As footer that sets next, when one is above
	// the version the commits' changes make. Otherwise outgrown is the
	// highest valid footer, which is not above that version and so sets
	// nothing.
	footer, outgrown *releaseAs
	// refused are the Release-As footers that stop the release, as
	// findings.
	refused []string
}

// planVersion returns the plan of the non-merge commits since the previous
// release, oldest first, prev nil before the first release
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// pipeline"):
//   - The commits' highest change moves the previous release, and the
//     first release is 0.1.0 whatever the change.
//   - A Release-As: X.Y.Z footer above that version, or in a range whose
//     commits release nothing, raises the next version to X.Y.Z; of several
//     footers, the highest counts. A footer at or below the version the
//     changes make sets nothing, so a release never carries a change that
//     its version does not allow.
//   - A footer whose value is no version X.Y.Z, or that is not above the
//     previous release, is refused, unless a valid footer follows it in
//     the range: merging a commit with a valid footer is how a refused one
//     is replaced.
func planVersion(prev *releaseVersion, commits []commit) versionPlan {
	type candidate struct {
		footer  releaseAs
		version releaseVersion
		// parsed reports a version X.Y.Z; valid one above prev too.
		parsed, valid bool
	}
	var p versionPlan
	var candidates []candidate
	for _, c := range commits {
		if ch := commitChange(c); ch > p.top {
			p.top, p.first = ch, c
		}
		for _, value := range footersFold(c.Body, footerReleaseAs) {
			f := candidate{footer: releaseAs{commit: c, value: value}}
			f.version, f.parsed = parseReleaseTag("v" + value)
			f.valid = f.parsed && (prev == nil || compareVersions(f.version, *prev) > 0)
			candidates = append(candidates, f)
		}
	}
	lastValid := -1
	for i, f := range candidates {
		if f.valid {
			lastValid = i
		}
	}
	var highest *candidate
	for i, f := range candidates {
		where := fmt.Sprintf("%s %q", short(f.footer.commit.Hash), f.footer.commit.Subject)
		switch {
		case f.valid:
			if highest == nil || compareVersions(f.version, highest.version) > 0 {
				highest = &candidates[i]
			}
		case i < lastValid:
			// A valid footer of a later commit replaces it.
		case !f.parsed:
			p.refused = append(p.refused, fmt.Sprintf("%s: its Release-As footer %q is no version X.Y.Z; a later commit's valid Release-As footer replaces it", where, f.footer.value))
		default:
			p.refused = append(p.refused, fmt.Sprintf("%s: Release-As %s is not above the previous release %s; a later commit's valid Release-As footer replaces it", where, f.footer.value, prev.tag()))
		}
	}
	if len(p.refused) > 0 {
		return p
	}
	switch {
	case p.top == changeNone:
	case prev == nil:
		p.next, p.ok = firstVersion(), true
	default:
		p.next, p.ok = prev.bump(p.top), true
	}
	switch {
	case highest == nil:
	case !p.ok || compareVersions(highest.version, p.next) > 0:
		p.next, p.ok, p.footer = highest.version, true, &highest.footer
	default:
		p.outgrown = &highest.footer
	}
	return p
}

// reason explains a plan that releases, for the log.
func (p versionPlan) reason(since string) string {
	if p.footer != nil {
		return fmt.Sprintf("release %s: the Release-As footer of %s %q sets it", p.next, short(p.footer.commit.Hash), p.footer.commit.Subject)
	}
	r := fmt.Sprintf("release %s: a %s change since %s, first in %s %q", p.next, p.top, since, short(p.first.Hash), p.first.Subject)
	if p.outgrown != nil {
		r += fmt.Sprintf("; the Release-As %s footer of %s %q is not above it", p.outgrown.value, short(p.outgrown.commit.Hash), p.outgrown.commit.Subject)
	}
	return r
}

// releaseTag is a release tag and the commit it names.
type releaseTag struct {
	Name    string
	Version releaseVersion
	Commit  string
}

// releaseTags are a repository's release tags as one commit sees them.
type releaseTags struct {
	// all holds every release tag; at those naming the commit, before
	// the others the commit contains, after the others containing it.
	all, at, before, after []releaseTag
}

// previous is the highest release tag the commit contains besides its own:
// the previous release.
func (t releaseTags) previous() (releaseTag, bool) {
	if len(t.before) == 0 {
		return releaseTag{}, false
	}
	return slices.MaxFunc(t.before, func(a, b releaseTag) int { return compareVersions(a.Version, b.Version) }), true
}

// tagList lists the tags' names, in order.
func tagList(tags []releaseTag) string {
	var out []string
	for _, t := range tags {
		out = append(out, t.Name)
	}
	return strings.Join(out, ", ")
}

// errShallow is the error of a shallow clone, whose missing history would
// hide commits and tags.
var errShallow = errors.New("the repository is a shallow clone: the release tags and commits need its whole history (git fetch --unshallow --tags)")

// readReleaseTags reads every release tag of the repository, vX.Y.Z, with
// the commit it names, sorted by version, and sorts them by their place
// relative to commit: naming it, contained in it, or containing it. A
// shallow clone, which hides commits and tags, is an error, as is a release
// tag that names no commit.
func readReleaseTags(ctx context.Context, git gitRunner, dir, commit string) (releaseTags, error) {
	if err := checkCommit(commit); err != nil {
		return releaseTags{}, err
	}
	out, err := git(ctx, dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return releaseTags{}, err
	}
	if strings.TrimSpace(string(out)) != "false" {
		return releaseTags{}, errShallow
	}
	out, err = git(ctx, dir, "for-each-ref", "--format=%(refname:strip=2)%09%(objecttype)%09%(objectname)%09%(*objecttype)%09%(*objectname)", "refs/tags")
	if err != nil {
		return releaseTags{}, err
	}
	var t releaseTags
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSuffix(line, "\r"); line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			return releaseTags{}, fmt.Errorf("unexpected git for-each-ref line %q", line)
		}
		v, ok := parseReleaseTag(f[0])
		if !ok {
			continue
		}
		target := f[2]
		switch {
		case f[1] == "commit":
		case f[1] == "tag" && f[3] == "commit":
			target = f[4]
		default:
			return releaseTags{}, fmt.Errorf("release tag %s names no commit", f[0])
		}
		t.all = append(t.all, releaseTag{Name: f[0], Version: v, Commit: target})
	}
	slices.SortFunc(t.all, func(a, b releaseTag) int { return compareVersions(a.Version, b.Version) })
	merged, err := tagNames(ctx, git, dir, "--merged="+commit)
	if err != nil {
		return releaseTags{}, err
	}
	containing, err := tagNames(ctx, git, dir, "--contains="+commit)
	if err != nil {
		return releaseTags{}, err
	}
	for _, r := range t.all {
		switch {
		case r.Commit == commit:
			t.at = append(t.at, r)
		case merged[r.Name]:
			t.before = append(t.before, r)
		case containing[r.Name]:
			t.after = append(t.after, r)
		}
	}
	return t, nil
}

// tagNames returns the names of the tags git for-each-ref selects with
// filter, such as --merged=<commit>.
func tagNames(ctx context.Context, git gitRunner, dir, filter string) (map[string]bool, error) {
	out, err := git(ctx, dir, "for-each-ref", filter, "--format=%(refname:strip=2)", "refs/tags")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// nextOptions are the flags of `releasekit next`.
type nextOptions struct {
	// root is the repository: a clone with its whole history and tags.
	root string
	// to is the commit to release.
	to string
	// outputs, when set, receives the step outputs ($GITHUB_OUTPUT).
	outputs string
}

// nextResult is the release decision for one commit.
type nextResult struct {
	Commit string
	// Version and Tag name the release; both are empty when the commit
	// releases nothing.
	Version, Tag string
	// Previous is the previous release's tag; empty before the first
	// release.
	Previous string
	// Tagged reports that Tag names Commit already: an earlier attempt of
	// its release run pushed it, and the run resumes.
	Tagged bool
	// Reason explains the decision.
	Reason string
}

// writeOutputs writes the decision as key=value lines, the $GITHUB_OUTPUT
// format: version, tag and previous, each empty when absent, and tagged.
func (r nextResult) writeOutputs(w io.Writer) error {
	_, err := fmt.Fprintf(w, "version=%s\ntag=%s\nprevious=%s\ntagged=%t\n", r.Version, r.Tag, r.Previous, r.Tagged)
	return err
}

// appendOutputs appends the decision to the file path, created when absent.
func (r nextResult) appendOutputs(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The output file given on the command line, $GITHUB_OUTPUT in CI.
	if err != nil {
		return err
	}
	if err := r.writeOutputs(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runNext decides what a push of the commit o.to to main releases
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// pipeline"): the version that the Conventional Commits titles and footers
// of the non-merge commits since the previous release make (planVersion),
// 0.1.0 for the first, or nothing. A commit that a release tag already
// names resumes that release, which a failed run left behind; a commit that
// a later release contains releases nothing; a refused Release-As footer
// and a release tag naming another commit for the version due are
// findings, so a tag is never created twice.
func runNext(ctx context.Context, o nextOptions, git gitRunner) (nextResult, error) {
	commit, err := resolveCommit(ctx, git, o.root, o.to)
	if err != nil {
		return nextResult{}, err
	}
	tags, err := readReleaseTags(ctx, git, o.root, commit)
	if err != nil {
		return nextResult{}, err
	}
	r := nextResult{Commit: commit}
	var prev *releaseVersion
	from, since := "", "the first commit"
	if p, ok := tags.previous(); ok {
		prev, from, since, r.Previous = &p.Version, p.Commit, p.Name, p.Name
	}
	commits, err := logCommits(ctx, git, o.root, from, commit)
	if err != nil {
		return nextResult{}, err
	}
	plan := planVersion(prev, commits)
	due := "no release"
	if plan.ok {
		due = plan.next.tag()
	}
	switch {
	case len(tags.at) > 1:
		return nextResult{}, &findingsError{[]string{fmt.Sprintf("%s carries the release tags %s; one commit is at most one release", short(commit), tagList(tags.at))}}
	case len(tags.at) == 1:
		at := tags.at[0]
		if len(plan.refused) > 0 {
			return nextResult{}, &findingsError{plan.refused}
		}
		if !plan.ok || plan.next != at.Version {
			return nextResult{}, &findingsError{[]string{fmt.Sprintf("%s names %s, but the commits since %s make %s", at.Name, short(commit), since, due)}}
		}
		r.Version, r.Tag, r.Tagged = at.Version.String(), at.Name, true
		r.Reason = fmt.Sprintf("%s names %s already: its release resumes", at.Name, short(commit))
	case len(tags.after) > 0:
		r.Reason = fmt.Sprintf("nothing to release: %s, of a later commit, contains %s", tags.after[0].Name, short(commit))
	case len(plan.refused) > 0:
		return nextResult{}, &findingsError{plan.refused}
	case !plan.ok:
		r.Reason = fmt.Sprintf("nothing to release: no feat, fix, perf, build(deps), breaking change or Release-As footer since %s", since)
	default:
		for _, t := range tags.all {
			if t.Version == plan.next {
				return nextResult{}, &findingsError{[]string{fmt.Sprintf("%s is due for %s, but it names %s, which neither contains it nor follows it", t.Name, short(commit), short(t.Commit))}}
			}
		}
		r.Version, r.Tag = plan.next.String(), plan.next.tag()
		r.Reason = plan.reason(since)
	}
	if o.outputs != "" {
		if err := r.appendOutputs(o.outputs); err != nil {
			return nextResult{}, err
		}
	}
	return r, nil
}
