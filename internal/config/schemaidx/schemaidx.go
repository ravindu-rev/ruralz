// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package schemaidx is the navigable index over the embedded rendered JSON
// Schema of each served apiVersion (architecture R-3). It reads the
// generated schema with the standard library only and resolves the subset
// the generator emits: $ref, properties, items, additionalProperties,
// allOf, if/then/else dispatch (the resource kind at the root, the Policy
// config by spec.type) and the seven x-ruralz-* keywords of
// docs/architecture/02-configuration-model.md "Schema keywords that drive
// tooling", plus default values and the scalar definitions Duration,
// ByteSize, Decimal and IntOrString.
//
// Overlay merge (keyed lists), substitution (forbidden positions and
// re-typing), defaults and normalization, canonical form (since-omission,
// schema level), diff (impact classes) and render (escaping positions) all
// navigate the schema through this package; internal/config/schemaview only
// compiles and validates with jsonschema/v6. Nothing here is table-driven:
// adding a +ruralz:default or +ruralz:impact marker to pkg/config/v1alpha1
// and regenerating the schema needs no code change (02 req 10).
//
// An Index is built once per process with Load or Embedded and passed
// explicitly; it is immutable and safe for concurrent use.
package schemaidx

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/api/schema"
)

// ErrSchema marks every error Load returns for a schema it cannot index: a
// malformed keyword, an unresolvable $ref or a construct outside the
// supported subset.
var ErrSchema = errors.New("schemaidx: unsupported or invalid schema")

// ListType is the x-ruralz-list type of an array.
type ListType uint8

// List types (02 req 17, 01 req 25).
const (
	// ListNone means the array carries no x-ruralz-list keyword: lists
	// inside open objects and free-form content, treated as atomic.
	ListNone ListType = iota
	// ListMap is merged and diffed by a key and sorted by it.
	ListMap
	// ListOrderedMap is merged and diffed by a key in authored order.
	ListOrderedMap
	// ListSet is sorted by element and replaced whole.
	ListSet
	// ListAtomic keeps authored order and is replaced whole.
	ListAtomic
)

// String returns the schema spelling ("map", "orderedMap", "set",
// "atomic"), or "" for ListNone.
func (l ListType) String() string {
	switch l {
	case ListMap:
		return "map"
	case ListOrderedMap:
		return "orderedMap"
	case ListSet:
		return "set"
	case ListAtomic:
		return "atomic"
	default:
		return ""
	}
}

// Keyed reports whether entries are matched by a key field.
func (l ListType) Keyed() bool { return l == ListMap || l == ListOrderedMap }

func parseListType(s string) (ListType, bool) {
	switch s {
	case "map":
		return ListMap, true
	case "orderedMap":
		return ListOrderedMap, true
	case "set":
		return ListSet, true
	case "atomic":
		return ListAtomic, true
	default:
		return ListNone, false
	}
}

// Scalar names the scalar definition a value normalizes by (02 req 15).
type Scalar uint8

// Scalar definitions, recognized by their $defs name.
const (
	// ScalarNone is any other value.
	ScalarNone Scalar = iota
	// ScalarDuration is $defs/Duration (Go duration syntax).
	ScalarDuration
	// ScalarByteSize is $defs/ByteSize (integer or quantity).
	ScalarByteSize
	// ScalarDecimal is $defs/Decimal (a decimal number as a string).
	ScalarDecimal
	// ScalarIntOrString is $defs/IntOrString.
	ScalarIntOrString
)

// String returns the definition name, or "" for ScalarNone.
func (s Scalar) String() string {
	switch s {
	case ScalarDuration:
		return "Duration"
	case ScalarByteSize:
		return "ByteSize"
	case ScalarDecimal:
		return "Decimal"
	case ScalarIntOrString:
		return "IntOrString"
	default:
		return ""
	}
}

func scalarOf(def string) Scalar {
	switch def {
	case "Duration":
		return ScalarDuration
	case "ByteSize":
		return ScalarByteSize
	case "Decimal":
		return ScalarDecimal
	case "IntOrString":
		return ScalarIntOrString
	default:
		return ScalarNone
	}
}

// TypeSet is a set of JSON Schema instance types. TypeNumber holds the
// non-integer numbers, so the schema type "number" is TypeNumber|TypeInteger
// and intersecting it with "integer" leaves TypeInteger.
type TypeSet uint8

// Instance types.
const (
	// TypeNull is null.
	TypeNull TypeSet = 1 << iota
	// TypeBoolean is true or false.
	TypeBoolean
	// TypeObject is an object.
	TypeObject
	// TypeArray is an array.
	TypeArray
	// TypeNumber is a number that is not an integer.
	TypeNumber
	// TypeString is a string.
	TypeString
	// TypeInteger is an integer.
	TypeInteger

	// AllTypes is every type: a schema without a type constraint.
	AllTypes = TypeNull | TypeBoolean | TypeObject | TypeArray | TypeNumber | TypeString | TypeInteger
)

