// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// sbomOptions are the flags of `releasekit sbom`.
type sbomOptions struct {
	// manifest is the release directory's release-manifest.json, whose
	// image plan names the image SBOM and the digests.
	manifest string
	// scans holds Syft's CycloneDX scan of each platform image as
	// <os>_<arch>.cdx.json.
	scans string
}

// maxSBOMBytes bounds one SBOM document read.
const maxSBOMBytes = 64 << 20

// indexRef is the bom-ref of the image SBOM's subject, the image index.
const indexRef = "image-index"

// scanName is the file of a platform's scan below the scans directory:
// linux/amd64 is linux_amd64.cdx.json.
func scanName(platform string) string {
	return strings.ReplaceAll(platform, "/", "_") + sbomSuffix
}

// readJSONDoc reads the JSON document at path, at most limit bytes, into
// v, keeping numbers as json.Number so a merged document repeats them
// exactly. found is false when the file does not exist; a document too
// large or not decoding into v is a problem, other read failures errors.
func readJSONDoc(path string, limit int64, v any) (found bool, problem string, err error) {
	f, err := os.Open(path) //nolint:gosec // A file the caller names below a directory given on the command line.
	if errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return true, "", err
	}
	if int64(len(data)) > limit {
		return true, fmt.Sprintf("%s exceeds %d bytes", filepath.Base(path), limit), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return true, fmt.Sprintf("%s is not a CycloneDX JSON document: %v", filepath.Base(path), err), nil
	}
	return true, "", nil
}

// ociPurl is the package URL of an image or image index by digest
// (purl type oci): its repository and, for a platform image, its
// architecture.
func ociPurl(repository, digest, arch string) string {
	name := repository[strings.LastIndex(repository, "/")+1:]
	q := "repository_url=" + repository
	if arch != "" {
		q = "arch=" + arch + "&" + q
	}
	return "pkg:oci/" + name + "@" + strings.Replace(digest, ":", "%3A", 1) + "?" + q
}

