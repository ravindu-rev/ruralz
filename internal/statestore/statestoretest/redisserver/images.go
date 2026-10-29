// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"fmt"
	"strconv"
	"strings"
)

// The images of the container flavor
// (docs/engineering/01-tech-stack-and-libraries.md "CI tooling"), each
// pinned by the digest of its multi-arch index, its version tag kept
// beside it. Never a module default. A bump changes the tag and the
// digest together.
const (
	// ImageRedis8 is Redis 8, release 8.10.3.
	ImageRedis8 = "redis:8.10.3@sha256:298641b35ac51aa999f9bb85707d6681fe635ce59cda51886a0b6adc1da37b8e"
	// ImageValkey9 is Valkey 9.0, release 9.0.7; Start checks that it
	// runs 9.0.1 or newer.
	ImageValkey9 = "valkey/valkey:9.0.7@sha256:7e1deda01b64ece3167d370ab5be3452d29734feec22cbcbc69b3a64f6859d6d"
	// ImageDragonfly is Dragonfly, release v2.0.2, from the project's own
	// registry.
	ImageDragonfly = "ghcr.io/dragonflydb/dragonfly:v2.0.2@sha256:061f00b201b58e12f974e4e96c761a881ee1c05e9d8ee967466a604b7299753b"
)

// version is a server version: major, minor and patch.
type version [3]int

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// less reports whether v is older than w.
func (v version) less(w version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

// parseVersion parses "major.minor.patch" as INFO server reports it.
func parseVersion(s string) (version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("redisserver: version %q", s)
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, fmt.Errorf("redisserver: version %q", s)
		}
		v[i] = n
	}
	return v, nil
}

// floor returns the INFO server field holding k's version and the oldest
// version k accepts; ok is false when k has no floor.
func (k Kind) floor() (field string, oldest version, ok bool) {
	switch k {
	case Redis8, Redis8Cluster, Redis8Sentinel:
		return "redis_version", version{8, 0, 0}, true
	case Valkey9, Valkey9Cluster, Valkey9Sentinel:
		return "valkey_version", version{9, 0, 1}, true
	default:
		return "", version{}, false
	}
}

// checkVersion checks k's server version, read from INFO server.
func checkVersion(k Kind, info map[string]string) error {
	field, floor, ok := k.floor()
	if !ok {
		return nil
	}
	got, err := parseVersion(info[field])
	if err != nil {
		return fmt.Errorf("%w (INFO server %s)", err, field)
	}
	if got.less(floor) {
		return fmt.Errorf("redisserver: %s runs %s %s, want %s or newer", k, field, got, floor)
	}
	return nil
}
