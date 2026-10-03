// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package resolver implements secret.Resolver for ruralzd: the env and file
// secretRef providers, the RURALZ_SECRET_ROOT and RURALZ_SECRET_ rules,
// per-kind size caps and checks, the Node-wide watch table and the 2 s file
// poller (spec 01 requirements 44 to 46, spec 06 requirements 87 to 91,
// spec 04 requirement 3; architecture 2.8, R-9, R-55). Only ruralzd links
// it (depguard secretresolver).
//
// Values live only in memory, in cells: one cell holds the current value
// of one Ref. A Store resolved for a Revision points at its cells; a
// reference the active Store already holds keeps its cell (no re-read
// across Hot Reloads), so a rotation polled through the active Store also
// reaches every older Store sharing the cell, including one kept by a
// carried-over Filter or a retired snapshot. A file reference whose last
// rotation the active Store refused keeps its cell too: Resolve reads the
// file again and, when the new uses accept its value, holds it as a
// pending value of the new Store, which Activate applies to the shared
// cell. Watch registrations are Node-wide by Ref and survive Activate:
// every poll cycle delivers the active cell's value to each registration
// that has not seen its version, whichever Store the registration was
// made on.
//
// Nothing this package returns, logs or records contains a resolved value:
// diagnostics, errors and log records carry the reference (provider, name,
// key) and fixed reasons only; reasons produced by consumer checks and
// watchers are withheld when they repeat part of the value, and a
// consumer check or watcher that panics is reported by a fixed reason.
// Clearing is best effort: the package clears the buffers it allocates for
// file content and values once done with them, but copies made by the
// standard library (the JSON decoder, URL and certificate parsing, the encoded
// forms the leak check builds) and Go strings are left to the garbage
// collector.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Defaults and limits (architecture R-9; spec 01 requirements 44 and 46).
const (
	// DefaultRoot is RURALZ_SECRET_ROOT when the setting is unset.
	DefaultRoot = "/etc/ruralz"
	// DefaultPollInterval is how often file references are polled.
	DefaultPollInterval = 2 * time.Second
	// MaxValueBytes caps a file (and a value) of every kind but KindPEMCRL.
	MaxValueBytes = 4 << 20
	// MaxCRLBytes caps a file (and a value) of KindPEMCRL.
	MaxCRLBytes = 16 << 20
	// MinAPIKeyBytes is the shortest KindAPIKey value after trimming
	// (spec 06 requirement 13; RZ-CFG-026 interim, 01 risk 37).
	MinAPIKeyBytes = 22
	// DefaultRetryInitial is the first delay of ResolveWithRetry (proposed).
	DefaultRetryInitial = 500 * time.Millisecond
	// DefaultRetryMax caps the doubling delay of ResolveWithRetry (proposed).
	DefaultRetryMax = 10 * time.Second
)

// CodeUnresolvable is the diagnostic code of every failing use: an
// unresolvable reference, or a value that fails its check (spec 01
// requirement 44, spec 06 requirements 13, 51 and 80).
const CodeUnresolvable = "RZ-CFG-026"

// Environment variable rules of the env provider (OQ-security-and-identity-22 (a)).
const (
	// EnvStateStoreURL is the one env name outside the prefix that resolves.
	EnvStateStoreURL = "RURALZ_STATE_STORE_URL"
	// EnvSecretPrefix starts every other env name that resolves.
	EnvSecretPrefix = "RURALZ_SECRET_"
)

