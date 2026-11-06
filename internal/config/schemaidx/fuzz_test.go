// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
)

// FuzzLoad: arbitrary schema bytes never panic or hang; a schema that
// loads enumerates finitely (Load bounds the fields), and every enumerated
// field resolves by Lookup on the instance its path and Where describe
// (instanceFor) when its schema path splits back unambiguously
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
		// A kind's schema reached again through a dispatch result below it.
		`{"allOf":[{"if":{"properties":{"kind":{"const":""}},"required":["kind"]},"then":{"properties":{"00000000":{"$ref":"#/$defs/d0"}}}}],"$defs":{"d0":{"properties":{"":{"$ref":"#"}}}}}`,
		// Nested dispatch with else branches through the root.
		`{"allOf":[{"if":{"properties":{"kind":{"const":""}},"required":["kind"]},"then":{"$ref":"#/$defs/T"}}],"$defs":{"T":{"properties":{"0":{},"1":{}},"allOf":[{"if":{"properties":{"0":{"const":0}},"required":["0"]},"else":{"properties":{"g":{"const":true}}},"then":{"properties":{"g":{"$ref":"#"}}}}]}}}`,
		// Rules whose conditions hold together: one repeating another's,
		// one testing a subset of another's; their else branches apply
		// only when neither holds.
		`{"allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"spec":{"$ref":"#/$defs/S"}}}}],"$defs":{"S":{"additionalProperties":false,"allOf":[{"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"then":{"properties":{"x":{}}}},{"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"else":{"properties":{"y":{}}}}]}}}`,
		`{"allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"spec":{"$ref":"#/$defs/S"}}}}],"$defs":{"S":{"additionalProperties":false,"allOf":[{"if":{"properties":{"t":{"const":"a"},"u":{"const":1}},"required":["t","u"]},"then":{"properties":{"x":{}}}},{"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"else":{"properties":{"y":{}}}}]}}}`,
		// A branch giving the member its rule tests a schema with its own
		// dispatch: the member holds the const there, so nothing below it.
		`{"allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"spec":{"$ref":"#/$defs/S"}}}}],"$defs":{"S":{"properties":{"t":{"properties":{"u":{}}}},"allOf":[{"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"then":{"properties":{"t":{"$ref":"#/$defs/T"}}}}]},"T":{"additionalProperties":false,"allOf":[{"if":{"properties":{"u":{"const":"b"}},"required":["u"]},"then":{"properties":{"w":{}}}}]}}}`,
		// A typed map that also declares the member name k.
		`{"allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"m":{"properties":{"k":{"const":1}},"additionalProperties":{"properties":{"z":{}}}}}}}]}`,
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
		key := ""
		if check {
			key = freeName(x)
		}
		i := 0
		for fld := range x.FieldsSeq() {
			if i++; i > 200 {
				break
			}
			if !check {
				continue
			}
			res, path := instanceFor(fld, key)
			if _, ok := x.Lookup(fld.Kind, res, path); !ok {
				t.Fatalf("enumerated field %s %q (where %+v) does not resolve", fld.Kind, fld.Path, fld.Where)
			}
		}
	})
}

// FuzzWalkLookup: any instance tree walks without panic under every kind of
// the committed schema, and Lookup of the path of each position that path
// names yields the Info Walk reported. Lookup, like tree.At, follows a path
// through the first member of a name, the first keyed-list entry with a
// key and the first equal set element, so a path names the position Walk
// visits first with it, provided its parent's path names the parent; a
// later duplicate member name, keyed-list key or set element, and every
// position below one, shares the path of an earlier position.
func FuzzWalkLookup(f *testing.F) {
	for _, s := range []string{
		`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"q"},"spec":{"type":"quota","config":{"consumerQuota":"c","extra":[1,{"a":2}]}}}`,
		`{"spec":{"listeners":[{"name":"h","port":80,"tls":{"certificates":[{"name":"c","privateKey":{"secretRef":{"provider":"env","name":"K"}}}]}}],"trustedProxies":["10.0.0.0/8",1.5]}}`,
		`{"spec":{"upstreams":[{"name":"a"},{"name":"a"},{"weight":2}],"policies":[{"name":"p"}],"match":{"methods":["GET","GET"],"headers":[{"name":1}]}}}`,
		`{"metadata":{"labels":{"a":"b"},"namespace":"x"},"spec":{"credentials":{"apiKeys":[{"name":"k","secretRef":{"name":"n"}}]}}}`,
		`[1,[2,{"a":null}]]`, `"s"`, `{"spec":{"type":["auth.jwt"],"config":{"issuers":[{"issuer":"i","audiences":["a"]}]}}}`,
		// A later duplicate member and keyed-list entry dispatch otherwise.
		`{"spec":{"type":"quota"},"spec":{"type":"auth.jwt","config":{"issuers":[]}}}`,
		`{"spec":{"composition":{"steps":[{"name":"a"},{"name":"a","collection":true,"group":"g"}]}}}`,
		// Rules matching together: two test spec.type against "plugin"
		// (PolicySpec), two test op against "move" and two against
		// "append" (ArrayOp).
		`{"spec":{"type":"plugin"}}`,
		`{"spec":{"type":"transform.request","config":{"arrayOps":[{"op":"move","from":"a","to":"b"},{"op":"append"},{"op":"delete"}]}}}`,
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
			// named[d] reports whether the path of the current position at
			// depth d names it: Walk visits it first with that path, and
			// named[d-1] holds for its parent.
			var named []bool
			err := x.Walk(kind, res, func(c *Cursor) error {
				p := c.Path()
				key := p.String()
				seen[key]++
				d := c.Depth()
				named = append(named[:d], seen[key] == 1 && (d == 0 || named[d-1]))
				if !named[d] {
					return nil
				}
				l, ok := x.Lookup(kind, res, p)
				switch {
				case ok && l != c.Info:
					t.Fatalf("%s %s: Lookup %+v != Walk %+v", kind, key, l, c.Info)
				case !ok && (c.Info.Known() || c.Info.Free):
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
