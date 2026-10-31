// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/netaddr"
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

// Validate reports whether s is a usable source: every static address is
// host:port with a port from 1 to 65,535, and DNS discovery names a service
// and exactly one of a numeric port and a port name (RZ-CFG-005 at
// validation). The address, name and port rules are internal/netaddr's,
// which validation applies too.
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
	if err := netaddr.CheckName(d.Service); err != nil {
		return fmt.Errorf("%w: discovery.service: %w", ErrInvalidSpec, err)
	}
	switch {
	case d.Port != 0 && d.PortName != "":
		return fmt.Errorf("%w: discovery.port is either a number or a name", ErrInvalidSpec)
	case d.Port == 0 && d.PortName == "":
		return fmt.Errorf("%w: discovery.port is required", ErrInvalidSpec)
	case d.PortName != "":
		if err := netaddr.CheckPortName(d.PortName); err != nil {
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
	identity string     // the configured address
	host     string     // DNS name; "" for an IP literal
	addr     netip.Addr // the literal when host is ""
	port     uint16
	weight   uint32
}

// parseStatic splits a static address into its parts (netaddr.ParseHostPort).
func parseStatic(e StaticEndpoint) (staticEntry, error) {
	hp, err := netaddr.ParseHostPort(e.Address)
	if err != nil {
		return staticEntry{}, fmt.Errorf("%w: endpoint address %q: %w", ErrInvalidSpec, e.Address, err)
	}
	return staticEntry{identity: e.Address, host: hp.Host, addr: hp.Addr, port: hp.Port, weight: e.Weight}, nil
}
