// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

// The wazero study (docs/architecture/05-wasm-plugin-system.md "wazero
// study"): the behavior of the pinned wazero version that the runtime
// rules rest on, measured as tests so a version bump re-runs it. The
// compiled-code measurements read the process's executable mappings and
// run on Linux (study_linux_test.go); the rest run everywhere.

import (
	"context"
	"errors"
	"runtime"
	"runtime/metrics"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/sys"

	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
)

// studyConfig is the runtime configuration ruralzd runs (ADR-0004):
// compiler mode, the shared in-memory cache, a page limit with the full
// capacity reserved, and interruption on context done.
func studyConfig(cache wazero.CompilationCache, pages uint32) wazero.RuntimeConfig {
	return wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(cache).
		WithMemoryLimitPages(pages).
		WithMemoryCapacityFromMax(true).
		WithCloseOnContextDone(true).
		WithDebugInfoEnabled(false)
}

// manyFunctions returns a module of n functions, each returning its index
// plus a constant, so no two modules of different n share bytes.
func manyFunctions(n int, salt int32) []byte {
	var m ht.Module
	sig := ht.Sig(nil, ht.I32)
	for i := range n {
		f := m.Func(sig, nil, ht.I32Const(int32(i)+salt), ht.I32Const(salt), ht.Op(ht.OpI32Add))
		if i == 0 {
			m.ExportFunc("f0", f)
		}
	}
	return m.Encode()
}

// TestStudyCompileChecksContextPerFunctionWithTwoWorkers: with two or more
// compilation workers, wazero checks the compile context before it takes
// each function and returns the context's error, so a deadline cancels a
// compile between functions; with one worker it never checks and the
// compile runs to its end.
func TestStudyCompileChecksContextPerFunctionWithTwoWorkers(t *testing.T) {
	bin := manyFunctions(64, 7)
	cases := []struct {
		name    string
		workers int
		wantErr bool
	}{
		{name: "one worker compiles a canceled compile to its end", workers: 1},
		{name: "two workers stop a canceled compile", workers: 2, wantErr: true},
		{name: "four workers stop a canceled compile", workers: 4, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := wazero.NewCompilationCache()
			defer func() { _ = cache.Close(context.Background()) }()
			rt := wazero.NewRuntimeWithConfig(t.Context(), studyConfig(cache, 1))
			defer func() { _ = rt.Close(context.Background()) }()
			ctx, cancel := context.WithCancel(experimental.WithCompilationWorkers(t.Context(), tc.workers))
			cancel()
			cm, err := rt.CompileModule(ctx, bin)
			if tc.wantErr {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("CompileModule(canceled, %d workers) = %v, want context.Canceled", tc.workers, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CompileModule(canceled, %d workers) = %v, want a compiled module", tc.workers, err)
			}
			_ = cm.Close(context.Background())
		})
	}
}

// TestStudyGuestLoopDoesNotHoldGarbageCollection: under
// WithCloseOnContextDone, compiled loops leave native code at every loop
// header to check the module's exit code in Go, so a garbage collection
// started while a guest loops forever completes without waiting for the
// call, which then ends on its context.
func TestStudyGuestLoopDoesNotHoldGarbageCollection(t *testing.T) {
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	ctx := t.Context()
	rt := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, 1))
	defer func() { _ = rt.Close(context.Background()) }()

	entered := make(chan struct{})
	_, err := rt.NewHostModuleBuilder("study").NewFunctionBuilder().
		WithGoFunction(api.GoFunc(func(context.Context, []uint64) { close(entered) }), nil, nil).
		Export("entered").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m ht.Module
	enter := m.ImportFunc("study", "entered", ht.Sig(nil))
	spin := m.Func(ht.Sig(nil), nil, ht.Call(enter), ht.Loop(ht.BlockEmpty), ht.Br(0), ht.End())
	m.ExportFunc("spin", spin)
	mod, err := rt.InstantiateWithConfig(ctx, m.Encode(), wazero.NewModuleConfig().WithStartFunctions())
	if err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := mod.ExportedFunction("spin").Call(callCtx)
		done <- err
	}()
	<-entered
	before := gcCycles()
	runtime.GC()
	if after := gcCycles(); after <= before {
		t.Fatalf("GC cycles %d after runtime.GC, want more than %d", after, before)
	}
	select {
	case err := <-done:
		t.Fatalf("guest returned %v before its context was canceled", err)
	default:
	}
	cancel()
	err = <-done
	var exit *sys.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != sys.ExitCodeContextCanceled {
		t.Fatalf("spin after cancel = %v, want exit code %d", err, sys.ExitCodeContextCanceled)
	}
	if !mod.IsClosed() {
		t.Error("the module is open after its call ended on its context, want closed")
	}
}

