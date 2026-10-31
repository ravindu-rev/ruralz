// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// Tests for the activation of docs/architecture/03-data-plane.md
// "Activation": its order, the Hot Reload of a later Revision, the
// pending Revision, metrics, logs and the candidate write.

func TestBootActivatesTheSource(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	a := rev("A")
	if got := r.active(); got != a.Digest {
		t.Fatalf("published %s, want A", got.Short())
	}
	if !r.ready.Ready() || r.ready.Readyz().Revision != a.Digest.Short() {
		t.Fatalf("/readyz = %+v, want ready with A", r.ready.Readyz())
	}
	if c := r.comp.last(); c.activated.Load() != 1 || c.discarded.Load() != 0 {
		t.Fatalf("Activate %d, Discard %d; want 1 and 0", c.activated.Load(), c.discarded.Load())
	}
	if n := r.metrics.activations(); n != 1 {
		t.Errorf("ruralz_config_activations_total{result=activated} = %d, want 1", n)
	}
	if got := r.metrics.revisionInfo("active"); got != a.Digest.Short() {
		t.Errorf("revision info active = %q, want %s", got, a.Digest.Short())
	}
	// The candidate, promoted in file mode.
	cand, last, err := r.store.Pointers()
	if err != nil || cand != a.Digest || last != a.Digest {
		t.Fatalf("lkg/ names %s and %s (%v), want A twice", cand.Short(), last.Short(), err)
	}
	if got := r.metrics.revisionInfo("lkg"); got != a.Digest.Short() {
		t.Errorf("revision info lkg = %q", got)
	}
	recs := r.logs.find("revision activated")
	if len(recs) != 1 || recs[0].attrs["origin"] != "source" || recs[0].attrs["revision"] != a.Digest.Short() {
		t.Errorf("activation records %+v", recs)
	}
	if r.notifier.n.Load() == 0 {
		t.Error("STATUS= was not synced after the readiness change")
	}
}

func TestActivationOrder(t *testing.T) {
	// Compile, ready every Part before the swap, publish, switch the Node,
	// commit every Part after the swap, then the candidate.
	r := newRig(t)
	r.start()
	r.booted()
	a := rev("A").Digest.Short()
	want := []string{
		"compile:" + a, "prepare:listeners:" + a, "prepare:admin:" + a,
		"activate:" + a, "commit:listeners:" + a, "commit:admin:" + a,
	}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for _, p := range r.parts {
		p.mu.Lock()
		published := slices.Clone(p.published)
		p.mu.Unlock()
		if !slices.Equal(published, []bool{false, true}) {
			t.Errorf("%s saw the snapshot published %v at Prepare and Commit, want [false true]", p.name, published)
		}
	}
}

func TestHotReload(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	first := r.holder.Active()
	r.l.Offer(load(result("B")))
	r.waitActive("B")
	calls := r.comp.calls()
	if len(calls) != 2 || calls[1].Previous != first {
		t.Fatalf("compile inputs %d, Previous %p; want the active snapshot %p as Previous", len(calls), calls[1].Previous, first)
	}
	if got := r.holder.Counts(); !got.Active {
		t.Fatalf("Counts = %+v", got)
	}
	b := rev("B").Digest
	if cand, last, _ := r.store.Pointers(); cand != b || last != b {
		t.Fatalf("lkg/ names %s and %s, want B", cand.Short(), last.Short())
	}
	if r.ready.Readyz().Revision != b.Short() {
		t.Fatalf("/readyz revision %q, want B", r.ready.Readyz().Revision)
	}
	if n := r.metrics.activations(); n != 2 {
		t.Fatalf("activations = %d, want 2", n)
	}
}

