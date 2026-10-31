// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// zonedDriver is a fake driver whose calls carry the zone label
// (statestore.ZoneSetter): it records every zone map it is handed.
type zonedDriver struct {
	*driver
	deps statestore.Deps

	zmu  sync.Mutex
	maps []statestore.Zones
}

var _ statestore.ZoneSetter = (*zonedDriver)(nil)

func (d *zonedDriver) SetZones(z statestore.Zones) {
	d.zmu.Lock()
	defer d.zmu.Unlock()
	d.maps = append(d.maps, z)
}

// handed returns the zone maps SetZones received, in order.
func (d *zonedDriver) handed() []statestore.Zones {
	d.zmu.Lock()
	defer d.zmu.Unlock()
	return slices.Clone(d.maps)
}

// zonedOpener opens zonedDrivers, recording each with its Deps.
type zonedOpener struct {
	mu     sync.Mutex
	opened []*zonedDriver
}

func (o *zonedOpener) open(_ context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := &zonedDriver{driver: &driver{cfg: cfg, name: cfg.URL}, deps: deps}
	o.opened = append(o.opened, d)
	return d, nil
}

func (o *zonedOpener) drivers() []*zonedDriver {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.opened)
}

// zonedSpec returns a standalone redis Spec whose main connection has the
// zone map z.
func zonedSpec(z statestore.Zones) Spec {
	s := redisSpec("redis://127.0.0.1:6379")
	s.Main.Zones = z
	return s
}

// sameMaps reports whether got holds want, in order.
func sameMaps(got []statestore.Zones, want ...statestore.Zones) bool {
	return slices.EqualFunc(got, want, maps.Equal)
}

// TestSpecOfZones checks the zone maps SpecOf hands the drivers:
// stateStore.zones to a redis main connection and stateStore.cache.zones
// to the cache connection, each keyed by netaddr.CanonicalHost, so hosts
// compare case-insensitively and IP literals in canonical form; none for
// memory, which validation refuses beside zones, or for the fallback URL.
func TestSpecOfZones(t *testing.T) {
	redis, memoryDriver := v1alpha1.StateStoreDriverRedis, v1alpha1.StateStoreDriverMemory
	zones := []v1alpha1.StateStoreZone{
		{Host: "Valkey-0.STATE.internal", Zone: "eu-west-1a"},
		{Host: "2001:0DB8:0:0:0:0:0:1", Zone: "eu-west-1b"},
		{Host: "10.0.2.15", Zone: "eu-west-1b"},
	}
	want := statestore.Zones{"valkey-0.state.internal": "eu-west-1a", "2001:db8::1": "eu-west-1b", "10.0.2.15": "eu-west-1b"}
	cacheZones := []v1alpha1.StateStoreZone{{Host: "CACHE.state.internal", Zone: "eu-west-1a"}}
	cache := &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}, Zones: cacheZones}

	spec, err := SpecOf(&v1alpha1.StateStore{Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: mainRef}, Zones: zones, Cache: cache}, fixtureSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(spec.Main.Zones, want) {
		t.Errorf("main zones = %v, want %v", spec.Main.Zones, want)
	}
	if spec.Cache == nil || !maps.Equal(spec.Cache.Zones, statestore.Zones{"cache.state.internal": "eu-west-1a"}) {
		t.Errorf("cache zones = %+v, want the cache connection's own", spec.Cache)
	}

	spec, err = SpecOf(&v1alpha1.StateStore{Driver: &memoryDriver, Zones: zones, Cache: cache}, fixtureSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Main.Zones != nil || spec.Cache == nil || len(spec.Cache.Zones) != 1 {
		t.Errorf("memory main zones %v, cache %+v; want none and the cache connection's", spec.Main.Zones, spec.Cache)
	}
	if spec, err := SpecOf(nil, fixtureSecrets()); err != nil || spec.Main.Zones != nil {
		t.Errorf("fallback URL zones = %v, %v; want none", spec.Main.Zones, err)
	}
	if spec, err := SpecOf(&v1alpha1.StateStore{Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: mainRef}}, fixtureSecrets()); err != nil || spec.Main.Zones != nil {
		t.Errorf("no zone map: zones = %v, %v; want nil", spec.Main.Zones, err)
	}
}

