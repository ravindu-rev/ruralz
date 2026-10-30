// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package adminclient is the ruralz CLI's client of a Node's admin API on
// Gateway.spec.admin.port, default 9901, shared by every command that
// calls it: ruralz node dump and the admin sides of ruralz bundle diff
// read /config/dump, ruralz dev tap streams /tap, and the local ruralzd
// launcher polls /readyz (docs/reference/01-cli-and-api-surface.md
// "Admin API conventions" and "Admin API"; the endpoints are
// docs/architecture/03-data-plane.md "Admin endpoints").
//
// # Addressing
//
// A base URL, from --admin (else RURALZ_ADMIN_URL, else
// http://127.0.0.1:9901) or an admin FROM or TO of ruralz bundle diff, is
// an http or https URL with a host. The port defaults by scheme; userinfo,
// a query or a fragment is a usage error (exit 2). A path prefix is kept,
// for a reverse proxy, and admin paths are appended to it.
//
// # Authentication
//
// The operator token of --admin-token-file (else RURALZ_ADMIN_TOKEN_FILE)
// goes as a bearer token, and --client-cert with --client-key is presented
// for admin mTLS. A token file follows the Node's rule
// (adminauth.ReadToken); it is read before any request, which an invalid
// file refuses with exit 2, and again before each call, so ruralz dev tap
// reads it again each time it subscribes. The client never sends a token
// in cleartext beyond loopback: holding one over http, it tests the
// connected peer address by the Node's rule (adminauth.Loopback) and
// refuses any other peer before sending a byte. The client certificate and
// key come together, only with https, as unencrypted PEM; anything else is
// a usage error. Every https Node is verified, against the system roots
// plus the certificates of --ca-file, under the Node's client TLS policy
// (tlsconf.Client).
//
// # Deadlines
//
// 10 s to connect and finish TLS, 30 s from the established connection to
// the response headers, and 5 min for a whole call that does not stream,
// which --timeout replaces, 0 meaning none; a /tap stream has no body
// deadline. Every deadline runs on the injected clock. An expired deadline
// exits 2, and canceling the context, as Ctrl-C does, ends any wait.
//
// # Transport
//
// Direct connections only: proxy variables are ignored, so the loopback
// test sees the real peer and no proxy sees a token. Each call speaks
// HTTP/1.1 over a fresh connection, so nothing retries a request; no
// redirect is followed, a 3xx exiting 2 with its Location.
//
// # Bounds
//
// At most 256 MiB of /config/dump, 64 KiB of a problem document or a
// /readyz body, and 1 MiB per /tap line, the Node's per-subscriber buffer,
// which a line enters whole with its newline; a larger body or line exits
// 2.
//
// # Identity
//
// Each request carries User-Agent: ruralz/<version> and a fresh
// traceparent that is not sampled, so the requestId of a problem document
// names the CLI's request.
//
// # Errors and exit codes
//
// Every error exits 2, through command.NoResult or a usage error, except
// that a context canceled by a signal ends the command by its cause
// (command.Code). A problem document is a ProblemError, printed as one line
// "<status> <code> <title>[: <detail>] (requestId <id>)"; with --output
// json, command.Execute also writes the document to standard output as
// received.
//
// # Version skew
//
// A Node's authenticated responses carry ruralz-version, its build
// version; when the CLI is two or more minor releases newer, outside the
// version skew policy, the client warns once on standard error.
package adminclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Admin paths the CLI calls (docs/architecture/03-data-plane.md "Admin
// endpoints").
const (
	// PathReadyz is the readiness endpoint, open on every Node.
	PathReadyz = "/readyz"
	// PathConfigDump is the active Revision in ruralz.canonical.v1 form.
	PathConfigDump = "/config/dump"
	// PathTap streams sampled request and response metadata.
	PathTap = "/tap"
)

// DefaultURL is the admin base URL without --admin and RURALZ_ADMIN_URL.
const DefaultURL = "http://127.0.0.1:9901"

// Environment variables that stand in for an absent flag; a flag always
// wins (docs/_meta/foundation-pack.md section 2).
const (
	// EnvURL stands in for --admin; the CLI reads it, a Node never does.
	EnvURL = "RURALZ_ADMIN_URL"
	// EnvTokenFile stands in for --admin-token-file; on a Node it names
	// the operator token file.
	EnvTokenFile = adminauth.EnvTokenFile
)

// Deadlines of every call (targets).
const (
	// ConnectTimeout bounds connecting and finishing TLS.
	ConnectTimeout = 10 * time.Second
	// HeaderTimeout bounds the wait from the established connection to
	// the response headers, of a stream too.
	HeaderTimeout = 30 * time.Second
	// DefaultTimeout bounds a whole call that does not stream unless
	// --timeout replaces it.
	DefaultTimeout = 5 * time.Minute
)

// Bounds of what the client reads (targets); more exits 2.
const (
	// MaxConfigDumpBytes bounds a /config/dump body.
	MaxConfigDumpBytes = 256 << 20
	// MaxProblemBytes bounds a problem document.
	MaxProblemBytes = 64 << 10
	// MaxReadyzBytes bounds a /readyz body.
	MaxReadyzBytes = 64 << 10
	// MaxTapLineBytes bounds a /tap line, its newline included: the size
	// of the Node's per-subscriber buffer, which a line enters whole.
	MaxTapLineBytes = 1 << 20
)

// HeaderVersion is the response header that carries a Node's build
// version on its authenticated admin responses.
const HeaderVersion = "Ruralz-Version"

// Config addresses and authenticates one Node.
type Config struct {
	// URL is the admin base URL.
	URL string
	// TokenFile names the operator token file; empty sends no token.
	TokenFile string
	// CAFile names a PEM file whose certificates join the system roots
	// for an https Node; empty adds none, and an http Node ignores it.
	CAFile string
	// ClientCert and ClientKey name the PEM client certificate chain and
	// its unencrypted key for admin mTLS, both or neither, https only.
	ClientCert string
	// ClientKey names the key of ClientCert.
	ClientKey string
	// Timeout bounds each whole call that does not stream; 0 means none,
	// as --timeout 0 does, so callers without that flag pass
	// DefaultTimeout.
	Timeout time.Duration
	// Names are where the values came from, for messages.
	Names Names
}

// Names are the flags, arguments or environment variables a Config's
// values came from, used in messages; an empty name is the value's flag:
// --admin, --admin-token-file, --ca-file, --client-cert, --client-key or
// --timeout.
type Names struct {
	URL, TokenFile, CAFile, ClientCert, ClientKey, Timeout string
}

// withDefaults fills the empty names with the default flags.
func (n Names) withDefaults() Names {
	for _, f := range []struct {
		p   *string
		def string
	}{
		{&n.URL, "--admin"},
		{&n.TokenFile, "--admin-token-file"},
		{&n.CAFile, "--ca-file"},
		{&n.ClientCert, "--client-cert"},
		{&n.ClientKey, "--client-key"},
		{&n.Timeout, "--timeout"},
	} {
		if *f.p == "" {
			*f.p = f.def
		}
	}
	return n
}

// Options are the client's dependencies.
type Options struct {
	// Stderr receives the version skew warning; nil discards it.
	Stderr io.Writer
	// Clock runs every deadline; nil is clock.Real().
	Clock clock.Clock
	// Rand supplies the traceparent IDs; nil is crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version, sent in User-Agent and compared
	// with a Node's ruralz-version; empty is buildinfo.Get().Version.
	Version string
}

// Client calls the admin API of one Node. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	names     Names
	tokenFile string
	timeout   time.Duration
	// tls is the configuration of an https Node; nil for http.
	tls       *tls.Config
	transport *http.Transport
	clock     clock.Clock
	version   string
	stderr    io.Writer
	limits    limits
	// dial opens the TCP connection of a call; tests replace it before
	// the first call.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	randMu sync.Mutex
	rand   io.Reader
	// warned is set once the skew warning was written.
	warned atomic.Bool
}

