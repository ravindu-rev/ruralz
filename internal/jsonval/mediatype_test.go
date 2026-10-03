// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import "testing"

func TestIsJSONMediaType(t *testing.T) {
	// 07 req 44 and 78 (T13 content types); 03 req 25.
	for ct, want := range map[string]bool{
		"application/json":                    true,
		"application/JSON":                    true,
		"Application/Json; charset=utf-8":     true,
		"application/json; charset=UTF-16":    true, // parameters are the caller's (07 req 78)
		"  application/json\t":                true,
		"application/json;":                   true,
		"application/problem+json":            true,
		"application/vnd.api+json":            true,
		"application/vnd.api+JSON; version=2": true,
		"text/x+json":                         true,
		"text/json":                           false,
		"text/plain":                          false,
		"application/jsonx":                   false,
		"application/json-seq":                false,
		"application/+json":                   false,
		"application/x-json":                  false,
		"application/json+xml":                false,
		"json":                                false,
		"":                                    false,
		"/json":                               false,
		"application/":                        false,
		"application/js on":                   false,
		"appli cation/json":                   false,
		"application/json/x":                  false,
		"application\x00/json":                false,
		"application/é+json":                  false,
	} {
		if got := IsJSONMediaType(ct); got != want {
			t.Fatalf("IsJSONMediaType(%q) = %v, want %v", ct, got, want)
		}
	}
}
