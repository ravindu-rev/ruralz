// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package pluginhosttest is the test support of internal/pluginhost: a
// WebAssembly binary encoder that emits the modules each test needs, so no
// test depends on a guest toolchain the repository lacks, and a guest
// builder that adds the Plugin ABI v1 exports (rz_abi_version, rz_alloc,
// rz_free, rz_configure and the Phase exports) a test guest needs. Only
// tests import it.
package pluginhosttest

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ValType is a WebAssembly value type.
type ValType byte

// Value types.
const (
	I32 ValType = 0x7f
	I64 ValType = 0x7e
	F32 ValType = 0x7d
	F64 ValType = 0x7c
)

// External kinds of imports and exports.
const (
	KindFunc   byte = 0x00
	KindTable  byte = 0x01
	KindMemory byte = 0x02
	KindGlobal byte = 0x03
)

// Section identifiers.
const (
	SectionCustom    byte = 0
	SectionType      byte = 1
	SectionImport    byte = 2
	SectionFunction  byte = 3
	SectionTable     byte = 4
	SectionMemory    byte = 5
	SectionGlobal    byte = 6
	SectionExport    byte = 7
	SectionStart     byte = 8
	SectionElement   byte = 9
	SectionCode      byte = 10
	SectionData      byte = 11
	SectionDataCount byte = 12
)

// FuncType is a function signature.
type FuncType struct {
	Params, Results []ValType
}

// Sig returns the FuncType of params and results.
func Sig(params []ValType, results ...ValType) FuncType {
	return FuncType{Params: params, Results: results}
}

// Params returns its arguments as a parameter list for Sig.
func Params(v ...ValType) []ValType { return v }

// Locals is one run of locals of a function body: N locals of Type.
type Locals struct {
	N    uint32
	Type ValType
}

// fn is one defined function.
type fn struct {
	typ    uint32
	locals []Locals
	body   []byte
}

// imp is one imported function.
type imp struct {
	module, name string
	kind         byte
	typ          uint32
	// mem, for a memory import: the limits.
	mem memLimits
}

// memLimits are memory limits in 64 KiB pages.
type memLimits struct {
	min, max uint32
	hasMax   bool
}

// global is one defined global with a constant initializer.
type global struct {
	typ     ValType
	mutable bool
	init    int64
}

// export is one export.
type export struct {
	name  string
	kind  byte
	index uint32
}

// data is one active data segment of memory 0 at a constant offset.
type data struct {
	offset uint32
	bytes  []byte
}

// custom is one custom section.
type custom struct {
	name    string
	payload []byte
}

// Module is a WebAssembly module under construction. Imports come before
// defined functions, as in the function index space; the zero value is an
// empty module.
type Module struct {
	types   []FuncType
	imports []imp
	funcs   []fn
	memory  *memLimits
	table   *memLimits
	globals []global
	exports []export
	data    []data
	elems   []uint32
	start   *uint32
	customs []custom
	// raw sections replace the encoder's own section of that id, for
	// tests of malformed or oversized modules.
	raw map[byte][]byte
	// extra sections are appended after the known ones, in order.
	extra [][]byte
}

// Type returns the index of t in the type section, adding it once.
func (m *Module) Type(t FuncType) uint32 {
	for i, u := range m.types {
		if equalTypes(t, u) {
			return uint32(i)
		}
	}
	m.types = append(m.types, t)
	return uint32(len(m.types) - 1) //nolint:gosec // G115: a test module holds few types.
}

func equalTypes(a, b FuncType) bool {
	if len(a.Params) != len(b.Params) || len(a.Results) != len(b.Results) {
		return false
	}
	for i := range a.Params {
		if a.Params[i] != b.Params[i] {
			return false
		}
	}
	for i := range a.Results {
		if a.Results[i] != b.Results[i] {
			return false
		}
	}
	return true
}

// ImportFunc imports module.name with signature t and returns its function
// index. It panics after the first defined function, since imports take
// the low indexes.
func (m *Module) ImportFunc(module, name string, t FuncType) uint32 {
	if len(m.funcs) > 0 {
		panic("pluginhosttest: ImportFunc after Func")
	}
	m.imports = append(m.imports, imp{module: module, name: name, kind: KindFunc, typ: m.Type(t)})
	return m.funcImports() - 1
}

// ImportMemory imports module.name as the module's memory.
func (m *Module) ImportMemory(module, name string, minPages uint32) {
	m.imports = append(m.imports, imp{module: module, name: name, kind: KindMemory, mem: memLimits{min: minPages}})
}

// funcImports returns the number of imported functions.
func (m *Module) funcImports() uint32 {
	var n uint32
	for _, i := range m.imports {
		if i.kind == KindFunc {
			n++
		}
	}
	return n
}