// Process setting names used in Config.Protected and in NewResolver errors
// (spec 04 requirement 2; OQ-security-and-identity-7 (a); R-49).
const (
	// SettingSecretRoot is the secret root setting.
	SettingSecretRoot = "RURALZ_SECRET_ROOT" //nolint:gosec // G101: a setting name, not a credential.
	// SettingDataDir is the data directory setting.
	SettingDataDir = "RURALZ_DATA_DIR"
	// SettingAdminTLSDir is the admin server TLS directory setting.
	SettingAdminTLSDir = "RURALZ_ADMIN_TLS_DIR"
	// SettingAdminTokenFile is the admin operator token file setting.
	SettingAdminTokenFile = "RURALZ_ADMIN_TOKEN_FILE" //nolint:gosec // G101: a setting name, not a credential.
	// SettingAdminMetricsTokenFile is the admin metrics token file setting.
	SettingAdminMetricsTokenFile = "RURALZ_ADMIN_METRICS_TOKEN_FILE" //nolint:gosec // G101: a setting name, not a credential.
	// SettingMACKeyFile is the State Store entry MAC key file setting.
	SettingMACKeyFile = "RURALZ_STATE_STORE_MAC_KEY_FILE"
	// SettingEnrollmentTokenFile is the Enrollment token file setting (M2).
	SettingEnrollmentTokenFile = "RURALZ_ENROLLMENT_TOKEN_FILE" //nolint:gosec // G101: a setting name, not a credential.
)

// Provider names (v1alpha1.SecretProvider values; the package imports no
// configuration types).
const (
	providerEnv        = "env"
	providerFile       = "file"
	providerKubernetes = "kubernetes"
	providerVault      = "vault"
)

// Startup refusal errors (spec 01 requirement 45, spec 04 requirement 3,
// spec 06 requirement 88). They carry no RZ code: ruralzd exits 2 with the
// message, which names the setting.
var (
	// ErrRelativeRoot reports a relative RURALZ_SECRET_ROOT.
	ErrRelativeRoot = errors.New("resolver: " + SettingSecretRoot + " is not an absolute path")
	// ErrProtectedInRoot reports a protected path that is the secret root
	// or lies inside it.
	ErrProtectedInRoot = errors.New("resolver: a protected path is " + SettingSecretRoot + " or inside it")
	// ErrRunning reports a second concurrent Run.
	ErrRunning = errors.New("resolver: Run is already running")
)

// Protected is a process setting whose path must not be the secret root
// or lie inside it.
type Protected struct {
	// Setting is the setting name, such as RURALZ_DATA_DIR.
	Setting string
	// Path is the effective path; empty when the setting is unset.
	Path string
}

// Config configures a Resolver. The zero value of every field selects the
// documented default; an empty Protected protects nothing.
type Config struct {
	// Root is RURALZ_SECRET_ROOT; empty selects DefaultRoot. It must be
	// absolute; it need not exist (file references then fail).
	Root string
	// Protected lists the paths the root must not contain: the effective
	// RURALZ_DATA_DIR, RURALZ_ADMIN_TLS_DIR, both admin token files, the
	// State Store MAC key file and (M2) the Enrollment token file.
	Protected []Protected
	// LookupEnv reads the process environment; nil selects os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Clock drives the poller and retries; nil selects clock.Real().
	Clock clock.Clock
	// PollInterval is the file poll period; 0 selects DefaultPollInterval.
	PollInterval time.Duration
	// RetryInitial and RetryMax bound the ResolveWithRetry backoff; 0
	// selects DefaultRetryInitial and DefaultRetryMax.
	RetryInitial, RetryMax time.Duration
	// Logger comes from internal/telemetry; nil discards.
	Logger *slog.Logger
	// RotationFailures returns the ruralz_config_secret_rotation_failures_total
	// counter of a provider (emit.ConfigMetrics.SecretRotationFail). It is
	// called once for env and once for file in NewResolver, so both series
	// exist from the start (spec 01 requirement 56); nil counts nothing.
	RotationFailures func(provider string) emit.Counter
	// Status raises and clears the secret_rotation_failed degraded reason;
	// nil records nothing.
	Status emit.NodeStatus
	// FileOwners lists the user IDs allowed to own a secret file; empty
	// allows any owner. Unix only. Whatever the owner, a file writable by
	// group or others is refused. The default accepts files that keep
	// their host owner, such as Docker Compose bind-mounted secrets.
	FileOwners []int
}

