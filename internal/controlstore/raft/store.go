// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package raft is the default Control Store driver (ADR-0006): embedded
// hashicorp/raft with the raft-boltdb/v2 log and stable store on bbolt, a
// file snapshot store keeping two snapshots, the controlstore.Machine as
// the state machine, a digest-addressed content store, and the 8092 peer
// layer over mutual TLS. It is the only package that imports
// hashicorp/raft (depguard), and ruralzd never links it (depgate).
//
// # Data directory
//
//	raft/raft.db     bbolt log and stable store, with the persisted commit index
//	snapshots/       state-machine snapshots, never content objects
//	content/sha256/  content objects by digest
//
// # Time
//
// hashicorp/raft runs its heartbeats, elections and leader contact on the
// real clock, so freshness, peer contact and the 8092 byte budget read the
// real clock too. Every other time the store keeps (entry times, the
// finalize soak, the audit retention, the status-report cadence, the
// leadership transfer bound) runs on Config.Clock, which tests replace.
package raft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// ReportInterval is how often every replica reports its status to the
// leader over the status-forwarding class: the 5 s (target) cadence of
// status forwarding (docs/architecture/04-control-plane-and-gitops.md
// "Replica roles"). A replica also reports at once when the leader
// changes and after each snapshot.
const ReportInterval = 5 * time.Second

// snapshotsKept is how many state-machine snapshots a replica keeps: two
// (docs/operations/03-capacity-planning.md "Ruralz Control sizing").
const snapshotsKept = 2

// Config configures one replica.
type Config struct {
	// DataDir holds the Control Store (RURALZ_DATA_DIR).
	DataDir string
	// ServerID is this replica's server ID, which its peer certificate
	// names.
	ServerID controlstore.ServerID
	// Address is the peer address other replicas dial, HOST:PORT on 8092.
	Address string
	// Listener is the bound 8092 listener; the store closes it.
	Listener net.Listener
	// TLS supplies the peer certificate and peer CA pool.
	TLS TLSSource
	// Bootstrap starts a single-voter cluster on an empty Control Store:
	// the first ruralz control serve. A directory join or restore wrote
	// never bootstraps.
	Bootstrap bool
	// Peers are addresses to report to while no leader is known, such as
	// the peer ruralz control join dialed.
	Peers []string
	// BinaryVersion is this binary's product version.
	BinaryVersion string
	// Clock runs the store's own times; nil is clock.Real().
	Clock clock.Clock
	// Logger receives the store's and raft's records; nil discards.
	Logger *slog.Logger
	// AuditRetention prunes sealed audit segments older than it once a
	// backup holds them; zero is DefaultAuditRetention.
	AuditRetention time.Duration
	// TimedSoak, when positive, finalizes an upgrade without an operator
	// once the finalize conditions have held without a break for it,
	// committing the entry TimedFinalizeAudit returns with the version
	// advance.
	TimedSoak          time.Duration
	TimedFinalizeAudit func(controlstore.FinalizeStatus) []byte
	// HeartbeatTimeout, ElectionTimeout, LeaderLeaseTimeout and
	// CommitTimeout override hashicorp/raft's defaults (1000 ms, 1000 ms,
	// 500 ms and 50 ms) when positive.
	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
	CommitTimeout      time.Duration
	// ForwardHandler and JoinHandler serve the write-forwarding and Join
	// classes; a connection of a class without one closes.
	ForwardHandler Handler
	JoinHandler    Handler
	// StatusHandler serves, on the leader, the status-forwarding requests
	// of kinds other than the store's own replica reports, which
	// followers send with SendStatus; nil refuses them.
	StatusHandler func(ctx context.Context, from controlstore.PeerIdentity, kind string, payload json.RawMessage) (json.RawMessage, error)
	// OnHalt is told once when the replica halts, so the process exports
	// it and stops before serving.
	OnHalt func(*controlstore.HaltError)
}

