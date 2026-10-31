// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// gatesOptions are the flags of `releasegate gates`.
type gatesOptions struct {
	tag, commit string
	runID       int64
	// runURL links the release run in the tracking issue.
	runURL string
	// records holds the Latency and soak jobs' ruralz.bench.v1 records.
	records string
	// skipped receives the ruralz.skippedgates.v1 file; summary the
	// Markdown outcome.
	skipped, summary string
	// dir is the checkout of the tagged commit.
	dir string
}

// Gate outcomes.
const (
	outcomePassed        = "passed"
	outcomeFailed        = "failed"
	outcomeSkipped       = "skipped"
	outcomeNotApplicable = "not applicable"
)

// gateResult is one gate's decision.
type gateResult struct {
	Gate     string
	Outcome  string
	Findings []string
	// Skip is the maintainer's skip of a failed gate.
	Skip *skip
}

// evidence is what the gates read, gathered once.
type evidence struct {
	jobs           []workflowJob
	nightly        []issue
	crashersOpen   []issue
	crashersClosed []issue
	records        []benchRecord
	// prev is the previous release the macro comparisons compare with;
	// nil for the first release.
	prev *baseline
	// missingSeeds are the closed fuzz-crasher issues whose seed file is
	// absent at the commit, or that name none.
	missingSeeds []string
	// candidate is the Release candidate gate's outcome.
	candidateApplies bool
	candidate        []string
}

// skippedFormat identifies the skipped-gates file of the release notes.
const skippedFormat = "ruralz.skippedgates.v1"

// skippedFile is the ruralz.skippedgates.v1 document: the gates a
// maintainer skipped, in gate order, with who skipped each and why.
type skippedFile struct {
	Format  string `json:"format"`
	Skipped []skip `json:"skipped"`
}

