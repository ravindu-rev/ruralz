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

// outHeader is the pooled header of one attempt's outgoing request, the
// copy-free forwarding writer's output: the client's fields after the
// request Phases share the client's value slices, capped, so the copy
// copies no values and an append to a field reallocates instead of
// writing into the client's array; the forwarding fields, the Via entry
// and the empty User-Agent are written into storage it owns. A header is
// reused only once the response of the request that carried it was
// closed, as net/http allows (http.RoundTripper); an attempt that ended
// without a response leaves its header to the collector, since a
// transport may still write it.
type outHeader struct {
	h   http.Header
	fwd clientaddr.Writer
	// via and agent hold the Via line and the empty User-Agent line.
	via   [1]string
	agent [1]string
}

// getOut returns a pooled outgoing header, empty.
func (m *Manager) getOut() *outHeader {
	oh, _ := m.outs.Get().(*outHeader)
	if oh == nil {
		oh = &outHeader{h: make(http.Header, 16)}
	}
	return oh
}

// putOut returns oh to the pool, emptied.
func (m *Manager) putOut(oh *outHeader) {
	clear(oh.h)
	oh.via[0] = ""
	m.outs.Put(oh)
}

// newRequest builds one attempt's outgoing request to Endpoint st on
// rctx, in rec, its header in oh:
//
//   - the leg's method, its normalized path sent as is (RawPath, so
//     percent-encodings such as %2F are kept) and its raw query;
//   - the URL host and Host field are the Endpoint authority (its host
//     name, SRV target or discovery.service, else its IP, with the port);
//   - the client header after the request Phases, copied without copying
//     its values (outHeader), without the hop-by-hop fields and Expect
//     (httpfield.StripHopByHop), then the forwarding fields of
//     clientaddr.Rewrite (overwritten from an untrusted peer, appended
//     behind a trusted proxy) written by oh's clientaddr.Writer, or, for a
//     Response Cache replay (Outbound.Replay), none at all; a client
//     without a User-Agent gets none from net/http either;
//   - the Outbound's Via entry after the Via entries the client sent
//     (docs/architecture/07-multi-protocol.md "HTTP intermediary
//     behavior");
//   - the client body, opened for this attempt; GetBody only for a gated
//     body, so net/http can resend it on a fresh connection; and the
//     client's forwardable trailer fields after the body (withTrailer).
//
// The request lives in rec, so building it allocates nothing beyond the
// record.
func (ls *legState) newRequest(rctx context.Context, rec *attemptRecord, oh *outHeader, st *epState, body io.ReadCloser, cl int64) *http.Request {
	u, x := ls.u, ls.o.X
	n := st.net.Load()
	h := oh.h
	for k, vs := range x.Header() {
		h[k] = vs[:len(vs):len(vs)]
	}
	httpfield.StripHopByHop(h, httpfield.TowardUpstream)
	if ls.o.Replay {
		// A replay has no client hop: the forwarding fields the saved
		// request kept go, and none are added
		// (docs/architecture/03-data-plane.md "Upstream layer").
		oh.fwd.Rewrite(h, clientaddr.Forwarding{})
	} else {
		u.set.fwd.rewrite(&oh.fwd, h, x)
	}
	if v := ls.o.Via; v != "" {
		if vs := h[headerVia]; len(vs) > 0 {
			// The client's values are capped: the append reallocates.
			h[headerVia] = append(vs, v)
		} else {
			oh.via[0] = v
			h[headerVia] = oh.via[:1:1]
		}
	}
	if _, ok := h[headerUserAgent]; !ok {
		h[headerUserAgent] = oh.agent[:1:1]
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
	// WithContext inlines, so its copy lands in rec without a heap
	// request of its own.
	rec.req = *r.WithContext(rctx)
	return &rec.req
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

// rewrite sets the forwarding fields of h for the client of x through w
// (clientaddr.Rewrite): overwritten from an untrusted peer, appended behind
// a trusted proxy.
func (f forwarding) rewrite(w *clientaddr.Writer, h http.Header, x filter.Exchange) {
	peer := x.Source().Peer.Addr()
	w.Rewrite(h, clientaddr.Forwarding{Peer: peer, PeerTrusted: f.trusted.Contains(peer), Scheme: x.Scheme(), Host: x.Host()})
}

// setPath sets the URL path so the request line carries p unchanged
// (routematch.SetRequestPath): p is the escaped form (RawPath) and its
// decoding the Path. An empty path is "/".
func setPath(u *url.URL, p string) { routematch.SetRequestPath(u, p) }
