// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/check"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthzGeoIP

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is two empty lists or a code that is not two upper-case
	// letters.
	CodeConfig = check.CodeConfig
	// CodeDenied is a refused client country (403).
	CodeDenied = "RZ-AUTH-014"
)

// Causes of cannot decide, never sent to the client.
var (
	// errNoAddress: the request has no valid client address.
	errNoAddress = errors.New("authz.geoip: request has no valid client address")
	// errNoDatabase: the Node has no valid GeoIP database.
	errNoDatabase = errors.New("authz.geoip: the Node has no valid GeoIP database (RURALZ_GEOIP_DATABASE_FILE)")
)

// Factory builds authz.geoip Filters (filter.Factory) over the Node's
// GeoIP database; a nil Database is none, so every request its Filters
// see cannot decide.
type Factory struct {
	// Database is the Node's GeoIP database, which internal/filter/builtin
	// constructs once per process; nil is none.
	Database *Database
}

var _ filter.Factory = Factory{}

// Build compiles config.allow and config.deny into country sets. A config
// of the wrong type, two empty lists (check.Empty) or a code that is not
// two upper-case letters, which the schema refuses, is an error carrying
// RZ-CFG-005. The lists compile in microseconds, so nothing is carried
// over from env.Previous. Each Filter counts as a live user of the
// database until Close.
func (f Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthzGeoIPConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "authz.geoip: config is %T, want *v1alpha1.AuthzGeoIPConfig", env.Config)
	}
	if check.Empty(cfg) {
		return nil, errcode.Errorf(CodeConfig, "authz.geoip: %s", check.MessageEmpty)
	}
	allow, err := compile(fieldAllow, cfg.Allow)
	if err != nil {
		return nil, err
	}
	deny, err := compile(fieldDeny, cfg.Deny)
	if err != nil {
		return nil, err
	}
	flt := &Filter{
		Decider:  auth.NewAuthz(env.Metrics),
		db:       f.Database,
		allow:    allow,
		deny:     deny,
		hasAllow: len(cfg.Allow) > 0,
	}
	f.Database.acquire()
	return flt, nil
}

// Config members holding the lists.
const (
	fieldAllow = "allow"
	fieldDeny  = "deny"
)

// compile parses the codes of one list into a set; the first code that is
// not two upper-case letters is RZ-CFG-005 naming the list and the code.
func compile(field string, codes []v1alpha1.CountryCode) (countrySet, error) {
	var s countrySet
	for _, code := range codes {
		c, ok := parseCountry(string(code))
		if !ok {
			return countrySet{}, errcode.Errorf(CodeConfig, "authz.geoip: config.%s entry %q is not an ISO 3166-1 alpha-2 code", field, string(code))
		}
		s.add(c)
	}
	return s, nil
}

// Filter is a compiled authz.geoip Policy. The embedded Decider counts its
// decisions and builds its answers.
type Filter struct {
	*auth.Decider
	// db is the Node's database, read per request; nil is none.
	db *Database
	// allow and deny are the compiled lists.
	allow, deny countrySet
	// hasAllow is true when config.allow is non-empty, so every address
	// outside it is refused.
	hasAllow bool
	// closed makes Close release the database once.
	closed atomic.Bool
}

var (
	_ filter.Filter = (*Filter)(nil)
	_ filter.Closer = (*Filter)(nil)
)

// Handle decides in onRequestHeaders and continues in every other Phase:
// the client address, source.ip, is placed in a country by the database
// in use, and a deny match, or a country outside a non-empty allow list,
// is 403 RZ-AUTH-014; an address the database cannot place matches
// neither list. A request without a valid client address, a Node without
// a database and a lookup that fails cannot decide (403 RZ-AUTH-015 under
// closed). It allocates nothing for an admitted request.
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	a := x.Source().IP
	if !a.IsValid() {
		return f.Undecided(x, "", errNoAddress)
	}
	db := f.db.current()
	if db == nil {
		return f.Undecided(x, "", errNoDatabase)
	}
	c, err := db.place(a)
	if err != nil {
		return f.Undecided(x, "", fmt.Errorf("authz.geoip: %w", err))
	}
	if !f.admits(c) {
		return f.Deny(x, CodeDenied)
	}
	return f.Allow(x)
}

// admits reports whether the lists admit an address placed in c: c is in
// no deny entry and, when the allow list is non-empty, in an allow entry.
// noCountry, an address the database cannot place, matches neither list.
func (f *Filter) admits(c country) bool {
	if f.deny.has(c) {
		return false
	}
	return !f.hasAllow || f.allow.has(c)
}

// Close ends the Filter's use of the database (filter.Closer); later
// calls do nothing.
func (f *Filter) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		f.db.release()
	}
	return nil
}
