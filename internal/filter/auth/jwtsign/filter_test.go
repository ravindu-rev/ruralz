// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the auth.jwt-sign Filter (docs/architecture/08-security-and-
// identity.md "JWT signing"): both modes, the body rules, the claims
// rules and the failures.

// claimsJSON is a claims object an hour ahead, as an Upstream writes it.
func claimsJSON() string { return `{"sub":"alice","exp":` + expIn(time.Hour) + `,"roles":["a","b"]}` }

// TestPathsModeEachAlgorithm: with config.paths, the object at the path
// becomes the compact JWS of exactly its bytes, which verifies under the
// key's public key for every algorithm and carries the kid; the rest of
// the body keeps its member order and text, and Content-Length follows.
func TestPathsModeEachAlgorithm(t *testing.T) {
	for _, c := range algCases(t) {
		t.Run(string(c.alg), func(t *testing.T) {
			f := build(t, pathsConfig(c.alg, "edge-1", "token"), newStore(c.value))
			x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"id":7,"token":`+claimsJSON()+`,"z":0.10}`)
			wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
			tok := tokenAt(t, m.Raw, "token")
			p := verify(t, tok, c.alg, c.pub)
			if string(p.payload) != claimsJSON() {
				t.Errorf("payload = %s, want the object %s", p.payload, claimsJSON())
			}
			if p.header["kid"] != "edge-1" {
				t.Errorf("kid = %v, want edge-1", p.header["kid"])
			}
			if want := `{"id":7,"token":"` + tok + `","z":0.10}`; string(m.Raw) != want {
				t.Errorf("body = %s, want %s", m.Raw, want)
			}
			if got := m.Header().Get("Content-Length"); got != strconv.Itoa(len(m.Raw)) || m.setBodies != 1 {
				t.Errorf("Content-Length = %q for %d bytes, %d SetBody calls", got, len(m.Raw), m.setBodies)
			}
		})
	}
}

// TestPathsModeWithoutKid: without config.keyId the header holds alg
// alone.
func TestPathsModeWithoutKid(t *testing.T) {
	k := keys(t)
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "token"), newStore(pkcs8(t, k.ed)))
	x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"token":`+claimsJSON()+`}`)
	wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
	p := verify(t, tokenAt(t, m.Raw, "token"), v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public())
	if _, ok := p.header["kid"]; ok || len(p.header) != 1 {
		t.Errorf("header = %v, want alg alone", p.header)
	}
}

// TestPathsModeNormalizesClaims: the claims are the object as the body
// holds it in the transform form: whitespace dropped, member order and
// number text kept.
func TestPathsModeNormalizesClaims(t *testing.T) {
	k := keys(t)
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(pkcs8(t, k.ed)))
	exp := expIn(time.Hour)
	x, m := onResponse(t, http.StatusOK, jsonHeader(), "{ \"t\" : { \"b\" : 1.50 , \"a\" : \"\\u0041\" , \"exp\" : "+exp+" } }")
	wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
	p := verify(t, tokenAt(t, m.Raw, "t"), v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public())
	if want := `{"b":1.50,"a":"A","exp":` + exp + `}`; string(p.payload) != want {
		t.Errorf("payload = %s, want %s", p.payload, want)
	}
}

