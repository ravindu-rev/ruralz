// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// imageOptions are the flags of `releasekit image`.
type imageOptions struct {
	// dir is the OCI image layout the image build wrote (dist/image/).
	dir string
	// metadata is the build's metadata file (docker buildx
	// --metadata-file) when the build also pushed the image.
	metadata string
}

// maxMetadataBytes bounds the build metadata file.
const maxMetadataBytes = 1 << 20

// buildMetadata is the part of docker buildx's metadata file the image
// step reads: the digest of the image the build exported, here the image
// index pushed untagged by digest.
type buildMetadata struct {
	Digest string `json:"containerimage.digest"`
}

// layoutImage reads an image build's OCI layout: one image index of one
// image manifest per platform of imagePlatforms() and nothing else. It
// returns the image and its platform manifests in imagePlatforms() order;
// any other layout is a finding.
func layoutImage(dir string) (*ociImage, []platformManifest, error) {
	img, err := readImage(dir)
	if err != nil {
		return nil, nil, &findingsError{[]string{fmt.Sprintf("image layout %s: %v", dir, err)}}
	}
	ordered, problems := planManifests(img, imagePlatforms())
	if len(problems) > 0 {
		return nil, nil, &findingsError{problems}
	}
	return img, ordered, nil
}

// runImage checks the image step's output (docs/engineering/04-release-versioning-and-compatibility.md,
// "Packaging and audit", step 2) and returns the image index digest: the
// layout holds one image index of one image manifest per platform, whose
// manifest, configuration and layers each match their descriptor, and,
// when the build also pushed the image, the index digest its metadata
// file records is the layout's, so the image the audit reads is the one in
// the registry.
func runImage(o imageOptions) (string, error) {
	img, manifests, err := layoutImage(o.dir)
	if err != nil {
		return "", err
	}
	for _, m := range manifests {
		d := img.descriptorOf(m.Platform)
		if _, err := readPlatform(o.dir, d); err != nil {
			return "", &findingsError{[]string{fmt.Sprintf("image %s, %s: %v", img.Digest, m.Platform, err)}}
		}
	}
	if o.metadata == "" {
		return img.Digest, nil
	}
	f, err := os.Open(o.metadata) //nolint:gosec // The metadata file given on the command line.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var md buildMetadata
	if err := json.NewDecoder(io.LimitReader(f, maxMetadataBytes)).Decode(&md); err != nil {
		return "", fmt.Errorf("%s: %w", o.metadata, err)
	}
	if md.Digest != img.Digest {
		return "", &findingsError{[]string{fmt.Sprintf("the build pushed the image index %q, but its layout %s holds %s", md.Digest, o.dir, img.Digest)}}
	}
	return img.Digest, nil
}