// Store is one replica's Control Store on Raft.
type Store struct {
	cfg     Config
	id      controlstore.ServerID
	binary  controlstore.Version
	clk     clock.Clock
	rclk    clock.Clock
	log     *slog.Logger
	machine *controlstore.Machine
	content *contentStore
	logs    *commitLog
	snaps   *hraft.FileSnapshotStore
	trans   *transport
	nt      *hraft.NetworkTransport
	r       *hraft.Raft
	ready   chan struct{}
	started time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// futures counts the goroutines waiting on raft futures for callers.
	futures sync.WaitGroup

	fetchCh  chan struct{}
	reportCh chan struct{}
	truncCh  chan struct{}
	haltCh   chan struct{}

	mu          sync.Mutex
	lead        *leadState
	leadGen     uint64
	leadChanged chan struct{}
	leaderUntil time.Time
	halted      *controlstore.HaltError
	closed      bool
	futuresDone bool
	live        map[controlstore.ServerID]liveReport

	// beforeBarrier, set only by tests, runs when this replica becomes
	// raft leader, before its Barrier.
	beforeBarrier atomic.Pointer[func()]
	// afterShutdownTransfer, set only by tests, gets the result of the
	// leadership transfer of a stopping leader.
	afterShutdownTransfer atomic.Pointer[func(error)]
	// beforeTransfer, set only by tests, runs before each leadership
	// transfer attempt; an error it returns fails the attempt.
	beforeTransfer atomic.Pointer[func() error]
}

// leadState is the published leadership of one term.
type leadState struct {
	term    uint64
	barrier uint64
	lost    chan struct{}
}

// liveReport is the leader's newest status report from one replica.
type liveReport struct {
	status replicaStatus
	at     time.Time
}

// Compile-time check of the contract.
var _ controlstore.Store = (*Store)(nil)

// validate checks the configuration.
func (c *Config) validate() (controlstore.Version, error) {
	switch {
	case c.DataDir == "":
		return controlstore.Version{}, fmt.Errorf("%w: a data directory", controlstore.ErrInvalid)
	case c.Address == "":
		return controlstore.Version{}, fmt.Errorf("%w: a peer address", controlstore.ErrInvalid)
	case c.Listener == nil:
		return controlstore.Version{}, fmt.Errorf("%w: an 8092 listener", controlstore.ErrInvalid)
	case c.TLS == nil:
		return controlstore.Version{}, fmt.Errorf("%w: peer TLS credentials", controlstore.ErrInvalid)
	case (c.TimedSoak > 0) != (c.TimedFinalizeAudit != nil):
		return controlstore.Version{}, fmt.Errorf("%w: a timed soak needs its audit entry", controlstore.ErrInvalid)
	}
	if err := c.ServerID.Validate(); err != nil {
		return controlstore.Version{}, err
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return controlstore.Version{}, fmt.Errorf("%w: peer address %q: %w", controlstore.ErrInvalid, c.Address, err)
	}
	return controlstore.ParseVersion(c.BinaryVersion)
}

