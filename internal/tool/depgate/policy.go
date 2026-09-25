// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

// target is one binary built for one platform.
type target struct {
	binary, goos, goarch string
}

// targets lists every shipped binary and platform (ADR-0001).
func targets() []target {
	var out []target
	for _, bin := range []string{"ruralzd", "ruralz-control", "ruralz"} {
		for _, arch := range []string{"amd64", "arm64"} {
			out = append(out, target{bin, "linux", arch})
		}
	}
	for _, p := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}} {
		out = append(out, target{"ruralz", p[0], p[1]})
	}
	return out
}

// allowedLicenses pass G2 without review.
func allowedLicenses() []string {
	return []string{"Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC"}
}

// mplExceptions are the MPL-2.0 modules admitted by name: used unmodified,
// notices kept, patches to MPL-2.0 files published under MPL-2.0. Keyed by
// module path, because version selection may raise versions.
func mplExceptions() []string {
	return []string{
		"github.com/hashicorp/raft",
		"github.com/hashicorp/raft-boltdb/v2",
		"github.com/hashicorp/go-immutable-radix",
		"github.com/hashicorp/golang-lru",
	}
}

// licenseElections map dual-licensed modules to the license Ruralz elects,
// recorded in NOTICE.
func licenseElections() map[string]string {
	return map[string]string{
		"github.com/eclipse/paho.golang": "EDL-1.0",
	}
}

// licenseOverrides map modules whose license text the classifier cannot
// read to a reviewed SPDX identifier.
func licenseOverrides() map[string]string {
	return map[string]string{}
}

// cryptoDelegation lists golang.org/x/crypto packages that delegate to the
// Go Cryptographic Module in FIPS mode. No research row names one yet, and
// nothing links x/crypto in M0, so the list is empty.
func cryptoDelegation() []string {
	return nil
}

// cryptoExceptions are G3 exception rows by module path.
func cryptoExceptions() map[string]string {
	return map[string]string{
		"github.com/quic-go/quic-go": "accepted outside FIPS builds",
	}
}

// raftModules must never be linked into ruralzd.
func raftModules() []string {
	return []string{"github.com/hashicorp/raft"}
}

// advisoryFloors are minimum versions forced by security advisories.
func advisoryFloors() map[string]string {
	return map[string]string{
		"github.com/gorilla/websocket":        "v1.5.3",
		"oras.land/oras-go/v2":                "v2.6.2",
		"github.com/lestrrat-go/jwx/v4":       "v4.5.0",
		"github.com/hashicorp/raft-boltdb/v2": "v2.4.2",
	}
}
