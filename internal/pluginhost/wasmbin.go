// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The structure check reads a binary's sections before any compile
// (docs/architecture/05-wasm-plugin-system.md "Default limits", Artifact
// size and structure): the counts the limits bound, the imports the
// Capability gate checks and the exports the ABI requires. It reads only
// what it needs and skips other sections by their sizes; full validation
// is wazero's, at compile time.

// errMalformed reports a binary the structure check cannot read; the
// compile would refuse it too, so it is a compile error, not a structure
// limit.
var errMalformed = errors.New("malformed WebAssembly binary")

// External kinds of the import and export sections.
const (
	externFunc   byte = 0x00
	externTable  byte = 0x01
	externMemory byte = 0x02
	externGlobal byte = 0x03
	externTag    byte = 0x04
)

// Value types of function signatures (WebAssembly 2.0).
const (
	valI32       byte = 0x7f
	valI64       byte = 0x7e
	valF32       byte = 0x7d
	valF64       byte = 0x7c
	valV128      byte = 0x7b
	valFuncref   byte = 0x70
	valExternref byte = 0x6f
)

// funcType is a function signature as value type bytes.
type funcType struct {
	params, results []byte
}

// equal reports whether t has exactly params and results.
func (t funcType) equal(params, results []byte) bool {
	return string(t.params) == string(params) && string(t.results) == string(results)
}

// wasmImport is one import.
type wasmImport struct {
	module, name string
	kind         byte
	// typ is the signature of a function import.
	typ funcType
}

// wasmExport is one export.
type wasmExport struct {
	name  string
	kind  byte
	index uint32
}

// memoryType is the limits of one memory, in 64 KiB pages.
type memoryType struct {
	min, max       uint64
	hasMax, shared bool
}

// moduleInfo is what the structure check reads from a binary.
type moduleInfo struct {
	types   []funcType
	imports []wasmImport
	// funcImports is the number of imported functions, the first indexes
	// of the function index space.
	funcImports uint32
	// funcTypes holds the type index of each defined function.
	funcTypes []uint32
	exports   []wasmExport
	memories  []memoryType
	// memoryImports counts imported memories.
	memoryImports int
	// functions is the number of defined functions (the code section).
	functions int
	// maxLocals is the largest number of locals one defined function
	// declares, and maxLocalsFunc that function's index among the defined
	// ones.
	maxLocals     uint64
	maxLocalsFunc int
	// dataBytes is the total payload of the data segments.
	dataBytes uint64
	// codeSectionBytes is the payload size of the code section.
	codeSectionBytes int
}

// exportedFunc returns the signature of the function exported as name.
func (m *moduleInfo) exportedFunc(name string) (funcType, bool) {
	for _, e := range m.exports {
		if e.name != name || e.kind != externFunc {
			continue
		}
		return m.funcSignature(e.index)
	}
	return funcType{}, false
}

// funcSignature returns the signature of function index i.
func (m *moduleInfo) funcSignature(i uint32) (funcType, bool) {
	if i < m.funcImports {
		n := uint32(0)
		for _, im := range m.imports {
			if im.kind != externFunc {
				continue
			}
			if n == i {
				return im.typ, true
			}
			n++
		}
		return funcType{}, false
	}
	d := i - m.funcImports
	if d >= uint32(len(m.funcTypes)) { //nolint:gosec // G115: funcTypes is bounded by the binary's size.
		return funcType{}, false
	}
	t := m.funcTypes[d]
	if t >= uint32(len(m.types)) { //nolint:gosec // G115: types is bounded by the binary's size.
		return funcType{}, false
	}
	return m.types[t], true
}

// reader reads a binary.
type reader struct {
	b   []byte
	off int
}

func (r *reader) eof() bool { return r.off >= len(r.b) }

func (r *reader) byte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, errMalformed
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

