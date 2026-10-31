// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// FuzzLastKnownGood is this package's target for the "Last-Known-Good,
// file mode" property of docs/engineering/03-testing-and-quality-strategy.md
// "Required properties": under random renders, failures at the gate
// steps, failed and interrupted candidate writes, restarts and handovers,
// a Revision becomes the candidate and Last-Known-Good only by
// activation, only the lock holder writes, and a boot takes a valid
// Bundle, else a verified Last-Known-Good, else stays not ready.
func FuzzLastKnownGood(f *testing.F) {
	for _, seed := range [][]byte{
		{0, 0, 6, 0},
		{0, 1, 2, 3, 0, 6},
		{0, 4, 0, 4, 0, 6, 14},
		{0, 5, 6, 0, 7, 0},
		{1, 6, 22, 0, 15, 4, 7},
		{0, 0, 4, 1, 2, 5, 3, 6, 0, 7, 0, 30},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 16 {
			ops = ops[:16]
		}
		p := newProperty(t)
		for i, op := range ops {
			p.step(i, op)
			p.check()
		}
	})
}

// property drives processes over one data directory and checks the
// invariants after each step.
type property struct {
	t         *testing.T
	dir       *nodedir.Dir
	lock      *nodedir.Lock
	files     *lkg.Store // reads lkg/ as an outside observer
	r         *rig       // the running process
	activated map[revision.Digest]bool
	failing   bool // a directory stands where candidate.json goes
	// frozen is what lkg/ named when a process without the lock started.
	frozen *[2]revision.Digest
}

func newProperty(t *testing.T) *property {
	t.Helper()
	p := &property{t: t, activated: map[revision.Digest]bool{}}
	p.r = newRig(t)
	p.dir, p.lock = p.r.dir, p.r.lock
	var err error
	if p.files, err = lkg.New(lkg.Config{Dir: p.dir}); err != nil {
		t.Fatal(err)
	}
	p.boot(p.r)
	return p
}

// boot starts r and records what it activated.
func (p *property) boot(r *rig) {
	p.t.Helper()
	r.start()
	r.booted()
	p.settle(r)
	p.record()
}

// record notes the active Revision as activated.
func (p *property) record() {
	if d := p.r.l.Active(); !d.IsZero() {
		p.activated[d] = true
	}
}

// settle waits until r runs no job and nothing is due (settled), so what
// lkg/ names is what r wrote and no write of r is still to come.
func (p *property) settle(r *rig) {
	p.t.Helper()
	waitFor(p.t, "the loader to settle", r.settled)
}

// settled reports whether r's activation loop has nothing to start: no
// job runs and its plan finds nothing due, in the order plan reads it.
// That includes the Last-Known-Good write a lock holder owes, as a
// handover successor does once it took the lock: TookLock returns before
// the loop writes, so lkg/ read in between is not what the new holder
// leaves. The lock is read from held, not from the Loader's Lock, which
// withLockPause blocks.
func (r *rig) settled() bool {
	held, now := r.held.Load(), r.clk.Now()
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.job != nil:
		return false
	case l.stopped || l.closed:
		return true
	case l.watchDueLocked(), !l.reloadDue.IsZero() && !now.Before(l.reloadDue), len(l.requests) > 0:
		return false
	case l.holds > 0 || !l.bootDone:
		return true
	case l.next != nil && (l.next.digest == l.active.Digest || !now.Before(l.next.due)):
		return false
	}
	return !held || !l.lkgOwedLocked() || now.Before(l.lkgDue)
}

// withLockPause makes the Loader's lock check, the first step of each
// plan of its activation loop, wait while pause is locked, so a test sees
// the loader between a change and the plan that acts on it.
func withLockPause(pause *sync.RWMutex) rigOption {
	return func(r *rig) {
		lock := r.cfg.Lock
		r.cfg.Lock = func() *nodedir.Lock {
			pause.RLock()
			defer pause.RUnlock()
			return lock()
		}
	}
}

// pointers returns what lkg/ names, zero for an absent or unreadable
// pointer.
func (p *property) pointers() [2]revision.Digest {
	cand, last, _ := p.files.Pointers()
	return [2]revision.Digest{cand, last}
}

