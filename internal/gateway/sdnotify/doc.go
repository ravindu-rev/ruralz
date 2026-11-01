// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package sdnotify reports a Node's state to systemd through the
// notification protocol of sd_notify(3) and feeds the service watchdog
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Handover
// under systemd"; the T5 unit sets Type=notify, NotifyAccess=all and
// WatchdogSec=30s).
//
// # Messages
//
// A [Notifier] sends each message as one datagram of newline-separated
// assignments to the AF_UNIX socket NOTIFY_SOCKET names, an absolute path
// or an abstract socket name starting with "@", using the standard
// library only:
//
//	READY=1, STATUS=                  a process that took the lock at start,
//	                                  after its first boot attempt, ready or
//	                                  not (Notifier.Ready)
//	MAINPID=<pid>, READY=1, STATUS=   a handover successor once it takes the
//	                                  lock (Notifier.MainPID)
//	STATUS=                           every readiness change
//	                                  (Notifier.SyncStatus)
//	STOPPING=1, STATUS=draining       the start of a Drain a stop signal
//	                                  started (Notifier.Stopping)
//	WATCHDOG=1                        the watchdog (Notifier.Watchdog)
//
// STATUS= is formed from the /readyz state ([StatusOf]): "ready
// rev-<12 hex>", "not ready: <reason> <code> <detail>" with reasons joined
// by a semicolon and a space, or "draining". It is one line of valid UTF-8,
// so a detail can never inject an assignment, and it is cut so the whole
// datagram fits [MaxMessage], since systemd ignores a larger one. Without
// NOTIFY_SOCKET every method does nothing.
//
// # Gate
//
// A Notifier is open, held or muted. A process that took the lock at
// start is open. A handover successor starts held and sends nothing,
// watchdog pings included, until MainPID opens it after it takes the
// lock. A process that handed over mutes its Notifier at its Drain's start
// and sends nothing more: under NotifyAccess=all systemd would take its
// messages for the unit's, so a STOPPING=1 would stop the unit the
// successor serves and a WATCHDOG=1 would hide a hung successor.
//
// # Watchdog
//
// With WATCHDOG_USEC set ([ReadEnv], [WatchdogInterval]), a [Watchdog]
// sends WATCHDOG=1 every half interval while every registered part, the
// admin server, the retirer and the activation loop, has run within the
// interval: each part has a [Probe] that returns once the part has run,
// and a part counts as run when its last probe succeeded no longer than
// one interval ago. Its first WATCHDOG=1 goes as soon as every part has
// run, not a half interval after it starts. As sd_watchdog_enabled(3)
// does, a WATCHDOG_PID naming another process means no watchdog. systemd
// sets both variables for the unit's ExecStart= process only, so a
// handover successor, which the ExecReload= helper starts, finds no
// interval of its own: its Watchdog runs with the interval its holder's
// handover accepted message carried, started once it took the lock and
// sent MAINPID=. Its predecessor pinged every half interval until its
// Drain started, so the successor's first ping, as soon as its parts
// have run, comes before the service manager's watchdog runs out. Time
// comes from an injected clock.Clock.
package sdnotify
