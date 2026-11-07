// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"hash/maphash"
	"log/slog"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// binding is one plugin Policy's view of the host, which every call of the
// Policy's Filter carries: what it may do, where its state lives and the
// budgets it shares across requests. It is immutable but for its
// synchronized limiters and event names.
type binding struct {
	// policy is the Policy's metadata.name, plugin its Plugin's.
	policy, plugin string
	// class is the Policy's Filter class: consumer_info needs a class after
	// auth in onRequestHeaders.
	class phase.Class
	// caps are the granted Capabilities.
	caps capSet
	// creds is the Revision's credential-header set; hideAuthorization
	// keeps authorization hidden in upstream-leg Phases even with
	// credentials.read, since an upstream-auth Filter of a leg the Policy
	// runs in sets it.
	creds             *redact.Set
	hideAuthorization bool
	// limits are the Node's body limits.
	limits filter.Limits
	// state is the State Store binding; nil without state.read and
	// state.write.
	state *stateBinding
	// events are the guest event names metric_add bound, at most 16.
	events *eventNames
	// logs is the Policy's 1 MiB/s guest log budget.
	logs   *logLimiter
	logger *slog.Logger
	// tel receives the Policy's Plugin telemetry.
	tel Telemetry
}

// stateBinding is where a Policy's guest state lives: the rzplg: keys of
// its Environment namespace on the main State Store connection.
type stateBinding struct {
	namespace string
	store     statestore.Store
	enqueuer  statestore.Enqueuer
	timeout   time.Duration
	keys      *keyRate
}

// perRequest is a plugin Policy's state for one request: its context
// entries, its one blocking State Store call and its one onLog write. A
// Gateway or Route Policy keeps it in its PolicyState slot; an Upstream
// Policy in its request-wide shared slot, so later attempts and steps of
// the request replay its State Store result.
type perRequest struct {
	ctx      map[string][]byte
	ctxBytes int
	// called marks the one blocking State Store call; replay holds its
	// result for the upstream-leg replay.
	called bool
	replay stateReplay
	// logWrite marks the one onLog write.
	logWrite bool
}

// stateReplay is the first blocking State Store result of a request.
type stateReplay struct {
	fn    string
	key   string
	found bool
	value []byte
	n     int64
	err   *statestore.Error
}

// logLimiter is a Policy's guest log budget per second, on the Node clock.
type logLimiter struct {
	clk    clock.Clock
	mu     sync.Mutex
	window time.Time
	used   int
}

// admit charges n bytes to the current second and reports whether they
// fit in logRateBytes.
func (l *logLimiter) admit(n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if now.Sub(l.window) >= time.Second || now.Before(l.window) {
		l.window, l.used = now, 0
	}
	if l.used+n > logRateBytes {
		return false
	}
	l.used += n
	return true
}

// eventNames binds a Policy's guest event names first come, at most
// metricNames of them.
type eventNames struct {
	mu    sync.Mutex
	names []string
}

// bind returns the index of name, binding it when room is left; false for
// a name past the limit.
func (e *eventNames) bind(name string) (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, n := range e.names {
		if n == name {
			return i, true
		}
	}
	if len(e.names) >= metricNames {
		return 0, false
	}
	e.names = append(e.names, name)
	return len(e.names) - 1, true
}

// keyRate counts the distinct state keys a Policy touches per minute on
// this Node, at most stateKeysPerMinute.
type keyRate struct {
	clk    clock.Clock
	seed   maphash.Seed
	mu     sync.Mutex
	window time.Time
	seen   map[uint64]struct{}
}

// newKeyRate returns an empty counter.
func newKeyRate(clk clock.Clock) *keyRate {
	return &keyRate{clk: clk, seed: maphash.MakeSeed(), seen: map[uint64]struct{}{}}
}

// admit records key and reports whether it fits this minute's budget; a
// key already seen this minute always fits.
func (k *keyRate) admit(key string) bool {
	h := maphash.String(k.seed, key)
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.clk.Now()
	if now.Sub(k.window) >= time.Minute || now.Before(k.window) {
		k.window = now
		clear(k.seen)
	}
	if _, ok := k.seen[h]; ok {
		return true
	}
	if len(k.seen) >= stateKeysPerMinute {
		return false
	}
	k.seen[h] = struct{}{}
	return true
}
