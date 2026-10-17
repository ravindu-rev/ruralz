// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestSlotRelatedFirstEntry covers the WP-38 S2 round-2 finding that
// RZ-CFG-018 lacked the related location 01 req 39 requires ("second ref
// (first ref)"), for its one producer, attachList, at each of the scopes
// it serves: the Gateway list, a Route list and an Upstream list. Each
// list is positioned (entry i at line 10+i, column 7 of its own file) and
// checked in both orders, and with three Policies in one slot, where both
// later entries relate to the first; each finding is at the later entry
// and relates, as "first entry at", to the file, line and column of the
// first entry in the slot, on 1 and 4 workers. Without positions (no
// tree, so the first entry's location has no file) the finding has no
// related location.
func TestSlotRelatedFirstEntry(t *testing.T) {
	type scope struct {
		name, dir string
		typ       v1alpha1.PolicyType
		attach    func(b *builder, names []string) *hub.Resource
	}
	scopes := []scope{
		{"Gateway", "gateway", v1alpha1.PolicyTypeRateLimit, func(b *builder, names []string) *hub.Resource {
			b.gateway(names...)
			return b.rs[len(b.rs)-1]
		}},
		{"Route", "routes", v1alpha1.PolicyTypeAuthJWT, func(b *builder, names []string) *hub.Resource {
			b.route("orders", names, nil, nil)
			return b.rs[len(b.rs)-1]
		}},
		{"Upstream", "upstreams", v1alpha1.PolicyTypeAuthUpstreamOAuth2, func(b *builder, names []string) *hub.Resource {
			b.upstream("orders", names...)
			return b.rs[len(b.rs)-1]
		}},
	}
	type finding struct {
		at, first int // the lines of the entry and of the first entry in the slot
	}
	for _, sc := range scopes {
		for _, tc := range []struct {
			name  string
			list  []string
			wants []finding
		}{
			{"two Policies", []string{"a", "b"}, []finding{{11, 10}}},
			{"two Policies reversed", []string{"b", "a"}, []finding{{11, 10}}},
			{"three Policies", []string{"a", "b", "c"}, []finding{{11, 10}, {12, 10}}},
			{"three Policies, another order", []string{"c", "a", "b"}, []finding{{11, 10}, {12, 10}}},
		} {
			for _, positioned := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/positioned=%v", sc.name, tc.name, positioned), func(t *testing.T) {
					b := newBuilder()
					for _, p := range []string{"a", "b", "c"} {
						b.policy(p, sc.typ, nil, slot("s"))
					}
					owner := sc.attach(b, tc.list)
					var files *tree.FileTable
					file := ""
					if positioned {
						files = &tree.FileTable{}
						positionList(files, owner, sc.dir, fieldPolicies, tc.list)
						file = sc.dir + "/" + owner.Name + ".yaml"
					}
					bundle := b.bundle()
					for _, workers := range []int{1, 4} {
						_, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), bundle, files, workers)
						if err != nil {
							t.Fatal(err)
						}
						var slots diag.List
						for _, d := range ds {
							if d.Code == CodeSlot {
								slots = append(slots, d)
							}
						}
						if len(slots) != len(tc.wants) {
							t.Fatalf("workers %d: %d RZ-CFG-018, want %d:\n%s", workers, len(slots), len(tc.wants), diagText(t, ds))
						}
						for i, want := range tc.wants {
							d := slots[i]
							if !positioned {
								if d.File != "" || len(d.Related) != 0 {
									t.Errorf("workers %d: %+v, want no location and no related location", workers, d)
								}
								continue
							}
							wantRel := []diag.Related{{Location: diag.Location{File: file, Line: want.first, Column: 7}, Message: "first entry at"}}
							if d.Location != (diag.Location{File: file, Line: want.at, Column: 7}) || !slices.Equal(d.Related, wantRel) {
								t.Errorf("workers %d: finding at %+v related %+v, want at line %d related %+v", workers, d.Location, d.Related, want.at, wantRel)
							}
						}
					}
				})
			}
		}
	}
}
