// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/reload"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// Codes of the gate steps FuzzLastKnownGoodFileMode drives.
const (
	codeParse    = "RZ-CFG-001"
	codeRef      = "RZ-CFG-009"
	codeUnserved = "RZ-CFG-040"
	codeResolve  = "RZ-CFG-026"
	codeBind     = "RZ-CFG-039"
	codeFault    = "RZ-CFG-045"
)

// lkgKind is what a rendered source holds.
type lkgKind int

// Source kinds.
const (
	// srcValid activates.
	srcValid lkgKind = iota
	// srcParse fails stage B: RZ-CFG-001.
	srcParse
	// srcRef fails stage H, references: RZ-CFG-009.
	srcRef
	// srcUnserved passes the pipeline and fails the Node's serve check:
	// RZ-CFG-040.
	srcUnserved
	// srcSecret names a file secret that does not exist yet: the
	// transient RZ-CFG-026.
	srcSecret
	// srcMissing is no source at all.
	srcMissing
)

// FuzzLastKnownGoodFileMode is the "Last-Known-Good, file mode" row of the
// Required properties: under random renders, failures at every gate step,
// failed or interrupted candidate writes, restarts and handovers, a
// Revision becomes the candidate and Last-Known-Good only by activation,
// only the lock holder writes, and boot takes a valid Bundle, else a
// verified Last-Known-Good, else stays not ready (foundation pack section
// 8.2, docs/architecture/03-data-plane.md "Last-Known-Good files",
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Validation gate").
//
// Simulated processes share one data directory and its lock, one source
// directory and one fake clock. Each runs the real source watcher
// (internal/gateway/source) over the configuration pipeline, the real
// activation loader (internal/gateway/reload) with the real snapshot
// compiler and Last-Known-Good store (internal/gateway/lkg), and a
// retirer; the watcher loads at its first poll and on Reload, as SIGHUP
// makes it, and polls no more (noPoll). The compiler and the listener part inject the gate failures
// the real parts cannot produce on demand: a panic (RZ-CFG-045) and a
// port that cannot bind (RZ-CFG-039). Each op byte is one step: a valid
// render; a render failing to parse (RZ-CFG-001), a reference
// (RZ-CFG-009), the serve check (RZ-CFG-040), secret resolution
// (RZ-CFG-026, then resolved and retried), listener binding (RZ-CFG-039,
// then freed and retried) or the compile (RZ-CFG-045); candidate writes
// starting or stopping to fail; an interrupted write's unnamed files; a
// tampered Last-Known-Good; a restart onto a valid, broken or missing
// source; a handover. After every step lkg/ names only activated
// Revisions, and names the active one once its write succeeded; a process
// without the lock never changes it; and each boot activates exactly what
// the boot order allows, staying not ready without it.
func FuzzLastKnownGoodFileMode(f *testing.F) {
	// Op 10 restarts onto a valid source, 22 onto a broken one and 34 onto
	// none; 11 is a handover.
	for _, seed := range [][]byte{
		{0, 1, 2, 3, 10},
		{0, 7, 0, 22, 7, 34},
		{4, 5, 6, 0, 11},
		{0, 9, 22, 0, 34},
		{8, 0, 7, 11, 0, 10},
		{1, 3, 6, 11, 22, 34, 0, 5},
		{7, 0, 11, 7, 0, 10},
		{0, 9, 11, 0, 22},
		{0, 34, 6, 4, 10},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 12 {
			ops = ops[:12]
		}
		e := newLKGEnv(t)
		for i, op := range ops {
			e.step(i, op)
			e.check()
		}
	})
}

// lkgEnv is the shared directories, clock and fault switches of the
// processes, and the oracle's state.
type lkgEnv struct {
	t      *testing.T
	tc     *toolchain
	clk    *clocktest.Fake
	dir    *nodedir.Dir
	lock   *nodedir.Lock
	store  *lkg.Store // reads lkg/ as an outside observer
	config string     // RURALZ_CONFIG
	root   string     // RURALZ_SECRET_ROOT
	part   *faultPart
	// panicNext makes the next compile panic.
	panicNext atomic.Bool
	cur       *lkgProcess
	renders   int
	// src is the source's kind and srcDigest its Revision, zero unless
	// the pipeline builds it.
	src       lkgKind
	srcDigest revision.Digest
	// writesFailing: a directory stands where candidate.json goes, which
	// held savedCandidate before (nil for none).
	writesFailing  bool
	savedCandidate []byte
	// recorders hold the log records of every process started.
	recorders []*logRecorder
}

