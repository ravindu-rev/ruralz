// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// synthGateway is a Gateway of the synthetic schema whose since fields
// all hold their defaults.
const synthGateway = `{"apiVersion": "test/v1", "kind": "Gateway",
  "metadata": {"name": "edge", "owner": "platform"},
  "spec": {"port": 80, "mode": "fast",
    "tuning": {"name": "t", "depth": 3, "limits": {"a": 2.5, "b": 1}},
    "items": [{"name": "a", "depth": 3}, {"name": "b", "depth": 4}],
    "byName": {"x": {"depth": 3}},
    "open": {"mode": "fast"}}}`

// synthEncode encodes docs at the synthetic schema's level.
func synthEncode(t *testing.T, docs ...string) []byte {
	t.Helper()
	rev, err := NewEncoder(syntheticIndex(t)).Encode(context.Background(), bundleOf(t, docs...), Options{Level: 2, Workers: 2})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	return rev.Content
}

// TestSinceOmission: a field with x-ruralz-since above 0 is written only
// when it differs from its default by canonical bytes; without a default
// only when present; level-0 fields always when present.
func TestSinceOmission(t *testing.T) {
	got := synthEncode(t, synthGateway)
	want := doc(`{"kind":"Gateway","metadata":{"name":"edge"},"spec":{"byName":{"x":{}},"items":[{"name":"a"},{"depth":4,"name":"b"}],"open":{"mode":"fast"},"port":80,"tuning":{"name":"t"}}}`)
	sameBytes(t, "at defaults", got, []byte(want))

	changed := synthEncode(t, `{"kind": "Gateway", "metadata": {"name": "edge", "owner": "team-a"},
	  "spec": {"mode": "slow", "extra": "", "tuning": {"depth": 4, "limits": {"a": 1}}}}`)
	want = doc(`{"kind":"Gateway","metadata":{"name":"edge","owner":"team-a"},"spec":{"extra":"","mode":"slow","tuning":{"depth":4,"limits":{"a":1}}}}`)
	sameBytes(t, "changed", changed, []byte(want))

	// Absent without a default stays absent; an absent parent is not
	// created.
	bare := synthEncode(t, `{"kind": "Gateway", "metadata": {"name": "edge"}, "spec": {}}`)
	sameBytes(t, "bare", bare, []byte(doc(`{"kind":"Gateway","metadata":{"name":"edge"},"spec":{}}`)))
}

// TestSinceRoundTrip: content with omitted since fields decodes and
// re-encodes identically; re-materializing the defaults (stage G on
// FromResources) restores the fields and still gives the same bytes;
// writing a since field at its default is not canonical form (31(b)
// and 33).
func TestSinceRoundTrip(t *testing.T) {
	idx := syntheticIndex(t)
	ctx := context.Background()
	content := synthEncode(t, synthGateway, `{"kind": "Route", "metadata": {"name": "r"}, "spec": {"anything": [1, {"mode": "fast"}]}}`)
	dec := NewDecoder(idx, DecoderOptions{Workers: 2})
	rs, err := dec.Decode(ctx, content, Source{Name: "c.json"})
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].APIVersion != "test/v1" {
		t.Errorf("APIVersion = %q", rs[0].APIVersion)
	}
	enc := NewEncoder(idx)
	again, err := enc.EncodeResources(ctx, rs, Options{Level: 2})
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "decode then encode", again.Content, content)

	materializeSince(t, idx, rs)
	if mode, _ := rs[0].Root.At(path("spec", "mode")); mode == nil || mode.Text != "fast" || mode.Style != tree.StyleDefaulted {
		t.Errorf("restored spec.mode = %+v", mode)
	}
	restored, err := enc.EncodeResources(ctx, rs, Options{Level: 2})
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "materialized", restored.Content, content)

	explicit := strings.Replace(string(content), `"port":80`, `"mode":"fast","port":80`, 1)
	if _, err := dec.Decode(ctx, []byte(explicit), Source{}); codeOf(err) != CodeMismatch {
		t.Errorf("since field at its default: error = %v, want %s", err, CodeMismatch)
	}
}

