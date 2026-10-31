// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package launch is the local ruralzd launcher of the ruralz CLI
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI"): it finds the ruralzd binary, renders a Bundle into a private test
// render, runs ruralzd on it, awaits its readiness and each activation, and
// stops it. ruralz dev run drives it today; ruralz test run --bundle, Planned
// (M2), takes the same parts.
//
// # Binary
//
// FindRuralzd returns --ruralzd PATH when given, else the ruralzd beside the
// running ruralz, else the first ruralzd on PATH; none exits 2 naming both
// places. Once ruralzd runs, a holder.json in its data directory whose
// version differs from the CLI's own exits 2.
//
// # Workspace
//
// NewWorkspace creates the private directory of one launch, mode 0700, with
// symbolic links resolved so every path the child sees lies under it as
// written: the test render RURALZ_CONFIG names (bundle.yaml, replaced by
// atomic rename), RURALZ_DATA_DIR (data/), RURALZ_SECRET_ROOT when secret
// overrides exist (secrets/), and the file RURALZ_ADMIN_TOKEN_FILE names
// (admin-token), which holds a generated operator token. The secret root is
// a sibling of the data directory and the token file, so it contains
// neither, as ruralzd requires. Remove deletes the directory once the child
// has exited.
//
// # Rendering
//
// A Renderer renders the source for --env, or from the process
// environment, through the configuration pipeline facade, and rewrites the
// rendered Bundle into the test render: with ephemeral ports every listener
// port and admin.port becomes a free loopback port, kept across re-renders,
// a listener added later getting a new one; with secret overrides every
// secretRef an override names becomes a provider file reference to a copy,
// mode 0600, under the private RURALZ_SECRET_ROOT, keeping its key, and
// every file reference must have an override; with the memory State Store
// the Gateway's stateStore becomes driver memory. A rewritten file is a test
// render whose digest, the test digest, is reported beside the unmodified
// Revision digest; with nothing rewritten the two are equal. A render that
// fails returns its diagnostics.
//
// # Child
//
// Start runs ruralzd with no arguments in its own process group, its
// standard output and standard error copied to the CLI's standard error:
// each JSON log line compacted to "<time> <level> <message> key=value..."
// without the Node, Revision, trace and span IDs when that stream is a
// terminal and JSON output is off, every byte passed unchanged otherwise.
// The child inherits the CLI's environment with RURALZ_CONFIG,
// RURALZ_DATA_DIR, RURALZ_ADMIN_TOKEN_FILE and RURALZ_BIND_LOOPBACK=true set,
// RURALZ_SECRET_ROOT set when overrides exist, RURALZ_FETCH_ALLOW set to
// 127.0.0.0/8,::1 when the CLI's environment leaves it unset or empty, and
// RURALZ_ADMIN_TLS_DIR and RURALZ_ADMIN_METRICS_TOKEN_FILE removed, so its
// admin port serves plain HTTP on loopback, where the generated token works.
//
// AwaitReady polls the admin port's /readyz every 0.5 s (target) until it
// answers 200, within the ready timeout counted from the launch; when the
// timeout passes first it returns the last answer's reasons, and a child
// that exits first ends the wait at once. AwaitActive polls /readyz after a
// swap until its revision names the new test render, within the ready
// timeout; when it passes first the child still serves its earlier
// Revision, the cause being in its log.
//
// Terminate sends the child SIGTERM, which starts a Drain; a second call
// sends a second SIGTERM, which jumps the Drain to its deadline; the child
// gets SIGKILL once it outlives the 30 s exit bound (target) counted from
// the first SIGTERM.
package launch

import (
	"io"
	"os"
	"time"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Targets of docs/reference/01-cli-and-api-surface.md "Local ruralzd
// launched by the CLI".
const (
	// PollInterval is how often ruralz dev run polls its sources.
	PollInterval = 500 * time.Millisecond
	// SettleTime is how long a change must stay unchanged before a
	// re-render.
	SettleTime = 250 * time.Millisecond
	// ReadyInterval is how often /readyz is polled.
	ReadyInterval = 500 * time.Millisecond
	// DefaultReadyTimeout bounds readiness from the launch, and each
	// activation from its swap, unless --ready-timeout replaces it.
	DefaultReadyTimeout = 30 * time.Second
	// ExitBound is how long the child may run after the first SIGTERM
	// before it gets SIGKILL.
	ExitBound = 30 * time.Second
)

// Names in the private directory.
const (
	// ConfigFile is the test render RURALZ_CONFIG names.
	ConfigFile = "bundle.yaml"
	// DataDir is RURALZ_DATA_DIR.
	DataDir = "data"
	// SecretDir is RURALZ_SECRET_ROOT when secret overrides exist.
	SecretDir = "secrets"
	// TokenFile is the file RURALZ_ADMIN_TOKEN_FILE names.
	TokenFile = "admin-token"
)

// Options are the launcher's dependencies. The zero value is the real
// machine.
type Options struct {
	// Clock runs every poll, deadline and the exit bound; nil is
	// clock.Real().
	Clock clock.Clock
	// Rand supplies the generated operator token and the admin client's
	// traceparent IDs; nil is crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version, which holder.json must carry;
	// empty is buildinfo.Get().Version.
	Version string
	// Executable returns the path of the running ruralz, beside which
	// FindRuralzd looks first; nil is os.Executable.
	Executable func() (string, error)
	// ExtraFiles are inherited by the child as file descriptors 3 and up:
	// the clock command socket of the controllable-clock ruralzd test build
	// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
	// scope"). Start hands them over: it closes them once the child holds
	// its own copies, so the child's exit ends their peers' streams. The
	// CLI passes none.
	ExtraFiles []*os.File
}

// timer returns the configured clock, or the real one.
func (o Options) timer() clock.Clock {
	if o.Clock == nil {
		return clock.Real()
	}
	return o.Clock
}

// version returns the configured build version, or the binary's.
func (o Options) version() string {
	if o.Version == "" {
		return buildinfo.Get().Version
	}
	return o.Version
}

// executable returns the path of the running ruralz.
func (o Options) executable() (string, error) {
	if o.Executable == nil {
		return os.Executable()
	}
	return o.Executable()
}
