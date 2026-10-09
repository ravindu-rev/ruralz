// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package retire publishes compiled snapshots and retires them (spec 04
// group H, architecture R-17 and R-58). A Holder keeps the active
// *snapshot.Snapshot in one atomic.Pointer that every request pins with the
// lock-free protocol of spec 04 req 50: load the pointer, increment the
// request's stripe, re-load the pointer and, when it changed, undo and
// retry. Publish swaps the pointer and retires the previous snapshot; at
// most K = 2 snapshots stay retired, plus at most one closing or ending
// (req 52). A closing snapshot gets a 30 s grace period, after which it is
// ending: the ending protocol (req 53) walks its pinned requests with
// bounded goroutines, marks each ended, moves its connection deadlines and
// cancels its root context, so the handler answers 503 RZ-RT-014 before
// commit or the connection is reset after it. A snapshot is freed at zero
// pins: the retirer calls Binding.Release (09 req 56), closes the resources
// no other live snapshot shares and releases its State Store handles (req
// 51). Memory is left to the garbage collector.
//
// The Drain deadline reuses the ending protocol through EndAll with
// RZ-RT-016 (spec 04 req 63). Retirement is driven by Run, which owns every
// goroutine the Holder starts; Close retires the last snapshot at shutdown
// and waits until every snapshot is freed. At the Drain deadline the
// supervisor calls EndAll(ctx, snapshot.EndDrain) first and Close second:
// EndAll ends every pinned request and every request that pins after it,
// so Close waits only for handlers that are already answering.
//
// Resources and State Store handles follow one rule (architecture 3.3
// step 9): an instance is closed or released exactly once, when the last
// live snapshot holding it is freed; instances are compared by identity,
// and one instance set as both StateStore and CacheStore counts once.
// Compile therefore either opens a new snapshot.StoreHandle per snapshot
// and role (one reference each) or carries the previous snapshot's
// instance over without retaining it again; retaining a carried-over
// instance once per snapshot would leave references that are never
// released, and the old driver would never close.
//
// Every Publish takes a freshly compiled snapshot: a snapshot that is live
// or was freed (its resources closed, its Binding released) is rejected
// with ErrPublished.
package retire

