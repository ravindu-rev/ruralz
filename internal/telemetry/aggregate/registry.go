// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package aggregate holds the Ruralz-owned metric aggregates of
// docs/architecture/10-observability.md and implements emit.Meter: the
// registry built from catalog.Families, deterministic admission of each
// Revision into a Plan under the cardinality budget, folding into
// _overflow, the Binding of each snapshot with its retiring and release
// rules, cache-line striped counters and gauges, integer histograms with
// exemplar slots, and collection into metricdata through an
// sdkmetric.Producer that both the OTLP reader and the Prometheus exporter
// read (OQ-observability-16 (a)).
//
// Request-path recording takes no lock, allocates nothing and never
// blocks: a handle loads one atomic pointer (its label set, or _overflow
// once folded) and adds to one atomic word on the request's stripe.
// Admission, binding and collection take the registry mutex off the
// request path; collection runs single-flight on the reader's goroutine.
package aggregate

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Limits are the admission limits, retiring ceiling and striping caps.
// They are not configurable in a Revision (OQ-observability-10 (a)); tests
// lower them. A zero field takes its default.
type Limits struct {
	// CounterFamily is the label sets one counter or gauge family admits
	// per Revision (6,000).
	CounterFamily int
	// HistogramFamily is the label sets one histogram family admits per
	// Revision (2,000).
	HistogramFamily int
	// Revision is the series one Revision admits (100,000).
	Revision int
	// Retiring is the Node-wide ceiling of retiring series (25,000).
	// Listener and enumeration-only series count against it but never
	// fold (09 req 55), so retiring listener groups alone can hold the
	// retiring count above it once no foldable group is left to fold: the
	// ceiling bounds what folding can reclaim, not the listener share.
	Retiring int
	// ListenerStripedSeries and ListenerStripedHistograms cap the striped
	// listener and enumeration-only counter or gauge series (1,000) and
	// histogram label sets (64).
	ListenerStripedSeries     int
	ListenerStripedHistograms int
	// PolicyStripedSeries and PolicyStripedHistograms cap the striped
	// label sets of Gateway-scoped Policies (1,024 and 256).
	PolicyStripedSeries     int
	PolicyStripedHistograms int
	// StripedRoutes and StripedUpstreams are how many admitted Routes and
	// Upstreams get striped request and attempt label sets (1,000 each).
	StripedRoutes    int
	StripedUpstreams int
}

// DefaultLimits returns the spec 09 req 49, 55 and 56 targets.
func DefaultLimits() Limits {
	return Limits{
		CounterFamily:             6000,
		HistogramFamily:           2000,
		Revision:                  100000,
		Retiring:                  25000,
		ListenerStripedSeries:     1000,
		ListenerStripedHistograms: 64,
		PolicyStripedSeries:       1024,
		PolicyStripedHistograms:   256,
		StripedRoutes:             1000,
		StripedUpstreams:          1000,
	}
}

// withDefaults fills zero fields.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return Limits{
		CounterFamily:             pick(l.CounterFamily, d.CounterFamily),
		HistogramFamily:           pick(l.HistogramFamily, d.HistogramFamily),
		Revision:                  pick(l.Revision, d.Revision),
		Retiring:                  pick(l.Retiring, d.Retiring),
		ListenerStripedSeries:     pick(l.ListenerStripedSeries, d.ListenerStripedSeries),
		ListenerStripedHistograms: pick(l.ListenerStripedHistograms, d.ListenerStripedHistograms),
		PolicyStripedSeries:       pick(l.PolicyStripedSeries, d.PolicyStripedSeries),
		PolicyStripedHistograms:   pick(l.PolicyStripedHistograms, d.PolicyStripedHistograms),
		StripedRoutes:             pick(l.StripedRoutes, d.StripedRoutes),
		StripedUpstreams:          pick(l.StripedUpstreams, d.StripedUpstreams),
	}
}

// Options configure a Registry.
type Options struct {
	// Clock gives admission and collection times; nil is clock.Real().
	Clock clock.Clock
	// Stripes is the stripe count S; 0 means min(GOMAXPROCS at start, 8).
	// Values above 8 are capped.
	Stripes int
	// Limits override the admission limits (tests only).
	Limits Limits
	// Codes are the registered RZ codes (errcode.All), the only values a
	// code label takes. Nil accepts any code of the RZ-<AREA>-<NNN> shape.
	Codes []string
	// Logger receives the one DEBUG record per code table that drops an
	// unregistered code; nil logs nothing.
	Logger *slog.Logger
	// ScopeVersion is the instrumentation scope version of the produced
	// metrics (the build version).
	ScopeVersion string
}

