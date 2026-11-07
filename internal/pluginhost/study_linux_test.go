// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package pluginhost

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
)

// span is one mapping [start, end).
type span struct{ start, end uint64 }

// execSpans returns the anonymous executable mappings of the process, the
// native code wazero compiles: the Go runtime maps no anonymous executable
// memory, and the vDSO carries a name.
func execSpans(t *testing.T) []span {
	t.Helper()
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read /proc/self/maps: %v", err)
	}
	var out []span
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || len(f[1]) < 3 || f[1][2] != 'x' || f[4] != "0" || len(f) > 5 {
			continue
		}
		lo, hi, ok := strings.Cut(f[0], "-")
		if !ok {
			continue
		}
		s, err1 := strconv.ParseUint(lo, 16, 64)
		e, err2 := strconv.ParseUint(hi, 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, span{s, e})
	}
	return out
}

// total returns the bytes of spans.
func total(spans []span) uint64 {
	var n uint64
	for _, s := range spans {
		n += s.end - s.start
	}
	return n
}

// sentinel is a finalizer barrier object.
type sentinel struct {
	p *int
	_ [32]byte
}

// barrier collects garbage, then queues a sentinel's finalizer in a second
// collection and waits for it to run. The finalizer goroutine runs one
// queued batch to its end before it takes the next, newest first within a
// batch, so two barriers in a row return only after every finalizer queued
// before the first one has run.
func barrier() {
	runtime.GC()
	done := make(chan struct{})
	s := &sentinel{p: new(int)}
	runtime.SetFinalizer(s, func(*sentinel) { close(done) })
	s = nil //nolint:ineffassign,wastedassign // drops the only reference so the sentinel becomes garbage.
	runtime.GC()
	<-done
}

// quiesce collects garbage and runs every pending finalizer, wazero's
// munmap of unreachable compiled code among them, until the executable
// mappings stop changing, and returns them.
func quiesce(t *testing.T) []span {
	t.Helper()
	barrier()
	barrier()
	prev := execSpans(t)
	for range 64 {
		barrier()
		barrier()
		cur := execSpans(t)
		if slices.Equal(cur, prev) {
			return cur
		}
		prev = cur
	}
	t.Fatal("executable mappings kept changing")
	return nil
}

// TestStudyCompiledCodeIsSharedAcrossMemoryLimits: the compilation cache
// keys compiled code by the SHA-256 of the binary (wazero's module ID),
// not by the runtime's page limit, so runtimes with different
// limits.memoryBytes that share one cache compile a digest once: the
// second compile maps no executable memory.
func TestStudyCompiledCodeIsSharedAcrossMemoryLimits(t *testing.T) {
	ctx := t.Context()
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	small := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 16))
	defer func() { _ = small.Close(context.Background()) }()
	large := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 256))
	defer func() { _ = large.Close(context.Background()) }()
	bin := manyFunctions(200, 101)

	base := total(quiesce(t))
	a, err := small.CompileModule(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	first := total(quiesce(t))
	if first <= base {
		t.Fatalf("executable mappings were %d bytes after the first compile and %d before it, want more after", first, base)
	}
	b, err := large.CompileModule(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	if second := total(quiesce(t)); second != first {
		t.Errorf("executable mappings were %d bytes after the second compile, at another page limit, and %d before it, want no change", second, first)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

// compileAndRun compiles bin in rt, instantiates it once and closes the
// instance, returning the compiled module, still open.
func compileAndRun(t *testing.T, rt wazero.Runtime, bin []byte) wazero.CompiledModule {
	t.Helper()
	ctx := t.Context()
	cm, err := rt.CompileModule(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		t.Fatal(err)
	}
	if err := mod.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return cm
}

// TestStudyCloseLeavesTheLastClosedModuleMappedUntilTheCacheCloses:
// CompiledModule.Close drops the cache's map entry of unshared code, but
// wazero's engine keeps its compiled modules in a slice sorted by code
// address and deletes by shifting the tail down without clearing the
// vacated slot, so the backing array still references the module that sat
// last; with every module and instance closed, the last module closed
// stays mapped for as long as its compilation cache lives, and closing the
// cache unmaps everything at the next collection.
func TestStudyCloseLeavesTheLastClosedModuleMappedUntilTheCacheCloses(t *testing.T) {
	ctx := t.Context()
	cache := wazero.NewCompilationCache()
	rt := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 1))
	base := total(quiesce(t))
	a := compileAndRun(t, rt, manyFunctions(300, 202))
	b := compileAndRun(t, rt, manyFunctions(300, 404))
	compiled := total(quiesce(t))
	if compiled <= base {
		t.Fatalf("executable mappings were %d bytes after compiling and %d before, want more after", compiled, base)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := total(quiesce(t)); got == base {
		t.Error("after closing every module, no compiled code stays mapped, want the last closed module's")
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := total(quiesce(t)); got >= base {
		t.Errorf("executable mappings were %d bytes after the runtime and cache closed and %d with them open before any compile, want fewer after", got, base)
	}
}

// TestStudyClosingADigestCacheFreesItsCode: a compilation cache per digest,
// shared by that digest's runtimes, frees exactly that digest's code when
// its runtimes and cache close, while another digest's cache keeps its own.
func TestStudyClosingADigestCacheFreesItsCode(t *testing.T) {
	ctx := t.Context()
	keepCache := wazero.NewCompilationCache()
	defer func() { _ = keepCache.Close(context.Background()) }()
	keep := wazero.NewRuntimeWithConfig(ctx, studyConfig(keepCache, 1))
	defer func() { _ = keep.Close(context.Background()) }()
	kept := compileAndRun(t, keep, manyFunctions(300, 505))
	base := total(quiesce(t))

	cache := wazero.NewCompilationCache()
	small := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 1))
	large := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 16))
	bin := manyFunctions(300, 606)
	x := compileAndRun(t, small, bin)
	y := compileAndRun(t, large, bin)
	if total(quiesce(t)) <= base {
		t.Fatal("compiling the digest mapped no executable bytes")
	}
	for _, c := range []interface{ Close(context.Context) error }{x, y, small, large, cache} {
		if err := c.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := total(quiesce(t)); got != base {
		t.Errorf("executable mappings were %d bytes after the digest's runtimes and cache closed and %d with the other digest's alone, want equal", got, base)
	}
	runtime.KeepAlive(kept)
}

