// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/tetratelabs/wazero"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// goMemLimit reads the Go runtime's memory limit without changing it.
func goMemLimit() int64 { return debug.SetMemoryLimit(-1) }

// setCap records the Node Plugin memory cap of the newest activation and
// re-checks plugin_memlimit.
func (e *Engine) setCap(c int64) {
	if c <= 0 {
		c = DefaultNodeCap
	}
	e.mu.Lock()
	e.nodeCap = c
	e.mu.Unlock()
	e.checkMemLimit()
}

// reserve takes n bytes of the Node Plugin memory cap for one instance;
// warm allows the 10% swap headroom a swap's warming may use.
func (e *Engine) reserve(n int64, warm bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	limit := e.nodeCap
	if warm {
		limit += e.nodeCap * swapHeadroom / 100
	}
	if e.reserved+n > limit {
		return false
	}
	e.reserved += n
	return true
}

// unreserve returns n bytes and wakes every pool a full cap refused.
func (e *Engine) unreserve(n int64) {
	e.mu.Lock()
	e.reserved -= n
	var refused []*pool
	for _, p := range e.pools {
		p.mu.Lock()
		if p.capRefused {
			refused = append(refused, p)
		}
		p.mu.Unlock()
	}
	e.mu.Unlock()
	for _, p := range refused {
		p.wake()
	}
}

// Reserved returns the Plugin memory every live instance reserves.
func (e *Engine) Reserved() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reserved
}

// poolHealthChanged recomputes plugin_pool_degraded: raised while a pool's
// fault breaker is open or its last background instantiation failed.
func (e *Engine) poolHealthChanged() {
	if e.status == nil {
		return
	}
	e.mu.Lock()
	pools := make([]*pool, 0, len(e.pools))
	for _, p := range e.pools {
		pools = append(pools, p)
	}
	e.mu.Unlock()
	degraded := false
	for _, p := range pools {
		if p.degraded() {
			degraded = true
			break
		}
	}
	e.status.SetDegraded(catalog.ReasonPluginPoolDegraded, "pluginhost", degraded)
}

// checkMemLimit raises plugin_memlimit while Plugins are active and
// GOMEMLIMIT is unset or below the Plugin memory cap plus M_fix
// (docs/operations/03-capacity-planning.md "Formulas"; ADR-0004).
func (e *Engine) checkMemLimit() {
	if e.status == nil {
		return
	}
	e.mu.Lock()
	active := len(e.pools) > 0
	nodeCap := e.nodeCap
	e.mu.Unlock()
	lim := e.memLim()
	low := lim <= 0 || lim == unsetMemLimit || lim < nodeCap+fixedMemory
	e.status.SetDegraded(catalog.ReasonPluginMemlimit, "pluginhost", active && low)
}

// unsetMemLimit is the Go runtime's limit without GOMEMLIMIT: math.MaxInt64.
const unsetMemLimit = 1<<63 - 1

// fixedMemory is M_fix, the Node's memory outside Plugins and bodies that
// the capacity planning formulas add (docs/operations/03-capacity-
// planning.md "Formulas").
const fixedMemory = 512 << 20

