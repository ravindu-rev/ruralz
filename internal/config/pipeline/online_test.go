// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// TestOnlineStage covers the online stage: it follows the digest, which it
// never changes, and in M1 fails each Plugin with RZ-CFG-028 at the
// Plugin resource, naming it, while a Bundle without Plugins passes; a
// run with an offline error never reaches it.
func TestOnlineStage(t *testing.T) {
	p := newPipeline(t, Options{})
	shop := loader.Source{Path: filepath.Join(examplesDir, "shop-bundle")}
	offline, err := p.Run(t.Context(), Request{Source: shop})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	online, err := p.Run(t.Context(), Request{Source: shop, Online: true})
	if err != nil {
		t.Fatalf("Run(Online): %v", err)
	}
	wantTexts(t, "online", online.Diagnostics,
		`plugins/geo-block.yaml:1:1 error RZ-CFG-028 Plugin/geo-block: Plugin "geo-block" cannot be checked online: fetching Plugin artifacts by digest is Planned (M2)`)
	if online.Validated == nil || online.Validated.Revision.Digest != offline.Validated.Revision.Digest {
		t.Error("the online stage changed the digest or dropped the Revision")
	}
	rest, err := p.Run(t.Context(), Request{Source: loader.Source{Path: filepath.Join(examplesDir, "shop-rest-bundle")}, Online: true})
	if err != nil {
		t.Fatalf("Run(shop-rest-bundle): %v", err)
	}
	wantClean(t, "a Bundle without Plugins", rest.Diagnostics)
	failing := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"r.yaml":      routeDoc("a", "nowhere", ""),
	}), Request{Online: true})
	if len(withCode(failing.Diagnostics, CodeArtifact)) != 0 || failing.Validated != nil {
		t.Errorf("a failing run reached the online stage: %q", texts(failing.Diagnostics))
	}
	if ds, err := p.Online(t.Context(), nil); ds != nil || err != nil {
		t.Errorf("Online(nil) = %q, %v", texts(ds), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Online(ctx, online.Validated); !errors.Is(err, context.Canceled) {
		t.Errorf("Online(canceled) = %v, want context.Canceled", err)
	}
}