// TestSinceAboveLevel: a field above the served level, or unknown to the
// schema, is content from a newer producer.
func TestSinceAboveLevel(t *testing.T) {
	idx := syntheticIndex(t)
	content := synthEncode(t, `{"kind": "Gateway", "metadata": {"name": "edge"}, "spec": {"future": "x", "port": 1}}`)
	dec := NewDecoder(idx, DecoderOptions{})
	if _, err := dec.Decode(context.Background(), content, Source{}); err != nil {
		t.Fatalf("level-2 decoder: %v", err)
	}
	dec.level = 1
	_, err := dec.Decode(context.Background(), content, Source{})
	ds := DiagnosticsOf(err)
	if codeOf(err) != CodeNewer || len(ds) != 1 || ds[0].Path.String() != "spec.future" || !strings.Contains(ds[0].Message, "at schema level 2, above this release's level 1") {
		t.Errorf("level-1 decoder: error = %v", err)
	}
	unknown := []byte(doc(`{"kind":"Gateway","metadata":{"name":"edge"},"spec":{"port":1,"zz":1}}`))
	_, err = NewDecoder(idx, DecoderOptions{}).Decode(context.Background(), unknown, Source{})
	if ds := DiagnosticsOf(err); codeOf(err) != CodeNewer || len(ds) != 1 || !strings.Contains(ds[0].Message, "unknown to test/v1 at schema level 2") {
		t.Errorf("unknown field: error = %v", err)
	}
}

// TestSinceMetadataStrip: since omission and the conversion-data strip
// combine in metadata.
func TestSinceMetadataStrip(t *testing.T) {
	got := synthEncode(t, `{"kind": "Gateway", "metadata": {"name": "edge", "owner": "platform", "annotations": {"ruralz.io/conversion-data": "{}"}}, "spec": {}}`)
	sameBytes(t, "metadata", got, []byte(doc(`{"kind":"Gateway","metadata":{"name":"edge"},"spec":{}}`)))
	// An omission check that meets a value without canonical form fails
	// with its path.
	_, err := NewEncoder(syntheticIndex(t)).Encode(context.Background(),
		bundleOf(t, `{"kind": "Gateway", "metadata": {"name": "edge"}, "spec": {"tuning": {"limits": {"a": null}}}}`), Options{Level: 2})
	if err == nil || !strings.Contains(err.Error(), "Gateway/edge spec.tuning.limits.a: null") {
		t.Errorf("null under a since field: error = %v", err)
	}
}

// path returns a path of fields.
func path(names ...string) diag.Path {
	out := make(diag.Path, len(names))
	for i, n := range names {
		out[i] = diag.Field(n)
	}
	return out
}

