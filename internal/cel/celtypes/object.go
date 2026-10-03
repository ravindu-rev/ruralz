// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"crypto/tls"
	"fmt"
	"net/netip"
	"reflect"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// viewStruct lists the expr views that are CEL objects.
type viewStruct interface {
	expr.Request | expr.Source | expr.Route | expr.Consumer | expr.Auth |
		expr.Response | expr.AttemptError | expr.Upstream | expr.Step | expr.AI
}

// object is the CEL value of an expr view. It holds one pointer, so boxing
// it in a ref.Val allocates nothing; fields are read on selection only.
type object[T viewStruct] struct{ p *T }

// viewOf returns the CEL value of p: null for a nil pointer (03 req 18).
func viewOf[T viewStruct](p *T) ref.Val {
	if p == nil {
		return types.NullValue
	}
	return object[T]{p}
}

// kind returns the object's type.
func (o object[T]) kind() objKind {
	switch any(o.p).(type) {
	case *expr.Request:
		return kindRequest
	case *expr.Source:
		return kindSource
	case *expr.Route:
		return kindRoute
	case *expr.Consumer:
		return kindConsumer
	case *expr.Auth:
		return kindAuth
	case *expr.Response:
		return kindResponse
	case *expr.AttemptError:
		return kindError
	case *expr.Upstream:
		return kindUpstream
	case *expr.Step:
		return kindStep
	default:
		return kindAI
	}
}

// ConvertToNative returns the view pointer for its own type or any.
func (o object[T]) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if typeDesc == reflect.TypeOf(o.p) || typeDesc == reflect.TypeFor[any]() {
		return o.p, nil
	}
	return nil, fmt.Errorf("%w: %s to %v", ErrUnsupported, o.kind().TypeName(), typeDesc)
}

// ConvertToType supports type(x) and the identity conversion.
func (o object[T]) ConvertToType(t ref.Type) ref.Val {
	k := o.kind()
	switch {
	case t == types.TypeType:
		return types.NewObjectType(k.TypeName())
	case t.TypeName() == k.TypeName():
		return o
	}
	return types.NewErr("type conversion error from '%s' to '%s'", k.TypeName(), t.TypeName())
}

// Equal reports whether other is the same view or one with equal fields.
func (o object[T]) Equal(other ref.Val) ref.Val {
	x, ok := other.(object[T])
	if !ok {
		return types.False
	}
	if x.p == o.p {
		return types.True
	}
	k := o.kind()
	for i := range objectFields(k) {
		a, errA := getField(k, i, o.p)
		b, errB := getField(k, i, x.p)
		if errA != nil || errB != nil {
			if (errA == nil) != (errB == nil) {
				return types.False
			}
			continue
		}
		if types.Equal(a.(ref.Val), b.(ref.Val)) != types.True {
			return types.False
		}
	}
	return types.True
}

// Type returns the object type.
func (o object[T]) Type() ref.Type { return o.kind() }

// Value returns the view pointer; cel-go field accessors receive it.
func (o object[T]) Value() any { return o.p }

// Get selects a field by name (dynamic access, traits.Indexer).
func (o object[T]) Get(key ref.Val) ref.Val {
	k := o.kind()
	name, ok := key.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(key)
	}
	i, ok := fieldNumber(k, string(name))
	if !ok {
		return errVal(noSuchField(k.TypeName()))
	}
	v, err := getField(k, i, o.p)
	if err != nil {
		return errVal(err)
	}
	return v.(ref.Val)
}

// IsSet tests a field's presence by name (traits.FieldTester).
func (o object[T]) IsSet(field ref.Val) ref.Val {
	k := o.kind()
	name, ok := field.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(field)
	}
	i, ok := fieldNumber(k, string(name))
	if !ok {
		return errVal(noSuchField(k.TypeName()))
	}
	return types.Bool(isFieldSet(k, i, o.p))
}

// getter returns the cel-go field accessor of field i of k.
func getter(k objKind, i int) ref.FieldGetter {
	return func(target any) (any, error) { return getField(k, i, target) }
}

