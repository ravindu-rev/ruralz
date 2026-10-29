// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// siteEnv is the environment of one site: the variables it declares and the
// bodies it may select (docs/architecture/02-configuration-model.md
// "Allowed places" and "Variables": a body is read in body Phases and
// composition only).
type siteEnv struct {
	// place is the site's place.
	place expr.Place
	// vars are the declared variables.
	vars expr.Var
	// reqBody and respBody report whether request.body and response.body
	// may be selected.
	reqBody, respBody bool
	// composition is set for the composition places, with the mode and the
	// earlier steps in list order.
	composition bool
	mode        v1alpha1.CompositionMode
	earlier     []string
	// clientRequest reports that request is the client request, whose
	// header block expr.InputCaps bounds. In an upstream leg, at Upstream
	// scope and in the Upstream fields, it is the attempt's outgoing
	// request, which upstream-scope Policies and upstream authentication
	// extend beyond those caps.
	clientRequest bool
}

// resolve returns the environment of site; false for an unknown place.
func resolve(site expr.Site) (siteEnv, bool) {
	p, ok := expr.LookupPlace(site.Place)
	if !ok {
		return siteEnv{}, false
	}
	se := siteEnv{place: p, vars: p.Vars, clientRequest: clientRequest(site)}
	switch p.ID {
	case expr.PlacePolicyWhen:
		// response exists only when the Policy's first Phase at this
		// attachment is a response Phase; the Policy reads a body only when
		// it gates that body itself.
		se.vars = expr.VarsBase
		if site.FirstPhase.IsResponse() {
			se.vars |= expr.VarResponse
			se.respBody = site.PolicyType == v1alpha1.PolicyTypeTransformResponse
		}
		se.reqBody = site.GatesBody
	case expr.PlaceStepPathExpression, expr.PlaceStepWhen:
		se.reqBody = true
		se.composition = true
		se.mode = site.Mode
		se.earlier = site.EarlierSteps
	case expr.PlaceAuthzCELRule:
		// A rule that selects request.body moves the Policy to
		// onRequestBody.
		se.reqBody = true
	case expr.PlaceTransformRequestBody, expr.PlaceTransformRequestValue:
		se.vars = expr.VarsBase
		if site.Scope == phase.ScopeUpstream {
			se.vars |= expr.VarUpstream
		}
		se.reqBody = true
	case expr.PlaceTransformResponseBody, expr.PlaceTransformResponseValue:
		se.vars = expr.VarsBase | expr.VarResponse
		if site.Scope == phase.ScopeUpstream {
			se.vars |= expr.VarUpstream
		}
		se.respBody = true
	default:
		// Every other place declares its whole union and reads no body.
	}
	return se, true
}

// clientRequest reports whether request at site is the client request:
// everywhere but at Upstream scope, in a Policy.spec.when first evaluated
// in an upstream-leg Phase, and in the Upstream fields, which are
// evaluated per attempt over the outgoing request.
func clientRequest(site expr.Site) bool {
	if site.Scope == phase.ScopeUpstream {
		return false
	}
	if site.Place == expr.PlacePolicyWhen && site.FirstPhase.UpstreamLeg() && site.FirstPhase != phase.OnChunk {
		return false
	}
	switch site.Place {
	case expr.PlaceHashKey, expr.PlaceRetryOn, expr.PlaceFailureWhen, expr.PlaceMessagingKey:
		return false
	default:
		return true
	}
}

// key returns the site part of a program's identity: two sites with equal
// keys accept the same sources and compile them to the same program.
func (se siteEnv) key() siteKey {
	k := siteKey{
		place: se.place.ID, vars: se.vars, reqBody: se.reqBody, respBody: se.respBody,
		clientRequest: se.clientRequest,
	}
	if se.composition {
		k.steps = string(se.mode) + "\x00" + strings.Join(se.earlier, "\x00")
	}
	return k
}

// siteKey identifies an environment, the site checks that apply in it and
// the request the input caps can bound.
type siteKey struct {
	place             expr.PlaceID
	vars              expr.Var
	reqBody, respBody bool
	clientRequest     bool
	// steps holds the composition mode and the earlier step names.
	steps string
}
