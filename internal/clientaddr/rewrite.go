// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// Forwarding describes the client connection whose request is forwarded.
type Forwarding struct {
	// Peer is the connection peer after PROXY v2 handling (Result.Peer's
	// address).
	Peer netip.Addr
	// PeerTrusted is true when Peer is inside Gateway.spec.trustedProxies
	// (Result.PeerTrusted).
	PeerTrusted bool
	// Scheme is the listener scheme, http or https.
	Scheme string
	// Host is the client request authority (Request.Host), which the
	// Upstream receives in X-Forwarded-Host because Host carries the
	// Endpoint authority (R-28).
	Host string
}

// Rewrite sets the forwarding fields of h, the header of an outgoing
// Upstream request, from f (04 req 18, 06 req 63):
//
//   - Untrusted peer: X-Forwarded-For is overwritten with the peer address,
//     X-Forwarded-Proto with the scheme, X-Forwarded-Host with the host, and
//     Forwarded is removed; a field whose value would be empty (an invalid
//     peer, an empty scheme or host) is removed instead.
//   - Trusted peer: the X-Forwarded-For lines are joined and ", <peer>" is
//     appended (just "<peer>" when there were none); X-Forwarded-Proto,
//     X-Forwarded-Host and Forwarded are kept, and the first two are set
//     from f when absent.
//
// Addresses are written unmapped and without zone, IPv6 in RFC 5952 form
// without brackets. Case variants of the four field names are folded into
// the canonical name first.
//
// Preconditions the caller (the outgoing request build) must meet:
//
//   - h is non-nil; Rewrite panics on a nil header, which it could not
//     write the fields into (http.Header.Clone returns nil for a nil
//     header, so clone a non-nil one or start from an empty map).
//   - h is a fresh copy of the client request header, rewritten once per
//     attempt: Rewrite is not idempotent from a trusted peer, so a second
//     call on the same header (a retry reusing it) appends the peer
//     again.
//   - The hop-by-hop fields and the fields Connection lists were removed
//     before: removal after Rewrite would let a client's
//     "Connection: X-Forwarded-For" strip the field Rewrite set.
func Rewrite(h http.Header, f Forwarding) {
	if h == nil {
		panic("clientaddr: Rewrite called with a nil http.Header")
	}
	foldVariants(h)
	peer := ""
	if f.Peer.IsValid() {
		peer = f.Peer.Unmap().WithZone("").String()
	}
	if !f.PeerTrusted || peer == "" {
		setOrDelete(h, headerXForwardedFor, peer)
		setOrDelete(h, headerXForwardedProto, f.Scheme)
		setOrDelete(h, headerXForwardedHost, f.Host)
		delete(h, headerForwarded)
		return
	}
	prior := joinLines(h[headerXForwardedFor])
	if prior == "" {
		h[headerXForwardedFor] = []string{peer}
	} else {
		h[headerXForwardedFor] = []string{prior + ", " + peer}
	}
	if len(h[headerXForwardedProto]) == 0 {
		setOrDelete(h, headerXForwardedProto, f.Scheme)
	}
	if len(h[headerXForwardedHost]) == 0 {
		setOrDelete(h, headerXForwardedHost, f.Host)
	}
}

// Rewrite applies the package-level Rewrite, with its preconditions, with
// the trust of peer decided by t.
func (t *Trusted) Rewrite(h http.Header, peer netip.Addr, scheme, host string) {
	Rewrite(h, Forwarding{Peer: peer, PeerTrusted: t.Contains(peer), Scheme: scheme, Host: host})
}

// setOrDelete sets one field line, or removes the field for "".
func setOrDelete(h http.Header, name, v string) {
	if v == "" {
		delete(h, name)
		return
	}
	h[name] = []string{v}
}

// joinLines joins field lines with ", " dropping empty ones.
func joinLines(lines []string) string {
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return trimOWS(lines[0])
	}
	var b strings.Builder
	for _, l := range lines {
		l = trimOWS(l)
		if l == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(l)
	}
	return b.String()
}

// foldVariants moves the lines of non-canonical spellings of the
// forwarding fields (keys set directly in the map) under the canonical
// key, after its own lines and in byte order of the spellings, so no
// spelling escapes the rule and the result never depends on map order.
// It allocates only when such a spelling exists.
func foldVariants(h http.Header) {
	var variants []string
	for k := range h {
		if canonicalForwardingName(k) != "" {
			variants = append(variants, k)
		}
	}
	if len(variants) == 0 {
		return
	}
	slices.Sort(variants)
	for _, k := range variants {
		name := canonicalForwardingName(k)
		// Clip so the append never writes into a backing array another
		// header shares.
		h[name] = append(slices.Clip(h[name]), h[k]...)
		delete(h, k)
	}
}

// canonicalForwardingName returns the canonical forwarding field name k is
// a non-canonical spelling of, else "".
func canonicalForwardingName(k string) string {
	for _, name := range [...]string{headerXForwardedFor, headerXForwardedProto, headerXForwardedHost, headerForwarded} {
		if k != name && len(k) == len(name) && strings.EqualFold(k, name) {
			return name
		}
	}
	return ""
}
