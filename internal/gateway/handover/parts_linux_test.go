// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/drain"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/sdnotify"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The Node's parts implement the handover's interfaces.
var (
	_ Drainer    = (*drain.Drainer)(nil)
	_ Sockets    = (*listener.Set)(nil)
	_ Sockets    = (*listener.AdminPort)(nil)
	_ Pinner     = (*listener.Set)(nil)
	_ Pinner     = (*listener.AdminPort)(nil)
	_ Admin      = (*admin.Server)(nil)
	_ Notifier   = (*sdnotify.Notifier)(nil)
	_ Ceiling    = (*admission.ConnLimiter)(nil)
	_ Ceiling    = (*admission.Units)(nil)
	_ Ceiling    = (*body.Budget)(nil)
	_            = Meter{Conns: (*admission.ConnLimiter)(nil), Units: (*admission.Units)(nil), Budget: (*body.Budget)(nil)}
	_ drain.Lock = (*Lock)(nil)
)

// fresh is the number of fresh connections each routing check makes; a
// hashed reuseport group would spread them over its sockets.
const fresh = 16

// partsWatchdog is the holder's watchdog interval in the real-parts tests,
// the T5 unit's WatchdogSec=30s.
const partsWatchdog = 30 * time.Second

// tagHandler answers every request with tag.
func tagHandler(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tag) })
}

// holdingHandler is the holder's handler: it answers every request with
// its tag. Armed, it answers the next request in full, with its
// Content-Length, and flushed, then returns only once the arming's gate is
// closed. The holder's server closes a connection only after its handler
// returned, so that connection stays open, and counted, after its client
// read the whole answer and closed: the state of a routing check's fresh
// connection from its answer to the server's close.
type holdingHandler struct {
	tag string
	mu  sync.Mutex
	// gate, while set, arms the handler: the next request's handler waits
	// until it is closed.
	gate chan struct{}
}

// arm makes the next request's handler wait, after its answer, until gate
// is closed.
func (h *holdingHandler) arm(gate chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gate = gate
}

func (h *holdingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	gate := h.gate
	h.gate = nil
	h.mu.Unlock()
	w.Header().Set("Content-Length", strconv.Itoa(len(h.tag)))
	_, _ = io.WriteString(w, h.tag)
	if gate == nil {
		return
	}
	// The client reads the whole answer now. Its close ends the request's
	// context, so the handler waits for the gate alone.
	_ = http.NewResponseController(w).Flush()
	<-gate
}

// freshClient returns a client that opens a fresh connection per request.
func freshClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: bound}
}

// fetch sends GET / to addr through c and returns the answer.
func fetch(ctx context.Context, c *http.Client, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b), err
}

// routes sends one request on each of fresh connections to addr and
// counts the answers by tag ("" for none).
func routes(t *testing.T, addr string) map[string]int {
	t.Helper()
	c := freshClient()
	got := make(map[string]int)
	for range fresh {
		tag, err := fetch(t.Context(), c, addr)
		if err != nil {
			got[""]++
			continue
		}
		got[tag]++
	}
	return got
}

// keepAlive is an HTTP/1.1 keep-alive connection the test drives by hand.
type keepAlive struct {
	c  net.Conn
	br *bufio.Reader
}

func dialKeepAlive(t *testing.T, addr string) *keepAlive {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &keepAlive{c: c, br: bufio.NewReader(c)}
}

// get sends GET / and returns the answer.
func (k *keepAlive) get(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://node/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(k.c); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(k.br, req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b)
}

// partsReadiness is the readiness the Drains fail.
type partsReadiness struct{ ev *events }

func (r partsReadiness) Drain()       { r.ev.add("readiness:drain") }
func (partsReadiness) Answered() bool { return true }

// partsRetirer ends nothing.
type partsRetirer struct{}

func (partsRetirer) EndAll(context.Context, snapshot.EndReason) (int, error) { return 0, nil }
func (partsRetirer) Close(context.Context) error                             { return nil }

// httpAdmin is an admin server: a tagged server whose keep-alives the
// handover moves.
type httpAdmin struct {
	srv *http.Server
	ev  *events
}

