// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"90s"`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d) != 90*time.Second {
		t.Fatalf("got %v", time.Duration(d))
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"1m30s"` {
		t.Fatalf("canonical form = %s, want \"1m30s\"", out)
	}
	for _, bad := range []string{`"-1s"`, `"soon"`, `5`} {
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want error", bad)
		}
	}
}

func TestByteSize(t *testing.T) {
	cases := map[string]ByteSize{
		`0`:       0,
		`1024`:    1024,
		`"64Ki"`:  64 << 10,
		`"10Mi"`:  10 << 20,
		`"1Gi"`:   1 << 30,
		`"1.5Gi"`: 3 << 29,
		`"2k"`:    2000,
		`"3M"`:    3_000_000,
		`"512"`:   512,
		`"0.5Ki"`: 512,
		`"7Ei"`:   7 << 60,
	}
	for in, want := range cases {
		var got ByteSize
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Unmarshal(%s) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{`-1`, `1.5`, `"1.5"`, `"10MB"`, `"Mi"`, `"1e3"`, `"0.3Ki"`, `"8Ei"`, `"1.Ki"`, `".5Ki"`, `true`} {
		var got ByteSize
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("Unmarshal(%s) = %d, want error", bad, got)
		}
	}
	out, err := json.Marshal(ByteSize(10 << 20))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "10485760" {
		t.Errorf("canonical form = %s", out)
	}
}

func TestIntOrString(t *testing.T) {
	var v IntOrString
	if err := json.Unmarshal([]byte(`8080`), &v); err != nil || v.IsString || v.Int != 8080 {
		t.Fatalf("got %+v, %v", v, err)
	}
	if err := json.Unmarshal([]byte(`"http"`), &v); err != nil || !v.IsString || v.Str != "http" {
		t.Fatalf("got %+v, %v", v, err)
	}
	if err := json.Unmarshal([]byte(`true`), &v); err == nil {
		t.Fatal("bool accepted")
	}
	for _, want := range []string{`8080`, `"http"`} {
		var in IntOrString
		if err := json.Unmarshal([]byte(want), &in); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(in)
		if err != nil || string(out) != want {
			t.Errorf("round trip %s = %s, %v", want, out, err)
		}
	}
}

func TestDecodeRoute(t *testing.T) {
	const doc = `{
	  "apiVersion": "ruralz/v1alpha1",
	  "kind": "Route",
	  "metadata": {"name": "orders"},
	  "spec": {
	    "match": {"path": {"prefix": "/v1/orders"}, "methods": ["GET"]},
	    "upstreams": [{"name": "orders", "weight": 100}],
	    "timeout": "5s"
	  }
	}`
	var r Route
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindRoute || r.APIVersion != APIVersion || r.Metadata.Name != "orders" {
		t.Fatalf("envelope = %+v", r.TypeMeta)
	}
	if r.Spec.Match.Path == nil || r.Spec.Match.Path.Prefix != "/v1/orders" {
		t.Fatalf("match = %+v", r.Spec.Match)
	}
	if *r.Spec.Upstreams[0].Weight != 100 || time.Duration(*r.Spec.Timeout) != 5*time.Second {
		t.Fatalf("spec = %+v", r.Spec)
	}
}

// TestJSONSchemaDocument covers 07 req 74: config.schema is an object or a
// boolean, kept as written.
func TestJSONSchemaDocument(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{`{"type":"object","required":["id"]}`, `{"type":"object","required":["id"]}`, true},
		{` {"type": "string"} `, `{"type": "string"}`, true},
		{`true`, `true`, true},
		{`false`, `false`, true},
		{`{}`, `{}`, true},
		{`"object"`, ``, false},
		{`1`, ``, false},
		{`[{"type":"object"}]`, ``, false},
		{`truex`, ``, false},
		// A direct call is checked as encoding/json would check it.
		{`{garbage`, ``, false},
		{`{"a":1} {}`, ``, false},
		{`{"a":}`, ``, false},
	}
	for _, c := range cases {
		var d JSONSchemaDocument
		err := d.UnmarshalJSON([]byte(c.in))
		if (err == nil) != c.ok {
			t.Errorf("UnmarshalJSON(%s) err = %v, want ok %v", c.in, err, c.ok)
			continue
		}
		if c.ok && string(d) != c.want {
			t.Errorf("UnmarshalJSON(%s) = %s, want %s", c.in, d, c.want)
		}
	}
}

func TestJSONSchemaDocumentNullAndMarshal(t *testing.T) {
	d := JSONSchemaDocument(`true`)
	if err := d.UnmarshalJSON([]byte(`null`)); err != nil || string(d) != "true" {
		t.Fatalf("null changed the document to %s, %v", d, err)
	}
	var empty JSONSchemaDocument
	out, err := json.Marshal(empty)
	if err != nil || string(out) != "null" {
		t.Fatalf("empty document = %s, %v", out, err)
	}
	var cfg ValidationJSONSchemaConfig
	if err := json.Unmarshal([]byte(`{"schema": {"type": "object", "properties": {"id": {"type": "integer"}}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	out, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"schema":{"type":"object","properties":{"id":{"type":"integer"}}}}`; string(out) != want {
		t.Fatalf("round trip = %s, want %s", out, want)
	}
	if err := json.Unmarshal([]byte(`{"schema": "yes"}`), &cfg); err == nil {
		t.Fatal("a string schema was accepted")
	}
}

// TestJSONSchemaDocumentCopies checks the document does not alias the
// decoder's buffer.
func TestJSONSchemaDocumentCopies(t *testing.T) {
	buf := []byte(`{"a":1}`)
	var d JSONSchemaDocument
	if err := d.UnmarshalJSON(buf); err != nil {
		t.Fatal(err)
	}
	buf[2] = 'b'
	if string(d) != `{"a":1}` {
		t.Fatalf("document aliases its input: %s", d)
	}
}

// FuzzParseByteSize checks ParseByteSize never panics, never returns a
// negative size, and that an accepted plain integer round-trips.
func FuzzParseByteSize(f *testing.F) {
	for _, s := range []string{"0", "10Mi", "64Ki", "1.5Gi", "7Ei", "8Ei", "1e3", "0.3Ki", "", "Mi", "99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParseByteSize(s)
		if err != nil {
			return
		}
		if b < 0 {
			t.Fatalf("ParseByteSize(%q) = %d", s, b)
		}
		var again ByteSize
		out, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &again); err != nil || again != b {
			t.Fatalf("round trip of %q: %d -> %s -> %d, %v", s, b, out, again, err)
		}
	})
}

// FuzzJSONSchemaDocument checks that UnmarshalJSON never panics, that it
// keeps only valid JSON objects and booleans, that marshaling a kept
// document then succeeds (07 req 74), and that a rejected input leaves the
// document unchanged.
func FuzzJSONSchemaDocument(f *testing.F) {
	for _, s := range []string{`{}`, `true`, `false`, `null`, `"x"`, `[1]`, ` {"a":1} `, ``, `{garbage`, `{"a":1}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d := JSONSchemaDocument(`true`)
		if err := d.UnmarshalJSON(data); err != nil {
			if string(d) != "true" {
				t.Fatalf("rejected %q but changed the document to %q", data, d)
			}
			return
		}
		if !json.Valid(d) {
			t.Fatalf("kept invalid JSON %q", d)
		}
		if d[0] != '{' && string(d) != "true" && string(d) != "false" {
			t.Fatalf("kept %q", d)
		}
		if out, err := json.Marshal(d); err != nil || !json.Valid(out) {
			t.Fatalf("marshaling kept %q = %q, %v", d, out, err)
		}
	})
}
