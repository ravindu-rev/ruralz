// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"encoding/binary"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/tetratelabs/wazero/api"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/redact"
)

// hostLog is log(level, p, l): one guest log line at level.
func hostLog(c *call, m api.Memory, stack []uint64) int32 {
	level, p, l := s32(stack), u32(stack, 1), u32(stack, 2)
	if level < levelDebug || level > levelError {
		return errInvalid
	}
	checkRange(m, p, l)
	n := min(l, uint32(logMessageBytes+1))
	c.guestLog(level, mem(m, p, n), uint64(l))
	return 0
}

// hostClockNow is clock_now(out): the host clock in Unix nanoseconds, an
// i64 written at out.
func hostClockNow(c *call, m api.Memory, stack []uint64) int32 {
	out := u32(stack, 0)
	checkRange(m, out, 8)
	m.WriteUint64Le(out, uint64(c.e.clk.Now().UnixNano()))
	return 0
}

// hostRandomBytes is random_bytes(o, c): c bytes from the host random
// source.
func hostRandomBytes(c *call, m api.Memory, stack []uint64) int32 {
	o, n := u32(stack, 0), u32(stack, 1)
	checkRange(m, o, n)
	b := make([]byte, n)
	c.e.randomBytes(b)
	if n > 0 {
		m.Write(o, b)
	}
	return lenResult(int(n))
}

// hostRequestInfo is request_info(field, o, c): the method, scheme, host,
// path, query, source address or Route of the request. In upstream-leg
// Phases the path and query are the attempt's outgoing request's.
func hostRequestInfo(c *call, m api.Memory, stack []uint64) int32 {
	field, o, n := s32(stack), u32(stack, 1), u32(stack, 2)
	var v string
	switch field {
	case requestMethod:
		v = c.x.Method()
	case requestScheme:
		v = c.x.Scheme()
	case requestHost:
		v = c.x.Host()
	case requestPath:
		v = c.x.Path()
		if leg := c.x.Leg(); leg != nil && leg.Request != nil && leg.Request.URL != nil {
			v = leg.Request.URL.Path
		}
	case requestQuery:
		v = c.x.Received().RawQuery
		if leg := c.x.Leg(); leg != nil && leg.Request != nil && leg.Request.URL != nil {
			v = leg.Request.URL.RawQuery
		} else if msg := c.x.Message(); msg != nil && c.ph.CanShortCircuit() {
			v = msg.RawQuery()
		}
	case requestSource:
		if ip := c.x.Source().IP; ip.IsValid() {
			v = ip.String()
		}
	case requestRoute:
		if r := c.x.Route(); r != nil {
			v = r.Name
		}
	default:
		return errInvalid
	}
	return output(m, o, n, []byte(v))
}

// requestHeader returns the request header of the Phase: the attempt's
// outgoing request in upstream-leg Phases, else the client request.
func (c *call) requestHeader() http.Header {
	if leg := c.x.Leg(); leg != nil && leg.Request != nil {
		return leg.Request.Header
	}
	return c.x.Header()
}

// responseHeader returns the response header of the Phase: the leg's
// response in upstream-leg Phases, the client response from onResponse.
func (c *call) responseHeader() http.Header {
	if msg := c.x.Message(); msg != nil {
		return msg.Header()
	}
	return nil
}

// hidden reports whether reads hide header name: a credential header of
// the Revision without credentials.read, and authorization in an
// upstream-leg Phase whose leg runs an upstream-auth Filter, with it too.
func (c *call) hidden(name string) bool {
	if !c.b.creds.Credential(name) {
		return false
	}
	if c.x.Leg() != nil && c.b.hideAuthorization && strings.EqualFold(name, redact.Authorization) {
		return true
	}
	return !c.b.caps.has(capCredentialsRead)
}

// headerValue returns the field lines of name in h joined with ", ", and
// whether any exists.
func headerValue(h http.Header, name string) (string, bool) {
	var vs []string
	for k, lines := range h {
		if strings.EqualFold(k, name) {
			vs = append(vs, lines...)
		}
	}
	if len(vs) == 0 {
		return "", false
	}
	return strings.Join(vs, ", "), true
}

