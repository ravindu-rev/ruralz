// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for spec 06 requirements 12 and 13 on secretRef-held API keys:
// digests in a side index beside the snapshot, replaced copy-on-write on
// rotation and read without locks; the raw key discarded; keys normalized
// and rejected under 22 bytes (RZ-CFG-026 at activation; on rotation the
// last value is kept and a rotation failure counted); and R-55: watches are
// Node-wide, so a KeyIndex carried over into a later snapshot keeps
// following rotations after the resolver activates a newer Store.

var (
	refA = envRef("RURALZ_SECRET_KEY_A")
	refB = envRef("RURALZ_SECRET_KEY_B")
)

// buildWith compiles an index over cs with store, carrying prev over.
func buildWith(t *testing.T, store secret.Store, prev *KeyIndex, cs ...*v1alpha1.Consumer) *Index {
	t.Helper()
	ix, err := Build(cs, nil, store, prev)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ix
}

func TestReq12SecretKeyBinding(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: " " + testKeyA + "\n", refB: testKeyB})
	r.activate(s)
	ix := buildWith(t, s, nil,
		consumer("acme", withSecretKey("primary", refA), withHash("literal", testKeyC)),
		consumer("beta", withSecretKey("primary", refB)),
	)
	defer ix.Release()
	tests := []struct {
		key  string
		want string
	}{
		{testKeyA, "acme"}, // normalized: trimmed before hashing
		{" " + testKeyA + "\n", ""},
		{testKeyB, "beta"},
		{testKeyC, "acme"}, // literal digests stay in the snapshot index
	}
	for _, tt := range tests {
		if c, amb := ix.APIKey(DigestString(tt.key)); nameOf(c) != tt.want || amb {
			t.Errorf("APIKey(%q) = %q, %v; want %q", tt.key, nameOf(c), amb, tt.want)
		}
	}
	if got := len(ix.KeyIndex().Keys()); got != 2 {
		t.Fatalf("KeyIndex holds %d keys, want 2", got)
	}
	if r.watchCount() != 2 {
		t.Fatalf("watches = %d, want one per distinct Ref", r.watchCount())
	}
}

func TestReq14SecretAndLiteralDigestConflicts(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	tests := []struct {
		name      string
		cs        []*v1alpha1.Consumer
		want      string
		ambiguous bool
	}{
		{"literal in one Consumer, secretRef in another", []*v1alpha1.Consumer{
			consumer("a", withHash("lit", testKeyA)), consumer("b", withSecretKey("ref", refA)),
		}, "", true},
		{"literal and secretRef of one Consumer", []*v1alpha1.Consumer{
			consumer("a", withHash("lit", testKeyA), withSecretKey("ref", refA)),
		}, "a", false},
		{"one Ref in two Consumers", []*v1alpha1.Consumer{
			consumer("a", withSecretKey("ref", refA)), consumer("b", withSecretKey("ref", refA)),
		}, "", true},
		{"one Ref twice in one Consumer", []*v1alpha1.Consumer{
			consumer("a", withSecretKey("one", refA), withSecretKey("two", refA)),
		}, "a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix := buildWith(t, s, nil, tt.cs...)
			defer ix.Release()
			if c, amb := ix.APIKey(DigestString(testKeyA)); nameOf(c) != tt.want || amb != tt.ambiguous {
				t.Fatalf("APIKey = %q, %v; want %q, %v", nameOf(c), amb, tt.want, tt.ambiguous)
			}
		})
	}
}

func TestReq13ActivationRejectsUnusableKeys(t *testing.T) {
	const canary = "canary-9f3c1d-short"
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: canary, refB: testKeyB})
	tests := []struct {
		name  string
		store secret.Store
		c     *v1alpha1.Consumer
	}{
		{"key under 22 bytes", s, consumer("a", withSecretKey("k", refA))},
		{"reference not in the Store", s, consumer("a", withSecretKey("k", envRef("RURALZ_SECRET_MISSING")))},
		{"no secret table", nil, consumer("a", withSecretKey("k", refB))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build([]*v1alpha1.Consumer{tt.c}, nil, tt.store, nil)
			if code, _ := errcode.CodeOf(err); code != CodeUnresolved {
				t.Fatalf("Build error %v, want %s", err, CodeUnresolved)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("error carries the secret value: %v", err)
			}
		})
	}
	if r.watchCount() != 0 {
		t.Fatalf("a rejected build left %d watches", r.watchCount())
	}
}