// TestPathsModeManyPaths: every path is applied, nested paths and a star
// included, each object signed on its own; members no path names are
// kept.
func TestPathsModeManyPaths(t *testing.T) {
	k := keys(t)
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmES256, "", "access", "session.id", "items.*.claims"), newStore(sec1(t, k.p256)))
	obj := func(sub string) string { return `{"sub":"` + sub + `","exp":` + expIn(time.Hour) + `}` }
	body := `{"access":` + obj("a") + `,"session":{"id":` + obj("s") + `,"n":1},"items":[{"claims":` + obj("i0") + `},{"x":0},{"claims":` + obj("i2") + `}]}`
	x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
	wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
	for _, c := range []struct {
		path []any
		sub  string
	}{
		{[]any{"access"}, "a"}, {[]any{"session", "id"}, "s"}, {[]any{"items", 0, "claims"}, "i0"}, {[]any{"items", 2, "claims"}, "i2"},
	} {
		p := verify(t, tokenAt(t, m.Raw, c.path...), v1alpha1.JWTSignAlgorithmES256, k.p256.Public())
		if string(p.payload) != obj(c.sub) {
			t.Errorf("%v payload = %s, want %s", c.path, p.payload, obj(c.sub))
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(m.Raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["session"].(map[string]any)["n"] != 1.0 || len(doc["items"].([]any)[1].(map[string]any)) != 1 {
		t.Errorf("members no path names changed: %s", m.Raw)
	}
}

// TestPathsApplyInOrder: each path reads the document the one before it
// wrote, so a path inside an earlier one's match signs into its claims,
// and a path through an earlier one's token is a path error.
func TestPathsApplyInOrder(t *testing.T) {
	k := keys(t)
	inner := `{"sub":"inner","exp":` + expIn(time.Hour) + `}`
	body := `{"a":{"b":` + inner + `,"exp":` + expIn(time.Hour) + `}}`
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "a.b", "a"), newStore(pkcs8(t, k.ed)))
	x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
	wantNext(t, "inner first", f.Handle(t.Context(), phase.OnResponse, x))
	outer := verify(t, tokenAt(t, m.Raw, "a"), v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public())
	var claims map[string]any
	if err := json.Unmarshal(outer.payload, &claims); err != nil {
		t.Fatal(err)
	}
	nested, _ := claims["b"].(string)
	if p := verify(t, nested, v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public()); string(p.payload) != inner {
		t.Errorf("nested payload = %s, want %s", p.payload, inner)
	}
	g := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "a", "a.b"), newStore(pkcs8(t, k.ed)))
	x, m = onResponse(t, http.StatusOK, jsonHeader(), body)
	wantUndecided(t, "outer first", g.Handle(t.Context(), phase.OnResponse, x), errTypePath)
	if string(m.Raw) != body || m.setBodies != 0 {
		t.Errorf("a failed Policy changed the body: %s", m.Raw)
	}
}

// TestPathsModeNothingToSign: a body no path matches, a missing member, a
// name on an array, an index past the end and a star over an empty array
// keep the body's bytes, unnormalized, with no SetBody.
func TestPathsModeNothingToSign(t *testing.T) {
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "token", "l.claims", "l.5", "e.*.c"), newStore(pkcs8(t, keys(t).ed)))
	for _, body := range []string{
		`{ "id" : 1 }`,
		`{"l":[{"a":1}],"e":[]}`,
		`[1, 2]`,
	} {
		x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
		wantNext(t, body, f.Handle(t.Context(), phase.OnResponse, x))
		if string(m.Raw) != body || m.setBodies != 0 {
			t.Errorf("%s: body = %s after %d SetBody calls, want it unchanged", body, m.Raw, m.setBodies)
		}
	}
}

// TestPathsModeNotAnObject: a match that is not an object, a path through
// a scalar and a star on an object are path errors; nothing is written.
func TestPathsModeNotAnObject(t *testing.T) {
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "token"), newStore(pkcs8(t, keys(t).ed)))
	g := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "token.claims", "s.*"), newStore(pkcs8(t, keys(t).ed)))
	for _, c := range []struct {
		f    *Filter
		body string
	}{
		{f, `{"token":"already"}`},
		{f, `{"token":12}`},
		{f, `{"token":null}`},
		{f, `{"token":[` + claimsJSON() + `]}`},
		{g, `{"token":1}`},
		{g, `{"s":{"a":{}}}`},
	} {
		x, m := onResponse(t, http.StatusOK, jsonHeader(), c.body)
		wantUndecided(t, c.body, c.f.Handle(t.Context(), phase.OnResponse, x), errTypePath)
		if m.setBodies != 0 {
			t.Errorf("%s: SetBody called", c.body)
		}
	}
}

// TestClaimsModeVariables: config.claims reads response with its status,
// headers and body, consumer and auth, and the token it builds is written
// at target, creating the missing objects.
func TestClaimsModeVariables(t *testing.T) {
	k := keys(t)
	src := `{"sub": auth.claims.sub, "consumer": consumer.name, "tier": consumer.tier, "status": response.status,` +
		` "order": response.body.id, "trace": response.headers["x-trace"], "exp": auth.claims.exp}`
	f := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmPS256, src, "session.token"), newStore(pkcs8(t, k.rsa)))
	h := jsonHeader()
	h.Set("X-Trace", "t-1")
	x, m := onResponse(t, http.StatusCreated, h, `{"id":42,"session":{"n":1}}`)
	wantNext(t, "claims", f.Handle(t.Context(), phase.OnResponse, x))
	tok := tokenAt(t, m.Raw, "session", "token")
	p := verify(t, tok, v1alpha1.JWTSignAlgorithmPS256, k.rsa.Public())
	want := `{"consumer":"acme","exp":` + expIn(time.Hour) + `,"order":42,"status":201,"sub":"alice","tier":"gold","trace":"t-1"}`
	if string(p.payload) != want {
		t.Errorf("payload = %s, want %s", p.payload, want)
	}
	if string(m.Raw) != `{"id":42,"session":{"n":1,"token":"`+tok+`"}}` {
		t.Errorf("body = %s", m.Raw)
	}
}