// maxStripes is the stripe cap (spec 09 req 49).
const maxStripes = 8

// ScopeName is the instrumentation scope of every produced metric.
const ScopeName = "github.com/ravindu-rev/ruralz/internal/telemetry"

// Registry holds every aggregate of a process and implements emit.Meter.
type Registry struct {
	clk     clock.Clock
	stripes int
	tab     [256]uint8
	limits  Limits
	codes   []string
	codeSet map[string]struct{}
	logger  *slog.Logger
	scope   string
	start   time.Time

	fams   []*family
	byName map[string]*family

	// nodeStripedSeries and nodeStripedHists count the striped Node-wide
	// label sets, which share the listener striping caps.
	nodeStripedSeries int
	nodeStripedHists  int

	node *emit.NodeMetrics
	rr   atomic.Uint64

	mu sync.Mutex
	// Retiring accounting (guarded by mu). retiringSeries is the sum of
	// ceil over the retiring groups. queue holds the retiring foldable
	// groups oldest first from queueHead; an item whose group left the
	// retiring state is cleared in place and counted in stale until
	// tidyQueue drops it.
	retiringSeries int
	queue          []retireItem
	queueHead      int
	stale          int
	// funcs are gauge functions read at collection, by family and label
	// set; replaced copy-on-write (guarded by mu).
	funcs map[funcKey]func() int64
	rev   [2]revisionInfo

	producer *Producer
	// Collector-owned state.
	rtSamples   []metrics.Sample
	lastCollect time.Time
	// retiringBuf holds the retiring flags of the exported groups, copied
	// under mu at the start of each collection.
	retiringBuf []bool
}

// funcKey addresses one Node-wide gauge label set.
type funcKey struct {
	fam int
	i   int
}

// revisionInfo is one role's ruralz_config_revision_info series.
type revisionInfo struct {
	display string
	attrs   attribute.Set
	start   time.Time
}

// retireItem is one entry of the oldest-first retiring queue; g is nil
// once the group left the retiring state.
type retireItem struct {
	g *group
}

// Errors of the Node-wide accessors.
var (
	// ErrUnknownFamily is returned for a name that is not a catalog family.
	ErrUnknownFamily = errors.New("aggregate: unknown metric family")
	// ErrNotNodeFamily is returned for a family keyed by listener or
	// resource, or one the registry computes itself.
	ErrNotNodeFamily = errors.New("aggregate: not a Node-wide family")
	// ErrLabelValues is returned for label values outside the family's
	// enumerations.
	ErrLabelValues = errors.New("aggregate: label values outside the enumeration")
)

