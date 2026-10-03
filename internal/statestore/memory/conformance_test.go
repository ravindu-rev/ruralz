// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// The driver conformance suite of spec 08 section 6.2 (Done when:
// "Conformance suite passes on memory"), run twice: on clocktest.Fake,
// where the literal numbers of section 6.2 are checked, and on the real
// clock, which exercises the tolerant path the redis driver's live runs
// take.

// start is the fake clock's start: mid-window for every window the suite
// uses, and after the 2026 GCRA epoch.
func start() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 123_456_789, time.UTC) }

// skewClock is a Node clock offset from its base (spec 08 req 42 skew).
type skewClock struct {
	base clock.Clock
	off  atomic.Int64
}

func (s *skewClock) Now() time.Time                  { return s.base.Now().Add(time.Duration(s.off.Load())) }
func (s *skewClock) Since(t time.Time) time.Duration { return s.Now().Sub(t) }
func (s *skewClock) NewTimer(d time.Duration) clock.Timer {
	return s.base.NewTimer(d)
}

func (s *skewClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return s.base.AfterFunc(d, f)
}

func (s *skewClock) Sleep(ctx context.Context, d time.Duration) error {
	return s.base.Sleep(ctx, d)
}

// harnessClocks are one driver's store and Node clocks.
type harnessClocks struct {
	fake *clocktest.Fake // nil on the real clock
	node *skewClock
}

func memoryHarness(exact bool) statestoretest.Harness {
	var mu sync.Mutex
	reg := map[statestore.Driver]*harnessClocks{}
	get := func(d statestore.Driver) *harnessClocks {
		mu.Lock()
		defer mu.Unlock()
		return reg[d]
	}
	return statestoretest.Harness{
		Exact:    exact,
		Parallel: true,
		Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
			hc := &harnessClocks{}
			base := clock.Real()
			if exact {
				hc.fake = clocktest.New(start())
				base = hc.fake
			}
			hc.node = &skewClock{base: base}
			d, err := New(context.Background(), statestore.Config{Driver: statestore.DriverMemory},
				statestore.Deps{Clock: hc.node, MACKey: o.MACKey, Metrics: newMetrics().handles()},
				Options{ServerClock: base})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { checkAccounting(t, d) })
			mu.Lock()
			reg[d] = hc
			mu.Unlock()
			return d
		},
		Advance: func(t *testing.T, d statestore.Driver, by time.Duration) {
			if hc := get(d); hc.fake != nil {
				hc.fake.Advance(by)
				return
			}
			if err := clock.Real().Sleep(context.Background(), by); err != nil {
				t.Fatal(err)
			}
		},
		Now:        func(d statestore.Driver) time.Time { return get(d).fake.Now() },
		DigestSlot: keys.DigestSlot,
		SkewNode: func(_ *testing.T, d statestore.Driver, off time.Duration) {
			get(d).node.off.Store(int64(off))
		},
		Inspect: func(t *testing.T, d statestore.Driver, k statestoretest.Key) (statestoretest.Stored, bool) {
			t.Helper()
			return inspect(d.(*Driver), k)
		},
		Tamper: func(t *testing.T, d statestore.Driver, k statestore.CacheKey, v statestore.Digest, edit func([]byte) []byte) {
			t.Helper()
			if !tamper(d.(*Driver), k, v, edit) {
				t.Fatal("no stored variant to tamper with")
			}
		},
	}
}

