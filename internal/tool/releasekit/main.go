// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command releasekit decides, packages, audits and publishes a release
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// pipeline", "Release artifacts" and "Packaging and audit"), run by
// release.yml, release-build.yml, `make release-dry-run` and `make release`:
//
//	releasekit next      [-C DIR] [-to REF] [-outputs FILE]
//	releasekit image     -dir DIR [-metadata FILE]
//	releasekit package   [-C DIR] -dist DIR -notices DIR -out DIR -version V -commit C [-image DIR]
//	releasekit sbom      -manifest FILE -scans DIR
//	releasekit notes     [-C DIR] -version V [-from REF] -to REF [-manifest FILE] -out FILE
//	releasekit checksums -dir DIR
//	releasekit verify    [-C DIR] -manifest FILE [-image DIR]
//	releasekit latest    -version V [-api URL] [-repo OWNER/NAME]
//	releasekit tag-image -version V -digest DIGEST [-check] [-repository REF] [-registry URL] [-api URL] [-repo OWNER/NAME]
//
// next decides what a push of the commit -to to main releases, from the
// Conventional Commits titles and footers of the non-merge commits since
// the previous release, the highest vX.Y.Z tag the commit contains besides
// its own: a breaking change ("!" or a BREAKING CHANGE footer) moves the
// minor while the major is 0 and the major from 1.0.0, feat the minor, fix
// and perf the patch, anything else nothing; the first release is 0.1.0. A
// commit that a release tag already names resumes that release; one that a
// later release contains releases nothing. It prints the decision and
// writes version, tag, previous and tagged as key=value lines to -outputs
// ($GITHUB_OUTPUT), or to standard output without it; version is empty
// when nothing is released. A release tag of the version due on another
// commit is a finding.
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
// ruralz-<apiVersion>-<view>.schema.json, and release-manifest.json with
// the image digests of the -image layout. It refuses a binary not built
// with the release flags: the pinned toolchain without GOEXPERIMENT,
// CGO_ENABLED=0, -trimpath, no build tags, -s -w, from the commit (and,
// for a release, from a clean checkout of its tag); and a release without
// an image.
//
// sbom merges the CycloneDX 1.7 scans of each platform image,
// -scans/<os>_<arch>.cdx.json as Syft writes them, into the image SBOM the
// manifest names, beside the manifest: one document whose subject is the
// image index and whose components are each platform image with its
// packages.
//
// notes writes the Markdown release notes of the commits -from..-to
// (-from root for the first release; empty, the previous release of -to,
// as next reads it, or root without one): artifacts with the image digests
// of -manifest, features, fixes, performance, dependency updates,
// deprecations (Deprecated: footers), upgrade notes (breaking titles and
// BREAKING CHANGE: footers), schema, Plugin ABI and Control Stream changes,
// and verification steps.
//
// checksums writes SHA256SUMS (`sha256sum` format) over every other file
// of the release directory (the archives and their SBOMs, the image SBOM,
// the schema files, release-manifest.json and NOTES.md), once each file
// the manifest records matches its SHA-256 and no other file is present.
//
// verify audits the release directory of -manifest against the manifest
// and the repository (the ADR-0002 release audit of every archive): sizes
// and digests, members, modes and timestamps, LICENSE, README.md and
// deploy/systemd/ equal to the repository's, NOTICE starting with the
// repository NOTICE, THIRD_PARTY_LICENSES and the CycloneDX 1.7 SBOM
// covering every module the binary links, each binary's release build,
// the schema assets, SHA256SUMS, the notes, the image plan with the image
// digests and, per platform, the image of the -image layout and its SBOM,
// a Sigstore bundle when present, and no stray file.
//
// latest prints whether the release V is at or above every published
// release, the only case in which the publish job marks it latest.
// tag-image tags the verified image index DIGEST in the registry X.Y.Z and,
// when V is the newest published release of its line, X.Y, copying the
// index unchanged; the immutable X.Y.Z tag must be absent or already name
// DIGEST, so it is never pushed again. -check only checks that.
//
// The GitHub API root and repository come from GITHUB_API_URL and
// GITHUB_REPOSITORY (-api and -repo override them), the token from GH_TOKEN
// or GITHUB_TOKEN; tag-image signs in to the registry with that token as
// GITHUB_ACTOR.
//
// Exit status: 0 success, 1 findings, 2 usage, API or I/O error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, environ{getenv: os.Getenv, client: &http.Client{Timeout: 2 * time.Minute}})
	stop()
	os.Exit(code)
}