// TestHeaderLimitCappedWhileActive checks degraded reason
// header_limit_capped (docs/architecture/03-data-plane.md "Listeners and
// protocols"): raised while the active Revision's
// limits.maxRequestHeaderBytes exceeds the 256 KiB process ceiling, which
// caps it, cleared once a Revision within the ceiling activates, and
// raised again by the next capped one.
func TestHeaderLimitCappedWhileActive(t *testing.T) {
	r := newRig(t)
	capped := func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		c := r.comp.make(in)
		c.snap.Gateway.Limits.HeaderLimitCapped = true
		return c, nil, nil
	}
	r.comp.script(rev("A").Digest, capped)
	r.start()
	r.booted()
	if !r.status.raised("header_limit_capped") {
		t.Fatal("header_limit_capped is not raised while a capped Revision is active")
	}
	r.l.Offer(load(result("B")))
	r.waitActive("B")
	if r.status.raised("header_limit_capped") {
		t.Fatal("header_limit_capped stays raised after a Revision within the ceiling activated")
	}
	r.comp.script(rev("C").Digest, capped)
	r.l.Offer(load(result("C")))
	r.waitActive("C")
	if !r.status.raised("header_limit_capped") {
		t.Fatal("header_limit_capped is not raised by the next capped Revision")
	}
}

func TestSameRevisionActivatesNothing(t *testing.T) {
	// A touched or reformatted file renders the active digest: nothing
	// activates, counts or logs above debug.
	r := newRig(t)
	r.start()
	r.booted()
	before := len(r.comp.calls())
	r.l.Offer(load(result("A")))
	r.idle()
	if n := len(r.comp.calls()); n != before {
		t.Fatalf("compiled %d times after a load of the active Revision, want %d", n, before)
	}
	if n := r.metrics.activations(); n != 1 {
		t.Fatalf("activations = %d, want 1", n)
	}
	if n := r.metrics.totalRejections(); n != 0 {
		t.Fatalf("rejections = %d", n)
	}
	recs := r.logs.find("renders the active revision")
	if len(recs) != 1 || recs[0].level != -4 {
		t.Fatalf("records %+v, want one debug record", recs)
	}
}

func TestPendingKeepsOnlyTheLatest(t *testing.T) {
	// With K = 1, the third snapshot makes the first closing; while it is
	// pinned, the retirer accepts nothing new, and only the latest pending
	// Revision activates once it is freed.
	r := newRig(t, withRetire(retire.Config{K: 1, Stripes: 1}))
	r.start()
	r.booted()
	pins := []*snapshot.PinnedRequest{}
	pin := func() *snapshot.Snapshot {
		rec := &snapshot.PinnedRequest{Cancel: func() {}}
		s := r.holder.Pin(0, rec)
		pins = append(pins, rec)
		return s
	}
	a := pin()
	r.l.Offer(load(result("B")))
	r.waitActive("B")
	pin()
	r.l.Offer(load(result("C")))
	r.waitActive("C")
	pin()
	if c := r.holder.Counts(); c.Retired != 1 || c.Closing != 1 {
		t.Fatalf("Counts = %+v, want one retired and one closing", c)
	}
	r.l.Offer(load(result("D")))
	r.l.Offer(load(result("E")))
	waitFor(t, "E to be pending", func() bool {
		p := r.l.Snapshots().Pending
		return p != nil && *p == rev("E").Digest.String()
	})
	if r.active() != rev("C").Digest {
		t.Fatal("a pending Revision activated while the closing snapshot is pinned")
	}
	// Freeing the closing snapshot lets the latest pending one activate.
	r.holder.Unpin(a, 0, pins[0])
	r.waitActive("E")
	for _, in := range r.comp.calls() {
		if in.Validated.Revision.Digest == rev("D").Digest {
			t.Fatal("the replaced pending Revision D was compiled")
		}
	}
	if p := r.l.Snapshots().Pending; p != nil {
		t.Fatalf("pending = %s after the activation", *p)
	}
}

func TestActivationMetrics(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		r.clk.Advance(400 * time.Millisecond)
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	routes := make([]string, 1001)
	for i := range routes {
		routes[i] = "r" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
	}
	ld := load(result("B", routes...))
	ld.Spent = 100 * time.Millisecond
	r.l.Offer(ld)
	r.waitActive("B")
	// A watched directory is unsigned: no verify stage.
	if got := r.metrics.stage("verify/le10000"); len(got) != 0 {
		t.Errorf("verify recorded %v for a source load", got)
	}
	if got := r.metrics.stage("compile/le10000"); len(got) != 1 || got[0] != 500*time.Millisecond {
		t.Errorf("compile = %v, want the load's 100ms plus the compile's 400ms", got)
	}
	if got := r.metrics.stage("total/le10000"); len(got) != 1 || got[0] != 500*time.Millisecond {
		t.Errorf("total = %v, want 500ms", got)
	}
	if got := r.metrics.stage("swap/le10000"); len(got) != 1 {
		t.Errorf("swap = %v, want one sample", got)
	}
	if got := r.metrics.stage("total/le1000"); len(got) != 1 {
		t.Errorf("the boot's total/le1000 = %v, want one sample", got)
	}
}