// tester returns the cel-go presence test of field i of k.
func tester(k objKind, i int) ref.FieldTester {
	return func(target any) bool { return isFieldSet(k, i, target) }
}

// getField returns field i of the k view target, a ref.Val. target is the
// view pointer (object.Value), or null for an absent variable.
func getField(k objKind, i int, target any) (any, error) {
	switch k {
	case kindRequest:
		if r, ok := target.(*expr.Request); ok && r != nil {
			return requestField(r, i)
		}
	case kindSource:
		if s, ok := target.(*expr.Source); ok && s != nil {
			return sourceField(s, i), nil
		}
	case kindRoute:
		if r, ok := target.(*expr.Route); ok && r != nil {
			return routeField(r, i), nil
		}
	case kindConsumer:
		if c, ok := target.(*expr.Consumer); ok && c != nil {
			return consumerField(c, i), nil
		}
	case kindAuth:
		if a, ok := target.(*expr.Auth); ok && a != nil {
			return authField(a, i)
		}
	case kindResponse:
		if r, ok := target.(*expr.Response); ok && r != nil {
			return responseField(r, i)
		}
	case kindError:
		if e, ok := target.(*expr.AttemptError); ok && e != nil {
			return types.String(e.Kind), nil
		}
	case kindUpstream:
		if u, ok := target.(*expr.Upstream); ok && u != nil {
			return upstreamField(u, i), nil
		}
	case kindStep:
		if s, ok := target.(*expr.Step); ok && s != nil {
			return stepField(s, i)
		}
	case kindAI:
		if a, ok := target.(*expr.AI); ok && a != nil {
			return aiField(a, i), nil
		}
	case numKinds:
	}
	return nil, selectError(k, i, target)
}

// selectError is the error of selecting field i of k on target, which is
// not a k view: null for an absent variable.
func selectError(k objKind, i int, target any) error {
	fields := objectFields(k)
	name := ""
	if i >= 0 && i < len(fields) {
		name = fields[i].name
	}
	if isNullTarget(target) {
		return nullSelect(k.TypeName(), name)
	}
	return fmt.Errorf("%w: %s.%s on %s", ErrUnsupported, k.TypeName(), name, typeNameOf(target))
}

// isNullTarget reports a CEL null (as a value or as cel-go's Null.Value)
// or a nil view pointer.
func isNullTarget(target any) bool {
	if target == nil || target == types.NullValue || target == types.NullValue.Value() {
		return true
	}
	switch p := target.(type) {
	case *expr.Request:
		return p == nil
	case *expr.Source:
		return p == nil
	case *expr.Route:
		return p == nil
	case *expr.Consumer:
		return p == nil
	case *expr.Auth:
		return p == nil
	case *expr.Response:
		return p == nil
	case *expr.AttemptError:
		return p == nil
	case *expr.Upstream:
		return p == nil
	case *expr.Step:
		return p == nil
	case *expr.AI:
		return p == nil
	}
	return false
}

// typeNameOf names the CEL or Go type of v for error text.
func typeNameOf(v any) string {
	if rv, ok := v.(ref.Val); ok {
		return rv.Type().TypeName()
	}
	return fmt.Sprintf("%T", v)
}

