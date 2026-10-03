// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest_test

import (
	"context"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// refDriver is a second, deliberately plain implementation of the script
// semantics of spec 08 req 45 on Redis-like strings and hashes keyed by
// the real key layout, built independently of the memory driver. The
// conformance suite passing on both shows it encodes the spec rather than
// one implementation's choices; its raw keys give the suite's Inspect and
// Tamper cases a second backend.
type refDriver struct {
	mu     sync.Mutex
	clk    *clocktest.Fake
	mac    *keys.MAC
	model  *statestoretest.Model
	str    map[string]refValue
	hash   map[string]*refHash
	closed bool
}

type refValue struct {
	v   string
	exp int64 // Unix ms; gone when now > exp
}

type refHash struct {
	f   map[string][]byte
	ord []string // variant hex digests in store order
	exp int64
}

func newRefDriver(clk *clocktest.Fake, macKey []byte) (*refDriver, error) {
	mac, err := keys.NewMAC(macKey)
	if err != nil {
		return nil, err
	}
	return &refDriver{clk: clk, mac: mac, model: statestoretest.NewModel(), str: map[string]refValue{}, hash: map[string]*refHash{}}, nil
}

func (r *refDriver) nowMs() int64 { return r.clk.Now().UnixMilli() }

func (r *refDriver) get(k string) (string, bool) {
	v, ok := r.str[k]
	if !ok || r.nowMs() > v.exp {
		delete(r.str, k)
		return "", false
	}
	return v.v, true
}

func (r *refDriver) getHash(k string) *refHash {
	h, ok := r.hash[k]
	if !ok || r.nowMs() > h.exp {
		delete(r.hash, k)
		return nil
	}
	return h
}

func (r *refDriver) budget(ctx context.Context, rb *statestore.RequestBudget, t time.Duration) bool {
	if rb == nil {
		return t > 0 && ctx.Err() == nil
	}
	_, ok := rb.CallTimeout(ctx, r.clk.Now(), t)
	return ok
}

func (r *refDriver) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(calls) == 0 {
		return 0
	}
	slot, n := keys.SlotOf(calls[0]), 1
	for slot >= 0 && n < len(calls) && keys.SlotOf(calls[n]) == slot {
		n++
	}
	group := calls[:n]
	t := group[0].Timeout
	for _, c := range group {
		c.Done, c.Err, c.Trip, c.Batch = false, nil, statestore.TripSingle, -1
		if n > 1 {
			c.Trip, c.Batch = statestore.TripScriptMulti, 0
		}
		t = min(t, c.Timeout)
	}
	fail := func(e *statestore.Error) int {
		for _, c := range group {
			c.Done, c.Err = false, e
		}
		return n
	}
	switch {
	case slot < 0 || r.closed:
		return fail(statestore.ErrFailed)
	case !r.budget(ctx, rb, t):
		return fail(statestore.ErrNotAttempted)
	}
	now := r.clk.Now()
	for _, c := range group {
		var allowed bool
		switch c.Kind {
		case statestore.OpGCRA:
			g := &c.GCRA
			if !r.model.GCRA(now, g) {
				return fail(statestore.ErrFailed)
			}
			allowed = g.Allowed
			if allowed {
				for _, l := range g.Limits {
					tat, _ := r.model.TAT(g.Policy, l, g.Digest)
					_, tau, _ := statestoretest.GCRAParams(l)
					px := max(1, int64(math.Ceil((tat-statestoretest.GCRAMicros(g.ServerNow)+tau)/1000)))
					k := string(keys.AppendRateLimit(nil, g.Policy, l, g.Digest))
					r.str[k] = refValue{strconv.FormatFloat(tat, 'g', 17, 64), r.nowMs() + px}
				}
			}
		default:
			q := &c.Quota
			if ok, _ := r.model.Quota(now, now, q); !ok {
				return fail(statestore.ErrFailed)
			}
			allowed = q.Allowed
			if allowed {
				k := string(keys.AppendQuota(nil, q.Name, q.Window, q.WindowStart, q.Digest))
				exp := q.WindowStart.Add(2 * q.Window).UnixMilli()
				if v, ok := r.str[k]; ok && q.Used > 1 {
					exp = v.exp
				}
				r.str[k] = refValue{strconv.FormatInt(q.Used, 10), exp}
			}
		}
		c.Done = true
		if !allowed {
			break
		}
	}
	return n
}

