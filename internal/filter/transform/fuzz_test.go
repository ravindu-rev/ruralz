// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FuzzOperations runs the JSON body operations over arbitrary bodies: a
// call either continues, its body strict JSON in the transform form when
// it changed, or cannot decide and leaves the message as it was.
func FuzzOperations(f *testing.F) {
	in, _ := ordersBody()
	for _, seed := range []string{in, `{}`, `[]`, `{"items": []}`, `{"a": {"b": [1, {"c": "x"}]}}`, `"s"`, `{"internal": 1, "items": [{"sku": 1}]}`, `{`, ""} {
		f.Add(seed)
	}
	cfg := &v1alpha1.TransformRequestConfig{
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus"), arrayOp("append", "a.b", "list"), arrayOp("delete", "items.0", "")},
		Set:      []v1alpha1.TransformRequestSet{reqSet("body", "meta.seen", "true"), reqSet("header", "x-a", `"1"`)},
		Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", "internal")},
		Replace:  []v1alpha1.Replace{pattern("customer.email", "^[^@]+", "***"), literally("a.b.*.c", "x", "y")},
	}
	flt := build(f, cfg, phase.ScopeRoute)
	var ed dotpath.Editor
	f.Fuzz(func(t *testing.T, body string) {
		h := jsonHeader()
		x, m := onRequest(t, h, body, "")
		res := flt.Handle(context.Background(), phase.OnRequestBody, x)
		switch res.Outcome {
		case filter.Continue:
			if m.setBodies == 0 {
				if string(m.Raw) != body && (body != "" || m.Raw != nil) {
					t.Fatalf("body %q changed without a write", m.Raw)
				}
				return
			}
			norm, err := ed.Normalize(nil, m.Raw)
			if err != nil || string(norm) != string(m.Raw) {
				t.Fatalf("rewritten body %q is not strict JSON in the transform form: %v", m.Raw, err)
			}
			if h.Get("X-A") != "1" {
				t.Fatalf("header %v", h)
			}
		case filter.CannotDecide:
			if string(m.Raw) != body && (body != "" || m.Raw != nil) || m.setBodies != 0 || h.Get("X-A") != "" {
				t.Fatalf("edited after a failure: body %q, header %v", m.Raw, h)
			}
		default:
			t.Fatalf("Handle = %+v", res)
		}
	})
}

// FuzzCheckPath covers the path rule over arbitrary results: an accepted
// path starts with "/", holds no "?" or "#" and no dot segment, is at most
// MaxPathBytes long and is its own normalized form.
func FuzzCheckPath(f *testing.F) {
	for _, seed := range []string{"/", "/a/b", "/a/../b", "/%2e", "/a?b", "/caf\xc3\xa9", "/a%2F%7e", "a", "//x/./"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, p string) {
		got, err := checkPath(p)
		if err != nil {
			return
		}
		if got == "" || got[0] != '/' || strings.ContainsAny(got, "?#") || len(got) > MaxPathBytes {
			t.Fatalf("checkPath(%q) = %q", p, got)
		}
		if again, err := routematch.NormalizePath(got); err != nil || again != got {
			t.Fatalf("checkPath(%q) = %q, normalizing again %q, %v", p, got, again, err)
		}
		if err := routematch.CheckForwardPath(got); err != nil {
			t.Fatalf("checkPath(%q) = %q, which a forwarded path may not be: %v", p, got, err)
		}
	})
}

// FuzzReplacers covers the replacers against the standard library: the
// literal replacer equals bytes.ReplaceAll, the pattern replacer
// regexp.ReplaceAll, and each reports a change exactly when the output
// differs.
func FuzzReplacers(f *testing.F) {
	for _, seed := range [][3]string{{"abcabc", "b", "x"}, {"aaa", "", "-"}, {"héllo", "l+", "$0$0"}, {"", "", "x"}, {"x", "(", "y"}} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, s, old, repl string) {
		lit := &literalReplacer{old: []byte(old), repl: []byte(repl)}
		got, changed := lit.Replace(nil, []byte(s))
		want := strings.ReplaceAll(s, old, repl)
		if string(got) != want || changed != (want != s) {
			t.Fatalf("literal %q→%q on %q = %q, %v; want %q", old, repl, s, got, changed, want)
		}
		pr, err := compilePattern(old, repl)
		if err != nil {
			return
		}
		got, changed = pr.Replace([]byte("pre:"), []byte(s))
		want = string(pr.re.ReplaceAll([]byte(s), []byte(repl)))
		if string(got) != "pre:"+want || changed != (want != s) {
			t.Fatalf("pattern %q→%q on %q = %q, %v; want %q", old, repl, s, got, changed, want)
		}
	})
}

// compilePattern returns the pattern replacer of re and repl.
func compilePattern(re, repl string) (*patternReplacer, error) {
	f, err := Build(context.Background(), buildEnvNoCEL(), RequestSpec(&v1alpha1.TransformRequestConfig{Replace: []v1alpha1.Replace{pattern("", re, repl)}}))
	if err != nil {
		return nil, err
	}
	return f.ops[0].repl.(*patternReplacer), nil
}

// buildEnvNoCEL is the BuildEnv of a config without CEL fields.
func buildEnvNoCEL() filter.BuildEnv { return filter.BuildEnv{Scope: phase.ScopeRoute} }

// TestReplacersEqualStandardLibrary runs FuzzReplacers' property over
// fixed inputs, including the empty-match rules.
func TestReplacersEqualStandardLibrary(t *testing.T) {
	cases := [][3]string{
		{"abcabc", "b", "x"},
		{"aaa", "", "-"},
		{"héllo", "", "|"},
		{"", "", "x"},
		{"x", "x", "x"},
		{"abab", "a*", "-"},
		{"héllo", "l+", "[$0]"},
		{"a1b22", `(\d+)`, "<$1>"},
		{"aaa", "^", "^"},
		{"abc", "$", "!"},
	}
	for _, c := range cases {
		s, old, repl := c[0], c[1], c[2]
		lit := &literalReplacer{old: []byte(old), repl: []byte(repl)}
		got, changed := lit.Replace(nil, []byte(s))
		if want := strings.ReplaceAll(s, old, repl); string(got) != want || changed != (want != s) {
			t.Errorf("literal %q→%q on %q = %q, %v; want %q", old, repl, s, got, changed, want)
		}
		pr, err := compilePattern(old, repl)
		if err != nil {
			t.Fatalf("compile %q: %v", old, err)
		}
		got, changed = pr.Replace(nil, []byte(s))
		if want := string(pr.re.ReplaceAll([]byte(s), []byte(repl))); string(got) != want || changed != (want != s) {
			t.Errorf("pattern %q→%q on %q = %q, %v; want %q", old, repl, s, got, changed, want)
		}
	}
}
