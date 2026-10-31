// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The rotation of a file secret State Store URL
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"): a rotated value that differs from a
// connection's URL in use opens a new client for that connection at once,
// off the request path, as a changed URL does at a Hot Reload; calls move
// to the new client, and the old one closes once its queued writes are
// written or dropped.

// urlRef and cacheURLRef are the file secrets the rotation tests read the
// main and the cache URL from.
var (
	urlRef      = secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/state-store-url"}
	cacheURLRef = secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/cache-url"}
)

// watchedSecrets is a fake secret.Store whose values a test rotates, as
// the resolver's file poll does: rotate publishes a value, then delivers
// it to every watch of its reference on the caller's goroutine.
type watchedSecrets struct {
	mu      sync.Mutex
	values  map[secret.Ref]string
	watches map[secret.Ref]map[int]func(secret.Value) error
	next    int
	stopped int
}

func newWatchedSecrets(values map[secret.Ref]string) *watchedSecrets {
	return &watchedSecrets{values: maps.Clone(values), watches: map[secret.Ref]map[int]func(secret.Value) error{}}
}

func (s *watchedSecrets) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue([]byte(v)), true
}

func (s *watchedSecrets) Watch(r secret.Ref, fn func(secret.Value) error) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := s.next
	if s.watches[r] == nil {
		s.watches[r] = map[int]func(secret.Value) error{}
	}
	s.watches[r][id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.watches[r], id)
			s.stopped++
		})
	}
}

// set changes the value of r without delivering it: a rotation the
// resolver published but whose watches have not run yet.
func (s *watchedSecrets) set(r secret.Ref, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[r] = v
}

// rotate publishes v as the value of r and delivers it to every watch of
// r, returning their errors joined.
func (s *watchedSecrets) rotate(r secret.Ref, v string) error {
	s.mu.Lock()
	s.values[r] = v
	fns := slices.Collect(maps.Values(s.watches[r]))
	s.mu.Unlock()
	var errs []error
	for _, fn := range fns {
		errs = append(errs, fn(secret.NewValue([]byte(v))))
	}
	return errors.Join(errs...)
}

// watching returns the number of live watches of r.
func (s *watchedSecrets) watching(r secret.Ref) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watches[r])
}

// fileSpec returns a standalone redis Spec whose URL, url, was read from
// the file secret urlRef of secrets.
func fileSpec(url string, secrets secret.Store) Spec {
	s := redisSpec(url)
	ref := urlRef
	s.MainURL, s.Secrets = &ref, secrets
	return s
}

// serving returns the fake driver a handle's calls reach now: it makes one
// Consume call and reports the driver whose call count grew.
func serving(t *testing.T, o *opener, h *Handle) *driver {
	t.Helper()
	ds := o.drivers()
	before := make([]int64, len(ds))
	for i, d := range ds {
		before[i] = d.calls.Load()
	}
	rb := budget()
	h.Store().Consume(t.Context(), rb, nil)
	for i, d := range ds {
		if d.calls.Load() != before[i] {
			return d
		}
	}
	t.Fatal("the call reached no driver")
	return nil
}