// corpusFile is one toolchain-built binary of wazero's module testdata.
type corpusFile struct {
	toolchain, path string
}

// toolchainCorpus lists Rust, TinyGo, Zig and C binaries the wazero
// module ships as test data, built by real guest toolchains.
func toolchainCorpus() []corpusFile {
	return []corpusFile{
		{"tinygo", "imports/wasi_snapshot_preview1/example/testdata/tinygo/cat.wasm"},
		{"tinygo", "imports/wasi_snapshot_preview1/testdata/tinygo/wasi.wasm"},
		{"tinygo", "examples/allocation/tinygo/testdata/greet.wasm"},
		{"tinygo", "internal/testing/dwarftestdata/testdata/tinygo/main.wasm"},
		{"rust", "imports/wasi_snapshot_preview1/example/testdata/cargo-wasi/cat.wasm"},
		{"rust", "imports/wasi_snapshot_preview1/testdata/cargo-wasi/wasi.wasm"},
		{"rust", "examples/allocation/rust/testdata/greet.wasm"},
		{"zig", "imports/wasi_snapshot_preview1/example/testdata/zig/cat.wasm"},
		{"zig", "imports/wasi_snapshot_preview1/testdata/zig/wasi.wasm"},
		{"zig", "examples/allocation/zig/testdata/greet.wasm"},
		{"zig-cc", "imports/wasi_snapshot_preview1/example/testdata/zig-cc/cat.wasm"},
		{"zig-cc", "imports/wasi_snapshot_preview1/testdata/zig-cc/wasi.wasm"},
	}
}

// wazeroDir returns the directory of the wazero module this build uses.
func wazeroDir(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not on PATH; the toolchain corpus lives in the wazero module directory")
	}
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/tetratelabs/wazero").Output()
	if err != nil {
		t.Fatalf("go list -m github.com/tetratelabs/wazero: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// codeBytes returns the bytes of the code section of a binary module.
func codeBytes(bin []byte) int {
	info, err := readModule(bin)
	if err != nil {
		return 0
	}
	return info.codeSectionBytes
}

// compiledBytes compiles bin in a fresh cache and returns the executable
// bytes the compile mapped.
func compiledBytes(t *testing.T, bin []byte) uint64 {
	t.Helper()
	ctx := t.Context()
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	// 4096 pages, the proposed 256 MiB maximum: Zig sets a 257-page
	// minimum.
	rt := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 4096).WithCoreFeatures(api.CoreFeaturesV2))
	defer func() { _ = rt.Close(context.Background()) }()
	before := total(quiesce(t))
	cm, err := rt.CompileModule(ctx, bin)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	after := total(quiesce(t))
	runtime.KeepAlive(cm)
	if after < before {
		return 0
	}
	return after - before
}

