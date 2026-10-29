// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ip

import (
	"context"
	"errors"
	"net/netip"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthzIP

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a malformed list entry or two empty lists.
	CodeConfig = "RZ-CFG-005"
	// CodeDenied is a refused client address (403).
	CodeDenied = "RZ-AUTH-013"
)

// errNoAddress is the cause of cannot decide for a request without a valid
// client address.
var errNoAddress = errors.New("authz.ip: request has no valid client address")

// Factory builds authz.ip Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles config.allow and config.deny. A config of the wrong type,
// two empty lists or a malformed entry is an error carrying RZ-CFG-005.
// The lists are small and compile in microseconds, so nothing is carried
// over from env.Previous.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthzIPConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "authz.ip: config is %T, want *v1alpha1.AuthzIPConfig", env.Config)
	}
	if len(cfg.Allow) == 0 && len(cfg.Deny) == 0 {
		return nil, errcode.Errorf(CodeConfig, "authz.ip: config.allow and config.deny are both empty")
	}
	allow, err := compile(fieldAllow, cfg.Allow)
	if err != nil {
		return nil, err
	}
	deny, err := compile(fieldDeny, cfg.Deny)
	if err != nil {
		return nil, err
	}
	return &Filter{
		Decider:  auth.NewAuthz(env.Metrics),
		allow:    allow,
		deny:     deny,
		hasAllow: len(cfg.Allow) > 0,
	}, nil
}

// compile parses the entries of one list into a prefix set; the first
// malformed entry is RZ-CFG-005 naming the list and the entry.
func compile(field string, entries []string) (*clientaddr.Trusted, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, s := range entries {
		p, err := clientaddr.ParsePrefix(s)
		if err != nil {
			return nil, errcode.Errorf(CodeConfig, "authz.ip: config.%s entry %q is not a CIDR or an IP address", field, s)
		}
		prefixes = append(prefixes, p)
	}
	return clientaddr.New(prefixes), nil
}

// Filter is a compiled authz.ip Policy. The embedded Decider counts its
// decisions and builds its answers.
type Filter struct {
	*auth.Decider
	// allow and deny are the compiled lists; a nil set holds nothing.
	allow, deny *clientaddr.Trusted
	// hasAllow is true when config.allow is non-empty, so every address
	// outside it is refused.
	hasAllow bool
}

// Handle decides in onRequestHeaders and continues in every other Phase:
// a deny match, or an address outside a non-empty allow list, is 403
// RZ-AUTH-013; a request without a valid client address cannot decide
// (403 RZ-AUTH-015 under closed); any other request is admitted.
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	a := x.Source().IP
	if !a.IsValid() {
		return f.Undecided(x, "", errNoAddress)
	}
	if !f.Admits(a) {
		return f.Deny(x, CodeDenied)
	}
	return f.Allow(x)
}

// Admits reports whether the lists admit the client address a: it is in
// no deny entry and, when the allow list is non-empty, in an allow entry.
// It allocates nothing.
func (f *Filter) Admits(a netip.Addr) bool {
	if f.deny.Contains(a) {
		return false
	}
	return !f.hasAllow || f.allow.Contains(a)
}
