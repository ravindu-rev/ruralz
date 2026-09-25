// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// schema is one JSON Schema node. encoding/json sorts map keys, which keeps
// the output deterministic.
type schema = map[string]any

// View selects which of the two published schema views to generate.
type view int

const (
	// authoring lets substitutable non-string scalars also be a ${VAR} string.
	authoring view = iota
	// rendered is strict and validates after substitution.
	rendered
)

func (v view) String() string {
	if v == authoring {
		return "authoring"
	}
	return "rendered"
}

// substitutionPattern matches a value that is exactly one ${VAR} or
// ${VAR:-default} expression (docs/architecture/02-configuration-model.md).
const substitutionPattern = `^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`

const (
	draft2020    = "https://json-schema.org/draft/2020-12/schema"
	defsPrefix   = "#/$defs/"
	substDefName = "Substitution"
)

// input is everything the generator reads.
type input struct {
	source    sourceInfo
	resources []reflect.Type
	configs   []reflect.Type
}

// generator builds one view.
type generator struct {
	in   input
	view view
	pkg  string
	defs map[string]schema
	// building guards against recursion while a definition is built.
	building map[string]bool
	known    wellKnown
}

// wellKnown holds types with a fixed schema or a special role.
type wellKnown struct {
	duration, byteSize, intOrString, rawMessage, secretValue, secretRef, policyType reflect.Type
}

func newWellKnown() wellKnown {
	return wellKnown{
		duration:    reflect.TypeFor[v1alpha1.Duration](),
		byteSize:    reflect.TypeFor[v1alpha1.ByteSize](),
		intOrString: reflect.TypeFor[v1alpha1.IntOrString](),
		rawMessage:  reflect.TypeFor[json.RawMessage](),
		secretValue: reflect.TypeFor[v1alpha1.SecretValue](),
		secretRef:   reflect.TypeFor[v1alpha1.SecretRef](),
		policyType:  reflect.TypeFor[v1alpha1.PolicyType](),
	}
}

// generate returns the schema view as JSON bytes.
func generate(in input, v view) ([]byte, error) {
	if len(in.resources) == 0 {
		return nil, errors.New("no resource types")
	}
	g := &generator{
		in:       in,
		view:     v,
		pkg:      in.resources[0].PkgPath(),
		defs:     map[string]schema{},
		building: map[string]bool{},
		known:    newWellKnown(),
	}
	root, err := g.root()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (g *generator) root() (schema, error) {
	var kinds []any
	var dispatch []any
	for _, t := range g.in.resources {
		name, err := g.resource(t)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, name)
		dispatch = append(dispatch, schema{
			"if":   schema{"properties": schema{"kind": schema{"const": name}}, "required": []any{"kind"}},
			"then": ref(name),
		})
	}
	if err := g.policyDispatch(); err != nil {
		return nil, err
	}
	if g.view == authoring {
		g.defs[substDefName] = schema{
			"description": "A value that is exactly one ${VAR} or ${VAR:-default} expression, substituted before the rendered view validates it.",
			"type":        "string",
			"pattern":     substitutionPattern,
		}
	}
	defs := schema{}
	for name, d := range g.defs {
		defs[name] = d
	}
	return schema{
		"$schema": draft2020,
		"title":   "Ruralz " + v1alpha1.APIVersion + " resource (" + g.view.String() + " view)",
		"description": "One Ruralz configuration resource. Generated from github.com/ravindu-rev/ruralz/pkg/config/v1alpha1 " +
			"by internal/tool/schemagen; do not edit.",
		"type":     "object",
		"required": []any{"apiVersion", "kind", "metadata", "spec"},
		"properties": schema{
			"apiVersion": schema{"const": v1alpha1.APIVersion},
			"kind":       schema{"enum": kinds},
		},
		"allOf": dispatch,
		"$defs": defs,
	}, nil
}

// resource builds the envelope definition of one kind. The envelope fields
// are hard-coded: substitution is forbidden in all of them.
func (g *generator) resource(t reflect.Type) (string, error) {
	name := t.Name()
	info := g.in.source[name]
	if info == nil || !slices.Contains(g.in.source["Kind"].enumOrNil(), name) {
		return "", fmt.Errorf("resource type %s is not a Kind constant", name)
	}
	spec, ok := t.FieldByName("Spec")
	if !ok {
		return "", fmt.Errorf("resource type %s has no Spec field", name)
	}
	specRef, err := g.typeSchema(spec.Type, fieldCtx{})
	if err != nil {
		return "", err
	}
	meta, ok := t.FieldByName("Metadata")
	if !ok {
		return "", fmt.Errorf("resource type %s has no Metadata field", name)
	}
	metaRef, err := g.typeSchema(meta.Type, fieldCtx{envelope: true})
	if err != nil {
		return "", err
	}
	g.defs[name] = schema{
		"description":          info.doc.description,
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"apiVersion", "kind", "metadata", "spec"},
		"properties": schema{
			"apiVersion": schema{"const": v1alpha1.APIVersion},
			"kind":       schema{"const": name},
			"metadata":   metaRef,
			"spec":       specRef,
		},
	}
	return name, nil
}

