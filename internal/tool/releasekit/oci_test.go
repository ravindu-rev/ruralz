// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/pem"
	"errors"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// fixtureCAVersion is the ca-certificates version of the fixture image.
const fixtureCAVersion = "20230311+deb12u1"

// fixtureCABundle is a CA bundle of two PEM certificates.
func fixtureCABundle() []byte {
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("fixture root one")}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("fixture root two")})...)
}

// caSection is the CA bundle's section the image build appends to the
// binary's THIRD_PARTY_LICENSES (deploy/container/Dockerfile).
func caSection(version string) string {
	return "\n" + sectionRule + "\n" +
		"ca-certificates " + version + " (Debian): /etc/ssl/certs/ca-certificates.crt\n" +
		"License: MPL-2.0 (certificate data)\n" +
		"Source: https://sources.debian.org/src/ca-certificates/" + version + "/\n" +
		"MPL-2.0 Source Code Form: the Source address above.\n" +
		strings.Repeat("-", 80) + "\n" +
		"copyright:\n\nFormat: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/\n"
}

// layerEntry is one entry of a fixture image layer; Name has no leading
// slash, and a directory's ends in one.
type layerEntry struct {
	Name     string
	Type     byte
	Mode     int64
	UID, GID int
	Data     []byte
}

// imageSpec is what one fixture platform image holds; a mutation changes
// it before it is written.
type imageSpec struct {
	Platform string
	Entries  []layerEntry
	Config   map[string]any
	// Layers splits Entries into one layer per slice when set.
	Layers [][]layerEntry
}

// configOf returns the config member of an image configuration.
func (s *imageSpec) configOf() map[string]any { return s.Config["config"].(map[string]any) }

// fixtureImageSpec is the image the Dockerfile builds for platform from
// the fixture's linux ruralzd binary and notices.
func (fx *fixture) fixtureImageSpec(t *testing.T, platform string) *imageSpec {
	t.Helper()
	arch := strings.TrimPrefix(platform, "linux/")
	notices := filepath.Join(fx.notices, "ruralzd_linux_"+arch)
	tpl := append(readBack(t, filepath.Join(notices, thirdPartyMember)), caSection(fixtureCAVersion)...)
	dir := func(name string) layerEntry { return layerEntry{Name: name, Type: tar.TypeDir, Mode: 0o755} }
	file := func(name string, mode int64, data []byte) layerEntry {
		return layerEntry{Name: name, Type: tar.TypeReg, Mode: mode, Data: data}
	}
	entries := []layerEntry{
		dir("etc/"), dir("etc/ssl/"), dir("etc/ssl/certs/"),
		file("etc/ssl/certs/ca-certificates.crt", 0o644, fixtureCABundle()),
		dir("usr/"), dir("usr/local/"), dir("usr/local/bin/"),
		file("usr/local/bin/ruralzd", 0o755, readBack(t, filepath.Join(fx.dist, "linux_"+arch, "ruralzd"))),
		dir("usr/share/"), dir("usr/share/doc/"), dir("usr/share/doc/ruralzd/"),
		file("usr/share/doc/ruralzd/LICENSE", 0o644, readBack(t, filepath.Join(fx.root, licenseMember))),
		file("usr/share/doc/ruralzd/NOTICE", 0o644, readBack(t, filepath.Join(notices, noticeMember))),
		file("usr/share/doc/ruralzd/THIRD_PARTY_LICENSES", 0o644, tpl),
		dir("var/"), dir("var/lib/"),
		{Name: "var/lib/ruralz/", Type: tar.TypeDir, Mode: 0o700, UID: imageUID, GID: imageUID},
	}
	labels := map[string]any{}
	for k, v := range imageLabels(fx.version, fixtureCommit) {
		labels[k] = v
	}
	return &imageSpec{
		Platform: platform,
		Entries:  entries,
		Config: map[string]any{
			"architecture": arch,
			"os":           "linux",
			"created":      "2026-10-01T12:30:45Z",
			"config": map[string]any{
				"User":         imageUser,
				"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}, "8443/tcp": map[string]any{}, "9901/tcp": map[string]any{}},
				"Entrypoint":   []any{imageBinary},
				"Labels":       labels,
			},
		},
	}
}

