// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"errors"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/ravindu-rev/ruralz/internal/phase"
)

// hostFunc is one Host Function of Plugin ABI v1, or one WASI shim import:
// its signature, the one Capability it needs, the Phases it is valid in
// and whether rz_configure may call it
// (docs/architecture/05-wasm-plugin-system.md "Host Function table").
type hostFunc struct {
	name   string
	params []api.ValueType
	// capability is the Capability the import requests; capNone for a WASI
	// import that needs none.
	capability capability
	// phases are the Phases a Phase call may call it in.
	phases phase.Set
	// configure allows the call from rz_configure: log, clock, random and
	// crypto.
	configure bool
	// fn runs the function on the call's stack and returns its i32 result.
	fn func(c *call, m api.Memory, stack []uint64) int32
	// void marks a function without a result (proc_exit).
	void bool
}

const (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

// sig returns a parameter list.
func sig(ts ...api.ValueType) []api.ValueType { return ts }

// Phase sets of the Host Function table.
func allPhases() phase.Set {
	var s phase.Set
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		s = s.Add(p)
	}
	return s
}

// beforeCommit are the Phases before the response is committed: every
// request and response Phase through onResponse.
func beforeCommit() phase.Set {
	return phase.Of(phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute, phase.OnUpstreamRequest,
		phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody, phase.OnResponse)
}

// abiFuncs returns the Host Functions of the ruralz.plugin.v1 module, in
// the table's order. Each needs exactly one Capability; nothing is
// ambient.
func abiFuncs() []hostFunc {
	all := allPhases()
	reqWrite := phase.Of(phase.OnRequestHeaders, phase.OnRequestBody, phase.OnUpstreamRequest)
	respRead := phase.Of(phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody, phase.OnResponse, phase.OnLog, phase.OnChunk)
	respWrite := phase.Of(phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody, phase.OnResponse)
	send := phase.Of(phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute, phase.OnUpstreamRequest)
	ctxSet := all &^ phase.Of(phase.OnLog)
	stateIncr := beforeCommit().Add(phase.OnLog)
	return []hostFunc{
		{name: "log", params: sig(i32, i32, i32), capability: capLogWrite, phases: all, configure: true, fn: hostLog},
		{name: "clock_now", params: sig(i32), capability: capClockRead, phases: all, configure: true, fn: hostClockNow},
		{name: "random_bytes", params: sig(i32, i32), capability: capRandomRead, phases: all, configure: true, fn: hostRandomBytes},
		{name: "request_info", params: sig(i32, i32, i32), capability: capRequestMetadataRead, phases: all, fn: hostRequestInfo},
		{name: "request_header_get", params: sig(i32, i32, i32, i32), capability: capRequestHeadersRead, phases: all, fn: hostRequestHeaderGet},
		{name: "request_headers_list", params: sig(i32, i32), capability: capRequestHeadersRead, phases: all, fn: hostRequestHeadersList},
		{name: "request_header_set", params: sig(i32, i32, i32, i32), capability: capRequestHeadersWrite, phases: reqWrite, fn: hostRequestHeaderSet},
		{name: "request_header_remove", params: sig(i32, i32), capability: capRequestHeadersWrite, phases: reqWrite, fn: hostRequestHeaderRemove},
		{name: "request_body_read", params: sig(i64, i32, i32), capability: capRequestBodyRead, phases: phase.Of(phase.OnRequestBody), fn: hostRequestBodyRead},
		{name: "request_body_replace", params: sig(i32, i32), capability: capRequestBodyWrite, phases: phase.Of(phase.OnRequestBody), fn: hostRequestBodyReplace},
		{name: "consumer_info", params: sig(i32, i32, i32), capability: capConsumerRead, phases: all, fn: hostConsumerInfo},
		{name: "response_header_get", params: sig(i32, i32, i32, i32), capability: capResponseHeadersRead, phases: respRead, fn: hostResponseHeaderGet},
		{name: "response_header_set", params: sig(i32, i32, i32, i32), capability: capResponseHeadersWrite, phases: respWrite, fn: hostResponseHeaderSet},
		{name: "response_body_read", params: sig(i64, i32, i32), capability: capResponseBodyRead, phases: phase.Of(phase.OnUpstreamResponseBody), fn: hostResponseBodyRead},
		{name: "response_body_replace", params: sig(i32, i32), capability: capResponseBodyWrite, phases: phase.Of(phase.OnUpstreamResponseBody), fn: hostResponseBodyReplace},
		{name: "response_send", params: sig(i32, i32, i32), capability: capResponseSend, phases: send, fn: hostResponseSend},
		{name: "chunk_read", params: sig(i32, i32, i32), capability: capStreamRead, phases: phase.Of(phase.OnChunk), fn: hostChunkRead},
		{name: "chunk_replace", params: sig(i32, i32), capability: capStreamWrite, phases: phase.Of(phase.OnChunk), fn: hostChunkReplace},
		{name: "context_get", params: sig(i32, i32, i32, i32), capability: capContextUse, phases: all, fn: hostContextGet},
		{name: "context_set", params: sig(i32, i32, i32, i32), capability: capContextUse, phases: ctxSet, fn: hostContextSet},
		{name: "state_get", params: sig(i32, i32, i32, i32), capability: capStateRead, phases: beforeCommit(), fn: hostStateGet},
		{name: "state_incr", params: sig(i32, i32, i64, i64, i32), capability: capStateWrite, phases: stateIncr, fn: hostStateIncr},
		{name: "metric_add", params: sig(i32, i32, i64), capability: capMetricsWrite, phases: all, fn: hostMetricAdd},
		{name: "crypto_digest", params: sig(i32, i32, i32, i32, i32), capability: capCryptoUse, phases: all, configure: true, fn: hostCryptoDigest},
		{name: "crypto_hmac", params: sig(i32, i32, i32, i32, i32, i32, i32), capability: capCryptoUse, phases: all, configure: true, fn: hostCryptoHMAC},
		{name: "crypto_key_sign", params: sig(i32, i32, i32, i32, i32), capability: capCryptoUse, phases: all, configure: true, fn: hostCryptoKeySign},
		{name: "crypto_key_verify", params: sig(i32, i32, i32, i32, i32), capability: capCryptoUse, phases: all, configure: true, fn: hostCryptoKeyVerify},
		{name: "crypto_key_hmac", params: sig(i32, i32, i32, i32, i32), capability: capCryptoUse, phases: all, configure: true, fn: hostCryptoKeyHMAC},
	}
}