// step applies op.
func (p *property) step(i int, op byte) {
	p.t.Helper()
	name := "R" + strconv.Itoa(i)
	switch op % 8 {
	case 0: // a valid render
		p.r.l.Offer(load(result(name)))
	case 1: // a parse failure
		p.r.l.Offer(load(failedResult(diag.List{finding("RZ-CFG-001", 1, "parse error")})))
	case 2: // the compile refuses it
		p.r.comp.script(rev(name).Digest, findings(finding("RZ-CFG-040", 1, "unserved")))
		p.r.l.Offer(load(result(name)))
	case 3: // a listener cannot bind: transient, pending its retry
		p.r.parts[0].set(func(fp *fakePart) { fp.prepareErr = bindError("port in use") })
		p.r.l.Offer(load(result(name)))
		p.settle(p.r)
		p.r.parts[0].set(func(fp *fakePart) { fp.prepareErr = nil })
	case 4: // candidate writes start or stop failing
		path := p.files.Path(lkg.CandidateFile)
		if p.failing {
			if err := os.Remove(path); err != nil {
				p.t.Fatal(err)
			}
		} else {
			_ = os.Remove(path)
			if err := os.Mkdir(path, 0o700); err != nil {
				p.t.Fatal(err)
			}
		}
		p.failing = !p.failing
	case 5: // an interrupted write leaves unnamed files
		junk := rev("junk" + name)
		for _, f := range []string{lkg.ContentName(junk.Digest), ".tmp-lkg.json-" + name} {
			if err := os.WriteFile(p.files.Path(f), junk.Content, 0o600); err != nil {
				p.t.Fatal(err)
			}
		}
	case 6: // a restart onto a valid, broken or missing source
		p.restart(i, op)
	case 7: // a handover successor beside the holder
		p.handover(i, op)
	}
	p.settle(p.r)
	p.record()
}

// bootLoad is the source a restarted process finds.
func bootLoad(i int, op byte) (source.Load, revision.Digest) {
	switch (op / 8) % 3 {
	case 0:
		name := "S" + strconv.Itoa(i)
		return load(result(name)), rev(name).Digest
	case 1:
		return load(failedResult(diag.List{finding("RZ-CFG-005", 1, "bad")})), revision.Digest{}
	default:
		return unavailable(), revision.Digest{}
	}
}

// restart stops the process and boots a new one on the same data
// directory: a valid source, else verified Last-Known-Good, else nothing.
func (p *property) restart(i int, op byte) {
	p.t.Helper()
	p.r.stop()
	ld, srcDigest := bootLoad(i, op)
	lastKnownGood := p.pointers()[1]
	p.r = newRigOn(p.t, p.dir, p.lock, withSource(ld))
	p.frozen = nil
	p.boot(p.r)
	want := srcDigest
	if want.IsZero() {
		want = lastKnownGood
	}
	if got := p.r.l.Active(); got != want {
		p.t.Fatalf("restart booted %s, want %s (source %s, Last-Known-Good %s)", got.Short(), want.Short(), srcDigest.Short(), lastKnownGood.Short())
	}
	if want.IsZero() && p.r.ready.Ready() {
		p.t.Fatal("a Node without usable configuration is ready")
	}
}

// handover boots a successor beside the holder: it boots the holder's
// candidate, or by the file-mode order without one, and writes nothing
// before it holds the lock; then the holder exits and it takes over.
func (p *property) handover(i int, op byte) {
	p.t.Helper()
	before := p.pointers()
	_, candErr := p.files.Candidate()
	ld, srcDigest := bootLoad(i, op)
	s := newRigOn(p.t, p.dir, p.lock, withSuccessorBoot(), withSource(ld))
	s.held.Store(false)
	s.start()
	s.booted()
	p.settle(s)
	var want revision.Digest
	switch {
	case candErr == nil:
		want = before[0]
	case errors.Is(candErr, lkg.ErrAbsent):
		want = srcDigest
		if want.IsZero() {
			want = before[1]
		}
	}
	if got := s.l.Active(); got != want {
		p.t.Fatalf("successor booted %s, want %s", got.Short(), want.Short())
	}
	if p.pointers() != before {
		p.t.Fatal("a successor wrote lkg/ before it held the lock")
	}
	if !s.l.Active().IsZero() {
		p.activated[s.l.Active()] = true
	}
	// The holder exits; the successor listens and takes the lock. Its loop
	// then writes lkg/ when lkg/ names another Revision, which the step's
	// settle waits for.
	p.r.stop()
	if err := s.l.Bound().Listen(p.t.Context()); err != nil {
		p.t.Fatal(err)
	}
	s.held.Store(true)
	s.l.TookLock()
	p.r = s
}

