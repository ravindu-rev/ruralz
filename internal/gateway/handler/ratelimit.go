// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/sfv"
)

// rateLimitFields writes the draft-ietf-httpapi-ratelimit-headers-11
// fields of the limits the ratelimit and quota Policies recorded
// (filter.Exchange.AddRateLimitField) on the response with status, after
// onResponse, generated responses included
// (docs/architecture/09-traffic-management-and-resilience.md "RateLimit
// response headers"): RateLimit-Policy lists every applied limit in chain
// order with q and w; RateLimit lists each limit whose count is known
// with r and t, but when a limit denied the request only the denied one,
// and on a 429 no other. A limit a Policy with responseHeaders none
// applied is never recorded; Retry-After comes with the denying Policy's
// 429. The fields replace any an Upstream or a Policy set, so a response
// states this Node's limits only; without a recorded limit nothing
// changes.
func (st *state) rateLimitFields(hdr http.Header, status int) {
	if st.x == nil {
		return
	}
	fields := st.x.RateLimitFields()
	if len(fields) == 0 {
		return
	}
	var buf [512]byte
	pol := buf[:0]
	for _, f := range fields {
		pol, _ = sfv.AppendRateLimitPolicy(pol, f.Name, f.Q, f.W)
	}
	if len(pol) > 0 {
		hdr[sfv.HeaderRateLimitPolicy] = []string{string(pol)}
	}
	denied := deniedLimit(fields)
	cur := buf[:0]
	for _, f := range fields {
		if !f.Known || (denied || status == http.StatusTooManyRequests) && !f.Denied {
			continue
		}
		cur, _ = sfv.AppendRateLimit(cur, f.Name, f.R, f.T)
	}
	if len(cur) > 0 {
		hdr[sfv.HeaderRateLimit] = []string{string(cur)}
	} else {
		delete(hdr, sfv.HeaderRateLimit)
	}
}

// deniedLimit reports whether a recorded limit denied the request.
func deniedLimit(fields []filter.RateLimitField) bool {
	for i := range fields {
		if fields[i].Denied {
			return true
		}
	}
	return false
}