func (t *typeInfo) enumOrNil() []string {
	if t == nil {
		return nil
	}
	return t.enum
}

// policyDispatch selects PolicySpec.config by PolicySpec.type.
func (g *generator) policyDispatch() error {
	spec, ok := g.defs["PolicySpec"]
	if !ok {
		if len(g.in.configs) > 0 {
			return errors.New("policy config types given but no PolicySpec")
		}
		return nil
	}
	byType := map[string]string{}
	for _, t := range g.in.configs {
		info := g.in.source[t.Name()]
		m, ok := info.docOrZero().get("policyType")
		if !ok {
			return fmt.Errorf("policy config %s lacks +ruralz:policyType", t.Name())
		}
		if prev, dup := byType[m.value]; dup {
			return fmt.Errorf("policy type %s has two configs: %s and %s", m.value, prev, t.Name())
		}
		if _, err := g.typeSchema(t, fieldCtx{}); err != nil {
			return err
		}
		byType[m.value] = t.Name()
	}
	// Every registered type has exactly one config, and every marker names a registered type.
	for name, info := range g.in.source {
		if m, ok := info.doc.get("policyType"); ok && byType[m.value] != name {
			return fmt.Errorf("type %s marks policyType %s but is not in the config table", name, m.value)
		}
	}
	registered := g.in.source[g.known.policyType.Name()].enumOrNil()
	var dispatch []any
	for _, pt := range registered {
		cfg, ok := byType[pt]
		if !ok {
			return fmt.Errorf("policy type %s has no config type", pt)
		}
		dispatch = append(dispatch, schema{
			"if":   schema{"properties": schema{"type": schema{"const": pt}}, "required": []any{"type"}},
			"then": schema{"properties": schema{"config": ref(cfg)}},
		})
	}
	if len(byType) != len(registered) {
		return fmt.Errorf("%d policy configs for %d policy types", len(byType), len(registered))
	}
	spec["allOf"] = append(asList(spec["allOf"]), dispatch...)
	return nil
}

