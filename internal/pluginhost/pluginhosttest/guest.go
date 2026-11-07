// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhosttest

import (
	"fmt"
	"strings"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ABI and WASI import modules.
const (
	ABIModule  = string(v1alpha1.PluginABIV1)
	WASIModule = "wasi_snapshot_preview1"
)

// hostParams returns the parameters of each Host Function of Plugin ABI
// v1 and of the WASI shim functions tests import; every one returns an
// i32 but proc_exit. The host checks imports against its own table, so a
// test of a wrong signature uses ImportFunc directly.
func hostParams(module, name string) ([]ValType, bool) {
	p := func(v ...ValType) []ValType { return v }
	if module == WASIModule {
		switch name {
		case "fd_write":
			return p(I32, I32, I32, I32), true
		case "clock_time_get":
			return p(I32, I64, I32), true
		case "clock_res_get", "random_get", "args_sizes_get", "environ_sizes_get", "args_get", "environ_get":
			return p(I32, I32), true
		case "proc_exit":
			return p(I32), true
		case "fd_close":
			return p(I32), true
		case "path_open":
			return p(I32, I32, I32, I32, I32, I64, I64, I32, I32), true
		case "sched_yield":
			return p(), true
		}
		return nil, false
	}
	switch name {
	case "log", "request_info", "consumer_info", "response_send", "chunk_read":
		return p(I32, I32, I32), true
	case "clock_now":
		return p(I32), true
	case "random_bytes", "request_headers_list", "request_header_remove", "request_body_replace",
		"response_body_replace", "chunk_replace":
		return p(I32, I32), true
	case "request_header_get", "request_header_set", "response_header_get", "response_header_set",
		"context_get", "context_set", "state_get":
		return p(I32, I32, I32, I32), true
	case "request_body_read", "response_body_read":
		return p(I64, I32, I32), true
	case "state_incr":
		return p(I32, I32, I64, I64, I32), true
	case "metric_add":
		return p(I32, I32, I64), true
	case "crypto_digest", "crypto_key_sign", "crypto_key_verify", "crypto_key_hmac":
		return p(I32, I32, I32, I32, I32), true
	case "crypto_hmac":
		return p(I32, I32, I32, I32, I32, I32, I32), true
	}
	return nil, false
}

// Guest is a Plugin ABI v1 guest under construction: a memory of two
// pages with no declared maximum, constant data from offset 1024, a bump
// allocator from the second page, and the required exports. Host
// Functions are imported by name before the first function is defined.
type Guest struct {
	M Module
	// funcs maps an imported Host Function to its index.
	funcs map[string]uint32
	// heap is the global holding the allocator's next free address;
	// configPtr and configLen hold what rz_configure received.
	heap, configPtr, configLen uint32
	data                       uint32
	configure                  []byte
	confFunc                   uint32
	built                      bool
}

// Scratch is the guest buffer tests point Host Function outputs at: 512
// bytes from offset 256.
const (
	Scratch    uint32 = 256
	ScratchLen uint32 = 512
)

// NewGuest returns a guest that imports the ABI Host Functions named
// (names of the form "wasi.fd_write" import the WASI shim's).
func NewGuest(imports ...string) *Guest {
	g := &Guest{funcs: map[string]uint32{}, data: 1024}
	for _, name := range imports {
		module, fn := ABIModule, name
		if strings.HasPrefix(name, "wasi.") {
			module, fn = WASIModule, strings.TrimPrefix(name, "wasi.")
		}
		params, ok := hostParams(module, fn)
		if !ok {
			panic(fmt.Sprintf("pluginhosttest: no signature for %s.%s", module, fn))
		}
		results := []ValType{I32}
		if fn == "proc_exit" {
			results = nil
		}
		g.funcs[name] = g.M.ImportFunc(module, fn, FuncType{Params: params, Results: results})
	}
	g.M.Memory(2, 0, false)
	g.M.Export("memory", KindMemory, 0)
	g.heap = g.M.Global(I32, true, 65536)
	g.configPtr = g.M.Global(I32, true, 0)
	g.configLen = g.M.Global(I32, true, 0)
	return g
}

// Fn returns the index of an imported Host Function.
func (g *Guest) Fn(name string) uint32 {
	f, ok := g.funcs[name]
	if !ok {
		panic("pluginhosttest: Host Function " + name + " is not imported")
	}
	return f
}

// Call returns the instructions that push args as i32 constants and call
// the imported Host Function name.
func (g *Guest) Call(name string, args ...[]byte) []byte {
	return Code(Code(args...), Call(g.Fn(name)))
}

// Str places s in the guest's constant data and returns the instructions
// pushing its address and length.
func (g *Guest) Str(s string) []byte {
	p := g.data
	g.M.Data(p, []byte(s))
	g.data += uint32(len(s)) + 8                             //nolint:gosec // G115: test strings are short.
	return Code(I32Const(int32(p)), I32Const(int32(len(s)))) //nolint:gosec // G115: test data stays below 64 KiB.
}

// Addr returns the instruction pushing the guest address a.
func Addr(a uint32) []byte { return I32Const(int32(a)) } //nolint:gosec // G115: test data stays below 64 KiB.

// Size returns the i32 of a test buffer's length n.
func Size(n int) int32 { return int32(n) } //nolint:gosec // G115: test data stays below 64 KiB.

// Bytes places b in constant data and returns its address.
func (g *Guest) Bytes(b []byte) uint32 {
	p := g.data
	g.M.Data(p, b)
	g.data += uint32(len(b)) + 8 //nolint:gosec // G115: test data is short.
	return p
}

// ConfigPtr and ConfigLen return the instructions reading what
// rz_configure received.
func (g *Guest) ConfigPtr() []byte { return GlobalGet(g.configPtr) }

// ConfigLen pushes the config length rz_configure received.
func (g *Guest) ConfigLen() []byte { return GlobalGet(g.configLen) }

// Configure sets rz_configure's body after it records its buffer; the body
// leaves the i32 result. Without it rz_configure returns 0.
func (g *Guest) Configure(body ...[]byte) { g.configure = Code(body...) }

// Phase exports the Phase function export (such as rz_on_request_headers)
// with locals and a body that leaves the i32 Phase result.
func (g *Guest) Phase(export string, locals []Locals, body ...[]byte) uint32 {
	g.required()
	f := g.M.Func(Sig(nil, I32), locals, body...)
	g.M.ExportFunc(export, f)
	return f
}

// Func defines a helper function.
func (g *Guest) Func(t FuncType, locals []Locals, body ...[]byte) uint32 {
	g.required()
	return g.M.Func(t, locals, body...)
}

// required defines the four required exports once, before any other
// function.
func (g *Guest) required() {
	if g.built {
		return
	}
	g.built = true
	ver := g.M.Func(Sig(nil, I32), nil, I32Const(1))
	g.M.ExportFunc("rz_abi_version", ver)
	alloc := g.M.Func(Sig(Params(I32), I32), []Locals{{N: 1, Type: I32}},
		GlobalGet(g.heap), LocalSet(1),
		GlobalGet(g.heap), LocalGet(0), I32Const(7), Op(OpI32Add), I32Const(-8), Op(OpI32And), Op(OpI32Add), GlobalSet(g.heap),
		LocalGet(1))
	g.M.ExportFunc("rz_alloc", alloc)
	free := g.M.Func(Sig(Params(I32)), nil)
	g.M.ExportFunc("rz_free", free)
	conf := g.M.Func(Sig(Params(I32, I32), I32), nil)
	g.M.ExportFunc("rz_configure", conf)
	g.confFunc = conf
}

// Encode returns the binary, with rz_configure's body.
func (g *Guest) Encode() []byte {
	g.required()
	body := Code(LocalGet(0), GlobalSet(g.configPtr), LocalGet(1), GlobalSet(g.configLen))
	if g.configure != nil {
		body = Code(body, g.configure)
	} else {
		body = Code(body, I32Const(0))
	}
	g.M.SetBody(g.confFunc, body)
	return g.M.Encode()
}

// Check returns instructions that run call and make the Phase return -1
// (cannot decide) unless it left want.
func Check(call []byte, want int32) []byte {
	return Code(call, I32Const(want), Op(OpI32Ne), If(BlockEmpty), I32Const(-1), Op(OpReturn), End())
}

// CheckMem returns instructions that make the Phase return -1 unless the
// guest memory at addr holds want.
func CheckMem(addr uint32, want []byte) []byte {
	var out []byte
	for i, b := range want {
		out = append(out, Check(Code(I32Const(int32(addr)+int32(i)), I32Load8U(0)), int32(b))...) //nolint:gosec // G115: test addresses are small.
	}
	return out
}