// digest returns the digest entry of a Plugin, fetching, verifying and
// checking its artifact on first use: the Source fetches and verifies by
// digest and signature, the structure check and the Plugin.spec checks
// run before any compile, and the digest's accounted compiled code is
// recorded. The entry's refs count its pools; a new entry has none.
func (e *Engine) digest(ctx context.Context, name string, spec *v1alpha1.PluginSpec, pages uint32) (*digestEntry, *artifact, error) {
	if !e.supported {
		return nil, nil, failTransient(CodeArtifact, "Plugin %q: wazero's compiler does not run on this platform", name)
	}
	d, err := imageDigest(spec.Image)
	if err != nil {
		return nil, nil, fail(CodeArtifact, "Plugin %q: %v", name, err)
	}
	e.mu.Lock()
	entry, ok := e.digests[d]
	if !ok {
		entry = &digestEntry{digest: d, runtimes: map[uint32]*runtimeEntry{}, ready: make(chan struct{})}
		e.digests[d] = entry
		e.mu.Unlock()
		entry.err = e.load(ctx, entry, spec.Image)
		close(entry.ready)
		if entry.err != nil {
			e.mu.Lock()
			if e.digests[d] == entry {
				delete(e.digests, d)
			}
			e.mu.Unlock()
		}
	} else {
		e.mu.Unlock()
		select {
		case <-entry.ready:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if entry.err != nil {
		return nil, nil, rename(entry.err, name)
	}
	a, err := checkArtifact(entry.bin, spec, pages)
	if err != nil {
		return nil, nil, rename(err, name)
	}
	return entry, a, nil
}

// rename prefixes an activation failure with the Plugin's name, keeping
// its code and class.
func rename(err error, name string) error {
	code, _ := errcode.CodeOf(err)
	var f *failure
	if errors.As(err, &f) {
		return errcode.Wrap(code, &failure{code: code, transient: f.transient, err: fmt.Errorf("plugin %q: %w", name, f.err)})
	}
	if code != "" {
		return errcode.Wrap(code, fmt.Errorf("plugin %q: %w", name, err))
	}
	return err
}

// load fetches the artifact of d and records its accounting.
func (e *Engine) load(ctx context.Context, d *digestEntry, image string) error {
	if e.source == nil {
		return failTransient(CodeArtifact, "no Plugin artifact source is configured on this Node")
	}
	bin, err := e.source.Fetch(ctx, image, d.digest)
	if err != nil {
		if _, ok := errcode.CodeOf(err); ok {
			return err
		}
		return failTransient(CodeArtifact, "the artifact is unavailable: %v", err)
	}
	d.bin = bin
	d.accounted = accountedCode(len(bin))
	d.cache = wazero.NewCompilationCache()
	return nil
}

// imageDigest returns the @sha256 digest of a Plugin image reference.
func imageDigest(image string) (revision.Digest, error) {
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == '@' {
			return revision.Parse(image[i+1:])
		}
	}
	return revision.Digest{}, errors.New("the image has no @sha256 digest")
}

// retain counts a pool's hold on d, adding its accounted code to the
// Node's on the first hold.
func (e *Engine) retain(d *digestEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d.refs == 0 {
		e.code += d.accounted
	}
	d.refs++
}

// drop releases a pool's hold on d: at the last one the digest's runtimes
// and cache close, which frees its code at the next garbage collection.
func (e *Engine) drop(ctx context.Context, d *digestEntry) {
	e.mu.Lock()
	d.refs--
	last := d.refs == 0
	if last {
		e.code -= d.accounted
		if e.digests[d.digest] == d {
			delete(e.digests, d.digest)
		}
	}
	e.mu.Unlock()
	if last {
		d.close(ctx)
	}
}

// checkCode checks the compiled-code limit for a Revision whose Plugins'
// distinct digests account for revisionCode (docs/architecture/05-wasm-
// plugin-system.md "Default limits", Compiled code per Node): beyond the
// limit alone it is deterministic, computed alike on every Node; beyond it
// with the retained code of older snapshots it is transient.
func (e *Engine) checkCode(revisionCode, newCode int64) error {
	if revisionCode > compiledCodeLimit {
		return fail(CodeCompiledLimit, "the Revision's Plugins account for %d bytes of compiled code, above the %d-byte limit per Node", revisionCode, compiledCodeLimit)
	}
	e.mu.Lock()
	retained := e.code
	e.mu.Unlock()
	if retained+newCode > compiledCodeLimit {
		return failTransient(CodeCompiledLimit, "retained compiled code of older snapshots (%d bytes) and this Revision's new Plugins (%d bytes) pass the %d-byte limit per Node", retained, newCode, compiledCodeLimit)
	}
	return nil
}

// measureCompile compiles d at pages, checking the measured native code a
// cold compile maps against the limit: an overrun is transient
// (plugin_compiled_code_over_estimate).
func (e *Engine) measureCompile(ctx context.Context, d *digestEntry, pages uint32) (*runtimeEntry, error) {
	before, ok := e.measure()
	r, err := e.compiled(ctx, d, pages)
	if err != nil || !ok {
		return r, err
	}
	after, ok := e.measure()
	if ok && after > compiledCodeLimit && after > before {
		e.log.LogAttrs(ctx, slog.LevelWarn, "plugin compiled code over its estimate",
			slog.String(catalog.KeyReason, "plugin_compiled_code_over_estimate"), slog.Int64("mapped_bytes", after))
		return nil, failTransient(CodeCompiledLimit, "the Node maps %d bytes of compiled Plugin code, above the %d-byte limit", after, compiledCodeLimit)
	}
	return r, nil
}

// configHash returns the pool key digest of an rz_configure input.
func configHash(config []byte) [32]byte { return sha256.Sum256(config) }