// TestClaimsModeEmptyBody: an empty body starts from an empty object and
// takes Content-Type application/json.
func TestClaimsModeEmptyBody(t *testing.T) {
	k := keys(t)
	f := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"sub": consumer.name, "exp": auth.claims.exp}`, "token"), newStore(pkcs8(t, k.ed)))
	x, m := onResponse(t, http.StatusOK, http.Header{"Content-Type": {"text/plain"}}, "")
	wantNext(t, "empty body", f.Handle(t.Context(), phase.OnResponse, x))
	verify(t, tokenAt(t, m.Raw, "token"), v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public())
	if got := m.Header().Values("Content-Type"); len(got) != 1 || got[0] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// TestClaimsModeStarTarget: a target with a star writes the one token at
// every match, and over an empty array writes nothing.
func TestClaimsModeStarTarget(t *testing.T) {
	f := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"exp": auth.claims.exp}`, "l.*.t"), newStore(pkcs8(t, keys(t).ed)))
	x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"l":[{},{}]}`)
	wantNext(t, "star", f.Handle(t.Context(), phase.OnResponse, x))
	if a, b := tokenAt(t, m.Raw, "l", 0, "t"), tokenAt(t, m.Raw, "l", 1, "t"); a != b {
		t.Errorf("tokens differ: %s, %s", a, b)
	}
	x, m = onResponse(t, http.StatusOK, jsonHeader(), `{"l":[]}`)
	wantNext(t, "empty array", f.Handle(t.Context(), phase.OnResponse, x))
	if m.setBodies != 0 || string(m.Raw) != `{"l":[]}` {
		t.Errorf("body = %s after %d SetBody calls", m.Raw, m.setBodies)
	}
}

// TestClaimsModeFailures: a result that is not a map, a runtime error, a
// target through a scalar or a name on an array, and a non-JSON body
// cannot decide, each with its error type, and write nothing.
func TestClaimsModeFailures(t *testing.T) {
	ed := pkcs8(t, keys(t).ed)
	cases := []struct {
		name, claims, target string
		ct, body             string
		anonymous            bool
		kind                 string
	}{
		{"a string result", `"token"`, "t", "application/json", `{}`, false, errTypeResultType},
		{"a list result", `[auth.claims.exp]`, "t", "application/json", `{}`, false, errTypeResultType},
		{"a null result", `response.body.missing`, "t", "application/json", `{"missing":null}`, false, errTypeResultType},
		{"an int keyed map with a string twin", `{1: "a", "1": "b", "exp": auth.claims.exp}`, "t", "application/json", `{}`, false, errTypeResultType},
		{"an anonymous request", `{"sub": consumer.name, "exp": 1}`, "t", "application/json", `{}`, true, errTypeCEL},
		{"a missing body member", `{"o": response.body.nope.x, "exp": 1}`, "t", "application/json", `{}`, false, errTypeCEL},
		{"a target through a scalar", `{"exp": auth.claims.exp}`, "a.b", "application/json", `{"a":1}`, false, errTypePath},
		{"a target name on an array", `{"exp": auth.claims.exp}`, "a", "application/json", `[1]`, false, errTypePath},
		{"a body that is not JSON", `{"exp": auth.claims.exp}`, "t", "text/html", `<p>`, false, errTypeNullBody},
		{"invalid JSON under a JSON type", `{"exp": auth.claims.exp}`, "t", "application/json", `{"a":`, false, errTypeNullBody},
		{"no exp", `{"sub": "a"}`, "t", "application/json", `{}`, false, errTypeInvalidValue},
		{"exp too far ahead", `{"exp": response.body.far}`, "t", "application/json", `{"far":` + expIn(48*time.Hour) + `}`, false, errTypeInvalidValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, c.claims, c.target), newStore(ed))
			x, m := onResponse(t, http.StatusOK, http.Header{"Content-Type": {c.ct}}, c.body)
			if c.anonymous {
				x.VarsV.Consumer, x.VarsV.Auth = nil, nil
			}
			wantUndecided(t, c.name, f.Handle(t.Context(), phase.OnResponse, x), c.kind)
			if m.setBodies != 0 || string(m.Raw) != c.body {
				t.Errorf("a failed Policy changed the body to %s", m.Raw)
			}
		})
	}
}

// TestExpRule: claims without an exp NumericDate, or with one more than
// MaxExpiry ahead of the request's start, are refused in both modes; one
// at the bound, and one that passed, are signed.
func TestExpRule(t *testing.T) {
	ed := pkcs8(t, keys(t).ed)
	paths := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(ed))
	claims := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `response.body.c`, "t"), newStore(ed))
	for _, c := range []struct {
		name, obj string
		ok        bool
	}{
		{"no exp", `{"sub":"a"}`, false},
		{"exp a string", `{"exp":"1"}`, false},
		{"exp a day and a second ahead", `{"exp":` + expIn(MaxExpiry+time.Second) + `}`, false},
		{"exp at the bound", `{"exp":` + expIn(MaxExpiry) + `}`, true},
		{"exp passed", `{"exp":` + expIn(-time.Minute) + `}`, true},
	} {
		x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"t":`+c.obj+`}`)
		res := paths.Handle(t.Context(), phase.OnResponse, x)
		if c.ok {
			wantNext(t, "paths "+c.name, res)
		} else {
			wantUndecided(t, "paths "+c.name, res, errTypeInvalidValue)
			if m.setBodies != 0 {
				t.Errorf("paths %s: SetBody called", c.name)
			}
		}
		x, _ = onResponse(t, http.StatusOK, jsonHeader(), `{"c":`+c.obj+`}`)
		res = claims.Handle(t.Context(), phase.OnResponse, x)
		if c.ok {
			wantNext(t, "claims "+c.name, res)
		} else {
			wantUndecided(t, "claims "+c.name, res, errTypeInvalidValue)
		}
	}
}