func (r *refDriver) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := calls[0].Timeout
	for _, c := range calls {
		c.Done, c.Err, c.Trip, c.Batch = false, nil, statestore.TripPipeline, -1
		if len(calls) > 1 {
			c.Batch = 0
		}
		t = min(t, c.Timeout)
	}
	ok := r.budget(ctx, rb, t)
	for _, c := range calls {
		if !ok {
			c.Err = statestore.ErrNotAttempted
			continue
		}
		l := &c.Lookup
		gen, _ := r.get(string(keys.AppendCacheGeneration(nil, l.Key.URI)))
		l.Generation, _ = strconv.ParseInt(gen, 10, 64)
		l.Names, l.EntryGen, l.Found, l.Entry = "", 0, false, nil
		pk := keys.AppendCachePartition(nil, l.Key)
		if h := r.getHash(string(pk)); h != nil {
			l.Names = string(h.f[keys.FieldNames])
			l.EntryGen, _ = strconv.ParseInt(string(h.f[string(keys.AppendField(nil, keys.FieldGeneration, l.Variant))]), 10, 64)
			field := keys.AppendField(nil, keys.FieldEntry, l.Variant)
			if stored, ok := h.f[string(field)]; ok {
				l.Entry, l.Found = r.mac.Open(pk, field, stored)
			}
		}
		c.Done = true
	}
}

func (r *refDriver) Write(ctx context.Context, batch []*statestore.Write) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range batch {
		w.Err, w.Applied = nil, false
		if w.Timeout <= 0 || ctx.Err() != nil {
			w.Err = statestore.ErrNotAttempted
			continue
		}
		switch {
		case w.Kind == statestore.OpRefund:
			k := string(keys.AppendQuota(nil, w.Refund.Name, w.Refund.Window, w.Refund.WindowStart, w.Refund.Digest))
			if v, ok := r.get(k); ok && v != "0" {
				n, _ := strconv.ParseInt(v, 10, 64)
				r.str[k] = refValue{strconv.FormatInt(n-1, 10), r.str[k].exp}
				r.model.Refund(w.Refund)
				w.Applied = true
			}
		case w.Kind == statestore.OpCacheInvalidate:
			k := string(keys.AppendCacheGeneration(nil, w.Invalidate))
			old, _ := r.get(k)
			g, _ := strconv.ParseInt(old, 10, 64)
			r.str[k] = refValue{strconv.FormatInt(max(r.clk.Now().UnixMicro(), g+1), 10), r.nowMs() + 26*3600*1000}
			w.Applied = true
		case w.Lease.Mode != 0:
			w.Applied = r.lease(&w.Lease)
		default:
			w.Applied = r.store(&w.Cache)
		}
	}
}

func (r *refDriver) store(c *statestore.CacheStore) bool {
	pk := keys.AppendCachePartition(nil, c.Key)
	lk := string(keys.AppendCacheLease(nil, c.Key))
	if _, held := r.get(lk); held {
		return false
	}
	r.str[lk] = refValue{strconv.FormatUint(c.Token, 10), r.nowMs() + 1000}
	h := r.getHash(string(pk))
	if h != nil && string(h.f[keys.FieldNames]) != c.Names {
		h = nil
	}
	ttl := min(max((c.TTL+time.Millisecond-1).Milliseconds(), 1), 25*3600*1000)
	if h == nil {
		h = &refHash{f: map[string][]byte{keys.FieldNames: []byte(c.Names)}}
	} else {
		ttl = min(max(h.exp-r.nowMs(), ttl), 25*3600*1000)
	}
	r.hash[string(pk)] = h
	vh := string(keys.AppendHex(nil, c.Variant))
	h.ord = append(slices.DeleteFunc(h.ord, func(s string) bool { return s == vh }), vh)
	h.f["g:"+vh] = []byte(strconv.FormatInt(c.Generation, 10))
	h.f["t:"+vh] = []byte(strconv.FormatInt(r.nowMs(), 10))
	field := keys.AppendField(nil, keys.FieldEntry, c.Variant)
	h.f[string(field)] = r.mac.Seal(nil, pk, field, c.Entry)
	for len(h.ord) > 8 {
		for _, p := range []string{"g:", "t:", "e:"} {
			delete(h.f, p+h.ord[0])
		}
		h.ord = h.ord[1:]
	}
	h.exp = r.nowMs() + ttl
	return true
}

