// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// MaxEntries is the number of rightmost forwarding entries Resolve examines
// (04 req 17, 06 req 60: 16, target).
const MaxEntries = 16

// Canonical forwarding field names.
const (
	headerForwarded       = "Forwarded"
	headerXForwardedFor   = "X-Forwarded-For"
	headerXForwardedProto = "X-Forwarded-Proto"
	headerXForwardedHost  = "X-Forwarded-Host"
)

// Via says where Result.IP came from.
type Via uint8

// Via values.
const (
	// ViaPeer: the connection peer (PROXY v2 source or TCP peer).
	ViaPeer Via = iota
	// ViaForwarded: a Forwarded for= entry behind a trusted peer.
	ViaForwarded
	// ViaXForwardedFor: an X-Forwarded-For entry behind a trusted peer.
	ViaXForwardedFor
)

// String returns peer, forwarded or x-forwarded-for.
func (v Via) String() string {
	switch v {
	case ViaPeer:
		return "peer"
	case ViaForwarded:
		return "forwarded"
	case ViaXForwardedFor:
		return "x-forwarded-for"
	}
	return "Via(" + strconv.Itoa(int(v)) + ")"
}

// Result is the client address of one request.
type Result struct {
	// IP is source.ip: IPv4-mapped IPv6 unmapped, no zone. It is invalid
	// when the peer is unknown (not a TCP connection) and no header
	// supplied an address.
	IP netip.Addr
	// Port is source.port: the peer (or PROXY v2 source) port, 0 when IP
	// came from a forwarding header (06 req 62).
	Port uint16
	// Peer is the connection peer after PROXY v2 handling, unmapped and
	// without zone.
	Peer netip.AddrPort
	// PeerTrusted is true when Peer is inside the trusted set; the
	// forwarding-header rewrite appends instead of overwriting then.
	PeerTrusted bool
	// Via says which input supplied IP.
	Via Via
}

// FromHeader reports whether IP came from Forwarded or X-Forwarded-For.
func (r Result) FromHeader() bool { return r.Via != ViaPeer }

// Resolve computes source.ip and source.port for a request whose
// connection peer is peer (the PROXY v2 source on opted-in listeners, else
// the TCP peer) and whose client request header is h (04 req 17, 06 reqs 60
// and 62). The rules are in the package documentation. Resolve examines at
// most MaxEntries entries. It allocates nothing for well-formed entries
// written without backslash escapes. Each Forwarded for value that holds a
// quoted-pair costs one allocation (the unescaped copy). The entry that
// ends the walk because netip.ParseAddr rejects it (deadbeef, 1.1.1.1.1)
// costs one more, the ParseAddr error.
func (t *Trusted) Resolve(peer netip.AddrPort, h http.Header) Result {
	peer = normalizeAddrPort(peer)
	r := Result{IP: peer.Addr(), Port: peer.Port(), Peer: peer, Via: ViaPeer}
	if !t.Contains(peer.Addr()) {
		return r
	}
	r.PeerTrusted = true
	if fwd := h[headerForwarded]; hasContent(fwd) {
		if ip, ok := t.walkForwarded(fwd); ok {
			r.IP, r.Port, r.Via = ip, 0, ViaForwarded
		}
		return r
	}
	if ip, ok := t.walkXFF(h[headerXForwardedFor]); ok {
		r.IP, r.Port, r.Via = ip, 0, ViaXForwardedFor
	}
	return r
}

// normalizeAddrPort unmaps the address and drops its zone.
func normalizeAddrPort(p netip.AddrPort) netip.AddrPort {
	if !p.Addr().IsValid() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(p.Addr().Unmap().WithZone(""), p.Port())
}

// hasContent reports whether any field line holds more than whitespace.
func hasContent(lines []string) bool {
	for _, l := range lines {
		if trimOWS(l) != "" {
			return true
		}
	}
	return false
}

// walker applies the right-to-left walk to entries fed newest first.
type walker struct {
	t      *Trusted
	last   netip.Addr
	parsed bool
	n      int
	done   bool
	result netip.Addr
	found  bool
}

// entry feeds one entry: ok false marks it unparsable, empty an empty list
// element. It reports whether the walk goes on.
func (w *walker) entry(a netip.Addr, ok, empty bool) bool {
	w.n++
	switch {
	case empty:
	case !ok:
		w.done = true
		return false
	case !w.t.Contains(a):
		w.result, w.found, w.done = a, true, true
		return false
	default:
		w.last, w.parsed = a, true
	}
	if w.n >= MaxEntries {
		w.done = true
		return false
	}
	return true
}