// uleb reads an unsigned LEB128 of at most bits bits.
func (r *reader) uleb(bits uint) (uint64, error) {
	var v uint64
	var shift uint
	for {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		if shift >= bits || (shift+7 > bits && uint64(c&0x7f)>>(bits-shift) != 0) {
			return 0, errMalformed
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
}

func (r *reader) u32() (uint32, error) {
	v, err := r.uleb(32)
	return uint32(v), err //nolint:gosec // G115: uleb(32) never exceeds 32 bits.
}

// sleb skips a signed LEB128 of at most bits bits.
func (r *reader) sleb(bits uint) error {
	for i := uint(0); ; i += 7 {
		c, err := r.byte()
		if err != nil {
			return err
		}
		if i >= bits {
			return errMalformed
		}
		if c&0x80 == 0 {
			return nil
		}
	}
}

// bytes returns the next n bytes.
func (r *reader) bytes(n uint32) ([]byte, error) {
	if int64(n) > int64(len(r.b)-r.off) {
		return nil, errMalformed
	}
	s := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return s, nil
}

// name reads a UTF-8 name.
func (r *reader) name() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(n)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errMalformed
	}
	return string(b), nil
}

// count reads a vector length that each element consumes at least min
// bytes of, so a count larger than the rest of the section is refused
// before anything is allocated for it.
func (r *reader) count(minBytes int) (int, error) {
	n, err := r.u32()
	if err != nil {
		return 0, err
	}
	if int64(n)*int64(minBytes) > int64(len(r.b)-r.off) {
		return 0, errMalformed
	}
	return int(n), nil
}

// valType reads one value type of a signature.
func (r *reader) valType() (byte, error) {
	c, err := r.byte()
	if err != nil {
		return 0, err
	}
	switch c {
	case valI32, valI64, valF32, valF64, valV128, valFuncref, valExternref:
		return c, nil
	default:
		return 0, errMalformed
	}
}

// limits reads memory or table limits.
func (r *reader) limits() (memoryType, error) {
	flags, err := r.byte()
	if err != nil {
		return memoryType{}, err
	}
	if flags > 0x03 {
		// 64-bit memories and other flags: not WebAssembly 2.0.
		return memoryType{}, errMalformed
	}
	var m memoryType
	if m.min, err = r.uleb(32); err != nil {
		return memoryType{}, err
	}
	if flags&0x01 != 0 {
		m.hasMax = true
		if m.max, err = r.uleb(32); err != nil {
			return memoryType{}, err
		}
	}
	m.shared = flags&0x02 != 0
	return m, nil
}

// constExpr skips a constant expression up to its end.
func (r *reader) constExpr() error {
	for {
		op, err := r.byte()
		if err != nil {
			return err
		}
		switch op {
		case 0x0b:
			return nil
		case 0x41:
			err = r.sleb(32)
		case 0x42:
			err = r.sleb(64)
		case 0x43:
			_, err = r.bytes(4)
		case 0x44:
			_, err = r.bytes(8)
		case 0x23, 0xd2:
			_, err = r.u32()
		case 0xd0:
			_, err = r.byte()
		case 0x6a, 0x6b, 0x6c, 0x7c, 0x7d, 0x7e:
			// Extended constant arithmetic: no immediates.
		case 0xfd:
			var sub uint32
			if sub, err = r.u32(); err == nil {
				if sub != 0x0c {
					return errMalformed
				}
				_, err = r.bytes(16)
			}
		default:
			return errMalformed
		}
		if err != nil {
			return err
		}
	}
}

// Structure limits (docs/architecture/05-wasm-plugin-system.md "Default
// limits", Artifact size and structure; targets).
const (
	// maxArtifactBytes caps the binary.
	maxArtifactBytes = 32 << 20
	// maxFunctions caps the defined functions.
	maxFunctions = 100_000
	// maxLocals caps the locals of one function.
	maxLocals = 50_000
	// maxDataBytes caps the data segments' payload.
	maxDataBytes = 16 << 20
)

// structureError is a binary over a structure limit.
type structureError struct{ msg string }

func (e *structureError) Error() string { return e.msg }