// materializeSince adds the default of every absent since field inside a
// present object, as stage G does on canonical re-entry.
func materializeSince(t *testing.T, idx *schemaidx.Index, rs []tree.Resource) {
	t.Helper()
	for _, r := range rs {
		err := idx.Walk(string(r.ID.Kind), r.Root, func(c *schemaidx.Cursor) error {
			if c.Node.Kind != tree.KindMap {
				return nil
			}
			for name, p := range c.Schema().Defaults() {
				if _, ok := c.Node.Get(name); !ok && p.Since() > 0 {
					c.Node.Set(name, tree.Pos{}, p.DefaultNode())
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// recursiveSince is a level-1 schema whose Gateway spec nests since
// positions in one another: N's member c is an N at schema level 1 with
// the default {}.
const recursiveSince = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Gateway"]}},
  "allOf": [
    {"if": {"properties": {"kind": {"const": "Gateway"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Gateway"}}
  ],
  "$defs": {
    "Meta": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {"name": {"type": "string"}}},
    "Gateway": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Gateway"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"$ref": "#/$defs/N"}}},
    "N": {"type": "object", "additionalProperties": false, "properties": {
      "c": {"$ref": "#/$defs/N", "x-ruralz-since": 1, "default": {}},
      "v": {"type": "integer"}}}
  }
}`

// TestSinceLinear: since omission costs one encoding of each value, so
// encoding and decoding a spec whose since positions nest depth deep is
// linear in depth; encoding each since position's whole value once to
// compare it with the default and again to write it would cost about
// 2^depth. The bytes Encode and Decode allocate, the least of three runs,
// grow by less than a factor of 3 from 6 to 12 and from 12 to 18 levels,
// where doubling per level would make it some 64 (counted bytes, not
// time); the values written grow by the same number from one depth to the
// next, and a value at its default is still left out at every level.
func TestSinceLinear(t *testing.T) {
	idx, err := schemaidx.Load("test/v1", []byte(recursiveSince))
	if err != nil {
		t.Fatal(err)
	}
	enc := NewEncoder(idx)
	dec := NewDecoder(idx, DecoderOptions{Workers: 1})
	spec := func(depth int) string {
		return strings.Repeat(`{"c":`, depth) + `{"v":1}` + strings.Repeat(`}`, depth)
	}
	gateway := func(depth int) string {
		return `{"apiVersion":"test/v1","kind":"Gateway","metadata":{"name":"g"},"spec":` + spec(depth) + `}`
	}
	least := func(fn func()) float64 {
		best := ^uint64(0)
		for range 3 {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			fn()
			runtime.ReadMemStats(&after)
			best = min(best, after.TotalAlloc-before.TotalAlloc)
		}
		return float64(best)
	}
	var encBytes, decBytes []float64
	var written []int
	for _, depth := range []int{6, 12, 18} {
		b := bundleOf(t, gateway(depth))
		var content []byte
		encBytes = append(encBytes, least(func() {
			rev, err := enc.Encode(context.Background(), b, Options{Level: 1})
			if err != nil {
				t.Fatal(err)
			}
			content = rev.Content
		}))
		if want := doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":` + spec(depth) + `}`); string(content) != want {
			t.Fatalf("depth %d: %s, want %s", depth, content, want)
		}
		decBytes = append(decBytes, least(func() {
			if _, err := dec.Decode(context.Background(), content, Source{}); err != nil {
				t.Fatal(err)
			}
		}))
		var o observed
		enc.observe = o.add
		now, after := due()
		if _, err := enc.Encode(context.Background(), b, Options{Level: 1, Now: now, YieldAfter: after}); err != nil {
			t.Fatal(err)
		}
		enc.observe = nil
		sites, _, _ := total(o.take())
		written = append(written, sites[siteValue])
	}
	for i := 1; i < 3; i++ {
		if r := encBytes[i] / encBytes[i-1]; r >= 3 {
			t.Errorf("Encode: %.0f bytes at %d levels, %.0f at %d: %.1f times, want linear", encBytes[i], 6*(i+1), encBytes[i-1], 6*i, r)
		}
		if r := decBytes[i] / decBytes[i-1]; r >= 3 {
			t.Errorf("Decode: %.0f bytes at %d levels, %.0f at %d: %.1f times, want linear", decBytes[i], 6*(i+1), decBytes[i-1], 6*i, r)
		}
	}
	if written[2]-written[1] != written[1]-written[0] || written[1]-written[0] != 6 {
		t.Errorf("values written at 6, 12 and 18 levels: %v, want 6 more per 6 levels", written)
	}
	// A spec at its default, nested in since positions, is left out level
	// by level: {"c":{"c":{}}} has the default {} innermost, so the inner c
	// is omitted, which leaves {"c":{}} at the default again, and so on.
	at := bundleOf(t, `{"apiVersion":"test/v1","kind":"Gateway","metadata":{"name":"g"},"spec":{"c":{"c":{"c":{}}},"v":2}}`)
	rev, err := enc.Encode(context.Background(), at, Options{Level: 1})
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "nested defaults", rev.Content, []byte(doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":{"v":2}}`)))
}
