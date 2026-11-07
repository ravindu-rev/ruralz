// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// Tests for OCI references: REGISTRY/REPOSITORY with a tag, a full sha256
// digest or both; the registry is always explicit.

func TestParseReference(t *testing.T) {
	d := revision.Sum([]byte("manifest"))
	tests := []struct {
		in   string
		want Reference
	}{
		{in: "ghcr.io/acme/gateway:prod", want: Reference{Registry: "ghcr.io", Repository: "acme/gateway", Tag: "prod"}},
		{in: "ghcr.io/acme/gateway@" + d.String(), want: Reference{Registry: "ghcr.io", Repository: "acme/gateway", Digest: d}},
		{in: "ghcr.io/acme/gateway:1.4.0@" + d.String(), want: Reference{Registry: "ghcr.io", Repository: "acme/gateway", Tag: "1.4.0", Digest: d}},
		{in: "localhost:5000/config:latest", want: Reference{Registry: "localhost:5000", Repository: "config", Tag: "latest"}},
		{in: "127.0.0.1:443/a/b/c:v1", want: Reference{Registry: "127.0.0.1:443", Repository: "a/b/c", Tag: "v1"}},
		{in: "[::1]:5000/repo:t", want: Reference{Registry: "[::1]:5000", Repository: "repo", Tag: "t"}},
		{in: "registry.example.com/a.b_c__d---e/f:Tag_1.x-y", want: Reference{Registry: "registry.example.com", Repository: "a.b_c__d---e/f", Tag: "Tag_1.x-y"}},
		{in: "r.io/x:" + strings.Repeat("t", 128), want: Reference{Registry: "r.io", Repository: "x", Tag: strings.Repeat("t", 128)}},
	}
	for _, tt := range tests {
		got, err := ParseReference(tt.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseReference(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if got.String() != tt.in {
			t.Errorf("ParseReference(%q).String() = %q", tt.in, got.String())
		}
	}
}

func TestParseReferenceRefuses(t *testing.T) {
	d := revision.Sum([]byte("manifest"))
	for name, in := range map[string]string{
		"no registry":           "gateway:prod",
		"empty":                 "",
		"no repository":         "ghcr.io/",
		"no tag or digest":      "ghcr.io/acme/gateway",
		"uppercase repository":  "ghcr.io/Acme/gateway:prod",
		"repository separator":  "ghcr.io/acme/-gateway:prod",
		"triple underscore":     "ghcr.io/a___b:prod",
		"empty component":       "ghcr.io/acme//gateway:prod",
		"tag too long":          "ghcr.io/x:" + strings.Repeat("t", 129),
		"tag leading dot":       "ghcr.io/x:.t",
		"tag bad character":     "ghcr.io/x:t+1",
		"empty tag":             "ghcr.io/x:",
		"display form digest":   "ghcr.io/x@rev-" + d.Hex()[:12],
		"sha512 digest":         "ghcr.io/x@sha512:" + d.Hex(),
		"uppercase digest":      "ghcr.io/x@sha256:" + strings.ToUpper(d.Hex()),
		"short digest":          "ghcr.io/x@" + d.String()[:40],
		"user information":      "user@ghcr.io/x:t",
		"port zero":             "ghcr.io:0/x:t",
		"port out of range":     "ghcr.io:65536/x:t",
		"port leading zero":     "ghcr.io:0443/x:t",
		"empty port":            "ghcr.io:/x:t",
		"registry with scheme":  "https://ghcr.io/x:t",
		"bad host label":        "-ghcr.io/x:t",
		"bracket without IPv6":  "[ghcr.io]/x:t",
		"colon inside the path": "ghcr.io/a:b/c",
	} {
		t.Run(name, func(t *testing.T) {
			if ref, err := ParseReference(in); err == nil {
				t.Fatalf("ParseReference(%q) = %+v, want an error", in, ref)
			} else if !errors.Is(err, ErrReference) {
				t.Fatalf("ParseReference(%q) = %v, want ErrReference", in, err)
			}
		})
	}
}

func TestReferenceName(t *testing.T) {
	ref, err := ParseReference("ghcr.io/acme/gateway:prod")
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.Name(); got != "ghcr.io/acme/gateway" {
		t.Errorf("Name() = %q", got)
	}
}

// FuzzParseReference: never panics, and an accepted reference prints back
// as itself and parses to the same value.
func FuzzParseReference(f *testing.F) {
	d := revision.Sum([]byte("m"))
	for _, s := range []string{
		"ghcr.io/acme/gateway:prod", "ghcr.io/a@" + d.String(), "localhost:5000/x:t@" + d.String(),
		"[::1]/x:t", "r/x", "", "a/b:c:d", "a/b@sha256:", "a:1/b:c",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseReference(s)
		if err != nil {
			if !errors.Is(err, ErrReference) {
				t.Fatalf("ParseReference(%q) error %v does not match ErrReference", s, err)
			}
			return
		}
		again, err := ParseReference(ref.String())
		if err != nil || again != ref {
			t.Fatalf("ParseReference(%q) = %+v, reparsed %+v, %v", s, ref, again, err)
		}
	})
}
