// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"context"
	"errors"
	"net/url"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes Build and Check return.
const (
	// CodeHTTPS: an issuers[].jwksUrl that is not an https URL.
	CodeHTTPS = "RZ-CFG-037"
	// codeSchema: a config that is not a valid AuthJWTConfig.
	codeSchema = "RZ-CFG-005"
)

// ErrNoManager reports a Factory built without a JWKS manager.
var ErrNoManager = errors.New("jwt: no JWKS manager")

// Factory builds auth.jwt Filters over one Node-wide JWKS manager.
type Factory struct {
	keys    *jwks.Manager
	revoker identity.Revoker
	codec   *codec
}

var _ filter.Factory = (*Factory)(nil)

// NewFactory returns the auth.jwt Factory. keys is the Node's JWKS
// manager, which the ruralzd supervisor runs as a filter.Component;
// revoker is consulted after every verification (nil means
// identity.NopRevoker, the M1 wiring).
func NewFactory(keys *jwks.Manager, revoker identity.Revoker) (*Factory, error) {
	if keys == nil {
		return nil, ErrNoManager
	}
	if revoker == nil {
		revoker = identity.NopRevoker{}
	}
	c, err := newCodec()
	if err != nil {
		return nil, err
	}
	return &Factory{keys: keys, revoker: revoker, codec: c}, nil
}

// roundKey memoizes a compile's prefetch round in BuildEnv.Shared.
type roundKey struct{}

// Build compiles one auth.jwt Policy: it checks the config (a jwksUrl
// that is not https is RZ-CFG-037, any other invalid config RZ-CFG-005),
// acquires the snapshot's credential index and the key set of every
// issuer, then waits for the first fetch of issuers without usable keys
// until the compile's prefetch round ends (jwks.PrefetchTimeout after its
// first auth.jwt Build) and returns the Filter whatever the fetches
// returned. A ctx that ends during the wait is its error, unchanged and
// without a code, and the Filter's resources are released.
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	cfg, ok := env.Config.(*v1alpha1.AuthJWTConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(codeSchema, "auth.jwt: config is %T, not an auth.jwt config", env.Config)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	ix, err := auth.AcquireIndex(env)
	if err != nil {
		return nil, err
	}
	srcs := make([]jwks.Source, len(cfg.Issuers))
	for i, is := range cfg.Issuers {
		srcs[i] = jwks.Source{Issuer: is.Issuer, URL: is.JWKSURL}
	}
	hs := f.keys.Acquire(srcs)
	keys := make([]keySource, len(hs))
	for i, h := range hs {
		keys[i] = h
	}
	flt := newFilter(env, cfg, ix, keys, f.revoker, f.codec)
	f.keys.Wait(ctx, f.round(env), hs)
	if err := ctx.Err(); err != nil {
		_ = flt.Close()
		return nil, err
	}
	return flt, nil
}

// round returns the prefetch round of the compile env belongs to: one per
// BuildEnv.Shared, so the waits of all the compile's auth.jwt Policies end
// together; a Build without Shared gets its own.
func (f *Factory) round(env filter.BuildEnv) *jwks.Round {
	if env.Shared == nil {
		return f.keys.NewRound()
	}
	v, err := env.Shared.Get(roundKey{}, func() (any, error) { return f.keys.NewRound(), nil })
	if r, ok := v.(*jwks.Round); ok && err == nil {
		return r
	}
	return f.keys.NewRound()
}

// newFilter assembles a Filter; keys[i] serves cfg.Issuers[i].
func newFilter(env filter.BuildEnv, cfg *v1alpha1.AuthJWTConfig, ix *identity.Index, keys []keySource,
	revoker identity.Revoker, c *codec,
) *Filter {
	flt := &Filter{
		Decider: auth.NewAuth(env.Metrics, auth.ChallengeJWT),
		issuers: make(map[string]*issuer, len(cfg.Issuers)),
		index:   ix,
		revoker: revoker,
		codec:   c,
	}
	if env.Policy != nil {
		flt.policy = env.Policy.Metadata.Name
	}
	if env.Metrics != nil {
		flt.age = env.Metrics.JWKSAge
	}
	for i, is := range cfg.Issuers {
		e := &issuer{name: is.Issuer, audiences: slices.Clone(is.Audiences), keys: keys[i]}
		flt.issuers[is.Issuer] = e
		flt.order = append(flt.order, e)
	}
	return flt
}

// validate checks what the schema leaves to the Factory: at least one
// issuer, each with a non-empty issuer named once, at least one audience
// and an https jwksUrl. It refuses the first of the findings Check
// reports offline, so both apply one rule set.
func validate(cfg *v1alpha1.AuthJWTConfig) error {
	if fs := findings(cfg); len(fs) > 0 {
		return fs[0].err()
	}
	return nil
}

// checkJWKSURL returns an error unless s is an absolute https URL with a
// host (TB-10).
func checkJWKSURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return errors.New("jwksUrl is not a URL")
	case u.Scheme != "https" || u.Opaque != "":
		return errors.New("jwksUrl is not an https URL")
	case u.Hostname() == "":
		return errors.New("jwksUrl has no host")
	}
	return nil
}
