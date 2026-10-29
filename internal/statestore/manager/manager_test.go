// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// TestAcquireSharesUnchangedDriver checks the Hot Reload rule: a snapshot
// whose State Store configuration is unchanged gets a new handle on the
// open driver, and the driver closes only when the last snapshot holding
// it releases its handle.
func TestAcquireSharesUnchangedDriver(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	old := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	cur := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	if n := len(o.drivers()); n != 1 {
		t.Fatalf("opened %d drivers, want 1", n)
	}
	if old.Main == cur.Main {
		t.Error("two snapshots share one Handle; each needs its own reference")
	}
	if old.Main.Store() != cur.Main.Store() || old.Main.Enqueuer() != cur.Main.Enqueuer() {
		t.Error("the unchanged configuration got another driver")
	}
	d := driverOf(t, cur.Main)
	old.Main.Release()
	if f.entries() != 1 || d.closed.Load() {
		t.Fatal("releasing the old snapshot closed the driver the new one holds")
	}
	cur.Main.Release()
	waitFor(t, "the driver close", func() bool { return d.closes.Load() == 1 })
	if f.entries() != 0 {
		t.Errorf("%d drivers left open", f.entries())
	}
}

// TestDriverKeptWhenOnlyTimeoutOrSourceChanges checks that the key is the
// role, the driver and, for redis, topology and URL: a new timeout or a
// configuration moving from the fallback to an explicit memory driver
// keeps the driver, so the memory driver keeps its counters.
func TestDriverKeptWhenOnlyTimeoutOrSourceChanges(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	fallback := f.acquire(t, memorySpec(statestore.SourceFallback))
	explicit := memorySpec(statestore.SourceGateway)
	explicit.Main.DefaultTimeout = 20 * time.Millisecond
	gw := f.acquire(t, explicit)
	slow := redisSpec("redis://127.0.0.1:6379")
	r1 := f.acquire(t, slow)
	slow.Main.DefaultTimeout = time.Second
	r2 := f.acquire(t, slow)
	if n := len(o.drivers()); n != 2 {
		t.Fatalf("opened %d drivers, want 2 (one memory, one redis)", n)
	}
	if fallback.Main.Store() != gw.Main.Store() || r1.Main.Store() != r2.Main.Store() {
		t.Error("a timeout or source change opened another driver")
	}
}

// TestChangedConfigOpensNewDriver checks that a changed URL, topology or
// driver opens a new driver through its opener before the swap, while the
// old driver serves the old snapshot until it is released.
func TestChangedConfigOpensNewDriver(t *testing.T) {
	cluster := redisSpec("redis://127.0.0.1:6379")
	cluster.Main.Topology = statestore.TopologyCluster
	tests := []struct {
		name     string
		old, cur Spec
	}{
		{"url", redisSpec("redis://127.0.0.1:6379"), redisSpec("redis://127.0.0.2:6379")},
		{"topology", redisSpec("redis://127.0.0.1:6379"), cluster},
		{"driver", memorySpec(statestore.SourceGateway), redisSpec("redis://127.0.0.1:6379")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &opener{}
			f := newFixture(t, o.openers())
			old := f.acquire(t, tt.old)
			cur := f.acquire(t, tt.cur)
			ds := o.drivers()
			if len(ds) != 2 || old.Main.Store() == cur.Main.Store() {
				t.Fatalf("opened %d drivers; the change must open a new one", len(ds))
			}
			if ds[1].cfg.Driver != tt.cur.Main.Driver || ds[1].cfg.Topology != tt.cur.Main.Topology || ds[1].cfg.URL != tt.cur.Main.URL {
				t.Errorf("new driver opened with %+v", ds[1].cfg)
			}
			if ds[0].closed.Load() {
				t.Fatal("the old driver closed while its snapshot holds it")
			}
			old.Main.Release()
			waitFor(t, "the old driver close", func() bool { return ds[0].closes.Load() == 1 })
			if ds[1].closed.Load() {
				t.Error("the new driver closed")
			}
		})
	}
}