// newLKGEnv starts the first process, the lock holder, on a valid source.
func newLKGEnv(t *testing.T) *lkgEnv {
	t.Helper()
	e := &lkgEnv{t: t, tc: tools(t), clk: clocktest.New(epoch()), part: &faultPart{}}
	var err error
	if e.dir, err = nodedir.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	e.lock, err = e.dir.TryLock()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no file lock on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.lock.Release() })
	if e.store, err = lkg.New(lkg.Config{Dir: e.dir}); err != nil {
		t.Fatal(err)
	}
	e.config = filepath.Join(t.TempDir(), "config")
	e.root = t.TempDir()
	e.render(srcValid)
	e.cur = e.start(reload.BootHolder, true)
	e.booted(e.cur)
	e.firstPoll(e.cur)
	e.waitActive(e.cur, e.srcDigest)
	e.settle(e.cur)
	return e
}

// noPoll is the source poll interval of every process: longer than all
// the time a run moves the clock, so a poll that a move made due never
// races a step's own Reload with a second load of the source. Polling and
// its settle debounce are internal/gateway/source's own tests.
const noPoll = 365 * 24 * time.Hour

// lkgProcess is one simulated ruralzd process.
type lkgProcess struct {
	l      *reload.Loader
	w      *source.Watcher
	ready  *readiness.Tracker
	holder *retire.Holder
	// held: the process holds the data-directory lock.
	held   atomic.Bool
	loads  atomic.Int64
	logs   *logRecorder
	cancel context.CancelFunc
	done   sync.WaitGroup
	once   sync.Once
}