// TestRotatedURLOpensANewDriverAtOnce checks the rotation itself: the
// watch opens a new driver with the rotated URL and the connection's other
// settings at once; the snapshot's handle, its Store and Enqueuer kept,
// moves its calls and writes to it; the old driver closes, as it holds
// nothing; and the Spec's URL references reach the watch.
func TestRotatedURLOpensANewDriverAtOnce(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	spec := fileSpec("redis://127.0.0.1:6379", secrets)
	spec.Main.TLS = &statestore.TLS{CACertificate: &secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/ca.crt"}, Secrets: secrets}
	spec.Main.Topology = statestore.TopologySentinel
	hs := f.acquire(t, spec)
	store, enq := hs.Main.Store(), hs.Main.Enqueuer()
	if secrets.watching(urlRef) != 1 {
		t.Fatalf("%d watches of the URL secret, want 1", secrets.watching(urlRef))
	}
	if err := secrets.rotate(urlRef, " redis://user:pw@127.0.0.2:6379\n"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	ds := o.drivers()
	if len(ds) != 2 {
		t.Fatalf("opened %d drivers after the rotation, want 2", len(ds))
	}
	old, cur := ds[0], ds[1]
	if cur.cfg.URL != "redis://user:pw@127.0.0.2:6379" || cur.cfg.Role != statestore.RoleMain || //nolint:gosec // G101: a test URL with a placeholder password.
		cur.cfg.Topology != statestore.TopologySentinel || cur.cfg.TLS != spec.Main.TLS {
		t.Errorf("the new driver opened with %+v; want the trimmed rotated URL and the connection's settings", cur.cfg)
	}
	if hs.Main.Store() != store || hs.Main.Enqueuer() != enq {
		t.Error("the handle's Store or Enqueuer changed; Filters keep the values they built with")
	}
	if got := serving(t, o, hs.Main); got != cur {
		t.Errorf("calls reach %s after the rotation, want the new driver", got.name)
	}
	if !enq.Enqueue(statestore.ClassSettle, refund("after")) {
		t.Fatal("Enqueue refused")
	}
	waitFor(t, "the write on the new driver", func() bool { return slices.Equal(cur.written(), []string{"after"}) })
	waitFor(t, "the old driver close", func() bool { return old.closes.Load() == 1 })
	if cur.closed.Load() {
		t.Error("the new driver closed")
	}
	if old.lateCalls.Load() != 0 || old.lateWrites.Load() != 0 {
		t.Error("a call or write reached the old driver after its close")
	}
}

// TestRotationSameValueKeepsTheDriver checks that a rotated value equal to
// the URL in use, surrounding whitespace aside, opens nothing.
func TestRotationSameValueKeepsTheDriver(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	if err := secrets.rotate(urlRef, "redis://127.0.0.1:6379\r\n"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	if n := len(o.drivers()); n != 1 {
		t.Errorf("opened %d drivers for an unchanged URL, want 1", n)
	}
}

// TestRotationWaitsForQueuedWrites checks that writes queued for the old
// driver before the rotation are written to it, which closes only after
// them, while later writes go to the new driver.
func TestRotationWaitsForQueuedWrites(t *testing.T) {
	o := &opener{setup: func(d *driver) {
		if d.cfg.URL == "redis://127.0.0.1:6379" {
			d.blockWrite, d.writeEntered = make(chan struct{}), make(chan struct{}, 4)
		}
	}}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	old := o.drivers()[0]
	if !hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("before")) {
		t.Fatal("Enqueue refused")
	}
	<-old.writeEntered
	if err := secrets.rotate(urlRef, "redis://127.0.0.2:6379"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	cur := o.drivers()[1]
	if !hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund("after")) {
		t.Fatal("Enqueue refused")
	}
	waitFor(t, "the write on the new driver", func() bool { return slices.Equal(cur.written(), []string{"after"}) })
	if old.closed.Load() {
		t.Fatal("the old driver closed while a write queued for it was being written")
	}
	close(old.blockWrite)
	waitFor(t, "the old driver close", func() bool { return old.closes.Load() == 1 })
	if got := old.written(); !slices.Equal(got, []string{"before"}) {
		t.Errorf("the old driver wrote %v, want [before]", got)
	}
	if old.lateWrites.Load() != 0 {
		t.Error("a write reached the old driver after its close")
	}
}

// TestRotationWaitsForCallsInProgress checks that a call in progress on
// the old driver finishes there, the old driver closing only after it,
// while calls starting after the rotation reach the new driver.
func TestRotationWaitsForCallsInProgress(t *testing.T) {
	o := &opener{setup: func(d *driver) {
		if d.cfg.URL == "redis://127.0.0.1:6379" {
			d.blockConsume, d.consumeEntered = make(chan struct{}), make(chan struct{}, 1)
		}
	}}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	old := o.drivers()[0]
	done := make(chan struct{})
	go func() {
		defer close(done)
		hs.Main.Store().Consume(context.Background(), budget(), nil)
	}()
	<-old.consumeEntered
	if err := secrets.rotate(urlRef, "redis://127.0.0.2:6379"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	cur := o.drivers()[1]
	hs.Main.Store().Read(t.Context(), budget(), nil)
	if cur.calls.Load() != 1 {
		t.Fatalf("the new driver saw %d calls, want the one started after the rotation", cur.calls.Load())
	}
	if old.closed.Load() {
		t.Fatal("the old driver closed under a call in progress")
	}
	close(old.blockConsume)
	<-done
	waitFor(t, "the old driver close", func() bool { return old.closes.Load() == 1 })
	if old.lateCalls.Load() != 0 {
		t.Error("the call in progress ended on a closed driver")
	}
}

// TestRotationRefusedKeepsTheDriver checks that a rotated value the opener
// refuses keeps the old driver serving, and that the watch returns the
// opener's error, which the resolver counts as a rotation failure.
func TestRotationRefusedKeepsTheDriver(t *testing.T) {
	refused := errors.New("RZ-CFG-042: the State Store url is refused")
	o := &opener{refuse: func(cfg statestore.Config) error {
		if cfg.URL == "rediss://10.0.0.1:6379" {
			return refused
		}
		return nil
	}}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	err := secrets.rotate(urlRef, "rediss://10.0.0.1:6379")
	var oe *OpenError
	if !errors.Is(err, refused) || !errors.As(err, &oe) || oe.Role != statestore.RoleMain {
		t.Fatalf("the rotation watch = %v, want the opener's refusal as an OpenError", err)
	}
	ds := o.drivers()
	if len(ds) != 1 || ds[0].closed.Load() {
		t.Fatalf("%d drivers, the first closed %v: a refused rotation keeps the driver", len(ds), ds[0].closed.Load())
	}
	if got := serving(t, o, hs.Main); got != ds[0] {
		t.Errorf("calls reach %s, want the kept driver", got.name)
	}
	if err := secrets.rotate(urlRef, "redis://127.0.0.2:6379"); err != nil {
		t.Fatalf("a later accepted rotation = %v", err)
	}
	if n := len(o.drivers()); n != 2 {
		t.Errorf("opened %d drivers after the accepted rotation, want 2", n)
	}
	if err := secrets.rotate(urlRef, " \n"); !errors.Is(err, ErrEmptyURL) {
		t.Errorf("an empty rotated value = %v, want ErrEmptyURL", err)
	}
}

// TestRotationMovesTheConnectionKey checks that the rotated connection is
// the one a later Revision resolving the rotated value shares, while the
// old URL opens a fresh driver.
func TestRotationMovesTheConnectionKey(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	first := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	if err := secrets.rotate(urlRef, "redis://127.0.0.2:6379"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	reload := f.acquire(t, fileSpec("redis://127.0.0.2:6379", secrets))
	if n := len(o.drivers()); n != 2 || reload.Main.e != first.Main.e {
		t.Fatalf("opened %d drivers; a Hot Reload on the rotated value must share the rotated connection", n)
	}
	if secrets.watching(urlRef) != 1 {
		t.Errorf("%d watches of the URL secret, want the shared connection's one", secrets.watching(urlRef))
	}
	other := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	back := f.acquire(t, fileSpec("redis://127.0.0.1:6379", other))
	if ds := o.drivers(); len(ds) != 3 || back.Main.e == first.Main.e || ds[2].cfg.URL != "redis://127.0.0.1:6379" {
		t.Errorf("the old URL got %d drivers; it must open a fresh one", len(ds))
	}
}

// TestRotationBeforeTheWatch checks a rotation published after SpecOf read
// the URL and before the Manager registered its watch: Acquire opens the
// connection on the rotated value, so the rotation is not lost.
func TestRotationBeforeTheWatch(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	spec := fileSpec("redis://127.0.0.1:6379", secrets)
	secrets.set(urlRef, "redis://127.0.0.2:6379\n")
	hs := f.acquire(t, spec)
	ds := o.drivers()
	if len(ds) != 1 || ds[0].cfg.URL != "redis://127.0.0.2:6379" {
		t.Fatalf("opened %d drivers, the first on its URL %q; want one on the rotated value", len(ds), ds[0].cfg.URL)
	}
	again := f.acquire(t, fileSpec("redis://127.0.0.2:6379", secrets))
	if again.Main.e != hs.Main.e || len(o.drivers()) != 1 {
		t.Error("the connection opened on the rotated value is not shared under that value")
	}
}

// TestRotationAfterRetireOpensNothing checks that the watch stops when
// the connection's last snapshot is released, and that a delivery racing
// that release opens nothing.
func TestRotationAfterRetireOpensNothing(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	e := hs.Main.e
	hs.Release()
	if secrets.watching(urlRef) != 0 {
		t.Fatalf("%d watches left after the connection retired", secrets.watching(urlRef))
	}
	if err := f.m.rotate(e, secret.NewValue([]byte("redis://127.0.0.2:6379"))); err != nil {
		t.Fatalf("a late rotation = %v", err)
	}
	if n := len(o.drivers()); n != 1 {
		t.Errorf("opened %d drivers after the connection retired, want 1", n)
	}
}

// TestRotationCarriesTheZoneMap checks that the new driver of a
// zone-labeling connection opens with the zone map the connection holds:
// the newest snapshot's whose call handed it over.
func TestRotationCarriesTheZoneMap(t *testing.T) {
	o := &zonedOpener{}
	f := newFixture(t, Openers{Memory: o.open, Redis: o.open})
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	z1 := statestore.Zones{"127.0.0.1": "eu-west-1a"}
	z2 := statestore.Zones{"127.0.0.1": "eu-west-1b"}
	s1 := fileSpec("redis://127.0.0.1:6379", secrets)
	s1.Main.Zones = z1
	s2 := s1
	s2.Main.Zones = z2
	f.acquire(t, s1)
	h2 := f.acquire(t, s2)
	h2.Main.Store().Consume(t.Context(), budget(), nil)
	if err := secrets.rotate(urlRef, "redis://127.0.0.2:6379"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	ds := o.drivers()
	if len(ds) != 2 || !maps.Equal(ds[1].cfg.Zones, z2) {
		t.Fatalf("the rotated driver opened with zones %v, want the held map %v", ds[1].cfg.Zones, z2)
	}
	h2.Main.Store().Consume(t.Context(), budget(), nil)
	if got := ds[1].handed(); len(got) != 0 {
		t.Errorf("the rotated driver was handed %v again", got)
	}
}

// TestCacheConnectionRotatesAlone checks that a rotated cache URL replaces
// only the cache connection's driver.
func TestCacheConnectionRotatesAlone(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379", cacheURLRef: "redis://127.0.0.1:6380"})
	spec := fileSpec("redis://127.0.0.1:6379", secrets)
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	ref := cacheURLRef
	spec.CacheURL = &ref
	hs := f.acquire(t, spec)
	if err := secrets.rotate(cacheURLRef, "redis://127.0.0.1:6381"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	ds := o.drivers()
	if len(ds) != 3 || ds[2].cfg.Role != statestore.RoleCache || ds[2].cfg.URL != "redis://127.0.0.1:6381" {
		t.Fatalf("opened %d drivers; want the cache connection's new one", len(ds))
	}
	if got := serving(t, o, hs.Cache); got != ds[2] {
		t.Errorf("cache calls reach %s, want the rotated cache driver", got.name)
	}
	if got := serving(t, o, hs.Main); got != ds[0] {
		t.Errorf("main calls reach %s, want the unchanged main driver", got.name)
	}
	waitFor(t, "the old cache driver close", func() bool { return ds[1].closes.Load() == 1 })
}

// TestRotationRefreshesTheStatus checks that the State Store status
// follows the rotated driver at once: the cleartext hop and the
// unauthenticated reason of the old one clear.
func TestRotationRefreshesTheStatus(t *testing.T) {
	o := &opener{setup: func(d *driver) {
		if d.cfg.URL == "redis://127.0.0.1:6379" {
			d.status = statestore.Status{Cleartext: true, Unauthenticated: true, EvictionPolicy: statestore.Yes}
		}
	}}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	if f.status.hop() != 1 || !f.status.on(catalog.ReasonStateStoreUnauthenticated) {
		t.Fatal("setup: the cleartext hop and the unauthenticated reason are not raised")
	}
	if err := secrets.rotate(urlRef, "rediss://user:pw@127.0.0.1:6379"); err != nil {
		t.Fatalf("the rotation watch = %v", err)
	}
	if f.status.hop() != 0 || f.status.on(catalog.ReasonStateStoreUnauthenticated) {
		t.Errorf("hops %d, unauthenticated %v after rotating to an authenticated TLS URL", f.status.hop(), f.status.on(catalog.ReasonStateStoreUnauthenticated))
	}
}

// TestCloseStopsURLWatches checks that Close stops every URL watch and
// closes the rotated-out driver still writing, and that a rotation after
// Close opens nothing.
func TestCloseStopsURLWatches(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	if err := f.m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if secrets.watching(urlRef) != 0 {
		t.Errorf("%d watches left after Close", secrets.watching(urlRef))
	}
	if err := f.m.rotate(hs.Main.e, secret.NewValue([]byte("redis://127.0.0.2:6379"))); err != nil {
		t.Fatalf("a rotation after Close = %v", err)
	}
	if n := len(o.drivers()); n != 1 || o.drivers()[0].closes.Load() != 1 {
		t.Errorf("%d drivers after Close, want the one, closed once", n)
	}
}

// TestSpecOfURLReferences checks that SpecOf hands the Manager each redis
// connection's url reference and the secret table: stateStore.url and
// stateStore.cache.url, and RURALZ_STATE_STORE_URL for the fallback URL;
// none for the memory driver.
func TestSpecOfURLReferences(t *testing.T) {
	file := func(name string) *v1alpha1.SecretValue {
		return &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: name}}
	}
	redis := v1alpha1.StateStoreDriverRedis
	main, cache := secret.RefOf(file("/etc/ruralz/url").SecretRef), secret.RefOf(file("/etc/ruralz/cache-url").SecretRef)
	store := secrets{main: "redis://127.0.0.1:6379", cache: "redis://127.0.0.1:6380", EnvRef(): "redis://127.0.0.1:6381"}
	tests := []struct {
		name            string
		ss              *v1alpha1.StateStore
		wantMain, cache *secret.Ref
	}{
		{"redis with a cache connection", &v1alpha1.StateStore{
			Driver: &redis, URL: file("/etc/ruralz/url"),
			Cache: &v1alpha1.StateStoreCache{URL: *file("/etc/ruralz/cache-url")},
		}, &main, &cache},
		{"fallback URL", nil, new(EnvRef()), nil},
		{"memory", &v1alpha1.StateStore{}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := SpecOf(tt.ss, store)
			if err != nil {
				t.Fatalf("SpecOf = %v", err)
			}
			if !sameRef(spec.MainURL, tt.wantMain) || !sameRef(spec.CacheURL, tt.cache) {
				t.Errorf("SpecOf references main %v, cache %v; want %v, %v", spec.MainURL, spec.CacheURL, tt.wantMain, tt.cache)
			}
			if (tt.wantMain != nil) != (spec.Secrets != nil) {
				t.Errorf("SpecOf Secrets set %v, want %v", spec.Secrets != nil, tt.wantMain != nil)
			}
		})
	}
}

// sameRef compares two optional references.
func sameRef(a, b *secret.Ref) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestConcurrentRotationsAndCalls checks, under the race detector, that
// rotations racing calls and writes never let one reach a closed driver,
// and that every driver closes exactly once.
func TestConcurrentRotationsAndCalls(t *testing.T) {
	o := &opener{}
	f := newFixture(t, o.openers())
	secrets := newWatchedSecrets(map[secret.Ref]string{urlRef: "redis://127.0.0.1:6379"})
	hs := f.acquire(t, fileSpec("redis://127.0.0.1:6379", secrets))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				hs.Main.Store().Consume(context.Background(), budget(), nil)
				hs.Main.Store().Read(context.Background(), budget(), nil)
				hs.Main.Enqueuer().Enqueue(statestore.ClassSettle, refund(strconv.Itoa(g)+"-"+strconv.Itoa(i)))
			}
		})
	}
	for i := range 50 {
		if err := secrets.rotate(urlRef, "redis://127.0.0."+strconv.Itoa(2+i)+":6379"); err != nil {
			t.Fatalf("rotation %d = %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	hs.Release()
	if err := f.m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	ds := o.drivers()
	if len(ds) != 51 {
		t.Fatalf("opened %d drivers, want 51", len(ds))
	}
	for _, d := range ds {
		if d.closes.Load() != 1 || d.lateCalls.Load() != 0 || d.lateWrites.Load() != 0 {
			t.Errorf("driver %s: closed %d times, %d calls and %d writes after its close", d.name, d.closes.Load(), d.lateCalls.Load(), d.lateWrites.Load())
		}
	}
}
