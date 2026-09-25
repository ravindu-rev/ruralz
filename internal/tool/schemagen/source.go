// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// markerPrefix starts a marker line in a doc comment.
const markerPrefix = "+ruralz:"

// marker is one "+ruralz:name=value" line.
type marker struct {
	name  string
	value string
	// set is true when the marker had "=value".
	set bool
}

// docInfo is a doc comment split into description text and markers.
type docInfo struct {
	description string
	markers     []marker
}

// get returns the first marker with the given name.
func (d docInfo) get(name string) (marker, bool) {
	for _, m := range d.markers {
		if m.name == name {
			return m, true
		}
	}
	return marker{}, false
}

// typeInfo is what the source says about one named type.
type typeInfo struct {
	doc docInfo
	// fields maps Go field names to their docs; nil unless a struct.
	fields map[string]docInfo
	// enum lists the string constants of the type in source order.
	enum []string
}

// sourceInfo maps type names to what the source says about them.
type sourceInfo map[string]*typeInfo

func (s sourceInfo) typ(name string) *typeInfo {
	t, ok := s[name]
	if !ok {
		t = &typeInfo{}
		s[name] = t
	}
	return t
}

// parseSources reads doc comments, markers and string constant blocks.
func parseSources(files []string) (sourceInfo, error) {
	info := sourceInfo{}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gd.Tok {
			case token.TYPE:
				if err := addTypes(info, gd); err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
			case token.CONST:
				if err := addConsts(info, gd); err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
			default:
			}
		}
	}
	return info, nil
}

func addTypes(info sourceInfo, gd *ast.GenDecl) error {
	for _, spec := range gd.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		group := ts.Doc
		if group == nil && len(gd.Specs) == 1 {
			group = gd.Doc
		}
		d, err := parseDoc(group)
		if err != nil {
			return fmt.Errorf("type %s: %w", ts.Name.Name, err)
		}
		t := info.typ(ts.Name.Name)
		t.doc = d
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		t.fields = map[string]docInfo{}
		for _, field := range st.Fields.List {
			fd, err := parseDoc(field.Doc)
			if err != nil {
				return fmt.Errorf("type %s: %w", ts.Name.Name, err)
			}
			if len(field.Names) > 1 {
				return fmt.Errorf("type %s: declare one field per line", ts.Name.Name)
			}
			name := ""
			if len(field.Names) == 1 {
				name = field.Names[0].Name
			} else if id, ok := field.Type.(*ast.Ident); ok {
				name = id.Name // embedded field
			}
			t.fields[name] = fd
		}
	}
	return nil
}

func addConsts(info sourceInfo, gd *ast.GenDecl) error {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || vs.Type == nil {
			continue
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok {
			continue
		}
		for _, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return err
			}
			t := info.typ(id.Name)
			t.enum = append(t.enum, s)
		}
	}
	return nil
}

// knownMarker reports whether name is a marker and whether it takes a value.
func knownMarker(name string) (takesValue, ok bool) {
	switch name {
	case "required", "secret", "open":
		return false, true
	case "ref", "cel", "list", "impact", "since", "validation", "default", "policyType",
		"minItems", "minimum", "maximum", "pattern", "minLength", "maxLength", "minProperties",
		"exactlyOneOf", "atMostOneOf", "atLeastOneOf":
		return true, true
	default:
		return false, false
	}
}

// parseDoc splits a comment group into description and markers.
func parseDoc(group *ast.CommentGroup) (docInfo, error) {
	if group == nil {
		return docInfo{}, nil
	}
	var d docInfo
	var text []string
	for _, line := range strings.Split(group.Text(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, markerPrefix) {
			if line != "" {
				text = append(text, line)
			}
			continue
		}
		body := strings.TrimPrefix(line, markerPrefix)
		name, value, set := strings.Cut(body, "=")
		takesValue, ok := knownMarker(name)
		if !ok {
			return docInfo{}, fmt.Errorf("unknown marker %q", markerPrefix+name)
		}
		if takesValue != set || (set && value == "") {
			return docInfo{}, fmt.Errorf("marker %q: value %s", markerPrefix+name, map[bool]string{true: "required", false: "not allowed"}[takesValue])
		}
		d.markers = append(d.markers, marker{name: name, value: value, set: set})
	}
	d.description = strings.Join(text, " ")
	return d, nil
}