// TestBodyRules: a response that cannot have content, a 206, a response
// with Content-Range and a generated response pass unchanged in both
// modes; a non-JSON body cannot decide in paths mode; an empty body passes
// in paths mode.
func TestBodyRules(t *testing.T) {
	ed := pkcs8(t, keys(t).ed)
	paths := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(ed))
	claims := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"exp": auth.claims.exp}`, "t"), newStore(ed))
	body := `{"t":` + claimsJSON() + `}`
	pass := []struct {
		name   string
		status int
		method string
		header http.Header
		gen    bool
	}{
		{"204", http.StatusNoContent, http.MethodGet, nil, false},
		{"304", http.StatusNotModified, http.MethodGet, nil, false},
		{"103", http.StatusEarlyHints, http.MethodGet, nil, false},
		{"206", http.StatusPartialContent, http.MethodGet, nil, false},
		{"Content-Range", http.StatusOK, http.MethodGet, http.Header{"content-range": {"bytes 0-1/2"}}, false},
		{"HEAD", http.StatusOK, http.MethodHead, nil, false},
		{"generated", http.StatusUnauthorized, http.MethodGet, nil, true},
	}
	for _, f := range []*Filter{paths, claims} {
		for _, c := range pass {
			h := jsonHeader()
			for k, v := range c.header {
				h[k] = v
			}
			x, m := onResponse(t, c.status, h, body)
			x.MethodV = c.method
			m.Gen = c.gen
			wantNext(t, c.name, f.Handle(t.Context(), phase.OnResponse, x))
			if m.setBodies != 0 || string(m.Raw) != body {
				t.Errorf("%s: body changed to %s", c.name, m.Raw)
			}
		}
	}
	for _, ct := range [][]string{{"text/plain"}, {"application/json", "application/json"}, nil} {
		x, _ := onResponse(t, http.StatusOK, http.Header{"Content-Type": ct}, body)
		wantUndecided(t, "Content-Type "+strings.Join(ct, ", "), paths.Handle(t.Context(), phase.OnResponse, x), errTypeNullBody)
	}
	x, m := onResponse(t, http.StatusOK, jsonHeader(), "")
	wantNext(t, "empty body", paths.Handle(t.Context(), phase.OnResponse, x))
	if m.setBodies != 0 {
		t.Error("an empty body was rewritten in paths mode")
	}
	wantNext(t, "another Phase", paths.Handle(t.Context(), phase.OnRequestHeaders, x))
}

// TestValidators: a rewritten response turns each strong ETag weak and
// loses Accept-Ranges, folding lines held under differently cased keys;
// SetBody drops Content-Encoding and the body digests.
func TestValidators(t *testing.T) {
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(pkcs8(t, keys(t).ed)))
	h := jsonHeader()
	h["Etag"] = []string{`"strong"`}
	h["etag"] = []string{`W/"weak"`}
	h["Accept-Ranges"] = []string{"bytes"}
	h["Content-Encoding"] = []string{"identity"}
	h["Content-Digest"] = []string{"sha-256=:x:"}
	x, m := onResponse(t, http.StatusOK, h, `{"t":`+claimsJSON()+`}`)
	wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
	got := m.Header()
	if et := got.Values("Etag"); len(et) != 2 || et[0] != `W/"strong"` || et[1] != `W/"weak"` {
		t.Errorf("ETag = %q, want both weak", et)
	}
	for k := range got {
		if k != http.CanonicalHeaderKey(k) {
			t.Errorf("key %q was not folded into its canonical key", k)
		}
	}
	for _, k := range []string{"Accept-Ranges", "Content-Encoding", "Content-Digest"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s kept on a rewritten body", k)
		}
	}
}

// TestOutputCap: a rewritten body past limits.maxResponseBodyBytes cannot
// decide (output_cap), in both modes.
func TestOutputCap(t *testing.T) {
	ed := pkcs8(t, keys(t).ed)
	body := `{"t":` + claimsJSON() + `}`
	for _, cfg := range []*v1alpha1.AuthJWTSignConfig{
		pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"),
		claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"exp": auth.claims.exp}`, "u"),
	} {
		env := buildEnv(t, cfg, newStore(ed))
		env.Limits.MaxResponseBodyBytes = int64(len(body) + 20)
		flt, err := Factory{}.Build(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		f := flt.(*Filter)
		x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
		wantUndecided(t, "over the cap", f.Handle(t.Context(), phase.OnResponse, x), errTypeOutputCap)
		if m.setBodies != 0 {
			t.Error("SetBody called past the cap")
		}
		_ = f.Close()
	}
}

