// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FuzzParseKey: any operator-supplied signing key value parses to a key
// that signs a token its public key verifies, or is refused with one of
// the fixed refusals (refusalTexts), which repeat no part of a value;
// nothing panics.
func FuzzParseKey(f *testing.F) {
	for _, c := range keyCases(f) {
		f.Add(c.value)
	}
	texts := refusalTexts()
	f.Fuzz(func(t *testing.T, value []byte) {
		k, err := parseKey(value)
		if err != nil {
			if !texts[err.Error()] {
				t.Fatalf("refusal %q is not one of the fixed refusals", err)
			}
			return
		}
		var alg v1alpha1.JWTSignAlgorithm
		switch k.family {
		case familyRSA:
			alg = v1alpha1.JWTSignAlgorithmRS256
		case familyP256:
			alg = v1alpha1.JWTSignAlgorithmES256
		case familyEd25519:
			alg = v1alpha1.JWTSignAlgorithmEdDSA
		default:
			t.Fatalf("accepted a key of family %v", k.family)
		}
		head, err := header(alg, "")
		if err != nil {
			t.Fatal(err)
		}
		s := signer{alg: alg, head: head}
		tok, err := s.appendToken(nil, k, []byte(`{"exp":1}`))
		if err != nil {
			t.Fatalf("an accepted key does not sign: %v", err)
		}
		verify(t, string(tok), alg, k.signer.Public())
	})
}

// FuzzPathsMode: for any body and path, the paths mode either passes the
// body unchanged, or writes a strict JSON body in which each value the
// path matches is a token that verifies, or cannot decide with an error
// type of the package documentation, never changing the body then.
func FuzzPathsMode(f *testing.F) {
	exp := expIn(time.Hour)
	for _, s := range []struct{ body, path string }{
		{`{"t":{"exp":` + exp + `}}`, "t"},
		{`{"l":[{"exp":` + exp + `},{"exp":` + exp + `,"a":[1]}]}`, "l.*"},
		{`{"a":{"b":{"exp":` + exp + `}},"c":"x"}`, "a.b"},
		{`{"t":"x"}`, "t"},
		{`[{"exp":1}]`, "0"},
		{`{"t":{"sub":"no exp"}}`, "t"},
		{`{ "t" : { "exp" : 1 , "s" : "é" } }`, "t"},
		{`not json`, "t"},
	} {
		f.Add([]byte(s.body), s.path)
	}
	k := keys(f)
	st := newStore(pkcs8(f, k.ed))
	f.Fuzz(func(t *testing.T, body []byte, path string) {
		if _, err := dotpath.Parse(path); err != nil {
			return
		}
		flt, err := Factory{}.Build(t.Context(), buildEnv(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", path), st))
		if err != nil {
			t.Fatalf("Build(%q): %v", path, err)
		}
		fl := flt.(*Filter)
		defer func() { _ = fl.Close() }()
		x, m := onResponse(t, http.StatusOK, jsonHeader(), "")
		m.Raw = bytes.Clone(body)
		res := fl.Handle(t.Context(), phase.OnResponse, x)
		switch res.Outcome {
		case filter.Continue:
		case filter.CannotDecide:
			switch errorType(res) {
			case errTypePath, errTypeInvalidValue, errTypeNullBody, errTypeOutputCap:
			default:
				t.Fatalf("cannot decide with error type %q: %v", errorType(res), res.Err)
			}
			if !bytes.Equal(m.Raw, body) || m.setBodies != 0 {
				t.Fatal("a failed Policy changed the body")
			}
			return
		default:
			t.Fatalf("outcome %v", res.Outcome)
		}
		if m.setBodies == 0 {
			if !bytes.Equal(m.Raw, body) {
				t.Fatal("the body changed without SetBody")
			}
			return
		}
		if !json.Valid(m.Raw) {
			t.Fatalf("rewritten body is not JSON: %s", m.Raw)
		}
		var ed dotpath.Editor
		p, _ := dotpath.Parse(path)
		matches, n, err := ed.Collect(m.Raw, p, nil)
		if err != nil || n == 0 {
			t.Fatalf("rewritten body %s: path matches %d, %v", m.Raw, n, err)
		}
		var toks []string
		if err := json.Unmarshal(append(append([]byte{'['}, matches...), ']'), &toks); err != nil {
			t.Fatalf("matches %s are not all token strings: %v", matches, err)
		}
		for _, tok := range toks {
			if strings.Count(tok, ".") != 2 {
				t.Fatalf("match %q is not a compact JWS", tok)
			}
			verify(t, tok, v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public())
		}
	})
}
