// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"regexp"
	"strings"
	"testing"
)

// TestParseVersion checks that versions are exactly three numbers.
func TestParseVersion(t *testing.T) {
	for in, want := range map[string]version{"9.0.1": {9, 0, 1}, "8.10.12": {8, 10, 12}, "0.0.0": {0, 0, 0}} {
		if got, err := parseVersion(in); err != nil || got != want {
			t.Errorf("parseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "9", "9.0", "9.0.1.2", "v9.0.1", "9.x.1", "9.-1.0", "df-v1.2.3"} {
		if _, err := parseVersion(in); err == nil {
			t.Errorf("parseVersion(%q): want an error", in)
		}
	}
}

// TestCheckVersion checks the version floors of the container flavor:
// Valkey 9.0.1 or newer (tech stack), Redis 8, none for Dragonfly or the
// process flavor.
func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		info map[string]string
		ok   bool
	}{
		{Valkey9, map[string]string{"valkey_version": "9.0.1", "redis_version": "7.2.4"}, true},
		{Valkey9Cluster, map[string]string{"valkey_version": "9.1.0"}, true},
		{Valkey9, map[string]string{"valkey_version": "10.0.0"}, true},
		{Valkey9, map[string]string{"valkey_version": "9.0.0", "redis_version": "7.2.4"}, false},
		{Valkey9, map[string]string{"redis_version": "9.0.1"}, false},
		{Redis8, map[string]string{"redis_version": "8.0.0"}, true},
		{Redis8Cluster, map[string]string{"redis_version": "8.4.6"}, true},
		{Redis8, map[string]string{"redis_version": "7.4.2"}, false},
		{Dragonfly, map[string]string{"redis_version": "7.4.0", "dragonfly_version": "df-v1.2.3"}, true},
		{Process, map[string]string{"redis_version": "7.0.15"}, true},
		{ProcessCluster, nil, true},
	} {
		err := checkVersion(tc.k, tc.info)
		if (err == nil) != tc.ok {
			t.Errorf("checkVersion(%s, %v) = %v, want ok %v", tc.k, tc.info, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "version") {
			t.Errorf("checkVersion(%s): %v does not name the version", tc.k, err)
		}
	}
}

// TestImagesPinnedByDigest checks that every container image is pinned
// by the digest of its index, with its version tag beside it, and that
// the tagged version is of the kind's major line and meets its floor.
func TestImagesPinnedByDigest(t *testing.T) {
	pinned := regexp.MustCompile(`^[a-z0-9][a-z0-9./-]*:v?([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}$`)
	for _, k := range FlavorContainer.Kinds() {
		m := pinned.FindStringSubmatch(k.Image())
		if m == nil {
			t.Errorf("%s: image %q, want image:version@sha256:<index digest>", k, k.Image())
			continue
		}
		v, err := parseVersion(m[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, floor, ok := k.floor(); ok && (v[0] != floor[0] || v.less(floor)) {
			t.Errorf("%s: image %q is version %s, want %d.x at %s or newer", k, k.Image(), v, floor[0], floor)
		}
	}
}
