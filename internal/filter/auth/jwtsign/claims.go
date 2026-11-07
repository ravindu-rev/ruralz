// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// MaxExpiry bounds how far ahead of the request's start a signed token's
// exp may lie: 24 hours (target), the bound auth.jwt accepts
// (docs/architecture/08-security-and-identity.md "JWT signing").
const MaxExpiry = 24 * time.Hour

// Claims refusals; each is the invalid_value error.type and names no
// claim value.
var (
	errNoExp      = errors.New("the claims hold no exp NumericDate")
	errExpTooLate = errors.New("the claims exp is more than " + MaxExpiry.String() + " ahead")
	errNotClaims  = errors.New("the claims are not a JSON object")
)

// scanOptions scan a claims object that is already strict JSON (the
// normalized body or a JSON writing of a CEL result), so the scan neither
// repeats the duplicate check nor stops at a depth it accepted.
func scanOptions() jsonval.ScanOptions {
	return jsonval.ScanOptions{MaxDepth: jsonval.MaxDepthLimit, AllowDuplicateNames: true}
}

// checkExp checks the claims object claims, one JSON object, against now:
// its exp member is a NumericDate (RFC 7519 section 2: a JSON number of
// seconds since the epoch, fractions allowed) at most MaxExpiry after now.
// An exp in the past is signed as it is. s is the caller's scanner, reset
// here.
func checkExp(s *jsonval.Scanner, claims []byte, now time.Time) error {
	s.Reset(claims, scanOptions())
	defer s.Reset(nil, jsonval.ScanOptions{})
	t, err := s.Next()
	if err != nil || t.Kind != jsonval.KindObjectStart {
		return errNotClaims
	}
	for {
		nt, err := s.Next()
		if err != nil {
			return errNotClaims
		}
		if nt.Kind == jsonval.KindObjectEnd {
			return errNoExp
		}
		vt, err := s.Next()
		if err != nil {
			return errNotClaims
		}
		if nameIs(s, nt, "exp") {
			return expWithin(s, vt, now)
		}
		if err := skip(s, vt); err != nil {
			return errNotClaims
		}
	}
}

// farSeconds bounds the NumericDates expWithin converts to a time: one
// further from the epoch lies past any bound, ahead or behind.
const farSeconds = 1e15

// expWithin checks the value token vt of exp: a finite JSON number at most
// MaxExpiry after now, compared as times, so an integer exp at the bound
// passes to the nanosecond.
func expWithin(s *jsonval.Scanner, vt jsonval.Token, now time.Time) error {
	if vt.Kind != jsonval.KindNumber {
		return errNoExp
	}
	exp, err := strconv.ParseFloat(string(s.Bytes(vt)), 64)
	switch {
	case err != nil || math.IsInf(exp, 0) || math.IsNaN(exp):
		return errNoExp
	case exp >= farSeconds:
		return errExpTooLate
	case exp <= -farSeconds:
		return nil
	}
	sec := math.Floor(exp)
	at := time.Unix(int64(sec), int64((exp-sec)*1e9))
	if at.After(now.Add(MaxExpiry)) {
		return errExpTooLate
	}
	return nil
}

// nameIs reports whether the member name token nt decodes to name.
func nameIs(s *jsonval.Scanner, nt jsonval.Token, name string) bool {
	if !nt.Escaped {
		b := s.Bytes(nt)
		return len(b) == len(name)+2 && string(b[1:len(b)-1]) == name
	}
	return s.Text(nt) == name
}

// skip consumes the rest of the value whose first token is t.
func skip(s *jsonval.Scanner, t jsonval.Token) error {
	if t.Kind != jsonval.KindObjectStart && t.Kind != jsonval.KindArrayStart {
		return nil
	}
	depth := s.Depth() - 1
	for {
		nt, err := s.Next()
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if (nt.Kind == jsonval.KindObjectEnd || nt.Kind == jsonval.KindArrayEnd) && s.Depth() == depth {
			return nil
		}
	}
}
