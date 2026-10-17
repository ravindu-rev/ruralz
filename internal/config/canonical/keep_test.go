// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestKeeper: a keeper holds the first n findings in content order
// whatever order they are offered in, fewer than 2n at any time, reports
// full once n were offered, and skips the resources after its bound (01
// req 50; the decode's determinism, 02 req 78).
func TestKeeper(t *testing.T) {
	const n, total = 5, 200
	all := make([]order, 0, total)
	for i := range total / 4 {
		// Four findings per resource of the resources member (ordinal 1),
		// and a few document members before and after it.
		for j := range 4 {
			all = append(all, order{member: 1, res: i, seq: j})
		}
	}
	all = append(all, order{member: 0}, order{member: 2}, order{member: 3})
	for seed := range uint64(20) {
		r := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // G404: deterministic test data
		k := newKeeper(n)
		k.setResources(1)
		if k.any() || k.full.Load() || k.after(1<<40) {
			t.Fatal("fresh keeper is not empty")
		}
		perm := r.Perm(len(all))
		for c, p := range perm {
			o := all[p]
			k.add(o, diag.Diagnostic{Message: strconv.Itoa(o.member) + "/" + strconv.Itoa(o.res) + "/" + strconv.Itoa(o.seq)}, nil)
			if len(k.entries) >= 2*n {
				t.Fatalf("seed %d: %d entries held", seed, len(k.entries))
			}
			if got, want := k.full.Load(), c+1 >= n; got != want {
				t.Fatalf("seed %d: full = %v after %d offers", seed, got, c+1)
			}
		}
		got := k.list(nil)
		want := []string{"0/0/0", "1/0/0", "1/0/1", "1/0/2", "1/0/3"}
		if len(got) != n {
			t.Fatalf("seed %d: %d kept", seed, len(got))
		}
		for i, d := range got {
			if d.Message != want[i] {
				t.Errorf("seed %d: kept %d = %s, want %s", seed, i, d.Message, want[i])
			}
		}
		// The bound is resource 0's last finding: resource 1 and later are
		// after it, resource 0 is not.
		if !k.any() || k.after(0) || !k.after(1) {
			t.Errorf("seed %d: after(0) = %v, after(1) = %v", seed, k.after(0), k.after(1))
		}
	}
	// A keeper offered fewer than n keeps them all and sets no bound.
	k := newKeeper(n)
	k.setResources(0)
	k.addResource(3, diag.List{{Message: "b"}, {Message: "c"}}, nil)
	k.add(order{member: -1}, diag.Diagnostic{Message: "a"}, nil)
	if got := k.list(nil); len(got) != 3 || got[0].Message != "a" || got[2].Message != "c" || k.full.Load() || k.after(1<<40) {
		t.Errorf("under the cap: kept %v, full %v", got, k.full.Load())
	}
	// A bound outside the resources array skips no resource.
	k = newKeeper(1)
	k.setResources(0)
	k.add(order{member: 1}, diag.Diagnostic{}, nil)
	if !k.full.Load() || k.after(1<<40) {
		t.Errorf("bound after the array: full %v, after %v", k.full.Load(), k.after(1<<40))
	}
	if newKeeper(0).n != 1 {
		t.Error("newKeeper(0) keeps no finding")
	}
}

// streamOf runs the parse and checks of a decode with one worker and
// returns its state, and the yield timer of its goroutine, whose site
// counts tell what the checks did, for the tests of what they skip.
func streamOf(t *testing.T, maxDiags int, content string) (*run, []*chunk, *yielder) {
	t.Helper()
	d := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1, MaxDiagnostics: maxDiags})
	r := newRun(d.maxDiags)
	now, after := due()
	y := newYielder(now, after)
	_, chunks, _, err := d.stream(context.Background(), newInput([]byte(content), 0, len(content), Source{}), r, y)
	if err != nil {
		t.Fatal(err)
	}
	return r, chunks, y
}