// getHeader is request_header_get and response_header_get.
func (c *call) getHeader(h http.Header, m api.Memory, stack []uint64) int32 {
	np, nl, o, n := u32(stack, 0), u32(stack, 1), u32(stack, 2), u32(stack, 3)
	name := memString(m, np, nl)
	checkRange(m, o, n)
	if !httpfield.IsToken(name) {
		return errInvalid
	}
	if c.hidden(name) {
		return errAbsent
	}
	v, ok := headerValue(h, name)
	if !ok {
		return errAbsent
	}
	return output(m, o, n, []byte(v))
}

// hostRequestHeaderGet is request_header_get(name, o, c).
func hostRequestHeaderGet(c *call, m api.Memory, stack []uint64) int32 {
	return c.getHeader(c.requestHeader(), m, stack)
}

// hostResponseHeaderGet is response_header_get(name, o, c).
func hostResponseHeaderGet(c *call, m api.Memory, stack []uint64) int32 {
	return c.getHeader(c.responseHeader(), m, stack)
}

// hostRequestHeadersList is request_headers_list(o, c): every field line
// as a little-endian u32 name length, the lowercase name, a u32 value
// length and the value, names in byte order and each name's lines in
// order; credential headers the call may not read are left out.
func hostRequestHeadersList(c *call, m api.Memory, stack []uint64) int32 {
	o, n := u32(stack, 0), u32(stack, 1)
	checkRange(m, o, n)
	h := c.requestHeader()
	type line struct{ name, value string }
	var lines []line
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	for _, k := range keys {
		if c.hidden(k) {
			continue
		}
		lk := strings.ToLower(k)
		for _, v := range h[k] {
			lines = append(lines, line{lk, v})
		}
	}
	var out []byte
	for _, l := range lines {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(l.name))) //nolint:gosec // G115: header names are bounded by the header ceiling.
		out = append(out, l.name...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(l.value))) //nolint:gosec // G115: header values are bounded by the header ceiling.
		out = append(out, l.value...)
	}
	return output(m, o, n, out)
}

// checkWrite reports whether a header write of name and value is allowed
// (docs/architecture/05-wasm-plugin-system.md "Host Function rules",
// Message writes): name an RFC 9110 token the Node does not manage (not
// host, a pseudo-header, a hop-by-hop or framing field, expect or the
// trace context), value without control bytes but HTAB.
func checkWrite(name, value string) bool {
	return httpfield.ValidName(name) && httpfield.Protect(name) == httpfield.Unprotected && httpfield.ValidValue(value)
}

// setHeader replaces every line of name in h, under any case, with value.
func setHeader(h http.Header, name, value string) {
	for k := range h {
		if strings.EqualFold(k, name) {
			delete(h, k)
		}
	}
	h[textproto.CanonicalMIMEHeaderKey(name)] = []string{value}
}

// removeHeader removes every line of name in h, under any case.
func removeHeader(h http.Header, name string) {
	for k := range h {
		if strings.EqualFold(k, name) {
			delete(h, k)
		}
	}
}

// hostRequestHeaderSet is request_header_set(name, value).
func hostRequestHeaderSet(c *call, m api.Memory, stack []uint64) int32 {
	name := memString(m, u32(stack, 0), u32(stack, 1))
	value := memString(m, u32(stack, 2), u32(stack, 3))
	if !checkWrite(name, value) {
		return errInvalid
	}
	h := c.requestHeader()
	if h == nil {
		return errAbsent
	}
	setHeader(h, name, value)
	return 0
}

// hostRequestHeaderRemove is request_header_remove(name).
func hostRequestHeaderRemove(c *call, m api.Memory, stack []uint64) int32 {
	name := memString(m, u32(stack, 0), u32(stack, 1))
	if !httpfield.ValidName(name) || httpfield.Protect(name) != httpfield.Unprotected {
		return errInvalid
	}
	h := c.requestHeader()
	if h == nil {
		return errAbsent
	}
	removeHeader(h, name)
	return 0
}