func (r *refDriver) lease(l *statestore.CacheLease) bool {
	pk := keys.AppendCachePartition(nil, l.Key)
	lk := string(keys.AppendCacheLease(nil, l.Key))
	tok, held := r.get(lk)
	if l.Mode == statestore.LeaseTake {
		if held {
			return false
		}
		r.str[lk] = refValue{strconv.FormatUint(l.Token, 10), r.nowMs() + 5000}
		return true
	}
	if !held || tok != strconv.FormatUint(l.Token, 10) {
		return false
	}
	h := r.getHash(string(pk))
	vh := string(keys.AppendHex(nil, l.Variant))
	if l.Mode == statestore.LeaseEndStale {
		if h != nil {
			h.ord = slices.DeleteFunc(h.ord, func(s string) bool { return s == vh })
			for _, p := range []string{"g:", "t:", "e:"} {
				delete(h.f, p+vh)
			}
		}
		return true
	}
	if h == nil || h.f["g:"+vh] == nil {
		return false
	}
	field := keys.AppendField(nil, keys.FieldEntry, l.Variant)
	h.f[string(field)] = r.mac.Seal(nil, pk, field, l.Entry)
	return true
}

func (r *refDriver) AdmitStoreBytes(statestore.CacheKey, int) bool { return true }
func (r *refDriver) Supports(c statestore.Capability) bool         { return c == statestore.CapScripts }
func (r *refDriver) RoundTrips() bool                              { return true }
func (r *refDriver) Status() statestore.Status {
	return statestore.Status{EvictionPolicy: statestore.Yes}
}

func (r *refDriver) Close(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// inspect answers the suite's Inspect from the raw keys.
func (r *refDriver) inspect(k statestoretest.Key) (statestoretest.Stored, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var key string
	switch k.Kind {
	case statestoretest.KeyRateLimit:
		key = string(keys.AppendRateLimit(nil, k.Policy, k.Limit, k.Digest))
	case statestoretest.KeyQuota:
		key = string(keys.AppendQuota(nil, k.Name, k.Window, k.WindowStart, k.Digest))
	case statestoretest.KeyCacheGeneration:
		key = string(keys.AppendCacheGeneration(nil, k.Cache.URI))
	case statestoretest.KeyCacheLease:
		key = string(keys.AppendCacheLease(nil, k.Cache))
	default:
		h := r.getHash(string(keys.AppendCachePartition(nil, k.Cache)))
		if h == nil {
			return statestoretest.Stored{}, false
		}
		return statestoretest.Stored{Value: string(h.f[keys.FieldNames]), TTL: time.Duration(h.exp-r.nowMs()) * time.Millisecond}, true
	}
	v, ok := r.get(key)
	return statestoretest.Stored{Value: v, TTL: time.Duration(r.str[key].exp-r.nowMs()) * time.Millisecond}, ok
}

func TestConformanceReferenceDriver(t *testing.T) {
	var mu sync.Mutex
	reg := map[statestore.Driver]*refDriver{}
	get := func(d statestore.Driver) *refDriver {
		mu.Lock()
		defer mu.Unlock()
		return reg[d]
	}
	statestoretest.RunConformance(t, statestoretest.Harness{
		Exact: true, Parallel: true, DigestSlot: keys.DigestSlot,
		Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
			r, err := newRefDriver(clocktest.New(start()), o.MACKey)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			reg[r] = r
			mu.Unlock()
			return r
		},
		Advance: func(_ *testing.T, d statestore.Driver, by time.Duration) { get(d).clk.Advance(by) },
		Now:     func(d statestore.Driver) time.Time { return get(d).clk.Now() },
		Inspect: func(_ *testing.T, d statestore.Driver, k statestoretest.Key) (statestoretest.Stored, bool) {
			return get(d).inspect(k)
		},
		Tamper: func(t *testing.T, d statestore.Driver, k statestore.CacheKey, v statestore.Digest, edit func([]byte) []byte) {
			r := get(d)
			r.mu.Lock()
			defer r.mu.Unlock()
			h := r.getHash(string(keys.AppendCachePartition(nil, k)))
			field := string(keys.AppendField(nil, keys.FieldEntry, v))
			if h == nil || h.f[field] == nil {
				t.Fatal("no entry to tamper with")
			}
			h.f[field] = edit(h.f[field])
		},
	})
}