// New builds the registry of every catalog family, with every Node-wide
// enumeration label set pre-created at 0 (spec 09 req 45).
func New(o Options) (*Registry, error) {
	r := &Registry{
		clk:    o.Clock,
		limits: o.Limits.withDefaults(),
		logger: o.Logger,
		scope:  o.ScopeVersion,
		byName: make(map[string]*family),
		funcs:  make(map[funcKey]func() int64),
	}
	if r.clk == nil {
		r.clk = clock.Real()
	}
	r.stripes = o.Stripes
	if r.stripes <= 0 {
		r.stripes = min(runtime.GOMAXPROCS(0), maxStripes)
	}
	r.stripes = min(r.stripes, maxStripes)
	for i := range r.tab {
		r.tab[i] = uint8(i % r.stripes) //nolint:gosec // G115: at most 7.
	}
	if o.Codes != nil {
		r.codes = slices.Clone(o.Codes)
		slices.Sort(r.codes)
		r.codes = slices.Compact(r.codes)
		r.codeSet = make(map[string]struct{}, len(r.codes))
		for _, c := range r.codes {
			r.codeSet[c] = struct{}{}
		}
	}
	r.start = r.clk.Now()
	r.lastCollect = r.start
	for i, c := range catalog.Families() {
		f, err := newFamily(i, c)
		if err != nil {
			return nil, err
		}
		if _, dup := r.byName[c.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate family %s", errCatalog, c.Name)
		}
		r.fams = append(r.fams, f)
		r.byName[c.Name] = f
	}
	if err := r.buildNode(); err != nil {
		return nil, err
	}
	r.reserveFixedStripes()
	r.rtSamples = []metrics.Sample{
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	r.producer = &Producer{r: r, sem: make(chan struct{}, 1)}
	return r, nil
}

// validCode reports whether code may label a series.
func (r *Registry) validCode(code string) bool {
	if r.codeSet != nil {
		_, ok := r.codeSet[code]
		return ok
	}
	return syntacticCode(code)
}

// Stripes returns the stripe count S.
func (r *Registry) Stripes() int { return r.stripes }

// NewStripe implements emit.Meter: stripes round robin over S, one per
// accepted connection.
func (r *Registry) NewStripe() emit.Stripe {
	return emit.Stripe((r.rr.Add(1) - 1) % uint64(r.stripes)) //nolint:gosec // G115: below 8.
}

// Node implements emit.Meter: the Node-wide handles, available before any
// Revision.
func (r *Registry) Node() *emit.NodeMetrics { return r.node }

// fixedGroup creates a never-released group of f and makes it exported.
func (r *Registry) fixedGroup(f *family, key groupKey, striped bool, now time.Time) *group {
	g := r.newGroup(f, groupSpec{key: key, striped: striped, start: now})
	g.state = stFixed
	g.shared = newHandleSet(g)
	f.fixed = append(f.fixed, g)
	f.dirty = true
	return g
}

// nodeGroup returns the Node-wide group of the family named name.
func (r *Registry) nodeGroup(name string) *group {
	f := r.byName[name]
	if f == nil || len(f.fixed) == 0 {
		return nil
	}
	return f.fixed[0]
}

// buildNode creates the Node-wide groups and handles.
func (r *Registry) buildNode() error {
	for _, f := range r.fams {
		if f.role != roleNode {
			continue
		}
		switch f.layout {
		case layoutRevisionInfo, layoutComputed:
			continue
		case layoutValues, layoutResultCode, layoutCode:
		case layoutStatus, layoutListenerRequests, layoutAttempts:
			return fmt.Errorf("%w: Node-wide %s has a resource layout", errCatalog, f.cat.Name)
		}
		striped := f.cat.Striped && r.stripes > 1
		g := r.fixedGroup(f, groupKey{phase: -1}, striped, r.start)
		if g.striped {
			if f.hist {
				r.nodeStripedHists += g.n
			} else {
				r.nodeStripedSeries += g.n
			}
		}
	}
	n := &emit.NodeMetrics{}
	counter := func(name string, i int) emit.Counter { return r.nodeGroup(name).shared.counter(i) }
	gauge := func(name string, i int) emit.Gauge { return r.nodeGroup(name).shared.gauge(i) }
	hist := func(name string, i int) emit.Histogram { return r.nodeGroup(name).shared.hist(i) }
	n.NodeResponses = &codeH{t: r.nodeGroup(catalog.HTTPNodeResponsesTotal).code.table}
	n.GatewayDurationSkipped = counter(catalog.HTTPGatewayDurationSkippedTotal, 0)
	n.BufferedBytes = gauge(catalog.NodeBufferedBytes, 0)
	n.TapEventsDropped = counter(catalog.TapEventsDroppedTotal, 0)
	for op := range emit.NumStateOps {
		n.State.CallDuration[op] = hist(catalog.StateCallDurationSeconds, op)
		n.State.Ops[op] = counter(catalog.StateOpsTotal, op)
		for res := range emit.NumStateResults {
			n.State.Calls[op][res] = counter(catalog.StateCallsTotal, op*emit.NumStateResults+res)
		}
	}
	for k := range emit.NumWriteKinds {
		n.State.WritesDropped[k] = counter(catalog.StateWritesDroppedTotal, k)
	}
	n.State.QueueItems = gauge(catalog.StateWriteQueueItems, 0)
	act := r.nodeGroup(catalog.ConfigActivationsTotal)
	n.Config.Activated = act.shared.counter(0)
	n.Config.Rejected = &codeH{t: act.code.table}
	n.Config.RetiredSnapshots = gauge(catalog.ConfigRetiredSnapshots, 0)
	n.Config.RetirementEnded = counter(catalog.SnapshotRetirementEndedTotal, 0)
	dur := r.byName[catalog.ConfigActivationDurationSeconds]
	durGroup := r.nodeGroup(catalog.ConfigActivationDurationSeconds)
	n.Config.ActivationDuration = func(stage, sizeClass string) emit.Histogram {
		if i, ok := dur.index([]string{stage, sizeClass}); ok {
			return durGroup.shared.hist(i)
		}
		return noopHist{}
	}
	n.Config.RevisionInfo = r.setRevisionInfo
	rot := r.byName[catalog.ConfigSecretRotationFailuresTotal]
	rotGroup := r.nodeGroup(catalog.ConfigSecretRotationFailuresTotal)
	n.Config.SecretRotationFail = func(provider string) emit.Counter {
		if i, ok := rot.index([]string{provider}); ok {
			return rotGroup.shared.counter(i)
		}
		return noopCounter{}
	}
	r.node = n
	return nil
}

// reserveFixedStripes decides at start which _overflow and _unmatched
// groups are striped. Those of Striped families are hot label sets every
// request on a Node may record into, once Routes, Upstreams or Policies
// fold or when no Route matches (spec 09 req 49); a single unpadded line
// would put every core on one cache line. They count against the
// listener and enumeration-only caps like the Node-wide groups, family by
// family in catalog order and _unmatched first, so the decision never
// depends on which Revisions were admitted before.
func (r *Registry) reserveFixedStripes() {
	if r.stripes <= 1 {
		return
	}
	reserve := func(f *family, sets int) bool {
		if f.hist {
			if r.nodeStripedHists+sets > r.limits.ListenerStripedHistograms {
				return false
			}
			r.nodeStripedHists += sets
			return true
		}
		if r.nodeStripedSeries+sets > r.limits.ListenerStripedSeries {
			return false
		}
		r.nodeStripedSeries += sets
		return true
	}
	for _, f := range r.fams {
		if !f.foldable() || !f.cat.Striped {
			continue
		}
		f.unmatchedStriped = f.hasUnmatched() && reserve(f, f.hotSets())
		f.overflowStriped = reserve(f, f.hotSets()*len(f.overflowPhases()))
	}
}

// revisionRoles are the role values of ruralz_config_revision_info.
func revisionRoles() [2]string { return [2]string{"active", "lkg"} }

// setRevisionInfo sets the series of role to revision display; an empty
// display removes it. At most two series exist (spec 09 req 43).
func (r *Registry) setRevisionInfo(role, display string) {
	roles := revisionRoles()
	i := slices.Index(roles[:], role)
	if i < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rev[i].display == display {
		return
	}
	r.rev[i] = revisionInfo{display: display}
	if display != "" {
		r.rev[i].attrs = attribute.NewSet(attribute.String(labelRevision, display), attribute.String(labelRole, role))
		r.rev[i].start = r.clk.Now()
	}
}

// nodeFamily resolves a Node-wide family and label set for the accessors.
func (r *Registry) nodeFamily(name string, values []string) (*group, int, error) {
	f := r.byName[name]
	if f == nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrUnknownFamily, name)
	}
	if f.role != roleNode || f.layout != layoutValues {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotNodeFamily, name)
	}
	i, ok := f.index(values)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s%v", ErrLabelValues, name, values)
	}
	return f.fixed[0], i, nil
}

