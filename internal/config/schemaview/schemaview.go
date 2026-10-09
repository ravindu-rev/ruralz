// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package schemaview is stage F of the configuration pipeline: it compiles
// the rendered JSON Schema view of each served apiVersion once per process
// with github.com/santhosh-tekuri/jsonschema/v6 (draft 2020-12, formats
// not asserted, a loader that refuses every URL, and the Ruralz vocabulary
// asserting the seven x-ruralz-* keyword shapes), validates resource trees
// after substitution, and maps every validation error to a diag.Diagnostic
// at a key-aware diag.Path with RZ-CFG-005, RZ-CFG-006 or RZ-CFG-012 and a
// nearest-name hint (01 reqs 32 to 35).
//
// Schema navigation (lookup by instance path, keywords, defaults,
// dispatch) belongs to internal/config/schemaidx (architecture R-3); this
// package uses it to turn instance locations into key-aware paths, to find
// x-ruralz-secret positions and to name allowed fields in hints. It is the
// only package besides validation.json-schema that imports jsonschema/v6,
// and no jsonschema type appears in its API.
//
// Views and Sets are immutable after construction and safe for concurrent
// use; Validate allocates per call and starts no goroutine. The mapping
// resolves each instance position once and shares its key-aware path with
// every diagnostic at or below it: a large path element, such as a keyed
// entry's key or a set element's canonical JSON, is held and rendered
// once, and diagnostics are deduplicated and sorted by interned path
// text, never by rendering their paths. Mapping work and memory are
// therefore linear in the resource size plus the number of validation
// errors times their location depth, and Validate returns at most
// MaxDiagnostics diagnostics, the first in sorted order (01 reqs 6, 50 and
// 54). Rendering the returned paths (diag.Path.String, diag.List.Sort)
// still writes such an element once per diagnostic below it, which the
// path contract of 01 req 48 does not bound. The package never logs,
// reads files or the network, or uses the clock (01 req 55).
package schemaview

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Diagnostic codes of stage F, each registered in internal/errcode.
const (
	// CodeSchema is a JSON Schema violation, a duplicate keyed-list entry or
	// set element, or a reserved label or annotation key (01 reqs 33 to 35).
	CodeSchema = "RZ-CFG-005"
	// CodeUnknownField is an unknown member of a closed object (01 req 33).
	CodeUnknownField = "RZ-CFG-006"
	// CodeLiteral is a non-conforming value at an x-ruralz-secret
	// position; its message never contains the value (01 req 33).
	CodeLiteral = "RZ-CFG-012"
)

// ErrSchema marks every error Compile returns for a schema that does not
// compile: invalid JSON, a draft 2020-12 or vocabulary keyword violation,
// an unresolvable or remote reference, or a schema that disagrees with its
// index.
var ErrSchema = errors.New("schemaview: schema does not compile")

// View is the compiled rendered view of one apiVersion. It is immutable
// and safe for concurrent use.
type View struct {
	idx *schemaidx.Index
	// base is the URL the document was compiled under; schema locations in
	// validation errors start with it.
	base string
	// doc is the decoded schema document, read to word messages by schema
	// location (combination keywords, type order, scalar definitions).
	doc any
	// root validates a resource of an unknown kind (the root dispatches on
	// kind); kinds holds each kind's definition, compiled once.
	root  *jsonschema.Schema
	kinds map[string]*jsonschema.Schema
}

// baseURL returns the URL a view of apiVersion is compiled under. It is a
// name only: every reference in a view is a local fragment, and the loader
// refuses every URL.
func baseURL(apiVersion string) string {
	return "urn:ruralz:schema:" + strings.ReplaceAll(apiVersion, "/", ":")
}

// Compile compiles rendered, the rendered view that idx indexes, once:
// the whole document (so every $ref resolves) and the definition of every
// kind the root dispatches. idx must have been loaded from the same
// bytes; Compile checks that the root kind dispatch agrees with it.
func Compile(rendered []byte, idx *schemaidx.Index) (*View, error) {
	if idx == nil {
		return nil, fmt.Errorf("%w: nil schema index", ErrSchema)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rendered))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	c, err := newCompiler()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	v := &View{idx: idx, base: baseURL(idx.APIVersion()), doc: doc, kinds: map[string]*jsonschema.Schema{}}
	if err := c.AddResource(v.base, doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	if v.root, err = c.Compile(v.base); err != nil {
		return nil, compileError(err)
	}
	refs := kindRefs(doc)
	for _, kind := range idx.Kinds() {
		ref, ok := refs[kind]
		if !ok {
			return nil, fmt.Errorf("%w: kind %q has no definition in the root dispatch", ErrSchema, kind)
		}
		sch, err := c.Compile(v.base + ref)
		if err != nil {
			return nil, fmt.Errorf("kind %s: %w", kind, compileError(err))
		}
		v.kinds[kind] = sch
	}
	if len(refs) != len(v.kinds) {
		return nil, fmt.Errorf("%w: the root dispatches %d kinds, the index %d", ErrSchema, len(refs), len(v.kinds))
	}
	return v, nil
}

