// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"errors"
	"net/textproto"
	"reflect"
	"testing"
)

// Tests for 05 req 67 (RateLimit-Policy and RateLimit on 429 responses,
// draft-ietf-httpapi-ratelimit-headers-11) and 05 req 68 (admitted
// responses: one RateLimit member per limit with a known remaining count,
// a RateLimit-Policy member for every applied limit), with golden bytes.

// limit mirrors filter.RateLimitField (this L0 package cannot import it).
type limit struct {
	name       string
	q, w, r, t int64
	known      bool
}

// fields builds both field values the way the request handler does after
// onResponse (R-18): every applied limit contributes a RateLimit-Policy
// member; only limits with a known remaining count contribute a RateLimit
// member.
func fields(t *testing.T, limits []limit) (policy, rl []byte) {
	t.Helper()
	var err error
	for _, l := range limits {
		if policy, err = AppendRateLimitPolicy(policy, l.name, l.q, l.w); err != nil {
			t.Fatal(err)
		}
		if l.known {
			if rl, err = AppendRateLimit(rl, l.name, l.r, l.t); err != nil {
				t.Fatal(err)
			}
		}
	}
	return policy, rl
}

func TestRateLimitGolden429Req67(t *testing.T) {
	// The TMR "RateLimit response headers" example: three limits applied,
	// ratelimit-gold.1 denied with r=0 and t=1.
	policy, _ := fields(t, []limit{
		{name: "ratelimit-global", q: 1000, w: 1},
		{name: "ratelimit-gold.1", q: 100, w: 1},
		{name: "ratelimit-gold.2", q: 5000, w: 60},
	})
	if want := golden(t, "ratelimit-policy-429.golden"); string(policy) != string(want) {
		t.Errorf("RateLimit-Policy = %s, want %s", policy, want)
	}
	denied, err := AppendRateLimit(nil, "ratelimit-gold.1", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "ratelimit-429.golden"); string(denied) != string(want) {
		t.Errorf("RateLimit = %s, want %s", denied, want)
	}
}

func TestRateLimitGoldenAdmittedReq68(t *testing.T) {
	// A fail_open (or unmetered) limit contributes only its
	// RateLimit-Policy member.
	policy, rl := fields(t, []limit{
		{name: "ratelimit-global", q: 1000, w: 1, r: 999, t: 1, known: true},
		{name: "ratelimit-gold.1", q: 100, w: 1, r: 99, t: 1, known: true},
		{name: "ratelimit-gold.2", q: 5000, w: 60, r: 4999, t: 60, known: true},
		{name: "quota-monthly", q: 10000, w: 2592000, r: 9950, t: 2592000, known: true},
		{name: "fail-open", q: 10, w: 1},
	})
	if want := golden(t, "ratelimit-policy-admitted.golden"); string(policy) != string(want) {
		t.Errorf("RateLimit-Policy = %s, want %s", policy, want)
	}
	if want := golden(t, "ratelimit-admitted.golden"); string(rl) != string(want) {
		t.Errorf("RateLimit = %s, want %s", rl, want)
	}
}

func TestRateLimitReparsesReq67(t *testing.T) {
	policy := golden(t, "ratelimit-policy-429.golden")
	l, err := ParseList(string(policy))
	if err != nil {
		t.Fatal(err)
	}
	want := List{
		ItemMember(String("ratelimit-global"), p("q", Integer(1000)), p("w", Integer(1))),
		ItemMember(String("ratelimit-gold.1"), p("q", Integer(100)), p("w", Integer(1))),
		ItemMember(String("ratelimit-gold.2"), p("q", Integer(5000)), p("w", Integer(60))),
	}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("ParseList(RateLimit-Policy) = %#v, want %#v", l, want)
	}
	// The helpers agree with the general serializer.
	general, err := AppendList(nil, want)
	if err != nil || string(general) != string(policy) {
		t.Errorf("AppendList = %s, %v; want %s", general, err, policy)
	}
}

func TestRateLimitFailuresReq67(t *testing.T) {
	tests := []struct {
		name       string
		policyName string
		a, b       int64
	}{
		{"non-ASCII name", "café", 1, 1},
		{"control byte in name", "a\nb", 1, 1},
		{"negative first", "a", -1, 1},
		{"negative second", "a", 1, -1},
		{"first over the integer range", "a", MaxInteger + 1, 1},
		{"second over the integer range", "a", 1, MaxInteger + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, fn := range []func([]byte, string, int64, int64) ([]byte, error){AppendRateLimitPolicy, AppendRateLimit} {
				dst := []byte(`"x";q=1;w=1`)
				got, err := fn(dst, tt.policyName, tt.a, tt.b)
				if !errors.Is(err, ErrSerialize) || string(got) != `"x";q=1;w=1` {
					t.Errorf("= %q, %v; want ErrSerialize and dst unchanged", got, err)
				}
			}
		})
	}
}

func TestRateLimitBounds(t *testing.T) {
	got, err := AppendRateLimit(nil, "", 0, MaxInteger)
	if err != nil || string(got) != `"";r=0;t=999999999999999` {
		t.Errorf("AppendRateLimit = %q, %v", got, err)
	}
}

func TestRateLimitHeaderKeysAreCanonical(t *testing.T) {
	for _, k := range []string{HeaderRateLimitPolicy, HeaderRateLimit} {
		if c := textproto.CanonicalMIMEHeaderKey(k); c != k {
			t.Errorf("%q is not canonical (%q)", k, c)
		}
	}
	if !equalFoldASCII(HeaderRateLimitPolicy, "RateLimit-Policy") || !equalFoldASCII(HeaderRateLimit, "RateLimit") {
		t.Error("header keys do not name the draft's fields")
	}
}

func equalFoldASCII(a, b string) bool {
	return textproto.CanonicalMIMEHeaderKey(a) == textproto.CanonicalMIMEHeaderKey(b)
}

func TestRateLimitDoesNotAllocate(t *testing.T) {
	buf := make([]byte, 0, 512)
	allocs := testing.AllocsPerRun(100, func() {
		var err error
		b := buf[:0]
		if b, err = AppendRateLimitPolicy(b, "ratelimit-gold.1", 100, 1); err != nil {
			t.Fatal(err)
		}
		if b, err = AppendRateLimitPolicy(b, "ratelimit-gold.2", 5000, 60); err != nil {
			t.Fatal(err)
		}
		if _, err = AppendRateLimit(b, "ratelimit-gold.1", 0, 1); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}