// TestCheckerSkips: the checks do no work whose result cannot matter
// (01 req 50): a resource whose findings all come after the newer
// findings kept is not checked, form findings are not offered once a
// newer finding is known (before the resource or in it), and the
// re-encoding is skipped once any finding is known. Each case runs with
// one worker, so the order of the checks is the content order.
func TestCheckerSkips(t *testing.T) {
	newerRoute := func(i int) string {
		return `{"kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{"y":1,"z":1}}`
	}
	t.Run("resources after the kept findings", func(t *testing.T) {
		// Cap 3: the keeper holds 4, which resources 0 and 1 fill; the
		// rest of the chunk is not checked.
		r, _, _ := streamOf(t, 3, resources(streamChunk, newerRoute))
		if r.checked.Load() != 2 {
			t.Fatalf("%d checked; want resources 0 and 1 checked only", r.checked.Load())
		}
		if !r.newer.full.Load() {
			t.Error("keeper not full")
		}
	})
	t.Run("form findings after a newer document member", func(t *testing.T) {
		r, _, _ := streamOf(t, 100, `{"aa":1,"format":"ruralz.canonical.v1","resources":[1,true]}`)
		if !r.newer.any() || r.form.any() {
			t.Errorf("newer %v, form %v; want no form finding offered", r.newer.any(), r.form.any())
		}
	})
	t.Run("form findings of a resource with a newer finding", func(t *testing.T) {
		r, _, _ := streamOf(t, 100, doc(`{"apiVersion":"x","kind":"Route","metadata":{"name":"r"},"spec":{},"status":{}}`))
		if !r.newer.any() || r.form.any() {
			t.Errorf("newer %v, form %v; want no form finding offered", r.newer.any(), r.form.any())
		}
	})
	// Route/s differs from its re-encoding. An entry is kept, and its
	// resource re-encoded, only while the decode can still succeed: with
	// no finding known and no resource before it rejected. The values the
	// re-encodings wrote (three per Route: metadata, its name and spec)
	// tell which resources were re-encoded, since a decode known to fail
	// keeps no entry (sweep).
	spaced := `{"kind":"Route","metadata":{"name":"s"},"spec": {}}`
	for _, tc := range []struct {
		name, first string
		reencoded   bool
		values      int
	}{
		{"re-encoding without a finding", `{"kind":"Route","metadata":{"name":"r"},"spec":{}}`, true, 6},
		{"re-encoding after a newer finding", `{"kind":"Route","metadata":{"name":"r"},"spec":{},"status":{}}`, false, 0},
		{"re-encoding after a form finding", `{"apiVersion":"x","kind":"Route","metadata":{"name":"r"},"spec":{}}`, false, 0},
		{"re-encoding after a rejected resource", `{"kind":"Route","metadata":{"name":"r"},"spec": {}}`, false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, chunks, y := streamOf(t, 100, doc(tc.first, spaced))
			if r.checked.Load() != 2 {
				t.Fatalf("%d resources checked, want 2", r.checked.Load())
			}
			var s *entry
			for _, c := range chunks {
				for i := range c.entries {
					if e := &c.entries[i]; e.id.Name == "s" {
						s = e
					}
				}
			}
			if got := s != nil && s.diff >= 0; got != tc.reencoded {
				t.Errorf("Route/s kept and re-encoded = %v, want %v", got, tc.reencoded)
			}
			if got := y.sites[siteValue]; got != tc.values {
				t.Errorf("the re-encodings wrote %d values, want %d", got, tc.values)
			}
		})
	}
}

// TestKeeperCost: the keeper sorts at most f/n+2 times for f offers, for
// every n up to the largest a decoder can pass and beyond it, where 2n
// overflows (WP-39 review: n = math.MaxInt/2+2 sorted on every offer,
// 5.6 s for 20,000 findings); counted sorts, not time.
func TestKeeperCost(t *testing.T) {
	const f = 20_000
	for _, n := range []int{1, 100, maxCount + 1, math.MaxInt/2 + 2, math.MaxInt - 1, math.MaxInt} {
		k := newKeeper(n)
		k.setResources(0)
		// Offered in reverse content order, so a sort is visible.
		for i := f - 1; i >= 0; i-- {
			k.add(order{member: 0, res: i}, diag.Diagnostic{}, nil)
		}
		offers := k.prunes
		got := k.list(nil)
		if limit := f/k.n + 2; k.prunes > limit {
			t.Errorf("n %d: %d sorts for %d offers, want at most %d", n, k.prunes, f, limit)
		}
		if n > f && offers != 0 {
			t.Errorf("n %d: %d sorts before the list, want none under the cap", n, offers)
		}
		if want := min(n, f); len(got) != want {
			t.Errorf("n %d: kept %d, want %d", n, len(got), want)
		}
	}
}