// Counter returns the handle of one label set of a Node-wide counter
// family, such as ruralz_telemetry_traces_unsampled_total{reason}; values
// are the enumeration label values in catalog order.
func (r *Registry) Counter(name string, values ...string) (emit.Counter, error) {
	g, i, err := r.nodeFamily(name, values)
	if err != nil {
		return nil, err
	}
	if g.fam.hist || g.fam.gauge {
		return nil, fmt.Errorf("%w: %s is not a counter", ErrNotNodeFamily, name)
	}
	return g.shared.counter(i), nil
}

// Gauge returns the handle of one label set of a Node-wide gauge family,
// such as ruralz_node_degraded_info{reason} or
// ruralz_security_cleartext_hops{hop}.
func (r *Registry) Gauge(name string, values ...string) (emit.Gauge, error) {
	g, i, err := r.nodeFamily(name, values)
	if err != nil {
		return nil, err
	}
	if !g.fam.gauge {
		return nil, fmt.Errorf("%w: %s is not a gauge", ErrNotNodeFamily, name)
	}
	return g.shared.gauge(i), nil
}

// GaugeFunc makes one label set of a Node-wide gauge family read f at
// collection instead of its stored value; f must be fast and must not
// block. A nil f restores the stored value.
func (r *Registry) GaugeFunc(name string, values []string, f func() int64) error {
	g, i, err := r.nodeFamily(name, values)
	if err != nil {
		return err
	}
	if !g.fam.gauge {
		return fmt.Errorf("%w: %s is not a gauge", ErrNotNodeFamily, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	funcs := make(map[funcKey]func() int64, len(r.funcs)+1)
	for k, v := range r.funcs {
		funcs[k] = v
	}
	if f == nil {
		delete(funcs, funcKey{g.fam.idx, i})
	} else {
		funcs[funcKey{g.fam.idx, i}] = f
	}
	r.funcs = funcs
	return nil
}