// outcome returns the client address the walk settled on.
func (w *walker) outcome() (netip.Addr, bool) {
	if w.found {
		return w.result, true
	}
	return w.last, w.parsed
}

// walkXFF walks X-Forwarded-For field lines right to left.
func (t *Trusted) walkXFF(lines []string) (netip.Addr, bool) {
	w := walker{t: t}
	for li := len(lines) - 1; li >= 0 && !w.done; li-- {
		s := lines[li]
		for end := len(s); ; {
			start := strings.LastIndexByte(s[:end], ',') + 1
			e := trimOWS(s[start:end])
			a, ok := parseXFFEntry(e)
			if !w.entry(a, ok, e == "") || start == 0 {
				break
			}
			end = start - 1
		}
	}
	return w.outcome()
}

// parseXFFEntry parses one X-Forwarded-For entry: an IPv4 or IPv6 address,
// optionally with a port (IPv6 then in brackets) or a zone.
func parseXFFEntry(e string) (netip.Addr, bool) {
	return parseHost(e, false)
}

// parseHost parses IPv4, IPv4:port, IPv6 (bare, zone allowed), [IPv6] or
// [IPv6]:port; obf admits RFC 7239 obfuscated ports. The result is
// unmapped and without zone.
func parseHost(v string, obf bool) (netip.Addr, bool) {
	if v == "" {
		return netip.Addr{}, false
	}
	if v[0] == '[' {
		inner, rest, ok := strings.Cut(v[1:], "]")
		if !ok || rest != "" && (rest[0] != ':' || !validPort(rest[1:], obf)) {
			return netip.Addr{}, false
		}
		a, ok := parseIP(inner)
		if !ok || !a.Is6() {
			return netip.Addr{}, false
		}
		return clean(a)
	}
	if host, port, ok := strings.Cut(v, ":"); ok && strings.IndexByte(port, ':') < 0 {
		// Exactly one colon: an IPv6 address has at least two, so this
		// is IPv4 with a port.
		a, ok := parseIP(host)
		if !ok || !a.Is4() || !validPort(port, obf) {
			return netip.Addr{}, false
		}
		return a, true
	}
	a, ok := parseIP(v)
	if !ok {
		return netip.Addr{}, false
	}
	return clean(a)
}

// parseIP parses an address once its characters are plausible, so that
// words such as "unknown" never reach netip.ParseAddr, whose error
// allocates. A zone (plausibleIP has checked it is non-empty) is cut off
// before parsing, because clean drops it anyway and netip.ParseAddr
// interns every zone it parses, which allocates for each zone the process
// has not seen. A zone is accepted only on an IPv6 address, so 1.2.3.4%x
// stays unparsable.
func parseIP(s string) (netip.Addr, bool) {
	if !plausibleIP(s) {
		return netip.Addr{}, false
	}
	addr, _, zoned := strings.Cut(s, "%")
	a, err := netip.ParseAddr(addr)
	if err != nil || zoned && !a.Is6() {
		return netip.Addr{}, false
	}
	return a, true
}

