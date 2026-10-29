// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// decoded is one body version's decoded JSON form: val is the decoded body,
// nil when the body is not JSON (empty, another media type, or not valid
// JSON) or on err; built is the size charged to the holder's account.
type decoded struct {
	done  bool
	val   expr.Value
	err   error
	built int64
}

// cel returns the CEL value of the body: the decoded body, JSON null when
// it is not JSON, or nil, a runtime error on selection, when it failed.
func (v *decoded) cel() expr.Value {
	switch {
	case v.err != nil:
		return nil
	case v.val == nil:
		return nullBody{}
	default:
		return v.val
	}
}

// drop releases the view's charge from acct and forgets it.
func (v *decoded) drop(acct *body.Account) {
	acct.Release(v.built)
	*v = decoded{}
}

// sharedDecoded is a decoded view concurrent legs read: built once under
// mu, then read lock-free once ready is set.
type sharedDecoded struct {
	mu    sync.Mutex
	ready atomic.Bool
	v     decoded
}

// nullBody is JSON null, the CEL value of a body that is not JSON
// (docs/architecture/03-data-plane.md "Transform Policies").
type nullBody struct{}

// IsNull reports true.
func (nullBody) IsNull() bool { return true }

// AppendBody appends null.
func (nullBody) AppendBody(dst []byte) ([]byte, error) { return append(dst, "null"...), nil }

// Native returns nil, JSON null.
func (nullBody) Native() (any, error) { return nil, nil }

// decode decodes b, a body under the header h that arrived under rawLimit,
// and charges the built values to acct. A body that is empty, whose one
// Content-Type field line is not application/json or +json, or that is not
// valid JSON is not JSON (no value, no error). Past filter.DecodedLimitFactor
// times rawLimit it is filter.ErrTooLarge, and a refused charge is
// filter.ErrBudget.
func (p *Pool) decode(acct *body.Account, h http.Header, b []byte, rawLimit int64) decoded {
	if len(b) == 0 || !jsonBody(h) || p.dec == nil {
		return decoded{done: true}
	}
	limit := body.DecodedCap(rawLimit)
	if limit <= 0 {
		// No raw limit admits a non-empty body; a zero budget would mean an
		// unlimited decode.
		return decoded{done: true, err: filter.ErrTooLarge}
	}
	v, built, err := p.dec.DecodeJSON(b, limit)
	switch {
	case errors.Is(err, expr.ErrTooLarge):
		return decoded{done: true, err: filter.ErrTooLarge}
	case err != nil, v == nil:
		return decoded{done: true}
	}
	if err := acct.ReserveDecoded(built, rawLimit); err != nil {
		return decoded{done: true, err: err}
	}
	return decoded{done: true, val: v, built: built}
}

// jsonBody reports whether h has exactly one Content-Type field line and
// it names JSON (jsonval.IsJSONMediaType); a repeated Content-Type is
// ambiguous, so the body is not JSON.
func jsonBody(h http.Header) bool {
	lines := h["Content-Type"]
	return len(lines) == 1 && jsonval.IsJSONMediaType(lines[0])
}

// setBodyFields updates the fields a replaced body of n bytes makes wrong:
// Content-Length is set, and Content-Encoding (limits count decoded bytes)
// and the body digests (Content-Digest and Repr-Digest of RFC 9530, the
// legacy Digest and Content-MD5) are removed.
func setBodyFields(h http.Header, n int) {
	if h == nil {
		return
	}
	h["Content-Length"] = []string{strconv.Itoa(n)}
	for _, k := range [...]string{"Content-Encoding", "Content-Digest", "Repr-Digest", "Digest", "Content-Md5"} {
		delete(h, k)
	}
}

// declaredLength returns the Content-Length of h, -1 when absent or
// invalid.
func declaredLength(h http.Header) int64 {
	lines := h["Content-Length"]
	if len(lines) != 1 {
		return -1
	}
	n, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