// lookupImport returns the Host Function an import names, by module and
// name.
func lookupImport(module, name string) (hostFunc, bool) {
	var fs []hostFunc
	switch module {
	case abiModule:
		fs = abiFuncs()
	case wasiModule:
		fs = wasiFuncs()
	default:
		return hostFunc{}, false
	}
	for _, f := range fs {
		if f.name == name {
			return f, true
		}
	}
	return hostFunc{}, false
}

// signature returns f's signature as value type bytes, as the structure
// check reads an import's.
func (f hostFunc) signature() funcType {
	t := funcType{params: make([]byte, len(f.params))}
	copy(t.params, f.params)
	if !f.void {
		t.results = []byte{valI32}
	}
	return t
}

// errNoCall reports a Host Function call outside any guest call, such as
// from a start function: the host traps the guest.
var errNoCall = errors.New("pluginhost: Host Function called outside a Phase call or rz_configure")

// errOutOfBounds reports a guest pointer and length outside its linear
// memory: the host traps the guest, never itself.
var errOutOfBounds = errors.New("pluginhost: guest buffer out of bounds")

// instantiateHost instantiates the ruralz.plugin.v1 and
// wasi_snapshot_preview1 host modules in rt.
func instantiateHost(ctx context.Context, rt wazero.Runtime) error {
	for _, mod := range []struct {
		name  string
		funcs []hostFunc
	}{{abiModule, abiFuncs()}, {wasiModule, wasiFuncs()}} {
		b := rt.NewHostModuleBuilder(mod.name)
		for _, f := range mod.funcs {
			b.NewFunctionBuilder().WithGoModuleFunction(gate(f), f.params, f.resultTypes()).WithName(f.name).Export(f.name)
		}
		if _, err := b.Instantiate(ctx); err != nil {
			return err
		}
	}
	return nil
}

// resultTypes returns f's results: i32, none for a void function.
func (f hostFunc) resultTypes() []api.ValueType {
	if f.void {
		return nil
	}
	return []api.ValueType{i32}
}

// gate wraps f with the checks every import passes before its body runs
// (docs/architecture/05-wasm-plugin-system.md "Contract"): the call budget,
// the Capability (re-checked on every call), rz_configure's subset and the
// Phase. ERR_BUDGET, ERR_DENIED and ERR_PHASE are sticky: the call then
// fails with RZ-PLG-006 whatever the guest returns.
func gate(f hostFunc) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		c, _ := ctx.Value(callKey{}).(*call)
		if c == nil {
			panic(errNoCall)
		}
		r := c.enter(f, mod.Memory(), stack)
		if !f.void {
			stack[0] = uint64(uint32(r)) //nolint:gosec // G115: an i32 result travels as its bit pattern.
		}
	}
}

