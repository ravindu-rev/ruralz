// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for a refused rotation taken by a Hot Reload and for rotations
// polled between Resolve and Activate: spec 01 requirement 46 (a failed
// check keeps the last value and counts a rotation failure; the last value
// is kept only while the new value fails), spec 06 requirements 13 and 76
// (secret_rotation_failed), architecture 2.8 and R-55, and the
// secret.Store contract (a Store kept by a carried-over Filter or a
// retired snapshot keeps seeing current values, rotations included).

// refusedRotation returns a harness whose active Store has a KindAPIKey
// use of the file "k", which holds a 30-byte key and has just been
// rotated to "short", refused by that use; the poll counted one failure.
func refusedRotation(t *testing.T) (*harness, secret.Ref, secret.Store, string) {
	t.Helper()
	h := newHarness(t, nil)
	key := strings.Repeat("k", 30)
	h.write("k", key)
	ref := fileRef(h.path("k"), "")
	s1 := h.resolve(use(ref, secret.KindAPIKey))
	h.r.Activate(s1)
	h.cycle()
	h.write("k", "short")
	h.cycle()
	if got := get(t, s1, ref); got != key || h.fileN.value() != 1 || len(h.status.raised()) != 1 {
		t.Fatalf("setup: value %q, failures %d, raised %v", got, h.fileN.value(), h.status.raised())
	}
	return h, ref, s1, key
}

// cellOf returns the cell st holds for ref.
func cellOf(st secret.Store, ref secret.Ref) *cell { return st.(*store).cells[ref] }

