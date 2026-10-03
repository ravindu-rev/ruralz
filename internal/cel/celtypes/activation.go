// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/interpreter"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Activation is the cel-go activation of one evaluation point: the
// variables of an expr.Vars (03 req 17, 39). It has the same layout as
// expr.Vars, so NewActivation converts a pointer without allocating, and
// resolving a variable wraps the view pointer without copying or
// converting any field: an expression pays only for what it selects.
// A nil view pointer is CEL null (03 req 18); a nil Steps is an empty map.
type Activation expr.Vars

// Compile-time check of the cel-go interface.
var _ interpreter.Activation = (*Activation)(nil)

// NewActivation returns the activation over v. Evaluation reads v and
// never writes it; v must not change while an evaluation runs.
func NewActivation(v *expr.Vars) *Activation { return (*Activation)(v) }

// ResolveName returns the CEL value of a variable.
func (a *Activation) ResolveName(name string) (any, bool) {
	switch name {
	case varRequest:
		return viewOf(a.Request), true
	case varSource:
		return viewOf(a.Source), true
	case varRoute:
		return viewOf(a.Route), true
	case varConsumer:
		return viewOf(a.Consumer), true
	case varAuth:
		return viewOf(a.Auth), true
	case varNow:
		return types.Timestamp{Time: a.Now.UTC()}, true
	case varResponse:
		return viewOf(a.Response), true
	case varError:
		return viewOf(a.Error), true
	case varUpstream:
		return viewOf(a.Upstream), true
	case varAttempt:
		return types.Int(a.Attempt), true
	case varSteps:
		return mapView[stepsSource]{stepsSource{a.Steps}}, true
	case varDuration:
		return types.Duration{Duration: a.Duration}, true
	case varAI:
		return viewOf(a.AI), true
	default:
		return nil, false
	}
}

// Parent returns nil: an Activation has no parent.
func (a *Activation) Parent() interpreter.Activation { return nil }
