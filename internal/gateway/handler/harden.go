// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// fieldLineOverhead is what RFC 9113 section 6.5.2 adds to a field line's
// name and value in the field-list size.
const fieldLineOverhead = 32

// harden applies request hardening before routing
// (docs/architecture/03-data-plane.md "Listeners and protocols";
// docs/architecture/07-multi-protocol.md "HTTP intermediary behavior";
// docs/architecture/08-security-and-identity.md "Request hardening") and
// returns the code of its refusal, "" when the request passes, in this
// order: CONNECT (404 RZ-RT-001, which the path alone cannot tell apart),
// the header block over limits.maxRequestHeaderBytes or MaxFieldLines (431
// RZ-RT-002), conflicting framing, an absolute-form target with another
// scheme than the listener's, an HTTP/2 Host naming another host than
// :authority (400 RZ-RT-017), then the path's own refusal (RZ-RT-017, or
// RZ-RT-001 for OPTIONS *).
func (st *state) harden() string {
	r := st.r
	if r.Method == http.MethodConnect {
		return codeNoRoute
	}
	limit := st.snap.Gateway.Limits.MaxRequestHeaderBytes
	if limit <= 0 || limit > listener.MaxHeaderBytes {
		limit = listener.MaxHeaderBytes
	}
	if size, lines := fieldListSize(r, st.scheme); size > limit || lines > MaxFieldLines {
		return codeHeaderTooLarge
	}
	if conflictingFraming(r) {
		return codeRejected
	}
	if s := r.URL.Scheme; s != "" && !strings.EqualFold(s, st.scheme) {
		return codeRejected
	}
	if r.ProtoMajor >= 2 {
		for _, h := range r.Header["Host"] {
			if !sameAuthority(h, r.Host, st.scheme) {
				return codeRejected
			}
		}
	}
	return st.pathCode
}

// fieldListSize returns the size request hardening counts for r, the same
// for every protocol: RFC 9113's field-list size, each field line's name
// and value plus 32 bytes, with :method, :scheme, :authority and :path
// counted as four field lines, also for HTTP/1.1, so one number bounds the
// request target with the fields; and the field lines net/http parsed,
// pseudo-header fields excluded and a joined Cookie as one.
func fieldListSize(r *http.Request, scheme string) (size int64, lines int) {
	target := len(routematch.RequestPath(r.URL))
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		target += 1 + len(r.URL.RawQuery)
	}
	size = int64(len(":method")+len(r.Method)+len(":scheme")+len(scheme)+
		len(":authority")+len(r.Host)+len(":path")+target) + 4*fieldLineOverhead
	for k, vs := range r.Header {
		for _, v := range vs {
			size += int64(len(k) + len(v) + fieldLineOverhead)
			lines++
		}
	}
	return size, lines
}

// conflictingFraming reports framing that request hardening refuses and
// net/http let through: a Transfer-Encoding with any coding other than
// exactly chunked, or one beside a Content-Length.
func conflictingFraming(r *http.Request) bool {
	te := r.TransferEncoding
	if len(te) == 0 {
		return len(r.Header["Transfer-Encoding"]) > 0
	}
	if len(te) > 1 || !strings.EqualFold(te[0], "chunked") {
		return true
	}
	_, cl := r.Header["Content-Length"]
	return cl
}

// sameAuthority reports whether two authorities name the same host:
// compared lowercased and without the scheme's default port.
func sameAuthority(a, b, scheme string) bool {
	return strings.EqualFold(withoutDefaultPort(a, scheme), withoutDefaultPort(b, scheme))
}

// withoutDefaultPort drops the scheme's default port from an authority.
func withoutDefaultPort(a, scheme string) string {
	port := ":80"
	if scheme == "https" {
		port = ":443"
	}
	if strings.HasSuffix(a, port) {
		return a[:len(a)-len(port)]
	}
	return a
}
