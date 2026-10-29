// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package readiness is the /readyz state of a Ruralz Gateway Node
// (docs/architecture/03-data-plane.md "Admin endpoints"; foundation pack
// section 8.5): 200 only with an active validated Revision, every
// secretRef it uses resolved, its listeners bound and no Drain, else 503
// with the reasons no_revision, secrets_unresolved, listeners_unbound and
// draining, in that order. Nothing else is a condition: a lost Control
// Stream or State Store never fails readiness, for any duration.
//
// A Tracker starts not ready, with no Revision and its listeners not yet
// bound. Activation reports the unresolved secretRef values and the
// unbound listeners of the Revision it activates, then the Revision
// itself, so /readyz never reads ready between two updates. Drain fails
// readiness at once and for good ("Sources, Last-Known-Good and Drain").
//
// Every update re-encodes the body once, so a probe costs one atomic load
// and one write. A Tracker is safe for concurrent use.
package readiness
