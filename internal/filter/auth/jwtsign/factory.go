// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ErrNoCEL reports a Build of a claims config without a CEL builder
// (BuildEnv.CEL).
var ErrNoCEL = errors.New("auth.jwt-sign: no CEL builder")

// Factory builds auth.jwt-sign Filters. It holds nothing: each Filter
// keeps its own signing key and its watch.
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles one auth.jwt-sign Policy at its attachment: a config with
// a finding of the package documentation ("Configuration") is RZ-CFG-005
// naming the first finding; config.claims is compiled at its site
// (BuildEnv.Site, expr.PlaceJWTSignClaims), RZ-CFG-014 or RZ-CFG-015 as
// the compiler reports it; and config.signingKey must be resolved and hold
// a private key fitting config.algorithm, else RZ-CFG-026. The key holder
// of BuildEnv.Previous is carried over when it follows the same reference
// and algorithm, so its watch and its last accepted key survive the swap.
// An ended ctx is its error, without a code; a claims config without
// BuildEnv.CEL is ErrNoCEL.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthJWTSignConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "%s: config is %T, want *v1alpha1.AuthJWTSignConfig", Type, env.Config)
	}
	if fs := findings(cfg); len(fs) > 0 {
		at := "config"
		if len(fs[0].path) > 0 {
			at += "." + fs[0].path.String()
		}
		return nil, errcode.Errorf(CodeConfig, "%s: %s: %s", Type, at, fs[0].message)
	}
	head, err := header(cfg.Algorithm, cfg.KeyID)
	if err != nil {
		return nil, errcode.Errorf(CodeConfig, "%s: %v", Type, err)
	}
	f := &Filter{
		signer: signer{alg: cfg.Algorithm, head: head},
		limit:  env.Limits.MaxResponseBodyBytes,
	}
	for _, p := range cfg.Paths {
		parsed, err := dotpath.Parse(p)
		if err != nil {
			// findings parsed every path already.
			return nil, errcode.Errorf(CodeConfig, "%s: config.paths: %v", Type, err)
		}
		f.paths = append(f.paths, parsed)
	}
	if cfg.Claims != "" {
		if f.claims, err = compileClaims(env, cfg.Claims); err != nil {
			return nil, err
		}
		if f.target, err = dotpath.Parse(cfg.Target); err != nil {
			return nil, errcode.Errorf(CodeConfig, "%s: config.target: %v", Type, err)
		}
	}
	if env.Secrets == nil {
		return nil, errcode.Errorf(CodeKey, "%s: no resolved secrets", Type)
	}
	ref := secret.RefOf(cfg.SigningKey.SecretRef)
	if f.keys = carried(env.Previous, ref, cfg.Algorithm); f.keys == nil {
		if f.keys, err = newHolder(env.Secrets, ref, cfg.Algorithm); err != nil {
			return nil, errcode.Wrap(CodeKey, fmt.Errorf("%s: %w", Type, err))
		}
	}
	return f, nil
}

// compileClaims compiles config.claims at env's site of
// expr.PlaceJWTSignClaims: a syntax, type or unavailable-variable error is
// RZ-CFG-014 and a static cost over the ceiling RZ-CFG-015.
func compileClaims(env filter.BuildEnv, src string) (expr.Program, error) {
	if env.CEL == nil {
		return nil, ErrNoCEL
	}
	site := expr.Site{Place: expr.PlaceJWTSignClaims, Scope: env.Scope, PolicyType: Type}
	if env.Site != nil {
		site = env.Site(expr.PlaceJWTSignClaims)
	}
	prg, iss := env.CEL.Compile(site, src)
	if prg != nil {
		return prg, nil
	}
	code, msg := CodeCEL, "did not compile"
	if len(iss) > 0 {
		msg = iss[0].Message
		if iss[0].Code == CodeCost {
			code = CodeCost
		}
	}
	return nil, errcode.Errorf(code, "%s: config.claims: %s", Type, msg)
}

// carried returns the key holder of prev, with a reference taken, when
// prev is an auth.jwt-sign Filter whose holder follows ref for alg and is
// still open; nil otherwise.
func carried(prev filter.Filter, ref secret.Ref, alg v1alpha1.JWTSignAlgorithm) *holder {
	p, ok := prev.(*Filter)
	if !ok || p == nil || p.keys == nil || p.keys.ref != ref || p.keys.alg != alg || !p.keys.retain() {
		return nil
	}
	return p.keys
}

// holder is the signing key of one config.signingKey reference and
// algorithm, which the Filters built for them share across snapshots: it
// watches the reference, and a rotated value that is not a private key
// fitting the algorithm keeps the last accepted key, the resolver
// counting the rotation failure and raising secret_rotation_failed
// (secret.Store.Watch).
type holder struct {
	secrets secret.Store
	ref     secret.Ref
	alg     v1alpha1.JWTSignAlgorithm

	cur atomic.Pointer[key]

	// mu guards the fields below. secret.Store never calls a watch
	// callback from Watch or Get, so mu is held across them.
	mu     sync.Mutex
	refs   int
	closed bool
	stop   func()
}

// errUnresolved is a config.signingKey missing from the resolved table.
var errUnresolved = errors.New("config.signingKey is not resolved")

// newHolder watches ref, loads its current value as a key for alg and
// holds one reference for the caller. An unresolved reference or a
// refused value is an error, and nothing keeps running.
func newHolder(secrets secret.Store, ref secret.Ref, alg v1alpha1.JWTSignAlgorithm) (*holder, error) {
	h := &holder{secrets: secrets, ref: ref, alg: alg, refs: 1}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Watch first, then read: a rotation published in between reaches the
	// callback, which waits for mu and then loads the newer value.
	h.stop = secrets.Watch(ref, h.rotate)
	v, ok := secrets.Get(ref)
	if !ok {
		h.closeLocked()
		return nil, fmt.Errorf("%w (%s)", errUnresolved, ref)
	}
	k, err := loadKey(v, alg)
	if err != nil {
		h.closeLocked()
		return nil, err
	}
	h.cur.Store(k)
	return h, nil
}

// loadKey parses the secret value v as a private key fitting alg.
func loadKey(v secret.Value, alg v1alpha1.JWTSignAlgorithm) (*key, error) {
	b := v.Reveal()
	defer clear(b)
	k, err := parseKey(b)
	if err != nil {
		return nil, err
	}
	if err := k.fits(alg); err != nil {
		return nil, err
	}
	return k, nil
}

// rotate is the watch callback: it loads v, or keeps the last key and
// returns why v is refused.
func (h *holder) rotate(v secret.Value) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	k, err := loadKey(v, h.alg)
	if err != nil {
		return fmt.Errorf("%s: %w", Type, err)
	}
	h.cur.Store(k)
	return nil
}

// current returns the current signing key.
func (h *holder) current() *key { return h.cur.Load() }

// retain takes a reference for a Filter carrying h over; false once h is
// closed.
func (h *holder) retain() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.refs++
	return true
}

// release drops a reference; the last one stops the watch.
func (h *holder) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.refs--; h.refs > 0 {
		return
	}
	h.closeLocked()
}

// closeLocked marks h closed and stops its watch.
func (h *holder) closeLocked() {
	h.closed = true
	if h.stop != nil {
		h.stop()
		h.stop = nil
	}
}