// Open opens the Control Store in cfg.DataDir and starts the replica: it
// restores the newest snapshot, applies the committed entries after it
// (Ready closes then) and joins the Raft cluster. A state this binary
// must not serve is a *controlstore.HaltError.
func Open(cfg Config) (*Store, error) {
	binary, err := cfg.validate()
	if err != nil {
		_ = cfg.Listener.Close()
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.AuditRetention <= 0 {
		cfg.AuditRetention = controlstore.DefaultAuditRetention
	}
	s := &Store{
		cfg: cfg, id: cfg.ServerID, binary: binary, clk: cfg.Clock, rclk: clock.Real(),
		log:     cfg.Logger.With(slog.String(catalog.KeyComponent, "controlstore")),
		machine: controlstore.NewMachine(binary), ready: make(chan struct{}),
		fetchCh: make(chan struct{}, 1), reportCh: make(chan struct{}, 1), truncCh: make(chan struct{}, 1), haltCh: make(chan struct{}, 1),
		leadChanged: make(chan struct{}), live: map[controlstore.ServerID]liveReport{},
	}
	s.started = s.rclk.Now()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.open(); err != nil {
		s.cancel()
		s.closeStores()
		_ = cfg.Listener.Close()
		return nil, err
	}
	s.wg.Go(s.haltLoop)
	s.wg.Go(s.leadershipLoop)
	s.wg.Go(s.fetchLoop)
	s.wg.Go(s.reportLoop)
	s.wg.Go(s.truncateLoop)
	if s.haltErr() == nil {
		close(s.ready)
	}
	return s, nil
}

// open builds the stores, the transport and raft.
func (s *Store) open() error {
	dir := s.cfg.DataDir
	for _, d := range []string{dir, filepath.Join(dir, "raft"), filepath.Join(dir, "snapshots")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("controlstore: data directory: %w", err)
		}
	}
	rlog := newRaftLogger(s.cfg.Logger)
	var err error
	if s.content, err = openContent(filepath.Join(dir, "content"), s.referenced); err != nil {
		return err
	}
	if s.logs, err = openLog(filepath.Join(dir, "raft", "raft.db")); err != nil {
		return err
	}
	if s.snaps, err = hraft.NewFileSnapshotStoreWithLogger(filepath.Join(dir, "snapshots"), snapshotsKept, rlog); err != nil {
		return fmt.Errorf("controlstore: snapshot store: %w", err)
	}
	rc := s.raftConfig(rlog)
	if err := s.loadLocal(); err != nil {
		return s.errOpen(err)
	}
	s.trans = newTransport(s.cfg.Listener, s.cfg.Address, s.cfg.TLS, s.clk, s.rclk, s.log, s.authorizePeer, map[controlstore.Class]Handler{
		controlstore.ClassContent: s.serveContent,
		controlstore.ClassStatus:  s.serveStatus,
		controlstore.ClassForward: s.cfg.ForwardHandler,
		controlstore.ClassJoin:    s.cfg.JoinHandler,
	})
	s.nt = hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{Stream: newStreamLayer(s.trans), MaxPool: 3, Timeout: ioTimeout, Logger: rlog})
	existing, err := hraft.HasExistingState(s.logs, s.logs, s.snaps)
	if err != nil {
		return fmt.Errorf("controlstore: read raft state: %w", err)
	}
	f := &fsm{s: s}
	if !existing && s.cfg.Bootstrap {
		cfgs := hraft.Configuration{Servers: []hraft.Server{{Suffrage: hraft.Voter, ID: hraft.ServerID(s.id), Address: hraft.ServerAddress(s.cfg.Address)}}}
		if err := hraft.BootstrapCluster(rc, s.logs, s.logs, s.snaps, s.nt, cfgs); err != nil {
			return fmt.Errorf("controlstore: bootstrap: %w", err)
		}
	}
	r, err := hraft.NewRaft(rc, f, s.logs, s.logs, s.snaps, s.nt)
	if err != nil {
		return s.errOpen(fmt.Errorf("controlstore: start raft: %w", err))
	}
	s.r = r
	if err := s.machine.CheckRun(s.newestSnapshot()); err != nil {
		var h *controlstore.HaltError
		if errors.As(err, &h) {
			s.haltWith(h)
		}
		return err
	}
	// Only now may peers reach the handlers, which use raft.
	s.trans.start()
	return nil
}

