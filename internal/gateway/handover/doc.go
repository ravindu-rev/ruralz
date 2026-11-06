// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package handover runs the in-place Zero-Downtime Upgrade of ruralzd:
// a new process on the same host and ${RURALZ_DATA_DIR} binds the same
// ports with SO_REUSEPORT and reports ready over the owner-only Unix
// socket ${RURALZ_DATA_DIR}/handover.sock; the lock holder steers every
// port to it, lingers, then stops accepting and Drains, releasing the
// data-directory lock to it (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "In-place handover", "Handover protocol" and "Handover under systemd";
// docs/adr/0015-zero-downtime-upgrades-so-reuseport.md). No descriptor,
// counter or in-memory state crosses between the processes: only the
// kernel, the lock and the ruralz.handover.v1 messages.
//
// # Protocol
//
// Each message is one JSON object of at most [MaxMessage] bytes on its
// own line, its type member naming it; readers ignore unknown members.
//
//	new process  ready       version, pid, markers, activeDigest (null
//	                         for none): verified, compiled, warmed and
//	                         bound, not listening
//	holder       refused     reason unknown_marker, older_candidate,
//	                         draining or busy: the new process exits 1
//	                         holding no listening socket
//	holder       reload      digest: a newer holder candidate; the new
//	                         process reloads it and sends ready again
//	holder       accepted    with the usage at acceptance: connections,
//	                         inflightUnits, bufferedBytes,
//	                         pluginMemoryBytes, rssBytes; and, while the
//	                         holder runs a watchdog, watchdogUsec, its
//	                         interval in microseconds
//	holder       usage       the usage members, every UsageInterval
//	new process  listening   its sockets joined their groups
//	holder       released    the lock was released at its Drain's start
//
// The holder refuses a ready message it cannot read with unknown_marker:
// another type, markers lacking one of handover, lkg, holder and
// canonical or naming an identifier this release does not read
// ([Markers]), or a malformed digest. Both sides check the other's user
// with SO_PEERCRED and refuse another user than their effective one.
//
// # Holder
//
// [StartHolder] serves the socket of the process holding the lock. A
// ready message is answered once no activation runs ([Activations]):
// accepted when its activeDigest is the holder's active Revision, or the
// holder has none; reload of the holder's candidate when that is the
// active Revision; else refused with older_candidate (a failed candidate
// write, or no candidate on an active holder). A ready message meets
// refused with draining once the holder's Drain started, and with busy
// while another new process's handover is in progress; a connection must
// be accepted within [GateTimeout], the new process's own bound. Before
// accepted the holder waits until no socket a Hot Reload replaced
// lingers, since the kernel would move the new process's socket into the
// place of one that closes, and holds its Drain (drain.Drainer.Hold);
// activations stay held from accepted until the holder's Drain, so a
// source change waits for the new process.
//
// The accepted message carries the interval the holder's watchdog runs
// with ([HolderConfig].Watchdog), so a new process to which the service
// manager gives none, as systemd gives none to a child of ExecReload=,
// feeds the unit's watchdog once it takes over; a process that took the
// lock as a successor passes the interval it runs with on in turn.
// Accepted, the holder reports its usage every [UsageInterval]. On
// listening, within [ListeningTimeout] of accepted, it steers every group
// to the new process, the admin port's included, closes idle admin
// connections and answers admin requests with Connection: close, so
// keep-alive health checkers move and no probe sees the Drain's 503, and
// keeps accepting for the [Linger]. At the linger's end it stops
// accepting on every socket, client and admin, then starts the
// handover's Drain (drain.Drainer.Handover), which mutes sd_notify, fails
// readiness, stops the source and releases the lock through [Lock]:
// handover.sock is closed and removed while the lock is held, the lock is
// released, then released is sent. Usage reports go on until the holder
// exits ([Holder.Close]).
//
// A new process that sends no listening in time, or whose connection
// closes first, aborts the handover: the holder resumes its Drain hold
// and activations and keeps serving. One lost during the linger makes it
// re-pin every group to its own sockets, keep admin connections alive
// again, skip its Drain and keep serving; a stop signal received
// meanwhile then starts the stop-signal Drain (drain.Drainer.Resume).
//
// # New process
//
// [Successor.Run] runs the gate of a process that found a live lock
// holder at start, booted the holder's active Revision from its verified
// candidate and bound its sockets without listening ([Bound]): every
// [RetryInterval] until [GateTimeout] after its start it takes the lock
// if the holder exited, else connects to the socket and sends ready.
// Refused, or not accepted in time, it closes its sockets and exits 1
// ([OutcomeRefused]); reload runs its reload and sends ready again; a
// failed reload refuses. Accepted, it records the holder's watchdog
// interval ([Successor.HolderWatchdog]), none when accepted carries no
// watchdogUsec; a negative one, or one a time.Duration cannot hold, breaks
// the protocol, and the process gives up as when refused. It lowers each
// Node-wide ceiling by the holder's usage ([Ceilings]) and sets a soft
// memory limit (debug.SetMemoryLimit) of [MemorySharePercent] of its
// cgroup's memory.max minus the holder's rssBytes, or without a cgroup
// limit GOMEMLIMIT minus rssBytes, and none with neither; it listens,
// still pinned to the holder, and sends listening. On released it takes
// the lock, retrying while another process holds it, pins its groups,
// writes holder.json and sends MAINPID= with READY=1 and STATUS=
// ([Notifier]), in that order, so the ExecReload= helper exits 0 and the
// service manager follows the new main process: [OutcomeHolder]. Only then
// does the process start its watchdog (internal/gateway), with its own
// interval when its environment enables one, else with the holder's, so
// no ping precedes the lock. A follower goes on applying the old holder's
// usage reports and restores the ceilings and the process's own memory
// limit once the old holder is gone.
//
// A connection that ends after accepted and before released, as when
// the holder crashed, makes the new process take the lock, retrying
// every RetryInterval until the gate's end and at least once; when the
// holder keeps it, a process whose listening reached the holder Drains
// on the failure timeline ([OutcomeLost]), and one whose listening did
// not closes its sockets and exits 1.
//
// # Stop signals
//
// A stop signal during a handover ends it in order. The holder answers a
// ready message with draining once its Drain started; after accepted the
// Drain waits for the linger's end, then runs on the handover timeline,
// or on the stop-signal timeline when the new process is lost during the
// linger. A new process whose Drain starts before it listens closes its
// sockets and its Drain, never ready and with nothing bound, goes
// straight to the flush ([OutcomeStopped]); one already listening closes
// the connection, so a holder still lingering re-pins, and Drains like
// any holder. A new process sends no sd_notify message until it takes
// the lock (sdnotify.Notifier held).
//
// # Platforms
//
// The in-place handover is Linux only: elsewhere SO_PEERCRED is missing,
// so the holder refuses every peer and a new process never connects.
package handover