// Func defines a function of signature t with locals and body, which must
// not hold the final end, and returns its function index.
func (m *Module) Func(t FuncType, locals []Locals, body ...[]byte) uint32 {
	m.funcs = append(m.funcs, fn{typ: m.Type(t), locals: locals, body: Code(body...)})
	return m.funcImports() + uint32(len(m.funcs)-1) //nolint:gosec // G115: a test module holds few functions.
}

// SetBody replaces the body of the defined function at index f.
func (m *Module) SetBody(f uint32, body ...[]byte) {
	m.funcs[f-m.funcImports()].body = Code(body...)
}

// Memory defines memory 0 with min pages and, when hasMax, max pages.
func (m *Module) Memory(minPages, maxPages uint32, hasMax bool) {
	m.memory = &memLimits{min: minPages, max: maxPages, hasMax: hasMax}
}

// Table defines table 0 of funcref with min elements, filled from index 0
// with funcs by Elements.
func (m *Module) Table(minElems uint32) {
	m.table = &memLimits{min: minElems}
}

// Elements places funcs into table 0 from index 0.
func (m *Module) Elements(funcs ...uint32) { m.elems = append(m.elems, funcs...) }

// Global defines a global of typ with the constant init and returns its
// index.
func (m *Module) Global(typ ValType, mutable bool, init int64) uint32 {
	m.globals = append(m.globals, global{typ: typ, mutable: mutable, init: init})
	return uint32(len(m.globals) - 1) //nolint:gosec // G115: a test module holds few globals.
}

// Export exports index of kind as name.
func (m *Module) Export(name string, kind byte, index uint32) {
	m.exports = append(m.exports, export{name: name, kind: kind, index: index})
}

// ExportFunc exports function f as name.
func (m *Module) ExportFunc(name string, f uint32) { m.Export(name, KindFunc, f) }

// Data places b in memory 0 at offset.
func (m *Module) Data(offset uint32, b []byte) {
	m.data = append(m.data, data{offset: offset, bytes: append([]byte(nil), b...)})
}

// Start makes f the start function.
func (m *Module) Start(f uint32) { m.start = &f }

// Custom adds a custom section.
func (m *Module) Custom(name string, payload []byte) {
	m.customs = append(m.customs, custom{name: name, payload: payload})
}

// RawSection replaces the encoder's section id with payload, as given.
func (m *Module) RawSection(id byte, payload []byte) {
	if m.raw == nil {
		m.raw = map[byte][]byte{}
	}
	m.raw[id] = payload
}

// AppendSection appends a section id with payload after the others.
func (m *Module) AppendSection(id byte, payload []byte) {
	m.extra = append(m.extra, Section(id, payload))
}

// Encode returns the binary module.
func (m *Module) Encode() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	section := func(id byte, present bool, payload func() []byte) {
		if p, ok := m.raw[id]; ok {
			out = append(out, Section(id, p)...)
			return
		}
		if present {
			out = append(out, Section(id, payload())...)
		}
	}
	section(SectionType, len(m.types) > 0, m.typeSection)
	section(SectionImport, len(m.imports) > 0, m.importSection)
	section(SectionFunction, len(m.funcs) > 0, m.functionSection)
	section(SectionTable, m.table != nil, m.tableSection)
	section(SectionMemory, m.memory != nil, m.memorySection)
	section(SectionGlobal, len(m.globals) > 0, m.globalSection)
	section(SectionExport, len(m.exports) > 0, m.exportSection)
	section(SectionStart, m.start != nil, func() []byte { return U32(*m.start) })
	section(SectionElement, len(m.elems) > 0, m.elementSection)
	section(SectionCode, len(m.funcs) > 0, m.codeSection)
	section(SectionData, len(m.data) > 0, m.dataSection)
	for _, c := range m.customs {
		out = append(out, Section(SectionCustom, append(Name(c.name), c.payload...))...)
	}
	for _, e := range m.extra {
		out = append(out, e...)
	}
	return out
}

func (m *Module) typeSection() []byte {
	b := U32(uint32(len(m.types))) //nolint:gosec // G115: a test module holds few types.
	for _, t := range m.types {
		b = append(b, 0x60)
		b = append(b, vec(t.Params)...)
		b = append(b, vec(t.Results)...)
	}
	return b
}

func vec(v []ValType) []byte {
	b := U32(uint32(len(v))) //nolint:gosec // G115: signatures are short.
	for _, t := range v {
		b = append(b, byte(t))
	}
	return b
}

func (m *Module) importSection() []byte {
	b := U32(uint32(len(m.imports))) //nolint:gosec // G115: a test module holds few imports.
	for _, i := range m.imports {
		b = append(b, Name(i.module)...)
		b = append(b, Name(i.name)...)
		b = append(b, i.kind)
		switch i.kind {
		case KindMemory:
			b = append(b, limits(i.mem)...)
		default:
			b = append(b, U32(i.typ)...)
		}
	}
	return b
}

