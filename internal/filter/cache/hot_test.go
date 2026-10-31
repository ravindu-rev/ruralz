// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"bytes"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// hotKeyOf returns partition n of one URI.
func hotKeyOf(n string) statestore.CacheKey {
	return statestore.CacheKey{URI: statestore.DigestOf("uri"), Partition: statestore.DigestOf(n)}
}

// found returns a lookup that found entry under names, resident for r.
func found(entry string, r time.Duration) *statestore.CacheLookup {
	return &statestore.CacheLookup{Partition: true, Names: "accept", Found: true, Entry: []byte(entry), Resident: r}
}

// TestHotLayerHoldsKeysReadTwice checks rule 1: a generation token or an
// entry looked up twice within the hold is kept from the read that follows
// the second lookup for one hold, then read again.
func TestHotLayerHoldsKeysReadTwice(t *testing.T) {
	l := newHotLayer(1<<20, time.Second)
	u := statestore.DigestOf("uri")
	tok := statestore.Token{9}
	k, v := hotKeyOf("p"), statestore.DigestOf("v")

	l.readToken(u, tok, true, t0)
	l.readEntry(k, v, found("e1", time.Second), t0)
	if _, ok := l.token(u, t0); ok {
		t.Fatal("a token read once is held")
	}
	if _, ok := l.entry(k, v, t0); ok {
		t.Fatal("an entry read once is held")
	}
	at := t0.Add(500 * time.Millisecond)
	l.readToken(u, tok, true, at)
	l.readEntry(k, v, found("e1", time.Second), at)
	if got, ok := l.token(u, at.Add(999*time.Millisecond)); !ok || got != tok {
		t.Fatalf("token(%v) = %v, %v; want the held token", at.Add(999*time.Millisecond), got, ok)
	}
	he, ok := l.entry(k, v, at.Add(400*time.Millisecond))
	if !ok || string(he.entry) != "e1" || he.names != "accept" || he.resident != 1400*time.Millisecond {
		t.Fatalf("entry = %+v, %v; want e1 resident 1.4 s", he, ok)
	}
	if _, ok := l.token(u, at.Add(time.Second)); ok {
		t.Error("the token is held past its hold")
	}
	// The key stayed hot: the read as the hold ends keeps it again.
	end := at.Add(time.Second)
	l.readToken(u, tok, true, end)
	if _, ok := l.token(u, end); !ok {
		t.Error("a key looked up within the last hold was not held again by its next read")
	}
}

// TestHotLayerHoldsOnlyWhatExists checks that a missing generation key and
// a variant not found are never held.
func TestHotLayerHoldsOnlyWhatExists(t *testing.T) {
	l := newHotLayer(1<<20, time.Second)
	u := statestore.DigestOf("uri")
	k, v := hotKeyOf("p"), statestore.DigestOf("v")
	miss := &statestore.CacheLookup{Partition: true, Names: ""}
	for i := range 3 {
		at := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		l.readToken(u, statestore.Token{}, false, at)
		l.readEntry(k, v, miss, at)
	}
	if _, ok := l.token(u, t0.Add(time.Second/2)); ok {
		t.Error("a missing generation key is held")
	}
	if _, ok := l.entry(k, v, t0.Add(time.Second/2)); ok {
		t.Error("a variant not found is held")
	}
}

// TestHotLayerCopiesEntries checks that a held entry is a copy: the
// lookup's buffer may be reused at once.
func TestHotLayerCopiesEntries(t *testing.T) {
	l := newHotLayer(1<<20, time.Second)
	k, v := hotKeyOf("p"), statestore.DigestOf("v")
	lk := found("abc", 0)
	l.readEntry(k, v, lk, t0)
	l.readEntry(k, v, lk, t0)
	copy(lk.Entry, "xyz")
	if he, ok := l.entry(k, v, t0); !ok || !bytes.Equal(he.entry, []byte("abc")) {
		t.Errorf("held entry %q, %v; want its own copy", he.entry, ok)
	}
}

// TestHotLayerBound checks the byte bound: past it the least recently used
// keys go.
func TestHotLayerBound(t *testing.T) {
	l := newHotLayer(2*hotOverhead+3000, time.Second)
	v := statestore.DigestOf("v")
	big := string(make([]byte, 1000))
	for _, p := range []string{"a", "b", "c"} {
		l.readEntry(hotKeyOf(p), v, found(big, 0), t0)
		l.readEntry(hotKeyOf(p), v, found(big, 0), t0)
	}
	if got := l.bytes(); got > 2*hotOverhead+3000 {
		t.Errorf("layer holds %d bytes past its bound", got)
	}
	if _, ok := l.entry(hotKeyOf("a"), v, t0); ok {
		t.Error("the least recently used entry was kept past the bound")
	}
	if _, ok := l.entry(hotKeyOf("c"), v, t0); !ok {
		t.Error("the newest entry was evicted")
	}
}

// TestHotLayerDropToken checks that a Node forgets a URI's token at once
// after bumping it.
func TestHotLayerDropToken(t *testing.T) {
	l := newHotLayer(1<<20, time.Second)
	u := statestore.DigestOf("uri")
	l.readToken(u, statestore.Token{1}, true, t0)
	l.readToken(u, statestore.Token{1}, true, t0)
	l.dropToken(u)
	if _, ok := l.token(u, t0); ok {
		t.Error("the token survived dropToken")
	}
	if l.bytes() != 0 {
		t.Errorf("layer charges %d bytes after dropping its only key", l.bytes())
	}
}
