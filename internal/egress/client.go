// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultMaxBodyBytes caps a response body when ClientOptions gives no cap
// (1 MiB, the JWKS cap of spec 06 requirement 37).
const DefaultMaxBodyBytes = 1 << 20

// MaxResponseHeaderBytes caps the response header block of a guarded
// fetch.
const MaxResponseHeaderBytes = 64 << 10

// Guarded client errors; each surfaces wrapped in a *url.Error from
// http.Client.Do, so callers match with errors.Is.
var (
	// ErrRedirect reports a 3xx response to a client that follows no
	// redirect (a destination-bound client or MaxRedirects 0 or less):
	// every 3xx status fails, also one without a Location header (300,
	// 304) and a 307 or 308 that http.Client would hand back unfollowed
	// (a request body without GetBody). The response is closed and not
	// returned. A client that follows redirects returns such responses
	// with a nil error, so its callers check the status as for any
	// non-200 response.
	ErrRedirect = errors.New("egress: redirect not followed")
	// ErrTooManyRedirects reports more redirects than MaxRedirects.
	ErrTooManyRedirects = errors.New("egress: too many redirects")
	// ErrScheme reports a request or redirect to a non-https URL; the
	// client never follows https to http.
	ErrScheme = errors.New("egress: only https destinations are fetched")
	// ErrDestination reports a request of a destination-bound client to an
	// origin other than its destination.
	ErrDestination = errors.New("egress: request to an origin other than the secret's destination")
	// ErrBodyTooLarge reports a response body above the cap.
	ErrBodyTooLarge = errors.New("egress: response body exceeds the limit")
)

// ClientOptions configures a guarded HTTP client.
type ClientOptions struct {
	// TLS is the client TLS configuration, normally
	// tlsconf.IdentityProvider() (TLS 1.2 or newer, system roots, spec 06
	// requirement 82). It is cloned; nil gives TLS 1.2 or newer with the
	// system roots. Verification is never disabled: InsecureSkipVerify is
	// forced off and MinVersion raised to TLS 1.2.
	TLS *tls.Config
	// MaxRedirects bounds followed https-to-https redirects: 3 for JWKS
	// (spec 06 requirement 37); 0 or negative never follows, and every 3xx
	// response fails with ErrRedirect (tokenUrl, spec 06 requirement 69).
	MaxRedirects int
	// Destination, when set, binds the client to one origin
	// ("https://host:port", see Origin and hub.SecretUse.Destination): the
	// client follows no redirect at all whatever MaxRedirects says (every
	// 3xx response fails with ErrRedirect), and a request to any other
	// origin fails with ErrDestination before a connection is made (R-49).
	Destination string
	// Timeout bounds one fetch, redirects and body read included (5 s for
	// JWKS); 0 leaves the bound to the request context.
	Timeout time.Duration
	// DialTimeout bounds each connect; 0 uses Timeout when it is shorter
	// than DefaultDialTimeout, else DefaultDialTimeout.
	DialTimeout time.Duration
	// MaxBodyBytes caps each response body after content decoding (1 MiB
	// for JWKS, 64 KiB for token responses); 0 or negative uses
	// DefaultMaxBodyBytes. Reading past the cap fails with ErrBodyTooLarge.
	MaxBodyBytes int64
}

