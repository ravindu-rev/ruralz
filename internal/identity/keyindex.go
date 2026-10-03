// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// SecretKey is one secretRef-held API key of a Consumer: the binding a
// KeyIndex maintains a digest for.
type SecretKey struct {
	// Consumer is the Consumer's metadata.name.
	Consumer string
	// Name is the apiKeys[].name.
	Name string
	// Ref is the secret reference.
	Ref secret.Ref
}

// compareSecretKeys orders by Consumer, key name, then reference.
func compareSecretKeys(a, b SecretKey) int {
	return cmp.Or(
		strings.Compare(a.Consumer, b.Consumer),
		strings.Compare(a.Name, b.Name),
		strings.Compare(string(a.Ref.Provider), string(b.Ref.Provider)),
		strings.Compare(a.Ref.Name, b.Ref.Name),
		strings.Compare(a.Ref.Key, b.Ref.Key),
	)
}

// keyEntry is the binding of one digest: a Consumer name, or ambiguous
// when two distinct Consumers hold keys with this digest.
type keyEntry struct {
	consumer  string
	ambiguous bool
}

type keyTable map[[32]byte]keyEntry

// KeyIndex holds the SHA-256 digests of secretRef-held API keys (spec 06
// requirements 12 and 13). The request path reads an immutable table
// through an atomic pointer, without locks; a rotation of a watched secret
// builds a new table and swaps it in (copy-on-write). Watches are
// registered through secret.Store.Watch, which is Node-wide and keyed by
// Ref (R-55), so a KeyIndex carried over into later snapshots keeps
// following rotations after the resolver activates a newer Store.
//
// A KeyIndex is shared by reference count: NewKeyIndex returns it with one
// reference held by the caller, Retain adds one, and the Release that drops
// the last one stops every watch. Bindings are Consumer names, never
// expr.Consumer pointers, so a carried-over KeyIndex resolves through the
// current snapshot's Index.
type KeyIndex struct {
	keys  []SecretKey
	refs  []secret.Ref // distinct refs of keys, in key order
	table atomic.Pointer[keyTable]

	mu      sync.Mutex
	digests [][32]byte // digest of keys[i]; guarded by mu
	count   int        // references; guarded by mu
	closed  bool       // guarded by mu
	stops   []func()   // guarded by mu
}

// NewKeyIndex resolves every key through store (a resolved Revision's
// secret table), normalizes and hashes it (keys under MinKeyBytes and
// references the store does not hold reject the Revision with RZ-CFG-026),
// discards the raw bytes, and registers one watch per distinct reference.
// When prev holds exactly the same bindings and is still open, prev is
// carried over instead (its watches kept) and gains a reference. With no
// keys it returns nil, a valid empty KeyIndex for Index.WithSecretKeys.
func NewKeyIndex(keys []SecretKey, store secret.Store, prev *KeyIndex) (*KeyIndex, error) {
	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, compareSecretKeys)
	sorted = slices.Compact(sorted)
	if len(sorted) == 0 {
		return nil, nil
	}
	if prev != nil && slices.Equal(prev.keys, sorted) && prev.Retain() {
		return prev, nil
	}
	if store == nil {
		return nil, errcode.Errorf(CodeUnresolved, "secretRef-held API keys need a resolved secret table")
	}
	k := &KeyIndex{keys: sorted, digests: make([][32]byte, len(sorted)), count: 1}
	for i, sk := range sorted {
		if !slices.Contains(k.refs, sk.Ref) {
			k.refs = append(k.refs, sk.Ref)
		}
		v, ok := store.Get(sk.Ref)
		if !ok {
			return nil, errcode.Errorf(CodeUnresolved, "consumer %q apiKeys %q: secretRef %s is not resolved", sk.Consumer, sk.Name, sk.Ref)
		}
		d, err := SecretKeyDigest(v.Reveal())
		if err != nil {
			return nil, errcode.Wrap(CodeUnresolved, fmt.Errorf("consumer %q apiKeys %q: %w", sk.Consumer, sk.Name, err))
		}
		k.digests[i] = d
	}
	k.mu.Lock()
	k.publishLocked()
	k.mu.Unlock()

	stops := make([]func(), 0, len(k.refs))
	for _, ref := range k.refs {
		stops = append(stops, store.Watch(ref, func(v secret.Value) error { return k.rotate(ref, v) }))
	}
	k.mu.Lock()
	k.stops = stops
	k.mu.Unlock()
	// A rotation between the first read and the registration is applied
	// here; the watch covers every later one. A short value keeps the last
	// digest, and the resolver reports that rotation failure itself.
	for _, ref := range k.refs {
		if v, ok := store.Get(ref); ok {
			_ = k.rotate(ref, v)
		}
	}
	return k, nil
}

// rotate applies a new value of ref: a value under MinKeyBytes returns an
// error, which keeps the last digest and makes the resolver count a
// rotation failure (spec 06 requirement 13); otherwise every key bound to
// ref takes the new digest and, if any changed, a new table is published.
func (k *KeyIndex) rotate(ref secret.Ref, v secret.Value) error {
	d, err := SecretKeyDigest(v.Reveal())
	if err != nil {
		return fmt.Errorf("identity: rotation of %s: %w", ref, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil
	}
	changed := false
	for i := range k.keys {
		if k.keys[i].Ref == ref && k.digests[i] != d {
			k.digests[i] = d
			changed = true
		}
	}
	if changed {
		k.publishLocked()
	}
	return nil
}

// publishLocked builds a table from the current digests and swaps it in.
func (k *KeyIndex) publishLocked() {
	t := make(keyTable, len(k.keys))
	for i, sk := range k.keys {
		e, dup := t[k.digests[i]]
		switch {
		case !dup:
			e.consumer = sk.Consumer
		case e.consumer != sk.Consumer:
			e.ambiguous = true
		}
		t[k.digests[i]] = e
	}
	k.table.Store(&t)
}

// lookup returns the binding of digest; lock-free.
func (k *KeyIndex) lookup(digest [32]byte) (keyEntry, bool) {
	t := k.table.Load()
	if t == nil {
		return keyEntry{}, false
	}
	e, ok := (*t)[digest]
	return e, ok
}

// Keys returns the bindings, sorted. The slice is shared: read it only.
func (k *KeyIndex) Keys() []SecretKey { return k.keys }

// Retain takes a reference; it returns false once the last reference was
// released (the watches are gone and the KeyIndex must not be reused).
func (k *KeyIndex) Retain() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return false
	}
	k.count++
	return true
}

// Release drops a reference; the last one stops every watch. Releasing a
// closed KeyIndex does nothing.
func (k *KeyIndex) Release() {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.count--
	if k.count > 0 {
		k.mu.Unlock()
		return
	}
	k.closed = true
	stops := k.stops
	k.stops = nil
	k.mu.Unlock()
	for _, stop := range stops {
		if stop != nil {
			stop()
		}
	}
}
