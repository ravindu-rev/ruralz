// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package node implements the ruralz node commands of
// docs/reference/01-cli-and-api-surface.md, "Rollout, promotion and Node
// verbs": ruralz node drain, which signals the local ruralzd that holds the
// data-directory lock and awaits its exit, and ruralz node dump, which
// saves a Node's /config/dump after checking it. The M2 verbs list, token
// and revoke are table rows marked Planned.
//
// # Registration
//
// The package holds no state. Specs returns the rows of the node noun,
// DrainSpec and DumpSpec one command each; the root's command table
// (cli.Registry) appends them and command.Execute dispatches them. Options
// carries the dependencies: the clock of the drain's poll and of the admin
// client's deadlines, the admin client's randomness and the CLI's build
// version; the zero value is the real machine.
//
// # ruralz node drain
//
// The command runs on Linux only (docs/reference/01-cli-and-api-surface.md
// "Platform support"): macOS lacks /proc/locks, and Windows has neither
// ruralzd nor SIGTERM. It reads holder.json in --data-dir (default
// RURALZ_DATA_DIR, else /var/lib/ruralz) and checks its ruralz.holder.v1
// format (nodedir.ReadHolderAt), opens a pidfd for the recorded PID (Linux
// 5.4 or newer; on an older kernel it keeps the PID), then checks, in
// order, that the holder's pidNamespace is the CLI's own, that a live
// process has its PID and startTime, and that the process holds a granted
// FLOCK WRITE lock on the inode of the data directory's lock file in
// /proc/locks. It then sends SIGTERM through the pidfd, so a process that
// took the PID after the checks is never signaled, and polls every 100 ms
// (target) until the process is gone, up to --timeout (default 5 min,
// target), counted from the signal. A process is gone once no process has
// its PID and start time, or once it has exited and awaits its parent's
// wait (zombie), since it then serves nothing and holds nothing.
//
// No holder, a stale record or another PID namespace exits 2 before any
// signal, and so does a denied signal; when --timeout passes first the
// command exits 2 naming the PID that still runs and sends no second
// signal, leaving the Node to its own Drain deadline. On success it prints
// "drained Node <nodeId> (pid <pid>) in <seconds> s", seconds with one
// decimal, to standard error and nothing to standard output. With --output
// json, once it has signaled, it prints one object to standard output
// instead: {"nodeId", "pid", "seconds", "result"}, seconds counted from
// the signal with millisecond precision and result drained, or timeout
// beside exit 2; a refusal before the signal prints no object. Ctrl-C ends
// the wait with exit 130 and no object, and the Node goes on draining.
//
// # ruralz node dump
//
// The command reads /config/dump of the Node --admin names through the
// admin client (internal/cli/adminclient), whose flags, deadlines, bounds
// and errors it takes, --timeout replacing the 5 min call deadline. Before
// it writes anything it checks the document with canonical.Decoder.ReadDump
// over the schema this CLI serves: the content must hash to its digest
// (RZ-CFG-027) and decode at the schema levels the CLI serves (RZ-CFG-024).
// A failed check, a body that is no dump document, or 503 RZ-RT-024 from a
// Node with no active Revision exits 2 and writes nothing. A dump that
// passes is written exactly as received, so the file is a ruralz bundle
// diff source: to standard output, which then holds only the document, or
// atomically to --output-file, after which one line on standard error names
// its Revision, full digest and Last-Known-Good digest: "saved rev-<12 hex>
// sha256:<64 hex>, Last-Known-Good sha256:<64 hex>", or ", no
// Last-Known-Good" for a Node that has none. The command takes no
// --output: its output is the document.
package node

import (
	"io"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Options are the dependencies of the node commands. The zero value is the
// real machine.
type Options struct {
	// Clock runs the drain's poll and --timeout and the admin client's
	// deadlines; nil is clock.Real().
	Clock clock.Clock
	// Rand supplies the admin client's traceparent IDs; nil is
	// crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version, which the admin client sends in
	// User-Agent and compares with a Node's ruralz-version; empty is
	// buildinfo.Get().Version.
	Version string

	// host replaces the machine ruralz node drain inspects and signals;
	// nil is the host's own (newHost). Tests set it.
	host host
}

// timer returns the configured clock, or the real one.
func (o Options) timer() clock.Clock {
	if o.Clock == nil {
		return clock.Real()
	}
	return o.Clock
}

// machine returns the configured host, or the real one.
func (o Options) machine() host {
	if o.host == nil {
		return newHost()
	}
	return o.host
}

// Specs returns the rows of the node noun for the root's command table:
// the noun description, ruralz node drain and ruralz node dump, and the M2
// verbs list, token and revoke as Planned rows, which exit 2 naming their
// milestone and stay out of help and completion.
func Specs(o Options) []command.Spec {
	return []command.Spec{
		{Path: []string{"node"}, Summary: "Operate Nodes"},
		DrainSpec(o),
		DumpSpec(o),
		{Path: []string{"node", "list"}, Summary: "Nodes with active and Last-Known-Good digests", Planned: "M2"},
		{Path: []string{"node", "token"}, Summary: "One-time Enrollment token", Planned: "M2"},
		{Path: []string{"node", "revoke"}, Summary: "Revoke an enrollment identity", Planned: "M2"},
	}
}