// writeBlob stores data in the layout and returns its descriptor.
func writeBlob(t *testing.T, layout, mediaType string, data []byte) ociDescriptor {
	t.Helper()
	sum := sha256Hex(data)
	writeFile(t, filepath.Join(layout, "blobs", "sha256", sum), string(data))
	return ociDescriptor{MediaType: mediaType, Digest: "sha256:" + sum, Size: int64(len(data))}
}

// layerTar renders entries as a gzip-compressed layer.
func layerTar(t *testing.T, entries []layerEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Typeflag: e.Type, Mode: e.Mode, Uid: e.UID, Gid: e.GID, ModTime: fixtureTime(), Size: int64(len(e.Data))}
		if e.Type != tar.TypeReg {
			h.Size = 0
		}
		if e.Type == tar.TypeSymlink {
			h.Linkname = "target"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.Type == tar.TypeReg {
			if _, err := tw.Write(e.Data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// mustJSON marshals v.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeImageLayout writes an OCI layout holding one image index of the
// specs, as docker buildx --output type=oci,tar=false does, and returns
// the index digest. extra adds descriptors to the index, such as an
// attestation manifest.
func writeImageLayout(t *testing.T, layout string, specs []*imageSpec, extra ...ociDescriptor) string {
	t.Helper()
	var manifests []map[string]any
	for _, s := range specs {
		layers := s.Layers
		if layers == nil {
			layers = [][]layerEntry{s.Entries}
		}
		var descs []ociDescriptor
		for _, l := range layers {
			descs = append(descs, writeBlob(t, layout, ociLayerGzip, layerTar(t, l)))
		}
		cfg := writeBlob(t, layout, ociConfigType, mustJSON(t, s.Config))
		man := writeBlob(t, layout, ociManifestType, mustJSON(t, map[string]any{
			"schemaVersion": 2, "mediaType": ociManifestType, "config": cfg, "layers": descs,
		}))
		osName, arch, _ := strings.Cut(s.Platform, "/")
		manifests = append(manifests, map[string]any{
			"mediaType": man.MediaType, "digest": man.Digest, "size": man.Size,
			"platform": map[string]any{"architecture": arch, "os": osName},
		})
	}
	for _, d := range extra {
		manifests = append(manifests, map[string]any{
			"mediaType": d.MediaType, "digest": d.Digest, "size": d.Size,
			"platform": map[string]any{"architecture": "unknown", "os": "unknown"},
		})
	}
	idx := writeBlob(t, layout, ociIndexType, mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": ociIndexType, "manifests": manifests}))
	writeFile(t, filepath.Join(layout, "index.json"), string(mustJSON(t, map[string]any{
		"schemaVersion": 2, "mediaType": ociIndexType,
		"manifests": []any{map[string]any{
			"mediaType": idx.MediaType, "digest": idx.Digest, "size": idx.Size,
			"annotations": map[string]any{"org.opencontainers.image.created": "2026-10-01T12:30:45Z"},
		}},
	})))
	writeFile(t, filepath.Join(layout, "oci-layout"), `{"imageLayoutVersion":"1.0.0"}`)
	return idx.Digest
}

// writeImage writes the fixture's image layout into a fresh directory,
// after mutate (when set) changed each platform's spec, and records it as
// the fixture's image.
func (fx *fixture) writeImage(t *testing.T, mutate func(*imageSpec)) string {
	t.Helper()
	var specs []*imageSpec
	for _, p := range imagePlatforms() {
		s := fx.fixtureImageSpec(t, p)
		if mutate != nil {
			mutate(s)
		}
		specs = append(specs, s)
	}
	fx.image = filepath.Join(t.TempDir(), "image")
	return writeImageLayout(t, fx.image, specs)
}

// fakeScan is a CycloneDX 1.7 scan of one platform image as Syft writes
// it: the platform manifest digest as its subject's version, a package
// per module.
func fakeScan(t *testing.T, digest string, purls []string) []byte {
	t.Helper()
	var comps, refs []any
	for i, p := range purls {
		ref := p + "?package-id=" + strings.Repeat("a", i+1)
		comps = append(comps, map[string]any{"bom-ref": ref, "type": "library", "purl": p})
		refs = append(refs, ref)
	}
	if comps == nil {
		comps = []any{}
	}
	return mustJSON(t, map[string]any{
		"bomFormat": "CycloneDX", "specVersion": "1.7", "serialNumber": "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79", "version": 1,
		"metadata": map[string]any{
			"timestamp": "2026-10-30T17:11:52Z",
			"tools":     map[string]any{"components": []any{map[string]any{"type": "application", "author": "anchore", "name": "syft", "version": "1.52.0"}}},
			"component": map[string]any{"bom-ref": "b24f926a3b3b952f", "type": "container", "name": "/work/dist/image", "version": digest},
		},
		"components":   comps,
		"dependencies": []any{map[string]any{"ref": "b24f926a3b3b952f", "dependsOn": refs}},
	})
}

// modulePurls are the pkg:golang package URLs of the fixture modules.
func (fx *fixture) modulePurls() []string {
	var out []string
	for _, d := range fx.deps {
		out = append(out, "pkg:golang/"+strings.ToLower(d.Path)+"@"+strings.ReplaceAll(d.Version, "+", "%2B")+"?type=module")
	}
	return out
}

// writeScans writes a fake scan of each platform image the manifest
// records into a fresh directory and returns it.
func (fx *fixture) writeScans(t *testing.T, m *manifest) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scans")
	for _, pm := range m.Images[0].Manifests {
		writeFile(t, filepath.Join(dir, scanName(pm.Platform)), string(fakeScan(t, pm.Digest, fx.modulePurls())))
	}
	return dir
}

// TestReadImage: the layout's one image index, its platform manifests in
// index order, and every blob checked against its descriptor.
func TestReadImage(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	digest := fx.writeImage(t, nil)
	img, err := readImage(fx.image)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != digest || len(img.Manifests) != 2 || img.Manifests[0].Platform != "linux/amd64" || img.Manifests[1].Platform != "linux/arm64" {
		t.Fatalf("readImage = %+v, want %s with linux/amd64 and linux/arm64", img, digest)
	}
	d := img.descriptorOf("linux/arm64")
	if d.Digest != img.Manifests[1].Digest {
		t.Fatalf("descriptorOf(linux/arm64) = %+v, want the manifest %s", d, img.Manifests[1].Digest)
	}
	if other := img.descriptorOf("linux/s390x"); other.Digest != "" {
		t.Errorf("descriptorOf(linux/s390x) = %+v, want none", other)
	}
	p, err := readPlatform(fx.image, d)
	if err != nil {
		t.Fatal(err)
	}
	if e := p.Files[imageDataDir]; !e.Dir || e.Mode != 0o700 || e.UID != imageUID {
		t.Errorf("%s = %+v", imageDataDir, e)
	}
	if !bytes.Equal(p.Files[imageBinary].Data, []byte("fake:ruralzd:linux:arm64")) || p.Config.Config.User != imageUser {
		t.Errorf("platform image = %+v", p.Config)
	}

	// A tampered blob, a second index entry and a non-index top level are
	// errors.
	blob := filepath.Join(fx.image, "blobs", "sha256", strings.TrimPrefix(img.Manifests[0].Digest, "sha256:"))
	writeFile(t, blob, "tampered")
	if d := img.descriptorOf("linux/amd64"); d.Digest != "" {
		if _, err := readPlatform(fx.image, d); err == nil || !strings.Contains(err.Error(), "does not match its descriptor") {
			t.Errorf("tampered manifest: %v", err)
		}
	}
	writeFile(t, filepath.Join(fx.image, "index.json"), `{"schemaVersion":2,"manifests":[]}`)
	if _, err := readImage(fx.image); err == nil || !strings.Contains(err.Error(), "names 0 manifests") {
		t.Errorf("empty index.json: %v", err)
	}
}

// TestApplyLayer: later layers replace files, whiteouts delete a path and
// its children, and an opaque whiteout empties a directory, as a
// container sees the layers.
func TestApplyLayer(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	fx.writeImage(t, func(s *imageSpec) {
		upper := []layerEntry{
			{Name: "etc/ssl/certs/.wh.ca-certificates.crt", Type: tar.TypeReg, Mode: 0o644},
			{Name: "usr/share/doc/ruralzd/.wh..wh..opq", Type: tar.TypeReg, Mode: 0o644},
			{Name: "usr/share/doc/ruralzd/LICENSE", Type: tar.TypeReg, Mode: 0o600, Data: []byte("replaced")},
		}
		s.Layers = [][]layerEntry{s.Entries, upper}
	})
	img, err := readImage(fx.image)
	if err != nil {
		t.Fatal(err)
	}
	d := img.descriptorOf("linux/amd64")
	p, err := readPlatform(fx.image, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Files[imageCABundle]; ok {
		t.Error("the whiteout left the CA bundle")
	}
	if _, ok := p.Files[imageDocDir+"/"+noticeMember]; ok {
		t.Error("the opaque whiteout left NOTICE")
	}
	if e := p.Files[imageDocDir+"/"+licenseMember]; string(e.Data) != "replaced" || e.Mode != 0o600 {
		t.Errorf("LICENSE = %+v, want the upper layer's", e)
	}
}

// TestRunImage: the image step's layout passes and prints its index
// digest; a pushed digest other than the layout's, a missing platform or
// an extra attestation manifest is a finding.
func TestRunImage(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	digest := fx.writeImage(t, nil)
	got, err := runImage(imageOptions{dir: fx.image})
	if err != nil || got != digest {
		t.Fatalf("runImage = %q, %v; want %s", got, err, digest)
	}
	meta := filepath.Join(t.TempDir(), "metadata.json")
	writeFile(t, meta, `{"buildx.build.ref":"builder/builder0/x","containerimage.digest":"`+digest+`"}`)
	if got, err := runImage(imageOptions{dir: fx.image, metadata: meta}); err != nil || got != digest {
		t.Errorf("runImage with the pushed digest = %q, %v", got, err)
	}
	other := "sha256:" + strings.Repeat("0", 64)
	writeFile(t, meta, `{"containerimage.digest":"`+other+`"}`)
	var fe *findingsError
	if _, err := runImage(imageOptions{dir: fx.image, metadata: meta}); !errors.As(err, &fe) || !hasFinding(fe.findings, "the build pushed the image index \""+other+"\"") {
		t.Errorf("another pushed digest: %v", err)
	}

	one := filepath.Join(t.TempDir(), "one")
	writeImageLayout(t, one, []*imageSpec{fx.fixtureImageSpec(t, "linux/amd64")})
	if _, err := runImage(imageOptions{dir: one}); !errors.As(err, &fe) || !hasFinding(fe.findings, "holds the platforms [linux/amd64], want [linux/amd64 linux/arm64]") {
		t.Errorf("one platform: %v", err)
	}
	attested := filepath.Join(t.TempDir(), "attested")
	att := writeBlob(t, attested, ociManifestType, []byte(`{"schemaVersion":2}`))
	writeImageLayout(t, attested, []*imageSpec{fx.fixtureImageSpec(t, "linux/amd64"), fx.fixtureImageSpec(t, "linux/arm64")}, att)
	if _, err := runImage(imageOptions{dir: attested}); !errors.As(err, &fe) || !hasFinding(fe.findings, "unknown/unknown") {
		t.Errorf("an attestation manifest: %v", err)
	}
	if _, err := runImage(imageOptions{dir: filepath.Join(t.TempDir(), "absent")}); !errors.As(err, &fe) {
		t.Errorf("no layout: %v, want a finding", err)
	}
}

// TestImageLabelsMatchDockerfile: the labels the audit wants are the
// Dockerfile's LABEL instruction, with the VERSION and COMMIT build
// arguments.
func TestImageLabelsMatchDockerfile(t *testing.T) {
	var label string
	for _, in := range dockerfile(t, readRepo(t, "deploy/container/Dockerfile")) {
		if in.Cmd == "LABEL" {
			label = in.Args
		}
	}
	want := imageLabels("${VERSION}", "${COMMIT}")
	var keys []string
	for _, kv := range regexp.MustCompile(`(\S+)="([^"]*)"`).FindAllStringSubmatch(label, -1) {
		keys = append(keys, kv[1])
		if w, found := want[kv[1]]; !found || kv[2] != w {
			t.Errorf("Dockerfile LABEL %s=%q, want %q", kv[1], kv[2], w)
		}
	}
	slices.Sort(keys)
	var wantKeys []string
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("Dockerfile labels %v, want %v", keys, wantKeys)
	}
	if path.Base(imageBinary) != "ruralzd" {
		t.Errorf("image binary %s", imageBinary)
	}
}