// readModule reads the structure of bin. A binary the reader cannot read
// fails with errMalformed (wrapped with where); a binary over a structure
// limit fails with a *structureError, as soon as the limit is passed.
func readModule(bin []byte) (*moduleInfo, error) {
	if len(bin) > maxArtifactBytes {
		return nil, &structureError{fmt.Sprintf("the binary is %d bytes, more than the %d-byte limit", len(bin), maxArtifactBytes)}
	}
	r := &reader{b: bin}
	head, err := r.bytes(8)
	if err != nil || string(head) != "\x00asm\x01\x00\x00\x00" {
		return nil, fmt.Errorf("%w: not a WebAssembly 1 binary", errMalformed)
	}
	m := &moduleInfo{}
	var last byte
	for !r.eof() {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, fmt.Errorf("%w: section %d size", errMalformed, id)
		}
		payload, err := r.bytes(size)
		if err != nil {
			return nil, fmt.Errorf("%w: section %d overruns the binary", errMalformed, id)
		}
		if id != 0 {
			// Sections appear once each, in order (data count, 12, sits
			// between import-free element and code, 9 and 10, and tag,
			// 13, between memory and global).
			if order(id) <= order(last) {
				return nil, fmt.Errorf("%w: section %d out of order", errMalformed, id)
			}
			last = id
		}
		s := &reader{b: payload}
		if err := m.section(id, s); err != nil {
			return nil, err
		}
		if !s.eof() && id != 0 {
			return nil, fmt.Errorf("%w: section %d has trailing bytes", errMalformed, id)
		}
	}
	if m.functions != len(m.funcTypes) {
		return nil, fmt.Errorf("%w: %d function declarations and %d bodies", errMalformed, len(m.funcTypes), m.functions)
	}
	return m, nil
}

// order returns the position of a known section id in the binary order;
// an unknown id is malformed and sorts last.
func order(id byte) int {
	switch id {
	case 1, 2, 3, 4, 5:
		return int(id)
	case 13: // tag
		return 6
	case 6, 7, 8, 9:
		return int(id) + 1
	case 12: // data count
		return 11
	case 10:
		return 12
	case 11:
		return 13
	case 0:
		return 0
	default:
		return 99
	}
}

// section reads one section's payload.
func (m *moduleInfo) section(id byte, r *reader) error {
	switch id {
	case 1:
		return m.typeSection(r)
	case 2:
		return m.importSection(r)
	case 3:
		return m.functionSection(r)
	case 5:
		return m.memorySection(r)
	case 7:
		return m.exportSection(r)
	case 10:
		return m.codeSection(r)
	case 11:
		return m.dataSection(r)
	case 0, 4, 6, 8, 9, 12, 13:
		// Custom, table, global, start, element, data count and tag
		// sections hold nothing the structure check reads.
		r.off = len(r.b)
		return nil
	default:
		return fmt.Errorf("%w: unknown section %d", errMalformed, id)
	}
}

func (m *moduleInfo) typeSection(r *reader) error {
	n, err := r.count(3)
	if err != nil {
		return err
	}
	m.types = make([]funcType, 0, n)
	for range n {
		form, err := r.byte()
		if err != nil {
			return err
		}
		if form != 0x60 {
			return fmt.Errorf("%w: type form 0x%02x", errMalformed, form)
		}
		var t funcType
		for _, dst := range []*[]byte{&t.params, &t.results} {
			k, err := r.count(1)
			if err != nil {
				return err
			}
			v := make([]byte, k)
			for i := range v {
				if v[i], err = r.valType(); err != nil {
					return err
				}
			}
			*dst = v
		}
		m.types = append(m.types, t)
	}
	return nil
}