// TestBufferSentinels: a spent buffer budget or an oversized body, when
// the body is read, decoded for CEL or replaced, is the SPI sentinel.
func TestBufferSentinels(t *testing.T) {
	ed := pkcs8(t, keys(t).ed)
	paths := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(ed))
	claims := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"x": response.body.t, "exp": auth.claims.exp}`, "u"), newStore(ed))
	body := `{"t":` + claimsJSON() + `}`
	for _, sentinel := range []error{filter.ErrBudget, filter.ErrTooLarge} {
		x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
		m.bodyErr = sentinel
		if res := paths.Handle(t.Context(), phase.OnResponse, x); res.Outcome != filter.CannotDecide || !errors.Is(res.Err, sentinel) {
			t.Errorf("Body %v: Handle = %+v", sentinel, res)
		}
		x, m = onResponse(t, http.StatusOK, jsonHeader(), body)
		m.setBodyErr = sentinel
		if res := paths.Handle(t.Context(), phase.OnResponse, x); res.Outcome != filter.CannotDecide || !errors.Is(res.Err, sentinel) {
			t.Errorf("SetBody %v: Handle = %+v", sentinel, res)
		}
		// response.body not decoded for lack of budget reads as a body
		// error of CEL, which is the sentinel.
		x, m = onResponse(t, http.StatusOK, jsonHeader(), body)
		x.VarsV.Response.Body = nil
		m.decodedErr = sentinel
		if res := claims.Handle(t.Context(), phase.OnResponse, x); res.Outcome != filter.CannotDecide || !errors.Is(res.Err, sentinel) {
			t.Errorf("Decoded %v: Handle = %+v (error type %q)", sentinel, res, errorType(res))
		}
	}
	x, m := onResponse(t, http.StatusOK, jsonHeader(), body)
	m.bodyErr = errors.New("leg reset")
	wantUndecided(t, "another body error", paths.Handle(t.Context(), phase.OnResponse, x), errTypeInternal)
}

// TestCanceledContext: a request that ended stops signing.
func TestCanceledContext(t *testing.T) {
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(pkcs8(t, keys(t).ed)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"t":`+claimsJSON()+`}`)
	if res := f.Handle(ctx, phase.OnResponse, x); res.Outcome != filter.CannotDecide || !errors.Is(res.Err, context.Canceled) || m.setBodies != 0 {
		t.Errorf("Handle = %+v", res)
	}
}

