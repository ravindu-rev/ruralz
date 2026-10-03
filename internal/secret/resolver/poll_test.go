// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for rotation: spec 01 requirement 46 (2 s poll, re-read on
// change of size, modification time or inode, a failed check keeps the
// last value and counts ruralz_config_secret_rotation_failures_total,
// env never rotates), spec 06 requirements 13, 51 and 76 (rotation
// failures raise secret_rotation_failed), architecture 2.8 and R-55
// (Node-wide watches that survive Activate).

// recorder is a watcher recording delivered values.
type recorder struct {
	mu   sync.Mutex
	got  []string
	fail func(string) error
}

func (w *recorder) fn(v secret.Value) error {
	s := string(v.Reveal())
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		if err := w.fail(s); err != nil {
			return err
		}
	}
	w.got = append(w.got, s)
	return nil
}

func (w *recorder) values() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.got)
}

func TestRotation(t *testing.T) {
	// Req 46: a changed file with a valid value replaces the value; every
	// Store holding the reference sees it; watchers get it once.
	h := newHarness(t, nil)
	p := newPEM(t)
	h.write("tls/tls.crt", p.cert)
	ref := fileRef(h.path("tls/tls.crt"), "")
	st := h.resolve(use(ref, secret.KindPEMCertificate))
	h.r.Activate(st)
	w := &recorder{}
	defer st.Watch(ref, w.fn)()

	h.cycle() // unchanged: nothing delivered
	if got := w.values(); len(got) != 0 {
		t.Fatalf("delivered without a change: %d values", len(got))
	}
	next := newPEM(t)
	h.write("tls/tls.crt", next.cert)
	h.cycle()
	if got := get(t, st, ref); got != next.cert {
		t.Error("Get does not return the rotated value")
	}
	if got := w.values(); len(got) != 1 || got[0] != next.cert {
		t.Errorf("watcher got %d values", len(got))
	}
	h.cycle()
	if got := w.values(); len(got) != 1 {
		t.Errorf("value delivered twice: %d", len(got))
	}
	if h.fileN.value() != 0 || len(h.status.raised()) != 0 {
		t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
	}
	if !strings.Contains(h.logs.String(), msgRotated) {
		t.Error("rotation not logged")
	}
}

func TestRotationFailureKeepsLastValue(t *testing.T) {
	// Req 46 and spec 06 req 76: an invalid new value keeps the last value,
	// counts once per distinct file state, logs a warning with provider
	// and reference, and raises secret_rotation_failed until a good value.
	h := newHarness(t, nil)
	p := newPEM(t)
	h.write("tls.crt", p.cert)
	ref := fileRef(h.path("tls.crt"), "")
	st := h.resolve(use(ref, secret.KindPEMCertificate))
	h.r.Activate(st)
	w := &recorder{}
	defer st.Watch(ref, w.fn)()

	h.write("tls.crt", "garbage")
	h.cycle()
	h.cycle()
	h.cycle()
	if got := get(t, st, ref); got != p.cert {
		t.Error("invalid value replaced the last one")
	}
	if n := h.fileN.value(); n != 1 {
		t.Errorf("failures = %d, want 1", n)
	}
	if n := h.envN.value(); n != 0 {
		t.Errorf("env failures = %d", n)
	}
	if got := h.status.raised(); !slices.Equal(got, []string{"secret " + ref.String()}) {
		t.Errorf("raised = %v", got)
	}
	logs := h.logs.String()
	for _, want := range []string{msgRotationFailed, `"provider":"file"`, `"reference":"` + ref.String() + `"`, errNoCertificate.Error()} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	// A second distinct bad state counts again.
	h.write("tls.crt", "garbage 2")
	h.cycle()
	if n := h.fileN.value(); n != 2 {
		t.Errorf("failures = %d, want 2", n)
	}
	// A good value clears the reason and reaches watchers.
	next := newPEM(t)
	h.write("tls.crt", next.cert)
	h.cycle()
	if got := get(t, st, ref); got != next.cert {
		t.Error("good value not applied")
	}
	if got := h.status.raised(); len(got) != 0 {
		t.Errorf("raised = %v", got)
	}
	if got := w.values(); len(got) != 1 || got[0] != next.cert {
		t.Errorf("watcher got %d values", len(got))
	}
}