func (a *httpAdmin) DisableKeepAlives() {
	a.srv.SetKeepAlivesEnabled(false)
	a.ev.add("admin:keepalives off")
}

func (a *httpAdmin) EnableKeepAlives() {
	a.srv.SetKeepAlivesEnabled(true)
	a.ev.add("admin:keepalives on")
}

// serveAdmin serves ln with a server answering tag.
func serveAdmin(t *testing.T, ln net.Listener, tag string, ev *events) *httpAdmin {
	t.Helper()
	srv := &http.Server{Handler: tagHandler(tag), ReadHeaderTimeout: bound}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &httpAdmin{srv: srv, ev: ev}
}

// partsBound is the new process's bound sockets: its listener Set's
// pending Plan and its admin port.
type partsBound struct {
	t     *testing.T
	set   *listener.Set
	plan  *listener.Plan
	admin *listener.AdminPort
	ev    *events
}

func (b *partsBound) Listen(context.Context) error {
	if err := b.plan.Commit(); err != nil { //nolint:contextcheck // Plan.Commit shuts replaced Servers down in the background, past the handover.
		return err
	}
	ln, err := b.admin.Listen()
	if err != nil {
		return err
	}
	serveAdmin(b.t, ln, "successor-admin", b.ev)
	return nil
}

func (b *partsBound) Close() error {
	b.set.StopAccepting()
	return b.admin.Close()
}

// partsNode is one ruralzd process of the real-parts tests.
type partsNode struct {
	set   *listener.Set
	conns *admission.ConnLimiter
	admin *listener.AdminPort
	drain *drain.Drainer
	lock  *Lock
	done  chan int // the Drain's exit code
}