// hostResponseHeaderSet is response_header_set(name, value).
func hostResponseHeaderSet(c *call, m api.Memory, stack []uint64) int32 {
	name := memString(m, u32(stack, 0), u32(stack, 1))
	value := memString(m, u32(stack, 2), u32(stack, 3))
	if !checkWrite(name, value) {
		return errInvalid
	}
	h := c.responseHeader()
	if h == nil {
		return errAbsent
	}
	setHeader(h, name, value)
	return 0
}

// readBody is request_body_read and response_body_read: the body bytes
// from offset that fit in (o, c), 0 at the end.
func (c *call) readBody(m api.Memory, stack []uint64) int32 {
	offset, o, n := s64(stack, 0), u32(stack, 1), u32(stack, 2)
	checkRange(m, o, n)
	if offset < 0 {
		return errInvalid
	}
	body, err := c.x.Message().Body(c.reqCtx)
	if err != nil {
		return errLimit
	}
	if offset >= int64(len(body)) {
		return 0
	}
	return partial(m, o, n, body[offset:])
}

// replaceBody is request_body_replace and response_body_replace: the body
// becomes the guest's bytes, within limit; the Node recomputes framing.
func (c *call) replaceBody(m api.Memory, stack []uint64, limit int64) int32 {
	p, l := u32(stack, 0), u32(stack, 1)
	if limit > 0 && int64(l) > limit {
		checkRange(m, p, l)
		return errLimit
	}
	b := mem(m, p, l)
	if b == nil {
		b = []byte{}
	}
	if err := c.x.Message().SetBody(c.reqCtx, b); err != nil {
		return errLimit
	}
	return 0
}

// hostRequestBodyRead is request_body_read(offset, o, c).
func hostRequestBodyRead(c *call, m api.Memory, stack []uint64) int32 { return c.readBody(m, stack) }

// hostRequestBodyReplace is request_body_replace(p, l), within
// limits.maxRequestBodyBytes.
func hostRequestBodyReplace(c *call, m api.Memory, stack []uint64) int32 {
	return c.replaceBody(m, stack, c.b.limits.MaxRequestBodyBytes)
}

// hostResponseBodyRead is response_body_read(offset, o, c).
func hostResponseBodyRead(c *call, m api.Memory, stack []uint64) int32 { return c.readBody(m, stack) }

// hostResponseBodyReplace is response_body_replace(p, l), within the
// step's maxBodyBytes, else limits.maxResponseBodyBytes.
func hostResponseBodyReplace(c *call, m api.Memory, stack []uint64) int32 {
	limit := c.b.limits.MaxResponseBodyBytes
	if leg := c.x.Leg(); leg != nil && leg.MaxBodyBytes > 0 {
		limit = leg.MaxBodyBytes
	}
	return c.replaceBody(m, stack, limit)
}

// hostConsumerInfo is consumer_info(field, o, c): the bound Consumer's
// name, Tier, tags (a JSON array), auth method or claims (a JSON object);
// ERR_ABSENT without a Consumer.
func hostConsumerInfo(c *call, m api.Memory, stack []uint64) int32 {
	field, o, n := s32(stack), u32(stack, 1), u32(stack, 2)
	checkRange(m, o, n)
	if field < consumerName || field > consumerClaims {
		return errInvalid
	}
	id := c.x.Identity()
	if id == nil || id.Consumer == nil {
		return errAbsent
	}
	var v []byte
	switch field {
	case consumerName:
		v = []byte(id.Consumer.Name)
	case consumerTier:
		v = []byte(id.Consumer.Tier)
	case consumerTags:
		tags := make([]any, len(id.Consumer.Tags))
		for i, t := range id.Consumer.Tags {
			tags[i] = t
		}
		b, err := jsonval.Append(nil, tags)
		if err != nil {
			return errInvalid
		}
		v = b
	case consumerMethod:
		v = []byte(id.Method)
	default:
		v = []byte("{}")
		if id.Claims != nil && !id.Claims.IsNull() {
			b, err := id.Claims.AppendBody(nil)
			if err != nil {
				return errInvalid
			}
			v = b
		}
	}
	return output(m, o, n, v)
}

// hostResponseSend is response_send(status, body): the response the
// Policy produces when the Phase returns RESPOND; status 200 to 599.
func hostResponseSend(c *call, m api.Memory, stack []uint64) int32 {
	status, p, l := s32(stack), u32(stack, 1), u32(stack, 2)
	body := mem(m, p, l)
	if status < 200 || status > 599 {
		return errInvalid
	}
	c.sent, c.sendStatus, c.sendBody = true, int(status), body
	return 0
}

