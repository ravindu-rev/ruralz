// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build go1.27

package jsonval

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
)

// Differential tests against the standard library's strict tokenizer,
// encoding/json/jsontext. Production code cannot import it under go 1.26.0
// (architecture convention 1, R-30), so it serves only as this go1.27 test
// oracle; the Go 1.26 floor job skips the file.

// oracleValid reports whether jsontext accepts data as one JSON value with
// the given duplicate policy. Its nesting limit is fixed at MaxDepthLimit.
func oracleValid(data []byte, allowDup bool) bool {
	return jsontext.Value(data).IsValid(jsontext.AllowDuplicateNames(allowDup))
}

// agree compares the scanner with jsontext on data. jsontext's depth limit
// is fixed, so the scanner runs at MaxDepthLimit, and a rejection at the
// default depth must be ErrDepth or a rejection jsontext shares.
func agree(t *testing.T, name string, data []byte) {
	t.Helper()
	for _, dup := range []bool{false, true} {
		deep := Validate(data, ScanOptions{MaxDepth: MaxDepthLimit, AllowDuplicateNames: dup})
		theirs := oracleValid(data, dup)
		if (deep == nil) != theirs {
			t.Fatalf("%s (duplicates allowed %v): jsonval error %v, jsontext accept = %v", name, dup, deep, theirs)
		}
		def := Validate(data, ScanOptions{AllowDuplicateNames: dup})
		if def != nil && theirs && !errors.Is(def, ErrDepth) {
			t.Fatalf("%s (duplicates allowed %v): default rejection %v, jsontext accepts", name, dup, def)
		}
		if def == nil && !theirs {
			t.Fatalf("%s (duplicates allowed %v): default accepts, jsontext rejects", name, dup)
		}
	}
}

// TestOracleConformance checks that the scanner and jsontext agree on
// accept and reject for every JSON conformance case, with duplicate names
// rejected and allowed (WP-02 "Done when").
func TestOracleConformance(t *testing.T) {
	for _, c := range conformanceCases() {
		agree(t, c.name, []byte(c.input))
	}
	// The nesting limit sits at the same depth in both.
	for _, depth := range []int{MaxDepthLimit - 1, MaxDepthLimit, MaxDepthLimit + 1} {
		agree(t, "depth", []byte(strings.Repeat("[", depth)+strings.Repeat("]", depth)))
	}
}

// oracleCanonical returns jsontext's RFC 8785 form of data.
func oracleCanonical(data []byte) ([]byte, error) {
	v := jsontext.Value(bytes.Clone(data))
	if err := v.Canonicalize(); err != nil {
		return nil, err
	}
	return v, nil
}

// checkCanonical compares AppendCanonical of the decoded input with
// jsontext's canonicalization (02 req 24; 02 test plan item 5), integer
// literals beyond ±(2^53−1) included. Inputs with a literal that overflows a
// double, which has no canonical form, are skipped.
func checkCanonical(t *testing.T, data []byte) {
	t.Helper()
	v, _, err := Decode(data, Options{MaxDepth: MaxDepthLimit})
	if err != nil {
		return
	}
	ours, err := AppendCanonical(nil, v)
	if errors.Is(err, ErrNumberRange) {
		return
	}
	if err != nil {
		t.Fatalf("AppendCanonical(%q): %v", data, err)
	}
	theirs, err := oracleCanonical(data)
	if err != nil {
		t.Fatalf("jsontext rejects %q: %v", data, err)
	}
	if !bytes.Equal(ours, theirs) {
		t.Fatalf("canonical form of %q:\n jsonval  %s\n jsontext %s", data, ours, theirs)
	}
}

func TestOracleCanonical(t *testing.T) {
	for _, c := range conformanceCases() {
		if c.accept {
			checkCanonical(t, []byte(c.input))
		}
	}
	for _, s := range canonicalCorpus() {
		checkCanonical(t, []byte(s))
	}
	checkCanonical(t, readOrders(t))
}

// canonicalCorpus holds inputs that exercise member order and number
// formatting in the RFC 8785 form.
func canonicalCorpus() []string {
	return []string{
		`{"\u20ac":1,"\r":2,"\ufb33":3,"1":4,"\ud83d\ude00":5,"\u0080":6,"\u00f6":7}`,
		`[0,-0,1.0,100,1E2,0.05,0.000001,1e-7,1e21,1e20,333333333.3333333,5e-324,1.7976931348623157e308,1.2345678901234568e20]`,
		`[9007199254740991,-9007199254740991,1e23,9.999999999999997e+22,1.0000000000000001e23,0.1,0.2,0.30000000000000004]`,
		`{"b":[{"z":1,"a":2}],"a":{"y":"<&>\u2028","x":"\u0000\u001f\u007f\"\\/"}}`,
		`"\ud834\udd1e\u00e9\t"`,
		`[1e-400,-1e-400,4.9e-324,2.2250738585072014e-308]`,
		`[9007199254740992,-9007199254740992,9007199254740993,-9007199254740993,123456789012345678901,18446744073709551616,11e17]`,
	}
}

// FuzzOracle checks, for arbitrary input, that the scanner and jsontext
// agree on accept and reject, and that accepted input has the same RFC 8785
// form in both, together with the encoding/json v1 checks of FuzzDecode.
func FuzzOracle(f *testing.F) {
	for _, c := range conformanceCases() {
		if len(c.input) < 1024 {
			f.Add([]byte(c.input))
		}
	}
	for _, s := range canonicalCorpus() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		agree(t, "fuzz input", data)
		checkCanonical(t, data)
		if Validate(data, ScanOptions{}) == nil && !json.Valid(data) {
			t.Fatalf("accepted input encoding/json rejects: %q", data)
		}
	})
}
