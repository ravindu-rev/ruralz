// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// role is how a family's label sets are keyed and admitted.
type role uint8

const (
	// roleNode families carry enumeration labels only: one group per
	// family, created with the registry, never folded.
	roleNode role = iota + 1
	// roleListener families are keyed by listener; admitted first, never
	// folded (spec 09 req 55).
	roleListener
	// roleRoute, roleUpstream and rolePolicy families are keyed by a
	// resource name and fold into _overflow.
	roleRoute
	roleUpstream
	rolePolicy
)

// layout is the shape of one group's label sets.
type layout uint8

const (
	// layoutValues: the mixed-radix product of the enumeration labels.
	layoutValues layout = iota + 1
	// layoutStatus: five status classes behind one StatusCounter handle.
	layoutStatus
	// layoutListenerRequests: protocol x status_class x origin behind one
	// ListenerRequests handle.
	layoutListenerRequests
	// layoutAttempts: five status classes with error="none" plus the four
	// errors with status_class="5xx" (spec 09 section 9 item 9).
	layoutAttempts
	// layoutResultCode: the first result value without a code, the second
	// result value with lazily created code label sets.
	layoutResultCode
	// layoutCode: code label sets only, created on first record.
	layoutCode
	// layoutRevisionInfo: ruralz_config_revision_info (at most two series).
	layoutRevisionInfo
	// layoutComputed: values computed at collection (runtime metrics,
	// folded label sets, series counts).
	layoutComputed
)

// Label names the aggregates treat specially.
const (
	labelPhase       = "phase"
	labelListener    = "listener"
	labelStatusClass = "status_class"
	labelError       = "error"
	labelCode        = "code"
	labelResult      = "result"
	labelRevision    = "revision"
	labelRole        = "role"
	labelInstrument  = "instrument"
	labelState       = "state"
)

// Resource labels by admission class.
const (
	labelRoute    = "route"
	labelUpstream = "upstream"
	labelPolicy   = "policy"
)

// Reserved resource label values (spec 09 req 41). Resource names are DNS
// labels, so values starting with "_" never collide with them.
const (
	// Overflow is the label value folded label sets record into.
	Overflow = "_overflow"
	// Unmatched is the route label value of requests no Route matched.
	Unmatched = "_unmatched"
)

// Policy types whose Policies record type-specific families. The strings
// are pkg/config/v1alpha1 PolicyType values (checked by a test); this
// package may not import the configuration types (architecture 1.2).
const (
	typeAuthPrefix     = "auth."
	typeAuthzPrefix    = "authz."
	typeUpstreamPrefix = "auth.upstream-"
	typeAuthJWT        = "auth.jwt"
	typeUpstreamOAuth2 = "auth.upstream-oauth2"
	typeRateLimit      = "ratelimit"
	typeQuota          = "quota"
	typeCache          = "cache"
)

// Phase indexes the aggregates need (phase.Phase values; checked by a test).
const (
	phaseOnUpstreamRequest = 3 // the last Phase that can short-circuit
	phaseOnLog             = 7
)

// Histogram word layout: 13 bucket counts, +Inf, the integer sum and one
// padding word, 128 bytes per label set and stripe.
const (
	numBounds  = 13
	numBuckets = numBounds + 1
	sumWord    = numBuckets
	histWords  = 16
	// histSeries is the series count of one histogram label set: 13
	// buckets, +Inf, _sum and _count (spec 09 req 42).
	histSeries = 16
)

// numClasses is the number of status classes (1xx to 5xx).
const numClasses = 5

// family is one catalog family with its registry state.
type family struct {
	idx      int
	cat      catalog.Family
	role     role
	layout   layout
	hist     bool
	gauge    bool
	resLabel string
	// phase is the phase label's enumeration when the family is keyed by
	// Phase as well as by resource.
	phase []string
	// enums are the enumeration labels of the group layout (layoutValues,
	// layoutStatus, layoutListenerRequests), in catalog order.
	enums []catalog.Label
	// n is the number of fixed label sets per group.
	n      int
	bounds *histBounds
	// result is the result enumeration of layoutResultCode families.
	result []string

	// overflowStriped and unmatchedStriped are fixed by New: whether the
	// family's _overflow and _unmatched groups are striped.
	overflowStriped  bool
	unmatchedStriped bool

	// Registry state, guarded by Registry.mu.
	groups        map[groupKey]*group
	fixed         []*group
	overflow      map[int8]*group
	unmatched     *group
	sorted        []*group
	dirty         bool
	activeFolded  int
	ceilingFolded int
}

