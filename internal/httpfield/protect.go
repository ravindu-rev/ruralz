// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import "strconv"

// Protection is the class of a field name the Node manages itself. No
// headers operation and no transform set or remove entry with target header
// may name a protected field (RZ-CFG-005), and the M2 Plugin host refuses
// writes to them (WASM "Host Function rules", Message writes).
type Protection uint8

// Protection classes.
const (
	// Unprotected is every other name, including the forwarding
	// fields, content-type and set-cookie.
	Unprotected Protection = iota
	// Pseudo is an HTTP/2 or HTTP/3 pseudo-header name (":"-prefixed).
	Pseudo
	// HostField is host: the Upstream layer sets Host from the
	// Endpoint.
	HostField
	// HopByHop is one of the fixed hop-by-hop fields (see IsHopByHop).
	HopByHop
	// Framing is content-length: framing follows the body.
	Framing
	// ExpectField is expect, dropped toward Upstreams.
	ExpectField
	// TraceContext is traceparent or tracestate, injected per
	// attempt.
	TraceContext
)

// String returns the class name used in diagnostics.
func (p Protection) String() string {
	switch p {
	case Unprotected:
		return "unprotected"
	case Pseudo:
		return "pseudo-header"
	case HostField:
		return "host"
	case HopByHop:
		return "hop-by-hop"
	case Framing:
		return "framing"
	case ExpectField:
		return "expect"
	case TraceContext:
		return "trace context"
	default:
		return "Protection(" + strconv.Itoa(int(p)) + ")"
	}
}

// Message returns the diagnostic text for writing the protected field name,
// such as "header traceparent is managed by the Node (trace context)", or ""
// when p is Unprotected. The name is lowercased; it is operator-authored
// configuration, never request content.
func (p Protection) Message(name string) string {
	if p == Unprotected {
		return ""
	}
	b := make([]byte, 0, len("header ")+len(name)+len(" is managed by the Node (")+len("trace context)"))
	b = append(b, "header "...)
	for i := range len(name) {
		c := name[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	b = append(b, " is managed by the Node ("...)
	b = append(b, p.String()...)
	b = append(b, ')')
	return string(b)
}

// Protect returns the protection class of a field name, compared without
// regard to ASCII case. It never allocates.
func Protect(name string) Protection {
	if name != "" && name[0] == ':' {
		return Pseudo
	}
	switch len(name) {
	case 2:
		if lowerEq(name, "te") {
			return HopByHop
		}
	case 4:
		if lowerEq(name, "host") {
			return HostField
		}
	case 6:
		if lowerEq(name, "expect") {
			return ExpectField
		}
	case 7:
		if lowerEq(name, "trailer") || lowerEq(name, "upgrade") {
			return HopByHop
		}
	case 10:
		if lowerEq(name, "connection") || lowerEq(name, "keep-alive") {
			return HopByHop
		}
		if lowerEq(name, "tracestate") {
			return TraceContext
		}
	case 11:
		if lowerEq(name, "traceparent") {
			return TraceContext
		}
	case 14:
		if lowerEq(name, "content-length") {
			return Framing
		}
	case 16:
		if lowerEq(name, "proxy-connection") {
			return HopByHop
		}
	case 17:
		if lowerEq(name, "transfer-encoding") {
			return HopByHop
		}
	case 18:
		if lowerEq(name, "proxy-authenticate") {
			return HopByHop
		}
	case 19:
		if lowerEq(name, "proxy-authorization") {
			return HopByHop
		}
	}
	return Unprotected
}

// IsHopByHop reports whether name is one of the fixed hop-by-hop fields,
// compared without regard to ASCII case: Connection, Keep-Alive,
// Proxy-Connection, TE, Trailer, Transfer-Encoding, Upgrade,
// Proxy-Authenticate and Proxy-Authorization (RFC 9110 section 7.6.1).
// Fields named by a Connection field are hop-by-hop too; see
// [ConnectionOptions] and [StripHopByHop]. It never allocates.
func IsHopByHop(name string) bool {
	return Protect(name) == HopByHop
}

// IsForwardableTrailer reports whether a trailer field may cross the Node
// to the next hop, compared without regard to ASCII case: every field but
// the hop-by-hop ones ([IsHopByHop]) and the framing fields
// Transfer-Encoding, Content-Length and Trailer, which a trailer section
// never carries forward (docs/architecture/03-data-plane.md "Upstream
// layer"; docs/architecture/07-multi-protocol.md "HTTP intermediary
// behavior"). It applies to an Upstream's response trailers and to a
// client's request trailers alike, and never allocates.
func IsForwardableTrailer(name string) bool {
	return name != "" && !IsHopByHop(name) && !lowerEq(name, "content-length")
}
