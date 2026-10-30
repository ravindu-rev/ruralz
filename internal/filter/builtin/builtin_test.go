// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/basic"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/upstreamoauth2"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/filter/quota"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// newTable returns New's table on a fake clock, failing the test on error.
func newTable(t *testing.T, o Options) *filter.Registry {
	t.Helper()
	if o.Clock == nil {
		o.Clock = clocktest.New(t0)
	}
	if o.Entropy == nil {
		o.Entropy = zeros{}
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestNewServesExactlyTheServedTypes: the table holds a Factory for every
// Policy type the registry serves in this release and for no other, so a
// Revision the serve check passes never meets a type without a Factory,
// and an unserved type (RZ-CFG-040) has none.
func TestNewServesExactlyTheServedTypes(t *testing.T) {
	tab := newTable(t, Options{})
	for _, e := range registry.New().Entries() {
		f, ok := tab.Factory(e.Type)
		switch {
		case e.Served && (!ok || f == nil):
			t.Errorf("%s is served (Planned (%s)) but has no Factory", e.Type, e.Planned)
		case !e.Served && ok:
			t.Errorf("%s is not served (Planned (%s)) but has a Factory", e.Type, e.Planned)
		}
	}
	if _, ok := tab.Factory("rateLimit"); ok {
		t.Error("an unregistered spelling has a Factory")
	}
}

// TestNewComponents: the Node-wide Components, each constructed once per
// New, in foundation pack section 10 row order of their types, the order
// the supervisor starts them, each running until its context ends and
// then returning nil.
func TestNewComponents(t *testing.T) {
	tab := newTable(t, Options{})
	cs := tab.Components()
	var names []string
	for _, c := range cs {
		names = append(names, c.Name())
	}
	want := []string{
		"jwks", basic.ComponentName, ratelimit.TableName, quota.CacheName,
		revalidate.ComponentName, upstreamoauth2.ComponentName,
	}
	if !slices.Equal(names, want) {
		t.Fatalf("Components = %q, want %q", names, want)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errs := make(chan error, len(cs))
	for _, c := range cs {
		go func() { errs <- c.Run(ctx) }()
	}
	cancel()
	for range cs {
		if err := <-errs; err != nil {
			t.Errorf("Run after cancel = %v, want nil", err)
		}
	}
}

// TestNewTablesShareNothing: each New constructs its own Components, so
// two tables (two processes' worth) never share Node-wide state, and each
// table's Components list is a copy.
func TestNewTablesShareNothing(t *testing.T) {
	a, b := newTable(t, Options{}), newTable(t, Options{})
	for i, c := range a.Components() {
		if c == b.Components()[i] {
			t.Errorf("component %s is shared by two tables", c.Name())
		}
	}
	cs := a.Components()
	cs[0] = nil
	if a.Components()[0] == nil {
		t.Error("Components returns the table's own slice")
	}
}

// counterLookup records the Node-wide counters New looks up.
type counterLookup struct {
	mu   sync.Mutex
	seen []string
	fail string
}

func (l *counterLookup) counter(name string, values ...string) (emit.Counter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := name + "{" + strings.Join(values, ",") + "}"
	l.seen = append(l.seen, key)
	if key == l.fail {
		return nil, errors.New("refused")
	}
	return nopCounter{}, nil
}

// nopCounter records nothing.
type nopCounter struct{}

func (nopCounter) Add(emit.Stripe, uint64) {}

// TestNewLooksUpNodeCounters: New takes the handles of
// ruralz_auth_basic_refusals_total for each cause and of
// ruralz_cache_revalidation_drops_total for each reason from
// Options.Counter, and fails, naming the metric, when it refuses one.
func TestNewLooksUpNodeCounters(t *testing.T) {
	l := &counterLookup{}
	newTable(t, Options{Counter: l.counter})
	var want []string
	for _, c := range catalog.BasicRefusalCauses() {
		want = append(want, catalog.AuthBasicRefusalsTotal+"{"+c+"}")
	}
	for _, r := range catalog.CacheRevalidationDropReasons() {
		want = append(want, catalog.CacheRevalidationDropsTotal+"{"+r+"}")
	}
	if !slices.Equal(l.seen, want) {
		t.Errorf("looked up %q, want %q", l.seen, want)
	}
	for _, fail := range []string{want[0], want[len(want)-1]} {
		_, err := New(Options{Clock: clocktest.New(t0), Entropy: zeros{}, Counter: (&counterLookup{fail: fail}).counter})
		metric, _, _ := strings.Cut(fail, "{")
		if err == nil || !strings.Contains(err.Error(), metric) {
			t.Errorf("Counter refusing %s: New error %v, want one naming %s", fail, err, metric)
		}
	}
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestNewFailsWithoutThrottleKey: a throttle key that cannot be read is
// New's error; no table is returned.
func TestNewFailsWithoutThrottleKey(t *testing.T) {
	tab, err := New(Options{Clock: clocktest.New(t0), Entropy: failingReader{}})
	if err == nil || tab != nil || !strings.Contains(err.Error(), "throttle key") {
		t.Fatalf("New = %v, %v, want the throttle key error", tab, err)
	}
}

// TestNewDefaults: a zero Options builds the whole table on the real
// clock, crypto/rand and the runtime generator, with no Status or
// counters.
func TestNewDefaults(t *testing.T) {
	tab, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []v1alpha1.PolicyType{v1alpha1.PolicyTypeAuthJWT, v1alpha1.PolicyTypeCache, v1alpha1.PolicyTypeAuthBasic} {
		if _, ok := tab.Factory(typ); !ok {
			t.Errorf("%s: no Factory", typ)
		}
	}
}

// racySource is a rand.Source whose state a concurrent call would race on.
type racySource struct{ n uint64 }

func (s *racySource) Uint64() uint64 {
	s.n++
	return s.n
}

// TestLockedSourceSerializes: the Source of Options.Rand, shared by the
// cache Factory and the revalidation workers, is called one call at a
// time whatever the Source (the race detector checks it), and draws its
// values in order.
func TestLockedSourceSerializes(t *testing.T) {
	src := &lockedSource{src: &racySource{}}
	var wg sync.WaitGroup
	const goroutines, draws = 8, 100
	for range goroutines {
		wg.Go(func() {
			for range draws {
				src.Uint64()
			}
		})
	}
	wg.Wait()
	if got := src.Uint64(); got != goroutines*draws+1 {
		t.Errorf("next value = %d, want %d", got, goroutines*draws+1)
	}
	var _ rand.Source = src
}