func (t *typeInfo) docOrZero() docInfo {
	if t == nil {
		return docInfo{}
	}
	return t.doc
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func ref(name string) schema {
	return schema{"$ref": defsPrefix + name}
}

// fieldCtx carries what a field's markers say about its value.
type fieldCtx struct {
	// envelope, ref, secret and cel fields never accept substitution.
	envelope bool
	noSubst  bool
}

// typeSchema returns the schema for a value of type t, registering named
// types as definitions and returning a $ref to them.
func (g *generator) typeSchema(t reflect.Type, ctx fieldCtx) (schema, error) {
	switch t {
	case g.known.duration:
		g.defs["Duration"] = schema{
			"description": "A non-negative duration in Go syntax, such as 50ms, 1m30s or 24h.",
			"type":        "string",
			"pattern":     `^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`,
		}
		return ref("Duration"), nil
	case g.known.byteSize:
		g.defs["ByteSize"] = schema{
			"description": "A number of bytes: an integer or a Kubernetes quantity such as 64Ki, 10Mi or 1Gi.",
			"anyOf": []any{
				schema{"type": "integer", "minimum": 0},
				schema{"type": "string", "pattern": `^[0-9]+(\.[0-9]+)?(k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)$|^[0-9]+$`},
			},
		}
		return ref("ByteSize"), nil
	case g.known.intOrString:
		g.defs["IntOrString"] = schema{
			"description": "An integer or a string, such as a port number or a port name.",
			"anyOf":       []any{schema{"type": "integer"}, schema{"type": "string"}},
		}
		return ref("IntOrString"), nil
	case g.known.rawMessage:
		return schema{"type": "object"}, nil
	default:
	}

	switch t.Kind() {
	case reflect.Pointer:
		return g.typeSchema(t.Elem(), ctx)
	case reflect.Slice:
		items, err := g.typeSchema(t.Elem(), ctx)
		if err != nil {
			return nil, err
		}
		return schema{"type": "array", "items": g.substitutable(items, ctx)}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key of %s must be a string", t)
		}
		out := schema{"type": "object"}
		if t.Elem().Kind() != reflect.Interface {
			values, err := g.typeSchema(t.Elem(), ctx)
			if err != nil {
				return nil, err
			}
			out["additionalProperties"] = values
		}
		if t.Name() != "" && t.PkgPath() == g.pkg {
			return g.namedDef(t, out)
		}
		return out, nil
	case reflect.Struct:
		if t.PkgPath() != g.pkg {
			return nil, fmt.Errorf("struct %s is outside the schema package", t)
		}
		return g.structDef(t)
	case reflect.String:
		if t.Name() != "" && t.PkgPath() != "" {
			return g.stringDef(t)
		}
		return schema{"type": "string"}, nil
	case reflect.Bool:
		return schema{"type": "boolean"}, nil
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		return schema{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return schema{"type": "number"}, nil
	default:
		return nil, fmt.Errorf("unsupported type %s", t)
	}
}

// namedDef registers a named non-struct definition with its type markers.
func (g *generator) namedDef(t reflect.Type, s schema) (schema, error) {
	info := g.in.source[t.Name()]
	if info != nil && info.doc.description != "" {
		s["description"] = info.doc.description
	}
	g.defs[t.Name()] = s
	return ref(t.Name()), nil
}

// stringDef registers a named string type: an enum when the source declares
// constants of the type, else a string with the type's markers.
func (g *generator) stringDef(t reflect.Type) (schema, error) {
	name := t.Name()
	if _, done := g.defs[name]; done {
		return ref(name), nil
	}
	info := g.in.source[name]
	if info == nil {
		return nil, fmt.Errorf("no source for type %s", name)
	}
	s := schema{"type": "string"}
	if info.doc.description != "" {
		s["description"] = info.doc.description
	}
	if len(info.enum) > 0 {
		values := make([]any, len(info.enum))
		for i, v := range info.enum {
			values[i] = v
		}
		s["enum"] = values
	}
	if err := applyValueMarkers(s, info.doc, "type "+name); err != nil {
		return nil, err
	}
	for _, m := range info.doc.markers {
		if !isValueMarker(m.name) {
			return nil, fmt.Errorf("type %s: marker %s is not allowed on a string type", name, m.name)
		}
	}
	g.defs[name] = s
	return ref(name), nil
}

// structDef registers a struct definition.
func (g *generator) structDef(t reflect.Type) (schema, error) {
	name := t.Name()
	if name == "" {
		return nil, fmt.Errorf("anonymous struct in %s", g.pkg)
	}
	if _, done := g.defs[name]; done || g.building[name] {
		return ref(name), nil
	}
	g.building[name] = true
	defer delete(g.building, name)
	envelope := name == "ObjectMeta"

	info := g.in.source[name]
	if info == nil {
		return nil, fmt.Errorf("no source for type %s", name)
	}
	props := schema{}
	var required []any
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			return nil, fmt.Errorf("%s.%s: embedded fields are only allowed in resource types", name, f.Name)
		}
		jsonName, omitempty, err := jsonTag(f)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", name, f.Name, err)
		}
		fd := info.fields[f.Name]
		prop, isRequired, err := g.fieldSchema(t, f, fd, envelope)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", name, f.Name, err)
		}
		if !isRequired && !omitempty {
			return nil, fmt.Errorf("%s.%s: optional field needs omitempty", name, f.Name)
		}
		props[jsonName] = prop
		if isRequired {
			required = append(required, jsonName)
		}
	}
	s := schema{"type": "object", "properties": props}
	if info.doc.description != "" {
		s["description"] = info.doc.description
	}
	if len(required) > 0 {
		s["required"] = required
	}
	open := false
	var allOf []any
	for _, m := range info.doc.markers {
		switch m.name {
		case "policyType":
		case "open":
			open = true
		case "minProperties":
			n, err := strconv.Atoi(m.value)
			if err != nil {
				return nil, fmt.Errorf("type %s: minProperties: %w", name, err)
			}
			s["minProperties"] = n
		case "exactlyOneOf", "atMostOneOf", "atLeastOneOf":
			fields := strings.Split(m.value, ",")
			for _, fname := range fields {
				if _, ok := props[fname]; !ok {
					return nil, fmt.Errorf("type %s: %s names unknown field %q", name, m.name, fname)
				}
			}
			allOf = append(allOf, fieldCombination(m.name, fields))
		default:
			return nil, fmt.Errorf("type %s: marker %s is not allowed on a struct", name, m.name)
		}
	}
	if !open {
		s["additionalProperties"] = false
	}
	if len(allOf) > 0 {
		s["allOf"] = allOf
	}
	g.defs[name] = s
	return ref(name), nil
}

