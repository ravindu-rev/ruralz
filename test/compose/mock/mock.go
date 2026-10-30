// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mock is the program of the Compose suite's test-only mock image
// (docs/engineering/03-testing-and-quality-strategy.md "Compose suite"),
// which test/compose/mockimage wraps in a main package and the suite builds
// on a scratch base; it is never published. One binary plays four parts:
//
//   - the mock Upstream (internal/testkit/mockup), answering each path as
//     a Routes file programs it and logging every request;
//   - the mock IdP (internal/testkit/mockidp) over TLS, publishing the
//     signing key it is given in its JWKS and issuing client-credentials
//     tokens to the one client it is given;
//   - a DNS sink, which answers every query NXDOMAIN and records the
//     question, so as a Node's only resolver it shows every name the Node
//     looked up;
//   - a probe client, which waits for a Node's /readyz from inside a
//     Compose network, sends one request and reads a DNS sink's record.
//
// "mock serve" runs the first three and a control API, plain HTTP, that
// the suite reads them through: GET /requests (RequestLog), GET /dns
// (DNSLog), GET /idp (IdPLog) and GET /healthz. "mock probe" prints one
// ProbeReport as JSON on standard output. The suite decodes both with the
// types of this package.
//
// Exit codes: ExitOK, ExitFailure (a part did not start, or the report
// could not be written) and ExitUsage.
package mock

import (
	"context"
	"io"
	"net/http"
)

// Exit codes of the mock program.
const (
	// ExitOK ends a serve stopped by SIGTERM or SIGINT, or a probe that
	// printed its report.
	ExitOK = 0
	// ExitFailure is a part that did not start or a report that could not
	// be written.
	ExitFailure = 1
	// ExitUsage is a usage error.
	ExitUsage = 2
)

// usage is the text Main writes for a usage error.
const usage = `usage: mock serve [flags]   mock Upstream, mock IdP, DNS sink and control API
       mock probe [flags]   wait for a Node, send one request, read a DNS sink

Run "mock serve -h" or "mock probe -h" for the flags.
`

// Main runs the mock program with args (without the program name) and
// returns its exit code. ctx ends a serve as SIGTERM does.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return ExitUsage
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "probe":
		return probe(ctx, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(stdout, usage)
		return ExitOK
	default:
		_, _ = io.WriteString(stderr, usage)
		return ExitUsage
	}
}

// Answer is one programmed answer of the mock Upstream.
type Answer struct {
	// Status is the response status; 0 means 200.
	Status int `json:"status,omitempty"`
	// ContentType is the Content-Type of Body; empty means
	// "text/plain; charset=utf-8".
	ContentType string `json:"contentType,omitempty"`
	// Body is the response body.
	Body string `json:"body,omitempty"`
	// Header is added to the response.
	Header map[string]string `json:"header,omitempty"`
}

// Routes programs the mock Upstream: Paths answers a request whose URL
// path equals a key (the query is not part of it), Default every other
// path.
type Routes struct {
	Default Answer            `json:"default"`
	Paths   map[string]Answer `json:"paths,omitempty"`
}

// Request is one request the mock Upstream received.
type Request struct {
	// Seq numbers requests from 1 in arrival order.
	Seq uint64 `json:"seq"`
	// Method, Host, URI (the request target as received), Path, RawQuery
	// and Proto describe the request line.
	Method   string `json:"method"`
	Host     string `json:"host"`
	URI      string `json:"uri"`
	Path     string `json:"path"`
	RawQuery string `json:"rawQuery,omitempty"`
	Proto    string `json:"proto"`
	// Header holds the request's fields.
	Header http.Header `json:"header"`
	// RemoteAddr is the client's address.
	RemoteAddr string `json:"remoteAddr"`
	// BodyBytes is the body's length.
	BodyBytes int64 `json:"bodyBytes"`
}

// RequestLog is the answer of GET /requests: the mock Upstream's logged
// requests, oldest first, and how many the bounded log evicted; a leak
// scan over it is complete only when Evicted is 0.
type RequestLog struct {
	Requests []Request `json:"requests"`
	Evicted  int64     `json:"evicted"`
}

// TokenRequest is one request to the mock IdP's token endpoint. It never
// carries the client secret: the token endpoint is the secret's only
// destination.
type TokenRequest struct {
	ClientID   string `json:"clientId"`
	AuthMethod string `json:"authMethod"`
	GrantType  string `json:"grantType"`
	Scope      string `json:"scope"`
	Status     int    `json:"status"`
	// AccessToken is the token issued, "" when none was.
	AccessToken string `json:"accessToken,omitempty"`
}

// IdPLog is the answer of GET /idp: the JWKS fetches counted and the token
// requests, oldest first.
type IdPLog struct {
	JWKSFetches   int64          `json:"jwksFetches"`
	TokenRequests []TokenRequest `json:"tokenRequests"`
}

// ProbeResult is one HTTP exchange of a probe: the last attempt's status,
// header and body (cut at maxProbeBody bytes), or its error.
type ProbeResult struct {
	URL      string      `json:"url"`
	Attempts int         `json:"attempts"`
	Status   int         `json:"status,omitempty"`
	Header   http.Header `json:"header,omitempty"`
	Body     string      `json:"body,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// ProbeReport is what "mock probe" prints: each part it was asked for.
type ProbeReport struct {
	// Ready is the wait for a 200 from the -ready URL.
	Ready *ProbeResult `json:"ready,omitempty"`
	// Request is the -get request, retried until it answers 200.
	Request *ProbeResult `json:"request,omitempty"`
	// SinkCheck is the lookup of -sink-check-name through the DNS sink at
	// -sink-check, which proves the sink answers and records: Error holds
	// the resolver's error, a not-found error when the sink answered.
	SinkCheck *SinkCheck `json:"sinkCheck,omitempty"`
	// DNS is the DNS sink's record read from -dns-log, after every other
	// part.
	DNS *DNSLog `json:"dns,omitempty"`
	// DNSError is why DNS is missing.
	DNSError string `json:"dnsError,omitempty"`
}

// SinkCheck is the probe's own lookup through a DNS sink.
type SinkCheck struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	NotFound bool   `json:"notFound"`
	Error    string `json:"error,omitempty"`
}
