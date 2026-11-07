// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Source fetches a Plugin artifact's guest binary by its image reference
// and digest: from the Node's artifact cache, or pulled from the registry
// (file mode) or through Ruralz Control's relay (Control mode), its digest
// and signature verified before it returns
// (docs/architecture/05-wasm-plugin-system.md "Hot-swap lifecycle", steps
// 1 and 2). A failure carries its RZ code: RZ-CFG-028 for an unavailable
// artifact or a digest mismatch, RZ-CFG-033 for a signature; a registry
// outage or a timeout is transient (IsTransient).
type Source interface {
	Fetch(ctx context.Context, image string, digest revision.Digest) ([]byte, error)
}

// Options configure an Engine. Every field may be left zero.
type Options struct {
	// Clock times calls, waits, breakers and limiters; nil uses
	// clock.Real().
	Clock clock.Clock
	// Logger receives guest logs and the host's own; nil discards them.
	Logger *slog.Logger
	// Status receives the degraded reasons plugin_pool_degraded,
	// plugin_parked_bound_low and plugin_memlimit; nil reports none.
	Status emit.NodeStatus
	// Rand is the host random source of random_bytes, random_get and key
	// signing; nil uses crypto/rand.
	Rand io.Reader
	// Procs is GOMAXPROCS; 0 reads runtime.GOMAXPROCS.
	Procs int
	// Source fetches artifacts; nil fails every fetch with RZ-CFG-028.
	Source Source
	// MemLimit returns the Go runtime's memory limit (GOMEMLIMIT); nil
	// reads runtime/debug.SetMemoryLimit.
	MemLimit func() int64
	// Measure returns the native code a compile mapped, for the measured
	// overrun check; nil measures on Linux and nowhere else.
	Measure func() (int64, bool)
}

// Engine is the Node's Plugin host: the wazero runtimes and compiled code
// of every Plugin artifact a live snapshot holds, the instance pools, the
// pool manager that fills them off the request path, and the Node's Plugin
// memory accounting (docs/architecture/05-wasm-plugin-system.md
// "Runtime"). One Engine serves every snapshot of the process; ruralzd
// runs it as a filter.Component, and `ruralz plugin test` links the same
// host. It is safe for concurrent use.
type Engine struct {
	clk     clock.Clock
	log     *slog.Logger
	status  emit.NodeStatus
	rand    io.Reader
	procs   int
	source  Source
	memLim  func() int64
	measure func() (int64, bool)
	started time.Time
	// supported is false when wazero's compiler does not run here: no
	// Plugin activates.
	supported bool

	mu      sync.Mutex
	digests map[revision.Digest]*digestEntry
	pools   map[poolKey]*pool
	// nodeCap is the Node Plugin memory cap of the newest activation;
	// reserved the linear memory every live instance reserves.
	nodeCap  int64
	reserved int64
	// code is the accounted compiled code of every retained digest.
	code int64

	// compile admits one cold compile at a time; abandoned counts the
	// compiles past their deadline still running.
	compile   chan struct{}
	abandoned int

	// work queues pools for the pool manager.
	work chan *pool
}

// NewEngine returns an Engine; its pool manager starts with Run.
func NewEngine(o Options) *Engine {
	e := &Engine{
		clk: o.Clock, log: o.Logger, status: o.Status, rand: o.Rand, procs: o.Procs, source: o.Source,
		memLim: o.MemLimit, measure: o.Measure,
		digests: map[revision.Digest]*digestEntry{},
		pools:   map[poolKey]*pool{},
		compile: make(chan struct{}, 1),
		nodeCap: DefaultNodeCap,
	}
	if e.clk == nil {
		e.clk = clock.Real()
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.rand == nil {
		e.rand = rand.Reader
	}
	if e.procs <= 0 {
		e.procs = runtime.GOMAXPROCS(0)
	}
	if e.memLim == nil {
		e.memLim = goMemLimit
	}
	if e.measure == nil {
		e.measure = measureCode
	}
	e.started = e.clk.Now()
	e.supported = compilerSupported()
	// The work queue holds every pool at most once (pool.queued), and a
	// Node holds few pools; its capacity only bounds a burst.
	e.work = make(chan *pool, 4096)
	return e
}

// Name implements filter.Component.
func (*Engine) Name() string { return "plugin host" }

// managerWorkers is the pool manager's worker count: max(1, GOMAXPROCS/2).
func (e *Engine) managerWorkers() int { return max(1, e.procs/2) }

// compileWorkers is wazero's compilation workers: at most max(1,
// GOMAXPROCS/4).
func (e *Engine) compileWorkers() int { return max(1, e.procs/4) }

// Run serves the pool manager until ctx is done, then closes every pool
// and runtime, and returns once its workers exited.
func (e *Engine) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range e.managerWorkers() {
		wg.Go(func() { e.manage(ctx) })
	}
	<-ctx.Done()
	wg.Wait()
	e.closeAll(ctx)
	return nil
}

