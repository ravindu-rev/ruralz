// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package secret is the contract for resolved secretRef values. Only Nodes
// resolve secrets (Security and identity, "Secrets" rule 1); the resolver
// lives in internal/secret/resolver, which ruralz and ruralz-control never
// import (depguard). A Value never prints its bytes: every formatting,
// logging and encoding path yields "[REDACTED]".
package secret

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Redacted replaces every secret value in output.
const Redacted = "[REDACTED]"

// Ref identifies a secret: provider, name and optional key. It is part of
// the canonical form; the resolved value never is.
type Ref struct {
	// Provider is env or file in M1 (kubernetes and vault from M2).
	Provider v1alpha1.SecretProvider
	// Name is the variable name or absolute file path.
	Name string
	// Key selects a JSON member of a file; unused for env.
	Key string
}

// RefOf converts a configuration reference.
func RefOf(r v1alpha1.SecretRef) Ref { return Ref{Provider: r.Provider, Name: r.Name, Key: r.Key} }

// String returns "<provider>:<name>" plus "#<key>" when set; never a value.
func (r Ref) String() string {
	s := string(r.Provider) + ":" + r.Name
	if r.Key != "" {
		s += "#" + r.Key
	}
	return s
}

// Value is a resolved secret. Its bytes are reachable only through Reveal.
type Value struct{ b []byte }

// NewValue copies b into a set Value; an empty b gives a set, empty value
// (IsZero false), a nil b the unset one.
func NewValue(b []byte) Value {
	if b == nil {
		return Value{}
	}
	c := make([]byte, len(b))
	copy(c, b)
	return Value{b: c}
}

// Reveal returns a copy of the bytes (nil when unset). Never log or wrap
// the result.
func (v Value) Reveal() []byte { return bytes.Clone(v.b) }

// Len returns the value length.
func (v Value) Len() int { return len(v.b) }

// IsZero reports an unset value; a resolved empty secret is not zero.
func (v Value) IsZero() bool { return v.b == nil }

// String returns "[REDACTED]".
func (Value) String() string { return Redacted }

// GoString returns "[REDACTED]".
func (Value) GoString() string { return Redacted }

// Format writes "[REDACTED]" for every verb.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalJSON returns the JSON string "[REDACTED]".
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText returns "[REDACTED]".
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue returns "[REDACTED]" for log/slog.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Kind says what a value holds; it selects the check run at resolution
// and rotation, whose failure is RZ-CFG-026 (activation) or a rotation
// failure (running Node).
type Kind uint8

// Secret kinds.
const (
	// KindOpaque is checked only for size.
	KindOpaque Kind = iota
	// KindPEMCertificate is a PEM certificate chain.
	KindPEMCertificate
	// KindPEMPrivateKey is a PEM private key.
	KindPEMPrivateKey
	// KindPEMCertPool is a PEM CA bundle.
	KindPEMCertPool
	// KindPEMCRL is PEM certificate revocation lists (16 MiB cap).
	KindPEMCRL
	// KindAPIKey is an API key: trimmed, at least 22 bytes.
	KindAPIKey
	// KindStateStoreURL is a State Store URL that must fit its topology.
	KindStateStoreURL
)

// Use is one reference to resolve, with its location for diagnostics.
type Use struct {
	// Ref is the reference.
	Ref Ref
	// Resource and Path locate the x-ruralz-secret field.
	Resource diag.ResourceID
	// Path is the key-aware path of the field.
	Path diag.Path
	// Kind selects the default check and size cap.
	Kind Kind
	// Check is an extra consumer check (such as a State Store URL fitting
	// its topology); nil for none. Its error text must not contain the value.
	Check func([]byte) error
}

// Store is the resolved secret table of one Revision, read lock-free on
// the request path. The set of references a Store holds never changes;
// their values follow rotations (copy-on-write in the resolver's
// Node-wide table), so a Store kept by a carried-over Filter or a retired
// snapshot keeps seeing current values.
type Store interface {
	// Get returns the latest resolved value of r, rotations included;
	// false when r is not one of the Store's uses.
	Get(r Ref) (Value, bool)
	// Watch registers fn for rotations of r. Registrations are Node-wide,
	// keyed by Ref and owned by the resolver: they survive Activate of a
	// later Store (a Filter carried over through BuildEnv.Previous keeps
	// its watch), fire whenever the active Store polls r, and stay silent
	// while no active Store references r. fn runs on the resolver's
	// goroutine and must not block; an fn error keeps the watcher's last
	// value and counts a rotation failure. stop unregisters and is called
	// from the Filter's Close.
	Watch(r Ref, fn func(Value) error) (stop func())
}

// Resolver resolves every Use of a Revision before activation (env and
// file in M1) and polls file references afterwards.
type Resolver interface {
	// Resolve returns a Store holding every use, or RZ-CFG-026 diagnostics
	// for each failing use (all reported, values never included).
	Resolve(ctx context.Context, uses []Use) (Store, diag.List)
	// Activate makes s current: the polled set becomes s's file
	// references. Watch registrations are kept.
	Activate(s Store)
	// Current returns the active Store.
	Current() Store
	// Run polls file references until ctx is done; its owner waits for it.
	Run(ctx context.Context) error
}
