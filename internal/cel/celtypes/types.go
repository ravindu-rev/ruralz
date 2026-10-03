// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// CEL type names of the Ruralz object types (03 req 17). They are part of
// the contract: they appear in diagnostics and in type(x).
const (
	TypeRequest  = "ruralz.Request"
	TypeSource   = "ruralz.Source"
	TypeRoute    = "ruralz.Route"
	TypeConsumer = "ruralz.Consumer"
	TypeAuth     = "ruralz.Auth"
	TypeResponse = "ruralz.Response"
	TypeError    = "ruralz.Error"
	TypeUpstream = "ruralz.Upstream"
	TypeStep     = "ruralz.Step"
	TypeAI       = "ruralz.AI"
)

// CEL variable names; they equal the spellings of expr.Var.Names.
const (
	varRequest  = "request"
	varSource   = "source"
	varRoute    = "route"
	varConsumer = "consumer"
	varAuth     = "auth"
	varNow      = "now"
	varResponse = "response"
	varError    = "error"
	varUpstream = "upstream"
	varAttempt  = "attempt"
	varSteps    = "steps"
	varDuration = "duration"
	varAI       = "ai"
)

// objKind is one Ruralz object type. It is also the ref.Type of the type's
// values: a small integer, so returning it from Type allocates nothing.
type objKind uint8

// Object kinds, in the order of 03 requirement 17.
const (
	kindRequest objKind = iota
	kindSource
	kindRoute
	kindConsumer
	kindAuth
	kindResponse
	kindError
	kindUpstream
	kindStep
	kindAI
	numKinds
)

// objectTraits are the traits of every object type: field selection and
// presence tests, as for cel-go object types.
const objectTraits = traits.FieldTesterType | traits.IndexerType

// TypeName returns the CEL type name.
func (k objKind) TypeName() string {
	switch k {
	case kindRequest:
		return TypeRequest
	case kindSource:
		return TypeSource
	case kindRoute:
		return TypeRoute
	case kindConsumer:
		return TypeConsumer
	case kindAuth:
		return TypeAuth
	case kindResponse:
		return TypeResponse
	case kindError:
		return TypeError
	case kindUpstream:
		return TypeUpstream
	case kindStep:
		return TypeStep
	case kindAI:
		return TypeAI
	default:
		return ""
	}
}

// HasTrait reports whether the object type has trait (ref.Type).
func (k objKind) HasTrait(trait int) bool { return trait&objectTraits == trait }

// IsObjectType reports whether name is a Ruralz object type, such as
// "ruralz.Error" (a leading dot, cel-go's absolute form, is accepted).
// Validation uses it to reject a struct construction of a Ruralz type in a
// checked AST as RZ-CFG-014: the views are built by the data plane only, so
// the Provider fails such a construction at evaluation (03 req 17, 27).
func IsObjectType(name string) bool {
	_, ok := kindByName(name)
	return ok
}

// kindByName returns the object kind named name ("ruralz.Request"; a
// leading dot is accepted as cel-go's absolute form).
func kindByName(name string) (objKind, bool) {
	if len(name) > 0 && name[0] == '.' {
		name = name[1:]
	}
	switch name {
	case TypeRequest:
		return kindRequest, true
	case TypeSource:
		return kindSource, true
	case TypeRoute:
		return kindRoute, true
	case TypeConsumer:
		return kindConsumer, true
	case TypeAuth:
		return kindAuth, true
	case TypeResponse:
		return kindResponse, true
	case TypeError:
		return kindError, true
	case TypeUpstream:
		return kindUpstream, true
	case TypeStep:
		return kindStep, true
	case TypeAI:
		return kindAI, true
	default:
		return 0, false
	}
}

// fieldType is the declared CEL type of a field.
type fieldType uint8

// Field types.
const (
	ftString     fieldType = iota + 1 // string
	ftInt                             // int
	ftBool                            // bool
	ftStringMap                       // map(string, string)
	ftStringList                      // list(string)
	ftDyn                             // dyn
)

// celType returns the CEL type of t.
func (t fieldType) celType() *types.Type {
	switch t {
	case ftString:
		return types.StringType
	case ftInt:
		return types.IntType
	case ftBool:
		return types.BoolType
	case ftStringMap:
		return types.NewMapType(types.StringType, types.StringType)
	case ftStringList:
		return types.NewListType(types.StringType)
	default:
		return types.DynType
	}
}

// fieldSpec declares one field.
type fieldSpec struct {
	name string
	typ  fieldType
}

// Field numbers per object type; a field's number is its index in
// objectFields.
const (
	reqMethod = iota
	reqScheme
	reqHost
	reqPath
	reqPathParams
	reqQuery
	reqHeaders
	reqBody
)

const (
	srcIP = iota
	srcPort
	srcTLSVersion
	srcClientCertSubject
)

const (
	routeName = iota
	routeLabels
)

const (
	consumerName = iota
	consumerTier
	consumerTags
	consumerLabels
	consumerQuotas
)

const (
	authMethod = iota
	authClaims
)

const (
	respStatus = iota
	respHeaders
	respBody
)

