// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"net/http"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// The fields below take their values from a fixed vocabulary. Boxing a
// string into a ref.Val allocates unless the string is a constant, so each
// function returns the constant of a known value and converts any other
// value as it is: request.method and request.scheme are read on most
// requests, and the rest by retry, breaker and access-log rules.

// methodVal is request.method: the RFC 9110 methods and PATCH as
// constants.
func methodVal(m string) ref.Val {
	switch m {
	case http.MethodGet:
		return types.String(http.MethodGet)
	case http.MethodHead:
		return types.String(http.MethodHead)
	case http.MethodPost:
		return types.String(http.MethodPost)
	case http.MethodPut:
		return types.String(http.MethodPut)
	case http.MethodPatch:
		return types.String(http.MethodPatch)
	case http.MethodDelete:
		return types.String(http.MethodDelete)
	case http.MethodConnect:
		return types.String(http.MethodConnect)
	case http.MethodOptions:
		return types.String(http.MethodOptions)
	case http.MethodTrace:
		return types.String(http.MethodTrace)
	default:
		return types.String(m)
	}
}

// schemeVal is request.scheme: "http" or "https".
func schemeVal(s string) ref.Val {
	switch s {
	case "https":
		return types.String("https")
	case "http":
		return types.String("http")
	default:
		return types.String(s)
	}
}

// authMethodVal is auth.method: "jwt", "api-key", "basic" or "mtls"
// (filter.Method*).
func authMethodVal(m string) ref.Val {
	switch m {
	case authMethodJWT:
		return types.String(authMethodJWT)
	case "api-key":
		return types.String("api-key")
	case "basic":
		return types.String("basic")
	case "mtls":
		return types.String("mtls")
	default:
		return types.String(m)
	}
}

// authMethodJWT is the auth.method whose claims are a JWT payload; every
// other method's claims are the empty map (expr.Auth).
const authMethodJWT = "jwt"

// errorKindVal is error.kind: "connect", "timeout", "reset" or "tls".
func errorKindVal(k string) ref.Val {
	switch k {
	case "connect":
		return types.String("connect")
	case "timeout":
		return types.String("timeout")
	case "reset":
		return types.String("reset")
	case "tls":
		return types.String("tls")
	default:
		return types.String(k)
	}
}

// statusVal is response.status and steps.<name>.status. Boxing an int of
// 256 or more allocates unless it is a constant, so the statuses rules
// test most often are constants: those of the default retryOn and
// failureWhen and the common success, redirect and error statuses.
func statusVal(s int) ref.Val {
	switch s {
	case http.StatusOK:
		return types.Int(http.StatusOK)
	case http.StatusCreated:
		return types.Int(http.StatusCreated)
	case http.StatusAccepted:
		return types.Int(http.StatusAccepted)
	case http.StatusNoContent:
		return types.Int(http.StatusNoContent)
	case http.StatusMovedPermanently:
		return types.Int(http.StatusMovedPermanently)
	case http.StatusFound:
		return types.Int(http.StatusFound)
	case http.StatusNotModified:
		return types.Int(http.StatusNotModified)
	case http.StatusBadRequest:
		return types.Int(http.StatusBadRequest)
	case http.StatusUnauthorized:
		return types.Int(http.StatusUnauthorized)
	case http.StatusForbidden:
		return types.Int(http.StatusForbidden)
	case http.StatusNotFound:
		return types.Int(http.StatusNotFound)
	case http.StatusConflict:
		return types.Int(http.StatusConflict)
	case http.StatusTooManyRequests:
		return types.Int(http.StatusTooManyRequests)
	case http.StatusInternalServerError:
		return types.Int(http.StatusInternalServerError)
	case http.StatusBadGateway:
		return types.Int(http.StatusBadGateway)
	case http.StatusServiceUnavailable:
		return types.Int(http.StatusServiceUnavailable)
	case http.StatusGatewayTimeout:
		return types.Int(http.StatusGatewayTimeout)
	default:
		return types.Int(s)
	}
}
