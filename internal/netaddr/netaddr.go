// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package netaddr parses the network addresses a Bundle names, so that
// validation (internal/config/validate) and the components that dial
// them (internal/gateway/upstream/discovery, internal/telemetry,
// internal/statestore) accept exactly the same values: a static Endpoint
// address, the DNS names and SRV service names of dns discovery, the OTLP
// collector endpoint, and the server hosts of the State Store zone maps
// (docs/architecture/02-configuration-model.md "Semantic validation").
//
// The package is a leaf: it imports only the standard library and keeps
// no state. Its errors state the rule a value breaks without quoting the
// value, so each caller chooses how much of it to show.
package netaddr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// DNS name limits in presentation form (RFC 1035 section 2.3.4).
const (
	// MaxName bounds a DNS name without its trailing dot.
	MaxName = 253
	// MaxLabel bounds one label.
	MaxLabel = 63
)

// maxPortName bounds an SRV service name (RFC 6335 section 5.1).
const maxPortName = 15

// DefaultOTLPPort is the port of an OTLP endpoint that names none.
const DefaultOTLPPort = "4317"

// HostPort is a parsed static Endpoint address.
type HostPort struct {
	// Host is the DNS name of the address; "" for an IP address.
	Host string
	// Addr is the IP address, an IPv4-mapped IPv6 address unmapped; the
	// zero Addr for a DNS name.
	Addr netip.Addr
	// Port is from 1 to 65535.
	Port uint16
}

// ParseHostPort parses a static Endpoint address (Upstream
// spec.endpoints[].address): host:port, the port a decimal number from 1
// to 65535, the host an IP address (IPv6 in brackets) or a DNS name that
// CheckName accepts.
func ParseHostPort(address string) (HostPort, error) {
	h, p, err := net.SplitHostPort(address)
	if err != nil {
		return HostPort{}, errors.New("the address is not host:port")
	}
	port, err := ParsePort(p)
	if err != nil {
		return HostPort{}, err
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return HostPort{Addr: a.Unmap(), Port: port}, nil
	}
	if err := CheckName(h); err != nil {
		return HostPort{}, fmt.Errorf("the host is neither an IP address nor a DNS name: %w", err)
	}
	return HostPort{Host: h, Port: port}, nil
}

// CanonicalHost returns the comparison form of a host as a URL names it or
// a Redis topology announces it, the key of the State Store zone maps
// (Gateway spec.stateStore.zones and spec.stateStore.cache.zones,
// docs/architecture/02-configuration-model.md "Gateway"): an IP literal in
// its canonical text (netip.Addr.String: IPv6 compressed and lower-case,
// an IPv4-mapped address and a zone kept as written), or a DNS name that
// CheckName accepts, lower-cased, since hosts compare case-insensitively.
// A trailing dot stays, as the host is written. An IPv6 literal takes no
// brackets: they belong to the URL syntax around it. Anything else, such
// as a host with a port, is an error.
func CanonicalHost(h string) (string, error) {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.String(), nil
	}
	if err := CheckName(h); err != nil {
		return "", fmt.Errorf("the host is neither an IP literal nor a DNS name: %w", err)
	}
	return strings.ToLower(h), nil
}

// ParsePort parses a decimal port from 1 to 65535.
func ParsePort(p string) (uint16, error) {
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, errors.New("the port is not a decimal number from 1 to 65535")
	}
	return uint16(n), nil
}

// CheckPort reports whether n is a port from 1 to 65535, as a numeric
// discovery.port must be.
func CheckPort(n int64) error {
	if n < 1 || n > 65535 {
		return errors.New("the port is not from 1 to 65535")
	}
	return nil
}

// CheckName reports whether n is a DNS name in presentation form: 1 to
// 253 bytes of labels of 1 to 63 letters, digits, hyphens and underscores,
// with an optional trailing dot. Underscores are allowed because SRV owner
// names and some internal zones use them.
func CheckName(n string) error {
	n = strings.TrimSuffix(n, ".")
	if n == "" || len(n) > MaxName {
		return errors.New("a DNS name is 1 to 253 bytes")
	}
	for label := range strings.SplitSeq(n, ".") {
		if label == "" || len(label) > MaxLabel {
			return errors.New("a DNS label is 1 to 63 bytes")
		}
		for i := range len(label) {
			if c := label[i]; !isNameByte(c) {
				return fmt.Errorf("the byte %q is not a letter, digit, '-' or '_'", c)
			}
		}
	}
	return nil
}

// IsName reports whether CheckName accepts n, without allocating: the
// test of names in DNS answers on the request path.
func IsName(n string) bool {
	n = strings.TrimSuffix(n, ".")
	if n == "" || len(n) > MaxName {
		return false
	}
	label := 0
	for i := range len(n) {
		c := n[i]
		switch {
		case c == '.':
			if label == 0 {
				return false
			}
			label = 0
		case isNameByte(c):
			label++
			if label > MaxLabel {
				return false
			}
		default:
			return false
		}
	}
	return label > 0
}

// isNameByte reports a byte a DNS label may hold.
func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// CheckPortName reports whether n is an SRV service name, as a named
// discovery.port must be: 1 to 15 letters, digits and hyphens with at
// least one letter (RFC 6335 section 5.1).
func CheckPortName(n string) error {
	if n == "" || len(n) > maxPortName {
		return errors.New("a port name is 1 to 15 characters")
	}
	letter := false
	for i := range len(n) {
		switch c := n[i]; {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9' || c == '-':
		default:
			return errors.New("a port name holds only letters, digits and '-'")
		}
	}
	if !letter {
		return errors.New("a port name needs a letter")
	}
	return nil
}

// OTLPEndpoint is a parsed telemetry.otlp.endpoint.
type OTLPEndpoint struct {
	// Scheme is http or https.
	Scheme string
	// Target is the gRPC target host:port; the port is DefaultOTLPPort
	// when the URL names none.
	Target string
}

// ParseOTLPEndpoint parses telemetry.otlp.endpoint: an http or https URL
// with a host and an optional, non-empty port, without path (but "/"),
// user information, query or fragment.
func ParseOTLPEndpoint(raw string) (OTLPEndpoint, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint must use http or https")
	case u.User != nil, u.RawQuery != "", u.ForceQuery, u.Fragment != "", u.Opaque != "":
		return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint has no user information, query or fragment")
	case u.Path != "" && u.Path != "/":
		return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint has no path")
	case u.Hostname() == "":
		return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint has no host")
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return OTLPEndpoint{}, errors.New("telemetry.otlp.endpoint has an empty port")
		}
		port = DefaultOTLPPort
	}
	return OTLPEndpoint{Scheme: u.Scheme, Target: net.JoinHostPort(u.Hostname(), port)}, nil
}