// isFieldSet reports whether field i of the k view target holds a non-default
// value, as has() tests a proto3 field: a non-empty string, a non-zero int,
// true, a non-empty map or list, an available body. has() on null is false.
func isFieldSet(k objKind, i int, target any) bool {
	switch k {
	case kindRequest:
		if r, ok := target.(*expr.Request); ok && r != nil {
			return requestFieldSet(r, i)
		}
	case kindSource:
		if s, ok := target.(*expr.Source); ok && s != nil {
			return sourceFieldSet(s, i)
		}
	case kindRoute:
		if r, ok := target.(*expr.Route); ok && r != nil {
			return i == routeName && r.Name != "" || i == routeLabels && len(r.Labels) > 0
		}
	case kindConsumer:
		if c, ok := target.(*expr.Consumer); ok && c != nil {
			return consumerFieldSet(c, i)
		}
	case kindAuth:
		if a, ok := target.(*expr.Auth); ok && a != nil {
			return i == authMethod && a.Method != "" || i == authClaims && a.Claims != nil
		}
	case kindResponse:
		if r, ok := target.(*expr.Response); ok && r != nil {
			return i == respStatus && r.Status != 0 || i == respHeaders && headerSize(r.Header, false) > 0 ||
				i == respBody && r.Body != nil
		}
	case kindError:
		if e, ok := target.(*expr.AttemptError); ok && e != nil {
			return e.Kind != ""
		}
	case kindUpstream:
		if u, ok := target.(*expr.Upstream); ok && u != nil {
			return i == upstreamName && u.Name != "" || i == upstreamEndpoint && u.Endpoint != ""
		}
	case kindStep:
		if s, ok := target.(*expr.Step); ok && s != nil {
			return i == stepStatus && s.Status != 0 || i == stepHeaders && headerSize(s.Header, false) > 0 ||
				i == stepBody && s.Body != nil
		}
	case kindAI:
		if a, ok := target.(*expr.AI); ok && a != nil {
			return aiFieldSet(a, i)
		}
	case numKinds:
	}
	return false
}

// requestField returns field i of r (03 req 20). The maps read r lazily:
// headers wrap the live header map, query parses on first selection.
func requestField(r *expr.Request, i int) (any, error) {
	switch i {
	case reqMethod:
		return types.String(r.Method), nil
	case reqScheme:
		return types.String(r.Scheme), nil
	case reqHost:
		return types.String(r.Host), nil
	case reqPath:
		return types.String(r.Path), nil
	case reqPathParams:
		return mapView[paramSource]{paramSource{&r.PathParams}}, nil
	case reqQuery:
		return mapView[querySource]{querySource{r}}, nil
	case reqHeaders:
		return mapView[requestHeaderSource]{requestHeaderSource{&r.Header}}, nil
	default:
		return bodyField(r.Body)
	}
}

// requestFieldSet is has() on field i of r.
func requestFieldSet(r *expr.Request, i int) bool {
	switch i {
	case reqMethod:
		return r.Method != ""
	case reqScheme:
		return r.Scheme != ""
	case reqHost:
		return r.Host != ""
	case reqPath:
		return r.Path != ""
	case reqPathParams:
		return len(r.PathParams) > 0
	case reqQuery:
		return len(r.Query()) > 0
	case reqHeaders:
		return headerSize(r.Header, true) > 0
	default:
		return r.Body != nil
	}
}

// sourceField returns field i of s (03 req 22).
func sourceField(s *expr.Source, i int) ref.Val {
	switch i {
	case srcIP:
		return types.String(ipString(s.IP))
	case srcPort:
		return types.Int(s.Port)
	case srcTLSVersion:
		return tlsVersionVal(s.TLSVersion)
	default:
		return types.String(s.ClientCertSubject)
	}
}

// sourceFieldSet is has() on field i of s.
func sourceFieldSet(s *expr.Source, i int) bool {
	switch i {
	case srcIP:
		return s.IP.IsValid()
	case srcPort:
		return s.Port != 0
	case srcTLSVersion:
		return tlsVersionVal(s.TLSVersion) != types.String("")
	default:
		return s.ClientCertSubject != ""
	}
}

// ipString is source.ip: IPv4-mapped IPv6 unmapped, IPv6 in RFC 5952 form
// without a zone, "" for no address.
func ipString(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.Unmap().WithZone("").String()
}

// tlsVersionVal is source.tlsVersion: "1.2" or "1.3" (the tls.minVersion
// spellings), "" on cleartext; ruralzd negotiates nothing older, so any
// other version is also "".
func tlsVersionVal(v uint16) ref.Val {
	switch v {
	case tls.VersionTLS13:
		return types.String("1.3")
	case tls.VersionTLS12:
		return types.String("1.2")
	default:
		return types.String("")
	}
}

