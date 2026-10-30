// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// checkSkew warns once on Stderr when the Node's ruralz-version, carried
// by its authenticated responses, makes the CLI two or more minor releases
// newer, outside the version skew policy
// (docs/engineering/04-release-versioning-and-compatibility.md "Control
// plane and data plane version skew"). A response without the header, or
// with a version that does not parse, is not checked.
func (c *Client) checkSkew(h http.Header) {
	node := h.Get(HeaderVersion)
	if node == "" || c.warned.Load() || !outsideSkew(c.version, node) {
		return
	}
	if !c.warned.CompareAndSwap(false, true) || c.stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(c.stderr,
		"warning: ruralz %s is two or more minor releases newer than the Node at %s (ruralz-version %s), outside the version skew policy\n",
		clean(c.version), c.base, clean(node))
}

// outsideSkew reports whether the CLI version cli is provably two or more
// minor releases newer than the Node version node, counting each
// (major, minor) pair as a release. Within one major the minors are
// compared. A CLI one major ahead is at least its own minor plus one
// releases newer, the releases MAJOR.0 to MAJOR.MINOR, so it is outside
// from MAJOR.1; two majors ahead always is. A Node at the same or a newer
// version, or a version that does not parse, is not outside.
func outsideSkew(cli, node string) bool {
	cmaj, cmin, ok := majorMinor(cli)
	if !ok {
		return false
	}
	nmaj, nmin, ok := majorMinor(node)
	if !ok {
		return false
	}
	switch {
	case cmaj == nmaj:
		return cmin-nmin >= 2
	case cmaj == nmaj+1:
		return cmin >= 1
	default:
		return cmaj > nmaj+1
	}
}

// majorMinor parses the major and minor of a SemVer 2.0.0 version,
// MAJOR.MINOR.PATCH with an optional pre-release and build suffix, as the
// product version is written without a leading "v".
func majorMinor(v string) (major, minor int, ok bool) {
	core, _, _ := strings.Cut(v, "+")
	core, _, _ = strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return 0, 0, false
		}
		var err error
		if n[i], err = strconv.Atoi(p); err != nil {
			return 0, 0, false
		}
	}
	return n[0], n[1], true
}
