// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// modulePath is the Ruralz module.
const modulePath = "github.com/ravindu-rev/ruralz"

// imageRegistry is where the container images are published.
const imageRegistry = "ghcr.io/ravindu-rev"

// Support levels of an archive's platform
// (docs/engineering/01-tech-stack-and-libraries.md, "Static builds").
const (
	supportProduction  = "production"
	supportDevelopment = "development"
)

// File names of the release directory besides the archives, their SBOMs
// and the schema assets.
const (
	manifestName  = "release-manifest.json"
	checksumsName = "SHA256SUMS"
	signatureName = "SHA256SUMS.sigstore.json"
	notesName     = "NOTES.md"
)

// sbomSuffix names the CycloneDX SBOM of an archive: <archive>.cdx.json.
const sbomSuffix = ".cdx.json"

// cycloneDXVersion is the SBOM specification version.
const cycloneDXVersion = "1.7"

// Legal and documentation members of every archive (ADR-0002 release
// audit), taken from the repository root (LICENSE, README.md) or from the
// artifact's notices directory written by depgate (NOTICE,
// THIRD_PARTY_LICENSES).
const (
	licenseMember    = "LICENSE"
	noticeMember     = "NOTICE"
	readmeMember     = "README.md"
	thirdPartyMember = "THIRD_PARTY_LICENSES"
)

// systemdDir is the repository directory that linux ruralzd archives
// carry at the same path.
const systemdDir = "deploy/systemd"

// systemdFiles are the files of systemdDir, exactly the T5 table of
// docs/operations/01-deployment-topologies.md: the unit, its ExecReload
// helper, the example EnvironmentFile, the sysctl settings and the
// sysusers.d entry that creates the ruralz user.
func systemdFiles() []string {
	return []string{"99-ruralzd.conf", "ruralzd-handover", "ruralzd.env", "ruralzd.service", "ruralzd.sysusers"}
}

// artifact is one release archive: a binary built for one platform.
type artifact struct {
	Binary, OS, Arch, Support string
}

// artifacts lists the archives of a release
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// artifacts"): ruralzd for linux/amd64 and linux/arm64 and, for
// development only, darwin/arm64 and darwin/amd64; ruralz for those and
// windows/amd64. ruralz-control archives are Planned (M2).
func artifacts() []artifact {
	return []artifact{
		{"ruralzd", "linux", "amd64", supportProduction},
		{"ruralzd", "linux", "arm64", supportProduction},
		{"ruralzd", "darwin", "arm64", supportDevelopment},
		{"ruralzd", "darwin", "amd64", supportDevelopment},
		{"ruralz", "linux", "amd64", supportProduction},
		{"ruralz", "linux", "arm64", supportProduction},
		{"ruralz", "darwin", "arm64", supportProduction},
		{"ruralz", "darwin", "amd64", supportProduction},
		{"ruralz", "windows", "amd64", supportProduction},
	}
}

// executable is the binary's file name in the archive and in dist/.
func (a artifact) executable() string {
	if a.OS == "windows" {
		return a.Binary + ".exe"
	}
	return a.Binary
}

// archiveName is "<binary>_<version>_<os>_<arch>.tar.gz", or ".zip" on
// Windows.
func (a artifact) archiveName(version string) string {
	ext := ".tar.gz"
	if a.OS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", a.Binary, version, a.OS, a.Arch, ext)
}

// target is "<binary>_<os>_<arch>", the artifact's notices directory.
func (a artifact) target() string { return a.Binary + "_" + a.OS + "_" + a.Arch }

// distDir is the artifact's build output directory below dist/.
func (a artifact) distDir() string { return a.OS + "_" + a.Arch }

// withSystemd reports whether the archive carries deploy/systemd/: linux
// ruralzd archives do (docs/operations/01-deployment-topologies.md, "T5
// and T6").
func (a artifact) withSystemd() bool { return a.Binary == "ruralzd" && a.OS == "linux" }