func TestRefusedRotationKeepsCellShared(t *testing.T) {
	t.Run("retained Store follows later rotations", func(t *testing.T) {
		// Req 46 and the Store contract: the Store a carried-over owner
		// keeps sees the rotation the reload applied, and every later one.
		h, ref, s1, _ := refusedRotation(t)
		w1 := &recorder{}
		defer s1.Watch(ref, w1.fn)()
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if cellOf(s2, ref) != cellOf(s1, ref) {
			t.Fatal("Resolve gave the new Store a new cell")
		}
		h.r.Activate(s2)
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "short" || b != "short" {
			t.Errorf("after Activate: retained %q, active %q", a, b)
		}
		z := strings.Repeat("z", 30)
		h.write("k", z)
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != z || b != z {
			t.Errorf("after a later rotation: retained %q, active %q", a, b)
		}
		if got := w1.values(); !slices.Equal(got, []string{"short", z}) {
			t.Errorf("watcher on the retained Store got %v", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("transient read failure with unchanged uses", func(t *testing.T) {
		// A Hot Reload landing while the last poll failed (file briefly
		// missing) keeps the shared cell.
		h := newHarness(t, nil)
		h.write("s", "v1")
		ref := fileRef(h.path("s"), "")
		s1 := h.resolve(use(ref, secret.KindOpaque))
		h.r.Activate(s1)
		if err := os.Remove(filepath.Join(h.root, "s")); err != nil {
			t.Fatal(err)
		}
		h.cycle()
		h.write("s", "v2")
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if cellOf(s2, ref) != cellOf(s1, ref) {
			t.Fatal("Resolve gave the new Store a new cell")
		}
		h.r.Activate(s2)
		h.cycle()
		h.write("s", "v3")
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "v3" || b != "v3" {
			t.Errorf("retained %q, active %q", a, b)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("carried-over key pair", func(t *testing.T) {
		// A listener carried over through a reload keeps its first Store
		// and pairs each rotated half with the other half's current value
		// (as tlsconf.CertIndex does). A non-atomic renewal that the
		// reload lands in, and later renewals, pair without failure.
		h := newHarness(t, nil)
		p1, p2, p3 := newPEM(t), newPEM(t), newPEM(t)
		h.write("tls.crt", p1.cert)
		h.write("tls.key", p1.key)
		crt, key := fileRef(h.path("tls.crt"), ""), fileRef(h.path("tls.key"), "")
		uses := []secret.Use{use(crt, secret.KindPEMCertificate), use(key, secret.KindPEMPrivateKey)}
		s1 := h.resolve(uses...)
		h.r.Activate(s1)
		h.cycle()
		pair := func(other secret.Ref, isCert bool) func(secret.Value) error {
			return func(v secret.Value) error {
				o, _ := s1.Get(other)
				c, k := v.Reveal(), o.Reveal()
				if !isCert {
					c, k = k, c
				}
				_, err := tls.X509KeyPair(c, k)
				return err
			}
		}
		defer s1.Watch(crt, pair(key, true))()
		defer s1.Watch(key, pair(crt, false))()
		for _, n := range []string{"tls.crt", "tls.key"} {
			if err := os.Remove(filepath.Join(h.root, n)); err != nil {
				t.Fatal(err)
			}
		}
		h.cycle()
		before := h.fileN.value()
		h.write("tls.crt", p2.cert)
		h.write("tls.key", p2.key)
		h.r.Activate(h.resolve(uses...))
		h.cycle()
		h.cycle()
		if n := h.fileN.value() - before; n != 0 || len(h.status.raised()) != 0 {
			t.Errorf("renewal through the reload: failures +%d, raised %v", n, h.status.raised())
		}
		h.write("tls.crt", p3.cert)
		h.write("tls.key", p3.key)
		h.cycle()
		if n := h.fileN.value() - before; n != 0 || len(h.status.raised()) != 0 {
			t.Errorf("later renewal: failures +%d, raised %v", n, h.status.raised())
		}
		if get(t, s1, crt) != p3.cert || get(t, s1, key) != p3.key {
			t.Error("the carried-over Store does not hold the renewed pair")
		}
	})
	t.Run("pending value before Activate", func(t *testing.T) {
		// The new Store returns the value its uses accepted from Resolve
		// on, so a Filter compiled from it sees that value; the active
		// Store keeps its last good value until Activate. A watch made on
		// the new Store before Activate is not called again with the
		// value it read.
		h, ref, s1, key := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if a, b := get(t, s1, ref), get(t, s2, ref); a != key || b != "short" {
			t.Fatalf("before Activate: active %q, new %q", a, b)
		}
		w1, w2 := &recorder{}, &recorder{}
		defer s1.Watch(ref, w1.fn)()
		defer s2.Watch(ref, w2.fn)()
		h.cycle()
		h.r.Activate(s2)
		h.cycle()
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "short" || b != "short" {
			t.Errorf("after Activate: retained %q, active %q", a, b)
		}
		if !slices.Equal(w1.values(), []string{"short"}) || len(w2.values()) != 0 {
			t.Errorf("watchers got %v and %v", w1.values(), w2.values())
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("Store never activated", func(t *testing.T) {
		// A Revision that never becomes current changes nothing: the
		// active Store keeps its last good value and its raised reason.
		h, ref, s1, key := refusedRotation(t)
		_ = h.resolve(use(ref, secret.KindOpaque))
		h.cycle()
		h.cycle()
		if got := get(t, s1, ref); got != key || h.fileN.value() != 1 || len(h.status.raised()) != 1 {
			t.Errorf("value %q, failures %d, raised %v", got, h.fileN.value(), h.status.raised())
		}
	})
	t.Run("file changed again before Activate", func(t *testing.T) {
		// The pending value is applied at Activate; the poll it starts
		// sees the file changed since Resolve read it and reads it again
		// against the new uses.
		h, ref, s1, _ := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		h.write("k", "other")
		h.cycle() // refused by the active KindAPIKey use: counted
		h.r.Activate(s2)
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "other" || b != "other" {
			t.Errorf("retained %q, active %q", a, b)
		}
		if h.fileN.value() != 2 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("file unreadable before Activate", func(t *testing.T) {
		// A failure independent of the uses (the file removed), polled
		// between Resolve and Activate, is counted once; the pending value
		// is applied and secret_rotation_failed never clears until the
		// file reads well again.
		h, ref, s1, _ := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if err := os.Remove(filepath.Join(h.root, "k")); err != nil {
			t.Fatal(err)
		}
		h.cycle() // counted: 2
		h.r.Activate(s2)
		if len(h.status.raised()) != 1 {
			t.Errorf("raised %v at Activate", h.status.raised())
		}
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "short" || b != "short" {
			t.Errorf("retained %q, active %q", a, b)
		}
		if h.fileN.value() != 2 || len(h.status.raised()) != 1 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
		h.write("k", "short")
		h.cycle()
		if h.fileN.value() != 2 || len(h.status.raised()) != 0 {
			t.Errorf("after the file returns: failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("same file state refused before Activate", func(t *testing.T) {
		// The poll between Resolve and Activate examines the very file
		// state Resolve read and the active uses refuse it: counted once.
		// The new uses accept it, so Activate clears the failure record
		// and secret_rotation_failed with it.
		h := newHarness(t, nil)
		key := strings.Repeat("k", 30)
		h.write("k", key)
		ref := fileRef(h.path("k"), "")
		s1 := h.resolve(use(ref, secret.KindAPIKey))
		h.r.Activate(s1)
		h.cycle()
		if err := os.Remove(filepath.Join(h.root, "k")); err != nil {
			t.Fatal(err)
		}
		h.cycle() // the file is missing: counted: 1
		h.write("k", "short")
		s2 := h.resolve(use(ref, secret.KindOpaque))
		h.cycle() // the state Resolve read, refused by KindAPIKey: counted: 2
		if h.fileN.value() != 2 || len(h.status.raised()) != 1 {
			t.Fatalf("before Activate: failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
		h.r.Activate(s2)
		if len(h.status.raised()) != 0 {
			t.Errorf("raised %v at Activate", h.status.raised())
		}
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != "short" || b != "short" {
			t.Errorf("retained %q, active %q", a, b)
		}
		if h.fileN.value() != 2 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("rotation accepted before Activate", func(t *testing.T) {
		// A rotation the active uses accept, polled before Activate,
		// supersedes the pending value; a watch made on the new Store
		// before Activate converges on it.
		h, ref, s1, _ := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		w2 := &recorder{}
		defer s2.Watch(ref, w2.fn)()
		next := strings.Repeat("n", 30)
		h.write("k", next)
		h.cycle()
		h.r.Activate(s2)
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != next || b != next {
			t.Errorf("retained %q, active %q", a, b)
		}
		if got := w2.values(); !slices.Equal(got, []string{next}) {
			t.Errorf("watcher got %v", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("file reverted before Activate", func(t *testing.T) {
		// The file holds the last good value again before Activate: the
		// pending value is dropped and the cell's value is published
		// again, so a watch made on the new Store, which read the pending
		// value, converges on the file's value.
		h, ref, s1, key := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		w2 := &recorder{}
		defer s2.Watch(ref, w2.fn)()
		h.write("k", key)
		h.cycle()
		if len(h.status.raised()) != 0 {
			t.Fatalf("raised %v after the revert", h.status.raised())
		}
		h.r.Activate(s2)
		h.cycle()
		if a, b := get(t, s1, ref), get(t, s2, ref); a != key || b != key {
			t.Errorf("retained %q, active %q", a, b)
		}
		if got := w2.values(); !slices.Equal(got, []string{key}) {
			t.Errorf("watcher got %v", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("two Revisions resolved before Activate", func(t *testing.T) {
		// Each Store settles its own pending value; the later one finds
		// the cell moved and publishes it again for its watchers.
		h, ref, s1, _ := refusedRotation(t)
		s2 := h.resolve(use(ref, secret.KindOpaque))
		s3 := h.resolve(use(ref, secret.KindOpaque))
		w3 := &recorder{}
		defer s3.Watch(ref, w3.fn)()
		h.r.Activate(s2)
		h.cycle()
		h.r.Activate(s3)
		h.cycle()
		for i, st := range []secret.Store{s1, s2, s3} {
			if got := get(t, st, ref); got != "short" {
				t.Errorf("Store %d = %q", i+1, got)
			}
		}
		if got := w3.values(); !slices.Equal(got, []string{"short"}) {
			t.Errorf("watcher got %v", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
}

func TestRotationBetweenResolveAndActivate(t *testing.T) {
	// Req 46: Resolve checks a reused value against the new uses, but a
	// poll of the still-active Store before Activate checks a rotation
	// only against the old uses. Activate checks it against the new uses:
	// a refused value stays (it is the only one) but counts one failure
	// and raises secret_rotation_failed until an accepted value is read.
	h := newHarness(t, nil)
	h.write("k", strings.Repeat("k", 30))
	ref := fileRef(h.path("k"), "")
	h.r.Activate(h.resolve(use(ref, secret.KindOpaque)))
	h.cycle()
	s2 := h.resolve(use(ref, secret.KindAPIKey))
	h.write("k", "short") // lands while the Revision compiles
	h.cycle()             // accepted by the active KindOpaque use
	if h.fileN.value() != 0 {
		t.Fatalf("failures %d before Activate", h.fileN.value())
	}
	h.r.Activate(s2)
	if h.fileN.value() != 1 || len(h.status.raised()) != 1 {
		t.Errorf("at Activate: failures %d, raised %v", h.fileN.value(), h.status.raised())
	}
	h.cycle()
	h.cycle()
	if got := get(t, s2, ref); got != "short" || h.fileN.value() != 1 || len(h.status.raised()) != 1 {
		t.Errorf("value %q, failures %d, raised %v", got, h.fileN.value(), h.status.raised())
	}
	if logs := h.logs.String(); !strings.Contains(logs, msgRotationFailed) || !strings.Contains(logs, errShortAPIKey.Error()) {
		t.Errorf("log:\n%s", logs)
	}
	fixed := strings.Repeat("f", 30)
	h.write("k", fixed)
	h.cycle()
	if got := get(t, s2, ref); got != fixed || h.fileN.value() != 1 || len(h.status.raised()) != 0 {
		t.Errorf("after a valid rotation: value %q, failures %d, raised %v", got, h.fileN.value(), h.status.raised())
	}
}
