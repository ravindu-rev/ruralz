// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"math"
	"strconv"

	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/types"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Cost limits of docs/architecture/02-configuration-model.md "Limits" and
// ADR-0019 (all targets).
const (
	// staticCostCeiling is the largest static estimate at nominal sizes
	// that passes validation; above it is RZ-CFG-015.
	staticCostCeiling = 10_000
	// runtimeCostLimit stops a tracked evaluation with a runtime error.
	runtimeCostLimit = 1_000_000
	// nominalStringSize sizes every string reached from a variable.
	nominalStringSize = 256
	// nominalContainerSize sizes every list or map reached from a variable.
	nominalContainerSize = 32
)

// nominalEstimator sizes the values cel-go cannot size itself at the
// nominal sizes: a string at 256 bytes and a list or map at 32 entries,
// whether typed or dyn. A dyn value is sized as a container where the
// expression iterates it or tests membership in it, else as a string. An
// object, type or null value has size 1, as cel-go's runtime cost charges
// a value that has no size, so comparing two of them is not unbounded.
type nominalEstimator struct {
	// containers are the IDs of comprehension ranges and in containers.
	containers map[int64]bool
}

// EstimateSize implements checker.CostEstimator.
func (e nominalEstimator) EstimateSize(n checker.AstNode) *checker.SizeEstimate {
	t := n.Type()
	if t == nil {
		return nil
	}
	switch t.Kind() {
	case types.StringKind, types.BytesKind:
		return &checker.SizeEstimate{Min: 0, Max: nominalStringSize}
	case types.ListKind, types.MapKind:
		return &checker.SizeEstimate{Min: 0, Max: nominalContainerSize}
	case types.DynKind, types.AnyKind, types.TypeParamKind:
		if e.containers[n.Expr().ID()] {
			return &checker.SizeEstimate{Min: 0, Max: nominalContainerSize}
		}
		return &checker.SizeEstimate{Min: 0, Max: nominalStringSize}
	case types.StructKind, types.TypeKind, types.NullTypeKind:
		return &checker.SizeEstimate{Min: 1, Max: 1}
	default:
		// Scalars: cel-go sizes them itself.
		return nil
	}
}

// EstimateCallCost implements checker.CostEstimator: no function needs a
// cost beyond those of cel-go and the enabled extensions.
func (nominalEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// cappedEstimator decides runtime tracking: it sizes the inputs a snapshot
// caps (expr.InputCaps) at their caps and gives no size for any other
// value, which cel-go then treats as unbounded. An estimate within the
// runtime limit therefore holds for every input within the caps, and the
// program needs no cost tracking. Only a value reached from a capped
// variable field is sized (costFacts.paths): a value an expression
// computes, such as a split element or a field of a call result, gets no
// size unless cel-go derives one from sized operands, so no cap is ever
// applied to data it does not bound. With the zero caps every input is
// unbounded.
type cappedEstimator struct {
	caps expr.InputCaps
	// clientRequest reports that request is the client request, whose
	// header block the caps bound (siteEnv.clientRequest).
	clientRequest bool
	// paths are the expression's values reached from a Ruralz variable.
	paths map[int64][]string
}

// EstimateSize implements checker.CostEstimator. Objects, types and null
// have size 1, as cel-go's runtime cost charges a value without a size.
func (e cappedEstimator) EstimateSize(n checker.AstNode) *checker.SizeEstimate {
	if t := n.Type(); t != nil {
		switch t.Kind() {
		case types.StructKind, types.TypeKind, types.NullTypeKind:
			return &checker.SizeEstimate{Min: 1, Max: 1}
		default:
		}
	}
	path, rooted := e.paths[n.Expr().ID()]
	if !rooted {
		return nil
	}
	limit, ok := e.capOf(path)
	if !ok {
		return nil
	}
	return &checker.SizeEstimate{Min: 0, Max: limit}
}

// capOf returns the cap of the value at path, a field path from a
// variable (checker.AstNode.Path), in bytes for a string and entries for a
// list or map; false when no cap bounds it.
func (e cappedEstimator) capOf(path []string) (uint64, bool) {
	if len(path) < 2 {
		return 0, false
	}
	c := e.caps
	switch path[0] {
	case "request":
		switch path[1] {
		case "method", "scheme", "host", "path", "pathParams", "query", "headers":
			if !e.clientRequest || c.RequestHeaderBytes <= 0 || c.AddedHeaderBytes < 0 {
				return 0, false
			}
			return uint64(c.RequestHeaderBytes) + uint64(c.AddedHeaderBytes), true
		default:
			return 0, false
		}
	case "auth":
		if path[1] != "claims" || c.ClaimsBytes <= 0 {
			return 0, false
		}
		return uint64(c.ClaimsBytes), true
	case "route":
		return routeCap(c.Routes, path[1], len(path) > 2)
	case "consumer":
		return consumerCap(c.Consumers, path[1], len(path) > 2)
	default:
		return 0, false
	}
}

// routeCap returns the cap of route.<field>, or of its keys and values when
// element is set.
func routeCap(c expr.RouteCaps, field string, element bool) (uint64, bool) {
	if !c.Known {
		return 0, false
	}
	var n int64
	switch {
	case field == "name" && !element:
		n = c.NameBytes
	case field == "labels":
		n = pick(element, c.LabelBytes, c.Labels)
	default:
		return 0, false
	}
	if n < 0 {
		return 0, false
	}
	return uint64(n), true
}

// consumerCap returns the cap of consumer.<field>, or of its elements,
// keys and values when element is set.
func consumerCap(c expr.ConsumerCaps, field string, element bool) (uint64, bool) {
	if !c.Known {
		return 0, false
	}
	var n int64
	switch {
	case field == "name" && !element:
		n = c.NameBytes
	case field == "tier" && !element:
		n = c.TierBytes
	case field == "tags":
		n = pick(element, c.TagBytes, c.Tags)
	case field == "labels":
		n = pick(element, c.LabelBytes, c.Labels)
	case field == "quotas":
		n = pick(element, c.QuotaNameBytes, c.Quotas)
	default:
		return 0, false
	}
	if n < 0 {
		return 0, false
	}
	return uint64(n), true
}

// pick returns elem when element is set, else whole.
func pick(element bool, elem, whole int64) int64 {
	if element {
		return elem
	}
	return whole
}

// EstimateCallCost implements checker.CostEstimator: cel-go's and the
// extensions' own estimates apply.
func (cappedEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// costFinding returns the RZ-CFG-015 message and hint for an estimate,
// or ok false when the estimate passes. an supplies the hint: json.encode,
// which cel-go cannot bound, or a comprehension.
func costFinding(est checker.CostEstimate, an *analysis) (msg, hint string, ok bool) {
	if est.Max <= staticCostCeiling {
		return "", "", false
	}
	sizes := " at nominal sizes (strings " + strconv.Itoa(nominalStringSize) +
		", lists and maps " + strconv.Itoa(nominalContainerSize) + ")"
	if est.Max == math.MaxUint64 {
		msg = "estimated cost is unbounded" + sizes
	} else {
		msg = "estimated cost " + strconv.FormatUint(est.Max, 10) + " exceeds " +
			strconv.Itoa(staticCostCeiling) + sizes
	}
	switch {
	case an.jsonEncode:
		hint = "json.encode has no cost bound; a transform body may return the map or list itself"
	case an.folds:
		hint = "nested comprehensions multiply: iterate over fewer or smaller values"
	}
	return msg, hint, true
}