// inspect reads one key like GET and PTTL would.
func inspect(d *Driver, k statestoretest.Key) (statestoretest.Stored, bool) {
	now := instantOf(d.server.Now()).ms
	ttl := func(exp int64) time.Duration { return time.Duration(exp-now) * time.Millisecond }
	switch k.Kind {
	case statestoretest.KeyRateLimit:
		s := d.shardOf(int(keys.DigestSlot(k.Digest)))
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.rl[rlKey{k.Policy, k.Limit.Requests, k.Limit.Window, k.Digest}]
		if !ok || expired(e.exp, now) {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: strconv.FormatFloat(e.tat, 'g', 17, 64), TTL: ttl(e.exp)}, true
	case statestoretest.KeyQuota:
		s := d.shardOf(int(keys.DigestSlot(k.Digest)))
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.qt[qtKey{k.Name, k.Window, k.WindowStart.UnixMilli(), k.Digest}]
		if !ok || expired(e.exp, now) {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: strconv.FormatInt(e.n, 10), TTL: ttl(e.exp)}, true
	case statestoretest.KeyCacheGeneration:
		s := d.shardOf(int(keys.DigestSlot(k.Cache.URI)))
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.gen[k.Cache.URI]
		if !ok || expired(e.exp, now) {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: strconv.FormatInt(e.n, 10), TTL: ttl(e.exp)}, true
	case statestoretest.KeyCachePartition:
		s := d.shardOf(int(keys.PartitionSlot(k.Cache)))
		s.mu.Lock()
		defer s.mu.Unlock()
		p, ok := s.part[k.Cache]
		if !ok || expired(p.exp, now) {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: p.names, TTL: ttl(p.exp)}, true
	case statestoretest.KeyCacheLease:
		s := d.shardOf(int(keys.PartitionSlot(k.Cache)))
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.lease[k.Cache]
		if !ok || expired(e.exp, now) {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: strconv.FormatUint(e.token, 10), TTL: ttl(e.exp)}, true
	default:
		return statestoretest.Stored{}, false
	}
}

// tamper rewrites one variant's stored bytes, keeping byte accounting.
func tamper(d *Driver, k statestore.CacheKey, v statestore.Digest, edit func([]byte) []byte) bool {
	s := d.shardOf(int(keys.PartitionSlot(k)))
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.part[k]
	if p == nil {
		return false
	}
	i := p.find(v)
	if i < 0 {
		return false
	}
	old := p.variants[i].entry
	nw := edit(old)
	delta := int64(len(nw) - len(old))
	p.variants[i].entry = nw
	p.bytes += delta
	d.bytes.Add(delta)
	return true
}

// checkAccounting verifies that the cache byte total equals the bytes the
// partitions hold, that no dropped variant stays referenced past a
// partition's length, that no shard exceeds its key bound, and that each
// shard's next is a lower bound of its keys' expiries.
func checkAccounting(t *testing.T, d *Driver) {
	t.Helper()
	var sum int64
	for i := range d.shards {
		s := &d.shards[i]
		s.mu.Lock()
		for k, p := range s.part {
			var b int64
			for _, v := range p.variants {
				b += int64(len(v.entry))
			}
			if b != p.bytes {
				t.Errorf("partition %x holds %d bytes, accounted %d", k.URI[:4], b, p.bytes)
			}
			if len(p.variants) > maxVariants {
				t.Errorf("partition %x holds %d variants", k.URI[:4], len(p.variants))
			}
			for _, v := range p.variants[len(p.variants):cap(p.variants)] {
				if v.entry != nil {
					t.Errorf("partition %x keeps a dropped %d-byte entry past its length", k.URI[:4], len(v.entry))
				}
			}
			sum += b
		}
		if n := s.keys(); n > d.perShard {
			t.Errorf("shard %d holds %d keys, bound %d", i, n, d.perShard)
		}
		if m := minExpiry(s); m < s.next {
			t.Errorf("shard %d: a key expires at %d, before the shard's lower bound %d", i, m, s.next)
		}
		s.mu.Unlock()
	}
	if got := d.bytes.Load(); got != sum {
		t.Errorf("driver accounts %d cache bytes, partitions hold %d", got, sum)
	}
}

// minExpiry returns the earliest expiry of s's keys, math.MaxInt64 when
// it holds none; the caller holds s.mu.
func minExpiry(s *shard) int64 {
	m := int64(math.MaxInt64)
	for _, e := range s.rl {
		m = min(m, e.exp)
	}
	for _, e := range s.qt {
		m = min(m, e.exp)
	}
	for _, e := range s.gen {
		m = min(m, e.exp)
	}
	for _, e := range s.lease {
		m = min(m, e.exp)
	}
	for _, p := range s.part {
		m = min(m, p.exp)
	}
	return m
}

func TestConformanceFakeClock(t *testing.T) {
	statestoretest.RunConformance(t, memoryHarness(true))
}

func TestConformanceRealClock(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps about 10 s of real time")
	}
	statestoretest.RunConformance(t, memoryHarness(false))
}