func (m *moduleInfo) importSection(r *reader) error {
	n, err := r.count(4)
	if err != nil {
		return err
	}
	m.imports = make([]wasmImport, 0, n)
	for range n {
		var im wasmImport
		if im.module, err = r.name(); err != nil {
			return err
		}
		if im.name, err = r.name(); err != nil {
			return err
		}
		if im.kind, err = r.byte(); err != nil {
			return err
		}
		switch im.kind {
		case externFunc:
			t, err := r.u32()
			if err != nil {
				return err
			}
			if t >= uint32(len(m.types)) { //nolint:gosec // G115: types is bounded by the binary's size.
				return fmt.Errorf("%w: import of type %d", errMalformed, t)
			}
			im.typ = m.types[t]
			m.funcImports++
		case externTable:
			if _, err := r.byte(); err != nil {
				return err
			}
			if _, err := r.limits(); err != nil {
				return err
			}
		case externMemory:
			if _, err := r.limits(); err != nil {
				return err
			}
			m.memoryImports++
		case externGlobal:
			if _, err := r.valType(); err != nil {
				return err
			}
			if _, err := r.byte(); err != nil {
				return err
			}
		case externTag:
			if _, err := r.byte(); err != nil {
				return err
			}
			if _, err := r.u32(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: import kind 0x%02x", errMalformed, im.kind)
		}
		m.imports = append(m.imports, im)
	}
	return nil
}

func (m *moduleInfo) functionSection(r *reader) error {
	n, err := r.count(1)
	if err != nil {
		return err
	}
	if n > maxFunctions {
		return &structureError{fmt.Sprintf("the binary defines %d functions, more than the %d-function limit", n, maxFunctions)}
	}
	m.funcTypes = make([]uint32, n)
	for i := range m.funcTypes {
		if m.funcTypes[i], err = r.u32(); err != nil {
			return err
		}
	}
	return nil
}

func (m *moduleInfo) memorySection(r *reader) error {
	n, err := r.count(2)
	if err != nil {
		return err
	}
	for range n {
		mt, err := r.limits()
		if err != nil {
			return err
		}
		m.memories = append(m.memories, mt)
	}
	return nil
}

func (m *moduleInfo) exportSection(r *reader) error {
	n, err := r.count(3)
	if err != nil {
		return err
	}
	m.exports = make([]wasmExport, 0, n)
	for range n {
		var e wasmExport
		if e.name, err = r.name(); err != nil {
			return err
		}
		if e.kind, err = r.byte(); err != nil {
			return err
		}
		if e.index, err = r.u32(); err != nil {
			return err
		}
		m.exports = append(m.exports, e)
	}
	return nil
}

func (m *moduleInfo) codeSection(r *reader) error {
	m.codeSectionBytes = len(r.b)
	n, err := r.count(2)
	if err != nil {
		return err
	}
	if n > maxFunctions {
		return &structureError{fmt.Sprintf("the binary defines %d functions, more than the %d-function limit", n, maxFunctions)}
	}
	m.functions = n
	for i := range n {
		size, err := r.u32()
		if err != nil {
			return err
		}
		body, err := r.bytes(size)
		if err != nil {
			return err
		}
		b := &reader{b: body}
		runs, err := b.count(2)
		if err != nil {
			return err
		}
		var locals uint64
		for range runs {
			k, err := b.u32()
			if err != nil {
				return err
			}
			if _, err := b.valType(); err != nil {
				return err
			}
			locals += uint64(k)
			if locals > maxLocals {
				return &structureError{fmt.Sprintf("function %d declares more than the %d-local limit", i, maxLocals)}
			}
		}
		if locals > m.maxLocals {
			m.maxLocals, m.maxLocalsFunc = locals, i
		}
	}
	return nil
}

func (m *moduleInfo) dataSection(r *reader) error {
	n, err := r.count(2)
	if err != nil {
		return err
	}
	for range n {
		flags, err := r.u32()
		if err != nil {
			return err
		}
		switch flags {
		case 0:
			err = r.constExpr()
		case 1:
		case 2:
			if _, err = r.u32(); err == nil {
				err = r.constExpr()
			}
		default:
			return fmt.Errorf("%w: data segment flags %d", errMalformed, flags)
		}
		if err != nil {
			return err
		}
		k, err := r.u32()
		if err != nil {
			return err
		}
		if _, err := r.bytes(k); err != nil {
			return err
		}
		m.dataBytes += uint64(k)
		if m.dataBytes > maxDataBytes {
			return &structureError{fmt.Sprintf("the data segments hold more than the %d-byte limit", maxDataBytes)}
		}
	}
	return nil
}