// manage is one pool manager worker: it fills queued pools.
func (e *Engine) manage(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-e.work:
			p.fill(ctx)
		}
	}
}

// randomBytes fills b from the host random source.
func (e *Engine) randomBytes(b []byte) {
	if _, err := io.ReadFull(e.rand, b); err != nil {
		panic(fmt.Errorf("pluginhost: host random source: %w", err))
	}
}

// compilerSupported reports whether wazero's compiler runs on this
// platform: amd64 and arm64, the production platforms. Without it no
// Plugin activates.
func compilerSupported() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return false
	}
	cache := wazero.NewCompilationCache()
	defer func() { _ = cache.Close(context.Background()) }()
	rt := wazero.NewRuntimeWithConfig(context.Background(), wazero.NewRuntimeConfigCompiler().WithCompilationCache(cache))
	_ = rt.Close(context.Background())
	return true
}

// digestEntry is one Plugin artifact's compiled code: its binary, its
// structure, the compilation cache of its own that every runtime of the
// digest shares, and a runtime per limits.memoryBytes. The wazero study
// fixed one cache per digest: the cache keys compiled code by the binary,
// whatever the runtime's page limit, so the digest compiles once, and
// closing the digest's runtimes and cache at retire frees its code at the
// next garbage collection, which CompiledModule.Close on a cache shared by
// every digest does not guarantee.
type digestEntry struct {
	digest revision.Digest
	bin    []byte
	cache  wazero.CompilationCache
	// runtimes are the digest's runtimes by page limit.
	runtimes map[uint32]*runtimeEntry
	// refs counts the pools holding the digest.
	refs int
	// accounted is the digest's accounted compiled code.
	accounted int64
	// ready closes once the first compile ended, with err its failure.
	ready chan struct{}
	err   error
}

// runtimeEntry is one wazero runtime of a digest at one page limit.
type runtimeEntry struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	pages    uint32
}

// runtimeConfig is the configuration ADR-0004 fixes: compiler mode, the
// digest's in-memory cache, the page limit with its whole capacity
// reserved at instantiation, interruption on context done, the
// WebAssembly 2.0 features and no DWARF.
func runtimeConfig(cache wazero.CompilationCache, pages uint32) wazero.RuntimeConfig {
	return wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(cache).
		WithMemoryLimitPages(pages).
		WithMemoryCapacityFromMax(true).
		WithCloseOnContextDone(true).
		WithCoreFeatures(api.CoreFeaturesV2).
		WithDebugInfoEnabled(false)
}

// compiled returns the runtime of d at pages, compiling d once: the first
// compile runs in the compile queue under the cold compilation limit; a
// later page limit compiles from the digest's cache.
func (e *Engine) compiled(ctx context.Context, d *digestEntry, pages uint32) (*runtimeEntry, error) {
	e.mu.Lock()
	if r, ok := d.runtimes[pages]; ok {
		e.mu.Unlock()
		return r, nil
	}
	e.mu.Unlock()
	r, err := e.compileRuntime(ctx, d, pages)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if have, ok := d.runtimes[pages]; ok {
		_ = r.rt.Close(context.WithoutCancel(ctx))
		return have, nil
	}
	d.runtimes[pages] = r
	return r, nil
}

// errCompileDeadline is a cold compile past its 30 s limit.
var errCompileDeadline = errors.New("the compile did not finish within the cold compilation limit")

