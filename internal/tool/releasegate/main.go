// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command releasegate makes the release decisions of CI stage 12
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// pipeline"; docs/engineering/03-testing-and-quality-strategy.md, "Release
// gates"), run by release.yml and release-verify.yml through the GitHub
// API:
//
//	releasegate tag    -tag TAG -commit SHA [-outputs FILE]
//	releasegate gates  -tag TAG -commit SHA -run ID [-records DIR] [-skipped FILE] [-summary FILE] [-C DIR]
//	releasegate latest -tag TAG
//	releasegate lines
//	releasegate image  -tag TAG -digest DIGEST [-check] [-repository REF] [-registry URL]
//
// tag checks the pushed tag, release.yml's first job: the name is vX.Y.Z or
// vX.Y.Z-rc.N, and it is an annotated tag on the commit whose signature
// GitHub verified, made with an SSH or OpenPGP signing key that a
// maintainer (repository role admin or maintain) registered. It appends
// version, kind, prerelease, previous, previous-version, latest and signer
// to -outputs ($GITHUB_OUTPUT); previous is the newest published release
// below the tag's X.Y.Z, whose linux/amd64 archive is the benchmark
// baseline.
//
// gates decides each release gate from the evidence of the testing
// document's second Release gates table: the jobs the release run -run
// re-ran, the open nightly-failure and fuzz-crasher issues, the seed file
// each closed fuzz-crasher issue names (present at -commit in the checkout
// -C), the ruralz.bench.v1 records of the Latency and soak jobs under
// -records, and the candidate tags and their release runs. It applies the
// skips a maintainer recorded on the tag's tracking issue, the issue
// titled with the tag that releasegate opens when absent and updates on
// every run with each gate's outcome, writes the ruralz.skippedgates.v1
// file of the skipped gates to -skipped for the release notes, and appends
// the outcome to -summary ($GITHUB_STEP_SUMMARY).
//
// latest prints whether the release TAG is above every published release,
// the only case in which the publish job marks it latest. lines prints, as
// a JSON array, the newest release tag of each supported line (N, N-1 and
// N-2), which release-verify.yml re-verifies weekly. image tags the
// verified image index DIGEST in the registry with X.Y.Z and X.Y for a
// release or X.Y.Z-rc.N for a candidate, copying the index unchanged; the
// immutable X.Y.Z or X.Y.Z-rc.N tag must be absent or already name DIGEST,
// so a tag is never re-pushed. -check only checks that.
//
// The GitHub API root, repository and server come from GITHUB_API_URL,
// GITHUB_REPOSITORY and GITHUB_SERVER_URL (-api and -repo override them),
// the token from GH_TOKEN or GITHUB_TOKEN; image signs in to the registry
// with that token as GITHUB_ACTOR.
//
// Exit status: 0 pass, 1 findings (a tag that is no release tag, a failed
// gate, a tag that names another digest), 2 usage, API or I/O error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, environ{getenv: os.Getenv, now: time.Now, client: &http.Client{Timeout: 2 * time.Minute}})
	stop()
	os.Exit(code)
}

// environ is what releasegate reads besides its flags; tests replace it.
type environ struct {
	getenv func(string) string
	now    func() time.Time
	client *http.Client
	// git runs git for the seed check of closed fuzz-crasher issues; nil
	// runs the git command.
	git gitRunner
}

const usage = "usage: releasegate tag|gates|latest|lines|image [flags]"

