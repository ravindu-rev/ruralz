// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// testLimit is the maxRequestBodyBytes of buildFilter.
const testLimit = 1 << 20

// mustCompile compiles raw or fails the test.
func mustCompile(t testing.TB, raw string) *Schema {
	t.Helper()
	s, err := CompileSchema([]byte(raw))
	if err != nil {
		t.Fatalf("CompileSchema(%.80q) = %v", raw, err)
	}
	return s
}

// compileIssues compiles raw and returns its findings.
func compileIssues(t testing.TB, raw string) []Issue {
	t.Helper()
	_, _, issues := compileRaw([]byte(raw), refusingLoader{})
	return issues
}

// configOf returns the config of a Policy with schema.
func configOf(schema string) *v1alpha1.ValidationJSONSchemaConfig {
	return &v1alpha1.ValidationJSONSchemaConfig{Schema: v1alpha1.JSONSchemaDocument(schema)}
}

// buildFilter builds a Filter for schema with maxRequestBodyBytes limit.
func buildFilter(t testing.TB, schema string, limit int64) *Filter {
	t.Helper()
	f, err := Factory{}.Build(context.Background(), filter.BuildEnv{
		Config: &v1alpha1.ValidationJSONSchemaConfig{Schema: v1alpha1.JSONSchemaDocument(schema)},
		Limits: filter.Limits{MaxRequestBodyBytes: limit},
	})
	if err != nil {
		t.Fatalf("Build(%.80q) = %v", schema, err)
	}
	return f.(*Filter)
}

// exchange returns an Exchange whose request message has the
// Content-Type ct (none when ct is "-") and body.
func exchange(ct string, body []byte) (*filtertest.Exchange, *filtertest.Message) {
	m := &filtertest.Message{Hdr: http.Header{}, Raw: body}
	if ct != "-" {
		m.Hdr["Content-Type"] = []string{ct}
	}
	return &filtertest.Exchange{ID: strings.Repeat("ab", 16), Msg: m}, m
}

// handle runs f in onRequestBody on a JSON body.
func handle(t testing.TB, f *Filter, body string) filter.Result {
	t.Helper()
	x, _ := exchange("application/json", []byte(body))
	return f.Handle(t.Context(), phase.OnRequestBody, x)
}

// detailOf returns the RZ-RT-009 detail of a 400, failing otherwise.
func detailOf(t testing.TB, r filter.Result) string {
	t.Helper()
	if r.Outcome != filter.Respond || r.Response == nil {
		t.Fatalf("result = %+v, want Respond", r)
	}
	if r.Response.Status != http.StatusBadRequest || r.Response.Code != CodeRejected || r.Response.Body != nil {
		t.Fatalf("response = %+v, want 400 %s problem", r.Response, CodeRejected)
	}
	return r.Response.Detail
}