// compileRuntime makes a runtime of d at pages and compiles d in it: one
// compile at a time, abandoned after the cold compilation limit when no
// other abandoned compile still runs, and retried twice before the
// transient failure is returned.
func (e *Engine) compileRuntime(ctx context.Context, d *digestEntry, pages uint32) (*runtimeEntry, error) {
	var last error
	for range 1 + compileRetries {
		r, err := e.compileOnce(ctx, d, pages)
		if err == nil || !errors.Is(err, errCompileDeadline) {
			return r, err
		}
		last = err
	}
	return nil, failTransient(CodeCompile, "Plugin artifact %s: %w", d.digest, last)
}

// compileOnce is one compile attempt.
func (e *Engine) compileOnce(ctx context.Context, d *digestEntry, pages uint32) (*runtimeEntry, error) {
	select {
	case e.compile <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	held := true
	release := func() {
		if held {
			held = false
			<-e.compile
		}
	}
	defer release()
	// The runtime outlives the activation that compiles it, and an
	// abandoned compile outlives the attempt: neither ends with ctx.
	bg := context.WithoutCancel(ctx)
	rt := wazero.NewRuntimeWithConfig(bg, runtimeConfig(d.cache, pages))
	if err := instantiateHost(bg, rt); err != nil {
		_ = rt.Close(bg)
		return nil, fmt.Errorf("pluginhost: host modules: %w", err)
	}
	cctx, cancel := context.WithCancel(experimental.WithCompilationWorkers(bg, e.compileWorkers()))
	type result struct {
		cm  wazero.CompiledModule
		err error
	}
	done := make(chan result, 1)
	go func() {
		cm, err := rt.CompileModule(cctx, d.bin)
		done <- result{cm, err}
	}()
	timer := e.clk.NewTimer(coldCompileTimeout)
	defer timer.Stop()
	var stopped error
	select {
	case r := <-done:
		cancel()
		if r.err != nil {
			_ = rt.Close(bg)
			return nil, fail(CodeCompile, "Plugin artifact %s does not compile: %s", d.digest, clip(r.err.Error()))
		}
		return &runtimeEntry{rt: rt, compiled: r.cm, pages: pages}, nil
	case <-timer.C():
		stopped = errCompileDeadline
	case <-ctx.Done():
		stopped = ctx.Err()
	}
	// The cancel stops a compile between functions when it runs on two or
	// more workers; a function in progress runs to its end. The compile
	// becomes the one abandoned compile a Node allows beside the next, or,
	// when one already runs, keeps the compile slot until it ends, so no
	// more than two compiles ever run.
	cancel()
	e.mu.Lock()
	abandon := e.abandoned < maxAbandoned
	if abandon {
		e.abandoned++
	}
	e.mu.Unlock()
	keepSlot := !abandon
	held = !keepSlot
	go func() {
		r := <-done
		if r.cm != nil {
			_ = r.cm.Close(bg)
		}
		_ = rt.Close(bg)
		if keepSlot {
			<-e.compile
			return
		}
		e.mu.Lock()
		e.abandoned--
		e.mu.Unlock()
	}()
	return nil, stopped
}

// clip shortens another component's error text for a diagnostic (512
// bytes, docs/architecture/02-configuration-model.md "Semantic
// validation").
func clip(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// closeAll closes every pool and runtime at shutdown.
func (e *Engine) closeAll(ctx context.Context) {
	e.mu.Lock()
	pools := make([]*pool, 0, len(e.pools))
	for _, p := range e.pools {
		pools = append(pools, p)
	}
	e.mu.Unlock()
	for _, p := range pools {
		p.closeInstances(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, d := range e.digests {
		d.close(ctx)
		delete(e.digests, k)
	}
	e.pools = map[poolKey]*pool{}
	e.code = 0
}

// close closes the digest's runtimes and cache, whether or not ctx is
// done.
func (d *digestEntry) close(ctx context.Context) {
	bg := context.WithoutCancel(ctx)
	for _, r := range d.runtimes {
		if r.compiled != nil {
			_ = r.compiled.Close(bg)
		}
		_ = r.rt.Close(bg)
	}
	d.runtimes = map[uint32]*runtimeEntry{}
	if d.cache != nil {
		_ = d.cache.Close(bg)
	}
}