// TestCacheHandleWithoutCacheConnection checks that without
// stateStore.cache the Response Cache handle is the Main handle, one
// reference released once.
func TestCacheHandleWithoutCacheConnection(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	hs := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	if hs.Cache != hs.Main {
		t.Fatal("Cache is not the Main handle")
	}
	d := driverOf(t, hs.Main)
	if got := hs.Main.e.refs.Load(); got != 1 {
		t.Errorf("references = %d, want 1", got)
	}
	hs.Main.Release()
	hs.Cache.Release() // the same instance: nothing more
	waitFor(t, "the driver close", func() bool { return d.closes.Load() == 1 })
}

// TestCacheConnection checks that stateStore.cache opens a second driver
// with the cache role, handed out as its own handle, each driver closing
// when its own handle is released.
func TestCacheConnection(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	spec := redisSpec("redis://127.0.0.1:6379")
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	hs := f.acquire(t, spec)
	if hs.Cache == hs.Main || hs.Cache.Store() == hs.Main.Store() {
		t.Fatal("the cache connection shares the main handle")
	}
	mainD, cacheD := driverOf(t, hs.Main), driverOf(t, hs.Cache)
	if mainD.cfg.Role != statestore.RoleMain || cacheD.cfg.Role != statestore.RoleCache {
		t.Errorf("roles = %d, %d; want main, cache", mainD.cfg.Role, cacheD.cfg.Role)
	}
	hs.Main.Release()
	waitFor(t, "the main driver close", func() bool { return mainD.closes.Load() == 1 })
	if cacheD.closed.Load() {
		t.Error("releasing Main closed the cache driver")
	}
	hs.Cache.Release()
	waitFor(t, "the cache driver close", func() bool { return cacheD.closes.Load() == 1 })
}

// TestHandlesRelease checks that Handles.Release gives back one reference
// per handle, the shared Main and Cache handle once, so a snapshot that
// is never published leaves no driver open.
func TestHandlesRelease(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	shared := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	split := redisSpec("redis://127.0.0.1:6379")
	split.Cache = cacheConfig("redis://127.0.0.1:6380")
	both := f.acquire(t, split)
	shared.Release()
	if got := both.Main.e.refs.Load(); got != 1 {
		t.Fatalf("main references = %d after releasing the shared handles, want 1", got)
	}
	both.Release()
	for _, d := range o.drivers() {
		waitFor(t, "driver "+d.name+" close", func() bool { return d.closes.Load() == 1 })
	}
	(Handles{}).Release() // nothing to release
}

// TestReleaseTwice checks that a second Release of one handle does
// nothing, so it never drops another snapshot's reference.
func TestReleaseTwice(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	a := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	b := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	a.Main.Release()
	a.Main.Release()
	if got := b.Main.e.refs.Load(); got != 1 || driverOf(t, b.Main).closed.Load() {
		t.Errorf("after a double Release: references %d, closed %v", got, driverOf(t, b.Main).closed.Load())
	}
}

// TestRetiredDriverNotReused checks that once a driver's last reference is
// gone it is never handed out again: the next snapshot opens a new one.
func TestRetiredDriverNotReused(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	first := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	first.Main.Release()
	again := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	if ds := o.drivers(); len(ds) != 2 || again.Main.Store() != ds[1] {
		t.Fatalf("opened %d drivers; a released driver was reused", len(ds))
	}
	if e := first.Main.e; e.Hold() {
		t.Error("Hold revived a retired driver")
	}
}

