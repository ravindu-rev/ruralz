// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package redirect compiles a Route's redirect and builds the Location it
// answers with (docs/architecture/09-traffic-management-and-resilience.md
// "Redirects"). A Redirect is the snapshot.Redirector of one Route with
// Route.spec.redirect; the request handler answers its requests after the
// onRequestHeaders Policies, so authentication and Rate Limits still apply
// (docs/architecture/03-data-plane.md "Short-circuit").
//
// # The Location
//
// From the fields, the Location is an absolute URI, the request URL with
// the redirect's parts applied:
//
//   - the scheme is scheme, else the request's;
//   - the authority is host, written as authored, else the request's host
//     as the client sent it, without its port when scheme changes the
//     request's scheme, since the new scheme's default port applies;
//   - the path is path; else, with prefix, the request path with the
//     match.path.prefix it matched replaced by prefix, a trailing slash of
//     either ignored, so segments join with one slash ("/v1/x" under
//     prefix /v1 to /v2 is "/v2/x", "/v1" is "/v2", and prefix / drops the
//     matched one: "/x"); else the request path, each in the Router's
//     normalized form;
//   - the query is the request's raw query, as received, under query keep
//     (the default), and none under drop.
//
// A request without a host, which only HTTP/1.0 can send without Host,
// leaves no authority to name: its Location is the path and query as an
// absolute-path reference, which the client resolves against the URL it
// used, and a path that starts with "//" is written behind a "/." segment,
// so it never reads as a network-path reference to another host.
//
// locationExpression, Planned (M2), builds the whole Location instead: a
// string under the computed-value rule of a header field (spaces and tabs
// around it trimmed, at most 8 KiB, httpfield.NormalizeValue) that holds
// only the characters of an RFC 3986 URI reference and parses as one. Its
// runtime-error rule is OQ-traffic-management-and-resilience-26, so the
// serve check refuses the field (RZ-CFG-040) until it is answered.
//
// A Redirect is immutable and safe for concurrent use. Location allocates
// the Location it returns and nothing else.
package redirect

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CodeInvalid is RZ-CFG-005: a redirect Compile refuses, which validation
// refuses first (docs/architecture/02-configuration-model.md "Semantic
// validation"), so only a Revision that skipped it meets the code.
const CodeInvalid = "RZ-CFG-005"

// DefaultStatus is the status of a redirect without one.
const DefaultStatus = 302

// MaxLocationBytes bounds the Location locationExpression builds: the
// computed-value cap of a header field (target).
const MaxLocationBytes = httpfield.MaxComputedValueBytes

// ErrLocation reports a Location locationExpression built that the
// computed-value rule or the URI reference syntax refuses; the error never
// holds the value.
var ErrLocation = errors.New("redirect: locationExpression built no valid Location")

// Redirect is one Route's compiled redirect (snapshot.Redirector).
type Redirect struct {
	status int
	// scheme replaces the request's; "" keeps it.
	scheme string
	// host replaces the request's authority; "" keeps it.
	host string
	// path replaces the request path, normalized; "" keeps it.
	path string
	// prefix is set when to replaces the matched prefix from.
	prefix bool
	// from is the Route's normalized match.path.prefix and to the
	// normalized redirect.prefix, each without a trailing slash ("" for
	// "/").
	from, to string
	// dropQuery drops the request's query.
	dropQuery bool
	// location builds the whole Location; nil for the fields form.
	location expr.Program
}

var _ snapshot.Redirector = (*Redirect)(nil)

// Compile compiles the redirect of rt, which holds Route.spec.redirect;
// builder compiles its locationExpression at site, the Route field's site
// (pipeline.RouteSite). It refuses with CodeInvalid what validation
// refuses: a status other than 301, 302, 307 or 308, a host that is no
// authority (netaddr.CheckAuthority), a path or prefix that may not
// replace the client's (routematch.CheckRedirectPath), a prefix on a Route
// whose match.path is no prefix, and a locationExpression beside a field it
// excludes; a locationExpression that does not compile is the builder's
// RZ-CFG-014 or RZ-CFG-015.
func Compile(rt *v1alpha1.Route, builder expr.Builder, site expr.Site) (*Redirect, error) {
	if rt == nil || rt.Spec.Redirect == nil {
		return nil, errcode.Errorf(CodeInvalid, "the Route holds no redirect")
	}
	r := rt.Spec.Redirect
	out := &Redirect{status: DefaultStatus, scheme: string(r.Scheme), host: r.Host}
	if r.Status != nil {
		out.status = int(*r.Status)
	}
	switch out.status {
	case 301, 302, 307, 308:
	default:
		return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect status %d is not 301, 302, 307 or 308", rt.Metadata.Name, out.status)
	}
	if r.Query != nil {
		out.dropQuery = *r.Query == v1alpha1.RedirectQueryDrop
	}
	if src := r.LocationExpression; src != "" {
		if r.Scheme != "" || r.Host != "" || r.Path != "" || r.Prefix != "" {
			return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect locationExpression excludes scheme, host, path and prefix", rt.Metadata.Name)
		}
		prg, issues := builder.Compile(site, src)
		if prg == nil {
			return nil, issueError(rt.Metadata.Name, issues)
		}
		out.location = prg
		return out, nil
	}
	if out.host != "" {
		if err := netaddr.CheckAuthority(out.host); err != nil {
			return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect host: %v", rt.Metadata.Name, err)
		}
	}
	var err error
	switch {
	case r.Path != "" && r.Prefix != "":
		return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect sets both path and prefix", rt.Metadata.Name)
	case r.Path != "":
		out.path, err = redirectPath(r.Path)
	case r.Prefix != "":
		m := rt.Spec.Match.Path
		if m == nil || m.Prefix == "" {
			return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect prefix needs a match.path prefix", rt.Metadata.Name)
		}
		var from string
		if from, err = routematch.NormalizePath(m.Prefix); err == nil {
			out.prefix, out.from = true, strings.TrimSuffix(from, "/")
			out.to, err = redirectPath(r.Prefix)
			out.to = strings.TrimSuffix(out.to, "/")
		}
	}
	if err != nil {
		return nil, errcode.Errorf(CodeInvalid, "Route %q: redirect: %v", rt.Metadata.Name, err)
	}
	return out, nil
}

