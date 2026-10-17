// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestZeroResolver covers the WP-38 S2 round-4 minor that a zero Resolver
// looked usable but panicked in Run, Resolve and CheckPolicies (a nil
// registry), and pins the chosen behavior for each exported method: the
// zero value and a nil *Resolver return exactly what New(nil, Options{})
// returns, with the default cap (not a cap of zero, which would keep one
// finding) and no clock.
func TestZeroResolver(t *testing.T) {
	b := newBuilder()
	b.policy("a", v1alpha1.PolicyTypeRateLimit, nil, slot("s"))
	b.policy("c", v1alpha1.PolicyTypeRateLimit, nil, slot("s"))
	b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot("gs"))
	b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot("gs"))
	for i := range 5 {
		b.policy(fmt.Sprintf("jwt%d", i), v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
	}
	b.gateway("g")
	for i := range 5 {
		b.route(fmt.Sprintf("r%d", i), []string{"a", "c", "x"}, nil, nil)
	}
	bundle := b.bundle()
	want := mustNew(t, nil, Options{})
	wantChains, wantDs, err := want.Run(t.Context(), bundle, nil, 1)
	if err != nil || len(wantDs) != 10 {
		t.Fatalf("New(nil, Options{}).Run = %d findings, %v; want 10", len(wantDs), err)
	}
	var nilResolver *Resolver
	for name, r := range map[string]*Resolver{"zero value": {}, "nil pointer": nilResolver} {
		t.Run(name, func(t *testing.T) {
			for _, workers := range []int{1, 4} {
				chains, ds, err := r.Run(t.Context(), bundle, nil, workers)
				if err != nil || !reflect.DeepEqual(chains, wantChains) || !reflect.DeepEqual(ds, wantDs) {
					t.Errorf("Run on %d workers = %d chains, %d findings, %v; want New's", workers, len(chains), len(ds), err)
				}
				_, rds, err := r.Resolve(t.Context(), bundle, nil, workers)
				_, wrds, _ := want.Resolve(t.Context(), bundle, nil, workers)
				if err != nil || !reflect.DeepEqual(rds, wrds) {
					t.Errorf("Resolve on %d workers = %d findings, %v; want New's %d", workers, len(rds), err, len(wrds))
				}
			}
			if got, want := r.CheckPolicies(bundle, nil), want.CheckPolicies(bundle, nil); !reflect.DeepEqual(got, want) || len(got) != 5 {
				t.Errorf("CheckPolicies = %d findings, want New's %d", len(got), len(want))
			}
			c := wantChains["r0"]
			if got, want := r.Rows(c, bundle), want.Rows(c, bundle); !reflect.DeepEqual(got, want) || len(got) == 0 {
				t.Errorf("Rows = %+v, want %+v", got, want)
			}
			if r.Rows(nil, bundle) != nil {
				t.Error("Rows of a nil chain is not nil")
			}
			if _, ds, err := r.Run(t.Context(), hub.NewBundle(nil), nil, 1); err != nil || ds != nil {
				t.Errorf("Run of an empty Bundle = %v, %v", ds, err)
			}
		})
	}
}
