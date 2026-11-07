// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// TestCheckExp: the claims hold an exp NumericDate, a JSON number of
// seconds with any fraction, at most MaxExpiry ahead of now (target); an
// exp that passed is signed as it is; any other exp is refused.
func TestCheckExp(t *testing.T) {
	now := start()
	cases := []struct {
		name   string
		claims string
		want   error
	}{
		{"an hour ahead", `{"sub":"a","exp":` + expIn(time.Hour) + `}`, nil},
		{"exactly the bound", `{"exp":` + expIn(MaxExpiry) + `}`, nil},
		{"a fraction within the bound", `{"exp":` + expIn(MaxExpiry-time.Second) + `.999}`, nil},
		{"past the bound by a second", `{"exp":` + expIn(MaxExpiry+time.Second) + `}`, errExpTooLate},
		{"past the bound by a fraction", `{"exp":` + expIn(MaxExpiry) + `.5}`, errExpTooLate},
		{"in exponent form past the bound", `{"exp":1E12}`, errExpTooLate},
		{"in the past", `{"exp":` + expIn(-time.Hour) + `}`, nil},
		{"negative", `{"exp":-1}`, nil},
		{"after nested members", `{"a":{"exp":"x","b":[{"exp":1}]},"l":[1,[2]],"exp":` + expIn(time.Minute) + `}`, nil},
		{"only nested", `{"a":{"exp":` + expIn(time.Minute) + `}}`, errNoExp},
		{"absent", `{"sub":"a"}`, errNoExp},
		{"empty claims", `{}`, errNoExp},
		{"a string", `{"exp":"` + expIn(time.Hour) + `"}`, errNoExp},
		{"null", `{"exp":null}`, errNoExp},
		{"an object", `{"exp":{"at":1}}`, errNoExp},
		{"out of the double range", `{"exp":1e999}`, errNoExp},
		{"escaped name", `{"exp":` + expIn(time.Hour) + `}`, nil},
		{"escaped name past the bound", `{"exp":` + expIn(48*time.Hour) + `}`, errExpTooLate},
		{"not an object", `[{"exp":1}]`, errNotClaims},
		{"not JSON", `{"exp":`, errNotClaims},
	}
	var s jsonval.Scanner
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkExp(&s, []byte(c.claims), now)
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Errorf("checkExp(%s) = %v, want %v", c.claims, err, c.want)
			}
		})
	}
}

// TestCheckExpBoundFollowsNow: the bound is relative to the request's
// start, to the nanosecond.
func TestCheckExpBoundFollowsNow(t *testing.T) {
	var s jsonval.Scanner
	claims := []byte(`{"exp":` + expIn(MaxExpiry) + `}`)
	if err := checkExp(&s, claims, start()); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if err := checkExp(&s, claims, start().Add(-time.Nanosecond)); !errors.Is(err, errExpTooLate) {
		t.Errorf("a nanosecond earlier: %v, want errExpTooLate", err)
	}
	if err := checkExp(&s, claims, start().Add(time.Hour)); err != nil {
		t.Errorf("an hour later: %v", err)
	}
}
