// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/netaddr"
)

// TestSpecValidate covers the source shapes and the dns rule (service
// and port required).
func TestSpecValidate(t *testing.T) {
	long := strings.Repeat("a", 64)
	tests := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"empty static", Spec{}, true},
		{"ipv4 literal", Spec{Static: []StaticEndpoint{{Address: "10.0.0.1:8080", Weight: 1}}}, true},
		{"ipv6 literal", Spec{Static: []StaticEndpoint{{Address: "[2001:db8::1]:443", Weight: 1}}}, true},
		{"host name", Spec{Static: []StaticEndpoint{{Address: "api.internal:80", Weight: 1}}}, true},
		{"rooted host name", Spec{Static: []StaticEndpoint{{Address: "api.internal.:80"}}}, true},
		{"underscore label", Spec{Static: []StaticEndpoint{{Address: "_svc.example:80"}}}, true},
		{"missing port", Spec{Static: []StaticEndpoint{{Address: "api.internal"}}}, false},
		{"port zero", Spec{Static: []StaticEndpoint{{Address: "api.internal:0"}}}, false},
		{"port too large", Spec{Static: []StaticEndpoint{{Address: "api.internal:65536"}}}, false},
		{"named port static", Spec{Static: []StaticEndpoint{{Address: "api.internal:http"}}}, false},
		{"empty host", Spec{Static: []StaticEndpoint{{Address: ":80"}}}, false},
		{"space in host", Spec{Static: []StaticEndpoint{{Address: "a b:80"}}}, false},
		{"long label", Spec{Static: []StaticEndpoint{{Address: long + ".example:80"}}}, false},
		{"empty label", Spec{Static: []StaticEndpoint{{Address: "a..b:80"}}}, false},
		{"dns numeric", Spec{DNS: &DNSSpec{Service: "orders.svc", Port: 8080}}, true},
		{"dns named", Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "http"}}, true},
		{"dns no service", Spec{DNS: &DNSSpec{Port: 80}}, false},
		{"dns no port", Spec{DNS: &DNSSpec{Service: "orders.svc"}}, false},
		{"dns both ports", Spec{DNS: &DNSSpec{Service: "orders.svc", Port: 80, PortName: "http"}}, false},
		{"dns bad port name", Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "h_ttp"}}, false},
		{"dns digit port name", Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "8080"}}, false},
		{"dns long port name", Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "abcdefghijklmnop"}}, false},
		{"dns and static", Spec{DNS: &DNSSpec{Service: "orders.svc", Port: 80}, Static: []StaticEndpoint{{Address: "a:1"}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok != (err == nil) {
				t.Fatalf("Validate = %v, want ok %v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error %v does not wrap ErrInvalidSpec", err)
			}
			if _, nerr := New(tc.spec, Options{Resolver: newFakeResolver()}); (nerr == nil) != tc.ok {
				t.Fatalf("New error %v, want ok %v", nerr, tc.ok)
			}
		})
	}
}

func TestSpecEqualKeyType(t *testing.T) {
	a := Spec{Static: []StaticEndpoint{{Address: "a:1", Weight: 1}, {Address: "b:2", Weight: 3}}}
	b := Spec{Static: []StaticEndpoint{{Address: "a:1", Weight: 1}, {Address: "b:2", Weight: 3}}}
	c := Spec{Static: []StaticEndpoint{{Address: "a:1", Weight: 1}, {Address: "b:2", Weight: 4}}}
	d1 := Spec{DNS: &DNSSpec{Service: "s", Port: 80}}
	d2 := Spec{DNS: &DNSSpec{Service: "s", Port: 80}}
	d3 := Spec{DNS: &DNSSpec{Service: "s", PortName: "http"}}
	if !a.Equal(b) || a.Equal(c) || a.Equal(d1) || !d1.Equal(d2) || d1.Equal(d3) || d1.Equal(a) {
		t.Fatal("Equal mismatch")
	}
	if a.key() != b.key() || a.key() == c.key() || d1.key() == d3.key() || d1.key() != d2.key() {
		t.Fatal("key mismatch")
	}
	if a.Type() != "static" || d1.Type() != "dns" {
		t.Fatalf("Type = %q, %q", a.Type(), d1.Type())
	}
}