// start runs a process with boot order boot, holding the lock or not.
func (e *lkgEnv) start(boot reload.Boot, held bool) *lkgProcess {
	t := e.t
	t.Helper()
	p := &lkgProcess{ready: readiness.New(), holder: retire.New(retire.Config{Clock: e.clk, Stripes: 1}), logs: &logRecorder{}}
	e.recorders = append(e.recorders, p.logs)
	p.held.Store(held)
	node := newNodeCompiler(t, e.clk, nodeParts{Filters: filter.NewRegistry(nil), Root: e.root})
	store, err := lkg.New(lkg.Config{Dir: e.dir, Version: "0.1.0-test", Clock: e.clk})
	if err != nil {
		t.Fatal(err)
	}
	var l *reload.Loader
	w, err := source.New(source.Config{Path: e.config, Pipeline: e.tc.pipe, Clock: e.clk, Interval: noPoll, Sink: func(ld source.Load) {
		l.Offer(ld)
		p.loads.Add(1)
	}})
	if err != nil {
		t.Fatal(err)
	}
	l, err = reload.New(reload.Config{
		Boot: boot, Source: w, Pipeline: e.tc.pipe, Compiler: &faultCompiler{inner: reload.CompilerOf(node.comp), panicNext: &e.panicNext},
		Holder: p.holder, Readiness: p.ready, LKG: store,
		Lock: func() *nodedir.Lock {
			if p.held.Load() {
				return e.lock
			}
			return nil
		},
		Parts: []reload.Part{e.part}, RequestID: func(*http.Request) string { return "0af7651916cd43dd8448eb211c80319c" },
		Clock: e.clk, Rand: fixedSource{}, Logger: slog.New(p.logs), FileLimit: func() (uint64, bool) { return 1 << 20, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.l, p.w = l, w
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done.Go(func() { _ = p.holder.Run(ctx) })
	p.done.Go(func() { _ = l.Run(ctx) })
	t.Cleanup(p.stop)
	return p
}

// stop ends the process, as its exit does.
func (p *lkgProcess) stop() {
	p.once.Do(func() {
		p.l.Stop()
		p.cancel()
		p.done.Wait()
	})
}

// booted waits for p's first boot attempt to end.
func (e *lkgEnv) booted(p *lkgProcess) {
	e.t.Helper()
	select {
	case <-p.l.Booted():
	case <-time.After(bound):
		e.t.Fatal("the boot did not end")
	}
}

// firstPoll waits until the watcher of p, a lock holder at start, made
// the first poll of its Run, which loads the source when the boot did not:
// a Reload, which that poll takes or which follows it, loads the source
// once more. So no first poll runs late, after a step rendered a new
// source, beside that step's own Reload.
func (e *lkgEnv) firstPoll(p *lkgProcess) {
	e.t.Helper()
	before := p.loads.Load()
	p.w.Reload()
	waitFor(e.t, "the watcher's first poll", func() bool { return p.loads.Load() > before })
}

// waitActive waits until p serves d.
func (e *lkgEnv) waitActive(p *lkgProcess, d revision.Digest) {
	e.t.Helper()
	waitFor(e.t, "Revision "+d.Short()+" to activate", func() bool { return p.l.Active() == d })
}

// waitRejected waits until p's last rejection is code, for d unless d is
// zero.
func (e *lkgEnv) waitRejected(p *lkgProcess, d revision.Digest, code string) {
	e.t.Helper()
	waitFor(e.t, "a rejection "+code, func() bool {
		lr := p.l.Snapshots().LastRejected
		if lr == nil || lr.Code != code {
			return false
		}
		return d.IsZero() && lr.Digest == nil || lr.Digest != nil && *lr.Digest == d.String()
	})
}

// pointers returns what candidate.json and lkg.json name, zero for one
// absent or unreadable.
func (e *lkgEnv) pointers() [2]revision.Digest {
	c, l, _ := e.store.Pointers()
	return [2]revision.Digest{c, l}
}

// settle waits until p runs no job, its last activation's candidate write
// included (reload.Loader.Hold, released at once), then, when p holds the
// lock, for the write of its active Revision: lkg/ names it, the clock
// moving past the retry of a write that failed; while writes fail, a
// process logged a write of it, failed or done before they began to fail
// (a successor that took the lock writes only when lkg/ names another
// Revision).
func (e *lkgEnv) settle(p *lkgProcess) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	if _, err := p.l.Hold(ctx); err != nil {
		e.t.Fatalf("the loader did not become idle: %v", err)
	}
	p.l.Release()
	d := p.l.Active()
	if d.IsZero() || !p.held.Load() {
		return
	}
	if e.writesFailing {
		waitFor(e.t, "the write of "+d.Short(), func() bool {
			for _, r := range e.recorders {
				if r.count("Last-Known-Good write failed", "revision", d.Short())+r.count("Last-Known-Good written", "revision", d.Short()) > 0 {
					return true
				}
			}
			return false
		})
		return
	}
	e.advanceUntil(p, "lkg/ to name "+d.Short(), lkg.RetryMax, func() bool { return e.pointers() == [2]revision.Digest{d, d} })
}

// retried waits until p serves d, rejected with a transient code whose
// cause is gone, the clock moving past the retry.
func (e *lkgEnv) retried(p *lkgProcess, d revision.Digest) {
	e.t.Helper()
	e.advanceUntil(p, "the retry of "+d.Short(), reload.RetryMax, func() bool { return p.l.Active() == d })
}

// retryWindow is how long advanceUntil waits for what a move of the clock
// made due before it moves the clock again.
const retryWindow = 500 * time.Millisecond

// advanceUntil moves the clock by step until cond holds, for an outcome
// that waits on a timer of p's loader, a retry of a load or a write. The
// loop arms a timer for its due time less the time it read just before,
// so a move between the two arms it a move late, and only the next move
// fires it. Each move runs under Hold, while p runs no job, so no
// activation passes its compile deadline meanwhile; cond holding without a
// move moves nothing.
func (e *lkgEnv) advanceUntil(p *lkgProcess, what string, step time.Duration, cond func() bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	for {
		if _, err := p.l.Hold(ctx); err != nil {
			e.t.Fatalf("timed out waiting for %s: %v", what, err)
		}
		done := cond()
		if !done {
			e.clk.Advance(step)
		}
		p.l.Release()
		if done || pollFor(retryWindow, cond) {
			return
		}
		if ctx.Err() != nil {
			e.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// render writes a source of kind, unique to this render, and records its
// Revision.
func (e *lkgEnv) render(kind lkgKind) {
	t := e.t
	t.Helper()
	e.renders++
	if err := os.RemoveAll(e.config); err != nil {
		t.Fatal(err)
	}
	e.src, e.srcDigest = kind, revision.Digest{}
	if kind == srcMissing {
		return
	}
	files := lkgBundle(e.renders, kind, filepath.Join(e.root, "secret-"+strconv.Itoa(e.renders)))
	writeTree(t, e.config, files)
	if kind == srcParse || kind == srcRef {
		return
	}
	res := e.tc.run(t, memBundle{}.withFiles(files), noEnv, nil, false)
	if res.Validated == nil || res.HasErrors() {
		t.Fatalf("source %d does not build:\n%s", kind, texts(res.Diagnostics))
	}
	e.srcDigest = res.Validated.Revision.Digest
}

// lkgBundle returns a Bundle of kind whose resources carry the render n,
// so every render has its own Revision; secret is the file a srcSecret
// Bundle's secretRef names.
func lkgBundle(n int, kind lkgKind, secret string) map[string][]byte {
	if kind == srcParse {
		return map[string][]byte{"ruralz.yaml": []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: [unclosed\n")}
	}
	ann := fmt.Sprintf("{property.ruralz.test/render: %q}", strconv.Itoa(n))
	protocol, upstream := "http", "u"
	if kind == srcUnserved {
		protocol = "grpc"
	}
	if kind == srcRef {
		upstream = "missing"
	}
	docs := "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge, annotations: " + ann + "}\nspec:\n  listeners:\n    - {name: http, protocol: http, port: 8080}\n" +
		"---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: u}\nspec:\n  protocol: " + protocol + "\n  endpoints: [{address: \"127.0.0.1:9\"}]\n" +
		"---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata: {name: r}\nspec:\n  match: {path: {prefix: /}}\n  upstreams: [{name: " + upstream + "}]\n"
	if kind == srcSecret {
		docs += "---\napiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: oauth}\nspec:\n  type: auth.upstream-oauth2\n" +
			"  config: {tokenUrl: \"https://idp.example/token\", clientId: gw, clientSecret: {secretRef: {provider: file, name: \"" + secret + "\"}}}\n"
	}
	return map[string][]byte{"ruralz.yaml": []byte(docs)}
}

// load renders a source of kind and makes the running process load it at
// once, as SIGHUP does, waiting until the load reached the loader.
func (e *lkgEnv) load(kind lkgKind) {
	e.t.Helper()
	e.render(kind)
	before := e.cur.loads.Load()
	e.cur.w.Reload()
	waitFor(e.t, "the source load", func() bool { return e.cur.loads.Load() > before })
}

// step applies op.
func (e *lkgEnv) step(i int, op byte) {
	t := e.t
	t.Helper()
	p := e.cur
	active := p.l.Active()
	switch op % 12 {
	case 0: // a valid render activates
		e.load(srcValid)
		e.waitActive(p, e.srcDigest)
	case 1: // a parse failure
		e.load(srcParse)
		e.waitRejected(p, revision.Digest{}, codeParse)
		e.unchanged(p, active)
	case 2: // a reference failure
		e.load(srcRef)
		e.waitRejected(p, revision.Digest{}, codeRef)
		e.unchanged(p, active)
	case 3: // the serve check refuses an unserved feature
		e.load(srcUnserved)
		e.waitRejected(p, e.srcDigest, codeUnserved)
		e.unchanged(p, active)
	case 4: // a secret that does not exist yet: retried once it does
		e.load(srcSecret)
		e.waitRejected(p, e.srcDigest, codeResolve)
		e.unchanged(p, active)
		if err := os.WriteFile(filepath.Join(e.root, "secret-"+strconv.Itoa(e.renders)), []byte("s3cret"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.retried(p, e.srcDigest)
		e.src = srcValid
	case 5: // a listener cannot bind: retried once it can
		e.part.fail.Store(true)
		e.load(srcValid)
		e.waitRejected(p, e.srcDigest, codeBind)
		e.unchanged(p, active)
		e.part.fail.Store(false)
		e.retried(p, e.srcDigest)
	case 6: // the compile panics once: RZ-CFG-045; the source stays valid
		e.panicNext.Store(true)
		e.load(srcValid)
		e.waitRejected(p, e.srcDigest, codeFault)
		e.unchanged(p, active)
	case 7: // candidate writes start or stop failing
		e.toggleWrites()
	case 8: // an interrupted write leaves unnamed files
		junk := []byte(`{"format":"ruralz.canonical.v1","resources":[]}`)
		for _, name := range []string{lkg.ContentName(revision.Sum([]byte(string(junk) + strconv.Itoa(i)))), ".tmp-lkg.json-" + strconv.Itoa(i)} {
			if err := os.WriteFile(e.store.Path(name), junk, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	case 9: // Last-Known-Good is tampered with: it no longer verifies
		if d := e.pointers()[1]; !d.IsZero() {
			if err := os.WriteFile(e.store.Path(lkg.ContentName(d)), []byte(`{"tampered":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	case 10: // a restart onto a valid, broken or missing source
		e.restart([]lkgKind{srcValid, srcParse, srcMissing}[int(op/12)%3])
	case 11: // a handover successor takes over from the holder
		e.handover()
	}
	e.settle(e.cur)
}

// isTampered reports whether the content file of d in lkg/ does not hash
// to d, as after it was tampered with and no write of d followed, which
// the boot's verification refuses (RZ-CFG-027). The caller reads it while
// no process writes: the holder stopped or settled.
func (e *lkgEnv) isTampered(d revision.Digest) bool {
	path := e.store.Path(lkg.ContentName(d))
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the test's own data directory.
	return err != nil || revision.Sum(data) != d
}

// unchanged fails t unless p still serves active.
func (e *lkgEnv) unchanged(p *lkgProcess, active revision.Digest) {
	e.t.Helper()
	if got := p.l.Active(); got != active {
		e.t.Fatalf("a rejected Revision changed what the Node serves: %s, want %s", got.Short(), active.Short())
	}
}

// toggleWrites makes candidate writes fail, with a directory where
// candidate.json goes, or succeed again: the directory makes way for the
// pointer it replaced, and the loader's retry of a failed write, due within
// lkg.RetryMax, writes the active Revision once settle moves the clock.
func (e *lkgEnv) toggleWrites() {
	t := e.t
	t.Helper()
	path := e.store.Path(lkg.CandidateFile)
	if e.writesFailing {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if e.savedCandidate != nil {
			if err := os.WriteFile(path, e.savedCandidate, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e.writesFailing = false
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the test's own data directory.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	e.savedCandidate = data
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	e.writesFailing = true
}

// lkgBoot returns the Revision a boot without a usable source may take:
// what lkg.json names, unless its content was tampered with.
func (e *lkgEnv) lkgBoot() revision.Digest {
	d := e.pointers()[1]
	if e.isTampered(d) {
		return revision.Digest{}
	}
	return d
}

// restart stops the holder and boots a new one on a source of kind: the
// source when it activates, else a verified Last-Known-Good, else nothing,
// not ready.
func (e *lkgEnv) restart(kind lkgKind) {
	t := e.t
	t.Helper()
	e.cur.stop()
	e.render(kind)
	want := e.srcDigest
	if kind != srcValid {
		want = e.lkgBoot()
	}
	e.cur = e.start(reload.BootHolder, true)
	e.booted(e.cur)
	e.firstPoll(e.cur)
	e.checkBoot(e.cur, want)
}

// checkBoot fails t unless p boots want, ready exactly when it serves one.
func (e *lkgEnv) checkBoot(p *lkgProcess, want revision.Digest) {
	e.t.Helper()
	if !want.IsZero() {
		e.waitActive(p, want)
		waitFor(e.t, "readiness", p.ready.Ready)
		return
	}
	if got := p.l.Active(); !got.IsZero() || p.ready.Ready() {
		e.t.Fatalf("a boot without a valid Bundle or verified Last-Known-Good serves %s, ready %v", got.Short(), p.ready.Ready())
	}
}

// handover boots a successor beside the holder: the holder's candidate,
// verified through the full gate, or by the file-mode order without one;
// it writes nothing before it holds the lock. Then the holder exits and
// the successor takes the lock, loads the source and writes its active
// Revision.
func (e *lkgEnv) handover() {
	t := e.t
	t.Helper()
	before := e.pointers()
	cand, candErr := e.store.Candidate()
	var want revision.Digest
	switch {
	case candErr == nil && !e.isTampered(cand.Digest):
		want = cand.Digest
	case errors.Is(candErr, lkg.ErrAbsent):
		want = e.srcDigest
		if e.src != srcValid {
			want = e.lkgBoot()
		}
	}
	s := e.start(reload.BootSuccessor, false)
	e.booted(s)
	if !want.IsZero() {
		e.waitActive(s, want)
	} else if got := s.l.Active(); !got.IsZero() {
		t.Fatalf("the successor booted %s, want nothing (candidate %v)", got.Short(), candErr)
	}
	if got := e.pointers(); got != before {
		t.Fatalf("lkg/ changed from %v to %v while the successor did not hold the lock", before, got)
	}
	e.cur.stop()
	if err := s.l.Bound().Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	loads := s.loads.Load()
	s.held.Store(true)
	s.l.TookLock()
	waitFor(t, "the successor's first source load", func() bool { return s.loads.Load() > loads })
	if e.src == srcValid {
		e.waitActive(s, e.srcDigest)
	}
	e.cur = s
}

// check asserts the invariants after a step: lkg/ names only Revisions
// some process activated.
func (e *lkgEnv) check() {
	e.t.Helper()
	activated := map[revision.Digest]bool{}
	for _, r := range e.recorders {
		maps.Copy(activated, r.activated())
	}
	for i, d := range e.pointers() {
		if !d.IsZero() && !activated[d] {
			e.t.Fatalf("lkg/ pointer %d names %s, which never activated", i, d.Short())
		}
	}
}

// faultCompiler is the snapshot compiler, panicking once when panicNext
// is set: a fault of the release, which the loader rejects with
// RZ-CFG-045.
type faultCompiler struct {
	inner     reload.Compiler
	panicNext *atomic.Bool
}

func (c *faultCompiler) Compile(ctx context.Context, in compile.Input) (reload.Compiled, diag.List, error) {
	if c.panicNext.CompareAndSwap(true, false) {
		panic("property: an injected compile fault")
	}
	return c.inner.Compile(ctx, in)
}

// faultPart is the client listeners part, whose sockets cannot bind
// (RZ-CFG-039) while fail is set.
type faultPart struct{ fail atomic.Bool }

func (p *faultPart) Prepare(context.Context, reload.Compiled) (reload.Switch, error) {
	if p.fail.Load() {
		return nil, errcode.Wrap(codeBind, errors.New("listener http: address already in use"))
	}
	return nopSwitch{}, nil
}

func (p *faultPart) Listen(context.Context) error { return nil }

func (p *faultPart) StopAccepting() {}

// nopSwitch commits nothing.
type nopSwitch struct{}

func (nopSwitch) Commit(context.Context) error { return nil }

func (nopSwitch) Abort() {}

// logRecord is one log record: its message and attributes as text.
type logRecord struct {
	msg   string
	attrs map[string]string
}

// logRecorder keeps every record of one process.
type logRecorder struct {
	mu   sync.Mutex
	recs []logRecord
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.recs = append(h.recs, rec)
	h.mu.Unlock()
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *logRecorder) WithGroup(string) slog.Handler { return h }

// count returns the records of msg whose attribute key is val.
func (h *logRecorder) count(msg, key, val string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		if r.msg == msg && r.attrs[key] == val {
			n++
		}
	}
	return n
}

// activated returns every Revision some process logged as activated.
func (h *logRecorder) activated() map[revision.Digest]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[revision.Digest]bool{}
	for _, r := range h.recs {
		if r.msg != "revision activated" {
			continue
		}
		if d, err := revision.Parse(r.attrs["digest"]); err == nil {
			out[d] = true
		}
	}
	return out
}
