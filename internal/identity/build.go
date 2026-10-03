// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Build compiles the complete credential index of one snapshot: NewIndex
// over consumers, then NewKeyIndex over its secretRef-held API keys with
// store, carrying prev over when its bindings are unchanged. The returned
// view holds one KeyIndex reference for the caller (Index.Release).
func Build(consumers []*v1alpha1.Consumer, compiled map[string]*expr.Consumer, store secret.Store, prev *KeyIndex) (*Index, error) {
	ix, err := NewIndex(consumers, compiled)
	if err != nil {
		return nil, err
	}
	k, err := NewKeyIndex(ix.SecretKeys(), store, prev)
	if err != nil {
		return nil, err
	}
	return ix.WithSecretKeys(k), nil
}

// BuildBundle is Build over the Consumers of b; a nil b has none.
func BuildBundle(b *hub.Bundle, compiled map[string]*expr.Consumer, store secret.Store, prev *KeyIndex) (*Index, error) {
	var cs []*v1alpha1.Consumer
	if b != nil {
		cs = b.Consumers()
	}
	return Build(cs, compiled, store, prev)
}
