// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// retryableMethod reports the methods the default retryOn retries on reset
// errors and status 503: GET, HEAD, OPTIONS, PUT and DELETE (05 req 4).
// Methods compare as received, case-sensitively, like CEL's "in".
func retryableMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "OPTIONS", "PUT", "DELETE":
		return true
	default:
		return false
	}
}

// DefaultRetryOn is the native form of expr.DefaultRetryOn (05 req 4): for
// GET, HEAD, OPTIONS, PUT and DELETE, a connect or reset error or status
// 503; for other methods, a connect error. kind is KindNone when the
// attempt got a response with status. Its tests check it against the
// prose rule of req 4 and pin the CEL source; the equality with the
// compiled default through the CEL engine is a test of WP-34 or WP-46 (see
// RetryConfig.RetryOn).
func DefaultRetryOn(method string, kind Kind, status int) bool {
	if kind != KindNone {
		return kind == KindConnect || (kind == KindReset && retryableMethod(method))
	}
	return status == 503 && retryableMethod(method)
}

// DefaultFailureWhen is the native form of expr.DefaultFailureWhen (05 req
// 4): any error, or status 502, 503 or 504.
func DefaultFailureWhen(kind Kind, status int) bool {
	return kind != KindNone || status == 502 || status == 503 || status == 504
}

// EvalRetryOn evaluates retryOn over the leg's Vars (request without body,
// response or error, attempt, upstream; 05 req 32). p nil runs the native
// default (DefaultRetryOn's rule over v). A runtime error means no retry
// (expr.RuleNoRetry): it returns false and the error, which the caller
// counts in ruralz_upstream_cel_errors_total{field="retryOn"}.
func EvalRetryOn(ctx context.Context, p expr.Program, v *expr.Vars) (bool, error) {
	if p == nil {
		return defaultRetryOnVars(v)
	}
	ok, err := p.EvalBool(ctx, v)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// EvalFailureWhen evaluates failureWhen over the leg's Vars (request,
// response or error, upstream; 05 req 32). p nil runs the native default
// (DefaultFailureWhen's rule over v). A runtime error counts as a failure
// (expr.RuleCountFailure): it returns true and the error, which the caller
// counts in ruralz_upstream_cel_errors_total{field="failureWhen"}.
func EvalFailureWhen(ctx context.Context, p expr.Program, v *expr.Vars) (bool, error) {
	if p == nil {
		return defaultFailureWhenVars(v)
	}
	ok, err := p.EvalBool(ctx, v)
	if err != nil {
		return true, err
	}
	return ok, nil
}

// tri is a CEL boolean that may be an error, for the native defaults'
// null handling.
type tri uint8

const (
	triFalse tri = iota
	triTrue
	triErr
)

// and is CEL's commutative &&: false wins over an error.
func (a tri) and(b tri) tri {
	switch {
	case a == triFalse || b == triFalse:
		return triFalse
	case a == triErr || b == triErr:
		return triErr
	default:
		return triTrue
	}
}

// or is CEL's commutative ||: true wins over an error.
func (a tri) or(b tri) tri {
	switch {
	case a == triTrue || b == triTrue:
		return triTrue
	case a == triErr || b == triErr:
		return triErr
	default:
		return triFalse
	}
}

// triOf converts a Go bool.
func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

// result converts a tri to the (bool, error) pair of a CEL evaluation at
// place; an error evaluates to onErr.
func (a tri) result(place expr.PlaceID, onErr bool) (bool, error) {
	if a == triErr {
		return onErr, expr.NewEvalError(place, expr.KindNull, "select on a null variable")
	}
	return a == triTrue, nil
}

// methodIn is `request.method in [GET, HEAD, OPTIONS, PUT, DELETE]`; a null
// request is a runtime error.
func methodIn(v *expr.Vars) tri {
	if v.Request == nil {
		return triErr
	}
	return triOf(retryableMethod(v.Request.Method))
}

// defaultRetryOnVars evaluates expr.DefaultRetryOn natively, with CEL's
// null and error semantics:
//
//	error != null
//	  ? (error.kind == "connect" || (error.kind == "reset" && request.method in [...]))
//	  : (response.status == 503 && request.method in [...])
func defaultRetryOnVars(v *expr.Vars) (bool, error) {
	if v == nil {
		v = &expr.Vars{}
	}
	var r tri
	if v.Error != nil {
		r = triOf(v.Error.Kind == "connect").or(triOf(v.Error.Kind == "reset").and(methodIn(v)))
	} else {
		status := triErr
		if v.Response != nil {
			status = triOf(v.Response.Status == 503)
		}
		r = status.and(methodIn(v))
	}
	return r.result(expr.PlaceRetryOn, false)
}

// defaultFailureWhenVars evaluates expr.DefaultFailureWhen natively:
//
//	error != null || response.status in [502, 503, 504]
func defaultFailureWhenVars(v *expr.Vars) (bool, error) {
	if v == nil {
		v = &expr.Vars{}
	}
	status := triErr
	if v.Response != nil {
		s := v.Response.Status
		status = triOf(s == 502 || s == 503 || s == 504)
	}
	return triOf(v.Error != nil).or(status).result(expr.PlaceFailureWhen, true)
}