// environ is what releasekit reads besides its flags and files; tests
// replace it.
type environ struct {
	getenv func(string) string
	// client reaches the GitHub API and the image registry.
	client *http.Client
}

// token is the GitHub token of the environment.
func (e environ) token() string {
	if t := e.getenv("GH_TOKEN"); t != "" {
		return t
	}
	return e.getenv("GITHUB_TOKEN")
}

// github returns the GitHub API client of the flags or the environment.
func (e environ) github(api, repo string) (*github, error) {
	if api == "" {
		api = e.getenv("GITHUB_API_URL")
	}
	if api == "" {
		api = "https://api.github.com"
	}
	if repo == "" {
		repo = e.getenv("GITHUB_REPOSITORY")
	}
	return newGitHub(api, repo, e.token(), e.client)
}

const usage = "usage: releasekit next|image|package|sbom|notes|checksums|verify|latest|tag-image [flags]"

// run dispatches a subcommand and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env environ) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("releasekit "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := "."
	var xo nextOptions
	var po packageOptions
	var no notesOptions
	var vo verifyOptions
	var imo imageOptions
	var so sbomOptions
	var to tagImageOptions
	var dir, version, api, repo string
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
	apiFlags := func() {
		fs.StringVar(&api, "api", "", "GitHub API root; default $GITHUB_API_URL or https://api.github.com")
		fs.StringVar(&repo, "repo", "", "repository owner/name; default $GITHUB_REPOSITORY")
	}
	switch cmd {
	case "next":
		fs.StringVar(&root, "C", ".", "repository root: a clone with its whole history and tags")
		fs.StringVar(&xo.to, "to", "HEAD", "the commit a push to main brings")
		fs.StringVar(&xo.outputs, "outputs", "", "append version, tag, previous and tagged to this file ($GITHUB_OUTPUT); empty prints them")
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
		fs.StringVar(&no.from, "from", "", "previous release tag, or root; empty takes the previous release of -to")
		fs.StringVar(&no.manifest, "manifest", "", "release-manifest.json, whose image digests the notes name; empty names none")
	case "verify":
		fs.StringVar(&root, "C", ".", "repository root")
		need(flagRef{"manifest", "release-manifest.json of the release directory", &vo.manifest})
		fs.StringVar(&vo.image, "image", "", "OCI image layout of the image the manifest records")
	case "latest":
		need(flagRef{"version", "the release, X.Y.Z", &version})
		apiFlags()
	case "tag-image":
		need(flagRef{"version", "the published release, X.Y.Z", &to.version},
			flagRef{"digest", "the verified image index digest, sha256:<64 hex>", &to.digest})
		fs.BoolVar(&to.check, "check", false, "only check that the immutable X.Y.Z tag is absent or names the digest")
		fs.StringVar(&to.repository, "repository", imageRegistry+"/ruralzd", "the image repository")
		fs.StringVar(&to.registry, "registry", "", "registry API root; default https:// and the repository's host")
		apiFlags()
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
	case "next":
		xo.root = root
		var r nextResult
		if r, err = runNext(ctx, xo, runGit); err == nil {
			_, _ = fmt.Fprintf(stdout, "releasekit: %s\n", r.Reason)
			if xo.outputs == "" {
				err = r.writeOutputs(stdout)
			}
		}
		asFindings()
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
			_, _ = fmt.Fprintf(stdout, "releasekit: packaged %d archives and %d schema files of %s with %s into %s\n",
				len(m.Archives), len(m.Schemas), po.version, image, po.out)
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
	case "latest":
		err = runLatest(ctx, env, api, repo, version, stdout)
	case "tag-image":
		to.user, to.token = env.getenv("GITHUB_ACTOR"), env.token()
		var published []releaseVersion
		if !to.check {
			published, err = publishedOf(ctx, env, api, repo)
		}
		if err == nil {
			findings, err = runTagImage(ctx, to, env.client, published, stdout)
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

// publishedOf lists the published releases through the GitHub API of the
// flags or the environment.
func publishedOf(ctx context.Context, env environ, api, repo string) ([]releaseVersion, error) {
	g, err := env.github(api, repo)
	if err != nil {
		return nil, err
	}
	return publishedReleases(ctx, g)
}

// runLatest prints whether the release version is at or above every
// published release.
func runLatest(ctx context.Context, env environ, api, repo, version string, stdout io.Writer) error {
	v, ok := parseReleaseTag("v" + version)
	if !ok {
		return fmt.Errorf("version %q is not a release X.Y.Z", version)
	}
	published, err := publishedOf(ctx, env, api, repo)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, isLatest(v, published))
	return err
}