// fieldCombination encodes exactlyOneOf, atMostOneOf and atLeastOneOf.
func fieldCombination(kind string, fields []string) schema {
	var each []any
	for _, f := range fields {
		each = append(each, schema{"required": []any{f}})
	}
	switch kind {
	case "exactlyOneOf":
		return schema{"oneOf": each}
	case "atLeastOneOf":
		return schema{"anyOf": each}
	default: // atMostOneOf: no two at once
		var pairs []any
		for i := range fields {
			for j := i + 1; j < len(fields); j++ {
				pairs = append(pairs, schema{"required": []any{fields[i], fields[j]}})
			}
		}
		if len(pairs) == 1 {
			return schema{"not": pairs[0]}
		}
		return schema{"not": schema{"anyOf": pairs}}
	}
}

func jsonTag(f reflect.StructField) (name string, omitempty bool, err error) {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return "", false, errors.New("missing json tag")
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return "", false, fmt.Errorf("json tag %q must name the field", tag)
	}
	return name, slices.Contains(strings.Split(opts, ","), "omitempty"), nil
}

// fieldSchema returns the property schema of one struct field.
func (g *generator) fieldSchema(parent reflect.Type, f reflect.StructField, d docInfo, envelope bool) (schema, bool, error) {
	ctx := fieldCtx{envelope: envelope, noSubst: envelope}
	_, isRequired := d.get("required")
	annotations := schema{}
	if d.description != "" {
		annotations["description"] = d.description
	}
	isSlice := f.Type.Kind() == reflect.Slice && f.Type != g.known.rawMessage
	base := f.Type
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	// The secretRef inside SecretValue is the secret value's own shape.
	isSecret := parent != g.known.secretValue && (base == g.known.secretValue || base == g.known.secretRef)
	_, secretMarked := d.get("secret")
	if isSecret != secretMarked {
		return nil, false, errors.New("+ruralz:secret must mark exactly the SecretValue and SecretRef fields")
	}
	if !isRequired && f.Type.Kind() != reflect.Pointer && isPresenceSensitive(f.Type) {
		return nil, false, errors.New("an optional bool or number must be a pointer")
	}
	if _, hasDefault := d.get("default"); hasDefault && f.Type.Kind() != reflect.Pointer {
		return nil, false, errors.New("a field with a default must be a pointer")
	}
	for _, m := range d.markers {
		switch m.name {
		case "required":
		case "secret":
			annotations["x-ruralz-secret"] = true
			ctx.noSubst = true
		case "ref":
			if !slices.Contains(g.in.source["Kind"].enumOrNil(), m.value) {
				return nil, false, fmt.Errorf("+ruralz:ref=%s is not a kind", m.value)
			}
			annotations["x-ruralz-ref"] = m.value
			ctx.noSubst = true
		case "cel":
			vars, result, ok := strings.Cut(m.value, ":")
			if !ok || vars == "" || !slices.Contains([]string{"bool", "string", "dyn"}, result) {
				return nil, false, fmt.Errorf("+ruralz:cel=%s: want <variables>:<bool|string|dyn>", m.value)
			}
			annotations["x-ruralz-cel"] = schema{"variables": toAny(strings.Split(vars, ",")), "result": result}
			ctx.noSubst = true
		case "list":
			if !isSlice {
				return nil, false, errors.New("+ruralz:list is only for lists")
			}
			l, err := listKeyword(m.value)
			if err != nil {
				return nil, false, err
			}
			annotations["x-ruralz-list"] = l
		case "impact":
			var classes []any
			for _, c := range strings.Split(m.value, ",") {
				if !slices.Contains([]string{"routing", "security", "traffic", "plugin", "ai", "metadata"}, c) {
					return nil, false, fmt.Errorf("unknown impact class %q", c)
				}
				classes = append(classes, c)
			}
			annotations["x-ruralz-impact"] = classes
		case "since":
			n, err := strconv.Atoi(m.value)
			if err != nil || n < 1 {
				return nil, false, fmt.Errorf("+ruralz:since=%s: want a level of 1 or more", m.value)
			}
			annotations["x-ruralz-since"] = n
		case "validation":
			annotations["x-ruralz-validations"] = append(asList(annotations["x-ruralz-validations"]), schema{"rule": m.value})
		case "default":
		default:
			if !isValueMarker(m.name) {
				return nil, false, fmt.Errorf("marker %s is not allowed on a field", m.name)
			}
		}
	}
	if isSlice {
		if _, ok := d.get("list"); !ok {
			return nil, false, errors.New("a list field needs +ruralz:list")
		}
	}

	value, err := g.typeSchema(f.Type, ctx)
	if err != nil {
		return nil, false, err
	}
	value = shallowCopy(value)
	if err := applyValueMarkers(value, d, "field"); err != nil {
		return nil, false, err
	}
	if m, ok := d.get("default"); ok {
		dv, err := g.defaultValue(f.Type, m.value)
		if err != nil {
			return nil, false, fmt.Errorf("+ruralz:default: %w", err)
		}
		annotations["default"] = dv
	}
	if !isSlice {
		value = g.substitutable(value, ctx)
	}
	for k, v := range value {
		if _, clash := annotations[k]; clash {
			return nil, false, fmt.Errorf("keyword %s set twice", k)
		}
		annotations[k] = v
	}
	return annotations, isRequired, nil
}