// Has reports whether t and u share a type.
func (t TypeSet) Has(u TypeSet) bool { return t&u != 0 }

// String lists the schema type names in t, joined by "|": "number" when
// non-integer numbers are included, else "integer".
func (t TypeSet) String() string {
	var names []string
	add := func(bit TypeSet, name string) {
		if t&bit != 0 {
			names = append(names, name)
		}
	}
	add(TypeNull, "null")
	add(TypeBoolean, "boolean")
	add(TypeObject, "object")
	add(TypeArray, "array")
	if t&TypeNumber != 0 {
		names = append(names, "number")
	} else {
		add(TypeInteger, "integer")
	}
	add(TypeString, "string")
	return strings.Join(names, "|")
}

func parseType(s string) (TypeSet, bool) {
	switch s {
	case "null":
		return TypeNull, true
	case "boolean":
		return TypeBoolean, true
	case "object":
		return TypeObject, true
	case "array":
		return TypeArray, true
	case "number":
		return TypeNumber | TypeInteger, true
	case "string":
		return TypeString, true
	case "integer":
		return TypeInteger, true
	default:
		return 0, false
	}
}

// Impact is a set of diff impact classes (x-ruralz-impact, 02 req 64).
type Impact uint8

// Impact classes, declared in name order so Strings is sorted.
const (
	// ImpactAI is "ai".
	ImpactAI Impact = 1 << iota
	// ImpactMetadata is "metadata".
	ImpactMetadata
	// ImpactPlugin is "plugin".
	ImpactPlugin
	// ImpactRouting is "routing".
	ImpactRouting
	// ImpactSecurity is "security".
	ImpactSecurity
	// ImpactTraffic is "traffic".
	ImpactTraffic
)

// numImpacts is the number of impact classes.
const numImpacts = 6

// impactName returns the name of class bit b (0-based, name order).
func impactName(b int) string {
	switch b {
	case 0:
		return "ai"
	case 1:
		return "metadata"
	case 2:
		return "plugin"
	case 3:
		return "routing"
	case 4:
		return "security"
	case 5:
		return "traffic"
	default:
		return ""
	}
}

// ParseImpact returns the class named s.
func ParseImpact(s string) (Impact, bool) {
	for b := range numImpacts {
		if impactName(b) == s {
			return 1 << b, true
		}
	}
	return 0, false
}

// Strings returns the class names in sorted order; nil when empty.
func (i Impact) Strings() []string {
	var out []string
	for b := range numImpacts {
		if i&(1<<b) != 0 {
			out = append(out, impactName(b))
		}
	}
	return out
}

// Has reports whether i includes every class of c.
func (i Impact) Has(c Impact) bool { return i&c == c }

// String returns the sorted class names joined by ", ".
func (i Impact) String() string { return strings.Join(i.Strings(), ", ") }

// CELSpec is an x-ruralz-cel keyword: the variables in scope and the
// result type ("bool", "string" or "dyn").
type CELSpec struct {
	// Variables are the variable names in scope, in schema order.
	Variables []string
	// Result is "bool", "string" or "dyn".
	Result string
}

// Validation is one x-ruralz-validations rule; parsed, not evaluated in M1.
type Validation struct {
	// Rule is a CEL expression over self.
	Rule string
	// Message is the optional failure message.
	Message string
}

// Keywords are the x-ruralz-* annotations of one schema node.
type Keywords struct {
	// List is the x-ruralz-list type of an array; ListNone when absent.
	List ListType
	// ListKey is the key field of a map or orderedMap list.
	ListKey string
	// Ref is the x-ruralz-ref target kind; "" when absent.
	Ref string
	// Secret is x-ruralz-secret.
	Secret bool
	// CEL is x-ruralz-cel; nil when absent.
	CEL *CELSpec
	// Impact is x-ruralz-impact.
	Impact Impact
	// Since is x-ruralz-since; 0 when absent (schema level 0).
	Since int
	// Validations are the x-ruralz-validations rules.
	Validations []Validation
}

func (k Keywords) clone() Keywords {
	if k.CEL != nil {
		c := *k.CEL
		c.Variables = slices.Clone(c.Variables)
		k.CEL = &c
	}
	k.Validations = slices.Clone(k.Validations)
	return k
}

// Index is the navigable schema of one apiVersion. It is immutable and safe
// for concurrent use.
type Index struct {
	apiVersion string
	level      int
	root       *Node
	kinds      []string
	resources  map[string]*Node
	defs       map[string]*Node
	// memo holds every Node built by Load; it is never written afterwards,
	// so runtime selection reads it without locking.
	memo map[string]*Node
}

// Load indexes the rendered schema of apiVersion. The schema's root
// apiVersion const, when present, must equal apiVersion. Load bounds the
// resolved positions and the enumerated fields, so an adversarial schema
// fails with ErrSchema instead of exhausting memory or time.
func Load(apiVersion string, rendered []byte) (*Index, error) {
	return load(apiVersion, rendered, maxNodes)
}