// TestClaimsCompile: config.claims reads response, consumer and auth only,
// a dyn place; another variable is RZ-CFG-014, a static cost over the
// ceiling RZ-CFG-015, both at Build, and nothing keeps running.
func TestClaimsCompile(t *testing.T) {
	for _, c := range []struct{ src, code string }{
		{`{"m": request.method, "exp": 1}`, CodeCEL},
		{`{"ip": source.ip, "exp": 1}`, CodeCEL},
		{`{"r": route.name, "exp": 1}`, CodeCEL},
		{`{"t": now, "exp": 1}`, CodeCEL},
		{`{"exp": 1,`, CodeCEL},
		{`{"n": response.body.a.map(x, response.body.a.map(y, response.body.a.map(z, x + y + z))), "exp": 1}`, CodeCost},
	} {
		st := newStore(pkcs8(t, keys(t).ed))
		_, err := Factory{}.Build(t.Context(), buildEnv(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, c.src, "t"), st))
		if code, _ := errcode.CodeOf(err); code != c.code {
			t.Errorf("Build(%s) = %v, want %s", c.src, err, c.code)
		}
		if st.live() != 0 {
			t.Errorf("Build(%s) left a watch running", c.src)
		}
	}
	env := buildEnv(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, `{"exp": 1}`, "t"), newStore(pkcs8(t, keys(t).ed)))
	env.CEL = nil
	if _, err := (Factory{}).Build(t.Context(), env); !errors.Is(err, ErrNoCEL) {
		t.Errorf("Build without CEL = %v, want ErrNoCEL", err)
	}
}

// TestClaimsRuntimeCostLimit: an expression within the static ceiling
// that a large response drives past the runtime cost limit stops with a
// CEL error, which cannot decide.
func TestClaimsRuntimeCostLimit(t *testing.T) {
	src := `{"n": response.body.items.filter(x, x == x).size(), "exp": auth.claims.exp}`
	f := build(t, claimsConfig(v1alpha1.JWTSignAlgorithmEdDSA, src, "t"), newStore(pkcs8(t, keys(t).ed)))
	small := `{"items":[1,2,3]}`
	x, _ := onResponse(t, http.StatusOK, jsonHeader(), small)
	wantNext(t, "small body", f.Handle(t.Context(), phase.OnResponse, x))
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := range 400_000 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('0')
	}
	b.WriteString(`]}`)
	x, m := onResponse(t, http.StatusOK, jsonHeader(), b.String())
	res := f.Handle(t.Context(), phase.OnResponse, x)
	wantUndecided(t, "large body", res, errTypeCEL)
	var ee *expr.EvalError
	if !errors.As(res.Err, &ee) || ee.Kind != expr.KindCostLimit {
		t.Errorf("cause = %v, want the cost limit", res.Err)
	}
	if m.setBodies != 0 {
		t.Error("SetBody called after the cost limit")
	}
}

// TestConcurrentHandle: one Filter signs for many requests at once, a
// rotation between them included (run under the race detector).
func TestConcurrentHandle(t *testing.T) {
	k := keys(t)
	st := newStore(pkcs8(t, k.ed))
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), st)
	type call struct {
		x filter.Exchange
		m *message
	}
	calls := make([]call, 64)
	for i := range calls {
		x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"t":{"i":`+strconv.Itoa(i)+`,"exp":`+expIn(time.Hour)+`}}`)
		calls[i] = call{x, m}
	}
	var wg sync.WaitGroup
	errs := make(chan string, len(calls))
	for i := range 16 {
		wg.Go(func() {
			for _, c := range calls[i*4 : i*4+4] {
				if res := f.Handle(context.Background(), phase.OnResponse, c.x); res.Outcome != filter.Continue || !bytes.Contains(c.m.Raw, []byte(`"t":"`)) {
					errs <- "request " + strconv.Itoa(i) + ": " + string(c.m.Raw)
				}
			}
		})
	}
	if err := st.rotate(pkcs8(t, k.ed)); err != nil {
		t.Errorf("rotate: %v", err)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