// substitutable adds the ${VAR} alternative in the authoring view to a
// constrained non-string scalar that permits substitution.
func (g *generator) substitutable(value schema, ctx fieldCtx) schema {
	if g.view != authoring || ctx.noSubst || !g.isNonStringScalar(value) {
		return value
	}
	return schema{"anyOf": []any{value, ref(substDefName)}}
}

// isNonStringScalar reports whether instances are integers, numbers or
// booleans, or a definition that accepts them (ByteSize, IntOrString).
func (g *generator) isNonStringScalar(s schema) bool {
	if r, ok := s["$ref"].(string); ok {
		name := strings.TrimPrefix(r, defsPrefix)
		switch name {
		case "ByteSize", "IntOrString":
			return true
		default:
			return false
		}
	}
	switch s["type"] {
	case "integer", "number", "boolean":
		return true
	default:
		return false
	}
}

func isPresenceSensitive(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// isValueMarker reports markers that emit standard value keywords.
func isValueMarker(name string) bool {
	switch name {
	case "minItems", "minimum", "maximum", "pattern", "minLength", "maxLength":
		return true
	default:
		return false
	}
}

// applyValueMarkers adds standard value keywords to s.
func applyValueMarkers(s schema, d docInfo, where string) error {
	for _, m := range d.markers {
		if !isValueMarker(m.name) {
			continue
		}
		switch m.name {
		case "pattern":
			s["pattern"] = m.value
		case "minimum", "maximum":
			n, err := strconv.ParseFloat(m.value, 64)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", where, m.name, err)
			}
			if n == float64(int64(n)) {
				s[m.name] = int64(n)
			} else {
				s[m.name] = n
			}
		default: // minItems, minLength, maxLength
			n, err := strconv.Atoi(m.value)
			if err != nil || n < 0 {
				return fmt.Errorf("%s: %s=%s: want a non-negative integer", where, m.name, m.value)
			}
			s[m.name] = n
		}
	}
	return nil
}

// defaultValue parses a +ruralz:default value for the field type.
func (g *generator) defaultValue(t reflect.Type, raw string) (any, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case g.known.duration:
		var d v1alpha1.Duration
		if err := json.Unmarshal([]byte(strconv.Quote(raw)), &d); err != nil {
			return nil, err
		}
		return time.Duration(d).String(), nil
	case g.known.byteSize:
		b, err := v1alpha1.ParseByteSize(raw)
		if err != nil {
			return nil, err
		}
		return int64(b), nil
	default:
	}
	switch t.Kind() {
	case reflect.String:
		s := raw
		if unq, err := strconv.Unquote(raw); err == nil {
			s = unq
		}
		if info := g.in.source[t.Name()]; t.Name() != "" && info != nil && len(info.enum) > 0 && !slices.Contains(info.enum, s) {
			return nil, fmt.Errorf("%q is not a %s value", s, t.Name())
		}
		return s, nil
	case reflect.Bool:
		return strconv.ParseBool(raw)
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		return strconv.ParseInt(raw, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(raw, 64)
	default:
		return nil, fmt.Errorf("no default syntax for %s", t)
	}
}

// listKeyword parses "<type>[,key=<field>]" into the x-ruralz-list value.
func listKeyword(v string) (schema, error) {
	kind, rest, hasKey := strings.Cut(v, ",")
	switch kind {
	case "map", "orderedMap":
		key, ok := strings.CutPrefix(rest, "key=")
		if !hasKey || !ok || key == "" {
			return nil, fmt.Errorf("+ruralz:list=%s needs key=<field>", kind)
		}
		return schema{"type": kind, "key": key}, nil
	case "set", "atomic":
		if hasKey {
			return nil, fmt.Errorf("+ruralz:list=%s takes no key", kind)
		}
		return schema{"type": kind}, nil
	default:
		return nil, fmt.Errorf("unknown list type %q", kind)
	}
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func shallowCopy(s schema) schema {
	out := make(schema, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}