func TestReq13RotationCopyOnWrite(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	ix := buildWith(t, s, nil, consumer("acme", withSecretKey("k", refA)))
	defer ix.Release()
	k := ix.KeyIndex()
	before := k.table.Load()

	// A short value is a rotation failure: the last digest keeps serving.
	r.rotate(refA, "too-short")
	if r.failureCount() != 1 {
		t.Fatalf("rotation failures = %d, want 1", r.failureCount())
	}
	if c, _ := ix.APIKey(DigestString(testKeyA)); nameOf(c) != "acme" {
		t.Fatal("a failed rotation dropped the last digest")
	}
	if k.table.Load() != before {
		t.Fatal("a failed rotation swapped the table")
	}

	// An unchanged value swaps nothing.
	r.rotate(refA, testKeyA+"\n")
	if k.table.Load() != before {
		t.Fatal("an unchanged digest swapped the table")
	}

	// A valid rotation publishes a new table; the old one is untouched.
	r.rotate(refA, testKeyB)
	if k.table.Load() == before {
		t.Fatal("rotation did not swap the table")
	}
	if _, ok := (*before)[DigestString(testKeyA)]; !ok {
		t.Fatal("rotation mutated the published table in place")
	}
	if c, _ := ix.APIKey(DigestString(testKeyB)); nameOf(c) != "acme" {
		t.Fatal("rotated key does not bind")
	}
	if c, _ := ix.APIKey(DigestString(testKeyA)); c != nil {
		t.Fatal("old key still binds after rotation")
	}
}

// Done when: "rotation swaps without locks on the read path and survives
// a carried-over KeyIndex" (R-55).
func TestR55RotationReachesCarriedOverKeyIndex(t *testing.T) {
	r := newFakeResolver()
	cs := []*v1alpha1.Consumer{consumer("acme", withSecretKey("k", refA))}

	s1 := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s1)
	ix1 := buildWith(t, s1, nil, cs...)

	// Next Revision: same bindings, a Consumer tier change, a new Store.
	cs2 := []*v1alpha1.Consumer{consumer("acme", withSecretKey("k", refA), func(c *v1alpha1.Consumer) { c.Spec.Tier = "gold" })}
	s2 := r.store(map[secret.Ref]string{refA: testKeyA})
	ix2 := buildWith(t, s2, ix1.KeyIndex(), cs2...)
	if ix2.KeyIndex() != ix1.KeyIndex() {
		t.Fatal("unchanged bindings did not carry the KeyIndex over")
	}
	if r.watchCount() != 1 {
		t.Fatalf("carry-over registered new watches: %d", r.watchCount())
	}
	r.activate(s2)

	r.rotate(refA, testKeyB)
	for i, ix := range []*Index{ix1, ix2} {
		if c, _ := ix.APIKey(DigestString(testKeyB)); nameOf(c) != "acme" {
			t.Fatalf("snapshot %d missed the rotation", i+1)
		}
	}
	// Bindings resolve through each snapshot's own Consumer views.
	if c, _ := ix2.APIKey(DigestString(testKeyB)); c.Tier != "gold" {
		t.Fatal("carried-over KeyIndex returned the old snapshot's Consumer")
	}

	// The old snapshot retires: its reference goes, the watch stays.
	ix1.Release()
	r.rotate(refA, testKeyC)
	if c, _ := ix2.APIKey(DigestString(testKeyC)); nameOf(c) != "acme" {
		t.Fatal("rotation lost after the old snapshot released the KeyIndex")
	}
	// The last reference stops the watch.
	ix2.Release()
	if r.watchCount() != 0 {
		t.Fatalf("watches after the last release = %d", r.watchCount())
	}
	if ix2.Retain() {
		t.Fatal("a closed KeyIndex accepted a reference")
	}
}

func TestKeyIndexCarryOverConditions(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA, refB: testKeyB})
	r.activate(s)
	base := buildWith(t, s, nil, consumer("acme", withSecretKey("k", refA)))
	defer base.Release()

	changed := buildWith(t, s, base.KeyIndex(), consumer("acme", withSecretKey("k", refB)))
	if changed.KeyIndex() == base.KeyIndex() {
		t.Fatal("a changed Ref carried the KeyIndex over")
	}
	changed.Release()
	renamed := buildWith(t, s, base.KeyIndex(), consumer("acme2", withSecretKey("k", refA)))
	if renamed.KeyIndex() == base.KeyIndex() {
		t.Fatal("a renamed Consumer carried the KeyIndex over")
	}
	renamed.Release()

	closed := buildWith(t, s, nil, consumer("x", withSecretKey("k", refA)))
	k := closed.KeyIndex()
	closed.Release()
	again := buildWith(t, s, k, consumer("x", withSecretKey("k", refA)))
	defer again.Release()
	if again.KeyIndex() == k {
		t.Fatal("a closed KeyIndex was carried over")
	}
	if c, _ := again.APIKey(DigestString(testKeyA)); nameOf(c) != "x" {
		t.Fatal("rebuilt KeyIndex does not bind")
	}
}

