// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// Field names the outgoing request writes.
const (
	headerUserAgent = "User-Agent"
	headerVia       = "Via"
)

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
//     untrusted peer, appended behind a trusted proxy), or, for a Response
//     Cache replay (Outbound.Replay), none at all; a client without a
//     User-Agent gets none from net/http either;
//   - the Outbound's Via entry after the Via entries the client sent
//     (docs/architecture/07-multi-protocol.md "HTTP intermediary
//     behavior");
//   - the client body, opened for this attempt; GetBody only for a gated
//     body, so net/http can resend it on a fresh connection; and the
//     client's forwardable trailer fields after the body (withTrailer).
func (ls *legState) newRequest(rctx context.Context, rec *attemptRecord, st *epState, body io.ReadCloser, cl int64) *http.Request {
	u, x := ls.u, ls.o.X
	n := st.net.Load()
	h := x.Header().Clone()
	if h == nil {
		h = make(http.Header, 4)
	}
	httpfield.StripHopByHop(h, httpfield.TowardUpstream)
	if ls.o.Replay {
		// A replay has no client hop: the forwarding fields the saved
		// request kept go, and none are added
		// (docs/architecture/03-data-plane.md "Upstream layer").
		clientaddr.Rewrite(h, clientaddr.Forwarding{})
	} else {
		u.set.fwd.rewrite(h, x)
	}
	if v := ls.o.Via; v != "" {
		// Clone capped every value slice, so the append never writes into
		// another field's values.
		h[headerVia] = append(h[headerVia], v)
	}
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
	gated := ls.o.Body != nil && ls.o.Body.Buffered() != nil
	if gated {
		r.GetBody = ls.getBody
	}
	if len(ls.o.Trailer) > 0 && ls.o.Body != nil && body != http.NoBody {
		withTrailer(&r, ls.o.Trailer, gated)
	}
	return r.WithContext(rctx)
}

// withTrailer declares on r the client trailer fields an attempt that sends
// the client body forwards, every one httpfield.IsForwardableTrailer
// admits, and sends their values after the body
// (docs/architecture/07-multi-protocol.md "HTTP intermediary behavior"; no
// Phase reads or edits them). A gated body was read whole, so its values
// are copied now; a streamed body's are copied when the client body
// reaches its end, which is when net/http fills them, on the goroutine
// that reads it. The request goes out without a declared length: HTTP/1.1
// carries a trailer section only in chunked framing.
func withTrailer(r *http.Request, client http.Header, gated bool) {
	var t http.Header
	for k := range client {
		if !httpfield.IsForwardableTrailer(k) {
			continue
		}
		if t == nil {
			t = make(http.Header, len(client))
		}
		t[k] = nil
	}
	if t == nil {
		return
	}
	r.Trailer, r.ContentLength = t, -1
	if gated {
		copyTrailer(t, client)
		return
	}
	r.Body = &trailerBody{ReadCloser: r.Body, client: client, out: t}
}

// copyTrailer copies into out the values client holds for out's names.
func copyTrailer(out, client http.Header) {
	for k := range out {
		if vs := client[k]; len(vs) > 0 {
			out[k] = append([]string(nil), vs...)
		}
	}
}

// trailerBody is a streamed client body whose trailer values reach the
// outgoing request once the body ends.
type trailerBody struct {
	io.ReadCloser
	client, out http.Header
	copied      bool
}

// Read reads the client body; at its end it copies the trailer values.
func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !b.copied {
		b.copied = true
		copyTrailer(b.out, b.client)
	}
	return n, err
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
