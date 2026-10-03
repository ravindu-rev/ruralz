// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Fuzz targets and property tests: JSON values round-trip through
// AppendBody and Native and iterate in ascending key order (03 req 6.3, 25,
// 41; 07 req 57); header lookup equals expr.JoinedHeader (03 req 19);
// canonicalization equals net/textproto.

// decodeLoose decodes one JSON text with json.Number numbers, or reports
// false.
func decodeLoose(data []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// FuzzValueJSON checks, for any JSON text: AppendBody writes JSON that
// decodes to the same tree (numbers verbatim), Native equals the tree, the
// CEL view iterates keys in ascending order, and the generic path over
// cel-go containers writes numerically the same document.
func FuzzValueJSON(f *testing.F) {
	for _, s := range []string{
		`{"b":1,"a":[1.5,"x",null,true]}`, `[]`, `{}`, `"s"`, `1e400`, `{"a":{"b":{"c":[]}}}`,
		`9007199254740993`, `-0`, `" \u0000😀"`, `{"":{"":[{}]}}`, `[1e-7,1E+2,0.1,-2.50]`,
		`{"z":"1","y":"2","x":{"w":[null,false]}}`, `18446744073709551616`, `true`, `null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tree, ok := decodeLoose(data)
		if !ok {
			return
		}
		v := FromNative(tree)
		body, err := v.AppendBody(nil)
		if s, isString := tree.(string); isString {
			if err != nil || string(body) != s {
				t.Fatalf("AppendBody(%q) = %q, %v", data, body, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("AppendBody(%q): %v", data, err)
		}
		if back, ok := decodeLoose(body); !ok || !reflect.DeepEqual(back, tree) {
			t.Fatalf("AppendBody(%q) = %q, decodes to %#v", data, body, back)
		}
		if n, err := v.Native(); err != nil || !reflect.DeepEqual(n, tree) {
			t.Fatalf("Native(%q) = %#v, %v", data, n, err)
		}
		walkOrdered(t, v.Val(), 0)
		g, err := FromVal(rebuild(v.Val())).AppendBody(nil)
		if err != nil {
			if !errors.Is(err, ErrNotJSON) {
				t.Fatalf("generic AppendBody(%q): %v", data, err)
			}
			return
		}
		if back, ok := decodeLoose(g); !ok || !numericEqual(back, tree) {
			t.Fatalf("generic AppendBody(%q) = %q", data, g)
		}
	})
}

// walkOrdered visits a CEL view, failing on keys out of ascending order or
// an error value.
func walkOrdered(t *testing.T, v ref.Val, depth int) {
	t.Helper()
	if depth > 200 {
		return
	}
	switch x := v.(type) {
	case traits.Mapper:
		prev := ""
		for i, k := range iterKeys(x) {
			if i > 0 && k <= prev {
				t.Fatalf("keys out of order: %q after %q", k, prev)
			}
			prev = k
			walkOrdered(t, x.Get(types.String(k)), depth+1)
		}
	case traits.Lister:
		for i := range int(x.Size().(types.Int)) {
			walkOrdered(t, x.Get(types.Int(i)), depth+1)
		}
	case *types.Err:
		t.Fatalf("error value: %v", x)
	}
}

// rebuild copies a CEL view into cel-go's own containers.
func rebuild(v ref.Val) ref.Val {
	switch x := v.(type) {
	case traits.Mapper:
		m := map[ref.Val]ref.Val{}
		for it := x.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			m[k] = rebuild(x.Get(k))
		}
		return types.NewRefValMap(types.DefaultTypeAdapter, m)
	case traits.Lister:
		var l []ref.Val
		for i := range int(x.Size().(types.Int)) {
			l = append(l, rebuild(x.Get(types.Int(i))))
		}
		return types.NewRefValList(types.DefaultTypeAdapter, l)
	}
	return v
}

// numericEqual compares trees with numbers compared as float64.
func numericEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		fx, errX := strconv.ParseFloat(string(x), 64)
		fy, errY := strconv.ParseFloat(string(y), 64)
		return errX == nil && errY == nil && fx == fy
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, e := range x {
			if !numericEqual(e, y[k]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !numericEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// FuzzHeaderLookup checks joinedHeader against expr.JoinedHeader over maps
// built by http.Header.Add and by direct assignment, with any bytes: both
// fold ASCII only and choose among non-canonical keys deterministically (03
// req 6.3). request.headers hides Host, the other header maps show it (03
// req 19).
func FuzzHeaderLookup(f *testing.F) {
	f.Add("X-Tenant", "accept", "x-tenant")
	f.Add("a b", "A b", "a B")
	f.Add("Host", "x", "HOST")
	f.Add("content-type", "Content-Type", "CONTENT-TYPE")
	f.Add("x-DUP", "X-dup", "x-dup")
	f.Add("\u212a", "x-dup", "k")
	// A name over maxStackKey bytes that is not a token: the exact key wins
	// over a smaller folded one, as in expr.JoinedHeader.
	longSpace := "a " + strings.Repeat("b", maxStackKey-1)
	f.Add(longSpace, "A "+strings.Repeat("b", maxStackKey-1), longSpace)
	f.Fuzz(func(t *testing.T, k1, k2, name string) {
		added := http.Header{}
		added.Add(k1, "v1")
		added.Add(k2, "v2")
		raw := http.Header{k1: {"v1"}}
		raw[k2] = append(raw[k2], "v2")
		for _, h := range []http.Header{added, raw} {
			checkHeaderLookup(t, h, name)
		}
	})
}

// checkHeaderLookup compares every header lookup path for name over h.
func checkHeaderLookup(t *testing.T, h http.Header, name string) {
	t.Helper()
	got, ok := joinedHeader(h, name)
	if rv, found := (requestHeaderSource{&h}).lookup(name); isHost(name) && found {
		t.Fatalf("Host visible in request.headers as %v", rv)
	} else if !isHost(name) && (found != ok || found && rv != types.String(got)) {
		t.Fatalf("request.headers[%q] = %v, %v; joinedHeader = %q, %v", name, rv, found, got, ok)
	}
	if rv, found := (headerSource{&h}).lookup(name); found != ok || found && rv != types.String(got) {
		t.Fatalf("response.headers[%q] = %v, %v; joinedHeader = %q, %v", name, rv, found, got, ok)
	}
	if want, wantOK := expr.JoinedHeader(h, name); got != want || ok != wantOK {
		t.Fatalf("joinedHeader(%q) = %q, %v; expr.JoinedHeader = %q, %v over %q", name, got, ok, want, wantOK, h)
	}
	if again, _ := joinedHeader(h, name); again != got {
		t.Fatalf("lookup not deterministic: %q then %q", got, again)
	}
	for _, hideHost := range []bool{false, true} {
		if len(headerNames(h, hideHost)) != headerSize(h, hideHost) {
			t.Fatalf("headerSize(%v) %d, names %v", hideHost, headerSize(h, hideHost), headerNames(h, hideHost))
		}
	}
}

// FuzzCanonicalKey checks the stack canonicalization against
// net/textproto and isCanonical against its definition.
func FuzzCanonicalKey(f *testing.F) {
	for _, s := range []string{"", "x-foo", "X-FOO", "a b", "\xff", "-a-", "Www-Authenticate"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want := textproto.CanonicalMIMEHeaderKey(s)
		if got := string(canonicalKey(nil, s)); got != want {
			t.Fatalf("canonicalKey(%q) = %q, want %q", s, got, want)
		}
		if got := isCanonical(s); got != (s != "" && validName(s) && want == s) {
			t.Fatalf("isCanonical(%q) = %v", s, got)
		}
	})
}

// TestConcurrentErrorEvaluation evaluates programs that fail over one
// activation from several goroutines (run with -race). The malformed-JSON
// body of 03 req 25 is one error Value "decoded once per body and shared by
// every reader"; legs and composition steps evaluate copied Vars that share
// it on their own goroutines (03 req 38, 39). cel-go labels the error value
// of a failed selection with its AST node ID in place, so every read must
// hand it a new error: a shared one is a data race, and keeps the node ID
// of the first program that read it. Null selections (03 req 18) and
// missing keys are covered as well.
func TestConcurrentErrorEvaluation(t *testing.T) {
	env := testEnv(t)
	// A missing key, constant or computed, fails in cel-go's own attribute
	// qualifier with a new "no such key" error.
	tests := []struct {
		src  string
		want error
		text string
	}{
		{src: `request.body.x == 1`, want: ErrBody},
		{src: `has(request.body.y) && request.body.y == 2`, want: ErrBody},
		{src: `has(request.body) && request.body.z == 3`, want: ErrBody},
		{src: `request.body.x == 1 || request.body.y == 2`, want: ErrBody},
		{src: `request.body.size() > 0`, want: ErrBody},
		{src: `response.body.total > 1.0`, want: ErrBody},
		{src: `has(response.body.total)`, want: ErrBody},
		{src: `steps.order.body.id == 7`, want: ErrBody},
		{src: `auth.claims.sub == "u1"`, want: ErrBody},
		{src: `consumer.name == "acme"`, want: ErrNull},
		{src: `has(consumer.tags) || consumer.tier == "gold"`, want: ErrNull},
		{src: `request.headers[request.method] == "a"`, text: "no such key"},
		{src: `route.labels[request.method] == "a"`, text: "no such key"},
		{src: `steps.ok.body[request.method] == 1`, text: "no such key"},
		{src: `request.headers["x-missing"] == "a"`, text: "no such key"},
		{src: `steps.ok.body.missing == 1`, text: "no such key"},
	}
	// failsAs reports whether err is the error test i expects.
	failsAs := func(i int, err error) bool {
		if tests[i].want != nil {
			return errors.Is(err, tests[i].want)
		}
		return err != nil && strings.Contains(err.Error(), tests[i].text)
	}
	// vars returns the activation: one error Value read as every body, a
	// null consumer, a prepared route and a decoded body without the key.
	vars := func(malformed *Value) *expr.Vars {
		v := fullVars()
		v.Request.Body = malformed
		v.Response.Body = malformed
		v.Auth.Claims = malformed
		v.Consumer = nil
		PrepareRoute(v.Route)
		st := &expr.Steps{}
		st.Add("order", expr.Step{Status: 200, Body: malformed})
		st.Add("ok", expr.Step{Status: 200, Body: FromNative(map[string]any{"id": jsonNum("7")})})
		v.Steps = st
		return v
	}
	// nodeID returns the node ID cel-go labeled the evaluation error with.
	nodeID := func(err error) int64 {
		var ce *types.Err
		if !errors.As(err, &ce) {
			return -1
		}
		return ce.NodeID()
	}
	prgs := make([]cel.Program, len(tests))
	wantIDs := make([]int64, len(tests))
	for i, tc := range tests {
		prgs[i] = compile(t, env, tc.src)
		// The node ID each program reports over a body of its own.
		_, _, err := prgs[i].Eval(NewActivation(vars(ErrorValue(errors.New("unexpected EOF")))))
		if !failsAs(i, err) {
			t.Fatalf("%s: err = %v, want %v %q", tc.src, err, tc.want, tc.text)
		}
		wantIDs[i] = nodeID(err)
	}
	shared := ErrorValue(errors.New("unexpected EOF"))
	a := NewActivation(vars(shared))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for n := range len(prgs) {
				i := (g + n) % len(prgs) // goroutines start on different programs
				out, _, err := prgs[i].Eval(a)
				if !failsAs(i, err) {
					t.Errorf("%s = %v, %v; want %v %q", tests[i].src, out, err, tests[i].want, tests[i].text)
					continue
				}
				if id := nodeID(err); id != wantIDs[i] {
					t.Errorf("%s: error node ID %d, want %d", tests[i].src, id, wantIDs[i])
				}
			}
		})
	}
	wg.Wait()
	// The shared Value itself is unchanged: every read is a new error.
	if v1, v2 := shared.Val(), shared.Val(); v1 == v2 || nodeID(v1.(error)) != 0 {
		t.Errorf("shared error value is reused or labeled: %v (node %d)", v1, nodeID(v1.(error)))
	}
}

// TestConcurrentEvaluation evaluates many programs over one activation from
// several goroutines (run with -race): views only read the expr structs,
// and request.query is parsed once under concurrency (03 req 38, 39).
func TestConcurrentEvaluation(t *testing.T) {
	env := testEnv(t)
	srcs := []string{
		`request.query.tag == "a,b"`,
		`request.headers.map(k, k).size() == 3`,
		`request.body.map(k, k)[0] == "a"`,
		`consumer.labels.org == "acme" && route.labels.map(k, k)[0] == "env"`,
		`steps.order.body.id == 7`,
		`auth.claims.scope.split(" ").exists(s, s == "orders:read")`,
	}
	prgs := make([]cel.Program, len(srcs))
	for i, src := range srcs {
		prgs[i] = compile(t, env, src)
	}
	v := fullVars()
	v.Request.SetRawQuery("tag=a&tag=b")
	a := NewActivation(v)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i, prg := range prgs {
				out, _, err := prg.Eval(a)
				if err != nil || out != types.True {
					t.Errorf("%s = %v, %v", srcs[i], out, err)
				}
			}
		})
	}
	wg.Wait()
}