// foldable reports whether the family's label sets can fold.
func (f *family) foldable() bool {
	return f.role == roleRoute || f.role == roleUpstream || f.role == rolePolicy
}

// seriesPerSet is the series count of one label set: 16 for a histogram
// (spec 09 req 42), else 1.
func (f *family) seriesPerSet() int {
	if f.hist {
		return histSeries
	}
	return 1
}

// errCatalog reports a catalog family this package cannot aggregate.
var errCatalog = errors.New("aggregate: unsupported catalog family")

// specialLayouts are the families whose layout is not the enumeration
// product.
func specialLayout(name string) (layout, bool) {
	switch name {
	case catalog.HTTPRequestsTotal, catalog.FilterShortCircuitsTotal:
		return layoutStatus, true
	case catalog.HTTPListenerRequestsTotal:
		return layoutListenerRequests, true
	case catalog.UpstreamAttemptsTotal:
		return layoutAttempts, true
	case catalog.AuthDecisionsTotal, catalog.ConfigActivationsTotal:
		return layoutResultCode, true
	case catalog.HTTPNodeResponsesTotal:
		return layoutCode, true
	case catalog.ConfigRevisionInfo:
		return layoutRevisionInfo, true
	case catalog.RuntimeGoroutines, catalog.RuntimeHeapBytes, catalog.RuntimeGCCyclesTotal,
		catalog.TelemetryFoldedLabelSets, catalog.TelemetrySeries:
		return layoutComputed, true
	default:
		return 0, false
	}
}

// newFamily classifies one catalog family.
func newFamily(idx int, c catalog.Family) (*family, error) {
	f := &family{
		idx:      idx,
		cat:      c,
		hist:     c.Kind == catalog.Histogram,
		gauge:    c.Kind == catalog.Gauge,
		groups:   make(map[groupKey]*group),
		overflow: make(map[int8]*group),
	}
	switch c.Class {
	case catalog.ClassRoute:
		f.role, f.resLabel = roleRoute, labelRoute
	case catalog.ClassUpstream:
		f.role, f.resLabel = roleUpstream, labelUpstream
	case catalog.ClassPolicy:
		f.role, f.resLabel = rolePolicy, labelPolicy
	case catalog.ClassListener:
		f.role = roleNode
		if len(c.Labels) > 0 && c.Labels[0].Name == labelListener {
			f.role, f.resLabel = roleListener, labelListener
		}
	default:
		return nil, fmt.Errorf("%w: %s has admission class %d", errCatalog, c.Name, c.Class)
	}
	if f.foldable() && (len(c.Labels) == 0 || c.Labels[0].Name != f.resLabel) {
		return nil, fmt.Errorf("%w: %s does not start with label %q", errCatalog, c.Name, f.resLabel)
	}
	if f.hist {
		b, err := newHistBounds(c.Bounds)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		f.bounds = b
	}
	special, isSpecial := specialLayout(c.Name)
	for i, l := range c.Labels {
		switch {
		case i == 0 && f.resLabel != "":
		case l.Name == labelPhase && f.role == rolePolicy:
			f.phase = l.Values
		case l.Code, l.Values == nil:
			if !isSpecial {
				return nil, fmt.Errorf("%w: %s label %q is not an enumeration", errCatalog, c.Name, l.Name)
			}
		default:
			f.enums = append(f.enums, l)
		}
	}
	f.n = 1
	for _, l := range f.enums {
		f.n *= len(l.Values)
	}
	f.layout = layoutValues
	if isSpecial {
		f.layout = special
	}
	return f, f.checkLayout()
}

// checkLayout verifies the label shapes the special layouts rely on.
func (f *family) checkLayout() error {
	names := make([]string, 0, len(f.enums))
	for _, l := range f.enums {
		names = append(names, l.Name)
	}
	bad := func(want string) error {
		return fmt.Errorf("%w: %s enumeration labels %v, want %s", errCatalog, f.cat.Name, names, want)
	}
	switch f.layout {
	case layoutStatus:
		if len(f.enums) != 1 || f.enums[0].Name != labelStatusClass || f.n != numClasses {
			return bad("[status_class]")
		}
	case layoutListenerRequests:
		if f.n != 2*numClasses*3 || len(f.enums) != 3 || f.enums[1].Name != labelStatusClass {
			return bad("[protocol status_class origin]")
		}
	case layoutAttempts:
		if len(f.enums) != 2 || f.enums[0].Name != labelStatusClass || f.enums[1].Name != labelError {
			return bad("[status_class error]")
		}
	case layoutResultCode:
		if len(f.enums) != 1 || f.enums[0].Name != labelResult || len(f.enums[0].Values) != 2 {
			return bad("[result] with two values")
		}
		f.result = f.enums[0].Values
		f.enums = nil
		f.n = 1
	case layoutCode:
		if len(f.enums) != 0 {
			return bad("[]")
		}
		f.n = 0
	case layoutRevisionInfo, layoutComputed:
		f.n = 0
	case layoutValues:
	}
	return nil
}

