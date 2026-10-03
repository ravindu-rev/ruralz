// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Spec is the compiled Endpoint source of one Upstream: static endpoints or
// DNS discovery, never both. The Upstream layer builds it from
// Upstream.spec.endpoints (weight default 1 applied) or
// Upstream.spec.discovery.
type Spec struct {
	// Static lists the configured endpoints in authored order.
	Static []StaticEndpoint
	// DNS is set for discovery.type dns.
	DNS *DNSSpec
}

// StaticEndpoint is one endpoints[] entry.
type StaticEndpoint struct {
	// Address is host:port; host is an IP literal or a DNS name.
	Address string
	// Weight is endpoints[].weight (0 allowed; see package balance).
	Weight uint32
}

// DNSSpec is discovery.type dns: exactly one of Port and PortName is set.
type DNSSpec struct {
	// Service is the DNS name looked up.
	Service string
	// Port is a numeric discovery.port: A and AAAA lookups of Service.
	Port uint16
	// PortName is a named discovery.port: SRV _<PortName>._tcp.<Service>.
	PortName string
}

// Spec errors; Validate wraps them with the offending value.
var (
	// ErrInvalidSpec reports a Spec the Upstream layer must not compile
	// (schema and reference validation normally reject it first).
	ErrInvalidSpec = errors.New("discovery: invalid endpoint source")
)

// maxNameLen bounds a DNS name in presentation form (RFC 1035 section
// 2.3.4).
const maxNameLen = 253

// Validate reports whether s is a usable source: every static address is
// host:port with a port from 1 to 65,535, and DNS discovery names a service
// and exactly one of a numeric port and a port name (05 req 97, RZ-CFG-005
// at validation).
func (s Spec) Validate() error {
	if s.DNS != nil {
		if len(s.Static) > 0 {
			return fmt.Errorf("%w: static endpoints and dns discovery are exclusive", ErrInvalidSpec)
		}
		return s.DNS.validate()
	}
	for _, e := range s.Static {
		if _, err := parseStatic(e); err != nil {
			return err
		}
	}
	return nil
}

func (d *DNSSpec) validate() error {
	if err := checkName(d.Service); err != nil {
		return fmt.Errorf("%w: discovery.service: %w", ErrInvalidSpec, err)
	}
	switch {
	case d.Port != 0 && d.PortName != "":
		return fmt.Errorf("%w: discovery.port is either a number or a name", ErrInvalidSpec)
	case d.Port == 0 && d.PortName == "":
		return fmt.Errorf("%w: discovery.port is required", ErrInvalidSpec)
	case d.PortName != "":
		if err := checkPortName(d.PortName); err != nil {
			return fmt.Errorf("%w: discovery.port: %w", ErrInvalidSpec, err)
		}
	}
	return nil
}

// Equal reports whether s and o describe the same source, so a Hot Reload
// can keep the running Source.
func (s Spec) Equal(o Spec) bool {
	if (s.DNS == nil) != (o.DNS == nil) {
		return false
	}
	if s.DNS != nil && *s.DNS != *o.DNS {
		return false
	}
	return slices.Equal(s.Static, o.Static)
}

// key identifies the source for carrying a previous Set across a Hot Reload.
func (s Spec) key() string {
	if s.DNS != nil {
		if s.DNS.PortName != "" {
			return "srv:" + s.DNS.PortName + "@" + s.DNS.Service
		}
		return "dns:" + s.DNS.Service + ":" + strconv.Itoa(int(s.DNS.Port))
	}
	var b strings.Builder
	b.WriteString("static:")
	for _, e := range s.Static {
		b.WriteString(e.Address)
		b.WriteByte('/')
		b.WriteString(strconv.FormatUint(uint64(e.Weight), 10))
		b.WriteByte(',')
	}
	return b.String()
}

// Type returns the /debug/upstreams source type: "dns" or "static".
func (s Spec) Type() string {
	if s.DNS != nil {
		return "dns"
	}
	return "static"
}

// staticEntry is one parsed static Endpoint.
type staticEntry struct {
	identity string     // the configured address (05 req 5)
	host     string     // DNS name; "" for an IP literal
	addr     netip.Addr // the literal when host is ""
	port     uint16
	weight   uint32
}

// parseStatic splits a static address into its parts.
func parseStatic(e StaticEndpoint) (staticEntry, error) {
	h, p, err := net.SplitHostPort(e.Address)
	if err != nil {
		return staticEntry{}, fmt.Errorf("%w: endpoint address %q: %w", ErrInvalidSpec, e.Address, err)
	}
	port, err := parsePort(p)
	if err != nil {
		return staticEntry{}, fmt.Errorf("%w: endpoint address %q: %w", ErrInvalidSpec, e.Address, err)
	}
	se := staticEntry{identity: e.Address, port: port, weight: e.Weight}
	if a, perr := netip.ParseAddr(h); perr == nil {
		se.addr = a.Unmap()
		return se, nil
	}
	if err := checkName(h); err != nil {
		return staticEntry{}, fmt.Errorf("%w: endpoint address %q: %w", ErrInvalidSpec, e.Address, err)
	}
	se.host = h
	return se, nil
}

// parsePort parses a decimal port from 1 to 65,535.
func parsePort(p string) (uint16, error) {
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("port %q is not a number from 1 to 65535", p)
	}
	return uint16(n), nil
}

// checkName accepts a DNS name in presentation form: 1 to 253 bytes of
// labels of at most 63 bytes, without spaces or control characters, with
// an optional trailing dot. Underscores are allowed (SRV owner names and
// some internal zones use them).
func checkName(n string) error {
	n = strings.TrimSuffix(n, ".")
	if n == "" || len(n) > maxNameLen {
		return fmt.Errorf("name %q must be 1 to %d bytes", n, maxNameLen)
	}
	for label := range strings.SplitSeq(n, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("name %q has an empty label or one over 63 bytes", n)
		}
		for i := range len(label) {
			c := label[i]
			if !isNameByte(c) {
				return fmt.Errorf("name %q has the invalid byte %q", n, c)
			}
		}
	}
	return nil
}

// isName reports whether checkName accepts n, without allocating.
func isName(n string) bool {
	n = strings.TrimSuffix(n, ".")
	if n == "" || len(n) > maxNameLen {
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
			if label > 63 {
				return false
			}
		default:
			return false
		}
	}
	return label > 0
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// checkPortName accepts an SRV service name: 1 to 15 letters, digits and
// hyphens with at least one letter (RFC 6335 section 5.1).
func checkPortName(n string) error {
	if n == "" || len(n) > 15 {
		return fmt.Errorf("port name %q must be 1 to 15 characters", n)
	}
	letter := false
	for i := range len(n) {
		c := n[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9' || c == '-':
		default:
			return fmt.Errorf("port name %q has the invalid byte %q", n, c)
		}
	}
	if !letter {
		return fmt.Errorf("port name %q needs a letter", n)
	}
	return nil
}
