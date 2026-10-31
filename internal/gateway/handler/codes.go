// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// RZ codes the handler answers (docs/architecture/03-data-plane.md "RZ-RT
// registry").
const (
	// codeNoRoute is 404: no Route matched, CONNECT and OPTIONS * included.
	codeNoRoute = "RZ-RT-001"
	// codeHeaderTooLarge is 431: the request target and headers pass
	// limits.maxRequestHeaderBytes or MaxFieldLines.
	codeHeaderTooLarge = "RZ-RT-002"
	// codeBodyTooLarge is 413: the request body passed
	// limits.maxRequestBodyBytes.
	codeBodyTooLarge = "RZ-RT-003"
	// codeFull is 503: the in-flight ceiling is full.
	codeFull = "RZ-RT-005"
	// codeWhen is 500: a match.when runtime error.
	codeWhen = "RZ-RT-006"
	// codeGrace and codeDrain end a pinned request: 503 before commit.
	codeGrace = "RZ-RT-014"
	codeDrain = "RZ-RT-016"
	// codeRejected is 400: request hardening refused the target or framing.
	codeRejected = "RZ-RT-017"
	// codeWebSocket is 400: a WebSocket upgrade on a Route whose Upstream
	// protocol is http.
	codeWebSocket = "RZ-RT-023"
	// codeFault is 500: an internal fault outside the Filter Chain.
	codeFault = "RZ-RT-025"
	// codeUpstreamTimeout and codeUpstreamReset are a response body that
	// failed while the response gate read it: a deadline ended it, or
	// anything else did.
	codeUpstreamTimeout = "RZ-UP-003"
	codeUpstreamReset   = "RZ-UP-004"
	// codeAfterCommit records an Upstream body that failed after commit;
	// the stream ends with an error and has no status.
	codeAfterCommit = "RZ-UP-009"
)

// upstreamArea is the prefix of the codes of a failed Upstream leg.
const upstreamArea = "RZ-UP-"

// unmatchedRoute is the route label and access-record route of a request no
// Route matched (docs/architecture/10-observability.md "Naming and label
// rules").
const unmatchedRoute = "_unmatched"

// Protocol label values (docs/architecture/10-observability.md "Naming and
// label rules").
const (
	protoHTTP1 = "http1"
	protoHTTP2 = "http2"
)

// statusTable returns the registered status of every code that has a
// single one. The Handler builds it once: errcode.Status builds the
// registry on every call, which the request path must not pay.
func statusTable() map[string]int {
	all := errcode.All()
	m := make(map[string]int, len(all))
	for _, c := range all {
		if c.Status != 0 {
			m[c.ID] = c.Status
		}
	}
	return m
}

// statusOf returns the status a Node-generated response with code carries:
// the registered status, 503 for the end codes and 500 for an internal
// fault, whose registry rows also name what happens after commit, 502 for
// any other Upstream code without a single status and 500 for anything
// else.
func (h *Handler) statusOf(code string) int {
	if s, ok := h.statuses[code]; ok {
		return s
	}
	switch {
	case code == codeGrace, code == codeDrain:
		return http.StatusServiceUnavailable
	case strings.HasPrefix(code, upstreamArea):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// protoIndex returns the protocol label index of r.
func protoIndex(r *http.Request) int {
	if r.ProtoMajor >= 2 {
		return emit.ProtoHTTP2
	}
	return emit.ProtoHTTP1
}

// protoLabel returns the protocol label of r.
func protoLabel(r *http.Request) string {
	if r.ProtoMajor >= 2 {
		return protoHTTP2
	}
	return protoHTTP1
}

// viaEntry returns the Via entry of r's client hop: its protocol version
// and the pseudonym ruralz (docs/architecture/07-multi-protocol.md "HTTP
// intermediary behavior").
func viaEntry(r *http.Request) string {
	switch {
	case r.ProtoMajor >= 2:
		return "2 ruralz"
	case r.ProtoMinor == 0:
		return "1.0 ruralz"
	default:
		return "1.1 ruralz"
	}
}