// labelValues returns the enumeration values of layout index i.
func (f *family) labelValues(i int) []string {
	out := make([]string, len(f.enums))
	for k := len(f.enums) - 1; k >= 0; k-- {
		vs := f.enums[k].Values
		out[k] = vs[i%len(vs)]
		i /= len(vs)
	}
	return out
}

// index returns the layout index of values, or false when a value is not
// in its enumeration.
func (f *family) index(values []string) (int, bool) {
	if len(values) != len(f.enums) {
		return 0, false
	}
	i := 0
	for k, v := range values {
		vs := f.enums[k].Values
		j := slices.Index(vs, v)
		if j < 0 {
			return 0, false
		}
		i = i*len(vs) + j
	}
	return i, true
}

// policyApplies reports whether a Policy of type typ records family f.
func policyApplies(f *family, typ string) bool {
	switch f.cat.Name {
	case catalog.FilterDurationSeconds, catalog.FilterShortCircuitsTotal, catalog.FilterFailuresTotal:
		return true
	case catalog.AuthDecisionsTotal:
		// Every auth- and authz-class decision (spec 06 req 10); the
		// upstream-auth class records no decisions.
		return (strings.HasPrefix(typ, typeAuthPrefix) && !strings.HasPrefix(typ, typeUpstreamPrefix)) ||
			strings.HasPrefix(typ, typeAuthzPrefix)
	case catalog.AuthJWKSAgeSeconds:
		return typ == typeAuthJWT
	case catalog.AuthUpstreamTokenAgeSeconds, catalog.AuthUpstreamRefreshFailuresTotal:
		return typ == typeUpstreamOAuth2
	case catalog.RateLimitDecisionsTotal, catalog.RateLimitBucketEvictionsTotal:
		return typ == typeRateLimit
	case catalog.QuotaDecisionsTotal:
		return typ == typeQuota
	case catalog.CacheStoreSkippedTotal:
		return typ == typeCache
	default:
		return false
	}
}

// overflowPhases returns the Phases the family's _overflow groups cover:
// {-1} without a phase label, else every Phase phaseApplies admits.
func (f *family) overflowPhases() []int8 {
	if f.phase == nil {
		return []int8{-1}
	}
	out := make([]int8, 0, len(f.phase))
	for i := range f.phase {
		if phaseApplies(f, i) {
			out = append(out, int8(i))
		}
	}
	return out
}

// hasUnmatched reports whether the family records requests no Route
// matched (route="_unmatched", spec 09 req 41): the Route request counter
// and duration histogram, never the cache families.
func (f *family) hasUnmatched() bool {
	if f.role != roleRoute {
		return false
	}
	switch f.layout {
	case layoutStatus:
		return true
	case layoutValues:
		return f.hist
	case layoutListenerRequests, layoutAttempts, layoutResultCode, layoutCode, layoutRevisionInfo, layoutComputed:
	}
	return false
}

// hotSets is the number of label sets in a group's striped block: the
// status classes of error="none" for attempts (the error series stay
// unsharded), the allow set of a result-code family (codes are
// unsharded), every fixed label set otherwise.
func (f *family) hotSets() int {
	if f.layout == layoutAttempts {
		return numClasses
	}
	return f.n
}

// phaseApplies reports whether a per-Phase Policy family has a label set
// for Phase p.
func phaseApplies(f *family, p int) bool {
	switch f.cat.Name {
	case catalog.FilterShortCircuitsTotal:
		// Only request-side Phases can short-circuit (phase.CanShortCircuit).
		return p <= phaseOnUpstreamRequest
	case catalog.FilterFailuresTotal:
		// onLog is read-only; failureMode never applies there.
		return p != phaseOnLog
	default:
		return true
	}
}