// check asserts the invariants: lkg/ names only activated Revisions, and
// never changes while no process holds the lock.
func (p *property) check() {
	p.t.Helper()
	ptrs := p.pointers()
	for i, d := range ptrs {
		if !d.IsZero() && !p.activated[d] {
			p.t.Fatalf("lkg/ pointer %d names %s, which never activated", i, d.Short())
		}
	}
	if !p.r.held.Load() {
		if p.frozen == nil {
			p.frozen = &ptrs
		} else if *p.frozen != ptrs {
			p.t.Fatal("lkg/ changed while no process held the lock")
		}
	}
}

// TestSettleWaitsForTheWriteOfASuccessorThatTookTheLock is the
// FuzzLastKnownGood sequence of a handover right after a handover, made
// deterministic: a handover successor that booted by the file-mode order,
// the holder having no candidate, owes lkg/ its active Revision once it
// took the lock. Until its loop wrote it, the loader is not settled; else
// the next handover expects the boot of an lkg/ that the new holder then
// rewrites.
func TestSettleWaitsForTheWriteOfASuccessorThatTookTheLock(t *testing.T) {
	p := newProperty(t)
	// The holder serves A, and lkg/ names no candidate.
	if err := os.Remove(p.files.Path(lkg.CandidateFile)); err != nil {
		t.Fatal(err)
	}
	var pause sync.RWMutex
	s := newRigOn(t, p.dir, p.lock, withSuccessorBoot(), withSource(load(result("S"))), withLockPause(&pause))
	s.held.Store(false)
	s.start()
	s.booted()
	p.settle(s)
	sd := rev("S").Digest
	if got := s.l.Active(); got != sd {
		t.Fatalf("the successor booted %s, want its source S", got.Short())
	}
	p.r.stop()
	if err := s.l.Bound().Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The process takes the lock while its loop cannot plan the write.
	pause.Lock()
	s.held.Store(true)
	s.l.TookLock()
	settled := s.settled()
	pause.Unlock()
	if settled {
		t.Fatal("settled while the successor that took the lock owes lkg/ its active Revision")
	}
	p.r = s
	p.settle(s)
	if got := p.pointers(); got != [2]revision.Digest{sd, sd} {
		t.Fatalf("lkg/ names %s and %s once settled, want S for both", got[0].Short(), got[1].Short())
	}
	p.record()
	p.check()
	// The next step is a handover (15 % 8 == 7) whose successor's source
	// fails ((15 / 8) % 3 == 1): it boots the new holder's candidate S, as
	// the model expects.
	p.step(1, 15)
	p.check()
	if got := p.r.l.Active(); got != sd {
		t.Fatalf("the next successor booted %s, want the candidate S", got.Short())
	}
}

// TestSettleWaitsForADueLastKnownGoodRetry checks that a loader whose
// retry of a failed candidate write is due is not settled until its loop
// wrote lkg/.
func TestSettleWaitsForADueLastKnownGoodRetry(t *testing.T) {
	var pause sync.RWMutex
	r := newRig(t, withLockPause(&pause))
	// A directory where candidate.json goes fails the boot's write.
	path := r.store.Path(lkg.CandidateFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.booted()
	waitFor(t, "the loader to settle", r.settled)
	if !r.lkgStat.raised("lkg_write_failed") {
		t.Fatal("the boot's write did not fail")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// The retry comes due while the loop cannot plan it.
	pause.Lock()
	r.clk.Advance(lkg.RetryDelay(1))
	settled := r.settled()
	pause.Unlock()
	if settled {
		t.Fatal("settled while the retry of a failed Last-Known-Good write is due")
	}
	waitFor(t, "the loader to settle", r.settled)
	a := rev("A").Digest
	if cand, last, err := r.store.Pointers(); err != nil || cand != a || last != a {
		t.Fatalf("lkg/ names %s and %s (%v) once settled, want A for both", cand.Short(), last.Short(), err)
	}
}