// loadLocal loads the Control Store before raft starts: the newest
// readable snapshot, then the committed entries after it up to the
// persisted commit index, so a replica serves its committed state, and
// is ready, before any leader contact, quorum or not. raft then starts
// without restoring the snapshot itself and scans every configuration
// entry after it; the entries it delivers again are skipped by index. An
// entry, snapshot or state this binary must not run halts.
func (s *Store) loadLocal() error {
	list, err := s.snaps.List()
	if err != nil {
		return fmt.Errorf("controlstore: list snapshots: %w", err)
	}
	for i, meta := range list {
		snap, err := s.readLocalSnapshot(meta.ID)
		if err == nil {
			s.machine.Restore(snap)
			break
		}
		var h *controlstore.HaltError
		if errors.As(err, &h) {
			s.haltWith(h)
			return h
		}
		s.log.Error("local snapshot unreadable", slog.String(catalog.KeyError, err.Error()))
		if i == len(list)-1 {
			return fmt.Errorf("controlstore: no local snapshot is readable: %w", err)
		}
	}
	upto, err := s.logs.committed()
	if err != nil {
		return fmt.Errorf("controlstore: read the commit index: %w", err)
	}
	for idx := s.machine.Index() + 1; idx <= upto; idx++ {
		var l hraft.Log
		if err := s.logs.GetLog(idx, &l); err != nil {
			return fmt.Errorf("controlstore: read committed entry %d: %w", idx, err)
		}
		if err := s.applyLocal(&l); err != nil {
			return err
		}
	}
	s.logs.noteApplied(s.machine.Index())
	return nil
}

// readLocalSnapshot decodes one local snapshot.
func (s *Store) readLocalSnapshot(id string) (*controlstore.Snapshot, error) {
	_, rc, err := s.snaps.Open(id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return controlstore.ReadSnapshot(rc, s.binary)
}

// applyLocal applies one committed entry of the startup replay.
func (s *Store) applyLocal(l *hraft.Log) error {
	switch l.Type {
	case hraft.LogCommand:
		res, err := s.machine.Apply(l.Index, l.Data)
		if err != nil {
			var h *controlstore.HaltError
			if errors.As(err, &h) {
				s.haltWith(h)
			}
			return err
		}
		s.content.release(res.Released)
	case hraft.LogConfiguration:
		s.machine.SetConfiguration(l.Index, members(hraft.DecodeConfiguration(l.Data)))
	default:
		// No-op and barrier entries never reach the state machine.
	}
	return nil
}

// members converts a raft configuration.
func members(c hraft.Configuration) []controlstore.Member {
	out := make([]controlstore.Member, 0, len(c.Servers))
	for _, srv := range c.Servers {
		out = append(out, controlstore.Member{ServerID: controlstore.ServerID(srv.ID), Address: string(srv.Address), Voter: srv.Suffrage == hraft.Voter})
	}
	return out
}

// errOpen is the error a failed Open returns: the halt, when one stopped
// it.
func (s *Store) errOpen(err error) error {
	if h := s.haltErr(); h != nil {
		return h
	}
	return err
}

// raftConfig returns the hashicorp/raft configuration: its defaults, the
// timing overrides and no legacy telemetry. The store restores the
// snapshot and replays the committed entries itself (loadLocal), so raft
// restores no snapshot at start and runs no replay of its own.
func (s *Store) raftConfig(rlog hclog.Logger) *hraft.Config {
	rc := hraft.DefaultConfig()
	rc.LocalID = hraft.ServerID(s.id)
	rc.Logger = rlog
	rc.NoLegacyTelemetry = true
	rc.NoSnapshotRestoreOnStart = true
	for _, o := range []struct {
		v   time.Duration
		dst *time.Duration
	}{
		{s.cfg.HeartbeatTimeout, &rc.HeartbeatTimeout},
		{s.cfg.ElectionTimeout, &rc.ElectionTimeout},
		{s.cfg.LeaderLeaseTimeout, &rc.LeaderLeaseTimeout},
		{s.cfg.CommitTimeout, &rc.CommitTimeout},
	} {
		if o.v > 0 {
			*o.dst = o.v
		}
	}
	return rc
}

// newestSnapshot returns the index of the newest local snapshot, zero
// without one.
func (s *Store) newestSnapshot() uint64 {
	if s.snaps == nil {
		return 0
	}
	list, err := s.snaps.List()
	if err != nil || len(list) == 0 {
		return 0
	}
	return list[0].Index
}

// closeStores closes the bbolt file and the transport after a failed Open.
func (s *Store) closeStores() {
	if s.r != nil {
		_ = s.r.Shutdown().Error()
	}
	if s.nt != nil {
		_ = s.nt.Close()
	}
	if s.trans != nil {
		s.trans.close()
	}
	if s.logs != nil {
		_ = s.logs.Close()
	}
}

// ServerID returns this replica's server ID.
func (s *Store) ServerID() controlstore.ServerID { return s.id }

// Ready is closed once the Control Store loaded.
func (s *Store) Ready() <-chan struct{} { return s.ready }

// Halted returns the halt that stopped this replica, or nil.
func (s *Store) Halted() error {
	if h := s.haltErr(); h != nil {
		return h
	}
	return nil
}

// haltErr returns the halt, typed.
func (s *Store) haltErr() *controlstore.HaltError {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.halted
}

// haltWith stops the replica before it serves anything more: raft shuts
// down, no snapshot is taken past the entry, every call fails with
// ErrHalted, and OnHalt is told once.
func (s *Store) haltWith(h *controlstore.HaltError) {
	s.mu.Lock()
	if s.halted != nil {
		s.mu.Unlock()
		return
	}
	s.halted = h
	s.mu.Unlock()
	s.log.Error("control store halted", slog.String(catalog.KeyReason, h.Reason), slog.String(catalog.KeyError, h.Error()))
	if s.cfg.OnHalt != nil {
		s.cfg.OnHalt(h)
	}
	signal(s.haltCh)
}

// haltLoop shuts raft down once the replica halts; the FSM goroutine that
// halts cannot, as raft's shutdown waits for it.
func (s *Store) haltLoop() {
	select {
	case <-s.haltCh:
		_ = s.r.Shutdown().Error()
	case <-s.ctx.Done():
	}
}

// check reports a halted or closed store.
func (s *Store) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.halted != nil:
		return fmt.Errorf("%w: %w", controlstore.ErrHalted, s.halted)
	case s.closed:
		return controlstore.ErrClosed
	default:
		return nil
	}
}

