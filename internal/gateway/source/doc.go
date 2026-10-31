// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package source is the configuration source of a file-mode Node: it
// watches RURALZ_CONFIG and loads it through the configuration pipeline
// (docs/architecture/03-data-plane.md "Sources, Last-Known-Good and
// Drain"; docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Configuration hot reload"). The activation loader
// (internal/gateway/reload) activates what it loads.
//
// # Watching
//
// A Watcher polls the source every PollInterval, 1 s (target), with the
// standard library alone (golang.org/x/sys stays confined, so no inotify):
//
//   - a replaced root loads at once: a directory renamed into place, a
//     file replaced by rename, a new symbolic-link target, a source that
//     appears;
//   - any other change to the walked names, sizes, modes or modification
//     times loads once the next poll sees no further change: the settle
//     debounce;
//   - a tree that changes while it loads is discarded, never delivered,
//     and loaded again: at once when its root was replaced, else once it
//     settles;
//   - Reload, which SIGHUP calls (NotifySIGHUP), loads at once, skipping
//     the settle.
//
// The walk reads every entry under a directory source but those whose
// name starts with a dot, which the loader never reads, except
// .ruralzignore files, which it does. A change that renders the active
// Revision is the loader's to ignore: the Watcher loads, the loader
// compares digests.
//
// # Loads
//
// Each load runs pipeline stages A to M on the source without an
// Environment, substituting ${VAR} from the process environment
// (Config.Variables), under the compile deadline, 60 s (target), which a
// load's time counts against; the activation gets the rest. A Load
// carries the pipeline's result, or why it has none: a source that cannot
// be read (ErrUnavailable: missing, unreadable, not a directory or
// regular file), a load past the deadline (ErrDeadline), a panic
// (*PanicError) or another fault. A missing source is reported once per
// disappearance and on every Reload.
//
// Load loads once outside Run, as a boot does: it loads again until the
// tree stays the same during a load, and records what it loaded, so Run
// loads only after a change.
//
// AdminPort reads Gateway.spec.admin.port from the source when its
// Gateway parses (stages A to E), so a Node binds the admin port of the
// Bundle it is booting before its first activation
// (docs/architecture/03-data-plane.md "Admin endpoints").
//
// # SIGHUP
//
// NotifySIGHUP registers SIGHUP and forwards it to Reload, so a SIGHUP
// loads the source and never starts a Drain or ends the process, as Go's
// default action would. The wiring registers it at start, before the
// Watcher runs; a Reload before Run makes Run's first poll load.
package source