// TestAcquireErrors checks that Acquire fails without a driver opener,
// with the opener's error, and after Close, and that a failed cache open
// leaves no reference on the main driver.
func TestAcquireErrors(t *testing.T) {
	openErr := errors.New("bad url")
	t.Run("no opener", func(t *testing.T) {
		o := &opener{}
		f := newFixture(t, Openers{Memory: o.open})
		if _, err := f.m.Acquire(t.Context(), redisSpec("redis://127.0.0.1:6379")); !errors.Is(err, ErrNoOpener) {
			t.Errorf("Acquire = %v, want ErrNoOpener", err)
		}
	})
	t.Run("opener error", func(t *testing.T) {
		o := &opener{err: openErr}
		f := newFixture(t, o.openers())
		if _, err := f.m.Acquire(t.Context(), redisSpec("redis://127.0.0.1:6379")); !errors.Is(err, openErr) {
			t.Errorf("Acquire = %v, want the opener's error", err)
		}
		if f.entries() != 0 {
			t.Errorf("%d drivers left open", f.entries())
		}
	})
	t.Run("cache opener error", func(t *testing.T) {
		mem := &opener{}
		f := newFixture(t, Openers{Memory: mem.open, Redis: (&opener{err: openErr}).open})
		spec := memorySpec(statestore.SourceGateway)
		spec.Cache = cacheConfig("redis://127.0.0.1:6380")
		if _, err := f.m.Acquire(t.Context(), spec); !errors.Is(err, openErr) {
			t.Fatalf("Acquire = %v, want the cache opener's error", err)
		}
		ds := mem.drivers()
		if len(ds) != 1 {
			t.Fatalf("opened %d memory drivers", len(ds))
		}
		waitFor(t, "the main driver close", func() bool { return ds[0].closes.Load() == 1 })
		if f.entries() != 0 {
			t.Errorf("%d drivers left open", f.entries())
		}
	})
	t.Run("closed", func(t *testing.T) {
		o := &opener{}
		f := newFixture(t, o.openers())
		if err := f.m.Close(t.Context()); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if _, err := f.m.Acquire(t.Context(), redisSpec("redis://127.0.0.1:6379")); !errors.Is(err, ErrClosed) {
			t.Errorf("Acquire after Close = %v, want ErrClosed", err)
		}
	})
}

// TestQueuedWriteKeepsDriverOpen checks that a write queued for a driver
// keeps it open after its snapshot's handle is released, and that the
// driver closes once the write is done.
func TestQueuedWriteKeepsDriverOpen(t *testing.T) {
	o := &opener{setup: func(d *driver) {
		d.blockWrite, d.writeEntered = make(chan struct{}), make(chan struct{}, 4)
	}}
	f := newFixture(t, o.openers())
	hs := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	d := driverOf(t, hs.Main)
	if !hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("r1")) {
		t.Fatal("Enqueue refused")
	}
	<-d.writeEntered
	hs.Main.Release()
	if d.closed.Load() || hs.Main.e.refs.Load() != 1 {
		t.Fatalf("closed %v, references %d: the queued write must hold the driver", d.closed.Load(), hs.Main.e.refs.Load())
	}
	close(d.blockWrite)
	waitFor(t, "the driver close", func() bool { return d.closes.Load() == 1 })
	if got := d.written(); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("written %v, want [r1]", got)
	}
	if d.lateWrites.Load() != 0 {
		t.Error("a write reached the driver after its close")
	}
}

// TestWritesReachTheirDriver checks that each handle's Enqueuer queues for
// its own driver: writes of the old and the new snapshot go to the store
// each Policy used.
func TestWritesReachTheirDriver(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	old := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	cur := f.acquire(t, redisSpec("redis://127.0.0.2:6379"))
	if !old.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("old")) ||
		!cur.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("new")) {
		t.Fatal("Enqueue refused")
	}
	oldD, curD := driverOf(t, old.Main), driverOf(t, cur.Main)
	waitFor(t, "both writes", func() bool { return len(oldD.written()) == 1 && len(curD.written()) == 1 })
	if oldD.written()[0] != "old" || curD.written()[0] != "new" {
		t.Errorf("old driver got %v, new driver %v", oldD.written(), curD.written())
	}
}

// TestCloseFlushesThenClosesDrivers checks the shutdown order: Close
// writes the queued writes, then closes every driver, a still-held
// handle's driver included; Release after Close does nothing more.
func TestCloseFlushesThenClosesDrivers(t *testing.T) {
	ev := &events{}
	o := &opener{events: ev, setup: func(d *driver) { d.blockWrite = make(chan struct{}) }}
	f := newFixture(t, o.openers())
	hs := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	d := driverOf(t, hs.Main)
	for i := range 3 {
		if !hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("w"+strconv.Itoa(i))) {
			t.Fatal("Enqueue refused")
		}
	}
	done := make(chan error, 1)
	go func() { done <- f.m.Close(context.Background()) }()
	close(d.blockWrite)
	if err := <-done; err != nil {
		t.Fatalf("Close = %v", err)
	}
	got := ev.list()
	if len(got) != 4 || got[3] != "close redis://127.0.0.1:6379" {
		t.Errorf("events %v, want three writes then the close", got)
	}
	hs.Main.Release()
	if d.closes.Load() != 1 {
		t.Errorf("driver closed %d times, want once", d.closes.Load())
	}
}

