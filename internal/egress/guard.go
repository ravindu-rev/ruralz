// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package egress is the SSRF guard for every Node connection made for the
// Bundle: jwksUrl and tokenUrl fetches, Upstream static Endpoints, DNS
// discovery results and active health probes (Security and identity,
// "Secrets" rule 6; spec 06 requirements 84 to 86).
//
// A Guard refuses, at connect time (after DNS resolution, in
// net.Dialer.Control) and on each redirect hop, destinations in 0.0.0.0/8,
// ::/128, 127.0.0.0/8, ::1/128, 169.254.0.0/16, fe80::/10 and
// fd00:ec2::254/128, their IPv4-mapped (::ffff:0:0/96) and NAT64
// (64:ff9b::/96, 64:ff9b:1::/48, low 32 bits checked as IPv4) forms and the
// IPv4-compatible range ::/96, unless the address is inside
// RURALZ_FETCH_ALLOW. RFC 1918 and other addresses stay reachable. State
// Store and OTLP connections are not Bundle destinations and do not use the
// guard.
//
// The guarded HTTP client (Client) sends no cookies, never follows https
// to http, re-checks every redirect hop, caps response bodies and, for a
// destination-bound secret (auth.upstream-oauth2 clientSecret, R-49),
// follows no redirect at all and refuses any other origin.
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EnvFetchAllow is the process setting holding the allow list (spec 06
// requirement 85).
const EnvFetchAllow = "RURALZ_FETCH_ALLOW"

// EnvProxyEntry is the reserved RURALZ_FETCH_ALLOW entry that opts the
// guarded HTTP client into HTTPS_PROXY, HTTP_PROXY and NO_PROXY.
const EnvProxyEntry = "env-proxy"

// DefaultDialTimeout bounds one connect when a caller gives no timeout.
const DefaultDialTimeout = 5 * time.Second

// maxEntryEcho bounds how much of a bad entry an error message repeats.
const maxEntryEcho = 64

// Sentinel errors. Callers map them to their own failure: a JWKS fetch
// failure (spec 06 requirement 36), RZ-AUTH-020 for a token fetch, or
// RZ-UP-001 for an Upstream connect (spec 06 requirement 86).
var (
	// ErrDenied reports a destination address the guard refuses.
	ErrDenied = errors.New("egress: destination address denied")
	// ErrAllowEntry reports an unparsable RURALZ_FETCH_ALLOW entry;
	// ruralzd refuses to start (exit 2).
	ErrAllowEntry = errors.New("egress: invalid " + EnvFetchAllow + " entry")
)

// Guard decides which destination addresses a Node may connect to. The
// zero value and a nil *Guard allow nothing beyond the default policy (no
// allow entries, proxy variables ignored). A Guard is immutable after
// ParseAllow and safe for concurrent use.
type Guard struct {
	// allow holds the RURALZ_FETCH_ALLOW prefixes, masked, IPv4-mapped
	// prefixes stored in IPv4 form.
	allow []netip.Prefix
	// envProxy is true when the env-proxy entry is present.
	envProxy bool
	// proxy selects the proxy of a request when envProxy is true
	// (http.ProxyFromEnvironment; replaced by in-package tests).
	proxy func(*http.Request) (*url.URL, error)
	// resolver, when set, replaces the default resolver of Dialer
	// (in-package tests use it for DNS rebinding cases).
	resolver *net.Resolver
}

// ParseAllow parses a RURALZ_FETCH_ALLOW value: comma-separated entries,
// each an IP address, a CIDR prefix or the reserved entry env-proxy
// (spec 06 requirement 85). Spaces around entries are ignored and an
// empty value allows nothing. Any other entry, an empty entry and an
// address with a zone are errors wrapping ErrAllowEntry; the message
// names the setting and the entry position.
func ParseAllow(v string) (*Guard, error) {
	g := &Guard{}
	if strings.TrimSpace(v) == "" {
		return g, nil
	}
	for i, raw := range strings.Split(v, ",") {
		e := strings.TrimSpace(raw)
		if e == EnvProxyEntry {
			g.envProxy = true
			g.proxy = http.ProxyFromEnvironment
			continue
		}
		p, err := parseEntry(e)
		if err != nil {
			return nil, fmt.Errorf("%w: %s entry %d (%q): %w", ErrAllowEntry, EnvFetchAllow, i+1, echo(e), err)
		}
		g.allow = append(g.allow, p)
	}
	return g, nil
}

// FromEnv reads RURALZ_FETCH_ALLOW through lookup (os.LookupEnv in
// ruralzd) and parses it; an unset variable allows nothing.
func FromEnv(lookup func(string) (string, bool)) (*Guard, error) {
	v, ok := lookup(EnvFetchAllow)
	if !ok {
		return &Guard{}, nil
	}
	return ParseAllow(v)
}

// Fixed entry errors: the parser's own messages repeat the whole entry,
// which echo already bounds.
var (
	errEmptyEntry = errors.New("empty entry")
	errZone       = errors.New("addresses with a zone are not allowed")
	errPrefix     = errors.New("not a CIDR prefix")
	errAddr       = errors.New("not an IP address, a CIDR prefix or " + EnvProxyEntry)
)

// parseEntry parses one IP or CIDR entry into a masked prefix; IPv4-mapped
// forms become IPv4 prefixes so checks compare unmapped addresses.
func parseEntry(e string) (netip.Prefix, error) {
	if e == "" {
		return netip.Prefix{}, errEmptyEntry
	}
	var p netip.Prefix
	if strings.Contains(e, "/") {
		q, err := netip.ParsePrefix(e)
		if err != nil {
			if strings.Contains(e, "%") {
				return netip.Prefix{}, errZone
			}
			return netip.Prefix{}, errPrefix
		}
		p = q
	} else {
		a, err := netip.ParseAddr(e)
		if err != nil {
			return netip.Prefix{}, errAddr
		}
		if a.Zone() != "" {
			return netip.Prefix{}, errZone
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	p = p.Masked()
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p, nil
}

// echo shortens an entry for an error message.
func echo(e string) string {
	if len(e) > maxEntryEcho {
		return e[:maxEntryEcho] + "..."
	}
	return e
}

// EnvProxy reports whether the env-proxy entry is present, so the guarded
// HTTP client honors HTTPS_PROXY, HTTP_PROXY and NO_PROXY; the guard then
// checks the proxy's address.
func (g *Guard) EnvProxy() bool { return g != nil && g.envProxy }

// Prefixes returns a copy of the allow prefixes in entry order.
func (g *Guard) Prefixes() []netip.Prefix {
	if g == nil {
		return nil
	}
	return append([]netip.Prefix(nil), g.allow...)
}

// String returns the canonical allow list (masked prefixes, then
// env-proxy), which ParseAllow accepts.
func (g *Guard) String() string {
	if g == nil {
		return ""
	}
	parts := make([]string, 0, len(g.allow)+1)
	for _, p := range g.allow {
		parts = append(parts, p.String())
	}
	if g.envProxy {
		parts = append(parts, EnvProxyEntry)
	}
	return strings.Join(parts, ",")
}

// inAllow reports whether a (unmapped, without zone) lies in an allow
// prefix.
func (g *Guard) inAllow(a netip.Addr) bool {
	if g == nil {
		return false
	}
	for _, p := range g.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether the guard admits a connection to a: inside
// RURALZ_FETCH_ALLOW, or outside every denied range.
func (g *Guard) Allowed(a netip.Addr) bool { return g.Check(a) == nil }

// Check returns nil when the guard admits a, else an error wrapping
// ErrDenied that names the address (never a secret). An invalid address
// is denied.
func (g *Guard) Check(a netip.Addr) error {
	if !a.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrDenied)
	}
	a = a.WithZone("").Unmap()
	if g.inAllow(a) {
		return nil
	}
	if Denied(a) {
		return fmt.Errorf("%w: %s is not in %s", ErrDenied, a, EnvFetchAllow)
	}
	return nil
}

// Control is a net.Dialer Control function: it runs after DNS resolution
// for every connect attempt and refuses denied addresses, so a name that
// re-resolves to a loopback or metadata address (DNS rebinding) is refused
// too. An address that is not ip:port (a Unix socket path) is denied.
func (g *Guard) Control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP destination", ErrDenied, address)
	}
	return g.Check(ap.Addr())
}

