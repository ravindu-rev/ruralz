// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"net/netip"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"
)

// net.prefix(string, int, int) returns the network of an IP address in
// CIDR form (docs/architecture/02-configuration-model.md "Limits"): an
// IPv4 address, an IPv4-mapped one included, masked to the first length,
// an IPv6 address to the second, so net.prefix(source.ip, 32, 64) gives an
// IPv4 client its own /32 and an IPv6 client its /64. An address that does
// not parse, or a length outside 0 to 32 or 0 to 128, is a runtime error.
// A call costs 1 plus 0.1 unit per input byte, and the estimator sizes its
// result at its input plus 4 bytes, the longest "/len" suffix.
const (
	// netPrefixName is the function's name in expressions.
	netPrefixName = "net.prefix"
	// netPrefixOverload is its one overload.
	netPrefixOverload = "net_prefix_string_int_int"
	// netPrefixSuffix is the most bytes the result adds to its input: a
	// "/" and a length of up to three digits.
	netPrefixSuffix = 4
)

// netLibrary declares net.prefix with its cost model, like urlLibrary.
type netLibrary struct{}

var _ celgo.Library = netLibrary{}

// CompileOptions declares the function and its cost estimate.
func (netLibrary) CompileOptions() []celgo.EnvOption {
	return []celgo.EnvOption{
		celgo.Function(netPrefixName,
			celgo.Overload(netPrefixOverload,
				[]*celgo.Type{celgo.StringType, celgo.IntType, celgo.IntType}, celgo.StringType,
				celgo.FunctionBinding(netPrefix))),
		celgo.CostEstimatorOptions(checker.OverloadCostEstimate(netPrefixOverload, estimateNetPrefix)),
	}
}

// ProgramOptions charges the function's runtime cost; it applies only to
// programs that track cost.
func (netLibrary) ProgramOptions() []celgo.ProgramOption {
	return []celgo.ProgramOption{
		celgo.CostTrackerOptions(interpreter.OverloadCostTracker(netPrefixOverload, trackNetPrefix)),
	}
}

// netPrefix is the binding of net.prefix. Its errors name the function and
// the rule broken, never the argument, which may hold request data.
func netPrefix(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("net.prefix: want 3 arguments")
	}
	s, ok1 := args[0].(types.String)
	v4, ok2 := args[1].(types.Int)
	v6, ok3 := args[2].(types.Int)
	if !ok1 || !ok2 || !ok3 {
		return types.NewErr("net.prefix: no such overload")
	}
	addr, err := netip.ParseAddr(string(s))
	if err != nil {
		return types.NewErr("net.prefix: cannot parse the address")
	}
	addr = addr.Unmap()
	bits, limit := int64(v6), int64(128)
	if addr.Is4() {
		bits, limit = int64(v4), 32
	}
	if bits < 0 || bits > limit {
		return types.NewErr("net.prefix: prefix length out of range")
	}
	p, err := addr.Prefix(int(bits))
	if err != nil {
		return types.NewErr("net.prefix: prefix length out of range")
	}
	return types.String(p.String())
}

// estimateNetPrefix is the static estimate of a net.prefix call: 1 plus
// 0.1 unit per byte of its address argument, the result sized at the
// address plus netPrefixSuffix bytes. An address without a size is
// unbounded.
func estimateNetPrefix(e checker.CostEstimator, _ *checker.AstNode, args []checker.AstNode) *checker.CallEstimate {
	if len(args) != 3 {
		return nil
	}
	size := checker.SizeEstimate{Min: 0, Max: ^uint64(0)}
	if sz := args[0].ComputedSize(); sz != nil {
		size = *sz
	} else if sz := e.EstimateSize(args[0]); sz != nil {
		size = *sz
	}
	call := size.MultiplyByCostFactor(common.StringTraversalCostFactor).Add(checker.FixedCostEstimate(1))
	result := checker.SizeEstimate{Min: size.Min, Max: addSat(size.Max, netPrefixSuffix)}
	return &checker.CallEstimate{CostEstimate: call, ResultSize: &result}
}

// trackNetPrefix is the runtime cost of one net.prefix call, as
// estimateNetPrefix charges it: 1 plus 0.1 unit per byte of the address,
// rounded up.
func trackNetPrefix(args []ref.Val, _ ref.Val) *uint64 {
	var n uint64
	if len(args) > 0 {
		if s, ok := args[0].(types.String); ok {
			n = uint64(len(s))
		}
	}
	c := cost.SafeAdd(1, cost.SafeMultiplyByFactor(n, common.StringTraversalCostFactor))
	return &c
}