// TestStudyCompiledCodePerWasmByte measures the native code wazero maps
// per byte of binary for guests built by real toolchains, the measure of
// the accounting rule (accountedCode): each compile maps its code and its
// entry preambles, each rounded up to whole pages, so a small binary pays
// the rounding, and every toolchain binary stays within codeFactor × its
// bytes plus codeAllowance on amd64 and arm64, while 4 × its bytes alone
// is passed by TinyGo and Zig binaries.
func TestStudyCompiledCodePerWasmByte(t *testing.T) {
	dir := wazeroDir(t)
	for _, c := range toolchainCorpus() {
		t.Run(c.toolchain+"/"+filepath.Base(filepath.Dir(c.path))+"/"+filepath.Base(c.path), func(t *testing.T) {
			bin, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.path))) //nolint:gosec // G304: a fixed path inside the wazero module directory.
			if err != nil {
				t.Fatal(err)
			}
			got := compiledBytes(t, bin)
			code := codeBytes(bin)
			t.Logf("%s %s on %s: %d binary bytes, %d code section bytes, %d executable bytes: %.2f per binary byte, %.2f per code byte",
				c.toolchain, c.path, runtime.GOARCH, len(bin), code, got,
				float64(got)/float64(len(bin)), float64(got)/float64(max(code, 1)))
			if want := accountedCode(len(bin)); int64(got) > want { //nolint:gosec // G115: mapping sizes fit int64.
				t.Errorf("%d executable bytes for %d binary bytes, want at most the accounted %d", got, len(bin), want)
			}
		})
	}
}

// TestStudyHostileShapesExceedTheAccountedCode: instruction mixes no
// compiler emits in bulk expand far more than toolchain output (bulk
// memory fills and indirect calls about 32 and 29 times their bytes), so
// the accounted size bounds only ordinary binaries, and a Node also
// measures the code each compile maps (measuredCode) against the limit.
func TestStudyHostileShapesExceedTheAccountedCode(t *testing.T) {
	var worst float64
	for _, s := range syntheticShapes() {
		bin := s.build()
		got := compiledBytes(t, bin)
		ratio := float64(got) / float64(len(bin))
		t.Logf("synthetic %s on %s: %d binary bytes, %d executable bytes: %.2f per binary byte", s.name, runtime.GOARCH, len(bin), got, ratio)
		if int64(got) > accountedCode(len(bin)) { //nolint:gosec // G115: mapping sizes fit int64.
			worst = max(worst, ratio)
		}
	}
	if worst < 2*codeFactor {
		t.Errorf("no synthetic shape passed twice the accounting factor (worst %.2f), want the hostile expansion the measured check exists for", worst)
	}
}

// shape is one synthetic binary of the study.
type shape struct {
	name  string
	build func() []byte
}

// syntheticShapes are instruction mixes that expand more than compiled
// language output: bounds-checked loads, indirect calls, divisions and
// many tiny functions.
func syntheticShapes() []shape {
	const n = 20000
	oneFunc := func(body ...[]byte) []byte {
		var m ht.Module
		m.Memory(1, 0, false)
		m.Func(ht.Sig(ht.Params(ht.I32), ht.I32), nil, body...)
		return m.Encode()
	}
	return []shape{
		{"loads", func() []byte {
			return oneFunc(ht.Repeat(n, ht.LocalGet(0), ht.I32Load(4), ht.LocalSet(0)), ht.LocalGet(0))
		}},
		{"stores", func() []byte {
			return oneFunc(ht.Repeat(n, ht.LocalGet(0), ht.LocalGet(0), ht.I32Store(8)), ht.LocalGet(0))
		}},
		{"divisions", func() []byte {
			return oneFunc(ht.Repeat(n, ht.LocalGet(0), ht.LocalGet(0), ht.Op(ht.OpI32DivS), ht.LocalSet(0)), ht.LocalGet(0))
		}},
		{"indirect calls", func() []byte {
			var m ht.Module
			sig := ht.Sig(ht.Params(ht.I32), ht.I32)
			callee := m.Func(sig, nil, ht.LocalGet(0))
			m.Table(1)
			m.Elements(callee)
			ti := m.Type(sig)
			m.Func(sig, nil, ht.Repeat(n, ht.LocalGet(0), ht.I32Const(0), ht.CallIndirect(ti), ht.LocalSet(0)), ht.LocalGet(0))
			return m.Encode()
		}},
		{"tiny functions", func() []byte { return manyFunctions(n, 303) }},
		{"memory fills", func() []byte {
			return oneFunc(ht.Repeat(n, ht.LocalGet(0), ht.I32Const(0), ht.I32Const(16), ht.MemoryFill()), ht.LocalGet(0))
		}},
	}
}