// Dialer returns a net.Dialer whose Control is the guard; timeout bounds
// each connect (DefaultDialTimeout when 0 or negative). The Upstream
// layer uses it for Endpoints, discovery results and probes; the guarded
// HTTP client uses it for IdP fetches.
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	d := &net.Dialer{Timeout: timeout, Control: g.Control}
	if g != nil && g.resolver != nil {
		d.Resolver = g.resolver
	}
	return d
}

// CheckHost checks a URL host (without port) that is an IP literal, so a
// redirect or request to a denied literal fails before any connect; a DNS
// name returns nil and is checked at connect time by Control.
func (g *Guard) CheckHost(host string) error {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, err := netip.ParseAddr(host); err == nil {
		return g.Check(a)
	}
	return nil
}

// Denied reports whether a lies in the default deny table of spec 06
// requirement 84, before RURALZ_FETCH_ALLOW is applied. It is a pure
// function, so static checks (ruralz bundle audit, M2) can call it.
func Denied(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return deniedV4(a)
	}
	b := a.As16()
	for _, p := range deniedV6() {
		if p.Contains(a) {
			return true
		}
	}
	for _, p := range nat64() {
		if p.Contains(a) {
			return deniedV4(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		}
	}
	return false
}

// deniedV4 reports an IPv4 address in 0.0.0.0/8, 127.0.0.0/8 or
// 169.254.0.0/16.
func deniedV4(a netip.Addr) bool {
	b := a.As4()
	return b[0] == 0 || b[0] == 127 || (b[0] == 169 && b[1] == 254)
}

// deniedV6 lists the IPv6 ranges refused outright: ::/96 (IPv4-compatible,
// covering ::/128 and ::1/128), fe80::/10 and the metadata address
// fd00:ec2::254/128.
func deniedV6() [3]netip.Prefix {
	return [3]netip.Prefix{
		netip.PrefixFrom(netip.IPv6Unspecified(), 96),
		netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfe, 0x80}), 10),
		netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfd, 0x00, 0x0e, 0xc2, 14: 0x02, 15: 0x54}), 128),
	}
}

// nat64 lists the NAT64 prefixes whose low 32 bits are checked as IPv4:
// 64:ff9b::/96 (RFC 6052) and 64:ff9b:1::/48 (RFC 8215).
func nat64() [2]netip.Prefix {
	return [2]netip.Prefix{
		netip.PrefixFrom(netip.AddrFrom16([16]byte{0x00, 0x64, 0xff, 0x9b}), 96),
		netip.PrefixFrom(netip.AddrFrom16([16]byte{0x00, 0x64, 0xff, 0x9b, 0x00, 0x01}), 48),
	}
}

// Origin returns the origin of u as "scheme://host:port" with the scheme
// and host lowercased and the default port made explicit (443 for https,
// 80 for http), the form hub.SecretUse.Destination uses for a
// destination-bound secret (R-49).
func Origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	} else if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(n, 10)
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