// Chunker is what onChunk sources give the Plugin host: the chunk of the
// call, its direction and its replacement. The streaming sources that call
// onChunk (SSE, WebSocket and LLM token streams) implement it on their
// Exchange.
type Chunker interface {
	// Chunk returns the chunk and whether it travels from the Upstream to
	// the client.
	Chunk() (data []byte, toClient bool)
	// ReplaceChunk replaces the chunk forwarded.
	ReplaceChunk(b []byte) error
	// EndStream ends the stream after this chunk (END_STREAM).
	EndStream()
}

// hostChunkRead is chunk_read(o, c, out): the chunk's bytes from where the
// call's last read ended, 0 at the end, and its direction at out.
func hostChunkRead(c *call, m api.Memory, stack []uint64) int32 {
	o, n, out := u32(stack, 0), u32(stack, 1), u32(stack, 2)
	checkRange(m, o, n)
	checkRange(m, out, 4)
	ch, ok := c.x.(Chunker)
	if !ok {
		return errAbsent
	}
	data, toClient := ch.Chunk()
	dir := directionToUpstream
	if toClient {
		dir = directionToClient
	}
	m.WriteUint32Le(out, uint32(dir))
	if c.chunkOff >= len(data) {
		return 0
	}
	w := partial(m, o, n, data[c.chunkOff:])
	c.chunkOff += int(w)
	return w
}

// hostChunkReplace is chunk_replace(p, l).
func hostChunkReplace(c *call, m api.Memory, stack []uint64) int32 {
	b := mem(m, u32(stack, 0), u32(stack, 1))
	ch, ok := c.x.(Chunker)
	if !ok {
		return errAbsent
	}
	if b == nil {
		b = []byte{}
	}
	if err := ch.ReplaceChunk(b); err != nil {
		return errLimit
	}
	return 0
}

// hostContextGet is context_get(key, o, c): the request's value of key
// for this Policy.
func hostContextGet(c *call, m api.Memory, stack []uint64) int32 {
	key := memString(m, u32(stack, 0), u32(stack, 1))
	o, n := u32(stack, 2), u32(stack, 3)
	checkRange(m, o, n)
	v, ok := c.req.ctx[key]
	if !ok {
		return errAbsent
	}
	return output(m, o, n, v)
}

// hostContextSet is context_set(key, value): keys and values of one
// Policy's request context hold at most 4 KiB.
func hostContextSet(c *call, m api.Memory, stack []uint64) int32 {
	key := memString(m, u32(stack, 0), u32(stack, 1))
	v := mem(m, u32(stack, 2), u32(stack, 3))
	if v == nil {
		v = []byte{}
	}
	used := c.req.ctxBytes
	if old, ok := c.req.ctx[key]; ok {
		used -= len(key) + len(old)
	}
	if used+len(key)+len(v) > contextBytes {
		return errLimit
	}
	if c.req.ctx == nil {
		c.req.ctx = map[string][]byte{}
	}
	c.req.ctx[key] = v
	c.req.ctxBytes = used + len(key) + len(v)
	return 0
}

// hostMetricAdd is metric_add(name, value): value added to
// ruralz_plugin_guest_events_total{policy, name}; a name outside
// ^[a-z][a-z0-9_]{0,31}$ is ERR_INVALID, a negative value or a 17th name
// ERR_LIMIT.
func hostMetricAdd(c *call, m api.Memory, stack []uint64) int32 {
	name := memString(m, u32(stack, 0), u32(stack, 1))
	value := s64(stack, 2)
	if !validEventName(name) {
		return errInvalid
	}
	if value < 0 {
		return errLimit
	}
	i, ok := c.b.events.bind(name)
	if !ok {
		return errLimit
	}
	c.b.tel.GuestEvent(i, name, value)
	return 0
}

// validEventName reports a name matching ^[a-z][a-z0-9_]{0,31}$.
func validEventName(s string) bool {
	if s == "" || len(s) > metricNameMax || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' {
			return false
		}
	}
	return true
}
