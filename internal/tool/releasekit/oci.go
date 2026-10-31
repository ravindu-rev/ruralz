// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The OCI image layout of the image build: `docker buildx build --output
// type=oci,tar=false` writes it to dist/image/ (step 2 of "Packaging and
// audit"), with index.json naming one multi-platform image index.
const (
	ociIndexType    = "application/vnd.oci.image.index.v1+json"
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociConfigType   = "application/vnd.oci.image.config.v1+json"
	ociLayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	ociLayerTar     = "application/vnd.oci.image.layer.v1.tar"
)

// maxBlobBytes bounds an index, manifest or config blob.
const maxBlobBytes = 4 << 20

// maxImageBytes bounds the uncompressed layer content the audit reads of
// one platform image, as maxArchiveBytes bounds an archive.
const maxImageBytes = 1 << 30

// ociDescriptor is an OCI content descriptor.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
}

// ociIndexDoc is an image index (or index.json).
type ociIndexDoc struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// ociManifestDoc is an image manifest.
type ociManifestDoc struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// imageConfig is the part of an image configuration the audit reads.
type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		User         string              `json:"User"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		Labels       map[string]string   `json:"Labels"`
		Volumes      map[string]struct{} `json:"Volumes"`
	} `json:"config"`
}

// ociImage is the image of a layout: the index digest and each platform's
// manifest digest, in index order.
type ociImage struct {
	Digest    string
	Manifests []platformManifest
	// descs are the platform manifests' descriptors, in the same order.
	descs []ociDescriptor
}

// digestPattern matches a sha256 digest.
func digestPattern() *regexp.Regexp { return regexp.MustCompile(`^sha256:[0-9a-f]{64}$`) }

// readBlob reads a blob of the layout by descriptor, checking its digest
// and size, at most limit bytes.
func readBlob(dir string, d ociDescriptor, limit int64) ([]byte, error) {
	if !digestPattern().MatchString(d.Digest) {
		return nil, fmt.Errorf("descriptor digest %q is not sha256:<64 hex>", d.Digest)
	}
	if d.Size < 0 || d.Size > limit {
		return nil, fmt.Errorf("blob %s: size %d outside 0 to %d bytes", d.Digest, d.Size, limit)
	}
	p := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
	f, err := os.Open(p) //nolint:gosec // A blob of the image layout given on the command line, named by a checked digest.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, d.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != d.Size || "sha256:"+sha256Hex(data) != d.Digest {
		return nil, fmt.Errorf("blob %s does not match its descriptor's digest and size", d.Digest)
	}
	return data, nil
}

// readImage reads the layout's image index: index.json names exactly one
// image index, whose manifests are one image manifest per platform.
func readImage(dir string) (*ociImage, error) {
	data, err := os.ReadFile(filepath.Join(dir, "index.json")) //nolint:gosec // The index of the image layout given on the command line.
	if err != nil {
		return nil, err
	}
	var top ociIndexDoc
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	if len(top.Manifests) != 1 || top.Manifests[0].MediaType != ociIndexType {
		return nil, fmt.Errorf("index.json names %d manifests, want one %s", len(top.Manifests), ociIndexType)
	}
	data, err = readBlob(dir, top.Manifests[0], maxBlobBytes)
	if err != nil {
		return nil, err
	}
	var idx ociIndexDoc
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("image index: %w", err)
	}
	if idx.SchemaVersion != 2 || idx.MediaType != ociIndexType {
		return nil, fmt.Errorf("image index %s is schema %d %q, want 2 %s", top.Manifests[0].Digest, idx.SchemaVersion, idx.MediaType, ociIndexType)
	}
	img := &ociImage{Digest: top.Manifests[0].Digest}
	for _, m := range idx.Manifests {
		if m.MediaType != ociManifestType || m.Platform == nil || !digestPattern().MatchString(m.Digest) {
			return nil, fmt.Errorf("image index %s lists %s %s, not a platform's %s", img.Digest, m.MediaType, m.Digest, ociManifestType)
		}
		p := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			p += "/" + m.Platform.Variant
		}
		img.Manifests = append(img.Manifests, platformManifest{Platform: p, Digest: m.Digest})
		img.descs = append(img.descs, m)
	}
	return img, nil
}

// planManifests returns the image's platform manifests in the order of
// platforms, or the problems: the index must hold exactly one manifest per
// platform, and nothing else, such as BuildKit's provenance or SBOM
// attestation manifests, which the image build turns off.
func planManifests(img *ociImage, platforms []string) ([]platformManifest, []string) {
	var got []string
	byPlatform := map[string]platformManifest{}
	for _, m := range img.Manifests {
		got = append(got, m.Platform)
		byPlatform[m.Platform] = m
	}
	sorted, want := slices.Clone(got), slices.Clone(platforms)
	slices.Sort(sorted)
	slices.Sort(want)
	if !slices.Equal(sorted, want) {
		return nil, []string{fmt.Sprintf("image %s holds the platforms %v, want %v", img.Digest, got, platforms)}
	}
	out := make([]platformManifest, 0, len(platforms))
	for _, pl := range platforms {
		out = append(out, byPlatform[pl])
	}
	return out, nil
}

// descriptorOf returns the image's descriptor of a platform manifest, or
// the zero descriptor, which no blob matches, for another platform.
func (img *ociImage) descriptorOf(platform string) ociDescriptor {
	for i, m := range img.Manifests {
		if m.Platform == platform {
			return img.descs[i]
		}
	}
	return ociDescriptor{}
}

// imageEntry is one path of an image's exported file system.
type imageEntry struct {
	Dir      bool
	Regular  bool
	Mode     fs.FileMode
	UID, GID int
	Data     []byte
}

// imagePlatform is one platform image: its configuration and exported file
// system, by absolute path.
type imagePlatform struct {
	Config imageConfig
	Files  map[string]imageEntry
}

// readPlatform reads one platform image of the layout: its manifest, its
// configuration, and its layers applied in order, whiteouts included, as
// the file system a container of the image sees.
func readPlatform(dir string, d ociDescriptor) (*imagePlatform, error) {
	data, err := readBlob(dir, d, maxBlobBytes)
	if err != nil {
		return nil, err
	}
	var m ociManifestDoc
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	if m.SchemaVersion != 2 || m.MediaType != ociManifestType || m.Config.MediaType != ociConfigType {
		return nil, fmt.Errorf("manifest %s is schema %d %q with a %q config, want 2 %s with %s", d.Digest, m.SchemaVersion, m.MediaType, m.Config.MediaType, ociManifestType, ociConfigType)
	}
	data, err = readBlob(dir, m.Config, maxBlobBytes)
	if err != nil {
		return nil, err
	}
	p := &imagePlatform{Files: map[string]imageEntry{}}
	if err := json.Unmarshal(data, &p.Config); err != nil {
		return nil, fmt.Errorf("config %s: %w", m.Config.Digest, err)
	}
	var total int64
	for _, l := range m.Layers {
		if err := applyLayer(dir, l, p.Files, &total); err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.Digest, err)
		}
	}
	return p, nil
}

// applyLayer applies one layer to files.
func applyLayer(dir string, l ociDescriptor, files map[string]imageEntry, total *int64) error {
	if l.MediaType != ociLayerGzip && l.MediaType != ociLayerTar {
		return fmt.Errorf("media type %q, want %s or %s", l.MediaType, ociLayerGzip, ociLayerTar)
	}
	data, err := readBlob(dir, l, maxImageBytes)
	if err != nil {
		return err
	}
	var rd io.Reader = bytes.NewReader(data)
	if l.MediaType == ociLayerGzip {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		rd = gz
	}
	tr := tar.NewReader(rd)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := "/" + strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		base := path.Base(name)
		if wh, ok := strings.CutPrefix(base, ".wh."); ok {
			parent := path.Dir(name)
			if wh == ".wh..opq" {
				for p := range files {
					if strings.HasPrefix(p, parent+"/") {
						delete(files, p)
					}
				}
				continue
			}
			gone := path.Join(parent, wh)
			for p := range files {
				if p == gone || strings.HasPrefix(p, gone+"/") {
					delete(files, p)
				}
			}
			continue
		}
		e := imageEntry{Mode: fs.FileMode(h.Mode & 0o7777), UID: h.Uid, GID: h.Gid, Dir: h.Typeflag == tar.TypeDir, Regular: h.Typeflag == tar.TypeReg}
		if e.Regular {
			if *total += h.Size; *total > maxImageBytes {
				return fmt.Errorf("image content exceeds %d bytes", maxImageBytes)
			}
			if e.Data, err = io.ReadAll(io.LimitReader(tr, h.Size)); err != nil {
				return err
			}
		}
		files[name] = e
	}
}
