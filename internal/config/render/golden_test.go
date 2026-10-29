// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestShopBundleGolden pins the rendered example Bundle for each
// Environment of its --environments file and with no Environment, in YAML
// and in JSON: the overlay merged (prod raises the orders retries and
// replaces the secondary candidate's when; staging deletes that
// candidate), every ${VAR} substituted (the OTLP endpoint takes the
// Environment's value, or its default without one), resources in
// canonical order with their envelope first, defaults never written and
// secretRefs written as references.
func TestShopBundleGolden(t *testing.T) {
	for _, env := range []string{"prod", "staging", ""} {
		name := env
		if name == "" {
			name = "no-environment"
		}
		t.Run(name, func(t *testing.T) {
			r := shop(t, env)
			golden(t, filepath.Join("testdata", "golden", "bundle-"+name+".yaml"), renderAs(t, r, FormatYAML))
			golden(t, filepath.Join("testdata", "golden", "bundle-"+name+".json"), renderAs(t, r, FormatJSON))
		})
	}
}

// TestRenderTwiceSameRevision is the configuration model's idempotence
// rule ("Overlays"): loading a rendered Bundle as file mode does and
// rendering it again yields the same Revision and the same bytes, for
// every Environment of the example Bundle.
func TestRenderTwiceSameRevision(t *testing.T) {
	for _, env := range []string{"prod", "staging", ""} {
		r := shop(t, env)
		first := renderYAML(t, r)
		again := rendered(t, first)
		if again.validated.Revision.Digest != r.validated.Revision.Digest {
			t.Errorf("env %q: rendered Bundle builds %s, source %s", env, again.validated.Revision.Digest.Short(), r.validated.Revision.Digest.Short())
		}
		if second := renderYAML(t, again); !bytes.Equal(second, first) {
			t.Errorf("env %q: second render differs:\n--- first\n%s--- second\n%s", env, first, second)
		}
	}
}

// TestEnvironmentsRenderApart checks that the overlay of each Environment
// reaches the output: the prod and staging renders of the example Bundle
// differ, and so do their Revisions.
func TestEnvironmentsRenderApart(t *testing.T) {
	prod, staging := shop(t, "prod"), shop(t, "staging")
	if bytes.Equal(renderYAML(t, prod), renderYAML(t, staging)) {
		t.Error("prod and staging render the same Bundle")
	}
	if prod.validated.Revision.Digest == staging.validated.Revision.Digest {
		t.Error("prod and staging build the same Revision")
	}
}