// runGates decides every release gate of the tag at commit, records the
// outcome on the tracking issue, writes the skipped-gates file and the
// summary, and returns one finding per failed gate.
func runGates(ctx context.Context, g *github, o gatesOptions, now time.Time, git gitRunner, stdout io.Writer) ([]string, error) {
	v, err := parseTag(o.tag)
	if err != nil {
		return nil, &usageError{err.Error()}
	}
	if !commitPattern().MatchString(o.commit) {
		return nil, &usageError{fmt.Sprintf("commit %q is not a full lower-case SHA-1", o.commit)}
	}
	ev, err := gather(ctx, g, v, o, now, git)
	if err != nil {
		return nil, err
	}
	iss, err := ensureTrackingIssue(ctx, g, o.tag, trackingBody(o, nil, nil, "Deciding the release gates."))
	if err != nil {
		return nil, err
	}
	skips, notes, err := readSkips(ctx, g, iss)
	if err != nil {
		return nil, err
	}
	results, notes := decide(v, o.commit, ev, skips, notes)
	body := trackingBody(o, results, notes, "")
	if err := updateTrackingIssue(ctx, g, iss, body); err != nil {
		return nil, err
	}
	if o.summary != "" {
		if err := appendFile(o.summary, body); err != nil {
			return nil, err
		}
	}
	var findings []string
	file := skippedFile{Format: skippedFormat, Skipped: []skip{}}
	for _, r := range results {
		_, _ = fmt.Fprintf(stdout, "releasegate: %s: %s\n", r.Gate, r.Outcome)
		switch r.Outcome {
		case outcomeFailed:
			findings = append(findings, fmt.Sprintf("gate %q failed: %s", r.Gate, strings.Join(r.Findings, "; ")))
		case outcomeSkipped:
			file.Skipped = append(file.Skipped, *r.Skip)
		}
	}
	if len(findings) == 0 && o.skipped != "" {
		data, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(o.skipped, append(data, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	return findings, nil
}

// gather reads the evidence of every gate.
func gather(ctx context.Context, g *github, v version, o gatesOptions, now time.Time, git gitRunner) (*evidence, error) {
	var ev evidence
	var err error
	if ev.jobs, err = listIn(ctx, g, g.repoPath("/actions/runs/%d/jobs?filter=latest", o.runID),
		func(p *jobsPage) []workflowJob { return p.Jobs }); err != nil {
		return nil, err
	}
	if ev.nightly, err = labeledIssues(ctx, g, labelNightly, "open"); err != nil {
		return nil, err
	}
	if ev.crashersOpen, err = labeledIssues(ctx, g, labelCrasher, "open"); err != nil {
		return nil, err
	}
	if ev.crashersClosed, err = labeledIssues(ctx, g, labelCrasher, "closed"); err != nil {
		return nil, err
	}
	for _, c := range ev.crashersClosed {
		seed, ok := crasherSeed(c.Body)
		if !ok {
			ev.missingSeeds = append(ev.missingSeeds, fmt.Sprintf("closed fuzz-crasher issue #%d names no seed file", c.Number))
			continue
		}
		present, err := fileAt(ctx, git, o.dir, o.commit, seed)
		if err != nil {
			return nil, err
		}
		if !present {
			ev.missingSeeds = append(ev.missingSeeds, fmt.Sprintf("closed fuzz-crasher issue #%d: its regression seed %s is not at %s", c.Number, seed, short(o.commit)))
		}
	}
	if o.records != "" {
		if ev.records, err = readRecords(o.records); err != nil {
			return nil, &usageError{err.Error()}
		}
	}
	published, err := publishedReleases(ctx, g)
	if err != nil {
		return nil, err
	}
	if p, ok := previousRelease(v, published); ok {
		c, found, err := tagCommit(ctx, g, p.tag())
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("the previous release %s has no tag", p.tag())
		}
		ev.prev = &baseline{Tag: p.tag(), Commit: c}
	}
	if ev.candidateApplies, ev.candidate, err = candidateFindings(ctx, g, v, o.commit, now); err != nil {
		return nil, err
	}
	return &ev, nil
}

// decide applies each gate's rule to the evidence, then the skips: a
// failed gate a maintainer skipped is skipped, except the Benchmarks gate
// of 0.1.0. It returns the results in gate order and the notes on skips
// that do not count.
func decide(v version, commit string, ev *evidence, skips map[string]skip, notes []string) ([]gateResult, []string) {
	sels := gateJobs()
	var results []gateResult
	for _, gate := range gates() {
		r := gateResult{Gate: gate}
		switch gate {
		case gateCI:
			r.Findings = ciFindings(ev.jobs)
			for _, i := range ev.nightly {
				r.Findings = append(r.Findings, fmt.Sprintf("nightly-failure issue #%d is open: %s", i.Number, i.Title))
			}
		case gateFuzzing:
			r.Findings = selectionFindings(ev.jobs, sels[gate])
			for _, i := range ev.crashersOpen {
				r.Findings = append(r.Findings, fmt.Sprintf("fuzz-crasher issue #%d is open: %s", i.Number, i.Title))
			}
			r.Findings = append(r.Findings, ev.missingSeeds...)
		case gateBenchmarks:
			r.Findings = append(selectionFindings(ev.jobs, sels[gate]), benchmarkFindings(ev.records, commit, ev.prev)...)
		case gateCandidate:
			if !ev.candidateApplies {
				r.Outcome = outcomeNotApplicable
			}
			r.Findings = ev.candidate
		default:
			r.Findings = selectionFindings(ev.jobs, sels[gate])
		}
		if r.Outcome == "" {
			r.Outcome = outcomePassed
			if len(r.Findings) > 0 {
				r.Outcome = outcomeFailed
			}
		}
		s, skipped := skips[gate]
		switch {
		case !skipped:
		case r.Outcome != outcomeFailed:
			notes = append(notes, fmt.Sprintf("%s: @%s's skip is unused: the gate %s", gate, s.By, r.Outcome))
		case gate == gateBenchmarks && v.benchmarksUnskippable():
			notes = append(notes, fmt.Sprintf("%s: @%s's skip does not count: no maintainer skips the Benchmarks gate of 0.1.0, which waits for RH-1 and passing budgets", gate, s.By))
		default:
			r.Outcome, r.Skip = outcomeSkipped, &s
		}
		results = append(results, r)
	}
	return results, notes
}

// maxFindingsShown bounds the findings the tracking issue lists per gate,
// so its body stays below GitHub's 65,536 characters.
const maxFindingsShown = 40

// trackingBody renders the tracking issue: each gate's outcome and
// findings, the skips and how to add one. A nil results says the gates are
// being decided (status).
func trackingBody(o gatesOptions, results []gateResult, notes []string, status string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release gates of `%s` at commit `%s`, decided by `internal/tool/releasegate` in [run %d](%s) "+
		"(docs/engineering/03-testing-and-quality-strategy.md, \"Release gates\").\n\n", o.tag, o.commit, o.runID, o.runURL)
	if results == nil {
		b.WriteString(status + "\n")
		return b.String()
	}
	b.WriteString("| Gate | Outcome |\n|---|---|\n")
	for _, r := range results {
		out := r.Outcome
		if r.Skip != nil {
			out = fmt.Sprintf("skipped by @%s (%s)", r.Skip.By, r.Skip.Role)
		}
		fmt.Fprintf(&b, "| %s | %s |\n", r.Gate, out)
	}
	for _, r := range results {
		if len(r.Findings) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n", r.Gate)
		for i, f := range r.Findings {
			if i == maxFindingsShown {
				fmt.Fprintf(&b, "- and %d more, in the run's log\n", len(r.Findings)-maxFindingsShown)
				break
			}
			b.WriteString("- " + escape(f) + "\n")
		}
	}
	var skipped []gateResult
	for _, r := range results {
		if r.Skip != nil {
			skipped = append(skipped, r)
		}
	}
	if len(skipped) > 0 {
		b.WriteString("\n### Skipped gates\n\n")
		for _, r := range skipped {
			fmt.Fprintf(&b, "- %s: @%s (%s): %s\n", r.Gate, r.Skip.By, r.Skip.Role, escape(oneLine(r.Skip.Reason)))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n### Skips that do not count\n\n")
		for _, n := range notes {
			b.WriteString("- " + escape(n) + "\n")
		}
	}
	b.WriteString("\n### Skipping a gate\n\n" +
		"A maintainer (repository role admin or maintain), the author included, skips a failed gate with a release-notes entry: " +
		"apply the label `" + skipPrefix + "<gate>`, such as `" + skipPrefix + gateBenchmarks + "`, comment `" + skipPrefix + "<gate>: <reason>` " +
		"on this issue, then re-run the release gates job. No maintainer skips the Benchmarks gate of 0.1.0.\n")
	return b.String()
}

// escape keeps finding text from adding Markdown markup or links.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_[]<>|~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// oneLine collapses white space, so a reason stays on its list line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// short is the 12-character form of a commit.
func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// appendFile appends text to path, creating it.
func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The summary file given on the command line, $GITHUB_STEP_SUMMARY in CI.
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// fileAt reports whether the repository at dir holds the file p at
// commit.
func fileAt(ctx context.Context, git gitRunner, dir, commit, p string) (bool, error) {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." {
		return false, nil
	}
	out, err := git(ctx, dir, "ls-tree", "--name-only", "--end-of-options", commit, "--", p)
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Split(strings.TrimSpace(string(out)), "\n"), p), nil
}
