// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"encoding/json"
	"errors"
	"testing"
)

// Tests for file references with key (spec 01 requirement 44: the file is
// a JSON object whose member key is a JSON string; spec 06 requirement
// 89: parse errors are fixed messages that never contain file content).

func TestJSONMember(t *testing.T) {
	tests := []struct {
		name, doc, key string
		want           string
		err            error
	}{
		{"string", `{"password":"p4ss"}`, "password", "p4ss", nil},
		{"escapes", `{"k":"a\nbé\"\\"}`, "k", "a\nbé\"\\", nil},
		{"empty string", `{"k":""}`, "k", "", nil},
		{"other members skipped", `{"a":{"b":[1,2,{"c":null}]},"k":"v","z":true}`, "k", "v", nil},
		{"whitespace", " \n{ \"k\" : \"v\" }\n\t", "k", "v", nil},
		{"nested key not top level", `{"a":{"k":"v"}}`, "k", "", errJSONMissing},
		{"missing", `{"a":"b"}`, "k", "", errJSONMissing},
		{"empty object", `{}`, "k", "", errJSONMissing},
		{"duplicate", `{"k":"a","k":"b"}`, "k", "", errJSONDuplicate},
		{"duplicate other member allowed", `{"a":"1","a":"2","k":"v"}`, "k", "v", nil},
		{"number", `{"k":6379}`, "k", "", errJSONNotString},
		{"null", `{"k":null}`, "k", "", errJSONNotString},
		{"object", `{"k":{"v":"x"}}`, "k", "", errJSONNotString},
		{"array root", `["k"]`, "k", "", errJSONNotObject},
		{"string root", `"k"`, "k", "", errJSONNotObject},
		{"empty", ``, "k", "", errJSONNotObject},
		{"trailing value", `{"k":"v"}{}`, "k", "", errJSONNotObject},
		{"trailing garbage", `{"k":"v"} x`, "k", "", errJSONNotObject},
		{"truncated", `{"k":"v"`, "k", "", errJSONNotObject},
		{"bad value", `{"a":tru,"k":"v"}`, "k", "", errJSONNotObject},
		{"bad key", `{1:"v"}`, "k", "", errJSONNotObject},
		{"invalid UTF-8", "{\"k\":\"\xff\"}", "k", "", errJSONNotUTF8},
		// Req 44: the value is the UTF-8 bytes of the string; a lone
		// surrogate escape has none (encoding/json would give U+FFFD).
		{"lone high surrogate", `{"k":"a\ud800b"}`, "k", "", errJSONSurrogate},
		{"lone high surrogate at end", `{"k":"\uD800"}`, "k", "", errJSONSurrogate},
		{"lone low surrogate", `{"k":"\udc00"}`, "k", "", errJSONSurrogate},
		{"reversed pair", `{"k":"\udc00\ud800"}`, "k", "", errJSONSurrogate},
		{"high then non-surrogate escape", `{"k":"\ud800\u0041"}`, "k", "", errJSONSurrogate},
		{"high then other escape", `{"k":"\ud800\n"}`, "k", "", errJSONSurrogate},
		{"two highs then low", `{"k":"\ud83d\ud83d\ude00"}`, "k", "", errJSONSurrogate},
		{"surrogate pair", `{"k":"\ud83d\ude00"}`, "k", "\U0001F600", nil},
		{"upper-case pair", `{"k":"\uD83D\uDE00x"}`, "k", "\U0001F600x", nil},
		{"escaped backslash before u", `{"k":"\\ud800"}`, "k", `\ud800`, nil},
		{"BMP escape", `{"k":"\u00e9\uFFFD"}`, "k", "\u00e9\ufffd", nil},
		{"lone surrogate in another member", `{"a":"\ud800","k":"v"}`, "k", "v", nil},
		{"PEM file with key", "-----BEGIN CERTIFICATE-----\nMIIB\n", "k", "", errJSONNotObject},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jsonMember([]byte(tc.doc), tc.key)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err == nil && string(got) != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

// FuzzJSONMember extracts a member from arbitrary documents: no panic; a
// failure is one of the fixed reasons; a success agrees with
// encoding/json on the same document when the key is not duplicated.
func FuzzJSONMember(f *testing.F) {
	f.Add([]byte(`{"password":"p4ss","user":"u"}`), "password")
	f.Add([]byte(`{"k":"a","k":"b"}`), "k")
	f.Add([]byte(`{"k":{"k":"v"}}`), "k")
	f.Add([]byte(`{"k":"\ud800"}`), "k")
	f.Add([]byte(`[]`), "")
	fixed := map[error]bool{
		errJSONNotUTF8: true, errJSONNotObject: true, errJSONMissing: true,
		errJSONDuplicate: true, errJSONNotString: true, errJSONSurrogate: true,
	}
	f.Fuzz(func(t *testing.T, doc []byte, key string) {
		got, err := jsonMember(doc, key)
		if err != nil {
			if !fixed[err] {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(doc, &m); err != nil {
			t.Fatalf("jsonMember accepted a document encoding/json refuses: %v", err)
		}
		var want string
		if err := json.Unmarshal(m[key], &want); err != nil {
			t.Fatalf("member does not decode as a string: %v", err)
		}
		if string(got) != want {
			t.Fatalf("value %q, encoding/json %q", got, want)
		}
	})
}
