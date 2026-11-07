// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"net"
	"net/url"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/netaddr"
)

// endpoint is a server's absolute http or https URL, its variables
// substituted with their defaults: the Upstream it names and the base
// path its operations' paths extend.
type endpoint struct {
	// scheme is http or https.
	scheme string
	// host is the host in lower case, an IPv6 literal without brackets.
	host string
	// port is the URL's port, or the scheme's default.
	port string
	// base is the URL's escaped path without a trailing slash; "" for the
	// root.
	base string
}

// address is the Endpoint address of the server, host:port.
func (e endpoint) address() string { return net.JoinHostPort(e.host, e.port) }

// key identifies the Upstream of the server: its scheme, host and port.
func (e endpoint) key() string { return e.scheme + "://" + e.address() }

// defaultPort is the port of a scheme without one.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// substitute replaces each {name} of a server URL with its variable's
// default. A name without a variable, or a brace that opens no name, is
// the reason the URL cannot be used.
func substitute(s Server) (string, string) {
	var b strings.Builder
	rest := s.URL
	for {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			b.WriteString(rest)
			return b.String(), ""
		}
		if rest[open] == '}' {
			return "", "has a } that closes no variable"
		}
		end := strings.IndexAny(rest[open+1:], "{}")
		if end < 0 || rest[open+1+end] == '{' {
			return "", "has a { that closes no variable"
		}
		name := rest[open+1 : open+1+end]
		found := false
		for _, v := range s.Variables {
			if v.Name == name {
				b.WriteString(rest[:open])
				b.WriteString(v.Default)
				found = true
				break
			}
		}
		if !found {
			return "", "names the variable " + quote(name) + ", which its variables lack"
		}
		rest = rest[open+1+end+1:]
	}
}

// resolveServer returns the endpoint of s, or the reason s names none: a
// URL that is relative, uses another scheme than http or https, carries
// user information, a query or a fragment, or whose host is neither an IP
// address nor a DNS name.
func resolveServer(s Server) (endpoint, string) {
	raw, why := substitute(s)
	if why != "" {
		return endpoint{}, "server URL " + quote(s.URL) + " " + why
	}
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, "server URL " + quote(raw) + " is not a URL"
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case u.Scheme == "" || u.Host == "":
		return endpoint{}, "server URL " + quote(raw) + " is relative: it names no host"
	case scheme != "http" && scheme != "https":
		return endpoint{}, "server URL " + quote(raw) + " uses the scheme " + quote(u.Scheme) + ", not http or https"
	case u.User != nil:
		return endpoint{}, "server URL " + quote(raw) + " carries user information, which an Upstream address never holds"
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return endpoint{}, "server URL " + quote(raw) + " has a query or a fragment"
	}
	e := endpoint{scheme: scheme, host: strings.ToLower(u.Hostname()), port: u.Port()}
	if e.port == "" {
		e.port = defaultPort(scheme)
	}
	if _, err := netaddr.ParseHostPort(e.address()); err != nil {
		return endpoint{}, "server URL " + quote(raw) + ": " + err.Error()
	}
	e.base = strings.TrimSuffix(u.EscapedPath(), "/")
	return e, ""
}
