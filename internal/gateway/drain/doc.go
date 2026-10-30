// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package drain runs the Drain of a Node: the graceful shutdown that fails
// readiness, stops accepting, shuts the client listeners' server shards
// down in waves and ends the remaining work at a bounded deadline
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults"; docs/architecture/03-data-plane.md "Sources,
// Last-Known-Good and Drain").
//
// # Triggers
//
// A [Drainer] runs one Drain, of one of three kinds:
//
//   - a stop signal, SIGTERM (which ruralz node drain sends) or SIGINT,
//     through [Drainer.Signal], which [Notify] feeds: the timeline below,
//     with STOPPING=1 at its start, then exit 0;
//   - an accepted handover, at the linger's end, through
//     [Drainer.Handover]: no accept window, the deadline 20 s after its
//     start, no notification at all, since the unit keeps serving through
//     the successor, then exit 0;
//   - a part of the Node that stopped after boot, through [Drainer.Fail]:
//     the stop-signal timeline without STOPPING=1, then exit 1, so the
//     process manager restarts the Node.
//
// A second stop signal jumps to the deadline and later ones do nothing; a
// Drain not started by a stop signal counts the first one as its first.
// While a handover steers, from accepted until the linger's end
// ([Drainer.Hold]), stop signals and failures wait: [Drainer.Handover]
// then starts the handover's Drain, a waiting signal counting as its
// first, and [Drainer.Resume], when the successor is lost during the
// linger and the holder keeps serving, starts the Drain they asked for. A
// holder whose Hold fails, because a Drain started, refuses the handover
// with draining. SIGHUP never starts a Drain, and SIGPIPE is ignored, so a
// closed stdout drops log lines instead of ending the process.
//
// # Timeline
//
// Offsets from a stop signal (target values):
//
//	0 s         readiness fails (/readyz 503 draining); STOPPING=1 and
//	            STATUS=draining; the source stops, so nothing more
//	            activates and a pending Revision is dropped; the
//	            data-directory lock and holder.json are released
//	0 to 5 s    accept window, while balancers observe /readyz
//	5 s         stop accepting: each client listener's dispatching
//	            listener closes
//	5 to 15 s   K = 8 waves: shard i of every client listener shuts down
//	            at one uniformly random time within the i-th eighth of the
//	            10 s jitter window, so its connections get GOAWAY, its idle
//	            HTTP/1.1 ones close and its busy ones get Connection: close
//	25 s        Drain deadline: every shard left shuts down, the remaining
//	            work ends with RZ-RT-016 through the ending protocol
//	            (retire.Holder.EndAll), the retirer closes and the
//	            listeners close every connection left; the deadline's
//	            record counts the connections left and the requests ended
//	25 to 30 s  the shutdown steps in order: the admin server, the
//	            Node-wide Components, the post-commit queue and the
//	            telemetry flush; then Run returns the exit code
//
// A handover's Drain starts at the linger's end, where the listeners
// already stopped accepting: its waves run from its start and its
// deadline follows 20 s later. A stop-signal or failure Drain of a Node
// that never answered /readyz with 200 and has no client listener bound
// goes straight to the deadline and exits after the flush. Everything
// after the deadline shares one bound, the deadline plus 5 s; after a jump
// the deadline is the jump. Each Drain sends its start notification
// within the planned exit bound, so a service manager that stops reading
// never stalls the Drain.
//
// Heartbeats with draining belong to the Control Stream, Planned (M2).
// The going-away signals to long-lived connections, WebSocket close 1001
// and the SSE end, for which the Drain tracks hijacked connections itself
// because Server.Shutdown ignores them, are Planned (M3).
//
// # Parts
//
// The Drainer drives the Node's parts through small interfaces:
// readiness.Tracker ([Readiness]), sdnotify.Notifier ([Notifier]),
// nodedir.Lock ([Lock]), listener.Set ([Listeners]) and retire.Holder
// ([Retirer]); the source's stop and the shutdown [Step] values are
// functions, so the handover can wrap the lock to send released once it is
// released. Time comes from an injected clock.Clock and the jitter from an
// injected rand.Source, so the timeline runs in controlled time in tests
// and in the controllable-clock build. A panic in any part is recovered,
// logged and makes the exit code 1; the Drain goes on.
package drain