// redirectPath returns p in the form a Location carries, the Router's
// normalized form, once routematch.CheckRedirectPath accepts it.
func redirectPath(p string) (string, error) {
	if err := routematch.CheckRedirectPath(p); err != nil {
		return "", err
	}
	return routematch.NormalizePath(p)
}

// issueError returns the error of a locationExpression the builder
// refused: its first issue, with that issue's code.
func issueError(route string, issues []expr.Issue) error {
	if len(issues) == 0 {
		return errcode.Errorf(CodeInvalid, "Route %q: redirect locationExpression does not compile", route)
	}
	return errcode.Errorf(issues[0].Code, "Route %q: redirect locationExpression: %s", route, issues[0].Message)
}

// Status implements snapshot.Redirector.
func (r *Redirect) Status() int { return r.status }

// Location implements snapshot.Redirector: the Location of the redirect
// for in (package documentation). It fails only for locationExpression:
// the program's runtime error, or ErrLocation for a result that is no
// valid Location.
func (r *Redirect) Location(in *snapshot.RedirectRequest) (string, error) {
	if r.location != nil {
		v, err := r.location.EvalString(in.Vars)
		if err != nil {
			return "", fmt.Errorf("redirect: locationExpression: %w", err)
		}
		return checkLocation(v)
	}
	path := in.Path
	switch {
	case r.path != "":
		path = r.path
	case r.prefix && strings.HasPrefix(path, r.from):
		path = r.to + path[len(r.from):]
	}
	if path == "" {
		path = "/"
	}
	query := ""
	if !r.dropQuery {
		query = in.RawQuery
	}
	authority := r.host
	if authority == "" {
		authority = in.Host
		if r.scheme != "" && r.scheme != in.Scheme {
			authority = withoutPort(authority)
		}
	}
	var b strings.Builder
	n := len(path) + len(query) + 1
	if authority != "" {
		n += len(r.scheme) + len(in.Scheme) + 3 + len(authority)
	} else {
		n += 2
	}
	b.Grow(n)
	switch {
	case authority != "":
		b.WriteString(r.schemeFor(in))
		b.WriteString("://")
		b.WriteString(authority)
	case strings.HasPrefix(path, "//"):
		// An absolute-path reference never starts with "//", which would
		// name another host: "/." resolves away and keeps the path.
		b.WriteString("/.")
	}
	b.WriteString(path)
	if query != "" {
		b.WriteByte('?')
		b.WriteString(query)
	}
	return b.String(), nil
}

// schemeFor returns the Location's scheme for in.
func (r *Redirect) schemeFor(in *snapshot.RedirectRequest) string {
	if r.scheme != "" {
		return r.scheme
	}
	return in.Scheme
}

// withoutPort returns authority a without its port: "[::1]:8080" is
// "[::1]" and "shop.example:8080" "shop.example".
func withoutPort(a string) string {
	if strings.HasPrefix(a, "[") {
		if i := strings.IndexByte(a, ']'); i >= 0 {
			return a[:i+1]
		}
		return a
	}
	if i := strings.LastIndexByte(a, ':'); i >= 0 && strings.IndexByte(a, ':') == i {
		return a[:i]
	}
	return a
}

// checkLocation returns v, the result of locationExpression, under the
// computed-value rule (httpfield.NormalizeValue: outer spaces and tabs
// trimmed, at most MaxLocationBytes, no control byte), when what is left is
// a non-empty RFC 3986 URI reference: only unreserved, reserved and "%"
// characters, which net/url parses. Any other result is ErrLocation.
func checkLocation(v string) (string, error) {
	v, err := httpfield.NormalizeValue(v)
	if err != nil || v == "" {
		return "", ErrLocation
	}
	for i := range len(v) {
		if !uriByte(v[i]) {
			return "", ErrLocation
		}
	}
	if _, err := url.Parse(v); err != nil {
		return "", ErrLocation
	}
	return v, nil
}

// uriByte reports a byte an RFC 3986 URI reference may hold: unreserved
// (ALPHA DIGIT - . _ ~), gen-delims (: / ? # [ ] @), sub-delims (! $ & '
// ( ) * + , ; =) and the "%" of a percent escape.
func uriByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte("-._~:/?#[]@!$&'()*+,;=%", c) >= 0
	}
}