// Client returns an HTTP client for IdP fetches (jwksUrl, tokenUrl) whose
// every connection passes the guard (spec 06 requirements 84 to 86):
//
//   - connections go through Dialer, so each connect, redirect hops and
//     proxy connections included, is checked after DNS resolution;
//   - only https URLs are requested, and https to http is never followed;
//   - redirects follow ClientOptions.MaxRedirects, and a hop to a denied
//     IP literal fails before dialing; a destination-bound client or one
//     with MaxRedirects 0 fails every 3xx response with ErrRedirect;
//   - no cookie jar, so no cookies are stored or sent;
//   - proxy variables are ignored unless RURALZ_FETCH_ALLOW holds
//     env-proxy;
//   - response headers are capped at MaxResponseHeaderBytes and bodies at
//     ClientOptions.MaxBodyBytes.
//
// The owner calls CloseIdleConnections when it stops using the client.
func (g *Guard) Client(o ClientOptions) *http.Client {
	maxBody := o.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	dialTimeout := o.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
		if o.Timeout > 0 && o.Timeout < dialTimeout {
			dialTimeout = o.Timeout
		}
	}
	tr := &http.Transport{
		DialContext:            g.Dialer(dialTimeout).DialContext,
		TLSClientConfig:        clientTLS(o.TLS),
		TLSHandshakeTimeout:    dialTimeout,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: MaxResponseHeaderBytes,
	}
	if g.EnvProxy() && g.proxy != nil {
		tr.Proxy = g.proxy
	}
	dest := o.Destination
	maxRedirects := o.MaxRedirects
	if dest != "" {
		maxRedirects = 0
		// Normalize the pinned origin; an unparsable one matches no
		// request, so the client fails closed.
		if u, err := url.Parse(dest); err == nil && u.Host != "" {
			dest = Origin(u)
		}
	}
	return &http.Client{
		Transport: &transport{guard: g, base: tr, destination: dest, maxBody: maxBody, noRedirect: maxRedirects <= 0},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return g.checkRedirect(req, via, maxRedirects)
		},
		Timeout: o.Timeout,
	}
}

// clientTLS clones c (or starts from an empty configuration) and enforces
// TLS 1.2 or newer with verification on.
func clientTLS(c *tls.Config) *tls.Config {
	var out *tls.Config
	if c == nil {
		out = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		out = c.Clone()
	}
	if out.MinVersion < tls.VersionTLS12 {
		out.MinVersion = tls.VersionTLS12
	}
	out.InsecureSkipVerify = false
	return out
}

// checkRedirect is the CheckRedirect policy: req is the next hop, via the
// requests made so far. For a client that follows no redirect the
// transport already refuses every 3xx; the first branch keeps the policy
// closed on its own.
func (g *Guard) checkRedirect(req *http.Request, via []*http.Request, maxRedirects int) error {
	if maxRedirects <= 0 {
		status := 0
		if req.Response != nil {
			status = req.Response.StatusCode
		}
		return fmt.Errorf("%w: status %d", ErrRedirect, status)
	}
	if len(via) > maxRedirects {
		return fmt.Errorf("%w: more than %d", ErrTooManyRedirects, maxRedirects)
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("%w: redirect to %s", ErrScheme, req.URL.Scheme)
	}
	return g.CheckHost(req.URL.Hostname())
}

// transport applies the per-request checks and the body cap around the
// guarded http.Transport.
type transport struct {
	guard       *Guard
	base        *http.Transport
	destination string
	maxBody     int64
	noRedirect  bool // every 3xx fails with ErrRedirect
}

// RoundTrip refuses non-https URLs, other origins of a destination-bound
// client and denied IP literals before any connection, refuses every 3xx
// response of a client that follows no redirect (http.Client consults
// CheckRedirect only for the redirects it would follow), then caps the
// response body.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, "https") {
		closeRequestBody(req)
		return nil, fmt.Errorf("%w: %s", ErrScheme, req.URL.Scheme)
	}
	if t.destination != "" && Origin(req.URL) != t.destination {
		closeRequestBody(req)
		return nil, fmt.Errorf("%w: %s", ErrDestination, Origin(req.URL))
	}
	if err := t.guard.CheckHost(req.URL.Hostname()); err != nil {
		closeRequestBody(req)
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.noRedirect && resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", ErrRedirect, resp.StatusCode)
	}
	if resp.ContentLength > t.maxBody {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: Content-Length %d above %d bytes", ErrBodyTooLarge, resp.ContentLength, t.maxBody)
	}
	resp.Body = &cappedBody{rc: resp.Body, left: t.maxBody}
	return resp, nil
}

// CloseIdleConnections closes the idle pooled connections; http.Client
// forwards to it.
func (t *transport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// closeRequestBody closes a request body a RoundTrip refuses, as the
// http.RoundTripper contract requires.
func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// cappedBody returns at most left bytes and then ErrBodyTooLarge when the
// body holds more, instead of truncating silently.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
	err  error
}

// Read implements io.Reader.
func (b *cappedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.rc.Read(p)
	if int64(n) > b.left {
		n = int(b.left)
		b.left = 0
		b.err = ErrBodyTooLarge
		return n, b.err
	}
	b.left -= int64(n)
	return n, err
}

// Close implements io.Closer.
func (b *cappedBody) Close() error { return b.rc.Close() }