func TestRotationUsesActiveChecks(t *testing.T) {
	// Spec 06 req 13: rotation to a short API key keeps the old value.
	h := newHarness(t, nil)
	key := strings.Repeat("k", 30)
	h.write("api.key", key)
	ref := fileRef(h.path("api.key"), "")
	h.r.Activate(h.resolve(use(ref, secret.KindAPIKey)))
	h.write("api.key", "short\n")
	h.cycle()
	if got := get(t, h.r.Current(), ref); got != key {
		t.Errorf("value = %q", got)
	}
	if h.fileN.value() != 1 {
		t.Errorf("failures = %d", h.fileN.value())
	}
	// A consumer check of the active Store runs too; a reason repeating the
	// value is withheld in the log.
	h.write("url", "redis://u:p@cache.internal:6379")
	uref := fileRef(h.path("url"), "")
	u := use(uref, secret.KindStateStoreURL)
	u.Check = func(b []byte) error {
		if strings.Contains(string(b), "cluster") {
			return errors.New("URL " + string(b) + " does not fit topology")
		}
		return nil
	}
	h.r.Activate(h.resolve(u))
	h.write("url", "redis://u:p@cluster.internal:6379")
	h.cycle()
	logs := h.logs.String()
	if strings.Contains(logs, "cluster.internal") || !strings.Contains(logs, errWithheld.Error()) {
		t.Errorf("consumer reason not withheld:\n%s", logs)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	// A deleted file keeps the value and counts once; restoring the same
	// content clears the reason without a new version.
	h := newHarness(t, nil)
	h.write("s", "v1")
	ref := fileRef(h.path("s"), "")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	w := &recorder{}
	defer st.Watch(ref, w.fn)()
	if err := os.Remove(filepath.Join(h.root, "s")); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	h.cycle()
	if h.fileN.value() != 1 || len(h.status.raised()) != 1 {
		t.Fatalf("failures %d raised %v", h.fileN.value(), h.status.raised())
	}
	if got := get(t, st, ref); got != "v1" {
		t.Errorf("value = %q", got)
	}
	h.write("s", "v1")
	h.cycle()
	if len(h.status.raised()) != 0 {
		t.Errorf("raised = %v", h.status.raised())
	}
	if len(w.values()) != 0 {
		t.Errorf("same content delivered: %v", w.values())
	}
}

func TestChangeDetection(t *testing.T) {
	// Req 46: stat first; re-read on a change of size, modification time,
	// inode or mode; a read too close to the modification time is re-read
	// on the next poll even when the stat looks unchanged.
	h := newHarness(t, nil)
	h.write("s", "aaaa")
	ref := fileRef(h.path("s"), "")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	path := filepath.Join(h.root, "s")

	// Same size and modification time, rewritten in place: undetectable
	// by stat, and the last read was trusted, so the value stays.
	if err := os.WriteFile(path, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.touch("s", time.Unix(0, st.(*store).cells[ref].fp.mtime))
	h.cycle()
	if got := get(t, st, ref); got != "aaaa" {
		t.Fatalf("value = %q, want the trusted aaaa", got)
	}

	// A new inode (atomic rename) is detected.
	tmp := filepath.Join(h.root, "s.tmp")
	if err := os.WriteFile(tmp, []byte("cccc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, time.Unix(0, st.(*store).cells[ref].fp.mtime), time.Unix(0, st.(*store).cells[ref].fp.mtime)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if got := get(t, st, ref); got != "cccc" {
		t.Fatalf("value = %q after a rename", got)
	}

	// A racy read: the file changed at the fake now, so the next poll
	// re-reads even though size, time and inode stay the same.
	now := h.clock.Now()
	if err := os.WriteFile(path, []byte("dddd"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.touch("s", now)
	h.cycle()
	if got := get(t, st, ref); got != "dddd" {
		t.Fatalf("value = %q", got)
	}
	if !st.(*store).cells[ref].racy {
		t.Fatal("read at the modification time is trusted")
	}
	if err := os.WriteFile(path, []byte("eeee"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.touch("s", now)
	h.clock.Advance(DefaultPollInterval)
	h.cycle()
	if got := get(t, st, ref); got != "eeee" {
		t.Fatalf("racy change missed: %q", got)
	}
	if st.(*store).cells[ref].racy {
		t.Error("still racy 2 s after the change")
	}

	// A mode change re-reads and checks the mode.
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // G302: the test makes the file group-writable on purpose.
		t.Fatal(err)
	}
	h.cycle()
	if ownerModeChecked && h.fileN.value() != 1 {
		t.Errorf("group-writable file: failures = %d", h.fileN.value())
	}
}

func TestEnvNeverPolled(t *testing.T) {
	// Req 46: env never rotates (restart).
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_E", "e1")
	ref := envRef("RURALZ_SECRET_E")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	h.setEnv("RURALZ_SECRET_E", "e2")
	h.cycle()
	if got := get(t, st, ref); got != "e1" {
		t.Errorf("env rotated to %q", got)
	}
}

func TestSharedFileKeys(t *testing.T) {
	// Several keys of one file are polled with one read; only the changed
	// key's watchers fire.
	h := newHarness(t, nil)
	h.write("creds.json", `{"user":"u1","password":"p1"}`)
	user, pass := fileRef(h.path("creds.json"), "user"), fileRef(h.path("creds.json"), "password")
	st := h.resolve(use(user, secret.KindOpaque), use(pass, secret.KindOpaque))
	h.r.Activate(st)
	wu, wp := &recorder{}, &recorder{}
	defer st.Watch(user, wu.fn)()
	defer st.Watch(pass, wp.fn)()
	h.write("creds.json", `{"user":"u1","password":"p2"}`)
	h.cycle()
	if len(wu.values()) != 0 || !slices.Equal(wp.values(), []string{"p2"}) {
		t.Errorf("user %v password %v", wu.values(), wp.values())
	}
	// The key vanishes: that reference fails, the other stays healthy.
	h.write("creds.json", `{"user":"u2"}`)
	h.cycle()
	if got := get(t, st, pass); got != "p2" {
		t.Errorf("password = %q", got)
	}
	if got := get(t, st, user); got != "u2" {
		t.Errorf("user = %q", got)
	}
	if got := h.status.raised(); !slices.Equal(got, []string{"secret " + pass.String()}) {
		t.Errorf("raised = %v", got)
	}
}

func TestRootUnavailableDuringPoll(t *testing.T) {
	// The root disappearing fails every file reference once.
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) { c.Root = root })
	h.root = root
	h.write("a", "a")
	h.write("b", "b")
	st := h.resolve(use(fileRef(h.path("a"), ""), secret.KindOpaque), use(fileRef(h.path("b"), ""), secret.KindOpaque))
	h.r.Activate(st)
	if err := os.Rename(root, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	h.cycle()
	if h.fileN.value() != 2 || len(h.status.raised()) != 2 {
		t.Errorf("failures %d raised %v", h.fileN.value(), h.status.raised())
	}
	if err := os.Rename(filepath.Join(base, "moved"), root); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if len(h.status.raised()) != 0 {
		t.Errorf("raised = %v", h.status.raised())
	}
}

func TestActivateClearsReasonOfDroppedReference(t *testing.T) {
	h := newHarness(t, nil)
	h.write("a", "a")
	ref := fileRef(h.path("a"), "")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	if err := os.Remove(filepath.Join(h.root, "a")); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if len(h.status.raised()) != 1 {
		t.Fatalf("raised = %v", h.status.raised())
	}
	h.r.Activate(h.resolve())
	if len(h.status.raised()) != 0 {
		t.Errorf("raised after dropping the reference = %v", h.status.raised())
	}
	// Activating a Store holding the still-failing cell raises it again.
	h.r.Activate(st)
	if len(h.status.raised()) != 1 {
		t.Errorf("raised after reactivation = %v", h.status.raised())
	}
}

func TestWatchSurvivesActivate(t *testing.T) {
	// R-55 and WP-07 "Done when": a watch registered on a Store that is no
	// longer active receives rotations within one poll, driven by Run on
	// the 2 s timer.
	h := newHarness(t, nil)
	cycles := make(chan struct{}, 64)
	h.r.cycleHook = func() { cycles <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()

	h.write("tls.key", "k1")
	ref := fileRef(h.path("tls.key"), "")
	s1 := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(s1) // kicks a cycle
	waitCycle(t, cycles)
	carried := &recorder{} // a Filter built on s1 and carried over
	stop := s1.Watch(ref, carried.fn)
	defer stop()

	h.write("other", "o")
	s2 := h.resolve(use(ref, secret.KindOpaque), use(fileRef(h.path("other"), ""), secret.KindOpaque))
	h.r.Activate(s2) // kicks a cycle
	waitCycle(t, cycles)
	fresh := &recorder{} // a Filter built on s2
	defer s2.Watch(ref, fresh.fn)()

	h.write("tls.key", "k2")
	waitTimer(t, h)
	h.clock.Advance(DefaultPollInterval) // one poll
	waitCycle(t, cycles)
	if got := carried.values(); !slices.Equal(got, []string{"k2"}) {
		t.Errorf("watch on the inactive Store got %v, want [k2]", got)
	}
	if got := fresh.values(); !slices.Equal(got, []string{"k2"}) {
		t.Errorf("watch on the active Store got %v", got)
	}
	if got := get(t, s1, ref); got != "k2" {
		t.Errorf("inactive Store Get = %q", got)
	}

	// While no active Store holds the reference, registrations stay silent.
	h.r.Activate(h.resolve())
	waitCycle(t, cycles)
	h.write("tls.key", "k3")
	waitTimer(t, h)
	h.clock.Advance(DefaultPollInterval)
	waitCycle(t, cycles)
	if got := carried.values(); len(got) != 1 {
		t.Errorf("silent watch got %v", got)
	}
	// A later Store holding it again delivers the unseen value at once.
	s3 := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(s3)
	waitCycle(t, cycles)
	if got := carried.values(); !slices.Equal(got, []string{"k2", "k3"}) {
		t.Errorf("watch after reactivation got %v", got)
	}
	stop()
	h.write("tls.key", "k4")
	waitTimer(t, h)
	h.clock.Advance(DefaultPollInterval)
	waitCycle(t, cycles)
	if got := carried.values(); len(got) != 2 {
		t.Errorf("stopped watch got %v", got)
	}
}

func TestWatcherFailure(t *testing.T) {
	// Architecture 2.8: an fn error keeps the watcher's last value and
	// counts a rotation failure (spec 06 req 13: the degraded reason is
	// raised); a later accepted value or stop clears it.
	h := newHarness(t, nil)
	h.write("api.key", strings.Repeat("a", 30))
	ref := fileRef(h.path("api.key"), "")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	w := &recorder{fail: func(s string) error {
		if strings.HasPrefix(s, "bad") {
			return errors.New("rejected " + s)
		}
		return nil
	}}
	stop := st.Watch(ref, w.fn)
	h.write("api.key", "bad-"+strings.Repeat("b", 30))
	h.cycle()
	h.cycle() // the same version is not delivered twice
	if n := h.fileN.value(); n != 1 {
		t.Errorf("failures = %d", n)
	}
	raised := h.status.raised()
	if len(raised) != 1 || !strings.Contains(raised[0], " watch ") {
		t.Errorf("raised = %v", raised)
	}
	if logs := h.logs.String(); !strings.Contains(logs, msgWatcherFailed) || strings.Contains(logs, "bbbbbbbb") {
		t.Errorf("log:\n%s", logs)
	}
	h.write("api.key", "good-"+strings.Repeat("c", 30))
	h.cycle()
	if len(h.status.raised()) != 0 || len(w.values()) != 1 {
		t.Errorf("raised %v values %d", h.status.raised(), len(w.values()))
	}
	h.write("api.key", "bad-"+strings.Repeat("d", 30))
	h.cycle()
	if len(h.status.raised()) != 1 {
		t.Fatalf("raised = %v", h.status.raised())
	}
	stop()
	stop() // idempotent
	if len(h.status.raised()) != 0 || h.r.watchCount() != 0 {
		t.Errorf("after stop: raised %v, watches %d", h.status.raised(), h.r.watchCount())
	}
}

func TestWatcherPanicContained(t *testing.T) {
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_P", "panic-test-value")
	ref := envRef("RURALZ_SECRET_P")
	h.r.Activate(h.resolve(use(ref, secret.KindOpaque)))
	// Registered on a Store without the reference: seen is 0, so the
	// active value is delivered on the next cycle.
	defer h.resolve().Watch(ref, func(secret.Value) error { panic("boom") })()
	ok := &recorder{}
	defer h.resolve().Watch(ref, ok.fn)()
	h.cycle()
	if h.envN.value() != 1 {
		t.Errorf("env failures = %d", h.envN.value())
	}
	if !slices.Equal(ok.values(), []string{"panic-test-value"}) {
		t.Errorf("other watcher got %v", ok.values())
	}
	if !strings.Contains(h.logs.String(), errWatcherPanicked.Error()) {
		t.Error("panic not logged")
	}
}

func TestWatchEdgeCases(t *testing.T) {
	h := newHarness(t, nil)
	st := h.resolve()
	// A nil fn registers nothing.
	st.Watch(envRef("RURALZ_SECRET_X"), nil)()
	if h.r.watchCount() != 0 {
		t.Error("nil fn registered")
	}
	// Several watchers on one reference; stopping one keeps the others.
	stops := make([]func(), 3)
	for i := range stops {
		stops[i] = st.Watch(envRef("RURALZ_SECRET_X"), func(secret.Value) error { return nil })
	}
	stops[1]()
	if h.r.watchCount() != 2 {
		t.Errorf("watches = %d", h.r.watchCount())
	}
	stops[0]()
	stops[2]()
	if h.r.watchCount() != 0 {
		t.Errorf("watches = %d", h.r.watchCount())
	}
	// A watcher stopped from inside its own delivery does not deadlock.
	h.setEnv("RURALZ_SECRET_Y", "y")
	ref := envRef("RURALZ_SECRET_Y")
	h.r.Activate(h.resolve(use(ref, secret.KindOpaque)))
	var self func()
	calls := 0
	self = st.Watch(ref, func(secret.Value) error { calls++; self(); return errors.New("ignored after stop") })
	h.cycle()
	h.cycle()
	if calls != 1 || h.envN.value() != 0 {
		t.Errorf("calls %d failures %d", calls, h.envN.value())
	}
}

func TestStoreGetUnknown(t *testing.T) {
	h := newHarness(t, nil)
	if v, ok := h.resolve().Get(envRef("RURALZ_SECRET_NONE")); ok || !v.IsZero() {
		t.Error("Get of an unheld reference")
	}
}

func TestStoreGetDoesNotAllocate(t *testing.T) {
	// Architecture 2.8: Store is read lock-free on the request path.
	h := newHarness(t, nil)
	h.setEnv("RURALZ_SECRET_A", "a")
	ref := envRef("RURALZ_SECRET_A")
	st := h.resolve(use(ref, secret.KindOpaque))
	if n := testing.AllocsPerRun(100, func() { _, _ = st.Get(ref) }); n != 0 {
		t.Errorf("Get allocates %v", n)
	}
}

func TestRacyFailureCountedOnce(t *testing.T) {
	// A failed read too close to the change is re-read on the next poll,
	// and the same failure is not counted again.
	h := newHarness(t, nil)
	h.write("k", strings.Repeat("k", 30))
	ref := fileRef(h.path("k"), "")
	h.r.Activate(h.resolve(use(ref, secret.KindAPIKey)))
	h.write("k", "short")
	h.touch("k", h.clock.Now())
	h.cycle()
	if !h.r.Current().(*store).cells[ref].racy {
		t.Fatal("failed read at the modification time is trusted")
	}
	h.cycle()
	h.clock.Advance(DefaultPollInterval)
	h.cycle()
	h.cycle()
	if n := h.fileN.value(); n != 1 {
		t.Errorf("failures = %d, want 1", n)
	}
}

func TestPollCanceled(t *testing.T) {
	// A canceled cycle stops between files and changes nothing.
	h := newHarness(t, nil)
	h.write("a", "a1")
	ref := fileRef(h.path("a"), "")
	st := h.resolve(use(ref, secret.KindOpaque))
	h.r.Activate(st)
	h.write("a", "a2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.r.cycle(ctx)
	if got := get(t, st, ref); got != "a1" {
		t.Errorf("canceled poll applied %q", got)
	}
	h.cycle()
	if got := get(t, st, ref); got != "a2" {
		t.Errorf("value = %q", got)
	}
}

// urlUse is a State Store URL use with a consumer topology check.
func urlUse(ref secret.Ref, check func([]byte) error) secret.Use {
	u := use(ref, secret.KindStateStoreURL)
	u.Check = check
	return u
}

// topologyCheck stands in for the State Store driver's topology fit (spec
// 08 requirement 7): cluster needs addr parameters, standalone refuses
// them.
func topologyCheck(cluster bool) func([]byte) error {
	return func(b []byte) error {
		if strings.Contains(string(b), "addr=") != cluster {
			return errors.New("the URL does not fit the topology")
		}
		return nil
	}
}

func TestRefusedRotationRetried(t *testing.T) {
	// Spec 01 req 46 and spec 06 reqs 13 and 76: the last value is kept
	// only while the new value fails its check, and secret_rotation_failed
	// clears once the reference reads well. A rotation the active uses
	// refused is taken by a Revision whose uses accept it, at Resolve and
	// at Activate; spec 08 req 7: the topology fit follows the Bundle.
	const (
		clusterURL = "redis://u:p@a.internal:7000?addr=b.internal:7001"
		soloURL    = "redis://u:p@solo.internal:6379" //nolint:gosec // G101: a test fixture, not a credential.
	)
	t.Run("topology switch at Resolve", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("url", clusterURL)
		ref := fileRef(h.path("url"), "")
		s1 := h.resolve(urlUse(ref, topologyCheck(true)))
		h.r.Activate(s1)
		w := &recorder{}
		defer s1.Watch(ref, w.fn)()
		h.write("url", soloURL) // refused by the cluster Bundle
		h.cycle()
		if got := get(t, s1, ref); got != clusterURL || h.fileN.value() != 1 || len(h.status.raised()) != 1 {
			t.Fatalf("value %q, failures %d, raised %v", got, h.fileN.value(), h.status.raised())
		}
		// The operator hot-reloads the Bundle to topology standalone.
		s2 := h.resolve(urlUse(ref, topologyCheck(false)))
		if got := get(t, s2, ref); got != soloURL {
			t.Errorf("new Store value = %q, want the refused rotation", got)
		}
		if got := get(t, s1, ref); got != clusterURL {
			t.Errorf("active Store value = %q: it took a value its uses refuse", got)
		}
		h.r.Activate(s2)
		if got := h.status.raised(); len(got) != 0 {
			t.Errorf("raised after activation = %v", got)
		}
		h.cycle()
		h.cycle()
		if got := w.values(); !slices.Equal(got, []string{soloURL}) {
			t.Errorf("carried watcher got %v", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
		// Back to cluster: the healthy solo URL is refused at Resolve.
		diags := h.resolveDiags(urlUse(ref, topologyCheck(true)))
		if !strings.HasSuffix(diags[0].Message, "the URL does not fit the topology") {
			t.Errorf("message = %q", diags[0].Message)
		}
	})
	t.Run("relaxed checks at Resolve", func(t *testing.T) {
		h := newHarness(t, nil)
		h.write("k", strings.Repeat("k", 30))
		ref := fileRef(h.path("k"), "")
		h.r.Activate(h.resolve(use(ref, secret.KindAPIKey)))
		h.write("k", "short")
		h.cycle()
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if got := get(t, s2, ref); got != "short" {
			t.Errorf("value = %q", got)
		}
		h.r.Activate(s2)
		h.cycle()
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("relaxed checks at Activate", func(t *testing.T) {
		// A Store resolved before the refused rotation shares the cell;
		// activating it has the next cycle read the file again, although
		// its fingerprint is unchanged and the read was trusted.
		h := newHarness(t, nil)
		key := strings.Repeat("k", 30)
		h.write("k", key)
		ref := fileRef(h.path("k"), "")
		h.r.Activate(h.resolve(use(ref, secret.KindAPIKey)))
		relaxed := h.resolve(use(ref, secret.KindOpaque))
		w := &recorder{}
		defer relaxed.Watch(ref, w.fn)()
		h.write("k", "short")
		h.cycle()
		h.cycle()
		if got := get(t, relaxed, ref); got != key || len(h.status.raised()) != 1 {
			t.Fatalf("value %q, raised %v", got, h.status.raised())
		}
		h.r.Activate(relaxed)
		h.cycle()
		if got := get(t, relaxed, ref); got != "short" {
			t.Errorf("value = %q, want the refused rotation", got)
		}
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
		if got := w.values(); !slices.Equal(got, []string{"short"}) {
			t.Errorf("watcher got %v", got)
		}
	})
	t.Run("still refused", func(t *testing.T) {
		// A value the new uses refuse too keeps the cell and its last good
		// value, checked against the new uses; the re-examination after
		// Activate does not count the same failure again.
		h := newHarness(t, nil)
		p := newPEM(t)
		h.write("tls.crt", p.cert)
		ref := fileRef(h.path("tls.crt"), "")
		s1 := h.resolve(use(ref, secret.KindPEMCertificate))
		h.r.Activate(s1)
		h.write("tls.crt", "garbage")
		h.cycle()
		s2 := h.resolve(use(ref, secret.KindPEMCertPool))
		if got := get(t, s2, ref); got != p.cert || s2.(*store).cells[ref] != s1.(*store).cells[ref] {
			t.Errorf("value %q, cell shared %v", got, s2.(*store).cells[ref] == s1.(*store).cells[ref])
		}
		h.r.Activate(s2)
		h.cycle()
		h.cycle()
		if h.fileN.value() != 1 || len(h.status.raised()) != 1 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
		// Uses refusing both values report the last good value's reason.
		diags := h.resolveDiags(use(ref, secret.KindPEMPrivateKey))
		if !strings.HasSuffix(diags[0].Message, errNoPrivateKey.Error()) {
			t.Errorf("message = %q", diags[0].Message)
		}
	})
	t.Run("unreadable or unchanged file", func(t *testing.T) {
		// A file that cannot be read now, or holds the last good value
		// again, keeps the cell; the cycle after Activate clears the reason.
		h := newHarness(t, nil)
		h.write("s", "v1")
		ref := fileRef(h.path("s"), "")
		s1 := h.resolve(use(ref, secret.KindOpaque))
		h.r.Activate(s1)
		if err := os.Remove(filepath.Join(h.root, "s")); err != nil {
			t.Fatal(err)
		}
		h.cycle()
		s2 := h.resolve(use(ref, secret.KindOpaque))
		if s2.(*store).cells[ref] != s1.(*store).cells[ref] || get(t, s2, ref) != "v1" {
			t.Error("unreadable file: cell not kept")
		}
		h.write("s", "v1")
		s3 := h.resolve(use(ref, secret.KindOpaque))
		if s3.(*store).cells[ref] != s1.(*store).cells[ref] {
			t.Error("unchanged value: cell not kept")
		}
		h.r.Activate(s3)
		h.cycle()
		if h.fileN.value() != 1 || len(h.status.raised()) != 0 {
			t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
		}
	})
	t.Run("fresh and stale keys of one file", func(t *testing.T) {
		// One read serves a stale key and a key the active Store lacks.
		h := newHarness(t, nil)
		h.write("creds.json", `{"user":"u1","password":"p1"}`)
		user, pass := fileRef(h.path("creds.json"), "user"), fileRef(h.path("creds.json"), "password")
		strict := use(pass, secret.KindOpaque)
		strict.Check = func(b []byte) error {
			if string(b) == "p2" {
				return errors.New("refused")
			}
			return nil
		}
		h.r.Activate(h.resolve(strict))
		h.write("creds.json", `{"user":"u2","password":"p2"}`)
		h.cycle()
		st := h.resolve(use(user, secret.KindOpaque), use(pass, secret.KindOpaque))
		if u, p := get(t, st, user), get(t, st, pass); u != "u2" || p != "p2" {
			t.Errorf("user %q password %q", u, p)
		}
		// A stale key missing from the file now keeps its last good value.
		h.write("creds.json", `{"user":"u3"}`)
		st = h.resolve(use(user, secret.KindOpaque), use(pass, secret.KindOpaque))
		if u, p := get(t, st, user), get(t, st, pass); u != "u3" || p != "p1" {
			t.Errorf("user %q password %q", u, p)
		}
	})
}

func TestConsumerCheckPanicContained(t *testing.T) {
	// Spec 01 section 6 (secret leak tests: never in panics): a consumer
	// check that panics, here with the value, is a fixed reason at
	// Resolve and a counted rotation failure during a poll; the resolver
	// goroutine survives and the value is printed nowhere.
	h := newHarness(t, nil)
	check := func(b []byte) error {
		if strings.HasPrefix(string(b), "bad") {
			panic("cannot use " + string(b))
		}
		return nil
	}
	h.write("s", "good-value-1")
	ref := fileRef(h.path("s"), "")
	u := use(ref, secret.KindOpaque)
	u.Check = check
	st := h.resolve(u)
	h.r.Activate(st)
	cycles := make(chan struct{}, 8)
	h.r.cycleHook = func() { cycles <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()
	waitCycle(t, cycles) // the cycle Activate kicked, before the rotation
	h.write("s", "bad-value-2")
	waitTimer(t, h)
	h.clock.Advance(DefaultPollInterval)
	waitCycle(t, cycles)
	if got := get(t, st, ref); got != "good-value-1" || h.fileN.value() != 1 {
		t.Errorf("value %q, failures %d", got, h.fileN.value())
	}
	if logs := h.logs.String(); !strings.Contains(logs, errCheckPanicked.Error()) || strings.Contains(logs, "bad-value") {
		t.Errorf("log:\n%s", logs)
	}
	h.write("t", "bad-value-3")
	tu := use(fileRef(h.path("t"), ""), secret.KindOpaque)
	tu.Check = check
	diags := h.resolveDiags(tu)
	if text := diagText(diags); !strings.HasSuffix(diags[0].Message, errCheckPanicked.Error()) || strings.Contains(text, "bad-value") {
		t.Errorf("diagnostics:\n%s", text)
	}
}

func TestKubernetesDataSwap(t *testing.T) {
	// Spec 01 section 6 (file provider plan): a Kubernetes-style ..data
	// symlink swap is observed within 2 poll intervals, here within one.
	// The kubelet's atomic writer writes a new timestamped directory,
	// points ..data_tmp at it, renames ..data_tmp over ..data and removes
	// the old directory; the user-visible names are links through ..data.
	h := newHarness(t, nil)
	cycles := make(chan struct{}, 8)
	h.r.cycleHook = func() { cycles <- struct{}{} }
	p1, p2 := newPEM(t), newPEM(t)
	const old, next = "..2026_09_26_12_00_00.1", "..2026_09_26_12_05_00.2"
	h.write(old+"/tls.crt", p1.cert)
	h.write(old+"/tls.key", p1.key)
	mustSymlink(t, old, filepath.Join(h.root, "..data"))
	mustSymlink(t, "..data/tls.crt", filepath.Join(h.root, "tls.crt"))
	mustSymlink(t, "..data/tls.key", filepath.Join(h.root, "tls.key"))
	crt, key := fileRef(h.path("tls.crt"), ""), fileRef(h.path("tls.key"), "")
	st := h.resolve(use(crt, secret.KindPEMCertificate), use(key, secret.KindPEMPrivateKey))
	h.r.Activate(st)
	wc, wk := &recorder{}, &recorder{}
	defer st.Watch(crt, wc.fn)()
	defer st.Watch(key, wk.fn)()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()
	waitCycle(t, cycles) // the cycle Activate kicked

	h.write(next+"/tls.crt", p2.cert)
	h.write(next+"/tls.key", p2.key)
	mustSymlink(t, next, filepath.Join(h.root, "..data_tmp"))
	if err := os.Rename(filepath.Join(h.root, "..data_tmp"), filepath.Join(h.root, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(h.root, old)); err != nil {
		t.Fatal(err)
	}
	waitTimer(t, h)
	h.clock.Advance(DefaultPollInterval)
	waitCycle(t, cycles)
	if get(t, st, crt) != p2.cert || get(t, st, key) != p2.key {
		t.Error("Get does not return the swapped values")
	}
	if !slices.Equal(wc.values(), []string{p2.cert}) || !slices.Equal(wk.values(), []string{p2.key}) {
		t.Errorf("watchers got %d and %d values, want 1 each", len(wc.values()), len(wk.values()))
	}
	if h.fileN.value() != 0 || len(h.status.raised()) != 0 {
		t.Errorf("failures %d, raised %v", h.fileN.value(), h.status.raised())
	}
}
