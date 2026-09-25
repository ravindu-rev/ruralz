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
