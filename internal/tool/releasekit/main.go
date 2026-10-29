// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command releasekit packages and audits a release in CI stage 12
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// artifacts" and "Release pipeline"), run by `make release-dry-run`:
//
//	releasekit package   [-C DIR] -dist DIR -notices DIR -out DIR -version V -commit C
//	releasekit checksums -dir DIR
//	releasekit notes     [-C DIR] -version V [-from REF] -to REF [-budgets DIR] [-skipped FILE] -out FILE
//	releasekit verify    [-C DIR] -manifest FILE
//
// package writes, into an absent or empty -out, one deterministic archive
// per artifact (sorted members, each dated the commit time, owner 0 without
// names; tar.gz, or zip on Windows) named <binary>_<version>_<os>_<arch>,
// holding the binary from -dist/<os>_<arch>/, the repository's LICENSE
// and README.md, NOTICE and THIRD_PARTY_LICENSES from depgate's -notices
// directory and, for linux ruralzd, deploy/systemd/; then a byte copy of
// each api/schema/ruralz/<apiVersion>/<view>.schema.json as
// ruralz-<apiVersion>-<view>.schema.json, and release-manifest.json. It
// refuses a binary not built with the release flags: the pinned
// toolchain without GOEXPERIMENT, CGO_ENABLED=0, -trimpath, no build tags,
// -s -w, from the commit (and, for a release or candidate, from a clean
// checkout of its tag).
//
// checksums writes SHA256SUMS (`sha256sum` format) over the archives and
// schema files the manifest lists, once each matches its manifest
// SHA-256 and no unlisted one is present.
//
// notes writes the Markdown release notes of the commits -from..-to
// (-from root for the first release; empty, the newest vX.Y.Z tag before
// -to, or root without one): artifacts, features, fixes,
// performance, dependency updates, deprecations (Deprecated: footers),
// upgrade notes (breaking titles and BREAKING CHANGE: footers), schema,
// Plugin ABI and Control Stream changes, the budget results of the
// ruralz.bench.v1 records under -budgets, verification steps and the
// gates a maintainer skipped (-skipped, a ruralz.skippedgates.v1 file).
//
// verify audits the release directory of -manifest against the manifest
// and the repository (the ADR-0002 release audit of every archive): sizes
// and digests, members, modes and timestamps, LICENSE, README.md and
// deploy/systemd/ equal to the repository's, NOTICE starting with the
// repository NOTICE, THIRD_PARTY_LICENSES and the CycloneDX 1.7 SBOM
// covering every module the binary links, each binary's release build,
// the schema assets, SHA256SUMS, the notes, the image plan and no stray
// file.
//
// Exit status: 0 success, 1 findings, 2 usage or I/O error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = "usage: releasekit package|checksums|notes|verify [flags]"

// run dispatches a subcommand and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("releasekit "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := "."
	var po packageOptions
	var no notesOptions
	var vo verifyOptions
	var dir string
	// required flags, in the order a missing one is reported.
	type flagRef struct {
		name, usage string
		value       *string
	}
	var required []flagRef
	need := func(refs ...flagRef) {
		for _, r := range refs {
			fs.StringVar(r.value, r.name, "", r.usage)
			required = append(required, r)
		}
	}
	switch cmd {
	case "package":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"dist", "build output: <os>_<arch>/<binary>", &po.dist},
			flagRef{"notices", "depgate -notices output: <binary>_<os>_<arch>/", &po.notices},
			flagRef{"out", "release directory to create; absent or empty", &po.out},
			flagRef{"version", "product version without the leading v", &po.version},
			flagRef{"commit", "full commit the binaries were built from", &po.commit})
	case "checksums":
		need(flagRef{"dir", "release directory", &dir})
	case "notes":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"version", "product version without the leading v", &no.version},
			flagRef{"to", "release commit or tag", &no.to},
			flagRef{"out", "notes file to write", &no.out})
		fs.StringVar(&no.from, "from", "", "previous release tag, or root; empty finds the newest vX.Y.Z tag before -to")
		fs.StringVar(&no.budgets, "budgets", "", "directory of ruralz.bench.v1 records; absent means none")
		fs.StringVar(&no.skipped, "skipped", "", "ruralz.skippedgates.v1 file; empty means no gate was skipped")
	case "verify":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"manifest", "release-manifest.json of the release directory", &vo.manifest})
	default:
		_, _ = fmt.Fprintf(stderr, "releasekit: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "releasekit %s: unexpected arguments %q\n", cmd, fs.Args())
		return 2
	}
	for _, r := range required {
		if *r.value == "" {
			_, _ = fmt.Fprintf(stderr, "releasekit %s: -%s is required\n", cmd, r.name)
			return 2
		}
	}

	var findings []string
	var err error
	switch cmd {
	case "package":
		po.root = root
		var m *manifest
		m, err = runPackage(ctx, po, packageDeps{git: runGit, facts: readBuildFacts})
		var fe *findingsError
		if errors.As(err, &fe) {
			findings, err = fe.findings, nil
		} else if err == nil {
			_, _ = fmt.Fprintf(stdout, "releasekit: packaged %d archives and %d schema files of %s into %s\n", len(m.Archives), len(m.Schemas), po.version, po.out)
		}
	case "checksums":
		if findings, err = runChecksums(dir); err == nil && len(findings) == 0 {
			_, _ = fmt.Fprintf(stdout, "releasekit: wrote %s in %s\n", checksumsName, dir)
		}
	case "notes":
		no.root = root
		if err = runNotes(ctx, no, runGit); err == nil {
			_, _ = fmt.Fprintf(stdout, "releasekit: wrote the release notes to %s\n", no.out)
		}
	case "verify":
		vo.root = root
		if findings, err = runVerify(vo, verifyDeps{facts: readBuildFacts}); err == nil && len(findings) == 0 {
			_, _ = fmt.Fprintf(stdout, "releasekit: %s verified\n", vo.manifest)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "releasekit %s: %v\n", cmd, err)
		return 2
	}
	for _, f := range findings {
		_, _ = fmt.Fprintln(stderr, f)
	}
	if len(findings) > 0 {
		_, _ = fmt.Fprintf(stderr, "releasekit %s: %d finding(s)\n", cmd, len(findings))
		return 1
	}
	return 0
}