func TestKeyIndexReferenceCounting(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	k, err := NewKeyIndex([]SecretKey{{Consumer: "a", Name: "k", Ref: refA}}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !k.Retain() {
			t.Fatal("Retain on an open KeyIndex failed")
		}
	}
	k.Release()
	k.Release()
	if r.watchCount() != 1 {
		t.Fatal("watches stopped while a reference remains")
	}
	k.Release()
	if r.watchCount() != 0 {
		t.Fatal("last Release kept the watch")
	}
	k.Release() // releasing a closed KeyIndex does nothing
	r.rotate(refA, testKeyB)
	if _, ok := k.lookup(DigestString(testKeyB)); ok {
		t.Fatal("a closed KeyIndex applied a rotation")
	}
	if err := k.rotate(refA, secret.NewValue([]byte(testKeyC))); err != nil {
		t.Fatalf("rotate on a closed KeyIndex: %v", err)
	}
}

func TestKeyIndexWithoutKeys(t *testing.T) {
	k, err := NewKeyIndex(nil, nil, nil)
	if k != nil || err != nil {
		t.Fatalf("NewKeyIndex(nil) = %v, %v; want nil, nil", k, err)
	}
	ix := mustIndex(t, consumer("a", withHash("k", testKeyA))).WithSecretKeys(nil)
	if c, _ := ix.APIKey(DigestString(testKeyA)); nameOf(c) != "a" {
		t.Fatal("view without a KeyIndex lost literal digests")
	}
	var empty KeyIndex
	if _, ok := empty.lookup([32]byte{}); ok {
		t.Fatal("an unpublished KeyIndex matched")
	}
}

// A rotation between NewKeyIndex's first read and its watch registration
// is not lost.
func TestKeyIndexRotationDuringRegistration(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	r.beforeWatch = func(ref secret.Ref) {
		r.mu.Lock()
		r.values[ref] = []byte(testKeyB) // rotated, no watch registered yet
		r.mu.Unlock()
	}
	ix := buildWith(t, s, nil, consumer("acme", withSecretKey("k", refA)))
	defer ix.Release()
	if c, _ := ix.APIKey(DigestString(testKeyB)); nameOf(c) != "acme" {
		t.Fatal("rotation during registration was lost")
	}
}

// Readers never lock: run lookups concurrently with rotations under -race.
// Every lookup answers the one Consumer or nothing, and every table a
// reader loads is complete: it binds exactly one of the two digests (a
// table edited in place would expose neither or both).
func TestReq12ConcurrentReadsDuringRotation(t *testing.T) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	ix := buildWith(t, s, nil, consumer("acme", withSecretKey("k", refA)))
	defer ix.Release()
	keys := []string{testKeyA, testKeyB}
	digests := [][32]byte{DigestString(testKeyA), DigestString(testKeyB)}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Int64
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				for _, d := range digests {
					if c, amb := ix.APIKey(d); amb || (c != nil && c.Name != "acme") {
						bad.Add(1)
					}
				}
				tab := *ix.KeyIndex().table.Load()
				_, a := tab[digests[0]]
				_, b := tab[digests[1]]
				if a == b || len(tab) != 1 {
					bad.Add(1)
				}
			}
		})
	}
	for i := range 500 {
		r.rotate(refA, keys[(i+1)%2])
	}
	stop.Store(true)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d reads saw an inconsistent table", bad.Load())
	}
}

func TestBuildBundle(t *testing.T) {
	ix, err := BuildBundle(nil, nil, nil, nil)
	if err != nil || len(ix.Consumers()) != 0 {
		t.Fatalf("BuildBundle(nil) = %v, %v", ix, err)
	}
	if _, err := Build([]*v1alpha1.Consumer{consumer("a", withBasic("u", "bad", nil))}, nil, nil, nil); err == nil {
		t.Fatal("Build accepted a malformed basic hash")
	}
}

// BenchmarkKeyIndexLookup: the lock-free read path of a secretRef-held key.
func BenchmarkKeyIndexLookup(b *testing.B) {
	r := newFakeResolver()
	s := r.store(map[secret.Ref]string{refA: testKeyA})
	r.activate(s)
	ix, err := Build([]*v1alpha1.Consumer{consumer("acme", withSecretKey("k", refA))}, nil, s, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer ix.Release()
	d := DigestString(testKeyA)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ix.APIKey(d)
		}
	})
}
