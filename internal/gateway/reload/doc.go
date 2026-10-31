// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package reload is the activation loader of a Node: it boots the first
// Revision, activates every later one with a Hot Reload, keeps
// Last-Known-Good, and serves /debug/snapshots and /config/dump
// (docs/architecture/03-data-plane.md "Configuration snapshots and hot
// reload", "Sources, Last-Known-Good and Drain" and "Admin endpoints";
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Configuration hot reload").
//
// # Activation
//
// A Revision the configuration pipeline accepted (gate steps 1 to 6:
// stages A to M of a source load, internal/gateway/source, or canonical
// re-entry of lkg/ content) activates off the request path: the snapshot
// compiler runs steps 7, 9 and 10 (Compiler); every Part readies its
// change, the client listeners binding added or changed sockets without
// listening (step 11, RZ-CFG-039), the admin port binding a moved port,
// the telemetry settings compiling; then one atomic pointer store
// publishes the snapshot (retire.Holder.Publish), its metric label sets
// bound at the swap; the Node switches its secret resolver and Upstream
// runtimes to it and every Part commits; readiness reports the secrets,
// listeners and Revision, in that order; the Revision's canonical form is
// written under lkg/ as the candidate, which file mode promotes to
// Last-Known-Good; and the retirer retires the previous snapshot. Steps 1
// to 12 run within one compile deadline, 60 s (target), the load's time
// counting against it and a wait for the retirer not: a Revision not
// ready to publish when it expires is rejected with the transient
// RZ-CFG-044. A panic, or an error without an RZ code, is recovered at
// this boundary, logged with its stack, and rejects the Revision with
// RZ-CFG-045. A Node never activates part of a Revision: a rejected one
// closes everything it opened and the active Revision keeps serving.
//
// Each activation counts once in ruralz_config_activations_total, times
// the verify (canonical content only: a watched directory is unsigned),
// compile, swap and total stages of
// ruralz_config_activation_duration_seconds by the Revision's size class,
// sets ruralz_config_revision_info{role="active"}, logs the activation,
// its warnings and the file descriptor warning of "Bounded resources".
// Degraded reason header_limit_capped stays raised while the active
// Revision's limits.maxRequestHeaderBytes exceeds the 256 KiB process
// ceiling that caps it ("Listeners and protocols").
//
// # Rejections and retries
//
// A rejected load writes one warn record with the code of its first
// error, the number of diagnostics and up to ten of them, the rest at
// debug, counts once in ruralz_config_activations_total with that code,
// parse and schema failures included, and becomes lastRejected of
// /debug/snapshots. A source load whose every failure is transient in the
// validation gate (RZ-CFG-026, RZ-CFG-039, RZ-CFG-042, RZ-CFG-044) is
// retried with full-jitter backoff from 1 s to 60 s (target) until it
// activates or the source changes; a retry that fails again logs at info
// and counts nothing more. While no Revision is active, /readyz names the
// unresolved secretRef values and unbound listeners of the boot load
// being retried; once one is active, a newer one that fails never changes
// readiness. A load that renders the active Revision activates nothing,
// counts nothing and logs at debug.
//
// # Pending Revisions
//
// The loader keeps only the latest pending Revision. It activates it once
// the retirer accepts a new snapshot (retire.Holder.WaitActivate), so a
// closing snapshot's grace is never cut short, and once no handover holds
// activations.
//
// # Boot
//
// A process that took the data-directory lock at start boots by the
// file-mode order (BootHolder): the source, else Last-Known-Good under
// degraded reason lkg_boot, else it stays not ready. Last-Known-Good is
// re-hashed (RZ-CFG-027 boots nothing) and re-validated from the schema
// stage (RZ-CFG-024 for content a newer release wrote). It serves under
// lkg_boot until a source Revision activates or a source load renders the
// active Revision; a transient source load keeps being retried meanwhile.
// Booted closes once the first boot attempt ended, ready or not, for
// READY=1. The source watcher starts then.
//
// A handover successor (BootSuccessor) boots the holder's active Revision
// from its candidate through the full gate, without a boot wait, or by
// the file-mode order when the holder has none. Its activations hold
// their switches, so nothing listens before the handover is accepted:
// Bound.Listen commits them, Bound.Close closes everything, Reload boots
// a newer holder candidate, and TookLock starts the source watcher once
// the process holds the lock.
//
// # Last-Known-Good
//
// Only the lock holder writes lkg/ (internal/gateway/lkg). A failed write
// retries after 1 s, doubling to 60 s (target), under degraded reason
// lkg_write_failed; a successor that took the lock writes its active
// Revision when lkg/ names another, as a job of the activation loop after
// TookLock returned. ruralz_config_revision_info{role="lkg"} follows what
// lkg.json names.
//
// # Handover and Drain
//
// Hold and Release are the holder's handover.Activations: Hold waits for
// a running activation, its candidate write included, then holds every
// later one. Stop, the Drain's StopSource, stops the source: nothing
// activates afterwards, the pending Revision is dropped and an activation
// still before its swap is abandoned.
//
// # Watchdog
//
// Probe is the activation loop's watchdog probe (sdnotify.Probe): the
// loop answers it whenever it runs, unless a job of it has run past its
// compile deadline. retire.Holder.Ping is the retirer's.
package reload
