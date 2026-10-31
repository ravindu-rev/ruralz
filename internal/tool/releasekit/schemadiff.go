// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// jsonObject is a decoded JSON object.
type jsonObject = map[string]any

// parseSchema decodes a JSON Schema document.
func parseSchema(data []byte) (jsonObject, error) {
	var doc jsonObject
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// object returns v[key] as an object, or nil.
func object(v any, key string) jsonObject {
	o, _ := v.(jsonObject)
	sub, _ := o[key].(jsonObject)
	return sub
}

// rendered is v[key] as compact JSON, or "" when absent.
func rendered(v any, key string) string {
	o, _ := v.(jsonObject)
	x, ok := o[key]
	if !ok {
		return ""
	}
	data, err := json.Marshal(x)
	if err != nil {
		return fmt.Sprint(x)
	}
	return string(data)
}

// list returns v[key] as compact JSON strings of its array elements.
func list(v any, key string) []string {
	o, _ := v.(jsonObject)
	arr, _ := o[key].([]any)
	var out []string
	for _, x := range arr {
		data, err := json.Marshal(x)
		if err != nil {
			continue
		}
		out = append(out, string(data))
	}
	return out
}

// diffLists returns the elements only in b and only in a.
func diffLists(a, b []string) (added, removed []string) {
	for _, x := range b {
		if !slices.Contains(a, x) {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			removed = append(removed, x)
		}
	}
	return added, removed
}

// schemaKinds lists the kinds a schema document serves (properties.kind.enum).
func schemaKinds(doc jsonObject) []string {
	var out []string
	for _, k := range list(object(doc, "properties")["kind"], "enum") {
		var s string
		if json.Unmarshal([]byte(k), &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// levelNote names a property's schema level (x-ruralz-since), if any.
func levelNote(prop any) string {
	if s := rendered(prop, "x-ruralz-since"); s != "" {
		return " at schema level " + s
	}
	return ""
}

// schemaChanges lists the differences between two documents of one
// apiVersion that a Bundle author sees: kinds, definitions, properties,
// enum values, defaults, schema levels and required members, one line
// each, in a fixed order.
func schemaChanges(from, to []byte) ([]string, error) {
	a, err := parseSchema(from)
	if err != nil {
		return nil, fmt.Errorf("old schema: %w", err)
	}
	b, err := parseSchema(to)
	if err != nil {
		return nil, fmt.Errorf("new schema: %w", err)
	}
	var out []string
	added, removed := diffLists(schemaKinds(a), schemaKinds(b))
	for _, k := range added {
		out = append(out, fmt.Sprintf("Kind `%s` added", k))
	}
	for _, k := range removed {
		out = append(out, fmt.Sprintf("Kind `%s` removed", k))
	}

	defsA, defsB := object(a, "$defs"), object(b, "$defs")
	names := slices.Sorted(maps.Keys(defsA))
	for _, n := range slices.Sorted(maps.Keys(defsB)) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		da, inA := defsA[n]
		db, inB := defsB[n]
		switch {
		case !inA:
			out = append(out, fmt.Sprintf("Definition `%s` added%s", n, levelNote(db)))
			continue
		case !inB:
			out = append(out, fmt.Sprintf("Definition `%s` removed", n))
			continue
		}
		vAdded, vRemoved := diffLists(list(da, "enum"), list(db, "enum"))
		for _, v := range vAdded {
			out = append(out, fmt.Sprintf("`%s`: value %s added", n, v))
		}
		for _, v := range vRemoved {
			out = append(out, fmt.Sprintf("`%s`: value %s removed", n, v))
		}
		out = append(out, propertyChanges(n, da, db)...)
	}
	return out, nil
}

// propertyChanges compares the properties of one definition present in
// both documents.
func propertyChanges(def string, da, db any) []string {
	var out []string
	pa, pb := object(da, "properties"), object(db, "properties")
	for _, p := range slices.Sorted(maps.Keys(pb)) {
		if _, ok := pa[p]; !ok {
			out = append(out, fmt.Sprintf("`%s.%s` added%s", def, p, levelNote(pb[p])))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(pa)) {
		if _, ok := pb[p]; !ok {
			out = append(out, fmt.Sprintf("`%s.%s` removed", def, p))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(pa)) {
		xb, ok := pb[p]
		if !ok {
			continue
		}
		xa := pa[p]
		if d1, d2 := rendered(xa, "default"), rendered(xb, "default"); d1 != d2 {
			out = append(out, fmt.Sprintf("`%s.%s`: default changed from %s to %s", def, p, orNone(d1), orNone(d2)))
		}
		if l1, l2 := rendered(xa, "x-ruralz-since"), rendered(xb, "x-ruralz-since"); l1 != l2 {
			out = append(out, fmt.Sprintf("`%s.%s`: schema level changed from %s to %s", def, p, orNone(l1), orNone(l2)))
		}
		vAdded, vRemoved := diffLists(list(xa, "enum"), list(xb, "enum"))
		for _, v := range vAdded {
			out = append(out, fmt.Sprintf("`%s.%s`: value %s added", def, p, v))
		}
		for _, v := range vRemoved {
			out = append(out, fmt.Sprintf("`%s.%s`: value %s removed", def, p, v))
		}
	}
	reqA, reqB := list(da, "required"), list(db, "required")
	nowRequired, noLonger := diffLists(reqA, reqB)
	for _, r := range nowRequired {
		out = append(out, fmt.Sprintf("`%s`: %s is now required", def, r))
	}
	for _, r := range noLonger {
		out = append(out, fmt.Sprintf("`%s`: %s is no longer required", def, r))
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