// Resolver resolves the secretRef uses of Revisions and polls the file
// references of the active one. It implements secret.Resolver.
type Resolver struct {
	root         string
	lookupEnv    func(string) (string, bool)
	clock        clock.Clock
	interval     time.Duration
	retryInitial time.Duration
	retryMax     time.Duration
	log          *slog.Logger
	failures     map[string]emit.Counter // by provider; immutable after NewResolver
	status       emit.NodeStatus
	owners       []int

	// mu serializes Resolve, Activate and the poll part of a cycle; it
	// guards version, raised and the poll fields of every cell.
	mu      sync.Mutex
	version uint64              // last value version handed out
	raised  map[secret.Ref]bool // references whose poll failure is raised

	active atomic.Pointer[store]
	empty  *store

	watchMu   sync.Mutex
	watches   map[secret.Ref][]*watcher // copy-on-write slices
	watcherID uint64                    // guarded by watchMu

	kick    chan struct{}
	running atomic.Bool

	// cycleHook, when set before Run starts, is called after every poll
	// cycle (tests).
	cycleHook func()
}

var _ secret.Resolver = (*Resolver)(nil)

// NewResolver validates cfg and returns a Resolver. It refuses a relative
// root and a root that is, or contains, a protected path (compared after
// filepath.Clean and filepath.EvalSymlinks where the path exists); the
// error names the settings and ruralzd exits 2 with it (spec 01
// requirement 45, spec 04 requirement 3, spec 06 requirement 88).
func NewResolver(cfg Config) (*Resolver, error) {
	root := cfg.Root
	if root == "" {
		root = DefaultRoot
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: %q", ErrRelativeRoot, root)
	}
	root = filepath.Clean(root)
	if err := checkProtected(root, cfg.Protected); err != nil {
		return nil, err
	}
	r := &Resolver{
		root:         root,
		lookupEnv:    cfg.LookupEnv,
		clock:        cfg.Clock,
		interval:     cfg.PollInterval,
		retryInitial: cfg.RetryInitial,
		retryMax:     cfg.RetryMax,
		log:          cfg.Logger,
		status:       cfg.Status,
		owners:       cfg.FileOwners,
		failures:     map[string]emit.Counter{},
		raised:       map[secret.Ref]bool{},
		watches:      map[secret.Ref][]*watcher{},
		kick:         make(chan struct{}, 1),
	}
	if r.lookupEnv == nil {
		r.lookupEnv = os.LookupEnv
	}
	if r.clock == nil {
		r.clock = clock.Real()
	}
	if r.interval <= 0 {
		r.interval = DefaultPollInterval
	}
	if r.retryInitial <= 0 {
		r.retryInitial = DefaultRetryInitial
	}
	if r.retryMax <= 0 {
		r.retryMax = DefaultRetryMax
	}
	r.retryMax = max(r.retryMax, r.retryInitial)
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	for _, p := range []string{providerEnv, providerFile} {
		var c emit.Counter
		if cfg.RotationFailures != nil {
			c = cfg.RotationFailures(p)
		}
		if c == nil {
			c = nopCounter{}
		}
		r.failures[p] = c
	}
	r.empty = &store{res: r}
	r.active.Store(r.empty)
	return r, nil
}

// Root returns the cleaned secret root.
func (r *Resolver) Root() string { return r.root }

// Current returns the active Store; before the first Activate it is an
// empty Store that holds no reference.
func (r *Resolver) Current() secret.Store { return r.active.Load() }

