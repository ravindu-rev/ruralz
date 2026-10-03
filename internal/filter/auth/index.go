// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/identity"
)

// ErrIndexClosed is returned when every Filter that held the snapshot's
// credential index released it before this Filter's build acquired it (a
// compile abandoned part-way).
var ErrIndexClosed = errors.New("auth: credential index released during compile")

// IndexHolder is implemented by auth Filters that keep a credential index,
// so the next snapshot's compile carries their KeyIndex over through
// BuildEnv.Previous (its secret watches kept, R-55).
type IndexHolder interface {
	CredentialIndex() *identity.Index
}

// indexKey memoizes the index in BuildEnv.Shared.
type indexKey struct{}

// sharedIndex is the memoized index; the first acquirer takes over the
// reference the build holds, later ones take their own.
type sharedIndex struct {
	ix      *identity.Index
	claimed atomic.Bool
}

// AcquireIndex returns the credential index of the snapshot being compiled
// (spec 06 requirement 12): built once per snapshot through env.Shared
// from the Consumers of env.Bundle, the compiled views env.Consumers and
// the resolved secrets env.Secrets, carrying over the KeyIndex of
// env.Previous when it is an IndexHolder whose bindings are unchanged.
// Every successful call holds one KeyIndex reference for the calling
// Filter, which it drops with Index.Release from its Close. Without
// env.Shared the index is built for this call alone. Build errors carry an
// RZ-CFG code and reject the Revision.
//
// Consumers may change without an auth Policy changing, so a Filter that
// reuses BuildEnv.Previous still acquires the new snapshot's index.
func AcquireIndex(env filter.BuildEnv) (*identity.Index, error) {
	build := func() (any, error) {
		var prev *identity.KeyIndex
		if h, ok := env.Previous.(IndexHolder); ok {
			if p := h.CredentialIndex(); p != nil {
				prev = p.KeyIndex()
			}
		}
		ix, err := identity.BuildBundle(env.Bundle, env.Consumers, env.Secrets, prev)
		if err != nil {
			return nil, fmt.Errorf("auth: credential index: %w", err)
		}
		return &sharedIndex{ix: ix}, nil
	}
	if env.Shared == nil {
		v, err := build()
		if err != nil {
			return nil, err
		}
		return v.(*sharedIndex).ix, nil
	}
	v, err := env.Shared.Get(indexKey{}, build)
	if err != nil {
		return nil, err
	}
	s, ok := v.(*sharedIndex)
	if !ok {
		return nil, fmt.Errorf("auth: credential index: shared value is %T", v)
	}
	if s.claimed.CompareAndSwap(false, true) {
		return s.ix, nil
	}
	if !s.ix.Retain() {
		return nil, ErrIndexClosed
	}
	return s.ix, nil
}