func (m *Module) functionSection() []byte {
	b := U32(uint32(len(m.funcs))) //nolint:gosec // G115: a test module holds few functions.
	for _, f := range m.funcs {
		b = append(b, U32(f.typ)...)
	}
	return b
}

func limits(l memLimits) []byte {
	if l.hasMax {
		return append(append([]byte{0x01}, U32(l.min)...), U32(l.max)...)
	}
	return append([]byte{0x00}, U32(l.min)...)
}

func (m *Module) tableSection() []byte {
	return append([]byte{0x01, 0x70}, limits(*m.table)...)
}

func (m *Module) memorySection() []byte {
	return append([]byte{0x01}, limits(*m.memory)...)
}

func (m *Module) globalSection() []byte {
	b := U32(uint32(len(m.globals))) //nolint:gosec // G115: a test module holds few globals.
	for _, g := range m.globals {
		b = append(b, byte(g.typ))
		if g.mutable {
			b = append(b, 0x01)
		} else {
			b = append(b, 0x00)
		}
		switch g.typ {
		case I64:
			b = append(b, I64Const(g.init)...)
		case F32:
			b = append(b, 0x43)
			b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(g.init)))
		case F64:
			b = append(b, 0x44)
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(float64(g.init)))
		default:
			b = append(b, I32Const(int32(g.init))...) //nolint:gosec // G115: an i32 global's initializer is an i32.
		}
		b = append(b, OpEnd)
	}
	return b
}

func (m *Module) exportSection() []byte {
	b := U32(uint32(len(m.exports))) //nolint:gosec // G115: a test module holds few exports.
	for _, e := range m.exports {
		b = append(b, Name(e.name)...)
		b = append(b, e.kind)
		b = append(b, U32(e.index)...)
	}
	return b
}

func (m *Module) elementSection() []byte {
	// One active segment of table 0 at offset 0 (flags 0).
	b := U32(1)
	b = append(b, 0x00)
	b = append(b, I32Const(0)...)
	b = append(b, OpEnd)
	b = append(b, U32(uint32(len(m.elems)))...) //nolint:gosec // G115: a test table is small.
	for _, f := range m.elems {
		b = append(b, U32(f)...)
	}
	return b
}

func (m *Module) codeSection() []byte {
	b := U32(uint32(len(m.funcs))) //nolint:gosec // G115: a test module holds few functions.
	for _, f := range m.funcs {
		body := U32(uint32(len(f.locals))) //nolint:gosec // G115: a test function holds few local runs.
		for _, l := range f.locals {
			body = append(body, U32(l.N)...)
			body = append(body, byte(l.Type))
		}
		body = append(body, f.body...)
		body = append(body, OpEnd)
		b = append(b, U32(uint32(len(body)))...) //nolint:gosec // G115: test bodies are below 4 GiB.
		b = append(b, body...)
	}
	return b
}

func (m *Module) dataSection() []byte {
	b := U32(uint32(len(m.data))) //nolint:gosec // G115: a test module holds few segments.
	for _, d := range m.data {
		b = append(b, 0x00)
		b = append(b, I32Const(int32(d.offset))...) //nolint:gosec // G115: test offsets are below 2 GiB.
		b = append(b, OpEnd)
		b = append(b, U32(uint32(len(d.bytes)))...) //nolint:gosec // G115: test segments are below 4 GiB.
		b = append(b, d.bytes...)
	}
	return b
}

// Section returns a section with id and payload.
func Section(id byte, payload []byte) []byte {
	b := []byte{id}
	b = append(b, U32(uint32(len(payload)))...) //nolint:gosec // G115: test payloads are below 4 GiB.
	return append(b, payload...)
}

// Name returns the vector encoding of a name.
func Name(s string) []byte {
	return append(U32(uint32(len(s))), s...) //nolint:gosec // G115: names are short.
}

// U32 returns the unsigned LEB128 encoding of v.
func U32(v uint32) []byte { return uleb(uint64(v)) }