// TestSpecOfZonesErrors checks the zone maps SpecOf refuses with ErrZones,
// which stage H refuses first (RZ-CFG-005): a host that is neither a
// hostname nor an IP literal, and one host named twice, in either map.
func TestSpecOfZonesErrors(t *testing.T) {
	redis := v1alpha1.StateStoreDriverRedis
	url := &v1alpha1.SecretValue{SecretRef: mainRef}
	twice := []v1alpha1.StateStoreZone{{Host: "valkey-0.state.internal", Zone: "a"}, {Host: "VALKEY-0.state.internal", Zone: "b"}}
	bad := []v1alpha1.StateStoreZone{{Host: "valkey-0.state.internal:6379", Zone: "a"}}
	tests := []struct {
		name, field string
		ss          *v1alpha1.StateStore
	}{
		{"main host with a port", "stateStore.zones", &v1alpha1.StateStore{Driver: &redis, URL: url, Zones: bad}},
		{"main host named twice", "stateStore.zones", &v1alpha1.StateStore{Driver: &redis, URL: url, Zones: twice}},
		{"cache host with a port", "stateStore.cache.zones", &v1alpha1.StateStore{Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}, Zones: bad}}},
		{"cache host named twice", "stateStore.cache.zones", &v1alpha1.StateStore{Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}, Zones: twice}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SpecOf(tt.ss, fixtureSecrets())
			if !errors.Is(err, ErrZones) || !strings.HasPrefix(err.Error(), tt.field+": ") {
				t.Fatalf("SpecOf = %v, want %v under %s", err, ErrZones, tt.field)
			}
			for _, u := range []string{mainURL, cacheURL} {
				if strings.Contains(err.Error(), u) {
					t.Errorf("error %q contains a URL value", err)
				}
			}
		})
	}
}

// TestZoneMapFollowsTheSwap checks a Hot Reload that changes only the zone
// map: the driver is kept, opened with its first snapshot's map; the
// newer snapshot's map reaches it at that snapshot's first call, after the
// swap, not at Acquire; calls of the older snapshot never hand it back; and
// a snapshot that is never published changes nothing. Every other method
// is the driver's.
func TestZoneMapFollowsTheSwap(t *testing.T) {
	o := &zonedOpener{}
	f := newFixture(t, Openers{Memory: o.open, Redis: o.open})
	z1 := statestore.Zones{"valkey-0.state.internal": "eu-west-1a"}
	z2 := statestore.Zones{"valkey-0.state.internal": "eu-west-1b"}
	z3 := statestore.Zones{"valkey-0.state.internal": "eu-west-1c"}
	ctx, rb := t.Context(), budget()

	s1 := f.acquire(t, zonedSpec(z1))
	ds := o.drivers()
	if len(ds) != 1 || !maps.Equal(ds[0].cfg.Zones, z1) {
		t.Fatalf("opened %d drivers, the first with zones %v; want one opened with %v", len(ds), ds[0].cfg.Zones, z1)
	}
	d := ds[0]
	if s1.Main.Store() == statestore.Store(d) || s1.Main.Store() != s1.Main.Store() {
		t.Fatal("Store is not one view per handle of a zone-labeling driver")
	}
	s1.Main.Store().Consume(ctx, rb, nil)
	if got := d.handed(); len(got) != 0 {
		t.Fatalf("the opening snapshot's call handed %v; the driver opened with its map", got)
	}

	s2 := f.acquire(t, zonedSpec(z2))
	if n := len(o.drivers()); n != 1 {
		t.Fatalf("a zone map change opened %d drivers, want the kept one", n)
	}
	if got := d.handed(); len(got) != 0 {
		t.Fatalf("Acquire handed %v before the swap", got)
	}
	s1.Main.Store().Read(ctx, rb, nil)
	if got := d.handed(); len(got) != 0 {
		t.Fatalf("the older snapshot's call handed %v", got)
	}
	s2.Main.Store().Read(ctx, rb, nil)
	s2.Main.Store().Consume(ctx, rb, nil)
	if got := d.handed(); !sameMaps(got, z2) {
		t.Fatalf("after the newer snapshot's calls the driver got %v, want z2 once", got)
	}
	s1.Main.Store().Write(ctx, nil)
	s1.Main.Store().Consume(ctx, rb, nil)
	if got := d.handed(); !sameMaps(got, z2) {
		t.Fatalf("an older snapshot's calls handed its map back: %v", got)
	}

	s3 := f.acquire(t, zonedSpec(z3))
	s3.Release()
	s2.Main.Store().Write(ctx, nil)
	if got := d.handed(); !sameMaps(got, z2) {
		t.Fatalf("a snapshot never published changed the map: %v", got)
	}

	v := s2.Main.Store()
	if !v.Supports(statestore.CapScripts) || !v.RoundTrips() || !v.AdmitStoreBytes(statestore.CacheKey{}, 1) {
		t.Error("the view does not forward Supports, RoundTrips and AdmitStoreBytes")
	}
	s1.Release()
	s2.Release()
}

