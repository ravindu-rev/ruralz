// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// TestCompileRegex: regex criteria match the whole input (04 req 28, 30).
func TestCompileRegex(t *testing.T) {
	rx, err := CompileRegex(`/v1/orders/[0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"/v1/orders/42":    true,
		"/v1/orders/42/x":  false,
		"/x/v1/orders/42":  false,
		"/v1/orders/":      false,
		"/v1/orders/4242x": false,
	} {
		if got := rx.MatchString(path); got != want {
			t.Errorf("match %q = %v; want %v", path, got, want)
		}
	}
	alt, err := CompileRegex(`a|b`)
	if err != nil || !alt.MatchString("b") || alt.MatchString("ab") {
		t.Errorf("alternation is not anchored as a whole: %v", err)
	}
	// A valid expression ending inside a \Q quote stays valid, and the
	// quote does not swallow the anchors (04 req 28, 30).
	quoted := []struct {
		re        string
		match, no []string
	}{
		{`\Qa.b`, []string{"a.b"}, []string{"axb", "a.b)$", "xa.b"}},
		{`\Qabc`, []string{"abc"}, []string{"abcd", "abc)$"}},
		{`x\Q`, []string{"x"}, []string{"x)$", "xy"}},
		{`\Qa\`, []string{`a\`}, []string{"a", `a\)$`}},
		{`\Qa\Eb.`, []string{"abc"}, []string{"ab", "abcd"}},
		{`\Qa\E\Q(b`, []string{"a(b"}, []string{"a(bc"}},
		{`\\Q[0-9]`, []string{`\Q1`}, []string{`\Q`, "1"}},
		{`[\\]\Q.`, []string{`\.`}, []string{`\x`}},
	}
	for _, tc := range quoted {
		rx, err := CompileRegex(tc.re)
		if err != nil {
			t.Errorf("CompileRegex(%q) = %v", tc.re, err)
			continue
		}
		for _, s := range tc.match {
			if !rx.MatchString(s) {
				t.Errorf("CompileRegex(%q) does not match %q", tc.re, s)
			}
		}
		for _, s := range tc.no {
			if rx.MatchString(s) {
				t.Errorf("CompileRegex(%q) matches %q", tc.re, s)
			}
		}
	}
	// An unbalanced expression must not escape the anchoring group.
	for _, bad := range []string{`(`, `a)(`, `a)|(.*`, `\8`, `)`} {
		_, err := CompileRegex(bad)
		if code, _ := errcode.CodeOf(err); code != CodeInvalid {
			t.Errorf("CompileRegex(%q) = %v; want %s", bad, err, CodeInvalid)
		}
	}
}

// TestMethodMatch: exact, case-sensitive tokens; HEAD does not imply GET
// (04 req 30).
func TestMethodMatch(t *testing.T) {
	cases := []struct {
		methods []string
		method  string
		want    bool
	}{
		{nil, "DELETE", true},
		{[]string{"GET"}, "GET", true},
		{[]string{"GET"}, "HEAD", false},
		{[]string{"GET"}, "get", false},
		{[]string{"GET", "POST"}, "POST", true},
	}
	for _, tc := range cases {
		if got := MethodMatch(tc.methods, tc.method); got != tc.want {
			t.Errorf("MethodMatch(%q, %q) = %v", tc.methods, tc.method, got)
		}
	}
}

// TestJoinHeader: repeated fields joined with ", " (04 req 25).
func TestJoinHeader(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a, b"},
		{[]string{"a, b", "c"}, "a, b, c"},
	}
	for _, tc := range cases {
		if got := JoinHeader(tc.in); got != tc.want {
			t.Errorf("JoinHeader(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	one := []string{"value"}
	if allocs := testing.AllocsPerRun(100, func() { _ = JoinHeader(one) }); allocs != 0 {
		t.Errorf("JoinHeader of one field: %v allocations", allocs)
	}
}

// TestHeaderMatcher covers the headers criterion (04 req 30): name
// case-insensitive, exact on the joined value, regex full match, present
// true and false.
func TestHeaderMatcher(t *testing.T) {
	cases := []struct {
		c      HeaderCriteria
		values []string
		want   bool
	}{
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu"}, []string{"eu"}, true},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu"}, []string{"EU"}, false},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu"}, nil, false},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu, us"}, []string{"eu", "us"}, true},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu, us"}, []string{"eu, us"}, true},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu, us"}, []string{"eu", "u"}, false},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu, us"}, []string{"eu"}, false},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu"}, []string{"eu", "us"}, false},
		{HeaderCriteria{Name: "X-Tenant", Exact: "eu,us"}, []string{"eu", "us"}, false},
		{HeaderCriteria{Name: "x-version", Regex: "v[0-9]+"}, []string{"v2"}, true},
		{HeaderCriteria{Name: "x-version", Regex: "v[0-9]+"}, []string{"v2-beta"}, false},
		{HeaderCriteria{Name: "x-version", Regex: "v[0-9]+, v[0-9]+"}, []string{"v1", "v2"}, true},
		{HeaderCriteria{Name: "x-version", Regex: ".*"}, nil, false},
		{HeaderCriteria{Name: "x-debug", Present: boolPtr(true)}, []string{""}, true},
		{HeaderCriteria{Name: "x-debug", Present: boolPtr(true)}, nil, false},
		{HeaderCriteria{Name: "x-debug", Present: boolPtr(false)}, nil, true},
		{HeaderCriteria{Name: "x-debug", Present: boolPtr(false)}, []string{}, true},
		{HeaderCriteria{Name: "x-debug", Present: boolPtr(false)}, []string{"1"}, false},
	}
	for _, tc := range cases {
		m, err := CompileHeader(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Match(tc.values); got != tc.want {
			t.Errorf("%+v on %q = %v; want %v", tc.c, tc.values, got, tc.want)
		}
	}
	m, _ := CompileHeader(HeaderCriteria{Name: "X-TENANT", Exact: "eu"})
	if m.Name != "x-tenant" || m.Key != "X-Tenant" {
		t.Errorf("name %q key %q", m.Name, m.Key)
	}
	values := []string{"eu"}
	if allocs := testing.AllocsPerRun(100, func() { _ = m.Match(values) }); allocs != 0 {
		t.Errorf("exact Match: %v allocations", allocs)
	}
	bad := []struct {
		c   HeaderCriteria
		msg string
	}{
		{HeaderCriteria{Exact: "x"}, "not an RFC 9110 token"},
		{HeaderCriteria{Name: "x y", Exact: "x"}, "not an RFC 9110 token"},
		{HeaderCriteria{Name: "x:y", Exact: "x"}, "not an RFC 9110 token"},
		{HeaderCriteria{Name: "x"}, "sets 0 of"},
		{HeaderCriteria{Name: "x", Exact: "a", Regex: "b"}, "sets 2 of"},
		{HeaderCriteria{Name: "x", Regex: "("}, "regular expression"},
	}
	for _, tc := range bad {
		_, err := CompileHeader(tc.c)
		if code, _ := errcode.CodeOf(err); code != CodeInvalid || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("CompileHeader(%+v) = %v; want %s with %q", tc.c, err, CodeInvalid, tc.msg)
		}
	}
	if _, err := CompileHeader(HeaderCriteria{Name: "!#$%&'*+-.^_`|~09az", Present: boolPtr(true)}); err != nil {
		t.Errorf("token characters rejected: %v", err)
	}
}
