// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"context"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// sinceSchema is a schema of level 1 with real kind names, so the since
// rules can be tested before any committed field carries x-ruralz-since.
const sinceSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Gateway"]}},
  "allOf": [
    {"if": {"properties": {"kind": {"const": "Gateway"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Gateway"}}
  ],
  "$defs": {
    "Meta": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {
      "name": {"type": "string"},
      "labels": {"type": "object", "additionalProperties": {"type": "string"}},
      "annotations": {"type": "object", "additionalProperties": {"type": "string"}}}},
    "Gateway": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Gateway"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"$ref": "#/$defs/GatewaySpec"}}},
    "GatewaySpec": {"type": "object", "additionalProperties": false, "properties": {
      "port": {"type": "integer"},
      "mode": {"type": "string", "default": "fast", "x-ruralz-since": 1},
      "extra": {"type": "string", "x-ruralz-since": 1},
      "tuning": {"$ref": "#/$defs/Tuning"},
      "items": {"type": "array", "items": {"$ref": "#/$defs/Tuning"}, "x-ruralz-list": {"type": "atomic"}},
      "named": {"type": "array", "items": {"$ref": "#/$defs/Tuning"}, "x-ruralz-list": {"type": "map", "key": "name"}},
      "bag": {"type": "array", "items": {"$ref": "#/$defs/Tuning"}, "x-ruralz-list": {"type": "set"}}}},
    "Tuning": {"type": "object", "additionalProperties": false, "properties": {
      "name": {"type": "string"},
      "depth": {"type": "integer", "default": 3, "x-ruralz-since": 1},
      "limits": {"type": "object", "default": {"b": 1, "a": 2.50}, "x-ruralz-since": 1,
        "properties": {"a": {"type": "number"}, "b": {"type": "integer"}}}}}
  }
}`

// sinceState builds a state of one Gateway over sinceSchema, its Revision
// encoded with since omission.
func sinceState(t *testing.T, idx *schemaidx.Index, spec string) *hub.Validated {
	t.Helper()
	src := `{"apiVersion": "test/v1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": ` + spec + `}`
	files := &tree.FileTable{}
	id := files.Add(tree.File{Path: "gw.json", Role: tree.RoleBase})
	docs, ds, err := profile.Parse(context.Background(), []byte(src), id, "gw.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatalf("parse %s: %v %s", spec, err, texts(ds))
	}
	b := hub.NewBundle([]*hub.Resource{{ID: hub.ID{Kind: v1alpha1.KindGateway, Name: "edge"}, Tree: docs[0].Root, Source: hub.Source{File: "gw.json"}}})
	rev, err := canonical.NewEncoder(idx).Encode(context.Background(), b, canonical.Options{Level: idx.Level()})
	if err != nil {
		t.Fatalf("encode %s: %v", spec, err)
	}
	return &hub.Validated{Bundle: b, Chains: map[string]*hub.Chain{}, Revision: rev}
}

// TestSinceOmission: at a schema level above 0 the comparison leaves out
// the since members at their defaults, as the canonical form does, and
// shows values in canonical form.
func TestSinceOmission(t *testing.T) {
	idx, err := schemaidx.Load("test/v1", []byte(sinceSchema))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if idx.Level() != 1 {
		t.Fatalf("Level() = %d, want 1", idx.Level())
	}
	e, err := New(idx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		from, to string
		want     []string
	}{
		{"default written or not", `{"port": 1, "mode": "fast"}`, `{"port": 2}`, []string{"~ spec.port: 1 -> 2"}},
		{"default left", `{"port": 1, "mode": "fast"}`, `{"port": 1, "mode": "slow"}`, []string{"+ spec.mode: slow"}},
		{"since without default", `{"port": 1}`, `{"port": 1, "extra": ""}`, []string{`+ spec.extra: ""`}},
		{"nested in atomic list", `{"port": 1, "items": [{"name": "a", "depth": 3}]}`, `{"port": 2, "items": [{"name": "a"}]}`, []string{"~ spec.port: 1 -> 2"}},
		{"object default", `{"port": 1, "tuning": {"limits": {"a": 2.5, "b": 1}}}`, `{"port": 2, "tuning": {}}`, []string{"~ spec.port: 1 -> 2"}},
		{"in map entries", `{"port": 1, "named": [{"name": "a", "depth": 3}]}`, `{"port": 1, "named": [{"name": "a", "depth": 4}]}`, []string{"+ spec.named[name=a].depth: 4"}},
		{"in set elements", `{"port": 1, "bag": [{"name": "a", "depth": 3}]}`, `{"port": 2, "bag": [{"name": "a"}]}`, []string{"~ spec.port: 1 -> 2"}},
		{"shown in canonical form", `{"port": 1}`, `{"port": 1, "tuning": {"name": "x", "depth": 3, "limits": {"a": 2.50, "b": 1}}}`, []string{`+ spec.tuning: {"name":"x"}`}},
		{"set element shown in canonical form", `{"port": 1, "bag": []}`, `{"port": 1, "bag": [{"name": "y", "depth": 3}]}`, []string{`+ spec.bag[item={"name":"y"}]`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := sinceState(t, idx, tc.from), sinceState(t, idx, tc.to)
			if from.Revision.Digest == to.Revision.Digest {
				t.Fatal("fixture: equal Revisions take the digest shortcut")
			}
			d, err := e.Compare(context.Background(), Side{State: from}, Side{State: to})
			if err != nil {
				t.Fatalf("Compare() error = %v", err)
			}
			sameLines(t, "ops", opLines(t, d, "Gateway/edge"), tc.want)
		})
	}
}
