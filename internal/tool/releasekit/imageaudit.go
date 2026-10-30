// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"debug/buildinfo"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The ruralzd image's file system and configuration
// (docs/engineering/04-release-versioning-and-compatibility.md, "Packaging
// and audit"): deploy/container/Dockerfile builds them, and the audit
// checks every platform image against them.
const (
	imageBinary   = "/usr/local/bin/ruralzd"
	imageCABundle = "/etc/ssl/certs/ca-certificates.crt"
	imageDocDir   = "/usr/share/doc/ruralzd"
	imageDataDir  = "/var/lib/ruralz"
	// imageUID is the user and group the image runs as and that owns the
	// data directory.
	imageUID  = 65532
	imageUser = "65532:65532"
)

// imagePorts are the ports the image exposes: 8080 and 8443 for traffic,
// 9901 for the admin API.
func imagePorts() []string { return []string{"8080/tcp", "8443/tcp", "9901/tcp"} }

// imageLabels are the OCI labels of the image of version at commit.
func imageLabels(version, commit string) map[string]string {
	return map[string]string{
		"org.opencontainers.image.title":       "ruralzd",
		"org.opencontainers.image.description": "Ruralz Gateway",
		"org.opencontainers.image.source":      repoURL,
		"org.opencontainers.image.licenses":    "Apache-2.0",
		"org.opencontainers.image.vendor":      "Revington",
		"org.opencontainers.image.version":     version,
		"org.opencontainers.image.revision":    commit,
	}
}

// caNotice matches the CA bundle's section the image build appends to the
// binary's THIRD_PARTY_LICENSES: the Debian package and version, its
// MPL-2.0 license, its source address, then its copyright file.
func caNotice() *regexp.Regexp {
	return regexp.MustCompile(`^\n` + regexp.QuoteMeta(sectionRule) + `\n` +
		`ca-certificates (\S+) \(Debian\): ` + regexp.QuoteMeta(imageCABundle) + `\n` +
		`License: MPL-2\.0 \(certificate data\)\n` +
		`Source: https://sources\.debian\.org/src/ca-certificates/(\S+)/\n` +
		`MPL-2\.0 Source Code Form: the Source address above\.\n` +
		`-+\ncopyright:\n\n\S`)
}

// imageFile is a regular file the image holds, owned by 0:0: its mode
// and, when known, content.
type imageFile struct {
	mode fs.FileMode
	data []byte
}

// wantedImageFiles are the regular files of the image for one platform.
// The CA bundle's and THIRD_PARTY_LICENSES's content are checked apart:
// the build takes them from the pinned base image.
func wantedImageFiles(license []byte, arch archived) map[string]imageFile {
	return map[string]imageFile{
		imageBinary:                          {mode: modeExec, data: arch.binary},
		imageCABundle:                        {mode: modeFile},
		imageDocDir + "/" + licenseMember:    {mode: modeFile, data: license},
		imageDocDir + "/" + noticeMember:     {mode: modeFile, data: arch.notice},
		imageDocDir + "/" + thirdPartyMember: {mode: modeFile},
	}
}

// ancestors returns every directory above the paths, without "/".
func ancestors(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range paths {
		for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
			out[d] = true
		}
	}
	return out
}

// verifyImage audits the image the manifest records: the layout's image
// index and platform manifests are the manifest's digests, every platform
// image is the one the Dockerfile builds from that platform's linux
// ruralzd archive, and the image SBOM covers every platform image.
func verifyImage(a *audit, dir, layout string, m *manifest, repo repoFiles, linux map[string]archived) error {
	plan := m.Images[0]
	if !plan.recorded() {
		return nil
	}
	if layout == "" {
		a.add("the manifest records the image %s@%s: give its OCI layout with -image to audit it", plan.Repository, plan.Digest)
	} else {
		img, manifests, err := layoutImage(layout)
		var fe *findingsError
		switch {
		case errors.As(err, &fe):
			a.findings = append(a.findings, fe.findings...)
		case err != nil:
			return err
		case img.Digest != plan.Digest:
			a.add("image layout %s holds the index %s, but the manifest records %s", layout, img.Digest, plan.Digest)
		case !slices.Equal(manifests, plan.Manifests):
			a.add("image layout %s holds the platform manifests %s, but the manifest records %s", layout, jsonText(manifests), jsonText(plan.Manifests))
		default:
			for _, pm := range manifests {
				d := img.descriptorOf(pm.Platform)
				p, err := readPlatform(layout, d)
				if err != nil {
					a.add("image %s, %s: %v", plan.Digest, pm.Platform, err)
					continue
				}
				arch, ok := linux[pm.Platform]
				if !ok {
					a.add("image %s, %s: no linux ruralzd archive of the release to compare it with", plan.Digest, pm.Platform)
					continue
				}
				for _, f := range platformProblems(p, pm.Platform, m, repo, arch) {
					a.add("image %s, %s: %s", plan.Digest, pm.Platform, f)
				}
			}
		}
	}
	infos := map[string]*buildinfo.BuildInfo{}
	for platform, arch := range linux {
		infos[platform] = arch.info
	}
	problems, err := imageSBOMProblems(filepath.Join(dir, plan.SBOM), plan, infos)
	if err != nil {
		return err
	}
	a.findings = append(a.findings, problems...)
	return nil
}