// Activate makes s current: its file references become the polled set.
// Watch registrations are kept (R-55); on the next cycle, which Activate
// starts at once when Run is running, every registration whose reference
// s holds receives the value it has not seen. Before s becomes current,
// Activate applies the refused rotations s's uses accept (its pending
// values) to the cells s shares with older Stores, so those Stores and
// their watchers follow, and checks against s's uses every value that
// changed since Resolve checked it; a value they refuse counts a rotation
// failure and raises secret_rotation_failed. The next cycle also reads
// again every file reference of s whose last examination failed and
// checks it against s's uses, so a value the previous uses refused is
// applied once s accepts it and secret_rotation_failed clears (spec 01
// requirement 46, spec 06 requirements 13 and 76). A nil s activates the
// empty Store. s must come from r's Resolve; any other Store is a
// programming error and panics.
func (r *Resolver) Activate(s secret.Store) {
	st := r.empty
	if s != nil {
		var ok bool
		st, ok = s.(*store)
		if !ok || st == nil || st.res != r {
			panic("resolver: Activate of a Store this Resolver did not resolve")
		}
	}
	ctx := context.Background() // Activate has no caller context; used for logging only
	r.mu.Lock()
	r.settleLocked(ctx, st)
	r.active.Store(st)
	r.recheckLocked(ctx, st, r.clock.Now())
	for _, g := range st.files {
		for _, ref := range g.refs {
			if c := st.cells[ref]; c.failed {
				c.racy = true // examine again against st's uses
			}
		}
	}
	r.syncDegradedLocked(st)
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// settleLocked applies the pending values of st to the cells st shares
// with older Stores and drops them, so Get on st reads its cells from
// then on. A pending value is applied when its cell still holds the value
// Resolve saw and its last examination still failed: every Store sharing
// the cell, and every watcher, then follows it (secret.Store, R-55), and
// the next poll re-reads the file if it changed since Resolve read it.
// Otherwise the cell moved since Resolve; when its value is older than the
// pending one, which st's Get and watchers may already have taken, it is
// published again at a new version so they converge on it, and
// recheckLocked checks it against st's uses.
func (r *Resolver) settleLocked(ctx context.Context, st *store) {
	pm := st.pending.Load()
	if pm == nil {
		return
	}
	for _, ref := range st.refs {
		p, ok := (*pm)[ref]
		if !ok {
			continue
		}
		c := st.cells[ref]
		cur := c.load()
		switch {
		case cur.version == p.base && c.failed:
			c.cur.Store(p.cv)
			c.examined(p.fp, p.at)
			r.log.InfoContext(ctx, msgRotated,
				slog.String(catalog.KeyProvider, string(ref.Provider)),
				slog.String(catalog.KeyReference, ref.String()))
		case cur.version < p.cv.version:
			b := cur.val.Reveal()
			c.set(b, r.nextVersionLocked())
			clear(b)
		}
	}
	st.pending.Store(nil)
}

// recheckLocked checks against st's uses every file value of st that
// changed since st last checked it: a poll through the previously active
// Store between Resolve and Activate checked it only against that Store's
// uses (spec 01 requirement 46). A refused value stays served, as the
// only value the reference has, but counts one rotation failure, is logged
// and raises secret_rotation_failed until a value st's uses accept is
// read. A cell whose last examination failed is left to the next poll.
func (r *Resolver) recheckLocked(ctx context.Context, st *store, now time.Time) {
	for _, g := range st.files {
		for _, ref := range g.refs {
			c := st.cells[ref]
			cv := c.load()
			if st.checked[ref] == cv.version {
				continue
			}
			st.checked[ref] = cv.version
			if c.failed {
				continue
			}
			b := cv.val.Reveal()
			err := st.check(ref, b)
			clear(b)
			if err != nil {
				r.failLocked(ctx, c, err, c.fp, now)
			}
		}
	}
}

// Run polls the active Store's file references every poll interval, and
// at once after each Activate, and delivers changed values to watchers on
// its own goroutine. It returns nil when ctx is done; its owner waits for
// it. Only one Run may be active at a time (ErrRunning).
func (r *Resolver) Run(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer r.running.Store(false)
	t := r.clock.NewTimer(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
			r.cycle(ctx)
			t.Reset(r.interval)
		case <-r.kick:
			r.cycle(ctx)
		}
	}
}

// cycle polls the active Store and fans out values watchers have not seen.
func (r *Resolver) cycle(ctx context.Context) {
	st := r.poll(ctx)
	r.fanout(ctx, st)
	if r.cycleHook != nil {
		r.cycleHook()
	}
}

// nextVersionLocked returns a new value version, unique Node-wide.
func (r *Resolver) nextVersionLocked() uint64 {
	r.version++
	return r.version
}

// failureCounter returns the rotation failure counter of provider.
func (r *Resolver) failureCounter(provider string) emit.Counter {
	if c, ok := r.failures[provider]; ok {
		return c
	}
	return nopCounter{}
}

// nopCounter counts nothing.
type nopCounter struct{}

func (nopCounter) Add(emit.Stripe, uint64) {}
