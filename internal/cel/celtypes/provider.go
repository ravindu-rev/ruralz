// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Provider declares the Ruralz object types to cel-go and adapts Go values
// to CEL values (03 req 17). It implements types.Provider and types.Adapter
// and delegates every other type to a cel-go registry, so the standard and
// well-known protobuf types keep working. Pass it to both
// cel.CustomTypeProvider and cel.CustomTypeAdapter. It is immutable and safe
// for concurrent use.
type Provider struct {
	base    *types.Registry
	objects [numKinds]objectDecl
}

// objectDecl is the cel-go declaration of one object type.
type objectDecl struct {
	typ      *types.Type
	typeType *types.Type
	names    []string
	fields   map[string]*types.FieldType
}

// Compile-time checks of the cel-go interfaces.
var (
	_ types.Provider = (*Provider)(nil)
	_ types.Adapter  = (*Provider)(nil)
)

// NewProvider returns a Provider over a new cel-go registry.
func NewProvider() (*Provider, error) {
	base, err := types.NewRegistry()
	if err != nil {
		return nil, fmt.Errorf("celtypes: registry: %w", err)
	}
	p := &Provider{base: base}
	for k := range numKinds {
		t := types.NewObjectType(k.TypeName())
		d := objectDecl{typ: t, typeType: types.NewTypeTypeWithParam(t), fields: map[string]*types.FieldType{}}
		for i, f := range objectFields(k) {
			d.names = append(d.names, f.name)
			d.fields[f.name] = &types.FieldType{
				Type:    f.typ.celType(),
				IsSet:   tester(k, i),
				GetFrom: getter(k, i),
			}
		}
		p.objects[k] = d
	}
	return p, nil
}

// object returns the declaration of the Ruralz type named name.
func (p *Provider) object(name string) (*objectDecl, bool) {
	k, ok := kindByName(name)
	if !ok {
		return nil, false
	}
	return &p.objects[k], true
}

// EnumValue returns the value of a protobuf enum constant.
func (p *Provider) EnumValue(enumName string) ref.Val { return p.base.EnumValue(enumName) }

// FindIdent resolves a type name used as a value, such as ruralz.Request
// in type(request) == ruralz.Request.
func (p *Provider) FindIdent(identName string) (ref.Val, bool) {
	if d, ok := p.object(identName); ok {
		return d.typ, true
	}
	return p.base.FindIdent(identName)
}

// FindStructType returns the type-type of a Ruralz or registered type.
func (p *Provider) FindStructType(structType string) (*types.Type, bool) {
	if d, ok := p.object(structType); ok {
		return d.typeType, true
	}
	return p.base.FindStructType(structType)
}

// FindStructFieldNames returns the fields of a type in declaration order.
func (p *Provider) FindStructFieldNames(structType string) ([]string, bool) {
	if d, ok := p.object(structType); ok {
		return slices.Clone(d.names), true
	}
	return p.base.FindStructFieldNames(structType)
}

// FindStructFieldType returns a field's type and its accessors, which read
// the expr view directly (no reflection).
func (p *Provider) FindStructFieldType(structType, fieldName string) (*types.FieldType, bool) {
	if d, ok := p.object(structType); ok {
		ft, found := d.fields[fieldName]
		return ft, found
	}
	return p.base.FindStructFieldType(structType, fieldName)
}

// NewValue creates a registered type's value. Ruralz views are built by the
// data plane only, so constructing one in CEL is an error.
func (p *Provider) NewValue(structType string, fields map[string]ref.Val) ref.Val {
	if d, ok := p.object(structType); ok {
		return errVal(fmt.Errorf("%w: %s values cannot be constructed", ErrUnsupported, d.typ.TypeName()))
	}
	return p.base.NewValue(structType, fields)
}

// NativeToValue adapts a Go value: CEL values as they are, expr views and
// Values to their CEL values (nil pointers to null), headers and JSON trees
// to the ordered map views; anything else through the cel-go registry.
func (p *Provider) NativeToValue(value any) ref.Val {
	switch v := value.(type) {
	case ref.Val:
		return v
	case *expr.Request:
		return viewOf(v)
	case *expr.Source:
		return viewOf(v)
	case *expr.Route:
		return viewOf(v)
	case *expr.Consumer:
		return viewOf(v)
	case *expr.Auth:
		return viewOf(v)
	case *expr.Response:
		return viewOf(v)
	case *expr.AttemptError:
		return viewOf(v)
	case *expr.Upstream:
		return viewOf(v)
	case *expr.Step:
		return viewOf(v)
	case *expr.AI:
		return viewOf(v)
	case *expr.Steps:
		return mapView[stepsSource]{stepsSource{v}}
	case expr.Value:
		return ToVal(v)
	case http.Header:
		return mapView[headerSource]{headerSource{&v}}
	case map[string]string:
		return mapView[stringSource]{stringSource{&v}}
	case map[string]any, []any, json.Number:
		return nativeVal(v)
	}
	return p.base.NativeToValue(value)
}