// usageError is a flag or argument problem: exit 2 after its message.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// run dispatches a subcommand and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env environ) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("releasegate "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	api := fs.String("api", "", "GitHub API root; default $GITHUB_API_URL or https://api.github.com")
	repo := fs.String("repo", "", "repository owner/name; default $GITHUB_REPOSITORY")
	var (
		tag, commit, outputs, records, skipped, summary, dir, digest, repository, registry string
		runID                                                                              string
		checkOnly                                                                          bool
	)
	switch cmd {
	case "tag":
		fs.StringVar(&tag, "tag", "", "the pushed tag, such as v0.1.0-rc.1")
		fs.StringVar(&commit, "commit", "", "the full commit the run builds ($GITHUB_SHA)")
		fs.StringVar(&outputs, "outputs", "", "append the step outputs to this file ($GITHUB_OUTPUT)")
	case "gates":
		fs.StringVar(&tag, "tag", "", "the release tag")
		fs.StringVar(&commit, "commit", "", "the tagged commit")
		fs.StringVar(&runID, "run", "", "the release run whose jobs the gates read ($GITHUB_RUN_ID)")
		fs.StringVar(&records, "records", "", "ruralz.bench.v1 records of the release's Latency and soak jobs")
		fs.StringVar(&skipped, "skipped", "", "write the ruralz.skippedgates.v1 file here")
		fs.StringVar(&summary, "summary", "", "append the outcome to this file ($GITHUB_STEP_SUMMARY)")
		fs.StringVar(&dir, "C", ".", "checkout of the tagged commit, for the seed files of closed fuzz-crasher issues")
	case "latest":
		fs.StringVar(&tag, "tag", "", "the release tag")
	case "lines":
	case "image":
		fs.StringVar(&tag, "tag", "", "the release or candidate tag")
		fs.StringVar(&digest, "digest", "", "the verified image index digest, sha256:<64 hex>")
		fs.BoolVar(&checkOnly, "check", false, "only check that the immutable tag is absent or names the digest")
		fs.StringVar(&repository, "repository", defaultImage, "the image repository")
		fs.StringVar(&registry, "registry", "", "registry API root; default https:// and the repository's host")
	default:
		_, _ = fmt.Fprintf(stderr, "releasegate: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "releasegate %s: unexpected arguments %q\n", cmd, fs.Args())
		return 2
	}
	required := map[string][]struct{ name, value string }{
		"tag":    {{"tag", tag}, {"commit", commit}},
		"gates":  {{"tag", tag}, {"commit", commit}, {"run", runID}},
		"latest": {{"tag", tag}},
		"image":  {{"tag", tag}, {"digest", digest}},
	}
	for _, r := range required[cmd] {
		if r.value == "" {
			_, _ = fmt.Fprintf(stderr, "releasegate %s: -%s is required\n", cmd, r.name)
			return 2
		}
	}
	if cmd == "image" {
		findings, err := runImage(ctx, imageOptions{
			tag: tag, digest: digest, check: checkOnly, repository: repository, registry: registry,
			user: env.getenv("GITHUB_ACTOR"), token: token(env),
		}, env.client, stdout)
		return finish(cmd, findings, err, stderr)
	}

	g, err := newGitHub(firstOf(*api, env.getenv("GITHUB_API_URL"), "https://api.github.com"), firstOf(*repo, env.getenv("GITHUB_REPOSITORY")), token(env), env.client)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "releasegate %s: %v\n", cmd, err)
		return 2
	}
	var findings []string
	switch cmd {
	case "tag":
		var r *tagResult
		if r, err = checkTag(ctx, g, tag, commit); err == nil {
			findings = r.Findings
			if len(findings) == 0 {
				_, _ = fmt.Fprintf(stdout, "releasegate: %s is a %s tag on %s signed by maintainer @%s\n", tag, r.Version.kind(), commit, r.Signer)
				if outputs != "" {
					err = appendOutputs(outputs, r)
				} else {
					err = writeOutputs(stdout, r)
				}
			}
		}
	case "gates":
		id, perr := strconv.ParseInt(runID, 10, 64)
		if perr != nil || id <= 0 {
			_, _ = fmt.Fprintf(stderr, "releasegate gates: -run %q is not a workflow run ID\n", runID)
			return 2
		}
		server := firstOf(env.getenv("GITHUB_SERVER_URL"), "https://github.com")
		git := env.git
		if git == nil {
			git = runGit
		}
		findings, err = runGates(ctx, g, gatesOptions{
			tag: tag, commit: commit, runID: id, records: records, skipped: skipped, summary: summary, dir: dir,
			runURL: fmt.Sprintf("%s/%s/actions/runs/%d", strings.TrimRight(server, "/"), g.repo, id),
		}, env.now(), git, stdout)
	case "latest":
		var latest bool
		if latest, err = latestRelease(ctx, g, tag); err == nil {
			_, _ = fmt.Fprintln(stdout, latest)
		}
	case "lines":
		var tags []string
		if tags, err = supportedLines(ctx, g); err == nil {
			_, _ = fmt.Fprintln(stdout, jsonArray(tags))
		}
	}
	return finish(cmd, findings, err, stderr)
}

// finish prints the findings or error and returns the exit status: usage,
// API and I/O errors exit 2, findings 1.
func finish(cmd string, findings []string, err error, stderr io.Writer) int {
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "releasegate %s: %v\n", cmd, err)
		return 2
	}
	for _, f := range findings {
		_, _ = fmt.Fprintln(stderr, f)
	}
	if len(findings) > 0 {
		_, _ = fmt.Fprintf(stderr, "releasegate %s: %d finding(s)\n", cmd, len(findings))
		return 1
	}
	return 0
}

// token is the GitHub token of the environment.
func token(env environ) string { return firstOf(env.getenv("GH_TOKEN"), env.getenv("GITHUB_TOKEN")) }

// firstOf returns the first non-empty string.
func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