// sbomSerial is the image SBOM's serial number: a UUID derived from the
// image index digest (version 8, RFC 9562's custom layout, over SHA-256),
// so rebuilding the image rewrites the same document.
func sbomSerial(indexDigest string) string {
	sum := sha256.Sum256([]byte("ruralz image SBOM " + indexDigest))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x80
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return "urn:uuid:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// asObject returns v as a JSON object, or nil.
func asObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// asArray returns v as a JSON array, or nil.
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

// asString returns v as a JSON string, or "".
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// prefixRefs prefixes the bom-ref of every component of the tree, so the
// same package found in two platform images keeps a unique reference.
func prefixRefs(components []any, prefix string) {
	for _, c := range components {
		obj := asObject(c)
		if obj == nil {
			continue
		}
		if ref := asString(obj["bom-ref"]); ref != "" {
			obj["bom-ref"] = prefix + ref
		}
		prefixRefs(asArray(obj["components"]), prefix)
	}
}

// platformScan is one platform image's scan as the merge reads it.
type platformScan struct {
	platform, digest string
	doc              map[string]any
}

// readScan reads a platform's scan and checks that Syft scanned that
// platform's image: CycloneDX 1.7 whose subject is the platform manifest
// digest.
func readScan(dir string, m platformManifest) (platformScan, []string, error) {
	file := filepath.Join(dir, scanName(m.Platform))
	var doc map[string]any
	found, problem, err := readJSONDoc(file, maxSBOMBytes, &doc)
	switch {
	case err != nil:
		return platformScan{}, nil, err
	case !found:
		return platformScan{}, []string{fmt.Sprintf("no scan of the %s image: Syft writes %s", m.Platform, scanName(m.Platform))}, nil
	case problem != "":
		return platformScan{}, []string{"scan " + problem}, nil
	}
	var out []string
	if f, v := asString(doc["bomFormat"]), asString(doc["specVersion"]); f != "CycloneDX" || v != cycloneDXVersion {
		out = append(out, fmt.Sprintf("the %s scan is %s %s, want CycloneDX %s", m.Platform, f, v, cycloneDXVersion))
	}
	subject := asObject(asObject(doc["metadata"])["component"])
	if got := asString(subject["version"]); got != m.Digest {
		out = append(out, fmt.Sprintf("the %s scan is of %q, not of the %s image manifest %s", m.Platform, got, m.Platform, m.Digest))
	}
	return platformScan{platform: m.Platform, digest: m.Digest, doc: doc}, out, nil
}

// mergeImageSBOM builds the image SBOM from the platform scans: one
// CycloneDX 1.7 document whose subject is the image index and whose
// components are each platform image, holding the packages its scan found
// (docs/engineering/04-release-versioning-and-compatibility.md, "Packaging
// and audit", step 4). Every bom-ref of a scan gains its platform as a
// prefix, and the scan's own subject becomes the platform component.
func mergeImageSBOM(plan imagePlan, commitTime string, scans []platformScan) map[string]any {
	var platforms, deps, tools []any
	seenTool := map[string]bool{}
	var indexDeps []any
	for _, s := range scans {
		prefix := s.platform + ":"
		subject := asObject(asObject(s.doc["metadata"])["component"])
		subjectRef := asString(subject["bom-ref"])
		nested := asArray(s.doc["components"])
		prefixRefs(nested, prefix)
		if nested == nil {
			nested = []any{}
		}
		_, arch, _ := strings.Cut(s.platform, "/")
		platforms = append(platforms, map[string]any{
			"bom-ref":    s.platform,
			"type":       "container",
			"name":       plan.Repository,
			"version":    s.digest,
			"purl":       ociPurl(plan.Repository, s.digest, arch),
			"components": nested,
		})
		indexDeps = append(indexDeps, s.platform)
		ref := func(r string) string {
			if r == subjectRef && r != "" {
				return s.platform
			}
			return prefix + r
		}
		for _, d := range asArray(s.doc["dependencies"]) {
			obj := asObject(d)
			if obj == nil {
				continue
			}
			on := []any{}
			for _, x := range asArray(obj["dependsOn"]) {
				on = append(on, ref(asString(x)))
			}
			deps = append(deps, map[string]any{"ref": ref(asString(obj["ref"])), "dependsOn": on})
		}
		for _, t := range asArray(asObject(asObject(asObject(s.doc["metadata"])["tools"]))["components"]) {
			obj := asObject(t)
			key := asString(obj["name"]) + " " + asString(obj["version"])
			if obj != nil && !seenTool[key] {
				seenTool[key] = true
				tools = append(tools, obj)
			}
		}
	}
	if tools == nil {
		tools = []any{}
	}
	deps = append([]any{map[string]any{"ref": indexRef, "dependsOn": indexDeps}}, deps...)
	return map[string]any{
		"bomFormat":    "CycloneDX",
		"specVersion":  cycloneDXVersion,
		"serialNumber": sbomSerial(plan.Digest),
		"version":      1,
		"metadata": map[string]any{
			"timestamp": commitTime,
			"tools":     map[string]any{"components": tools},
			"component": map[string]any{
				"bom-ref": indexRef,
				"type":    "container",
				"name":    plan.Repository,
				"version": plan.Digest,
				"purl":    ociPurl(plan.Repository, plan.Digest, ""),
			},
		},
		"components":   platforms,
		"dependencies": deps,
	}
}

// runSBOM merges the platform scans into the image SBOM beside the
// manifest and returns its name. A manifest without an image, a missing
// scan or a scan of another image is a finding.
func runSBOM(o sbomOptions) (string, error) {
	m, _, err := loadManifest(o.manifest)
	if err != nil {
		return "", err
	}
	if len(m.Images) != 1 || !m.Images[0].recorded() {
		return "", &findingsError{[]string{fmt.Sprintf("%s records no image, so there is no image SBOM to write", o.manifest)}}
	}
	plan := m.Images[0]
	if want := imageSBOMName(m.Version); plan.SBOM != want || !baseName(plan.SBOM) {
		return "", &findingsError{[]string{fmt.Sprintf("%s names the image SBOM %q, want %q", o.manifest, plan.SBOM, want)}}
	}
	var findings []string
	var scans []platformScan
	for _, pm := range plan.Manifests {
		s, problems, err := readScan(o.scans, pm)
		if err != nil {
			return "", err
		}
		findings = append(findings, problems...)
		scans = append(scans, s)
	}
	if len(plan.Manifests) == 0 {
		findings = append(findings, fmt.Sprintf("%s lists no platform manifest of the image", o.manifest))
	}
	if len(findings) > 0 {
		return "", &findingsError{findings}
	}
	data, err := json.MarshalIndent(mergeImageSBOM(plan, m.CommitTime, scans), "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(o.manifest), plan.SBOM), append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return plan.SBOM, nil
}

// imageSBOMProblems audits the image SBOM: CycloneDX 1.7 whose subject is
// the image index, with one container component per platform image, in
// plan order, each naming every module that platform's ruralzd links.
func imageSBOMProblems(file string, plan imagePlan, infos map[string]*buildinfo.BuildInfo) ([]string, error) {
	name := filepath.Base(file)
	var doc struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		Metadata    struct {
			Component struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"component"`
		} `json:"metadata"`
		Components []struct {
			Type       string         `json:"type"`
			Name       string         `json:"name"`
			Version    string         `json:"version"`
			Components []cdxComponent `json:"components"`
		} `json:"components"`
	}
	found, problem, err := readJSONDoc(file, maxSBOMBytes, &doc)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return []string{"image SBOM " + name + " is missing (releasekit sbom writes it from Syft's scans)"}, nil
	case problem != "":
		return []string{"image SBOM " + problem}, nil
	}
	var out []string
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != cycloneDXVersion {
		out = append(out, fmt.Sprintf("image SBOM %s is %s %s, want CycloneDX %s", name, doc.BOMFormat, doc.SpecVersion, cycloneDXVersion))
	}
	if c := doc.Metadata.Component; c.Type != "container" || c.Name != plan.Repository || c.Version != plan.Digest {
		out = append(out, fmt.Sprintf("image SBOM %s describes the %s %s %s, want the container %s %s", name, c.Type, c.Name, c.Version, plan.Repository, plan.Digest))
	}
	if len(doc.Components) != len(plan.Manifests) {
		out = append(out, fmt.Sprintf("image SBOM %s lists %d platform images, want %d", name, len(doc.Components), len(plan.Manifests)))
		return out, nil
	}
	for i, pm := range plan.Manifests {
		c := doc.Components[i]
		if c.Type != "container" || c.Name != plan.Repository || c.Version != pm.Digest {
			out = append(out, fmt.Sprintf("image SBOM %s: component %d is the %s %s %s, want the %s image %s %s", name, i, c.Type, c.Name, c.Version, pm.Platform, plan.Repository, pm.Digest))
			continue
		}
		bi := infos[pm.Platform]
		if bi == nil {
			continue
		}
		purls := map[string]bool{}
		collectPurls(c.Components, purls)
		for _, d := range bi.Deps {
			if !hasModule(purls, d) {
				out = append(out, fmt.Sprintf("image SBOM %s: the %s image lacks %s %s, which its ruralzd links", name, pm.Platform, d.Path, d.Version))
			}
		}
	}
	return out, nil
}