// limits are the body bounds of a Client; tests lower them.
type limits struct {
	dump, problem, readyz, tapLine int64
}

func defaultLimits() limits {
	return limits{dump: MaxConfigDumpBytes, problem: MaxProblemBytes, readyz: MaxReadyzBytes, tapLine: MaxTapLineBytes}
}

// New checks cfg and returns a client of its Node; nothing is sent yet.
// The base URL, the pairing of the client certificate and key and their
// https scheme, and the PEM forms are usage errors (exit 2); an unreadable
// file or an invalid token file exits 2 before any request.
func New(cfg Config, opts Options) (*Client, error) {
	names := cfg.Names.withDefaults()
	base, err := parseBase(names.URL, cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout < 0 {
		return nil, command.Usagef("%s %s: must be zero or greater (0 means none)", names.Timeout, command.FormatDuration(cfg.Timeout))
	}
	if (cfg.ClientCert == "") != (cfg.ClientKey == "") {
		return nil, command.Usagef("%s and %s come together", names.ClientCert, names.ClientKey)
	}
	if cfg.ClientCert != "" && base.Scheme != schemeHTTPS {
		return nil, command.Usagef("%s and %s need an https admin URL; %s is %s",
			names.ClientCert, names.ClientKey, names.URL, base)
	}
	c := &Client{
		base:      base,
		names:     names,
		tokenFile: cfg.TokenFile,
		timeout:   cfg.Timeout,
		clock:     opts.Clock,
		version:   opts.Version,
		stderr:    opts.Stderr,
		limits:    defaultLimits(),
		dial:      (&net.Dialer{}).DialContext,
		rand:      opts.Rand,
	}
	if c.clock == nil {
		c.clock = clock.Real()
	}
	if c.version == "" {
		c.version = buildinfo.Get().Version
	}
	if c.rand == nil {
		c.rand = rand.Reader
	}
	if _, err := c.readToken(); err != nil {
		return nil, err
	}
	if base.Scheme == schemeHTTPS {
		if c.tls, err = clientTLS(cfg, names, base.Hostname()); err != nil {
			return nil, err
		}
	}
	c.transport = c.newTransport()
	return c, nil
}

// URL returns the Node's admin base URL.
func (c *Client) URL() string { return c.base.String() }

// Close closes what the client's calls left open, a dial that a canceled
// call stopped waiting for included. The client stays usable.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// endpoint returns the URL of an admin path below the base URL's path
// prefix, with query (already encoded) as its query.
func (c *Client) endpoint(path, query string) *url.URL {
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + path
	if c.base.RawPath != "" {
		u.RawPath = strings.TrimSuffix(c.base.RawPath, "/") + path
	}
	u.RawQuery = query
	return &u
}

// readToken reads the token file by the Node's rule; "" without one.
func (c *Client) readToken() (string, error) {
	if c.tokenFile == "" {
		return "", nil
	}
	tok, err := adminauth.ReadToken(c.tokenFile)
	if err != nil {
		return "", command.NoResultf("%s %s: %w", c.names.TokenFile, c.tokenFile, err)
	}
	return tok, nil
}