// plausibleIP reports whether s is made of hex digits, dots and colons up
// to an optional non-empty zone.
func plausibleIP(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '%':
			return i > 0 && i < len(s)-1
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// clean unmaps a and drops its zone.
func clean(a netip.Addr) (netip.Addr, bool) {
	return a.Unmap().WithZone(""), true
}

// validPort accepts 1-5 digits up to 65535, or with obf an RFC 7239
// obfuscated port ("_" followed by ALPHA, DIGIT, ".", "_" or "-").
func validPort(p string, obf bool) bool {
	if obf && len(p) > 1 && p[0] == '_' {
		for i := 1; i < len(p); i++ {
			if !isObfChar(p[i]) {
				return false
			}
		}
		return true
	}
	if p == "" || len(p) > 5 {
		return false
	}
	v := 0
	for i := range len(p) {
		c := p[i]
		if c < '0' || c > '9' {
			return false
		}
		v = v*10 + int(c-'0')
	}
	return v <= 65535
}

// isObfChar reports whether c may follow "_" in an obfuscated identifier.
func isObfChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// walkForwarded walks Forwarded (RFC 7239) field lines right to left.
// Each line is split into elements from its right end (prevElement), so
// an element a trusted proxy appended stays intact whatever the
// client-supplied text to its left contains, and at most MaxEntries
// elements are delimited.
func (t *Trusted) walkForwarded(lines []string) (netip.Addr, bool) {
	w := walker{t: t}
	for li := len(lines) - 1; li >= 0 && !w.done; li-- {
		s := lines[li]
		for end := len(s); ; {
			start, bad := prevElement(s, end)
			e := trimOWS(s[start:end])
			var a netip.Addr
			ok := false
			if !bad && e != "" {
				a, ok = parseForwardedElement(e)
			}
			if !w.entry(a, ok, e == "" && !bad) || start == 0 {
				break
			}
			end = start - 1
		}
	}
	return w.outcome()
}

// prevElement returns the start of the Forwarded element of line s that
// ends at end (exclusive): the byte after the nearest "," left of end that
// lies outside quoted strings, else 0. The scan runs right to left; a '"'
// preceded by an odd run of backslashes is a quoted-pair and does not
// toggle quoting, which delimits well-formed elements exactly as a left to
// right parse does. bad reports an element that reaches the start of the
// line inside a quoted string: only the leftmost element of a line with an
// unterminated quoted string is unparsable, so elements a trusted proxy
// appended to the client's text (or an intermediary merged into the same
// line, RFC 9110 section 5.3) keep their boundaries (06 req 60, 04 req
// 17).
func prevElement(s string, end int) (start int, bad bool) {
	quoted := false
	for i := end - 1; i >= 0; i-- {
		switch s[i] {
		case '"':
			if !escapedAt(s, i) {
				quoted = !quoted
			}
		case ',':
			if !quoted {
				return i + 1, false
			}
		}
	}
	return 0, quoted
}

// escapedAt reports whether s[i] is preceded by an odd run of backslashes,
// that is, whether it is the second byte of a quoted-pair.
func escapedAt(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// parseForwardedElement returns the address of the element's single for
// parameter; any syntax error, a missing or repeated for, or a for value
// that is not an address (unknown, an obfuscated identifier) is unparsable.
func parseForwardedElement(e string) (netip.Addr, bool) {
	var forVal string
	var forQuoted, seen bool
	for e != "" {
		pair, rest, err := nextPair(e)
		if err {
			return netip.Addr{}, false
		}
		e = rest
		pair = trimOWS(pair)
		if pair == "" {
			continue
		}
		name, val, ok := strings.Cut(pair, "=")
		if !ok || !isToken(name) || val == "" {
			return netip.Addr{}, false
		}
		quoted := val[0] == '"'
		if quoted {
			if len(val) < 2 || val[len(val)-1] != '"' {
				return netip.Addr{}, false
			}
			val = val[1 : len(val)-1]
		} else if !isLenientToken(val) {
			return netip.Addr{}, false
		}
		if strings.EqualFold(name, "for") {
			if seen {
				return netip.Addr{}, false
			}
			forVal, forQuoted, seen = val, quoted, true
		}
	}
	if !seen {
		return netip.Addr{}, false
	}
	if forQuoted && strings.IndexByte(forVal, '\\') >= 0 {
		forVal = unescape(forVal)
	}
	return parseNode(forVal)
}

// nextPair splits e at the first ";" outside a quoted string; err reports
// a quote closed before the pair's end.
func nextPair(e string) (pair, rest string, err bool) {
	quoted, escaped := false, false
	for i := 0; i < len(e); i++ {
		c := e[i]
		switch {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case !quoted && c == ';':
			return e[:i], e[i+1:], false
		}
	}
	if quoted {
		return "", "", true
	}
	return e, "", false
}

// unescape removes quoted-pair backslashes from a quoted-string body.
func unescape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseNode parses an RFC 7239 node: IPv4 or bracketed IPv6, each with an
// optional port; "unknown" and obfuscated identifiers are unparsable. A
// bare IPv6 address is accepted leniently.
func parseNode(v string) (netip.Addr, bool) {
	if v == "" || v[0] == '_' || strings.EqualFold(v, "unknown") {
		return netip.Addr{}, false
	}
	return parseHost(v, true)
}

// isToken reports whether s is an RFC 9110 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isTchar(s[i]) {
			return false
		}
	}
	return true
}

// isTchar reports whether c is an RFC 9110 tchar.
func isTchar(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// isLenientToken reports whether v is a token, or one of the unquoted
// values senders commonly emit although RFC 7239 requires quoting them (an
// IPv4 address with a port, a bracketed IPv6 address, a host with a port).
func isLenientToken(v string) bool {
	for i := range len(v) {
		c := v[i]
		if !isTchar(c) && c != ':' && c != '[' && c != ']' {
			return false
		}
	}
	return true
}

// trimOWS trims optional whitespace (SP and HTAB).
func trimOWS(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