// TestCloseContextCutsDriverCloses checks that when Close's context ends
// before the drivers close, their closes are cut short and Close returns
// the context's error.
func TestCloseContextCutsDriverCloses(t *testing.T) {
	o := &opener{setup: func(d *driver) { d.blockClose = true }}
	f := newFixture(t, o.openers())
	f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Close = %v, want context.Canceled", err)
	}
	if d := o.drivers()[0]; d.closes.Load() != 1 {
		t.Errorf("driver closed %d times", d.closes.Load())
	}
}

// TestCloseReturnsDriverErrors checks that a driver failing to close at
// shutdown makes Close return its error.
func TestCloseReturnsDriverErrors(t *testing.T) {
	closeErr := errors.New("connection reset")
	o := &opener{setup: func(d *driver) { d.closeErr = closeErr }}
	f := newFixture(t, o.openers())
	f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	if err := f.m.Close(context.Background()); !errors.Is(err, closeErr) {
		t.Errorf("Close = %v, want the driver's error", err)
	}
	if err := f.m.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestMemoryFallbackReason checks state_store_memory_fallback and the
// startup warning: raised with one warning when a snapshot first uses the
// memory fallback, kept while any snapshot does, not raised for an
// explicit memory driver, and cleared when the last fallback snapshot is
// released.
func TestMemoryFallbackReason(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	r := catalog.ReasonStateStoreMemoryFallback
	first := f.acquire(t, memorySpec(statestore.SourceFallback))
	reload := f.acquire(t, memorySpec(statestore.SourceFallback))
	if !f.status.on(r) {
		t.Fatal("state_store_memory_fallback not raised")
	}
	if n := f.logs.count(warnFallback); n != 1 {
		t.Errorf("fallback warnings = %d, want 1", n)
	}
	first.Main.Release()
	explicit := f.acquire(t, memorySpec(statestore.SourceGateway))
	if !f.status.on(r) {
		t.Fatal("the reason cleared while a fallback snapshot is live")
	}
	reload.Main.Release()
	if f.status.on(r) {
		t.Error("an explicit memory driver holds state_store_memory_fallback")
	}
	if explicit.Main.Store() != reload.Main.Store() || len(o.drivers()) != 1 {
		t.Error("the fallback and the explicit memory driver are not one store")
	}
	f.acquire(t, memorySpec(statestore.SourceFallback))
	if n := f.logs.count(warnFallback); n != 2 {
		t.Errorf("fallback warnings = %d, want 2 after the fallback returned", n)
	}
}

// TestBreakerReasonFollowsStatus checks that state_store_breaker_open
// follows the drivers' Status at each poll.
func TestBreakerReasonFollowsStatus(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	hs := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	d := driverOf(t, hs.Main)
	r := catalog.ReasonStateStoreBreakerOpen
	for _, open := range []bool{true, false, true} {
		d.setStatus(statestore.Status{BreakerNotClosed: open, EvictionPolicy: statestore.Yes})
		waitFor(t, "the status timer", func() bool { return f.clk.Pending() == 1 })
		f.clk.Advance(DefaultStatusInterval)
		waitFor(t, "the breaker reason", func() bool { return f.status.on(r) == open })
	}
}

// TestStatusPollInterval checks that drivers are polled every 1 s
// ("State client and RZ-STS error codes"), and not before.
func TestStatusPollInterval(t *testing.T) {
	if DefaultStatusInterval != time.Second {
		t.Fatalf("DefaultStatusInterval = %v", DefaultStatusInterval)
	}
	o := &opener{}
	f := newFixture(t, o.openers())
	hs := f.acquire(t, redisSpec("redis://127.0.0.1:6379"))
	d := driverOf(t, hs.Main)
	base := d.statusCalls.Load()
	waitFor(t, "the status timer", func() bool { return f.clk.Pending() == 1 })
	f.clk.Advance(DefaultStatusInterval - time.Millisecond)
	if d.statusCalls.Load() != base {
		t.Fatal("polled before the interval")
	}
	f.clk.Advance(time.Millisecond)
	waitFor(t, "a poll", func() bool { return d.statusCalls.Load() == base+1 })
}

// TestEvictionReasonNeedsLimitPolicies checks that
// state_store_eviction_policy is raised only while a ratelimit or quota
// Policy uses a store that reports another maxmemory-policy than
// noeviction, and never for an unknown policy.
func TestEvictionReasonNeedsLimitPolicies(t *testing.T) {
	r := catalog.ReasonStateStoreEvictionPolicy
	t.Run("no", func(t *testing.T) {
		o := &opener{setup: func(d *driver) { d.status.EvictionPolicy = statestore.No }}
		f := newFixture(t, o.openers())
		spec := redisSpec("redis://127.0.0.1:6379")
		spec.Caches = true
		f.acquire(t, spec)
		if f.status.on(r) {
			t.Fatal("raised without a limit Policy")
		}
		spec.Limits = true
		limited := f.acquire(t, spec)
		if !f.status.on(r) {
			t.Fatal("not raised while a limit Policy uses the store")
		}
		limited.Main.Release()
		if f.status.on(r) {
			t.Error("still raised after the limit Policy's snapshot was released")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		o := &opener{setup: func(d *driver) { d.status.EvictionPolicy = statestore.Unknown }}
		f := newFixture(t, o.openers())
		spec := redisSpec("redis://127.0.0.1:6379")
		spec.Limits = true
		f.acquire(t, spec)
		if f.status.on(r) {
			t.Error("raised for an unknown policy")
		}
	})
	t.Run("cache connection", func(t *testing.T) {
		o := &opener{setup: func(d *driver) {
			if d.cfg.Role == statestore.RoleCache {
				d.status.EvictionPolicy = statestore.No
			} else {
				d.status.EvictionPolicy = statestore.Yes
			}
		}}
		f := newFixture(t, o.openers())
		spec := redisSpec("redis://127.0.0.1:6379")
		spec.Cache, spec.Limits, spec.Caches = cacheConfig("redis://127.0.0.1:6380"), true, true
		f.acquire(t, spec)
		if f.status.on(r) {
			t.Error("raised for the cache connection, which holds no limit keys")
		}
	})
}

// TestCleartextHopsAndCredentials checks that the state_store hop count
// is the number of live drivers reporting cleartext, and that
// state_store_unauthenticated follows the drivers' Status.
func TestCleartextHopsAndCredentials(t *testing.T) {
	o := &opener{setup: func(d *driver) {
		d.status = statestore.Status{Cleartext: true, Unauthenticated: true, EvictionPolicy: statestore.Yes}
	}}
	f := newFixture(t, o.openers())
	spec := redisSpec("redis://127.0.0.1:6379")
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	hs := f.acquire(t, spec)
	if f.status.hop() != 2 || !f.status.on(catalog.ReasonStateStoreUnauthenticated) {
		t.Fatalf("hops %d, unauthenticated %v; want 2, true", f.status.hop(), f.status.on(catalog.ReasonStateStoreUnauthenticated))
	}
	hs.Main.Release()
	if f.status.hop() != 1 {
		t.Errorf("hops = %d after the main driver closed, want 1", f.status.hop())
	}
	hs.Cache.Release()
	if f.status.hop() != 0 || f.status.on(catalog.ReasonStateStoreUnauthenticated) {
		t.Errorf("hops %d, unauthenticated %v after every driver closed", f.status.hop(), f.status.on(catalog.ReasonStateStoreUnauthenticated))
	}
}

// TestSharedCacheWarning checks the startup warning when a Revision's
// Response Cache shares the State Store holding its limit keys: no cache
// connection, or one to the main URL, warns; a cache connection to
// another deployment, or a Revision without both Policy kinds, does not.
func TestSharedCacheWarning(t *testing.T) {
	withCache := func(url string) Spec {
		s := redisSpec("redis://127.0.0.1:6379")
		s.Cache = cacheConfig(url)
		return s
	}
	tests := []struct {
		name           string
		spec           Spec
		limits, caches bool
		warn           bool
	}{
		{"no cache connection", redisSpec("redis://127.0.0.1:6379"), true, true, true},
		{"cache connection to the main URL", withCache("redis://127.0.0.1:6379"), true, true, true},
		{"cache connection elsewhere", withCache("redis://127.0.0.1:6380"), true, true, false},
		{"no limit Policy", redisSpec("redis://127.0.0.1:6379"), false, true, false},
		{"no cache Policy", redisSpec("redis://127.0.0.1:6379"), true, false, false},
		{"memory and a memory cache store", Spec{
			Main:  memorySpec(statestore.SourceGateway).Main,
			Cache: &statestore.Config{Driver: statestore.DriverMemory},
		}, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &opener{}
			f := newFixture(t, o.openers())
			tt.spec.Limits, tt.spec.Caches = tt.limits, tt.caches
			f.acquire(t, tt.spec)
			f.acquire(t, tt.spec) // a Hot Reload warns no more
			want := 0
			if tt.warn {
				want = 1
			}
			if n := f.logs.count(warnShared); n != want {
				t.Errorf("shared-store warnings = %d, want %d", n, want)
			}
		})
	}
}

// TestCloseClearsStatus checks that Close clears the State Store reasons
// and the hop count it raised.
func TestCloseClearsStatus(t *testing.T) {
	o := &opener{setup: func(d *driver) { d.status = statestore.Status{Cleartext: true, BreakerNotClosed: true} }}
	f := newFixture(t, o.openers())
	f.acquire(t, memorySpec(statestore.SourceFallback))
	if !f.status.on(catalog.ReasonStateStoreMemoryFallback) || !f.status.on(catalog.ReasonStateStoreBreakerOpen) || f.status.hop() != 1 {
		t.Fatal("setup: reasons not raised")
	}
	if err := f.m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	for r := catalog.Reason(1); r < catalog.NumReasons; r++ {
		if f.status.on(r) {
			t.Errorf("%s still raised after Close", r)
		}
	}
	if f.status.hop() != 0 {
		t.Errorf("hops = %d after Close", f.status.hop())
	}
}

// TestMemoryCountersSurviveHotReload checks, with the memory driver,
// that a Hot Reload keeping the State Store configuration keeps the
// driver's counters: a GCRA deny persists across snapshots.
func TestMemoryCountersSurviveHotReload(t *testing.T) {
	f := newFixture(t, Openers{Memory: memory.Open})
	gcra := func(h *Handle) bool {
		t.Helper()
		rb := statestore.NewRequestBudget(time.Second, 0)
		c := &statestore.Call{Kind: statestore.OpGCRA, Timeout: 50 * time.Millisecond, GCRA: statestore.GCRA{
			Policy: "rl", Digest: statestore.DigestOf("client"),
			Limits: []statestore.GCRALimit{{Requests: 1, Window: time.Minute}}, Out: make([]statestore.GCRAOutcome, 1),
		}}
		h.Store().Consume(t.Context(), &rb, []*statestore.Call{c})
		if c.Err != nil || !c.Done {
			t.Fatalf("GCRA call: done %v, err %v", c.Done, c.Err)
		}
		return c.GCRA.Allowed
	}
	old := f.acquire(t, memorySpec(statestore.SourceFallback))
	if !gcra(old.Main) {
		t.Fatal("first request denied")
	}
	reload := memorySpec(statestore.SourceGateway)
	reload.Main.DefaultTimeout = 20 * time.Millisecond
	cur := f.acquire(t, reload)
	old.Main.Release()
	if gcra(cur.Main) {
		t.Error("the Hot Reload reset the memory driver's counters")
	}
}

// TestConcurrentAcquireRelease checks, under the race detector, that
// snapshots acquiring, writing through and releasing handles concurrently
// never see their driver closed while they hold it, that no write reaches
// a closed driver, and that Close closes every driver exactly once.
func TestConcurrentAcquireRelease(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	urls := []string{"redis://127.0.0.1:6379", "redis://127.0.0.2:6379", "redis://127.0.0.3:6379"}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 100 {
				spec := redisSpec(urls[(g+i)%len(urls)])
				hs, err := f.m.Acquire(context.Background(), spec)
				if err != nil {
					t.Errorf("Acquire = %v", err)
					return
				}
				hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund(strconv.Itoa(g)+"-"+strconv.Itoa(i)))
				if d, ok := hs.Main.Store().(*driver); !ok || d.closed.Load() {
					t.Error("a held handle's driver is closed")
				}
				hs.Main.Release()
			}
		})
	}
	wg.Wait()
	if err := f.m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	for _, d := range o.drivers() {
		if d.closes.Load() != 1 || d.lateWrites.Load() != 0 {
			t.Errorf("driver %s: closed %d times, %d writes after close", d.name, d.closes.Load(), d.lateWrites.Load())
		}
	}
}