func uleb(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

func sleb(v int64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// Code concatenates instruction byte strings.
func Code(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Repeat returns n copies of code.
func Repeat(n int, code ...[]byte) []byte {
	one := Code(code...)
	out := make([]byte, 0, n*len(one))
	for range n {
		out = append(out, one...)
	}
	return out
}

// Opcodes without immediates.
const (
	OpUnreachable byte = 0x00
	OpNop         byte = 0x01
	OpElse        byte = 0x05
	OpEnd         byte = 0x0b
	OpReturn      byte = 0x0f
	OpDrop        byte = 0x1a
	OpSelect      byte = 0x1b
	OpI32Eqz      byte = 0x45
	OpI32Eq       byte = 0x46
	OpI32Ne       byte = 0x47
	OpI32LtS      byte = 0x48
	OpI32LtU      byte = 0x49
	OpI32GtS      byte = 0x4a
	OpI32GtU      byte = 0x4b
	OpI32LeS      byte = 0x4c
	OpI32GeS      byte = 0x4e
	OpI32GeU      byte = 0x4f
	OpI64Eqz      byte = 0x50
	OpI64Eq       byte = 0x51
	OpI64Ne       byte = 0x52
	OpI32Add      byte = 0x6a
	OpI32Sub      byte = 0x6b
	OpI32Mul      byte = 0x6c
	OpI32DivS     byte = 0x6d
	OpI32DivU     byte = 0x6e
	OpI32And      byte = 0x71
	OpI32Or       byte = 0x72
	OpI32Xor      byte = 0x73
	OpI32Shl      byte = 0x74
	OpI32ShrU     byte = 0x76
	OpI64Add      byte = 0x7c
	OpI64Sub      byte = 0x7d
	OpI64Mul      byte = 0x7e
	OpI64DivS     byte = 0x7f
	OpI32WrapI64  byte = 0xa7
	OpI64ExtendU  byte = 0xad
	OpI64ExtendS  byte = 0xac
)

// Block types.
const (
	BlockEmpty byte = 0x40
	BlockI32   byte = 0x7f
	BlockI64   byte = 0x7e
)

// Op returns a one-byte instruction.
func Op(op byte) []byte { return []byte{op} }

// I32Const returns i32.const v.
func I32Const(v int32) []byte { return append([]byte{0x41}, sleb(int64(v))...) }

// I64Const returns i64.const v.
func I64Const(v int64) []byte { return append([]byte{0x42}, sleb(v)...) }

// LocalGet returns local.get i.
func LocalGet(i uint32) []byte { return append([]byte{0x20}, U32(i)...) }

// LocalSet returns local.set i.
func LocalSet(i uint32) []byte { return append([]byte{0x21}, U32(i)...) }

// LocalTee returns local.tee i.
func LocalTee(i uint32) []byte { return append([]byte{0x22}, U32(i)...) }

// GlobalGet returns global.get i.
func GlobalGet(i uint32) []byte { return append([]byte{0x23}, U32(i)...) }

// GlobalSet returns global.set i.
func GlobalSet(i uint32) []byte { return append([]byte{0x24}, U32(i)...) }

// Call returns call f.
func Call(f uint32) []byte { return append([]byte{0x10}, U32(f)...) }

// CallIndirect returns call_indirect of type t on table 0.
func CallIndirect(t uint32) []byte { return append(append([]byte{0x11}, U32(t)...), 0x00) }

// Block returns block with block type bt.
func Block(bt byte) []byte { return []byte{0x02, bt} }

// Loop returns loop with block type bt.
func Loop(bt byte) []byte { return []byte{0x03, bt} }

// If returns if with block type bt.
func If(bt byte) []byte { return []byte{0x04, bt} }

// Br returns br l.
func Br(l uint32) []byte { return append([]byte{0x0c}, U32(l)...) }

// BrIf returns br_if l.
func BrIf(l uint32) []byte { return append([]byte{0x0d}, U32(l)...) }

// End returns end.
func End() []byte { return []byte{OpEnd} }

// mem returns a memory instruction op with alignment log2 align and offset.
func mem(op byte, align, offset uint32) []byte {
	return append(append([]byte{op}, U32(align)...), U32(offset)...)
}

// I32Load returns i32.load offset.
func I32Load(offset uint32) []byte { return mem(0x28, 2, offset) }

// I64Load returns i64.load offset.
func I64Load(offset uint32) []byte { return mem(0x29, 3, offset) }

// I32Load8U returns i32.load8_u offset.
func I32Load8U(offset uint32) []byte { return mem(0x2d, 0, offset) }

// I32Store returns i32.store offset.
func I32Store(offset uint32) []byte { return mem(0x36, 2, offset) }

// I64Store returns i64.store offset.
func I64Store(offset uint32) []byte { return mem(0x37, 3, offset) }

// I32Store8 returns i32.store8 offset.
func I32Store8(offset uint32) []byte { return mem(0x3a, 0, offset) }

// MemorySize returns memory.size of memory 0.
func MemorySize() []byte { return []byte{0x3f, 0x00} }

// MemoryGrow returns memory.grow of memory 0.
func MemoryGrow() []byte { return []byte{0x40, 0x00} }

// MemoryFill returns memory.fill of memory 0.
func MemoryFill() []byte { return []byte{0xfc, 0x0b, 0x00} }

// MemoryCopy returns memory.copy within memory 0.
func MemoryCopy() []byte { return []byte{0xfc, 0x0a, 0x00, 0x00} }

// Hex formats b for test failure messages.
func Hex(b []byte) string { return fmt.Sprintf("% x", b) }
