// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// finding is one RZ-CFG-005 finding at a path under spec.config.
type finding struct {
	path    diag.Path
	message string
}

// findings returns what Check reports about cfg, in field order: an
// allowOrigins entry that is neither "*" nor a serialized origin; an
// allowMethods, allowHeaders or exposeHeaders entry that is not an RFC
// 9110 token; and allowCredentials true with "*" or "null" in
// allowOrigins, or "*" in allowHeaders or exposeHeaders, wildcards a
// browser refuses for credentialed requests.
func findings(cfg *v1alpha1.CORSConfig) []finding {
	var out []finding
	item := func(field string, v string) diag.Path {
		return diag.Path{diag.Field(field), diag.Item(v)}
	}
	for _, o := range cfg.AllowOrigins {
		if o != wildcard && !serializedOrigin(o) {
			out = append(out, finding{item("allowOrigins", o), `neither "*" nor a serialized origin such as https://shop.example`})
		}
	}
	for _, list := range []struct {
		field, what string
		entries     []string
	}{
		{"allowMethods", "a method", cfg.AllowMethods},
		{"allowHeaders", "a field name", cfg.AllowHeaders},
		{"exposeHeaders", "a field name", cfg.ExposeHeaders},
	} {
		for _, v := range list.entries {
			if !httpfield.IsToken(v) {
				out = append(out, finding{item(list.field, v), "not " + list.what})
			}
		}
	}
	if cfg.AllowCredentials == nil || !*cfg.AllowCredentials {
		return out
	}
	for _, c := range []struct {
		field, value string
		list         []string
	}{
		{"allowOrigins", wildcard, cfg.AllowOrigins},
		{"allowOrigins", "null", cfg.AllowOrigins},
		{"allowHeaders", wildcard, cfg.AllowHeaders},
		{"exposeHeaders", wildcard, cfg.ExposeHeaders},
	} {
		if slices.Contains(c.list, c.value) {
			out = append(out, finding{diag.Path{diag.Field("allowCredentials")}, `true with "` + c.value + `" in ` + c.field})
		}
	}
	return out
}

// Check is the offline check of a cors Policy (a hub.PolicyCheck run at
// stage H by every binary): every finding Build refuses, each RZ-CFG-005
// at its path under spec.config. A resource whose Config is not a decoded
// CORSConfig yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.CORSConfig)
	if !ok || cfg == nil {
		return
	}
	for _, f := range findings(cfg) {
		p := append(diag.Path{diag.Field("spec"), diag.Field("config")}, f.path...)
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     CodeConfig,
			Severity: diag.SeverityError,
			Location: l,
			Resource: r.ResourceID(),
			Path:     p,
			Message:  f.message,
		})
	}
}

// serializedOrigin reports whether s is a serialized origin as browsers
// send it in Origin (the HTML Standard's serialization of an origin):
// "null", or scheme "://" host [":" port] with a lower-case scheme, a
// lower-case ASCII host (validHost, or a bracketed IPv6 address in its
// canonical compressed form), and a decimal port without leading zeros
// that is not the scheme's default; nothing may follow.
func serializedOrigin(s string) bool {
	if s == "null" {
		return true
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || !validScheme(scheme) || rest == "" {
		return false
	}
	host, port := rest, ""
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return false
		}
		host, port = rest[:end+1], rest[end+1:]
		if !canonicalIPv6(host[1 : len(host)-1]) {
			return false
		}
	} else {
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			host, port = rest[:i], rest[i:]
		}
		if !validHost(host) {
			return false
		}
	}
	if port == "" {
		return true
	}
	digits, ok := strings.CutPrefix(port, ":")
	if !ok || digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return false
	}
	n, err := strconv.ParseUint(digits, 10, 16)
	return err == nil && n != defaultPort(scheme)
}

// validScheme reports an RFC 3986 scheme in lower case.
func validScheme(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLowerAlnum(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// isLowerAlnum reports a lower-case ASCII letter or a digit.
func isLowerAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// defaultPort returns the default port of the URL Standard's special
// schemes with a port (http, https, ws, wss, ftp); 65536 otherwise, which
// no port equals.
func defaultPort(scheme string) uint64 {
	switch scheme {
	case "http", "ws":
		return 80
	case "https", "wss":
		return 443
	case "ftp":
		return 21
	default:
		return 1 << 16
	}
}

// validHost reports a serialized domain or IPv4 host as a browser can
// reach it: labels of lower-case ASCII letters, digits, "-" and "_"
// (internationalized names in their xn-- form), separated by single dots;
// a host whose last label is a number (decimal, or hex after 0x) is a
// canonical dotted IPv4 address, as browsers serialize such a host. A
// "*" label, which no Origin field carries, is refused.
func validHost(h string) bool {
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return false
		}
	}
	for i := range len(h) {
		if c := h[i]; !isLowerAlnum(c) && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	if !isNumber(h[strings.LastIndexByte(h, '.')+1:]) {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Is4() && a.String() == h
}

// isNumber reports a label the URL Standard's host parser reads as a
// number: decimal digits, or "0x" followed by hex digits.
func isNumber(label string) bool {
	if hex, ok := strings.CutPrefix(label, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return strings.Trim(label, "0123456789") == ""
}

// canonicalIPv6 reports an IPv6 address written as the URL Standard
// serializes it: lower-case hex pieces without leading zeros, the first
// longest run of two or more zero pieces compressed to "::", no zone and
// no embedded IPv4 notation.
func canonicalIPv6(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is6() || a.Zone() != "" {
		return false
	}
	b := a.As16()
	var pieces [8]uint16
	for i := range pieces {
		pieces[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	start, length := -1, 0
	for i := 0; i < 8; {
		if pieces[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && pieces[j] == 0 {
			j++
		}
		if j-i > length && j-i > 1 {
			start, length = i, j-i
		}
		i = j
	}
	var out strings.Builder
	for i := 0; i < 8; i++ {
		if i == start {
			out.WriteString("::")
			i += length - 1
			continue
		}
		if out.Len() > 0 && !strings.HasSuffix(out.String(), ":") {
			out.WriteByte(':')
		}
		out.WriteString(strconv.FormatUint(uint64(pieces[i]), 16))
	}
	return out.String() == s
}
