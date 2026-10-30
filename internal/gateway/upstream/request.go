// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// headerUserAgent is the User-Agent field name.
const headerUserAgent = "User-Agent"

// newRequest builds one attempt's outgoing request to Endpoint st on
// rctx:
//
//   - the leg's method, its normalized path sent as is (RawPath, so
//     percent-encodings such as %2F are kept) and its raw query;
//   - the URL host and Host field are the Endpoint authority (its host
//     name, SRV target or discovery.service, else its IP, with the port);
//   - a fresh copy of the client header after the request Phases, without
//     the hop-by-hop fields and Expect (httpfield.StripHopByHop), then the
//     forwarding fields of clientaddr.Rewrite (overwritten from an
//     untrusted peer, appended behind a trusted proxy); a client without a
//     User-Agent gets none from net/http either;
//   - the client body, opened for this attempt; GetBody only for a gated
//     body, so net/http can resend it on a fresh connection.
func (ls *legState) newRequest(rctx context.Context, rec *attemptRecord, st *epState, body io.ReadCloser, cl int64) *http.Request {
	u, x := ls.u, ls.o.X
	n := st.net.Load()
	h := x.Header().Clone()
	if h == nil {
		h = make(http.Header, 4)
	}
	httpfield.StripHopByHop(h, httpfield.TowardUpstream)
	u.set.fwd.rewrite(h, x)
	if _, ok := h[headerUserAgent]; !ok {
		h[headerUserAgent] = []string{""}
	}
	target := &rec.url
	*target = url.URL{Scheme: u.scheme, Host: n.authority, RawQuery: ls.query}
	setPath(target, ls.path)
	r := http.Request{
		Method:        ls.method,
		URL:           target,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          body,
		ContentLength: cl,
	}
	if ls.o.Body != nil && ls.o.Body.Buffered() != nil {
		r.GetBody = ls.getBody
	}
	return r.WithContext(rctx)
}

// forwarding writes the forwarding fields of one snapshot's outgoing
// requests from its Gateway.spec.trustedProxies.
type forwarding struct{ trusted *clientaddr.Trusted }

// newForwarding compiles the trusted proxies.
func newForwarding(trusted []netip.Prefix) forwarding {
	return forwarding{trusted: clientaddr.New(trusted)}
}

// rewrite sets the forwarding fields of h for the client of x
// (clientaddr.Rewrite): overwritten from an untrusted peer, appended behind
// a trusted proxy.
func (f forwarding) rewrite(h http.Header, x filter.Exchange) {
	f.trusted.Rewrite(h, x.Source().Peer.Addr(), x.Scheme(), x.Host())
}

// setPath sets the URL path so the request line carries p unchanged
// (routematch.SetRequestPath): p is the escaped form (RawPath) and its
// decoding the Path. An empty path is "/".
func setPath(u *url.URL, p string) { routematch.SetRequestPath(u, p) }