// enter runs f for c after the checks.
func (c *call) enter(f hostFunc, m api.Memory, stack []uint64) int32 {
	c.hostCalls++
	if c.hostCalls > callBudget {
		return c.stick(errBudget, "the call made more than the Host Function call budget")
	}
	if f.capability != capNone && !c.b.caps.has(f.capability) {
		return c.stick(errDenied, "the Capability "+f.capability.String()+" is not granted")
	}
	if c.configuring {
		if !f.configure {
			return c.stick(errPhase, f.name+" is not available to rz_configure")
		}
	} else if !c.validPhase(f) {
		return c.stick(errPhase, f.name+" is not valid in "+c.ph.String())
	}
	return f.fn(c, m, stack)
}

// validPhase reports whether f may run in c's Phase: its table Phases, and
// for consumer_info the Phases after the auth Filter class, so not
// onRequestHeaders for a Policy of class cors or auth.
func (c *call) validPhase(f hostFunc) bool {
	if !f.phases.Has(c.ph) {
		return false
	}
	if f.capability == capConsumerRead && c.ph == phase.OnRequestHeaders {
		return c.b.class > phase.ClassAuth
	}
	return true
}

// stick records a sticky error, the first one winning, and returns it.
func (c *call) stick(code int32, why string) int32 {
	if c.sticky == 0 {
		c.sticky, c.stickyWhy = code, why
	}
	return code
}

// mem reads n bytes at p of guest memory m, copied: the host never
// retains guest pointers. A range outside the memory traps the guest.
func mem(m api.Memory, p, n uint32) []byte {
	if n == 0 {
		return nil
	}
	b, ok := m.Read(p, n)
	if !ok {
		panic(errOutOfBounds)
	}
	return append([]byte(nil), b...)
}

// memString reads a string argument.
func memString(m api.Memory, p, n uint32) string {
	if n == 0 {
		return ""
	}
	b, ok := m.Read(p, n)
	if !ok {
		panic(errOutOfBounds)
	}
	return string(b)
}

// checkRange traps the guest when [o, o+n) is outside memory m.
func checkRange(m api.Memory, o, n uint32) {
	if uint64(o)+uint64(n) > uint64(m.Size()) {
		panic(errOutOfBounds)
	}
}

// output writes v to the guest buffer (o, cap) and returns len(v): above
// cap nothing is written, so the guest retries with a larger buffer. The
// whole buffer must lie in memory, written or not.
func output(m api.Memory, o, capacity uint32, v []byte) int32 {
	checkRange(m, o, capacity)
	if len(v) > int(capacity) {
		return lenResult(len(v))
	}
	if len(v) > 0 && !m.Write(o, v) {
		panic(errOutOfBounds)
	}
	return lenResult(len(v))
}

// partial writes as much of v as fits in (o, cap) and returns the bytes
// written: body and chunk reads, which stream through a fixed buffer.
func partial(m api.Memory, o, capacity uint32, v []byte) int32 {
	checkRange(m, o, capacity)
	n := min(len(v), int(capacity))
	if n > 0 && !m.Write(o, v[:n]) {
		panic(errOutOfBounds)
	}
	return lenResult(n)
}

// lenResult returns a length as an i32 result; a length past the i32
// range, impossible within linear memory limits, is ERR_LIMIT.
func lenResult(n int) int32 {
	if n > 1<<31-1 {
		return errLimit
	}
	return int32(n) //nolint:gosec // G115: n is checked against the i32 range.
}

// u32 returns stack slot i as an unsigned i32 argument.
func u32(stack []uint64, i int) uint32 { return uint32(stack[i]) } //nolint:gosec // G115: an i32 argument is its low 32 bits.

// s32 returns the first stack slot as a signed i32 argument: every signed
// i32 argument of the ABI, a level, a field, a status, an algorithm or a
// key handle, comes first.
func s32(stack []uint64) int32 { return int32(uint32(stack[0])) } //nolint:gosec // G115: an i32 argument is its low 32 bits.

// s64 returns stack slot i as a signed i64 argument.
func s64(stack []uint64, i int) int64 { return int64(stack[i]) } //nolint:gosec // G115: an i64 argument is its bit pattern.
