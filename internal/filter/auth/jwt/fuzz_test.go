// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// FuzzHandle drives untrusted Authorization values through the Filter:
// it never panics, a value whose token does not parse is RZ-AUTH-002, and
// every outcome is one of the auth.jwt decisions.
func FuzzHandle(f *testing.F) {
	fx := newFixture(f, options{})
	for _, seed := range []string{
		"", "Bearer", "Bearer a.b.c", "Basic dXNlcjpwYXNz", "Bearer ..",
		"Bearer eyJhbGciOiJub25lIn0.e30.",
		"Bearer eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJodHRwczovL2lkcC5leGFtcGxlIn0.AAAA",
		"Bearer " + mint(f, fx.keys.ed, nil, nil),
		"Bearer " + mint(f, fx.keys.rs, map[string]any{"kid": nil}, nil),
		"Bearer " + mint(f, fx.keys.es, map[string]any{"crit": []string{"x"}}, nil),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		x := exchange(value)
		r := fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
		switch r.Outcome {
		case filter.Continue:
			if x.Ident == nil {
				t.Fatal("Continue without an identity")
			}
		case filter.Respond:
			switch r.Response.Code {
			case auth.CodeMissing, auth.CodeInvalid, auth.CodeClaims, auth.CodeRevoked, auth.CodeUnknownKID:
			default:
				t.Fatalf("deny with %s", r.Response.Code)
			}
		case filter.CannotDecide:
			if r.Code != auth.CodeNoKeys || !errors.Is(r.Err, errNoKeys) {
				t.Fatalf("CannotDecide %+v", r)
			}
		default:
			t.Fatalf("outcome %v", r.Outcome)
		}
		raw, bearer := bearerToken(value)
		if !bearer {
			return
		}
		s := fx.f.codec.getScratch()
		defer fx.f.codec.putScratch(s)
		if _, err := fx.f.codec.parse(raw, s); err != nil && (r.Outcome != filter.Respond || r.Response.Code != auth.CodeInvalid) {
			t.Fatalf("a token that does not parse got %+v", r)
		}
	})
}