// gcCycles returns the completed GC cycles.
func gcCycles() uint64 {
	s := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// heapObjectBytes returns the live heap object bytes after a collection.
func heapObjectBytes() uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// TestStudyLinearMemoryIsGoHeap: with WithMemoryCapacityFromMax, each
// instance's linear memory is one Go heap allocation of its full limit at
// instantiation, so reserved Plugin memory counts toward GOMEMLIMIT and
// the GOGC heap goal whether or not the guest touches it.
func TestStudyLinearMemoryIsGoHeap(t *testing.T) {
	const pages, instances = 256, 4 // 16 MiB each
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	ctx := t.Context()
	rt := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, pages))
	defer func() { _ = rt.Close(context.Background()) }()
	var m ht.Module
	m.Memory(1, 0, false)
	cm, err := rt.CompileModule(ctx, m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	before := heapObjectBytes()
	mods := make([]api.Module, 0, instances)
	for range instances {
		mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName("").WithStartFunctions())
		if err != nil {
			t.Fatal(err)
		}
		mods = append(mods, mod)
	}
	after := heapObjectBytes()
	const want = instances * pages * 65536
	if after < before || after-before < want-(1<<20) {
		t.Errorf("live heap was %d bytes after %d instances of %d pages and %d before, want at least %d more", after, instances, pages, before, want)
	}
	for _, mod := range mods {
		if got := mod.Memory().Size(); got != 65536 {
			t.Errorf("Memory().Size() = %d, want one page in use", got)
		}
		_ = mod.Close(ctx)
	}
	runtime.KeepAlive(mods)
}

// TestStudyDeclaredMemoryAboveTheLimitFailsCompile: under
// WithMemoryCapacityFromMax, a module whose declared minimum or declared
// maximum exceeds the page limit fails CompileModule, and a module that
// declares no maximum gets the limit as its maximum.
func TestStudyDeclaredMemoryAboveTheLimitFailsCompile(t *testing.T) {
	const limit = 16
	cases := []struct {
		name     string
		min, max uint32
		hasMax   bool
		wantErr  bool
		wantPage uint32
	}{
		{name: "no maximum takes the limit", min: 1, wantPage: limit},
		{name: "maximum within the limit", min: 1, max: 8, hasMax: true, wantPage: 8},
		{name: "maximum at the limit", min: 1, max: limit, hasMax: true, wantPage: limit},
		{name: "maximum above the limit", min: 1, max: limit + 1, hasMax: true, wantErr: true},
		{name: "maximum of 4 GiB", min: 1, max: 65536, hasMax: true, wantErr: true},
		{name: "minimum above the limit", min: limit + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := wazero.NewCompilationCache()
			defer func() { _ = cache.Close(context.Background()) }()
			ctx := t.Context()
			rt := wazero.NewRuntimeWithConfig(ctx, studyConfig(cache, limit))
			defer func() { _ = rt.Close(context.Background()) }()
			var m ht.Module
			m.Memory(tc.min, tc.max, tc.hasMax)
			m.Export("memory", ht.KindMemory, 0)
			cm, err := rt.CompileModule(ctx, m.Encode())
			if tc.wantErr {
				if err == nil {
					t.Fatal("CompileModule succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			def := cm.ExportedMemories()["memory"]
			if got, encoded := def.Max(); got != tc.wantPage || encoded != tc.hasMax {
				t.Errorf("Max() = %d, %v, want %d, %v", got, encoded, tc.wantPage, tc.hasMax)
			}
		})
	}
}