// View runs fn on the committed state.
func (s *Store) View(fn func(controlstore.View)) error {
	if err := s.check(); err != nil {
		return err
	}
	s.machine.View(fn)
	return nil
}

// Changed returns a channel closed once the next index applies.
func (s *Store) Changed() <-chan struct{} { return s.machine.Changed() }

// referenced reports whether committed state references d.
func (s *Store) referenced(d controlstore.Digest) bool {
	var ok bool
	s.machine.View(func(v controlstore.View) { ok = v.Referenced(d) })
	return ok
}

// authorizePeer is the transport's membership rule: a revoked server ID is
// refused, and once this replica knows the membership, so is one that is
// neither a member nor pending.
func (s *Store) authorizePeer(p controlstore.PeerIdentity) error {
	var err error
	s.machine.View(func(v controlstore.View) {
		switch _, pending := v.PendingMember(p.ServerID); {
		case v.Revoked(p.ServerID):
			err = fmt.Errorf("server %s is revoked", p.ServerID)
		case v.KnowsMembers() && !v.IsMember(p.ServerID) && !pending:
			err = fmt.Errorf("server %s is neither a member nor pending", p.ServerID)
		}
	})
	return err
}

// signal makes a coalescing wake-up.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Shutdown transfers leadership when this replica leads and another voter
// exists, waiting up to TransferWait on the store's clock or until ctx
// ends, then stops raft, the transport and the background work and closes
// the stores.
func (s *Store) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.r.State() == hraft.Leader && s.otherVoters() > 0 {
		tctx, cancel := context.WithCancel(ctx)
		t := s.clk.AfterFunc(controlstore.TransferWait, cancel)
		err := s.transferLeadership(tctx)
		if hook := s.afterShutdownTransfer.Load(); hook != nil {
			(*hook)(err)
		}
		if err != nil {
			s.log.WarnContext(ctx, "leadership transfer at shutdown failed", slog.String(catalog.KeyError, err.Error()))
		}
		t.Stop()
		cancel()
	}
	s.cancel()
	err := s.r.Shutdown().Error()
	// The state machine stopped: persist what it applied, so a restart
	// applies it again before any leader contact.
	if perr := s.logs.persistApplied(s.machine.Index()); err == nil {
		err = perr
	}
	s.mu.Lock()
	s.futuresDone = true
	s.mu.Unlock()
	s.futures.Wait()
	_ = s.nt.Close()
	s.trans.close()
	s.wg.Wait()
	if cerr := s.logs.Close(); err == nil {
		err = cerr
	}
	return err
}

