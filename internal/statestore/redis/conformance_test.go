// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// The driver conformance suite against the fake server, whose EVALSHA
// mirrors ruralz_v1.lua: on clocktest.Fake, where the literal numbers are
// checked, and on the real clock, the tolerant path live servers take.
// The integration suite runs it against real servers.

// harnessState is one driver's server and clocks.
type harnessState struct {
	srv  *fakeServer
	fake *clocktest.Fake // nil on the real clock
	node *skewClock
}

func fakeHarness(exact bool) statestoretest.Harness {
	var mu sync.Mutex
	reg := map[statestore.Driver]*harnessState{}
	get := func(d statestore.Driver) *harnessState {
		mu.Lock()
		defer mu.Unlock()
		return reg[d]
	}
	return statestoretest.Harness{
		Exact:    exact,
		Parallel: true,
		Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
			hs := &harnessState{}
			base := clock.Real()
			if exact {
				hs.fake = clocktest.New(start())
				base = hs.fake
			}
			hs.node = &skewClock{base: base}
			hs.srv = newFakeServer(t, base, fakeOptions{})
			d, err := New(context.Background(), statestore.Config{Driver: statestore.DriverRedis, URL: hs.srv.url()},
				statestore.Deps{Clock: hs.node, MACKey: o.MACKey, Logger: slog.New(slog.DiscardHandler)},
				Options{Rand: lowRand})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			reg[d] = hs
			mu.Unlock()
			return d
		},
		Advance: func(t *testing.T, d statestore.Driver, by time.Duration) {
			if hs := get(d); hs.fake != nil {
				hs.fake.Advance(by)
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
			key, field := inspectKey(k)
			v, ttl, ok := get(d).srv.inspect(key, field)
			return statestoretest.Stored{Value: v, TTL: ttl}, ok
		},
		Tamper: func(t *testing.T, d statestore.Driver, k statestore.CacheKey, v statestore.Digest, edit func([]byte) []byte) {
			t.Helper()
			part := string(keys.AppendCachePartition(nil, k))
			if !get(d).srv.tamper(part, string(keys.AppendField(nil, keys.FieldEntry, v)), edit) {
				t.Fatal("no stored variant to tamper with")
			}
		},
	}
}

// inspectKey returns the key a statestoretest.Key names and, for a
// partition, the hash field holding its names.
func inspectKey(k statestoretest.Key) (key, field string) {
	switch k.Kind {
	case statestoretest.KeyRateLimit:
		return string(keys.AppendRateLimit(nil, k.Policy, k.Limit, k.Digest)), ""
	case statestoretest.KeyQuota:
		return string(keys.AppendQuotaCounter(nil, k.Name, k.Window, k.Month, k.WindowStart, k.Digest)), ""
	case statestoretest.KeyCacheGeneration:
		return string(keys.AppendCacheGeneration(nil, k.Cache.URI)), ""
	case statestoretest.KeyCachePartition:
		return string(keys.AppendCachePartition(nil, k.Cache)), keys.FieldNames
	default:
		return string(keys.AppendCacheLease(nil, k.Cache)), ""
	}
}

func TestConformanceFakeClock(t *testing.T) {
	statestoretest.RunConformance(t, fakeHarness(true))
}

func TestConformanceRealClock(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps about 10 s of real time")
	}
	statestoretest.RunConformance(t, fakeHarness(false))
}
