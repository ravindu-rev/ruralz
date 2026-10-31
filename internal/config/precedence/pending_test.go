// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The tests in this file pin behaviors that the documents leave open and
// that wait for the owner's decision, so that the decision, whichever it
// is, changes them on purpose.

// TestZeroPhaseReplacement pins today's answer to an open question: a Route
// Policy whose Phase selection is empty (headers or transform.* with
// config {}, which get no rows) still replaces the Gateway Policy in its
// slot, since the slot alone decides, so the Gateway Policy is removed
// with reason replaced and the replacing Policy runs in no Phase: Rows
// lists the removal and no row of its own. One row each for headers,
// transform.request and transform.response, on 1 and 4 workers.
func TestZeroPhaseReplacement(t *testing.T) {
	b := newBuilder()
	b.policy("hg", v1alpha1.PolicyTypeHeaders, headersCfg(true, true), slot("hdr"))
	b.policy("tg", v1alpha1.PolicyTypeTransformRequest, transformReqCfg("x"), slot("treq"))
	b.policy("ug", v1alpha1.PolicyTypeTransformResponse, &v1alpha1.TransformResponseConfig{Body: "x"}, slot("tresp"))
	b.policy("hr", v1alpha1.PolicyTypeHeaders, &v1alpha1.HeadersConfig{}, slot("hdr"))
	b.policy("tr", v1alpha1.PolicyTypeTransformRequest, &v1alpha1.TransformRequestConfig{}, slot("treq"))
	b.policy("ur", v1alpha1.PolicyTypeTransformResponse, &v1alpha1.TransformResponseConfig{}, slot("tresp"))
	b.gateway("hg", "tg", "ug")
	b.route("r", []string{"hr", "tr", "ur"}, nil, nil)
	bundle := b.bundle()
	for _, workers := range []int{1, 4} {
		chains, ds, err := mustNew(t, nil, Options{}).Run(t.Context(), bundle, nil, workers)
		if err != nil || len(ds) != 0 {
			t.Fatalf("Run: %v %v", err, ds)
		}
		c := chains["r"]
		for p := range phase.Count {
			if len(c.Client[p]) != 0 {
				t.Errorf("workers %d: %s runs %q, want nothing", workers, p, names(c.Client[p]))
			}
		}
		want := []hub.Removed{
			{Policy: "hg", Slot: "hdr", By: "hr", Reason: hub.Replaced},
			{Policy: "tg", Slot: "treq", By: "tr", Reason: hub.Replaced},
			{Policy: "ug", Slot: "tresp", By: "ur", Reason: hub.Replaced},
		}
		if !slices.Equal(c.Removed, want) {
			t.Errorf("workers %d: Removed = %+v, want %+v", workers, c.Removed, want)
		}
		var rows []string
		for _, row := range mustNew(t, nil, Options{}).Rows(c, bundle) {
			rows = append(rows, row.Phase+" "+row.Leg+" "+row.Policy+" "+row.Reason)
		}
		wantRows := []string{
			"none none hg replaced in slot hdr by hr",
			"none none tg replaced in slot treq by tr",
			"none none ug replaced in slot tresp by ur",
		}
		if !slices.Equal(rows, wantRows) {
			t.Errorf("workers %d: rows %q, want %q", workers, rows, wantRows)
		}
	}
}

// TestUnreachedUpstreamOnlyChecked pins today's answer to an open question:
// stage I checks the list of an Upstream no Route reaches (its RZ-CFG-020
// is reported) and builds its leg, but exports legs only through Route
// chains, so its valid attachments appear in no chain, and a Bundle without
// Routes has no chain at all for its Gateway attachments. A consumer that
// needs every attachment's Phases (the CEL sites, say) cannot take them
// from the chains alone.
func TestUnreachedUpstreamOnlyChecked(t *testing.T) {
	b := newBuilder()
	b.policy("hdr", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.policy("jwt", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	b.gateway("rl")
	b.upstream("orphan", "hdr", "jwt")
	b.upstream("used")
	b.route("r", nil, nil, []string{"used"})
	chains, ds := resolve(t, b.bundle())
	if got := codes(ds); !slices.Equal(got, []string{CodeScope}) || ds[0].Resource.Name != "orphan" {
		t.Errorf("diagnostics %v, want RZ-CFG-020 on Upstream orphan", ds)
	}
	for name, c := range chains {
		for _, l := range c.Legs {
			if l.Upstream == "orphan" {
				t.Errorf("%s reaches orphan", name)
			}
		}
	}
	nb := newBuilder()
	nb.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	nb.gateway("rl")
	if chains, ds := resolve(t, nb.bundle()); len(chains) != 0 || len(ds) != 0 {
		t.Errorf("a Bundle without Routes: %d chains, %v", len(chains), ds)
	}
}