func TestParseStatic(t *testing.T) {
	se, err := parseStatic(StaticEndpoint{Address: "[::ffff:10.0.0.1]:80", Weight: 2})
	if err != nil {
		t.Fatal(err)
	}
	if se.host != "" || se.addr.String() != "10.0.0.1" || se.port != 80 || se.identity != "[::ffff:10.0.0.1]:80" {
		t.Fatalf("parsed %+v", se)
	}
	se, err = parseStatic(StaticEndpoint{Address: "API.internal:443"})
	if err != nil {
		t.Fatal(err)
	}
	if se.host != "API.internal" || se.addr.IsValid() || se.identity != "API.internal:443" {
		t.Fatalf("parsed %+v", se)
	}
}

// TestSpecUsesNetaddr checks that the Upstream layer accepts exactly the
// addresses, names and port names validation accepts: Validate refuses a
// value if and only if internal/netaddr does, with netaddr's reason, and a
// static address parses to netaddr's host, address and port.
func TestSpecUsesNetaddr(t *testing.T) {
	agrees := func(err, want error) bool {
		if want == nil {
			return err == nil
		}
		return errors.Is(err, ErrInvalidSpec) && strings.HasSuffix(err.Error(), ": "+want.Error())
	}
	for _, a := range []string{
		"10.0.0.1:8080", "[2001:db8::1]:443", "[::ffff:10.0.0.1]:80", "[fe80::1%eth0]:80",
		"api.internal:80", "API.internal.:080", "_svc.example:80", "api.internal",
		"api.internal:0", "api.internal:65536", "api.internal:+80", "api.internal:http",
		":80", "a b:80", "a..b:80", strings.Repeat("a", 64) + ".example:80",
		strings.Repeat("a.", 127) + "a:80", "[::1]", "::1:80",
	} {
		hp, want := netaddr.ParseHostPort(a)
		se, err := parseStatic(StaticEndpoint{Address: a})
		switch {
		case !agrees(err, want):
			t.Errorf("parseStatic(%q) error = %v, netaddr says %v", a, err, want)
		case err == nil && (se.host != hp.Host || se.addr != hp.Addr || se.port != hp.Port):
			t.Errorf("parseStatic(%q) = %+v, netaddr parses %+v", a, se, hp)
		}
		if verr := (Spec{Static: []StaticEndpoint{{Address: a}}}).Validate(); !agrees(verr, want) {
			t.Errorf("Validate(endpoint %q) = %v, netaddr says %v", a, verr, want)
		}
	}
	for _, n := range []string{
		"orders.svc", "orders.svc.", "_http._tcp.orders", "10.0.0.1", "", ".", "a..b",
		"bad name", "a\x00b", strings.Repeat("a", 63), strings.Repeat("a", 64),
		strings.Repeat("a.", 126) + "a", strings.Repeat("a.", 127) + "a",
	} {
		if err, want := (Spec{DNS: &DNSSpec{Service: n, Port: 80}}).Validate(), netaddr.CheckName(n); !agrees(err, want) {
			t.Errorf("Validate(service %q) = %v, netaddr says %v", n, err, want)
		}
	}
	for _, p := range []string{"http", "grpc-web", "h2c", "h_ttp", "8080", "-", "a", "abcdefghijklmno", "abcdefghijklmnop"} {
		if err, want := (Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: p}}).Validate(), netaddr.CheckPortName(p); !agrees(err, want) {
			t.Errorf("Validate(port name %q) = %v, netaddr says %v", p, err, want)
		}
	}
}
