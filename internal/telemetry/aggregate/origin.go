// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// OriginOf returns the origin label index of a response for
// ruralz_http_listener_requests_total (spec 09 req 44): emit.OriginUpstream
// when the status was passed through from an Upstream response;
// emit.OriginDependency for a Node-generated response with an RZ-UP-*
// code or RZ-AI-004, RZ-AI-005 or RZ-AI-013 (the Upstream or provider
// could not answer); emit.OriginNode for every other Node-generated
// response, RZ-STS codes and responses without a code included.
//
// It depends on nothing but emit, so it can move into emit unchanged:
// the recording site (gateway/handler) may import emit but not this
// package (architecture 1.2).
func OriginOf(code string, fromUpstream bool) int {
	switch {
	case fromUpstream:
		return emit.OriginUpstream
	case strings.HasPrefix(code, "RZ-UP-"):
		return emit.OriginDependency
	}
	switch code {
	case "RZ-AI-004", "RZ-AI-005", "RZ-AI-013":
		return emit.OriginDependency
	default:
		return emit.OriginNode
	}
}