// newPartsNode builds a process's listeners, serving handler, admin port,
// Drain and Lock; a holder (steer) listens on fresh ports, a new process
// binds the holder's ports without listening.
func newPartsNode(t *testing.T, clk *clocktest.Fake, ev *events, handler http.Handler, steer bool, webPort, adminPort int) (*partsNode, *listener.Plan) {
	t.Helper()
	ctx := t.Context()
	n := &partsNode{conns: admission.NewConnLimiter(admission.DefaultConnections), lock: NewLock(), done: make(chan int, 1)}
	set, err := listener.New(listener.Options{Handler: handler, Conns: n.conns, Clock: clk, Hosts: []string{"127.0.0.1"}, Steer: steer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	n.set = set
	plan, err := set.Prepare(ctx, &snapshot.Snapshot{
		Listeners: []*snapshot.Listener{{Name: "web", Protocol: v1alpha1.ListenerProtocolHTTP, Port: webPort}},
		Gateway:   snapshot.Gateway{AdminPort: 9901},
	}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	n.admin, err = listener.BindAdminPort(ctx, listener.AdminPortOptions{Host: "127.0.0.1", Port: adminPort, Steer: steer})
	if err != nil {
		t.Fatalf("BindAdminPort: %v", err)
	}
	t.Cleanup(func() { _ = n.admin.Close() })
	n.drain, err = drain.New(drain.Config{
		Readiness: partsReadiness{ev: ev},
		Listeners: set,
		Retirer:   partsRetirer{},
		Lock:      n.lock,
		Clock:     clk,
		Rand:      rand.NewPCG(1, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { n.done <- n.drain.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-n.done:
		case <-time.After(bound):
			t.Error("a Drain did not end")
		}
	})
	return n, plan
}

// portOf returns the port of a.
func portOf(a net.Addr) int { return a.(*net.TCPAddr).Port }

// partsRig is a holder serving on real sockets and a new process bound
// beside it.
type partsRig struct {
	t          *testing.T
	clk        *clocktest.Fake
	dir        *nodedir.Dir
	ev         *events
	holder     *partsNode
	holderWeb  *holdingHandler
	holderSrv  *Holder
	acts       *fakeActivations
	successor  *partsNode
	succ       *Successor
	notifier   *fakeNotifier
	mem        *fakeMemory
	webAddr    string
	adminAddr  string
	keepAlive  *keepAlive
	holderPage *httpAdmin
	// kept counts the client connections the test keeps open on the
	// holder: the keep-alive connection, and each connection hold keeps
	// until its release. Only the test's goroutine touches it.
	kept int64
}

func newPartsRig(t *testing.T) *partsRig {
	t.Helper()
	ctx := t.Context()
	r := &partsRig{t: t, clk: newClock(), dir: dataDir(t), ev: &events{}, mem: &fakeMemory{limit: ownLimit}}
	r.notifier = &fakeNotifier{ev: r.ev}

	var plan *listener.Plan
	r.holderWeb = &holdingHandler{tag: "holder"}
	r.holder, plan = newPartsNode(t, r.clk, r.ev, r.holderWeb, true, 0, 0)
	if err := plan.Commit(); err != nil {
		t.Fatal(err)
	}
	if !r.holder.lock.Take(takeLock(t, r.dir)) {
		t.Fatal("Take refused the lock")
	}
	r.webAddr = r.holder.set.Addr("web").String()
	r.adminAddr = r.holder.admin.Addr().String()
	ln, err := r.holder.admin.Listen()
	if err != nil {
		t.Fatal(err)
	}
	r.holderPage = serveAdmin(t, ln, "holder-admin", r.ev)
	r.acts = &fakeActivations{ev: r.ev, revs: Revisions{Active: digestA, Candidate: digestA}}
	r.holderSrv, err = StartHolder(ctx, HolderConfig{
		Dir:         r.dir,
		Lock:        r.holder.lock,
		Activations: r.acts,
		Drain:       r.holder.drain,
		Sockets:     []Sockets{r.holder.set, r.holder.admin},
		Admin:       r.holderPage,
		Meter:       Meter{Conns: r.holder.conns, RSS: func() (int64, error) { return 1 << 30, nil }},
		Watchdog:    partsWatchdog,
		Clock:       r.clk,
	})
	if err != nil {
		t.Fatalf("StartHolder: %v", err)
	}
	t.Cleanup(func() { _ = r.holderSrv.Close() })

	// One keep-alive connection the holder serves through the handover.
	r.keepAlive = dialKeepAlive(t, r.webAddr)
	if got := r.keepAlive.get(t); got != "holder" {
		t.Fatalf("keep-alive answer %q, want holder", got)
	}
	r.kept = 1
	waitFor(t, "the holder's one open connection", func() bool { return r.holder.conns.Open() == r.kept })

	var succPlan *listener.Plan
	r.successor, succPlan = newPartsNode(t, r.clk, r.ev, tagHandler("successor"), false, portOf(r.holder.set.Addr("web")), portOf(r.holder.admin.Addr()))
	r.succ, err = NewSuccessor(SuccessorConfig{
		Dir:      r.dir,
		Lock:     r.successor.lock,
		Drain:    r.successor.drain,
		Start:    r.clk.Now(),
		Version:  "0.2.0-test",
		PID:      successorPID,
		Active:   func() revision.Digest { return digestA },
		Reload:   func(context.Context, revision.Digest) error { return errors.New("no reload in this test") },
		Bound:    &partsBound{t: t, set: r.successor.set, plan: succPlan, admin: r.successor.admin, ev: r.ev},
		Pinners:  []Pinner{r.successor.set, r.successor.admin},
		Record:   testRecord(t, successorPID),
		Notifier: r.notifier,
		Ceilings: Ceilings{Conns: r.successor.conns, SetMemoryLimit: r.mem.SetMemoryLimit, MemoryMax: r.mem.MemoryMax},
		Clock:    r.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.succ.Close() })
	return r
}

// run starts the new process's handover under ctx once the holder serves
// only the connections the test keeps open on it. The holder's server
// closes a routing check's fresh connection after its client read the
// answer, and counts it until then: a handover started at once can be
// accepted with that connection in the holder's usage.
func (r *partsRig) run(ctx context.Context) <-chan runResult {
	r.t.Helper()
	waitFor(r.t, "the holder serving only the connections the test keeps open", func() bool {
		return r.holder.conns.Open() == r.kept
	})
	ch := make(chan runResult, 1)
	go func() {
		out, err := r.succ.Run(ctx)
		ch <- runResult{out, err}
	}()
	return ch
}

// awaitSteering waits until the holder steered every group to the new
// process and armed its linger. The clock stands still from the
// handover's start until the test advances it, so the linger ends Linger
// after the time read here; the holder arms it after steering, and an
// advance before that would leave it armed from the advanced time.
func (r *partsRig) awaitSteering() {
	r.t.Helper()
	waitFor(r.t, "the steering", func() bool { return r.ev.count("admin:keepalives off") == 1 })
	ctx, cancel := context.WithTimeout(r.t.Context(), bound)
	defer cancel()
	if err := r.clk.BlockUntilDue(ctx, r.clk.Now().Add(Linger)); err != nil {
		r.t.Fatalf("the holder's linger is not armed: %v", err)
	}
}

// hold makes the holder keep one connection open past its client's end: a
// request on a fresh connection is answered in full while the holder's
// handler stays in it, so the client reads the answer and closes, and the
// holder serves the connection, and counts it, until release. release
// lets the handler return and waits until the holder closed the
// connection.
func (r *partsRig) hold() (release func()) {
	r.t.Helper()
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	r.t.Cleanup(open)
	r.holderWeb.arm(gate)
	if got, err := fetch(r.t.Context(), freshClient(), r.webAddr); err != nil || got != "holder" {
		r.t.Fatalf("held connection's answer %q, %v, want holder", got, err)
	}
	r.kept++
	waitFor(r.t, "the holder serving the held connection", func() bool { return r.holder.conns.Open() == r.kept })
	return func() {
		r.t.Helper()
		open()
		r.kept--
		waitFor(r.t, "the holder's close of the held connection", func() bool { return r.holder.conns.Open() == r.kept })
	}
}

// TestHandoverOverRealParts runs a whole handover between two processes'
// real listeners, admin ports, Drains and locks on one port pair: before
// it every new connection reaches the holder; once accepted the new
// process listens yet receives nothing until listening steers every
// group, admin port included, to it; it admits its ceiling minus the
// holder's connection; at the linger's end the holder stops accepting and
// its handover Drain releases the lock, which the new process takes,
// pinning its groups, writing holder.json and sending MAINPID=, with the
// holder's watchdog interval from accepted; the holder's keep-alive
// connection keeps being served by the holder, and once the holder is gone
// the new process's ceiling is whole again.
func TestHandoverOverRealParts(t *testing.T) {
	r := newPartsRig(t)
	if got := routes(t, r.webAddr); got["holder"] != fresh {
		t.Fatalf("before the handover: %v, want every connection on the holder", got)
	}
	if got := routes(t, r.adminAddr); got["holder-admin"] != fresh {
		t.Fatalf("admin before the handover: %v, want every connection on the holder", got)
	}
	done := r.run(t.Context())
	r.awaitSteering()
	if got := r.successor.conns.Ceiling(); got != admission.DefaultConnections-1 {
		t.Errorf("new process's connection ceiling %d, want %d minus the holder's connection", got, admission.DefaultConnections)
	}
	if got := routes(t, r.webAddr); got["successor"] != fresh {
		t.Fatalf("steered: %v, want every connection on the new process", got)
	}
	if got := routes(t, r.adminAddr); got["successor-admin"] != fresh {
		t.Fatalf("admin steered: %v, want every connection on the new process", got)
	}

	r.clk.Advance(Linger)
	res := recv(t, "the new process's handover", done)
	if res.out != OutcomeHolder || res.err != nil {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	select {
	case <-r.holder.drain.Started():
	default:
		t.Fatal("the holder's Drain did not start")
	}
	if h, err := r.dir.ReadHolder(); err != nil || h.PID != successorPID {
		t.Errorf("holder.json = %+v, %v, want PID %d", h, err, successorPID)
	}
	if r.ev.count("notify:mainpid "+strconv.Itoa(successorPID)) != 1 {
		t.Errorf("events %v, want MAINPID= once", r.ev.all())
	}
	if r.successor.lock.Held() == nil {
		t.Error("the new process's Lock does not hold the lock")
	}
	if got := r.succ.HolderWatchdog(); got != partsWatchdog {
		t.Errorf("the new process's HolderWatchdog() = %v, want the holder's %v", got, partsWatchdog)
	}
	if got := routes(t, r.webAddr); got["successor"] != fresh {
		t.Fatalf("after the handover: %v, want every connection on the new process", got)
	}
	if got := routes(t, r.adminAddr); got["successor-admin"] != fresh {
		t.Fatalf("admin after the handover: %v, want every connection on the new process", got)
	}
	if got := r.keepAlive.get(t); got != "holder" {
		t.Errorf("the holder's keep-alive connection answered %q, want holder until its wave", got)
	}

	if err := r.holderSrv.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ceilings restored", func() bool {
		return r.successor.conns.Ceiling() == admission.DefaultConnections && r.mem.current() == ownLimit
	})
}

// TestHandoverCeilingCountsHeldConnections runs a handover accepted while
// the holder still serves a connection whose client already read the
// whole answer and closed, as the holder's server keeps every connection
// until its handler returned: the new process's ceiling is lowered by
// both of the holder's connections, the keep-alive and that one, and stays
// so after the holder closed it until the next usage report, UsageInterval
// after accepted, gives that one back; once the holder is gone the ceiling
// is whole again.
func TestHandoverCeilingCountsHeldConnections(t *testing.T) {
	r := newPartsRig(t)
	release := r.hold()
	done := r.run(t.Context())
	r.awaitSteering()
	if got := r.successor.conns.Ceiling(); got != admission.DefaultConnections-2 {
		t.Fatalf("new process's connection ceiling %d, want %d minus the holder's keep-alive and held connections", got, admission.DefaultConnections)
	}
	release()
	if got := r.successor.conns.Ceiling(); got != admission.DefaultConnections-2 {
		t.Fatalf("connection ceiling %d once the holder closed the held connection, want %d until the next usage report", got, admission.DefaultConnections-2)
	}
	r.clk.Advance(UsageInterval)
	waitFor(t, "the usage report without the held connection", func() bool {
		return r.successor.conns.Ceiling() == admission.DefaultConnections-1
	})

	r.clk.Advance(Linger - UsageInterval)
	if res := recv(t, "the new process's handover", done); res.out != OutcomeHolder || res.err != nil {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	if err := r.holderSrv.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ceiling restored", func() bool { return r.successor.conns.Ceiling() == admission.DefaultConnections })
}

// TestHandoverLostSuccessorOverRealParts runs a new process lost during
// the linger: the holder re-pins its groups, so every new connection
// reaches it although the lost process's sockets still listen, and skips
// its Drain; a stop signal the holder received during the linger then
// starts the stop-signal Drain.
func TestHandoverLostSuccessorOverRealParts(t *testing.T) {
	for _, signaled := range []bool{false, true} {
		name := "no signal"
		if signaled {
			name = "signal during the linger"
		}
		t.Run(name, func(t *testing.T) {
			r := newPartsRig(t)
			ctx, cancel := context.WithCancel(t.Context())
			done := r.run(ctx)
			r.awaitSteering()
			if signaled {
				r.holder.drain.Signal()
			}
			cancel() // the new process ends its handover and closes the connection
			if res := recv(t, "the new process's handover", done); res.out != OutcomeStopped {
				t.Fatalf("Run = %v, %v, want stopped", res.out, res.err)
			}
			waitFor(t, "the re-pin", func() bool { return r.ev.count("admin:keepalives on") == 1 })
			if got := routes(t, r.webAddr); got["holder"] != fresh {
				t.Fatalf("re-pinned: %v, want every connection on the holder", got)
			}
			if got := routes(t, r.adminAddr); got["holder-admin"] != fresh {
				t.Fatalf("admin re-pinned: %v, want every connection on the holder", got)
			}
			started := func() bool {
				select {
				case <-r.holder.drain.Started():
					return true
				default:
					return false
				}
			}
			if signaled {
				waitFor(t, "the stop-signal Drain", started)
				return
			}
			r.clk.Advance(Linger)
			if started() {
				t.Fatal("the holder drained after its successor was lost")
			}
			// The holder releases its activations after the re-pin the
			// test waited for.
			waitFor(t, "the holder's activations released", func() bool { return !r.acts.isHeld() })
		})
	}
}
