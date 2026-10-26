// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"math"
	"strconv"

	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/types"
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

// unsizedEstimator gives no size, so cel-go treats every input as
// unbounded: an estimate within the runtime limit then holds for any
// input, and the program needs no cost tracking.
type unsizedEstimator struct{}

// EstimateSize implements checker.CostEstimator.
func (unsizedEstimator) EstimateSize(checker.AstNode) *checker.SizeEstimate { return nil }

// EstimateCallCost implements checker.CostEstimator.
func (unsizedEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
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
