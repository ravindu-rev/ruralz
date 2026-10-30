// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

// target is one binary built for one platform.
type target struct {
	binary, goos, goarch string
}

// name is the artifact name "<binary>_<os>_<arch>", which is also the
// directory of the target's notices.
func (t target) name() string { return t.binary + "_" + t.goos + "_" + t.goarch }

// targets lists every binary on every platform it ships for
// (docs/engineering/01-tech-stack-and-libraries.md, "Static builds"): the
// servers on linux/amd64 and linux/arm64 (production) and darwin/arm64 and
// darwin/amd64 (development only, still released as archives), the CLI on
// those plus windows/amd64.
func targets() []target {
	server := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}, {"darwin", "amd64"}}
	cli := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}}
	var out []target
	for _, bin := range []string{"ruralzd", "ruralz-control"} {
		for _, p := range server {
			out = append(out, target{bin, p[0], p[1]})
		}
	}
	for _, p := range cli {
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
// read to a reviewed SPDX identifier. A key is a module path, for the
// texts at the module root, or "<module path>/<directory>" for the texts
// of a nested directory that governs some linked packages.
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

// raftModules must never be linked into ruralzd (P2, ADR-0006).
func raftModules() []string {
	return []string{"github.com/hashicorp/raft"}
}

// jwxModules must never be linked into ruralz: the offline Policy checks
// every binary links through checks.Table() import no module cmd/ruralz
// does not link, and the auth.jwt Filter, whose check lives in a package
// of its own for that reason, links jwx
// (docs/engineering/01-tech-stack-and-libraries.md, "Dependency graph").
func jwxModules() []string {
	return []string{"github.com/lestrrat-go/jwx"}
}

// advisoryFloors are minimum versions forced by security advisories.
func advisoryFloors() map[string]string {
	return map[string]string{
		"github.com/gorilla/websocket":        "v1.5.3",
		"oras.land/oras-go/v2":                "v2.6.2",
		"github.com/lestrrat-go/jwx/v4":       "v4.5.0",
		"github.com/hashicorp/raft-boltdb/v2": "v2.4.2",
		"github.com/moby/go-archive":          "v0.3.0",
	}
}

// testSupportRoots are the trees of this module that no binary may link
// (docs/engineering/02-repository-layout-and-conventions.md, "Package
// layers"): test support, the CI tools and the suites under test/, as
// paths below the module path. Packages named "<parent>test" below their
// parent, such as internal/clock/clocktest, are test support too.
func testSupportRoots() []string {
	return []string{"internal/testkit", "internal/tool", "test"}
}

// testOnlyStandard are standard library test support packages; "testing"
// covers its subpackages too.
func testOnlyStandard() []string {
	return []string{"testing", "net/http/httptest"}
}

// testOnlyModules are third-party modules admitted for tests only (the
// Library catalog row "Test tooling (not shipped)"); a path covers the
// modules below it.
func testOnlyModules() []string {
	return []string{"github.com/testcontainers", "go.yaml.in/yaml/v3"}
}
