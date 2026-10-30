// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stageF runs the profile parser and the rendered schema on files and
// returns stage F's findings, which loadFiles refuses.
func stageF(t *testing.T, files map[string]string) diag.List {
	t.Helper()
	s := pipeline(t)
	table := &tree.FileTable{}
	var out diag.List
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		id := table.Add(tree.File{Path: rel, Role: tree.RoleBase})
		docs, ds, err := profile.Parse(t.Context(), []byte(files[rel]), id, rel, profile.FormatYAML, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("parse %s: %v\n%s", rel, err, diagText(t, ds))
		}
		for _, d := range docs {
			k, _ := d.Root.Get("kind")
			meta, _ := d.Root.Get("metadata")
			n, _ := meta.Get("name")
			av, _ := d.Root.Get("apiVersion")
			res := &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: n.Text}, APIVersion: av.Text, Root: d.Root, Start: d.Start}
			ds, _ := s.views.Validate(res, table)
			out = append(out, ds...)
		}
	}
	return out
}

// TestStateStoreZoneForm: a zone map zone is 1 to 63 characters from
// [a-z0-9-], the form of RURALZ_ZONE, which the schema states and stage F
// reports as RZ-CFG-005 at the entry's zone, in stateStore.zones and in
// stateStore.cache.zones; a zone in the form passes stages F to H.
func TestStateStoreZoneForm(t *testing.T) {
	long := strings.Repeat("a", 63)
	tests := []struct {
		zone string
		ok   bool
	}{
		{"eu-west-1a", true},
		{"a", true},
		{"0", true},
		{"-", true},
		{long, true},
		{"", false},
		{long + "a", false},
		{"EU-west-1a", false},
		{"eu_west_1a", false},
		{"eu.west", false},
		{"eu west", false},
		{"zoné", false},
	}
	v := newValidator(t, nil)
	for _, tc := range tests {
		for _, cache := range []bool{false, true} {
			lines := "    zones:\n      - {host: valkey-0.state.internal, zone: " + strconv.Quote(tc.zone) + "}\n"
			path := "spec.stateStore.zones[host=valkey-0.state.internal].zone"
			if cache {
				lines = cacheURL + "      zones:\n        - {host: valkey-0.state.internal, zone: " + strconv.Quote(tc.zone) + "}\n"
				path = "spec.stateStore.cache.zones[host=valkey-0.state.internal].zone"
			}
			t.Run(path+"="+strconv.Quote(tc.zone), func(t *testing.T) {
				files := stateStoreGateway(t, lines)
				ds := stageF(t, files)
				if tc.ok {
					if len(ds) > 0 {
						t.Fatalf("zone %q: want no stage F finding, got:\n%s", tc.zone, diagText(t, ds))
					}
					b, table := loadFiles(t, files)
					if _, ds := stageH(t, v, b, table, 2); len(ds) > 0 {
						t.Fatalf("zone %q: want no stage H finding, got:\n%s", tc.zone, diagText(t, ds))
					}
					return
				}
				if len(ds) == 0 {
					t.Fatalf("zone %q: want an %s at %s, got no finding", tc.zone, CodeSchema, path)
				}
				for _, d := range ds {
					if d.Code != CodeSchema || d.Path.String() != path {
						t.Errorf("zone %q: finding %s at %s, want %s at %s", tc.zone, d.Code, d.Path, CodeSchema, path)
					}
				}
			})
		}
	}
}

// TestZonesBesideMemoryWithoutDefaults: stage H reads an unset driver as
// its default, memory, so a Gateway that no stage G materialized still
// gets the stateStore.zones finding; driver redis passes, and so does an
// empty zone map beside memory, which can never label anything.
func TestZonesBesideMemoryWithoutDefaults(t *testing.T) {
	redis, memory := v1alpha1.StateStoreDriverRedis, v1alpha1.StateStoreDriverMemory
	zones := []v1alpha1.StateStoreZone{{Host: "valkey-0.state.internal", Zone: "eu-west-1a"}}
	tests := []struct {
		name string
		ss   *v1alpha1.StateStore
		path string // "" for no finding
	}{
		{"unset driver", &v1alpha1.StateStore{Zones: zones}, "spec.stateStore.zones"},
		{"driver memory", &v1alpha1.StateStore{Driver: &memory, Zones: zones}, "spec.stateStore.zones"},
		{"driver redis", &v1alpha1.StateStore{Driver: &redis, Zones: zones}, ""},
		{"driver memory, empty zone map", &v1alpha1.StateStore{Driver: &memory, Zones: []v1alpha1.StateStoreZone{}}, ""},
		{"driver memory, cache zones", &v1alpha1.StateStore{Driver: &memory, Cache: &v1alpha1.StateStoreCache{Zones: zones}}, ""},
	}
	v := newValidator(t, nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.Gateway{Spec: v1alpha1.GatewaySpec{
				Listeners:  []v1alpha1.Listener{{Name: "http", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 8080}},
				StateStore: tc.ss,
			}}
			b := hub.NewBundle([]*hub.Resource{typedResource(v1alpha1.KindGateway, "edge", gw, 1)})
			_, ds := stageH(t, v, b, nil, 1)
			if tc.path == "" {
				if len(ds) > 0 {
					t.Fatalf("want no finding, got:\n%s", diagText(t, ds))
				}
				return
			}
			if len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Path.String() != tc.path ||
				!strings.Contains(ds[0].Message, "applies to driver redis only") {
				t.Fatalf("want one %s at %s, got:\n%s", CodeSchema, tc.path, diagText(t, ds))
			}
		})
	}
}