import (
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Defaults (spec 04 reqs 50-53; all "target" values of DP "Why no
// in-flight request is dropped").
const (
	// DefaultK is the number of retired snapshots kept before the oldest
	// becomes closing.
	DefaultK = 2
	// DefaultGrace is the grace period of a closing snapshot.
	DefaultGrace = 30 * time.Second
	// DefaultEndingBound is the time in which an ending snapshot is expected
	// to reach zero pins: 5 s plus the largest Plugin limits.timeout (0 in
	// M1) plus 1 s. Past it the snapshot is overdue (snapshot_ending_overdue).
	DefaultEndingBound = 6 * time.Second
	// DefaultWriteSlack is how far ahead the ending protocol sets the write
	// deadline of an uncommitted request, so the handler can still write
	// its 503.
	DefaultWriteSlack = 5 * time.Second
	// DefaultPollInterval is the fallback pin poll while a retired snapshot
	// is still pinned; Unpin wakes the retirer at once, so polling only
	// covers pins removed without the Holder.
	DefaultPollInterval = 50 * time.Millisecond
	// DefaultMaxEnding bounds the ending callbacks running at once: the
	// Node in-flight ceiling (spec 04 req 19), which already bounds the
	// number of pinned requests.
	DefaultMaxEnding = 20_000
	// MaxStripes caps the pin and metric stripe count S.
	MaxStripes = 8
)

// statusSource is the NodeStatus source of snapshot_ending_overdue.
const statusSource = "retire"

// Config configures a Holder. Zero values select the defaults.
type Config struct {
	// Clock drives grace, ending and overdue timers and the deadlines of
	// the ending protocol; nil selects clock.Real().
	Clock clock.Clock
	// Logger receives transition logs; it comes from internal/telemetry.
	// nil discards them.
	Logger *slog.Logger
	// Stripes is the stripe count S; 0 selects StripeCount(). Values are
	// clamped to [1, 255] (emit.Stripe is a byte). The wiring passes the
	// meter's stripe count, so pin and metric stripes coincide (spec 04
	// req 50 reuses the metric stripes).
	Stripes int
	// NewStripe is the connection stripe source behind ConnStripe; the
	// wiring passes emit.Meter.NewStripe, so pins and metrics share one
	// round robin. nil selects the Holder's own round robin over Stripes.
	NewStripe func() emit.Stripe
	// K is the number of retired snapshots kept; <= 0 selects DefaultK.
	K int
	// Grace is the grace period of a closing snapshot; <= 0 selects
	// DefaultGrace.
	Grace time.Duration
	// EndingBound is the overdue bound of an ending snapshot; <= 0 selects
	// DefaultEndingBound.
	EndingBound time.Duration
	// WriteSlack is the write deadline slack of an uncommitted ended
	// request; <= 0 selects DefaultWriteSlack.
	WriteSlack time.Duration
	// PollInterval is the fallback pin poll; <= 0 selects
	// DefaultPollInterval.
	PollInterval time.Duration
	// MaxEnding bounds the concurrent ending callbacks; <= 0 selects
	// DefaultMaxEnding.
	MaxEnding int
	// RetiredSnapshots is ruralz_config_retired_snapshots: the snapshots
	// retired, closing or ending that are not freed yet. nil records
	// nothing.
	RetiredSnapshots emit.Gauge
	// RetirementEnded is ruralz_snapshot_retirement_ended_total: requests
	// ended by the grace end (RZ-RT-014). nil records nothing.
	RetirementEnded emit.Counter
	// Status raises and clears snapshot_ending_overdue; nil reports
	// nothing.
	Status emit.NodeStatus
	// OnClosing ends the streams of a snapshot that becomes closing
	// (WebSocket 1001, gRPC UNAVAILABLE, SSE retry hint). No stream kinds
	// exist in M1, so it is nil there; M3 sets the stream registry's hook.
	// It runs on a goroutine the Holder owns.
	OnClosing func(*snapshot.Snapshot)
}

// StripeCount returns S = min(GOMAXPROCS, MaxStripes), at least 1: the pin
// and metric stripe count (spec 04 req 50). Call it once at start.
func StripeCount() int { return min(max(runtime.GOMAXPROCS(0), 1), MaxStripes) }

// Holder publishes the active snapshot and retires the previous ones. Pin,
// Unpin, Active, ConnStripe and RequestStripe are the request path and
// allocate nothing. In the steady state they take only the per-stripe list
// lock of snapshot.Pins. While an ending walk runs, Pin's retry and Unpin
// also take the Holder-wide walkSet lock, and Unpin waits for an ending
// callback acting on its record; after EndAll, Pin takes rec.Mu and calls
// rec.Cancel to end the request as it pins. The caller therefore must not
// hold rec.Mu around Pin or Unpin. Every other method runs off the request
// path. A Holder is safe for concurrent use.
type Holder struct {
	cur     atomic.Pointer[snapshot.Snapshot]
	walking atomic.Int32 // ending walks in progress
	drained atomic.Bool  // set by the first EndAll (Drain deadline)
	kick    chan struct{}
	stripes int
	_       [32]byte // keep the connection counter off the read-mostly line

	rr atomic.Uint32 // round-robin connection stripes

	newStripe   func() emit.Stripe
	clock       clock.Clock
	log         *slog.Logger
	k           int
	grace       time.Duration
	endingBound time.Duration
	writeSlack  time.Duration
	poll        time.Duration
	sem         chan struct{}
	retiredG    emit.Gauge
	endedC      emit.Counter
	status      emit.NodeStatus
	onClosing   func(*snapshot.Snapshot)

	ws    walkSet
	freed tombstones

	mu       sync.Mutex
	live     []*tracked // activation order, oldest first; the active one last
	closed   bool
	degraded bool
	tasks    int
	changed  chan struct{} // closed and replaced on every state change
}

// New returns a Holder with no published snapshot.
func New(cfg Config) *Holder {
	h := &Holder{
		kick:        make(chan struct{}, 1),
		stripes:     cfg.Stripes,
		newStripe:   cfg.NewStripe,
		clock:       cfg.Clock,
		log:         cfg.Logger,
		k:           cfg.K,
		grace:       cfg.Grace,
		endingBound: cfg.EndingBound,
		writeSlack:  cfg.WriteSlack,
		poll:        cfg.PollInterval,
		retiredG:    cfg.RetiredSnapshots,
		endedC:      cfg.RetirementEnded,
		status:      cfg.Status,
		onClosing:   cfg.OnClosing,
		changed:     make(chan struct{}),
	}
	if h.stripes <= 0 {
		h.stripes = StripeCount()
	}
	h.stripes = min(h.stripes, 255)
	if h.clock == nil {
		h.clock = clock.Real()
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.k <= 0 {
		h.k = DefaultK
	}
	if h.grace <= 0 {
		h.grace = DefaultGrace
	}
	if h.endingBound <= 0 {
		h.endingBound = DefaultEndingBound
	}
	if h.writeSlack <= 0 {
		h.writeSlack = DefaultWriteSlack
	}
	if h.poll <= 0 {
		h.poll = DefaultPollInterval
	}
	maxEnding := cfg.MaxEnding
	if maxEnding <= 0 {
		maxEnding = DefaultMaxEnding
	}
	h.sem = make(chan struct{}, maxEnding)
	if h.retiredG == nil {
		h.retiredG = nopGauge{}
	}
	if h.endedC == nil {
		h.endedC = nopCounter{}
	}
	h.ws.cond.L = &h.ws.mu
	return h
}

// Stripes returns the stripe count S.
func (h *Holder) Stripes() int { return h.stripes }

// ConnStripe returns the stripe of a newly accepted connection, assigned
// round robin (spec 04 req 50): Config.NewStripe when set, the Holder's
// own round robin otherwise. The handler takes one value per connection
// here and uses it for both its pins and its metric handles; it never
// calls emit.Meter.NewStripe itself, so the two cannot diverge.
func (h *Holder) ConnStripe() emit.Stripe {
	if h.newStripe != nil {
		return h.newStripe()
	}
	return emit.Stripe((h.rr.Add(1) - 1) % uint32(h.stripes)) //nolint:gosec // G115: the modulus is at most 255.
}

// RequestStripe returns the stripe of one request on a connection: the
// connection's stripe offset by the HTTP/2 stream ID (client streams are
// odd, so the offset is streamID/2) or by any per-connection request
// sequence; HTTP/1.1 passes 0.
func (h *Holder) RequestStripe(conn emit.Stripe, streamID uint32) emit.Stripe {
	return emit.Stripe((uint32(conn) + streamID/2) % uint32(h.stripes)) //nolint:gosec // G115: the modulus is at most 255.
}

// Active returns the published snapshot without pinning it, or nil before
// the first Publish and after Close. A caller that keeps using it beyond
// one short read (a TLS handshake reading its listener settings) must pin
// it instead.
func (h *Holder) Active() *snapshot.Snapshot { return h.cur.Load() }

// Pin pins the active snapshot for one request on stripe and registers rec
// for the ending protocol (spec 04 reqs 50 and 53); it returns nil when no
// snapshot is published. rec.Cancel and rec.RC must be set before Pin and
// stay unchanged until Unpin returns, and the caller must not hold rec.Mu.
// Pin allocates nothing: it loads the pointer, adds rec to the snapshot's
// stripe, re-loads the pointer and, when a Publish swapped it in between,
// removes rec and retries.
//
// After EndAll a request is ended as it pins (spec 04 req 63): Pin marks
// rec with EndDrain, moves its deadlines and cancels its context before it
// returns, so the handler answers 503 with RZ-RT-016 without starting any
// work.
//
// A retry removes rec from a snapshot the retirer may already be ending;
// that happens only when the pointer was loaded before a retirement and
// re-loaded after its whole grace period, and the request then ends like
// every request pinned at grace end.
func (h *Holder) Pin(stripe emit.Stripe, rec *snapshot.PinnedRequest) *snapshot.Snapshot {
	for {
		s := h.cur.Load()
		if s == nil {
			return nil
		}
		s.Pins.Add(int(stripe), rec)
		if h.cur.Load() == s {
			if h.drained.Load() {
				h.endAtPin(rec)
			}
			return s
		}
		s.Pins.Remove(int(stripe), rec)
		if h.walking.Load() != 0 {
			h.ws.detach(rec)
		}
		h.wake()
	}
}

// Unpin releases the pin Pin returned, with the same stripe and record,
// after onLog (spec 04 req 34). The handler must call it before its
// ServeHTTP returns and never while holding rec.Mu: when an ending callback
// is acting on rec, Unpin waits for it, so no callback touches rec.RC after
// the handler returned or rec after it went back to its pool. Unpinning a
// retired snapshot wakes the retirer, which frees it at zero pins.
func (h *Holder) Unpin(s *snapshot.Snapshot, stripe emit.Stripe, rec *snapshot.PinnedRequest) {
	s.Pins.Remove(int(stripe), rec)
	if h.walking.Load() != 0 {
		h.ws.detach(rec)
	}
	if h.cur.Load() != s {
		h.wake()
	}
}

// wake nudges Run without blocking.
func (h *Holder) wake() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// State is the lifecycle state of a live snapshot (DP figure 4); a freed
// snapshot is no longer tracked.
type State uint8

// States.
const (
	// StateActive: new requests pin this snapshot.
	StateActive State = iota
	// StateRetired: a newer snapshot is active; pinned work finishes.
	StateRetired
	// StateClosing: over K retired; streams ended, grace running.
	StateClosing
	// StateEnding: grace ended; pinned requests were ended.
	StateEnding
)

// String returns the /debug/snapshots state name.
func (s State) String() string {
	switch s {
	case StateActive:
		return adminapi.StateActive
	case StateRetired:
		return adminapi.StateRetired
	case StateClosing:
		return adminapi.StateClosing
	case StateEnding:
		return adminapi.StateEnding
	default:
		return ""
	}
}

// tracked is the retirer's record of one live snapshot; guarded by
// Holder.mu.
type tracked struct {
	s           *snapshot.Snapshot
	state       State
	activatedAt time.Time
	retiredAt   time.Time
	graceEndsAt time.Time
	overdueAt   time.Time
	overdue     bool
}

type nopGauge struct{}

func (nopGauge) Add(emit.Stripe, int64) {}
func (nopGauge) Set(int64)              {}

type nopCounter struct{}

func (nopCounter) Add(emit.Stripe, uint64) {}