// TestCacheZoneMapFollowsTheSwap checks the cache connection's own zone
// map: its driver opens with stateStore.cache.zones, and a change reaches
// it at the newer snapshot's first cache call.
func TestCacheZoneMapFollowsTheSwap(t *testing.T) {
	o := &zonedOpener{}
	f := newFixture(t, Openers{Memory: o.open, Redis: o.open})
	c1 := statestore.Zones{"cache.state.internal": "eu-west-1a"}
	c2 := statestore.Zones{"cache.state.internal": "eu-west-1b"}
	spec := func(z statestore.Zones) Spec {
		s := zonedSpec(nil)
		s.Cache = cacheConfig("redis://127.0.0.1:6380")
		s.Cache.Zones = z
		return s
	}
	first := f.acquire(t, spec(c1))
	ds := o.drivers()
	if len(ds) != 2 || ds[0].cfg.Zones != nil || !maps.Equal(ds[1].cfg.Zones, c1) {
		t.Fatalf("opened %d drivers; want main without zones and cache with %v", len(ds), c1)
	}
	next := f.acquire(t, spec(c2))
	next.Cache.Store().Read(t.Context(), budget(), nil)
	if got := ds[1].handed(); !sameMaps(got, c2) {
		t.Fatalf("the cache driver got %v, want c2", got)
	}
	if got := ds[0].handed(); len(got) != 0 {
		t.Fatalf("a cache call handed the main driver %v", got)
	}
	first.Release()
	next.Release()
}

// TestZoneHandoverConcurrent checks that concurrent first calls of two
// newer snapshots leave the driver with the newest snapshot's map, whatever
// their interleaving: each map is handed at most once, the older never
// after the newer.
func TestZoneHandoverConcurrent(t *testing.T) {
	o := &zonedOpener{}
	f := newFixture(t, Openers{Memory: o.open, Redis: o.open})
	z1 := statestore.Zones{"valkey-0.state.internal": "a"}
	z2 := statestore.Zones{"valkey-0.state.internal": "b"}
	z3 := statestore.Zones{"valkey-0.state.internal": "c"}
	s1 := f.acquire(t, zonedSpec(z1))
	s2 := f.acquire(t, zonedSpec(z2))
	s3 := f.acquire(t, zonedSpec(z3))
	var wg sync.WaitGroup
	for i := range 16 {
		h := s2.Main
		if i%2 == 1 {
			h = s3.Main
		}
		wg.Go(func() {
			for range 100 {
				h.Store().Consume(context.Background(), budget(), nil)
			}
		})
	}
	wg.Wait()
	got := o.drivers()[0].handed()
	if !sameMaps(got, z3) && !sameMaps(got, z2, z3) {
		t.Fatalf("the driver got %v, want z3 last and each map at most once", got)
	}
	s1.Release()
	s2.Release()
	s3.Release()
}

// TestNodeZoneReachesDrivers checks that the Node's RURALZ_ZONE, which the
// wiring passes in Config.Deps.Zone, reaches every driver the Manager
// opens, main and cache.
func TestNodeZoneReachesDrivers(t *testing.T) {
	o := &zonedOpener{}
	m := New(t.Context(), Config{
		Deps:    statestore.Deps{Zone: "eu-west-1a", Logger: slog.New(slog.DiscardHandler)},
		Openers: Openers{Memory: o.open, Redis: o.open},
	})
	t.Cleanup(func() { _ = m.Close(t.Context()) })
	spec := zonedSpec(nil)
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	hs, err := m.Acquire(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range o.drivers() {
		if d.deps.Zone != "eu-west-1a" {
			t.Errorf("driver %s opened with zone %q, want eu-west-1a", d.name, d.deps.Zone)
		}
	}
	hs.Release()
}

// TestMemoryStoreIsTheDriver checks that a driver whose calls carry no
// zone label gets no view: the memory driver's handle returns the driver.
func TestMemoryStoreIsTheDriver(t *testing.T) {
	f := newFixture(t, Openers{Memory: memory.Open})
	hs := f.acquire(t, memorySpec(statestore.SourceGateway))
	if _, ok := hs.Main.Store().(*memory.Driver); !ok {
		t.Fatalf("Store() is %T, want the memory driver itself", hs.Main.Store())
	}
	hs.Release()
}

// budget returns a fresh per-request budget.
func budget() *statestore.RequestBudget {
	rb := statestore.NewRequestBudget(0, 0)
	return &rb
}