// routeField returns field i of r (03 req 23).
func routeField(r *expr.Route, i int) ref.Val {
	if i == routeName {
		return types.String(r.Name)
	}
	if p, ok := r.Prepared.(*prepared); ok && p.labels != nil {
		return p.labels
	}
	return mapView[stringSource]{stringSource{&r.Labels}}
}

// consumerField returns field i of c (03 req 23).
func consumerField(c *expr.Consumer, i int) ref.Val {
	switch i {
	case consumerName:
		return types.String(c.Name)
	case consumerTier:
		return types.String(c.Tier)
	case consumerTags:
		return listView[stringsSource]{stringsSource{&c.Tags}}
	case consumerLabels:
		if p, ok := c.Prepared.(*prepared); ok && p.labels != nil {
			return p.labels
		}
		return mapView[stringSource]{stringSource{&c.Labels}}
	default:
		return listView[stringsSource]{stringsSource{&c.Quotas}}
	}
}

// consumerFieldSet is has() on field i of c.
func consumerFieldSet(c *expr.Consumer, i int) bool {
	switch i {
	case consumerName:
		return c.Name != ""
	case consumerTier:
		return c.Tier != ""
	case consumerTags:
		return len(c.Tags) > 0
	case consumerLabels:
		return len(c.Labels) > 0
	default:
		return len(c.Quotas) > 0
	}
}

// authField returns field i of a (03 req 23): claims is an empty map for
// methods other than jwt.
func authField(a *expr.Auth, i int) (any, error) {
	if i == authMethod {
		return types.String(a.Method), nil
	}
	if a.Claims == nil {
		return mapView[*jsonObject]{}, nil
	}
	return bodyField(a.Claims)
}

// responseField returns field i of r (03 req 24).
func responseField(r *expr.Response, i int) (any, error) {
	switch i {
	case respStatus:
		return types.Int(r.Status), nil
	case respHeaders:
		return mapView[headerSource]{headerSource{&r.Header}}, nil
	default:
		return bodyField(r.Body)
	}
}

// upstreamField returns field i of u (03 req 24).
func upstreamField(u *expr.Upstream, i int) ref.Val {
	if i == upstreamName {
		return types.String(u.Name)
	}
	return types.String(u.Endpoint)
}

// stepField returns field i of s (03 req 24).
func stepField(s *expr.Step, i int) (any, error) {
	switch i {
	case stepStatus:
		return types.Int(s.Status), nil
	case stepHeaders:
		return mapView[headerSource]{headerSource{&s.Header}}, nil
	default:
		return bodyField(s.Body)
	}
}

// aiField returns field i of a (declared for validation; evaluated from M3).
func aiField(a *expr.AI, i int) ref.Val {
	switch i {
	case aiModel:
		return types.String(a.Model)
	case aiEstimatedInputTokens:
		return types.Int(a.EstimatedInputTokens)
	case aiMaxOutputTokens:
		return types.Int(a.MaxOutputTokens)
	default:
		return types.Bool(a.Stream)
	}
}

// aiFieldSet is has() on field i of a.
func aiFieldSet(a *expr.AI, i int) bool {
	switch i {
	case aiModel:
		return a.Model != ""
	case aiEstimatedInputTokens:
		return a.EstimatedInputTokens != 0
	case aiMaxOutputTokens:
		return a.MaxOutputTokens != 0
	default:
		return a.Stream
	}
}

// bodyField returns the CEL value of a body or claims Value: an error
// wrapping ErrBody when it is not available or did not decode. The error is
// returned as a plain error, so cel-go wraps it in a *types.Err of this
// evaluation's own and labels that with the node ID (03 req 25, 38).
func bodyField(v expr.Value) (any, error) {
	if v == nil {
		return nil, errBodyUnavailable()
	}
	val := ToVal(v)
	if e, ok := val.(*types.Err); ok {
		return nil, plainError(e)
	}
	return val, nil
}