const errKind = 0

const (
	upstreamName = iota
	upstreamEndpoint
)

const (
	stepStatus = iota
	stepHeaders
	stepBody
)

const (
	aiModel = iota
	aiEstimatedInputTokens
	aiMaxOutputTokens
	aiStream
)

// objectFields returns the fields of k in declaration order (03 req 17).
// It builds a new slice; the request path uses field numbers instead.
func objectFields(k objKind) []fieldSpec {
	switch k {
	case kindRequest:
		return []fieldSpec{
			reqMethod:     {"method", ftString},
			reqScheme:     {"scheme", ftString},
			reqHost:       {"host", ftString},
			reqPath:       {"path", ftString},
			reqPathParams: {"pathParams", ftStringMap},
			reqQuery:      {"query", ftStringMap},
			reqHeaders:    {"headers", ftStringMap},
			reqBody:       {"body", ftDyn},
		}
	case kindSource:
		return []fieldSpec{
			srcIP:                {"ip", ftString},
			srcPort:              {"port", ftInt},
			srcTLSVersion:        {"tlsVersion", ftString},
			srcClientCertSubject: {"clientCertSubject", ftString},
		}
	case kindRoute:
		return []fieldSpec{
			routeName:   {"name", ftString},
			routeLabels: {"labels", ftStringMap},
		}
	case kindConsumer:
		return []fieldSpec{
			consumerName:   {"name", ftString},
			consumerTier:   {"tier", ftString},
			consumerTags:   {"tags", ftStringList},
			consumerLabels: {"labels", ftStringMap},
			consumerQuotas: {"quotas", ftStringList},
		}
	case kindAuth:
		return []fieldSpec{
			authMethod: {"method", ftString},
			authClaims: {"claims", ftDyn},
		}
	case kindResponse:
		return []fieldSpec{
			respStatus:  {"status", ftInt},
			respHeaders: {"headers", ftStringMap},
			respBody:    {"body", ftDyn},
		}
	case kindError:
		return []fieldSpec{
			errKind: {"kind", ftString},
		}
	case kindUpstream:
		return []fieldSpec{
			upstreamName:     {"name", ftString},
			upstreamEndpoint: {"endpoint", ftString},
		}
	case kindStep:
		return []fieldSpec{
			stepStatus:  {"status", ftInt},
			stepHeaders: {"headers", ftStringMap},
			stepBody:    {"body", ftDyn},
		}
	case kindAI:
		return []fieldSpec{
			aiModel:                {"model", ftString},
			aiEstimatedInputTokens: {"estimatedInputTokens", ftInt},
			aiMaxOutputTokens:      {"maxOutputTokens", ftInt},
			aiStream:               {"stream", ftBool},
		}
	default:
		return nil
	}
}

// fieldNumber returns the number of the field of k named name.
func fieldNumber(k objKind, name string) (int, bool) {
	for i, f := range objectFields(k) {
		if f.name == name {
			return i, true
		}
	}
	return 0, false
}

// Variable is one CEL variable with its declared type (03 req 17).
type Variable struct {
	// Bit is the variable's expr.Var bit.
	Bit expr.Var
	// Name is the CEL name, the x-ruralz-cel spelling.
	Name string
	// Type is the declared CEL type.
	Type *types.Type
}

// Variables returns the variables of vs in expr.Var declaration order with
// their CEL types: request ruralz.Request, source ruralz.Source, route
// ruralz.Route, consumer ruralz.Consumer, auth ruralz.Auth, now
// google.protobuf.Timestamp, response ruralz.Response, error ruralz.Error,
// upstream ruralz.Upstream, attempt int, steps map(string, ruralz.Step),
// duration google.protobuf.Duration and ai ruralz.AI. expr.VarSelf (M2) has
// no declared type yet and is left out.
func Variables(vs expr.Var) []Variable {
	all := []Variable{
		{expr.VarRequest, varRequest, types.NewObjectType(TypeRequest)},
		{expr.VarSource, varSource, types.NewObjectType(TypeSource)},
		{expr.VarRoute, varRoute, types.NewObjectType(TypeRoute)},
		{expr.VarConsumer, varConsumer, types.NewObjectType(TypeConsumer)},
		{expr.VarAuth, varAuth, types.NewObjectType(TypeAuth)},
		{expr.VarNow, varNow, types.TimestampType},
		{expr.VarResponse, varResponse, types.NewObjectType(TypeResponse)},
		{expr.VarError, varError, types.NewObjectType(TypeError)},
		{expr.VarUpstream, varUpstream, types.NewObjectType(TypeUpstream)},
		{expr.VarAttempt, varAttempt, types.IntType},
		{expr.VarSteps, varSteps, types.NewMapType(types.StringType, types.NewObjectType(TypeStep))},
		{expr.VarDuration, varDuration, types.DurationType},
		{expr.VarAI, varAI, types.NewObjectType(TypeAI)},
	}
	out := all[:0]
	for _, v := range all {
		if vs&v.Bit != 0 {
			out = append(out, v)
		}
	}
	return out
}
