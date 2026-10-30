// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// serve is the serve check, step 7 of the validation gate: every resource
// of v, whether a Route reaches it or not, against the served-feature
// table (registry.Registry.CheckFeatures) and, for a Policy, the Policy
// type registry (CheckServed). It returns every RZ-CFG-040 finding,
// sorted.
func (c *Compiler) serve(v *hub.Validated, loc locator) (diag.List, error) {
	idx := c.d.Pipeline.HubIndex()
	var out diag.List
	for _, r := range v.Bundle.Resources() {
		if r.Tree != nil {
			at := func(p tree.Pos) diag.Location { return loc.pos(r, p) }
			ds, err := c.reg.CheckFeatures(idx, r.Kind, r.Name, r.Tree, at)
			if err != nil {
				return nil, err
			}
			out = append(out, ds...)
		}
		if p, ok := hub.Object[v1alpha1.Policy](r); ok && p != nil {
			if d, bad := c.reg.CheckServed(p); bad {
				d.Location = loc.at(r, d.Path)
				out = append(out, d)
			}
		}
	}
	out.Sort()
	return out, nil
}

// resolve resolves every secretRef of the Revision, step 9 of the
// validation gate: each use the pipeline found (hub.Validated.Secrets)
// and, for a Gateway without stateStore, RURALZ_STATE_STORE_URL when the
// variable is set, a use at the Gateway's spec; unset or blank, the
// memory driver serves (manager.SpecOf). It returns every failing use
// (RZ-CFG-026, or RZ-CFG-042 for a State Store URL without credentials),
// sorted; ctx's error when it ended meanwhile.
func (b *build) resolve(ctx context.Context) (diag.List, error) {
	uses := make([]secret.Use, 0, len(b.v.Secrets)+1)
	for _, s := range b.v.Secrets {
		u := secret.Use{Ref: secret.RefOf(s.Ref), Resource: s.Resource, Path: s.Path, Loc: s.Loc, Kind: s.Kind}
		if u.Loc == (diag.Location{}) {
			if r, ok := b.b.Get(hub.ID{Kind: v1alpha1.Kind(s.Resource.Kind), Name: s.Resource.Name}); ok {
				u.Loc = b.loc.at(r, s.Path)
			}
		}
		uses = append(uses, u)
	}
	if b.gw.Spec.StateStore == nil {
		// A blank value is unset, as manager.SpecOf reads it.
		if v, ok := b.c.lookupEnv(manager.EnvStateStoreURL); ok && strings.Trim(v, " \t\r\n") != "" {
			uses = append(uses, secret.Use{
				Ref: manager.EnvRef(), Resource: *b.gwRes.ResourceID(), Path: specPath(),
				Loc: b.loc.at(b.gwRes, specPath()), Kind: secret.KindStateStoreURL,
			})
		}
	}
	st, ds := b.c.d.Secrets.Resolve(ctx, uses)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st == nil || ds.HasErrors() {
		if len(ds) == 0 {
			return nil, errors.New("compile: the secret resolver returned neither a Store nor a finding")
		}
		ds.Sort()
		return ds, nil
	}
	b.secrets = st
	return nil, nil
}

// stores acquires the snapshot's State Store handles, the end of step 9:
// Gateway spec.stateStore resolved with the secret table, one new handle
// per role. An opener's refusal, a URL the transport rules refuse
// (RZ-CFG-042), one that does not fit its topology or a TLS value that
// does not load (RZ-CFG-026), or a client certificate without its key
// (RZ-CFG-005), is a finding at the connection's block: spec.stateStore,
// or spec.stateStore.cache for the cache connection; at spec without
// stateStore, when RURALZ_STATE_STORE_URL is refused.
func (b *build) stores(ctx context.Context) (diag.List, error) {
	spec, err := manager.SpecOf(b.gw.Spec.StateStore, b.secrets)
	if err != nil {
		// The schema, stage H (the zone map hosts) and the resolution rule
		// out every SpecOf error.
		return nil, fmt.Errorf("compile: stateStore: %w", err)
	}
	spec.Limits, spec.Caches = b.limitPolicies, b.cachePolicies
	hs, err := b.c.d.StateStores.Acquire(ctx, spec)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		code, ok := findingCode(err)
		if !ok {
			return nil, fmt.Errorf("compile: open the State Store: %w", err)
		}
		path := specPath("stateStore")
		var oe *manager.OpenError
		switch {
		case b.gw.Spec.StateStore == nil:
			path = specPath()
		case errors.As(err, &oe) && oe.Role == statestore.RoleCache:
			path = specPath("stateStore", "cache")
		}
		return diag.List{b.loc.finding(code, b.gwRes, path, err)}, nil
	}
	b.opened.handles = hs
	return nil, nil
}
