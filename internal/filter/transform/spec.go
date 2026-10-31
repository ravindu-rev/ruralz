// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Policy refuses (package documentation,
	// "Configuration").
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is a CEL field with a syntax, type or unavailable-variable
	// error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is a CEL field whose static cost estimate exceeds the
	// ceiling.
	CodeCost = "RZ-CFG-015"
)

// Limits of a transform config and of the values it writes (target,
// docs/architecture/03-data-plane.md "Transform Policies").
const (
	// MaxEntries is the most entries one Policy holds across its set,
	// remove, arrayOps and replace lists.
	MaxEntries = 32
	// MaxPatternBytes is the longest replace pattern, in bytes.
	MaxPatternBytes = 1 << 10
	// MaxPathBytes is the longest path a set entry with target path writes,
	// as computed and as forwarded.
	MaxPathBytes = 8 << 10
	// DefaultContentType is the Content-Type of a body config.body builds
	// without contentType, and of a body the body operations write where
	// there was none.
	DefaultContentType = "application/json"
)

// Side is the message a transform acts on.
type Side uint8

// Sides.
const (
	// Request is transform.request: the client request at Gateway and
	// Route scope, each attempt's outgoing request at Upstream scope.
	Request Side = iota + 1
	// Response is transform.response: the client response at Gateway and
	// Route scope, each leg's response at Upstream scope.
	Response
)

// PolicyType returns the Policy type of the side.
func (s Side) PolicyType() v1alpha1.PolicyType {
	if s == Response {
		return v1alpha1.PolicyTypeTransformResponse
	}
	return v1alpha1.PolicyTypeTransformRequest
}

// bodyPlace returns the CEL place of config.body.
func (s Side) bodyPlace() expr.PlaceID {
	if s == Response {
		return expr.PlaceTransformResponseBody
	}
	return expr.PlaceTransformRequestBody
}

// valuePlace returns the CEL place of a set entry's valueExpression.
func (s Side) valuePlace() expr.PlaceID {
	if s == Response {
		return expr.PlaceTransformResponseValue
	}
	return expr.PlaceTransformRequestValue
}

// Target is what a set or remove entry edits.
type Target uint8

// Targets; the zero Target is one the side does not know.
const (
	// TargetHeader is a header field.
	TargetHeader Target = iota + 1
	// TargetQuery is a query parameter (transform.request only).
	TargetQuery
	// TargetBody is a dot path into the JSON body.
	TargetBody
	// TargetPath is the forwarded path (transform.request set only).
	TargetPath
)

// String returns the config spelling of the target.
func (t Target) String() string {
	switch t {
	case TargetHeader:
		return string(v1alpha1.TransformRequestTargetHeader)
	case TargetQuery:
		return string(v1alpha1.TransformRequestTargetQuery)
	case TargetBody:
		return string(v1alpha1.TransformRequestTargetBody)
	case TargetPath:
		return string(v1alpha1.TransformRequestTargetPath)
	default:
		return "target " + strconv.Itoa(int(t))
	}
}

// targetOf returns the Target of a config spelling on side: header, query,
// body and path for Request, header and body for Response, 0 for anything
// else.
func targetOf(side Side, s string) Target {
	switch s {
	case string(v1alpha1.TransformRequestTargetHeader):
		return TargetHeader
	case string(v1alpha1.TransformRequestTargetBody):
		return TargetBody
	case string(v1alpha1.TransformRequestTargetQuery):
		if side == Request {
			return TargetQuery
		}
	case string(v1alpha1.TransformRequestTargetPath):
		if side == Request {
			return TargetPath
		}
	}
	return 0
}

// Spec is a transform config with the entry types of transform.request and
// transform.response unified, so one compiler and one check serve both.
type Spec struct {
	// Side is the Policy type.
	Side Side
	// Body is config.body, "" when absent.
	Body string
	// ContentType is config.contentType, nil when absent: DefaultContentType.
	ContentType *string
	// Set, Remove, ArrayOps and Replace are the config lists in order.
	Set      []SetEntry
	Remove   []RemoveEntry
	ArrayOps []v1alpha1.ArrayOp
	Replace  []v1alpha1.Replace
}

// SetEntry is one config.set entry.
type SetEntry struct {
	// Target is the parsed target, 0 for one the side does not know;
	// RawTarget is its spelling.
	Target    Target
	RawTarget string
	// Name is the header or query name or the body dot path, "" for target
	// path.
	Name string
	// ValueExpression is the CEL source.
	ValueExpression string
}

// RemoveEntry is one config.remove entry.
type RemoveEntry struct {
	// Target is the parsed target, 0 for one the side does not know;
	// RawTarget is its spelling.
	Target    Target
	RawTarget string
	// Name is the header or query name or the body dot path.
	Name string
}

// RequestSpec returns the Spec of a transform.request config; nil is an
// empty config.
func RequestSpec(c *v1alpha1.TransformRequestConfig) Spec {
	s := Spec{Side: Request}
	if c == nil {
		return s
	}
	s.Body, s.ContentType = c.Body, c.ContentType
	for _, e := range c.Set {
		s.Set = append(s.Set, SetEntry{Target: targetOf(Request, string(e.Target)), RawTarget: string(e.Target), Name: e.Name, ValueExpression: e.ValueExpression})
	}
	for _, e := range c.Remove {
		s.Remove = append(s.Remove, RemoveEntry{Target: targetOf(Request, string(e.Target)), RawTarget: string(e.Target), Name: e.Name})
	}
	s.ArrayOps, s.Replace = c.ArrayOps, c.Replace
	return s
}

// ResponseSpec returns the Spec of a transform.response config; nil is an
// empty config.
func ResponseSpec(c *v1alpha1.TransformResponseConfig) Spec {
	s := Spec{Side: Response}
	if c == nil {
		return s
	}
	s.Body, s.ContentType = c.Body, c.ContentType
	for _, e := range c.Set {
		s.Set = append(s.Set, SetEntry{Target: targetOf(Response, string(e.Target)), RawTarget: string(e.Target), Name: e.Name, ValueExpression: e.ValueExpression})
	}
	for _, e := range c.Remove {
		s.Remove = append(s.Remove, RemoveEntry{Target: targetOf(Response, string(e.Target)), RawTarget: string(e.Target), Name: e.Name})
	}
	s.ArrayOps, s.Replace = c.ArrayOps, c.Replace
	return s
}

// contentType returns the effective contentType.
func (s *Spec) contentType() string {
	if s.ContentType == nil {
		return DefaultContentType
	}
	return *s.ContentType
}

// entries returns the number of entries across the four lists.
func (s *Spec) entries() int {
	return len(s.Set) + len(s.Remove) + len(s.ArrayOps) + len(s.Replace)
}

// rewritesBody reports whether the config may rewrite the body: it sets
// body or holds an arrayOps or replace entry, or a set or remove entry with
// target body.
func (s *Spec) rewritesBody() bool {
	if s.Body != "" || len(s.ArrayOps) > 0 || len(s.Replace) > 0 {
		return true
	}
	for _, e := range s.Set {
		if e.Target == TargetBody {
			return true
		}
	}
	for _, e := range s.Remove {
		if e.Target == TargetBody {
			return true
		}
	}
	return false
}

// literal reports whether a replace entry is literal.
func literal(r v1alpha1.Replace) bool { return r.Literal != nil && *r.Literal }