// platformProblems audits one platform image: its configuration (the
// platform, USER 65532:65532, the exposed ports, the entrypoint, the OCI
// labels) and its exported file system (ruralzd byte-equal to the linux
// archive's binary, the CA bundle, LICENSE, NOTICE and THIRD_PARTY_LICENSES
// as built, the data directory owner-only for 65532, and no other file).
func platformProblems(p *imagePlatform, platform string, m *manifest, repo repoFiles, arch archived) []string {
	var out []string
	c := p.Config
	if got := c.OS + "/" + c.Architecture; got != platform {
		out = append(out, fmt.Sprintf("configuration is for %s", got))
	}
	if c.Config.User != imageUser {
		out = append(out, fmt.Sprintf("USER %q, want %s", c.Config.User, imageUser))
	}
	if ports := slices.Sorted(maps.Keys(c.Config.ExposedPorts)); !slices.Equal(ports, imagePorts()) {
		out = append(out, fmt.Sprintf("exposes %v, want %v", ports, imagePorts()))
	}
	if !slices.Equal(c.Config.Entrypoint, []string{imageBinary}) || len(c.Config.Cmd) > 0 {
		out = append(out, fmt.Sprintf("entrypoint %q and command %q, want [%s] alone", c.Config.Entrypoint, c.Config.Cmd, imageBinary))
	}
	if want := imageLabels(m.Version, m.Commit); !maps.Equal(c.Config.Labels, want) {
		out = append(out, fmt.Sprintf("labels %s, want %s", jsonText(c.Config.Labels), jsonText(want)))
	}

	files := wantedImageFiles(repo.license, arch)
	dirs := ancestors(append(slices.Collect(maps.Keys(files)), imageDataDir))
	for _, name := range slices.Sorted(maps.Keys(p.Files)) {
		e := p.Files[name]
		w, isFile := files[name]
		switch {
		case name == imageDataDir:
			if !e.Dir || e.Mode != 0o700 || e.UID != imageUID || e.GID != imageUID {
				out = append(out, fmt.Sprintf("%s is %s, want a directory of mode 0700 owned by %s", name, describe(e), imageUser))
			}
		case isFile:
			if !e.Regular || e.Mode != w.mode || e.UID != 0 || e.GID != 0 {
				out = append(out, fmt.Sprintf("%s is %s, want a file of mode %04o owned by 0:0", name, describe(e), w.mode))
			}
			if w.data != nil && !bytes.Equal(e.Data, w.data) {
				out = append(out, fmt.Sprintf("%s differs from %s", name, imageSource(name)))
			}
		case dirs[name]:
			if !e.Dir {
				out = append(out, fmt.Sprintf("%s is %s, want a directory", name, describe(e)))
			}
		default:
			out = append(out, fmt.Sprintf("unexpected %s", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if _, ok := p.Files[name]; !ok {
			out = append(out, fmt.Sprintf("missing %s", name))
		}
	}
	if _, ok := p.Files[imageDataDir]; !ok {
		out = append(out, fmt.Sprintf("missing %s", imageDataDir))
	}
	if e, ok := p.Files[imageCABundle]; ok && e.Regular {
		if n, bad := pemCertificates(e.Data); n == 0 || bad {
			out = append(out, fmt.Sprintf("%s is not a PEM bundle of certificates", imageCABundle))
		}
	}
	if e, ok := p.Files[imageDocDir+"/"+thirdPartyMember]; ok && e.Regular {
		rest, found := bytes.CutPrefix(e.Data, arch.thirdParty)
		if !found || len(arch.thirdParty) == 0 {
			out = append(out, fmt.Sprintf("%s/%s does not start with the archive's %s", imageDocDir, thirdPartyMember, thirdPartyMember))
		} else if mm := caNotice().FindSubmatch(rest); mm == nil || !bytes.Equal(mm[1], mm[2]) {
			out = append(out, fmt.Sprintf("%s/%s does not end with the CA bundle's MPL-2.0 notice and source address", imageDocDir, thirdPartyMember))
		}
	}
	return out
}

// imageSource names what an image file must equal, for a finding.
func imageSource(name string) string {
	switch name {
	case imageBinary:
		return "the linux ruralzd archive's binary"
	case imageDocDir + "/" + licenseMember:
		return "the repository's " + licenseMember
	default:
		return "the archive's " + path.Base(name)
	}
}

// describe renders an image entry's type, mode and owner for a finding.
func describe(e imageEntry) string {
	kind := "a special file"
	switch {
	case e.Dir:
		kind = "a directory"
	case e.Regular:
		kind = "a file"
	}
	return fmt.Sprintf("%s of mode %04o owned by %d:%d", kind, e.Mode, e.UID, e.GID)
}

// pemCertificates counts the PEM blocks of data and reports whether any is
// not a certificate or anything but white space surrounds them.
func pemCertificates(data []byte) (n int, bad bool) {
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" || len(b.Bytes) == 0 {
			bad = true
		}
		n++
	}
	return n, strings.TrimSpace(string(rest)) != "" || bad
}
