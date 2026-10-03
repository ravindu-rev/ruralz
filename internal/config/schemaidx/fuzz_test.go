// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
)

// FuzzLoad: arbitrary schema bytes never panic or hang; a schema that
// loads enumerates finitely (Load bounds the fields), and every enumerated
// field resolves by Lookup when its schema path splits back unambiguously
// (plainNames). The fields are read lazily, so a large but admitted
// enumeration costs only the 200 fields checked.
func FuzzLoad(f *testing.F) {
	f.Add(schema.RenderedV1alpha1())
	f.Add([]byte(synthetic))
	f.Add([]byte(dagSchema(4, 1)))
	for _, s := range []string{
		`{}`, `true`, `{"$ref":"#"}`, `{"properties":{"a":{"$ref":"#"}}}`,
		`{"allOf":[{"if":{"properties":{"t":{"const":1}},"required":["t"]},"then":{"properties":{"c":{"$ref":"#"}}},"else":{"$ref":"#"}}]}`,
		`{"items":{"$ref":"#"},"additionalProperties":{"$ref":"#/items"}}`,
		`{"anyOf":[{"type":"integer"},{"$ref":"#"}]}`,
		`{"properties":{"f":{"x-ruralz-list":{"type":"map","key":"k"},"items":{"properties":{"k":{}}}}}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		x, err := Load("test/v1", data)
		if err != nil {
			if !errors.Is(err, ErrSchema) {
				t.Fatalf("Load error without ErrSchema: %v", err)
			}
			return
		}
		check := plainNames(x)
		i := 0
		for fld := range x.FieldsSeq() {
			if i++; i > 200 {
				break
			}
			if !check {
				continue
			}
			res, path := instanceFor(fld)
			if _, ok := x.Lookup(fld.Kind, res, path); !ok {
				t.Fatalf("enumerated field %s %q does not resolve", fld.Kind, fld.Path)
			}
		}
	})
}

// FuzzWalkLookup: any instance tree walks without panic under every kind of
// the committed schema, and Lookup of each uniquely visited path yields the
// Info Walk reported.
func FuzzWalkLookup(f *testing.F) {
	for _, s := range []string{
		`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"q"},"spec":{"type":"quota","config":{"consumerQuota":"c","extra":[1,{"a":2}]}}}`,
		`{"spec":{"listeners":[{"name":"h","port":80,"tls":{"certificates":[{"name":"c","privateKey":{"secretRef":{"provider":"env","name":"K"}}}]}}],"trustedProxies":["10.0.0.0/8",1.5]}}`,
		`{"spec":{"upstreams":[{"name":"a"},{"name":"a"},{"weight":2}],"policies":[{"name":"p"}],"match":{"methods":["GET","GET"],"headers":[{"name":1}]}}}`,
		`{"metadata":{"labels":{"a":"b"},"namespace":"x"},"spec":{"credentials":{"apiKeys":[{"name":"k","secretRef":{"name":"n"}}]}}}`,
		`[1,[2,{"a":null}]]`, `"s"`, `{"spec":{"type":["auth.jwt"],"config":{"issuers":[{"issuer":"i","audiences":["a"]}]}}}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 4096 || !json.Valid([]byte(src)) {
			return
		}
		res := parseTree(t, src)
		x := v1(t)
		for _, kind := range x.Kinds() {
			seen := map[string]int{}
			infos := map[string]Info{}
			err := x.Walk(kind, res, func(c *Cursor) error {
				p := c.Path()
				key := p.String()
				seen[key]++
				infos[key] = c.Info
				if l, ok := x.Lookup(kind, res, p); ok && seen[key] == 1 && l != c.Info {
					t.Fatalf("%s %s: Lookup %+v != Walk %+v", kind, key, l, c.Info)
				} else if !ok && (c.Info.Known() || c.Info.Free) && !strings.Contains(key, "[") {
					t.Fatalf("%s %s: Walk resolved but Lookup did not", kind, key)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}
