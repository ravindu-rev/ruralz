// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

func testPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	celc, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	pl, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func testParams() BundleParams {
	p := DefaultBundleParams(3)
	p.Backends = [3]string{"127.0.0.1:9001", "127.0.0.1:9002", "127.0.0.1:9003"}
	return p
}

// TestRenderBundle renders the chaos Bundle for every overlay and State
// Store topology: the result is a one-file Bundle a Node loads, holding
// the run's backends, limits and topology, and the overlays add the cache
// connection, with or without TLS, drop the main connection's TLS block,
// or give /plain a timeout.
func TestRenderBundle(t *testing.T) {
	pl := testPipeline(t)
	for _, tc := range []struct {
		name, overlay, topology string
		want, absent            []string
	}{
		{"default", "", "standalone", []string{`address: "127.0.0.1:9002"`, "topology: standalone", "caCertificate:", "prefix: /refunded", "schema: false"}, []string{"cache-url"}},
		{"cluster", "", "cluster", []string{"topology: cluster"}, nil},
		{"sentinel", "", "sentinel", []string{"topology: sentinel"}, nil},
		{"cache connection", OverlayCacheConnection, "standalone", []string{"/etc/ruralz/state/cache-url", "/etc/ruralz/state/cache-ca.crt"}, nil},
		{"cleartext", OverlayCleartext, "standalone", nil, []string{"/etc/ruralz/state/ca.crt"}},
		{"cache connection cleartext", OverlayCacheCleartext, "standalone", []string{"/etc/ruralz/state/cache-url"}, []string{"/etc/ruralz/state/cache-ca.crt"}},
		{"route timeout", OverlayRouteTimeout, "standalone", []string{`timeout: "30s"`}, []string{"cache-url"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			p.Overlay, p.Topology = tc.overlay, tc.topology
			p.Limited = Limit{Requests: 120, Ceiling: 20}
			out, err := RenderBundle(t.Context(), pl, p)
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			for _, w := range append(tc.want, "requests: 120", "perNodeCeiling: 20") {
				if !strings.Contains(s, w) {
					t.Errorf("rendered Bundle lacks %q:\n%s", w, s)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(s, a) {
					t.Errorf("rendered Bundle holds %q", a)
				}
			}
			res, err := pl.Run(context.Background(), pipeline.Request{
				Source:    loader.Source{FS: fstest.MapFS{"bundle.yaml": &fstest.MapFile{Data: out}}, Path: "bundle.yaml"},
				Variables: subst.Map{},
			})
			if err != nil || res.Validated == nil {
				t.Fatalf("the rendered Bundle does not load as a Node loads it: %v %v", err, res.Diagnostics)
			}
		})
	}
}

// TestRenderBundleRefusals checks that parameters a Node would refuse
// fail the render: a missing backend, an unknown topology or overlay, and
// a declared ceiling above its limit (RZ-CFG-005).
func TestRenderBundleRefusals(t *testing.T) {
	pl := testPipeline(t)
	for _, tc := range []struct {
		name string
		edit func(*BundleParams)
		want string
	}{
		{"no backend", func(p *BundleParams) { p.Backends[1] = "" }, "backend 2"},
		{"topology", func(p *BundleParams) { p.Topology = "ring" }, "topology"},
		{"overlay", func(p *BundleParams) { p.Overlay = "prod" }, "overlay"},
		{"ceiling above limit", func(p *BundleParams) { p.Limited = Limit{Requests: 10, Ceiling: 11} }, "RZ-CFG-005"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			tc.edit(&p)
			if _, err := RenderBundle(t.Context(), pl, p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderBundle = %v, want an error naming %q", err, tc.want)
			}
		})
	}
	if err := diagError(nil); !errors.Is(err, err) || err.Error() != "no Revision" {
		t.Errorf("diagError(nil) = %v", err)
	}
}

// TestBundleKeySlots checks the hash slots the chaos Bundle's keys take,
// as Chaos Bundle requires: each Consumer's /limited ratelimit key and
// its quota key sit in different slots, so each makes its own State Store
// call, and the co-tenant key sits on the hot key's shard of a
// three-primary cluster.
func TestBundleKeySlots(t *testing.T) {
	for _, c := range Consumers() {
		rl, qt := keys.DigestSlot(statestore.DigestOf("rl:"+c)), keys.DigestSlot(statestore.DigestOf(c))
		if rl == qt {
			t.Errorf("Consumer %s: ratelimit and quota keys share slot %d", c, rl)
		}
	}
	shard := func(slot uint16) int {
		switch {
		case slot <= 5460:
			return 0
		case slot <= 10922:
			return 1
		default:
			return 2
		}
	}
	hot, cot := keys.DigestSlot(statestore.DigestOf(HotKey)), keys.DigestSlot(statestore.DigestOf(CotenantKey))
	if shard(hot) != shard(cot) {
		t.Errorf("hot key slot %d (shard %d), co-tenant slot %d (shard %d): want one shard", hot, shard(hot), cot, shard(cot))
	}
}
