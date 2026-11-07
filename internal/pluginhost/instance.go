// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/ravindu-rev/ruralz/internal/phase"
)

// instance is one wazero module instance of a pool, which serves one
// Phase call at a time: api.Function.Call is not goroutine-safe.
type instance struct {
	p   *pool
	mod api.Module
	// phases holds the Phase exports by Phase.
	phases [phase.Count]api.Function
	// calls counts the Phase calls served, for recycling.
	calls int
}

// errConfigRejected is rz_configure returning non-zero.
var errConfigRejected = errors.New("rz_configure refused the Policy config")

// errABIVersion is rz_abi_version returning another version.
var errABIVersion = errors.New("rz_abi_version did not return 1")

// newInstance instantiates the pool's compiled code and configures it
// within the instantiate limit (100 ms, rz_configure included): the
// guest's rz_abi_version must return 1, and rz_configure receives the
// Policy's canonical config, rz_alloc giving the buffer and rz_free taking
// it back. rz_configure may call only log, clock, random and crypto.
func (p *pool) newInstance(ctx context.Context) (*instance, error) {
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := startTimer(p.e.clk, instantiateTimeout, cancel)
	defer timer.stop()
	c := &call{e: p.e, b: p.configBinding, configuring: true, reqCtx: ictx, timer: timer}
	cctx := context.WithValue(ictx, callKey{}, c)
	mod, err := p.rt.rt.InstantiateModule(cctx, p.rt.compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, p.instantiateError(timer, err)
	}
	inst := &instance{p: p, mod: mod}
	c.inst = inst
	ok := false
	defer func() {
		if !ok {
			_ = mod.Close(context.WithoutCancel(ctx))
		}
	}()
	for _, ph := range p.phases.Phases() {
		inst.phases[ph] = mod.ExportedFunction(phaseExport(ph))
	}
	res, err := mod.ExportedFunction(exportABIVersion).Call(cctx)
	if err != nil {
		return nil, p.instantiateError(timer, err)
	}
	if int32(uint32(res[0])) != abiVersion { //nolint:gosec // G115: an i32 result is its low 32 bits.
		return nil, errABIVersion
	}
	if err := configure(cctx, mod, p.config); err != nil {
		return nil, p.instantiateError(timer, err)
	}
	if c.sticky != 0 {
		return nil, fmt.Errorf("%w: %s", errConfigRejected, c.stickyWhy)
	}
	ok = true
	return inst, nil
}

// errInstantiateDeadline is an instantiation or rz_configure past the
// instantiate limit.
var errInstantiateDeadline = errors.New("instantiate and rz_configure did not finish within 100 ms")

// instantiateError names a timeout as errInstantiateDeadline.
func (p *pool) instantiateError(t *callTimer, err error) error {
	if t.fired.Load() {
		return errInstantiateDeadline
	}
	return err
}

// configure passes config to rz_configure through a buffer from rz_alloc,
// which rz_free takes back after the call.
func configure(ctx context.Context, mod api.Module, config []byte) error {
	n := uint32(len(config)) //nolint:gosec // G115: a config is far below 4 GiB.
	res, err := mod.ExportedFunction(exportAlloc).Call(ctx, uint64(n))
	if err != nil {
		return err
	}
	ptr := uint32(res[0]) //nolint:gosec // G115: an i32 pointer is its low 32 bits.
	if n > 0 && !mod.Memory().Write(ptr, config) {
		return fmt.Errorf("%w: rz_alloc returned a buffer outside memory", errConfigRejected)
	}
	res, err = mod.ExportedFunction(exportConfigure).Call(ctx, uint64(ptr), uint64(n))
	if err != nil {
		return err
	}
	if int32(uint32(res[0])) != 0 { //nolint:gosec // G115: an i32 result is its low 32 bits.
		return errConfigRejected
	}
	if _, err := mod.ExportedFunction(exportFree).Call(ctx, uint64(ptr)); err != nil {
		return err
	}
	return nil
}

// close closes the instance's module, whether or not ctx is done.
func (i *instance) close(ctx context.Context) { _ = i.mod.Close(context.WithoutCancel(ctx)) }
