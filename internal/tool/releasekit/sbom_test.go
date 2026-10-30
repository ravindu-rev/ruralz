// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSBOMMerge: the image SBOM is one CycloneDX 1.7 document whose
// subject is the image index and whose components are each platform
// image, in plan order, holding its scan's packages under platform-unique
// references, with the scans' dependencies; merging the same scans again
// writes the same bytes.
func TestSBOMMerge(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	m := fx.release(t)
	plan := m.Images[0]
	path := filepath.Join(fx.out, plan.SBOM)
	first := readBack(t, path)
	if _, err := runSBOM(sbomOptions{manifest: filepath.Join(fx.out, manifestName), scans: fx.writeScans(t, m)}); err != nil {
		t.Fatal(err)
	}
	if again := readBack(t, path); !bytes.Equal(first, again) {
		t.Error("merging the same scans twice gives different documents")
	}

	var doc struct {
		BOMFormat    string `json:"bomFormat"`
		SpecVersion  string `json:"specVersion"`
		SerialNumber string `json:"serialNumber"`
		Metadata     struct {
			Timestamp string `json:"timestamp"`
			Tools     struct {
				Components []struct {
					Name string `json:"name"`
				} `json:"components"`
			} `json:"tools"`
			Component struct {
				Ref     string `json:"bom-ref"`
				Type    string `json:"type"`
				Name    string `json:"name"`
				Version string `json:"version"`
				Purl    string `json:"purl"`
			} `json:"component"`
		} `json:"metadata"`
		Components []struct {
			Ref        string `json:"bom-ref"`
			Type       string `json:"type"`
			Name       string `json:"name"`
			Version    string `json:"version"`
			Purl       string `json:"purl"`
			Components []struct {
				Ref  string `json:"bom-ref"`
				Purl string `json:"purl"`
			} `json:"components"`
		} `json:"components"`
		Dependencies []struct {
			Ref       string   `json:"ref"`
			DependsOn []string `json:"dependsOn"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.7" || doc.Metadata.Timestamp != "2026-10-01T12:30:45Z" ||
		!regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(doc.SerialNumber) {
		t.Errorf("header = %s %s %s %s", doc.BOMFormat, doc.SpecVersion, doc.Metadata.Timestamp, doc.SerialNumber)
	}
	if len(doc.Metadata.Tools.Components) != 1 || doc.Metadata.Tools.Components[0].Name != "syft" {
		t.Errorf("tools = %+v, want syft once", doc.Metadata.Tools.Components)
	}
	c := doc.Metadata.Component
	wantPurl := "pkg:oci/ruralzd@" + strings.Replace(plan.Digest, ":", "%3A", 1) + "?repository_url=ghcr.io/ravindu-rev/ruralzd"
	if c.Ref != indexRef || c.Type != "container" || c.Name != plan.Repository || c.Version != plan.Digest || c.Purl != wantPurl {
		t.Errorf("subject = %+v, want the image index %s", c, plan.Digest)
	}
	if len(doc.Components) != 2 {
		t.Fatalf("%d platform components, want 2", len(doc.Components))
	}
	for i, pm := range plan.Manifests {
		p := doc.Components[i]
		arch := strings.TrimPrefix(pm.Platform, "linux/")
		if p.Ref != pm.Platform || p.Type != "container" || p.Version != pm.Digest ||
			p.Purl != "pkg:oci/ruralzd@"+strings.Replace(pm.Digest, ":", "%3A", 1)+"?arch="+arch+"&repository_url=ghcr.io/ravindu-rev/ruralzd" {
			t.Errorf("component %d = %+v, want the %s image %s", i, p, pm.Platform, pm.Digest)
		}
		if len(p.Components) != 1 || !strings.HasPrefix(p.Components[0].Ref, pm.Platform+":pkg:golang/") ||
			p.Components[0].Purl != "pkg:golang/example.com/upper/mod@v1.2.3%2Bincompatible?type=module" {
			t.Errorf("component %d packages = %+v", i, p.Components)
		}
	}
	if len(doc.Dependencies) != 3 || doc.Dependencies[0].Ref != indexRef || strings.Join(doc.Dependencies[0].DependsOn, " ") != "linux/amd64 linux/arm64" ||
		doc.Dependencies[1].Ref != "linux/amd64" || !strings.HasPrefix(doc.Dependencies[1].DependsOn[0], "linux/amd64:pkg:golang/") {
		t.Errorf("dependencies = %+v", doc.Dependencies)
	}
	if doc.SerialNumber != sbomSerial(plan.Digest) || sbomSerial(plan.Digest) == sbomSerial(plan.Manifests[0].Digest) {
		t.Error("the serial number is not the image index digest's name-based UUID")
	}
}

// TestSBOMFindings: a manifest without an image, a missing scan, a scan of
// another image and a scan that is not CycloneDX 1.7 JSON are findings, and
// no image SBOM is written.
func TestSBOMFindings(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, fx *fixture, m *manifest, scans string)
		want   string
	}{
		{"no image", func(t *testing.T, fx *fixture, _ *manifest, _ string) {
			rewriteManifest(t, fx, func(m *manifest) { m.Images = images(m.Version) })
		}, "records no image, so there is no image SBOM to write"},
		{"SBOM name", func(t *testing.T, fx *fixture, _ *manifest, _ string) {
			rewriteManifest(t, fx, func(m *manifest) { m.Images[0].SBOM = "../image.cdx.json" })
		}, `names the image SBOM "../image.cdx.json", want "ruralzd_0.1.0_image.cdx.json"`},
		{"missing scan", func(_ *testing.T, _ *fixture, _ *manifest, scans string) {
			_ = os.Remove(filepath.Join(scans, "linux_arm64.cdx.json"))
		}, "no scan of the linux/arm64 image: Syft writes linux_arm64.cdx.json"},
		{"scan of another image", func(t *testing.T, _ *fixture, m *manifest, scans string) {
			writeFile(t, filepath.Join(scans, "linux_amd64.cdx.json"), string(fakeScan(t, m.Images[0].Manifests[1].Digest, nil)))
		}, "the linux/amd64 scan is of \"" + "sha256:"},
		{"scan version", func(t *testing.T, _ *fixture, m *manifest, scans string) {
			doc := strings.Replace(string(fakeScan(t, m.Images[0].Manifests[0].Digest, nil)), `"specVersion":"1.7"`, `"specVersion":"1.6"`, 1)
			writeFile(t, filepath.Join(scans, "linux_amd64.cdx.json"), doc)
		}, "the linux/amd64 scan is CycloneDX 1.6, want CycloneDX 1.7"},
		{"scan not JSON", func(t *testing.T, _ *fixture, _ *manifest, scans string) {
			writeFile(t, filepath.Join(scans, "linux_amd64.cdx.json"), "<bom/>")
		}, "scan linux_amd64.cdx.json is not a CycloneDX JSON document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			m := fx.release(t)
			_ = os.Remove(filepath.Join(fx.out, m.Images[0].SBOM))
			scans := fx.writeScans(t, m)
			c.change(t, fx, m, scans)
			_, err := runSBOM(sbomOptions{manifest: filepath.Join(fx.out, manifestName), scans: scans})
			var fe *findingsError
			if !errors.As(err, &fe) || !hasFinding(fe.findings, c.want) {
				t.Errorf("runSBOM = %v, want a finding %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(fx.out, m.Images[0].SBOM)); !os.IsNotExist(err) {
				t.Error("an image SBOM was written despite findings")
			}
		})
	}
	if _, err := runSBOM(sbomOptions{manifest: filepath.Join(t.TempDir(), "absent.json"), scans: t.TempDir()}); err == nil {
		t.Error("a missing manifest is not an error")
	}
}