// imagePlan is one container image of a release.
type imagePlan struct {
	// Repository is the image name, such as ghcr.io/ravindu-rev/ruralzd.
	Repository string `json:"repository"`
	// Binary is the binary the image runs.
	Binary string `json:"binary"`
	// Dockerfile and Target select the build.
	Dockerfile string `json:"dockerfile"`
	Target     string `json:"target"`
	// Platforms are the image's platforms.
	Platforms []string `json:"platforms"`
	// Tags are the tags to push; empty for a version that is neither a
	// release nor a release candidate.
	Tags []string `json:"tags"`
	// SBOM names the image SBOM, one CycloneDX document whose subject is
	// the image index.
	SBOM string `json:"sbom"`
	// Digest is the image index digest the build recorded and Manifests
	// each platform's manifest digest, in Platforms order; both are empty
	// when the build recorded no image (a development dry run without
	// Docker).
	Digest    string             `json:"digest"`
	Manifests []platformManifest `json:"manifests"`
}

// platformManifest is one platform's manifest digest of an image index.
type platformManifest struct {
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
}

// recorded reports whether the build recorded the image.
func (p imagePlan) recorded() bool { return p.Digest != "" }

// imagePlatforms are the platforms of the ruralzd image, in the order the
// manifest lists their manifest digests.
func imagePlatforms() []string { return []string{"linux/amd64", "linux/arm64"} }

// images lists the container images of a release, without the digests a
// build records: ruralzd for linux/amd64 and linux/arm64. The
// ruralz-control image is Planned (M2).
func images(version string) []imagePlan {
	return []imagePlan{{
		Repository: imageRegistry + "/ruralzd",
		Binary:     "ruralzd",
		Dockerfile: "deploy/container/Dockerfile",
		Target:     "ruralzd",
		Platforms:  imagePlatforms(),
		Tags:       imageTags(version),
		SBOM:       imageSBOMName(version),
		Manifests:  []platformManifest{},
	}}
}

// imageSBOMName is the image SBOM of a release:
// ruralzd_<version>_image.cdx.json.
func imageSBOMName(version string) string {
	return fmt.Sprintf("ruralzd_%s_image%s", version, sbomSuffix)
}

// withDigests returns the plan with the digests an image build recorded.
func (p imagePlan) withDigests(index string, manifests []platformManifest) imagePlan {
	p.Digest, p.Manifests = index, manifests
	return p
}

// benchArchiveName is the benchmark records archive of a release:
// ruralz_<version>_bench.tar.gz
// (docs/architecture/12-performance-budgets-and-benchmarking.md,
// "Publication").
func benchArchiveName(version string) string { return "ruralz_" + version + "_bench.tar.gz" }

// Version kinds.
const (
	kindRelease   = "release"
	kindCandidate = "candidate"
	kindDev       = "development"
)

// semver matches a Semantic Versioning 2.0.0 version without a leading "v"
// and without build metadata, which image tags cannot carry.
func semver() *regexp.Regexp {
	return regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*)?$`)
}

// checkVersion validates a product version as the Makefile derives it from
// `git describe`: X.Y.Z, X.Y.Z-rc.N, or a development version such as
// 0.0.0-dev or 0.1.0-3-gabcdef1-dirty.
func checkVersion(v string) error {
	if strings.HasPrefix(v, "v") {
		return fmt.Errorf("version %q: give the version without its leading v", v)
	}
	if !semver().MatchString(v) {
		return fmt.Errorf("version %q is not MAJOR.MINOR.PATCH[-PRERELEASE] (Semantic Versioning 2.0.0, no build metadata)", v)
	}
	return nil
}

// versionKind classifies a valid version: a release X.Y.Z, a release
// candidate X.Y.Z-rc.N (N from 1), or any other development build.
func versionKind(v string) string {
	switch {
	case regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(v):
		return kindRelease
	case regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-rc\.[1-9][0-9]*$`).MatchString(v):
		return kindCandidate
	default:
		return kindDev
	}
}

// imageTags are the tags of a version's images: X.Y.Z and X.Y for a
// release, X.Y.Z-rc.N for a candidate, none for a development build, and
// never latest. A pushed X.Y.Z or X.Y.Z-rc.N tag is never pushed again;
// X.Y moves to the newest patch of its line.
func imageTags(v string) []string {
	switch versionKind(v) {
	case kindRelease:
		parts := strings.SplitN(v, ".", 3)
		return []string{v, parts[0] + "." + parts[1]}
	case kindCandidate:
		return []string{v}
	default:
		return []string{}
	}
}

// checkCommit validates a full Git commit: SHA-1 or SHA-256 in lower-case
// hex.
func checkCommit(c string) error {
	if !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(c) {
		return fmt.Errorf("commit %q is not a full lower-case Git commit", c)
	}
	return nil
}
