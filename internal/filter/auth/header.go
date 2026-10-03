// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"strings"
)

// Single reads a credential field: value is its first field line and lines
// the number of field lines (spec 06 requirements 14, 24 and 41: absent is
// RZ-AUTH-001, more than one line RZ-AUTH-002). canonical is the name in
// http.CanonicalHeaderKey form, computed once at build time. net/http
// stores parsed fields under canonical keys; a key an earlier Filter added
// in another case is counted too. It allocates nothing.
func Single(h http.Header, canonical string) (value string, lines int) {
	vs := h[canonical]
	if len(vs) > 0 {
		value = vs[0]
	}
	lines = len(vs)
	for k, o := range h {
		if len(k) == len(canonical) && k != canonical && strings.EqualFold(k, canonical) {
			if lines == 0 && len(o) > 0 {
				value = o[0]
			}
			lines += len(o)
		}
	}
	return value, lines
}

// Strip deletes every field line of the credential field canonical, in any
// case, from the request forwarded upstream (spec 06 requirement 9:
// auth.api-key strips its header and auth.basic Authorization).
func Strip(h http.Header, canonical string) {
	delete(h, canonical)
	for k := range h {
		if len(k) == len(canonical) && strings.EqualFold(k, canonical) {
			delete(h, k)
		}
	}
}
