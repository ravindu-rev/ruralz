// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// The Ruralz functions url.pathEscape and url.queryEscape
// (docs/architecture/02-configuration-model.md "Limits"), available in
// every CEL place. As request.pathParams holds decoded captures, a
// composition pathExpression escapes what it splices with them
// (docs/architecture/03-data-plane.md "Composition engine").
const (
	funcPathEscape  = "url.pathEscape"
	funcQueryEscape = "url.queryEscape"
	// The overload IDs, which the cost estimates and trackers key on.
	overloadPathEscape  = "url_path_escape_string"
	overloadQueryEscape = "url_query_escape_string"
	// escapeGrowth sizes an escape's result: every input byte becomes at
	// most three ("%" and two hexadecimal digits).
	escapeGrowth = 3
)

// upperHex holds the hexadecimal digits a percent-encoding writes, upper
// case (RFC 3986 section 2.1).
const upperHex = "0123456789ABCDEF"

// urlLibrary declares the url functions with their cost model: a call
// costs 1 plus 0.1 unit per input byte, cel-go's string traversal factor,
// and the estimator sizes its result at three times its input.
type urlLibrary struct{}

var _ celgo.Library = urlLibrary{}

// CompileOptions declares the functions and their cost estimates.
func (urlLibrary) CompileOptions() []celgo.EnvOption {
	return []celgo.EnvOption{
		celgo.Function(funcPathEscape,
			celgo.Overload(overloadPathEscape, []*celgo.Type{celgo.StringType}, celgo.StringType,
				celgo.UnaryBinding(escapeBinding(keepInPathSegment)))),
		celgo.Function(funcQueryEscape,
			celgo.Overload(overloadQueryEscape, []*celgo.Type{celgo.StringType}, celgo.StringType,
				celgo.UnaryBinding(escapeBinding(isUnreserved)))),
		celgo.CostEstimatorOptions(
			cost.OverloadCostEstimate(overloadPathEscape, estimateEscape),
			cost.OverloadCostEstimate(overloadQueryEscape, estimateEscape),
		),
	}
}

// ProgramOptions charges the functions' runtime cost; it applies only to
// programs that track cost.
func (urlLibrary) ProgramOptions() []celgo.ProgramOption {
	return []celgo.ProgramOption{
		celgo.CostTrackerOptions(
			cost.OverloadTracker(overloadPathEscape, trackEscape),
			cost.OverloadTracker(overloadQueryEscape, trackEscape),
		),
	}
}

// escapeBinding returns the binding that percent-encodes every byte of a
// string keep refuses.
func escapeBinding(keep func(byte) bool) func(ref.Val) ref.Val {
	return func(v ref.Val) ref.Val {
		s, ok := v.(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(v)
		}
		return types.String(percentEncode(string(s), keep))
	}
}

// percentEncode encodes every byte of s that keep refuses as "%" and two
// upper-case hexadecimal digits; it returns s itself when nothing needs
// encoding.
func percentEncode(s string, keep func(byte) bool) string {
	n := 0
	for i := range len(s) {
		if !keep(s[i]) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	b := make([]byte, 0, len(s)+2*n)
	for i := range len(s) {
		c := s[i]
		if keep(c) {
			b = append(b, c)
			continue
		}
		b = append(b, '%', upperHex[c>>4], upperHex[c&0x0F])
	}
	return string(b)
}

// isUnreserved reports an RFC 3986 unreserved character: ALPHA, DIGIT,
// "-", ".", "_" and "~". url.queryEscape keeps only these, so its result
// is one query name or value, with "&", "=", "+" and the space escaped.
func isUnreserved(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	default:
		return c == '-' || c == '.' || c == '_' || c == '~'
	}
}

// keepInPathSegment reports a byte url.pathEscape keeps: an unreserved
// character, a sub-delimiter ("!", "$", "&", "'", "(", ")", "*", "+", ",",
// ";", "="), ":" or "@" (RFC 3986 pchar), so its result is one path
// segment, with "/", "?", "#" and "%" escaped.
func keepInPathSegment(c byte) bool {
	if isUnreserved(c) {
		return true
	}
	switch c {
	case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@':
		return true
	default:
		return false
	}
}

// estimateEscape is the static estimate of a url function call: 1 plus
// the string traversal of its input, with the result sized at three times
// the input. An input without a size is unbounded.
func estimateEscape(e cost.Estimator, _ *cost.AstNode, args []cost.AstNode) *cost.CallEstimate {
	if len(args) != 1 {
		return nil
	}
	size := cost.EstimateSize(e, args[0])
	c := size.MultiplyByCostFactor(cost.StringTraversalCostFactor).Add(cost.FixedCostEstimate(1))
	result := cost.RangedSizeEstimate(size.Min, cost.SafeMultiply(size.Max, escapeGrowth))
	return cost.NewCallEstimate(c, &result)
}

// trackEscape is the runtime cost of a url function call: 1 plus 0.1 unit
// per input byte, rounded up.
func trackEscape(args []ref.Val, _ ref.Val) *uint64 {
	n := uint64(1)
	if len(args) == 1 {
		if s, ok := args[0].(types.String); ok {
			n = cost.SafeAdd(n, cost.SafeMultiplyByFactor(uint64(len(s)), cost.StringTraversalCostFactor))
		}
	}
	return &n
}