// compileError wraps a compiler error in ErrSchema. A refused load keeps
// ErrRemoteReference in the chain (jsonschema's LoadURLError does not
// unwrap).
func compileError(err error) error {
	var load *jsonschema.LoadURLError
	if errors.As(err, &load) {
		return fmt.Errorf("%w: %w", ErrSchema, load.Err)
	}
	return fmt.Errorf("%w: %w", ErrSchema, err)
}

// kindRefs reads the root dispatch the generator emits,
// allOf[{"if": {"properties": {"kind": {"const": K}}}, "then": {"$ref": R}}],
// as a map from kind K to the local reference R.
func kindRefs(doc any) map[string]string {
	out := map[string]string{}
	root, _ := doc.(map[string]any)
	all, _ := root["allOf"].([]any)
	for _, e := range all {
		rule, _ := e.(map[string]any)
		cond, _ := rule["if"].(map[string]any)
		props, _ := cond["properties"].(map[string]any)
		kindSchema, _ := props["kind"].(map[string]any)
		kind, _ := kindSchema["const"].(string)
		then, _ := rule["then"].(map[string]any)
		ref, _ := then["$ref"].(string)
		if kind != "" && strings.HasPrefix(ref, "#") {
			out[kind] = ref
		}
	}
	return out
}

// APIVersion returns the apiVersion the view validates.
func (v *View) APIVersion() string { return v.idx.APIVersion() }

// Index returns the schema index the view navigates by.
func (v *View) Index() *schemaidx.Index { return v.idx }

// Kinds returns the kinds the view has a compiled definition for, in the
// index's schema order.
func (v *View) Kinds() []string { return v.idx.Kinds() }

// Set holds one View per served apiVersion. It is immutable.
type Set struct {
	views  map[string]*View
	served []string
}

// NewSet returns a Set of views; two views of one apiVersion are an error.
func NewSet(views ...*View) (*Set, error) {
	s := &Set{views: map[string]*View{}}
	for _, v := range views {
		if v == nil {
			return nil, errors.New("schemaview: nil view")
		}
		if _, dup := s.views[v.APIVersion()]; dup {
			return nil, fmt.Errorf("schemaview: two views for apiVersion %q", v.APIVersion())
		}
		s.views[v.APIVersion()] = v
		s.served = append(s.served, v.APIVersion())
	}
	slices.Sort(s.served)
	return s, nil
}

// Embedded compiles the rendered view of every apiVersion idx serves from
// the schemas embedded in api/schema (ruralz/<version>/rendered.schema.json
// serves ruralz/<version>). Call it once per process and share the Set.
func Embedded(idx *schemaidx.Set) (*Set, error) {
	return compileFS(schema.FS(), idx)
}

func compileFS(fsys fs.FS, idx *schemaidx.Set) (*Set, error) {
	if idx == nil {
		return nil, errors.New("schemaview: nil schema index set")
	}
	var views []*View
	for _, version := range idx.Served() {
		x, _ := idx.Index(version)
		file := path.Join(version, "rendered.schema.json")
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("schemaview: %w", err)
		}
		v, err := Compile(data, x)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		views = append(views, v)
	}
	return NewSet(views...)
}

// View returns the view of apiVersion; false for an unserved apiVersion,
// which stage C reports as RZ-CFG-007 before validation.
func (s *Set) View(apiVersion string) (*View, bool) {
	v, ok := s.views[apiVersion]
	return v, ok
}

// Served returns the served apiVersions, sorted.
func (s *Set) Served() []string { return slices.Clone(s.served) }

// Validate validates res with the view of its apiVersion (View.Validate).
// ok is false, with no diagnostics, for an unserved apiVersion, which
// stage C reports as RZ-CFG-007 before validation.
func (s *Set) Validate(res *tree.Resource, files *tree.FileTable) (_ diag.List, ok bool) {
	if res == nil {
		return nil, false
	}
	v, ok := s.views[res.APIVersion]
	if !ok {
		return nil, false
	}
	return v.Validate(res, files), true
}