func TestSizeClass(t *testing.T) {
	for n, want := range map[int]string{0: "le1000", 1000: "le1000", 1001: "le10000", 10000: "le10000", 10001: "gt10000"} {
		if got := sizeClass(n); got != want {
			t.Errorf("sizeClass(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestActivationWarningsAreLogged(t *testing.T) {
	r := newRig(t)
	r.comp.warnings = []compile.Warning{{Resource: &diag.ResourceID{Kind: "Policy", Name: "cache"}, Message: "cache and limit Policies share one State Store"}}
	res := result("A")
	res.Diagnostics = diag.List{{Code: "RZ-CFG-043", Severity: diag.SeverityWarning, Message: "a hot limit"}}
	r.src = newFakeSource(load(res))
	r.cfg.Source = r.src
	var err error
	if r.l, err = New(r.cfg); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.booted()
	if recs := r.logs.find("activated revision compile warning"); len(recs) != 1 ||
		recs[0].attrs["warning"] != "cache and limit Policies share one State Store" || recs[0].attrs["resource_name"] != "cache" {
		t.Errorf("compile warning records %+v", recs)
	}
	if recs := r.logs.find("activated revision has warnings"); len(recs) != 1 || recs[0].attrs["diagnostics"] != int64(1) {
		t.Errorf("pipeline warning records %+v", recs)
	}
}

// TestActivationWarningRecordsCarryEveryWarning checks the records of an
// activation with more warnings than one record holds: up to ten in the
// warn record, the rest at debug one per record, each a diag.Diagnostic
// the sink writes in its JSON form, so together they hold every warning
// the configuration pipeline reported, in order.
func TestActivationWarningRecordsCarryEveryWarning(t *testing.T) {
	r := newRig(t)
	res := result("A")
	for i := range 12 {
		res.Diagnostics = append(res.Diagnostics, diag.Diagnostic{
			Code: "RZ-CFG-013", Severity: diag.SeverityWarning, Message: "a secret-like variable " + strconv.Itoa(i),
			Location: diag.Location{File: "ruralz.yaml", Line: i + 1, Column: 3},
		})
	}
	r.src = newFakeSource(load(res))
	r.cfg.Source = r.src
	var err error
	if r.l, err = New(r.cfg); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.booted()
	recs := r.logs.find("activated revision")
	var warnings []record
	for _, rec := range recs {
		if rec.msg != "revision activated" {
			warnings = append(warnings, rec)
		}
	}
	if len(warnings) != 3 || warnings[0].level != slog.LevelWarn || warnings[1].level != slog.LevelDebug {
		t.Fatalf("records %+v, want one warn record and two debug ones", warnings)
	}
	if got, n := recordedFindings(t, warnings); n != 12 || !reflect.DeepEqual(got, res.Diagnostics) {
		t.Errorf("the records carry %d of %d warnings:\n%+v\nwant\n%+v", len(got), n, got, res.Diagnostics)
	}
}

func TestFileDescriptorWarning(t *testing.T) {
	r := newRig(t)
	r.fdLimit.Store(4096)
	r.comp.conns = 100
	r.start()
	r.booted()
	recs := r.logs.find("file descriptor limit")
	if len(recs) != 2 {
		t.Fatalf("records %d, want one at start and one at the activation", len(recs))
	}
	if recs[0].attrs["needed"] != "21280" || recs[1].attrs["needed"] != "21380" || recs[1].attrs["limit"] != "4096" {
		t.Errorf("records %+v", recs)
	}
	if got := FileDescriptorsNeeded(0); got != 21280 {
		t.Errorf("FileDescriptorsNeeded(0) = %d, want 20,256 + 1,024", got)
	}
}

func TestNoFileDescriptorWarningAboveTheNeed(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	if recs := r.logs.find("file descriptor limit"); len(recs) != 0 {
		t.Fatalf("records %+v", recs)
	}
}

func TestCommitFailureLeavesListenerUnbound(t *testing.T) {
	// A socket that fails to listen after the swap: the Revision is
	// active, and /readyz names the listener unbound.
	r := newRig(t)
	r.parts[0].set(func(p *fakePart) {
		p.commitErr = bindError("listener \"web\": listen: address in use")
	})
	r.start()
	r.booted()
	if r.active() != rev("A").Digest {
		t.Fatal("the Revision did not activate")
	}
	want := []string{"listeners_unbound/RZ-CFG-039"}
	if got := r.reasons(); !slices.Equal(got, want) {
		t.Fatalf("reasons %v, want %v", got, want)
	}
	if recs := r.logs.find("did not switch"); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
}

func TestRetirerClosedEndsActivations(t *testing.T) {
	r := newRig(t, withRetire(retire.Config{K: 1, Stripes: 1}))
	r.start()
	r.booted()
	if err := r.holder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.l.Offer(load(result("B")))
	waitFor(t, "the loader to see the retirer closed", func() bool {
		r.l.mu.Lock()
		defer r.l.mu.Unlock()
		return r.l.closed
	})
	r.idle()
	if r.l.Active() != rev("A").Digest {
		t.Fatal("a Revision activated after the retirer closed")
	}
	for _, in := range r.comp.calls() {
		if in.Validated.Revision.Digest == rev("B").Digest {
			t.Fatal("B was compiled after the retirer closed")
		}
	}
}

func TestPublishOnClosedRetirerIsQuiet(t *testing.T) {
	// The Drain deadline closes the retirer while an activation compiles:
	// the snapshot is discarded and nothing is rejected.
	r := newRig(t)
	release := make(chan struct{})
	r.comp.script(rev("B").Digest, func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		<-release
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	if err := r.holder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	r.idle()
	if c := r.comp.last(); c.discarded.Load() != 1 {
		t.Fatal("the unpublished snapshot was not discarded")
	}
	if n := r.metrics.totalRejections(); n != 0 {
		t.Fatalf("rejections = %d", n)
	}
}

func TestRunTwice(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	if err := r.l.Run(t.Context()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run = %v, want ErrRunning", err)
	}
}

func TestNewValidates(t *testing.T) {
	r := newRig(t)
	for name, edit := range map[string]func(c *Config){
		"no boot":       func(c *Config) { c.Boot = 0 },
		"no source":     func(c *Config) { c.Source = nil },
		"no pipeline":   func(c *Config) { c.Pipeline = nil },
		"no compiler":   func(c *Config) { c.Compiler = nil },
		"no holder":     func(c *Config) { c.Holder = nil },
		"no readiness":  func(c *Config) { c.Readiness = nil },
		"no LKG":        func(c *Config) { c.LKG = nil },
		"no request ID": func(c *Config) { c.RequestID = nil },
		"nil part":      func(c *Config) { c.Parts = []Part{nil} },
		"deadline":      func(c *Config) { c.Deadline = -1 },
	} {
		cfg := r.cfg
		edit(&cfg)
		if _, err := New(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("New(%s) = %v, want ErrConfig", name, err)
		}
	}
}

func TestOriginNames(t *testing.T) {
	for o, want := range map[Origin]string{
		OriginSource: "source", OriginLastKnownGood: "last_known_good", OriginCandidate: "candidate", 0: "none",
	} {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", o, got, want)
		}
	}
}

// TestLKGEntryDigest checks the rig's Last-Known-Good helper writes what
// the Store reads, the base of the boot tests.
func TestLKGEntryDigest(t *testing.T) {
	r := newRig(t)
	r.writeLKG("L")
	e, err := r.store.LastKnownGood()
	if err != nil || e.Digest != rev("L").Digest {
		t.Fatalf("LastKnownGood = %+v, %v", e, err)
	}
	if _, err := r.store.Candidate(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.store.Write(nil, rev("M"), true), lkg.ErrNotHolder) {
		t.Fatal("a write without the lock")
	}
}
