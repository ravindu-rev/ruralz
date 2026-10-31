// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command releasekit packages and audits a release in CI stage 12
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// artifacts", "Packaging and audit" and "Release pipeline"), run by
// `make release-dry-run` and `make release`:
//
//	releasekit image     -dir DIR [-metadata FILE]
//	releasekit package   [-C DIR] -dist DIR -notices DIR -out DIR -version V -commit C [-image DIR] [-bench DIR]
//	releasekit sbom      -manifest FILE -scans DIR
//	releasekit notes     [-C DIR] -version V [-from REF] -to REF [-budgets DIR] [-skipped FILE] [-manifest FILE] -out FILE
//	releasekit checksums -dir DIR
//	releasekit verify    [-C DIR] -manifest FILE [-image DIR]
//
// image checks the OCI image layout the image build wrote (one image
// index holding one image manifest per platform of the image plan, every
// blob matching its digest) and prints the index digest; with -metadata,
// the build's metadata file (docker buildx --metadata-file) must name the
// same index digest as pushed, so the pushed image is the audited one.
//
// package writes, into an absent or empty -out, one deterministic archive
// per artifact (sorted members, each dated the commit time, owner 0 without
// names; tar.gz, or zip on Windows) named <binary>_<version>_<os>_<arch>,
// holding the binary from -dist/<os>_<arch>/, the repository's LICENSE
// and README.md, NOTICE and THIRD_PARTY_LICENSES from depgate's -notices
// directory and, for linux ruralzd, deploy/systemd/; then a byte copy of
// each api/schema/ruralz/<apiVersion>/<view>.schema.json as
// ruralz-<apiVersion>-<view>.schema.json, the benchmark records archive
// ruralz_<version>_bench.tar.gz of the ruralz.bench.v1 records under -bench
// (with the histograms and CPU profiles they name), and
// release-manifest.json with the image digests of the -image layout. It
// refuses a binary not built with the release flags: the pinned
// toolchain without GOEXPERIMENT, CGO_ENABLED=0, -trimpath, no build tags,
// -s -w, from the commit (and, for a release or candidate, from a clean
// checkout of its tag); and a release or candidate without an image.
//
// sbom merges the CycloneDX 1.7 scans of each platform image,
// -scans/<os>_<arch>.cdx.json as Syft writes them, into the image SBOM the
// manifest names, beside the manifest: one document whose subject is the
// image index and whose components are each platform image with its
// packages.
//
// notes writes the Markdown release notes of the commits -from..-to
// (-from root for the first release; empty, the newest vX.Y.Z tag before
// -to, or root without one): artifacts with the image digests of
// -manifest, features, fixes, performance, dependency updates,
// deprecations (Deprecated: footers), upgrade notes (breaking titles and
// BREAKING CHANGE: footers), schema, Plugin ABI and Control Stream
// changes, every budget with its pooled result from the ruralz.bench.v1
// summary records under -budgets (the run records beside them are read and
// skipped), verification steps and the gates a maintainer skipped
// (-skipped, a ruralz.skippedgates.v1 file).
//
// checksums writes SHA256SUMS (`sha256sum` format) over every other file
// of the release directory (the archives and their SBOMs, the image SBOM,
// the schema files, release-manifest.json, NOTES.md and the benchmark
// records archive), once each file the manifest records matches its
// SHA-256 and no other file is present.
//
// verify audits the release directory of -manifest against the manifest
// and the repository (the ADR-0002 release audit of every archive): sizes
// and digests, members, modes and timestamps, LICENSE, README.md and
// deploy/systemd/ equal to the repository's, NOTICE starting with the
// repository NOTICE, THIRD_PARTY_LICENSES and the CycloneDX 1.7 SBOM
// covering every module the binary links, each binary's release build,
// the schema assets, the benchmark records archive, SHA256SUMS, the notes,
// the image plan with the image digests and, per platform, the image of
// the -image layout and its SBOM, a Sigstore bundle when present, and no
// stray file.
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

const usage = "usage: releasekit image|package|sbom|notes|checksums|verify [flags]"

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
	var imo imageOptions
	var so sbomOptions
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
	case "image":
		need(flagRef{"dir", "OCI image layout of the image build", &imo.dir})
		fs.StringVar(&imo.metadata, "metadata", "", "docker buildx --metadata-file of the push; its index digest must be the layout's")
	case "package":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"dist", "build output: <os>_<arch>/<binary>", &po.dist},
			flagRef{"notices", "depgate -notices output: <binary>_<os>_<arch>/", &po.notices},
			flagRef{"out", "release directory to create; absent or empty", &po.out},
			flagRef{"version", "product version without the leading v", &po.version},
			flagRef{"commit", "full commit the binaries were built from", &po.commit})
		fs.StringVar(&po.image, "image", "", "OCI image layout of the image build, whose digests the manifest records; empty records no image, which only a development build may lack")
		fs.StringVar(&po.bench, "bench", "", "ruralz.bench.v1 records of the release's Latency and soak jobs; empty or absent gives an empty records archive")
	case "sbom":
		need(flagRef{"manifest", "release-manifest.json of the release directory", &so.manifest},
			flagRef{"scans", "Syft's CycloneDX scan of each platform image: <os>_<arch>.cdx.json", &so.scans})
	case "checksums":
		need(flagRef{"dir", "release directory", &dir})
	case "notes":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"version", "product version without the leading v", &no.version},
			flagRef{"to", "release commit or tag", &no.to},
			flagRef{"out", "notes file to write", &no.out})
		fs.StringVar(&no.from, "from", "", "previous release tag, or root; empty finds the newest vX.Y.Z tag before -to")
		fs.StringVar(&no.budgets, "budgets", "", "directory of ruralz.bench.v1 records, whose summary records the notes list; absent means none")
		fs.StringVar(&no.skipped, "skipped", "", "ruralz.skippedgates.v1 file; empty means no gate was skipped")
		fs.StringVar(&no.manifest, "manifest", "", "release-manifest.json, whose image digests and records archive the notes name; empty names none")
	case "verify":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"manifest", "release-manifest.json of the release directory", &vo.manifest})
		fs.StringVar(&vo.image, "image", "", "OCI image layout of the image the manifest records")
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
	// asFindings moves the findings of a findingsError out of err.
	asFindings := func() {
		var fe *findingsError
		if errors.As(err, &fe) {
			findings, err = fe.findings, nil
		}
	}
	switch cmd {
	case "image":
		var digest string
		if digest, err = runImage(imo); err == nil {
			_, _ = fmt.Fprintln(stdout, digest)
		}
		asFindings()
	case "package":
		po.root = root
		var m *manifest
		m, err = runPackage(ctx, po, packageDeps{git: runGit, facts: readBuildFacts})
		asFindings()
		if err == nil && len(findings) == 0 {
			image := "no image"
			if m.Images[0].recorded() {
				image = "image " + m.Images[0].Digest
			}
			_, _ = fmt.Fprintf(stdout, "releasekit: packaged %d archives, %d schema files and %d benchmark records of %s with %s into %s\n",
				len(m.Archives), len(m.Schemas), m.Bench.Records, po.version, image, po.out)
		}
	case "sbom":
		var name string
		if name, err = runSBOM(so); err == nil {
			_, _ = fmt.Fprintf(stdout, "releasekit: wrote the image SBOM %s\n", name)
		}
		asFindings()
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
