// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"strings"
	"testing"
)

// TestResolveServer: a server names an Upstream only through an absolute
// http or https URL once its variables take their defaults: host in lower
// case, the scheme's port unless the URL names one, the path without its
// trailing slash as the base of its operations' paths.
func TestResolveServer(t *testing.T) {
	vars := []ServerVariable{{Name: "scheme", Default: "https"}, {Name: "host", Default: "API.Example.com"}, {Name: "port", Default: "8443"}}
	tests := []struct {
		name string
		s    Server
		want endpoint
		why  string // a part of the reason; empty when the server names an endpoint
	}{
		{name: "https", s: Server{URL: "https://api.example.com"}, want: endpoint{scheme: "https", host: "api.example.com", port: "443"}},
		{name: "http with a port and base", s: Server{URL: "http://api.example.com:8080/v1/"}, want: endpoint{scheme: "http", host: "api.example.com", port: "8080", base: "/v1"}},
		{name: "upper-case scheme and host", s: Server{URL: "HTTPS://API.Example.COM/V1"}, want: endpoint{scheme: "https", host: "api.example.com", port: "443", base: "/V1"}},
		{name: "variables", s: Server{URL: "{scheme}://{host}:{port}/api", Variables: vars}, want: endpoint{scheme: "https", host: "api.example.com", port: "8443", base: "/api"}},
		{name: "an IPv6 literal", s: Server{URL: "http://[2001:db8::1]:9000"}, want: endpoint{scheme: "http", host: "2001:db8::1", port: "9000"}},
		{name: "an IPv4 literal", s: Server{URL: "https://10.0.0.1"}, want: endpoint{scheme: "https", host: "10.0.0.1", port: "443"}},
		{name: "an escaped base", s: Server{URL: "https://a.example/v1%20beta"}, want: endpoint{scheme: "https", host: "a.example", port: "443", base: "/v1%20beta"}},
		{name: "relative", s: Server{URL: "/api/v3"}, why: `server URL "/api/v3" is relative: it names no host`},
		{name: "the root", s: Server{URL: "/"}, why: "is relative"},
		{name: "empty", s: Server{URL: ""}, why: "is relative"},
		{name: "another scheme", s: Server{URL: "wss://a.example"}, why: `uses the scheme "wss", not http or https`},
		{name: "user information", s: Server{URL: "https://u:p@a.example"}, why: "carries user information"}, //nolint:gosec // G101: user information the rule refuses, no credential.
		{name: "a query", s: Server{URL: "https://a.example/?v=1"}, why: "has a query or a fragment"},
		{name: "a fragment", s: Server{URL: "https://a.example/#x"}, why: "has a query or a fragment"},
		{name: "a bad host", s: Server{URL: "https://a_b!.example"}, why: "the host is neither an IP address nor a DNS name"},
		{name: "a bad port", s: Server{URL: "https://a.example:0"}, why: `server URL "https://a.example:0": `},
		{name: "a missing variable", s: Server{URL: "https://{region}.example"}, why: `names the variable "region", which its variables lack`},
		{name: "an open brace", s: Server{URL: "https://{region.example"}, why: "has a { that closes no variable"},
		{name: "a close brace", s: Server{URL: "https://region}.example"}, why: "has a } that closes no variable"},
		{name: "nested braces", s: Server{URL: "https://{a{b}}.example"}, why: "has a { that closes no variable"},
		{name: "not a URL", s: Server{URL: "http://a.example:x:y"}, why: "is not a URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := resolveServer(tc.s)
			if tc.why == "" {
				if why != "" || got != tc.want {
					t.Errorf("resolveServer(%q) = %+v, %q; want %+v", tc.s.URL, got, why, tc.want)
				}
				return
			}
			if !strings.Contains(why, tc.why) {
				t.Errorf("resolveServer(%q) reason = %q, want one containing %q", tc.s.URL, why, tc.why)
			}
		})
	}
}

// TestEndpointAddress: the address and key of an endpoint join host and
// port, bracketing an IPv6 literal, and the key tells schemes apart.
func TestEndpointAddress(t *testing.T) {
	e := endpoint{scheme: "https", host: "2001:db8::1", port: "443"}
	if e.address() != "[2001:db8::1]:443" || e.key() != "https://[2001:db8::1]:443" {
		t.Errorf("address %q key %q", e.address(), e.key())
	}
	if defaultPort("https") != "443" || defaultPort("http") != "80" {
		t.Error("default ports")
	}
}
