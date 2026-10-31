// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// plannedFile is the served-feature table schemagen writes into
// internal/config/registry from the +ruralz:planned markers
// (docs/engineering/02-repository-layout-and-conventions.md "Schema
// generation from Go types").
const plannedFile = "planned_gen.go"

// releaseConst names the string constant of internal/config/registry that
// holds the milestone the release serves.
const releaseConst = "ReleaseMilestone"

// lastMilestone is the latest milestone a +ruralz:planned marker may name.
const lastMilestone = 5

// feature is one row of the served-feature table: a kind (path empty), a
// field (value empty) or an enum value at a schema path of the kind's
// resources, with its milestone. policyType is set for a field of a Policy
// config, which only Policies of that type hold.
type feature struct {
	kind, policyType, path, value, milestone string
}

// compareFeatures orders rows by kind, Policy type, path and value.
func compareFeatures(a, b feature) int {
	return cmp.Or(
		cmp.Compare(a.kind, b.kind),
		cmp.Compare(a.policyType, b.policyType),
		cmp.Compare(a.path, b.path),
		cmp.Compare(a.value, b.value),
	)
}

// milestoneNumber returns n of a milestone "M<n>" with n a decimal number
// from 1 without a sign or leading zeros.
func milestoneNumber(s string) (int, bool) {
	rest, ok := strings.CutPrefix(s, "M")
	if !ok || rest == "" || rest[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// readRelease returns the milestone number of the ReleaseMilestone string
// constant that the non-test Go files of dir (internal/config/registry)
// declare. schemagen reads the source instead of importing the package,
// which links the table it generates.
func readRelease(dir string) (int, error) {
	files, err := sourceFiles(dir)
	if err != nil {
		return 0, err
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return 0, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != releaseConst || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return 0, fmt.Errorf("%s: %s is not a string literal", path, releaseConst)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return 0, err
				}
				n, ok := milestoneNumber(s)
				if !ok || n >= lastMilestone {
					return 0, fmt.Errorf("%s: %s = %q is not a milestone before M%d", path, releaseConst, s, lastMilestone)
				}
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("no %s string constant in %s", releaseConst, dir)
}

// tableBuilder collects the rows of the served-feature table.
type tableBuilder struct {
	in    input
	pkg   string
	known wellKnown
	rows  map[feature]bool
}

// plannedTable returns the served-feature table of in, sorted: a row for
// each kind whose spec type carries +ruralz:planned, and for each field
// and enum constant that carries it, at every schema path
// (schemaidx.Index.SchemaPath) where a kind's resources can hold it, the
// fields of each Policy config under spec.config with their Policy type.
// It fails for any marker whose milestone is not after release, up to M5,
// for a marker on a struct type that is no kind's spec type, and for a
// marker on a Kind or PolicyType constant: a kind takes the milestone of
// its spec type, and a Policy type its row of the Policy type registry.
func plannedTable(in input, release int) ([]feature, error) {
	if len(in.resources) == 0 {
		return nil, errors.New("no resource types")
	}
	b := &tableBuilder{in: in, pkg: in.resources[0].PkgPath(), known: newWellKnown(), rows: map[feature]bool{}}
	specs := map[string]reflect.Type{}
	for _, t := range in.resources {
		spec, ok := t.FieldByName("Spec")
		if !ok {
			return nil, fmt.Errorf("resource type %s has no Spec field", t.Name())
		}
		specs[deref(spec.Type).Name()] = t
	}
	if err := b.check(specs, release); err != nil {
		return nil, err
	}
	for _, t := range in.resources {
		spec, _ := t.FieldByName("Spec")
		st := deref(spec.Type)
		if m, ok := in.source[st.Name()].docOrZero().get("planned"); ok {
			b.rows[feature{kind: t.Name(), milestone: m.value}] = true
		}
		b.walk(t.Name(), "", spec.Type, "spec", nil)
		if st.Name() != "PolicySpec" {
			continue
		}
		for _, c := range in.configs {
			m, ok := in.source[c.Name()].docOrZero().get("policyType")
			if !ok {
				return nil, fmt.Errorf("policy config %s lacks +ruralz:policyType", c.Name())
			}
			b.walk(t.Name(), m.value, c, "spec.config", nil)
		}
	}
	return slices.SortedFunc(maps.Keys(b.rows), compareFeatures), nil
}

// check verifies every +ruralz:planned marker of the source, whether a
// kind's resources reach it or not.
func (b *tableBuilder) check(specs map[string]reflect.Type, release int) error {
	milestone := func(where, v string) error {
		if n, ok := milestoneNumber(v); !ok || n <= release || n > lastMilestone {
			return fmt.Errorf("%s: +ruralz:planned=%s: want a milestone after the release's own (M%d), up to M%d", where, v, release, lastMilestone)
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(b.in.source)) {
		info := b.in.source[name]
		if m, ok := info.doc.get("planned"); ok {
			if _, spec := specs[name]; !spec {
				return fmt.Errorf("type %s: +ruralz:planned is only for a kind's spec type, a field or an enum constant", name)
			}
			if err := milestone("type "+name, m.value); err != nil {
				return err
			}
		}
		for _, field := range slices.Sorted(maps.Keys(info.fields)) {
			if m, ok := info.fields[field].get("planned"); ok {
				if err := milestone(name+"."+field, m.value); err != nil {
					return err
				}
			}
		}
		for _, value := range info.enum {
			m, ok := info.consts[value].get("planned")
			if !ok {
				continue
			}
			switch name {
			case "Kind":
				return fmt.Errorf("Kind constant %q: +ruralz:planned goes on the kind's spec type", value)
			case b.known.policyType.Name():
				return fmt.Errorf("PolicyType constant %q: a Policy type takes its milestone from the Policy type registry, not +ruralz:planned", value)
			}
			if err := milestone(name+" constant "+strconv.Quote(value), m.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// walk adds the rows of the fields and enum values a value of type t holds
// at schema path path of a kind's resources: members joined by ".", "[]"
// for list elements and "{}" for typed-map values. stack holds the structs
// being walked, so a recursive type ends.
func (b *tableBuilder) walk(kind, policyType string, t reflect.Type, path string, stack []reflect.Type) {
	if b.known.opaque(t) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		b.walk(kind, policyType, t.Elem(), path, stack)
	case reflect.Slice:
		b.walk(kind, policyType, t.Elem(), path+"[]", stack)
	case reflect.Map:
		b.walk(kind, policyType, t.Elem(), path+"{}", stack)
	case reflect.Struct:
		if t.PkgPath() != b.pkg || slices.Contains(stack, t) {
			return
		}
		info := b.in.source[t.Name()]
		stack = append(stack, t)
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() || f.Anonymous {
				continue
			}
			name, _, err := jsonTag(f)
			if err != nil {
				// generate reports the tag.
				continue
			}
			child := path + "." + name
			if info != nil {
				if m, ok := info.fields[f.Name].get("planned"); ok {
					b.rows[feature{kind: kind, policyType: policyType, path: child, milestone: m.value}] = true
				}
			}
			b.walk(kind, policyType, f.Type, child, stack)
		}
	case reflect.String:
		info := b.in.source[t.Name()]
		if t.Name() == "" || info == nil {
			return
		}
		for _, v := range info.enum {
			if m, ok := info.consts[v].get("planned"); ok {
				b.rows[feature{kind: kind, policyType: policyType, path: path, value: v, milestone: m.value}] = true
			}
		}
	default:
	}
}

// deref returns the element type of a pointer type, else t.
func deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// plannedSource returns planned_gen.go of the table rows: package
// registry's plannedFeatures, in gofmt layout.
func plannedSource(rows []feature) []byte {
	var b bytes.Buffer
	b.WriteString(`// Code generated by schemagen. DO NOT EDIT.

package registry

// plannedFeatures returns the served-feature table that schemagen
// generates from the +ruralz:planned markers of pkg/config/v1alpha1: one
// row for each kind, field and enum value whose milestone is later than
// ReleaseMilestone, at each schema path where a resource of the kind holds
// it, in kind, Policy type, path and value order.
func plannedFeatures() []Feature {
`)
	if len(rows) == 0 {
		b.WriteString("\treturn nil\n}\n")
		return b.Bytes()
	}
	b.WriteString("\treturn []Feature{\n")
	for _, r := range rows {
		parts := []string{"Kind: " + strconv.Quote(r.kind)}
		if r.policyType != "" {
			parts = append(parts, "PolicyType: "+strconv.Quote(r.policyType))
		}
		if r.path != "" {
			parts = append(parts, "Path: "+strconv.Quote(r.path))
		}
		if r.value != "" {
			parts = append(parts, "Value: "+strconv.Quote(r.value))
		}
		parts = append(parts, "Milestone: "+strconv.Quote(r.milestone))
		b.WriteString("\t\t{" + strings.Join(parts, ", ") + "},\n")
	}
	b.WriteString("\t}\n}\n")
	return b.Bytes()
}