// wait waits for a raft future or ctx. The goroutine reading the future
// ends when it resolves, which raft guarantees by leadership loss within
// its lease or by shutdown, and Shutdown waits for it.
func (s *Store) wait(ctx context.Context, f hraft.Future) error {
	s.mu.Lock()
	if s.futuresDone {
		s.mu.Unlock()
		return f.Error()
	}
	s.futures.Add(1)
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		defer s.futures.Done()
		done <- f.Error()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fsm adapts the Machine to raft.FSM and raft.ConfigurationStore.
type fsm struct{ s *Store }

// Compile-time checks of the contracts.
var (
	_ hraft.FSM                = (*fsm)(nil)
	_ hraft.ConfigurationStore = (*fsm)(nil)
)

// applyResponse is what an applied entry returns to its proposer.
type applyResponse struct {
	res controlstore.Result
	err error
}

// Apply applies one committed command. An entry the startup replay
// already applied is skipped by its index; after a halt nothing more
// applies, so the state never moves past the entry that halted it.
func (f *fsm) Apply(l *hraft.Log) any {
	if h := f.s.haltErr(); h != nil {
		return &applyResponse{err: h}
	}
	if l.Index <= f.s.machine.Index() {
		return &applyResponse{res: controlstore.Result{Index: l.Index}}
	}
	res, err := f.s.machine.Apply(l.Index, l.Data)
	if err != nil {
		var h *controlstore.HaltError
		if errors.As(err, &h) {
			f.s.haltWith(h)
		}
		return &applyResponse{err: err}
	}
	f.s.logs.noteApplied(l.Index)
	f.s.content.release(res.Released)
	if res.Advanced {
		signal(f.s.truncCh)
	}
	if res.Rejected == nil {
		signal(f.s.fetchCh)
	}
	return &applyResponse{res: res}
}

// StoreConfiguration mirrors a committed configuration entry, skipping
// one the startup replay already applied, so the mirror never moves back.
func (f *fsm) StoreConfiguration(index uint64, c hraft.Configuration) {
	if index <= f.s.machine.Index() {
		return
	}
	f.s.machine.SetConfiguration(index, members(c))
	f.s.logs.noteApplied(index)
}

// Snapshot copies the state; a halted replica takes none.
func (f *fsm) Snapshot() (hraft.FSMSnapshot, error) {
	if h := f.s.haltErr(); h != nil {
		return nil, h
	}
	return &fsmSnapshot{snap: f.s.machine.Snapshot()}, nil
}

// Restore replaces the state with the leader's InstallSnapshot, then
// fetches the content it references.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	snap, err := controlstore.ReadSnapshot(rc, f.s.binary)
	if err != nil {
		var h *controlstore.HaltError
		if errors.As(err, &h) {
			f.s.haltWith(h)
		}
		return err
	}
	f.s.machine.Restore(snap)
	f.s.logs.noteApplied(snap.Index())
	signal(f.s.fetchCh)
	return nil
}

// fsmSnapshot writes one snapshot to raft's sink.
type fsmSnapshot struct{ snap *controlstore.Snapshot }

// Persist writes the snapshot.
func (f *fsmSnapshot) Persist(sink hraft.SnapshotSink) error {
	if _, err := f.snap.WriteTo(sink); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

// Release has nothing to free: the snapshot shares immutable values.
func (*fsmSnapshot) Release() {}