// load is Load with a bound on the resolved schema positions.
func load(apiVersion string, rendered []byte, limit int) (*Index, error) {
	c, err := compileDocument(rendered)
	if err != nil {
		return nil, err
	}
	if v, ok := c.rootAPIVersion(); ok && v != apiVersion {
		return nil, fmt.Errorf("%w: schema is for apiVersion %q, not %q", ErrSchema, v, apiVersion)
	}
	x := &Index{
		apiVersion: apiVersion,
		resources:  map[string]*Node{},
		defs:       map[string]*Node{},
		memo:       map[string]*Node{},
	}
	for _, r := range c.all {
		x.level = max(x.level, r.kw.Since)
	}
	b := &builder{idx: x, shared: x.memo, precompute: true, limit: limit}
	x.root = b.node([]*raw{c.root}, nil)
	for _, name := range c.defNames() {
		x.defs[name] = b.node([]*raw{c.defs[name]}, nil)
	}
	// Resolve each kind with the Load builder, so every resource schema
	// and its dispatch results are precomputed even when several root
	// rules match one kind.
	for _, kind := range x.root.DispatchValues("kind") {
		if slices.Contains(x.kinds, kind) {
			continue
		}
		n := b.variant(x.root, x.root.mask(stringMember("kind", kind), nil))
		if n == x.root {
			continue
		}
		x.kinds = append(x.kinds, kind)
		x.resources[kind] = n
	}
	if b.overflow {
		return nil, fmt.Errorf("%w: more than %d resolved schema positions", ErrSchema, limit)
	}
	if err := c.checkRefKinds(x.kinds); err != nil {
		return nil, err
	}
	if err := x.checkFields(maxFields, maxFieldBytes); err != nil {
		return nil, err
	}
	return x, nil
}

// APIVersion returns the apiVersion the index serves.
func (x *Index) APIVersion() string { return x.apiVersion }

// Level returns the schema level: the maximum x-ruralz-since in the schema,
// 0 when no field carries one (02 req 75).
func (x *Index) Level() int { return x.level }

// Kinds returns the resource kinds the root dispatches, in schema order.
func (x *Index) Kinds() []string { return slices.Clone(x.kinds) }

// Root returns the root schema node (before kind dispatch).
func (x *Index) Root() *Node { return x.root }

// Resource returns the envelope schema of kind (apiVersion, kind, metadata,
// spec): the root with the kind dispatch applied.
func (x *Index) Resource(kind string) (*Node, bool) {
	n, ok := x.resources[kind]
	return n, ok
}

// Spec returns the spec schema of kind.
func (x *Index) Spec(kind string) (*Node, bool) {
	r, ok := x.resources[kind]
	if !ok {
		return nil, false
	}
	return r.Property("spec")
}

// Def returns the node of the definition $defs/name.
func (x *Index) Def(name string) (*Node, bool) {
	n, ok := x.defs[name]
	return n, ok
}

// Set holds one Index per served apiVersion. It is immutable.
type Set struct {
	byVersion map[string]*Index
	served    []string
}

// NewSet returns a Set of indexes; two indexes for one apiVersion are an
// error.
func NewSet(indexes ...*Index) (*Set, error) {
	s := &Set{byVersion: map[string]*Index{}}
	for _, x := range indexes {
		if _, dup := s.byVersion[x.apiVersion]; dup {
			return nil, fmt.Errorf("schemaidx: two indexes for apiVersion %q", x.apiVersion)
		}
		s.byVersion[x.apiVersion] = x
		s.served = append(s.served, x.apiVersion)
	}
	slices.Sort(s.served)
	return s, nil
}

// Embedded indexes every rendered view embedded in api/schema
// (ruralz/<version>/rendered.schema.json serves apiVersion
// ruralz/<version>).
func Embedded() (*Set, error) {
	return loadFS(schema.FS())
}

func loadFS(fsys fs.FS) (*Set, error) {
	files, err := fs.Glob(fsys, "ruralz/*/rendered.schema.json")
	if err != nil {
		return nil, fmt.Errorf("schemaidx: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no rendered schema found", ErrSchema)
	}
	var indexes []*Index
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("schemaidx: %w", err)
		}
		version := "ruralz/" + path.Base(path.Dir(f))
		x, err := Load(version, data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		indexes = append(indexes, x)
	}
	return NewSet(indexes...)
}

// Index returns the index of apiVersion; false for an unserved apiVersion
// (the loader reports RZ-CFG-007, 02 req 4).
func (s *Set) Index(apiVersion string) (*Index, bool) {
	x, ok := s.byVersion[apiVersion]
	return x, ok
}

// Served returns the served apiVersions, sorted.
func (s *Set) Served() []string { return slices.Clone(s.served) }
