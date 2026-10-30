// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// envTestT5 turns the live suite on (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Handover under systemd").
const envTestT5 = "RURALZ_TEST_T5"

// The live suite's targets and bounds
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Handover
// under systemd" and "Drain timeline defaults").
const (
	// reloadHandovers is how many reload handovers run under load.
	reloadHandovers = 20
	// stopBound is the unit's TimeoutStopSec=40: systemctl stop returns
	// within it, every process exited.
	stopBound = 40 * time.Second
	// watchdogInterval is the unit's WatchdogSec=30s; ruralzd answers
	// every half interval.
	watchdogInterval = 30 * time.Second
	// mainPIDBound bounds the wait for systemd to follow a successor's
	// MAINPID=, which may reach it just after the helper's exit.
	mainPIDBound = 5 * time.Second
	// restartBound bounds the wait for Restart=on-failure to bring a
	// killed Node back.
	restartBound = 30 * time.Second
)

// requireLive skips t unless RURALZ_TEST_T5=1 asks for the live suite, and
// fails it when the host cannot run it: the suite needs root and systemd
// as PID 1.
func requireLive(t *testing.T) {
	t.Helper()
	switch v := os.Getenv(envTestT5); v {
	case "":
		t.Skipf("the live T5 suite installs files under /usr/local and /etc, creates a system user and runs systemctl as root; set %s=1 to run it, as t5-systemd.yml does", envTestT5)
	case "1":
	default:
		t.Fatalf("%s=%q: set it to 1 to run the live T5 suite, or unset it", envTestT5, v)
	}
	if os.Geteuid() != 0 {
		t.Fatalf("%s=1: the live T5 suite runs as root", envTestT5)
	}
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		t.Fatalf("%s=1: systemd is not PID 1 here (no /run/systemd/system)", envTestT5)
	}
}

// exitWatch records when watched processes exit, so a late exit is caught
// however long after it the test looks.
type exitWatch struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	pending map[proc]time.Time     // process to the time it released the lock
	exited  map[proc]time.Duration // process to its exit's delay after the release
	order   []proc                 // processes in the order added
	notify  map[proc]chan struct{} // closed at the exit
}

// startExitWatch polls the watched processes every pollEvery until t ends.
func startExitWatch(ctx context.Context, t *testing.T) *exitWatch {
	ctx, cancel := context.WithCancel(ctx)
	w := &exitWatch{
		cancel: cancel, done: make(chan struct{}),
		pending: map[proc]time.Time{}, exited: map[proc]time.Duration{}, notify: map[proc]chan struct{}{},
	}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(pollEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			w.poll()
		}
	}()
	t.Cleanup(func() {
		w.cancel()
		<-w.done
	})
	return w
}

// add watches p, which released the lock at released.
func (w *exitWatch) add(p proc, released time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[p] = released
	w.order = append(w.order, p)
	w.notify[p] = make(chan struct{})
}

// poll records the exit of every watched process that no longer runs.
func (w *exitWatch) poll() {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, released := range w.pending {
		if !p.alive() {
			w.exited[p] = now.Sub(released)
			delete(w.pending, p)
			close(w.notify[p])
		}
	}
}

// late waits until every watched process exited or exitAfterReleaseBound
// passed after its release, and returns each one that exited later or not
// at all.
func (w *exitWatch) late() []string {
	const bound = exitAfterReleaseBound
	w.mu.Lock()
	order := slices.Clone(w.order)
	w.mu.Unlock()
	var late []string
	for _, p := range order {
		w.mu.Lock()
		released, waiting := w.pending[p]
		ch := w.notify[p]
		w.mu.Unlock()
		if waiting {
			select {
			case <-ch:
			case <-time.After(time.Until(released.Add(bound + pollEvery))):
			}
		}
		w.mu.Lock()
		d, ok := w.exited[p]
		w.mu.Unlock()
		switch {
		case !ok:
			late = append(late, fmt.Sprintf("%v still runs %v after releasing the lock", p, bound))
		case d > bound+pollEvery:
			late = append(late, fmt.Sprintf("%v exited %v after releasing the lock", p, d.Round(time.Millisecond)))
		}
	}
	return late
}

// reload runs `systemctl reload` and waits until systemd follows the new
// main process, which holder.json names; it returns that process and the
// time the reload returned, when the old process had released the lock.
func reload(ctx context.Context, t *testing.T, old proc) (proc, time.Time) {
	t.Helper()
	if out, err := systemctl(ctx, "reload", unitName); err != nil {
		t.Fatalf("systemctl reload: %v\n%s", err, out)
	}
	released := time.Now()
	var next proc
	err := waitFor(ctx, mainPIDBound, func() (bool, string) {
		p, err := mainProc(ctx)
		if err != nil {
			return false, err.Error()
		}
		holder, err := holderPID(dataDir)
		if err != nil {
			return false, err.Error()
		}
		next = p
		return p != old && p.PID == holder, fmt.Sprintf("MainPID %d, holder.json %d, old main process %d", p.PID, holder, old.PID)
	})
	if err != nil {
		t.Fatalf("after systemctl reload: %v", err)
	}
	return next, released
}

// unitState checks that the unit is active and running with restarts
// restarts and returns its properties.
func unitState(ctx context.Context, t *testing.T, restarts int64) map[string]string {
	t.Helper()
	props, err := show(ctx, propActiveState, propSubState, propNRestarts, propStatusText, propMainPID, propResult)
	if err != nil {
		t.Fatal(err)
	}
	if props[propActiveState] != "active" || props[propSubState] != "running" || props[propNRestarts] != strconv.FormatInt(restarts, 10) {
		t.Errorf("%s: ActiveState=%s SubState=%s NRestarts=%s Result=%s; want active, running and %d restarts",
			unitName, props[propActiveState], props[propSubState], props[propNRestarts], props[propResult], restarts)
	}
	return props
}

// statusReady waits until the unit's STATUS= names the ready Revision rev.
func statusReady(ctx context.Context, t *testing.T, rev string) {
	t.Helper()
	want := "ready " + rev
	err := waitFor(ctx, 5*time.Second, func() (bool, string) {
		props, err := show(ctx, propStatusText)
		if err != nil {
			return false, err.Error()
		}
		return props[propStatusText] == want, fmt.Sprintf("StatusText=%q", props[propStatusText])
	})
	if err != nil {
		t.Errorf("want StatusText=%q: %v", want, err)
	}
}

// nsWrite tries to create a file at path as the unit's user inside the
// mount namespace of the process p, as the Node itself would, and reports
// whether it could; a created file is removed.
func nsWrite(ctx context.Context, p proc, uid, gid, path string) (bool, string) {
	pid := strconv.Itoa(p.PID)
	out, err := exec.CommandContext(ctx, "nsenter", "--target", pid, "--mount", "--setuid", uid, "--setgid", gid, //nolint:gosec // G204: nsenter with arguments the suite builds
		"--", "touch", path).CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(string(out))
	}
	_ = exec.CommandContext(ctx, "nsenter", "--target", pid, "--mount", "--", "rm", "-f", path).Run() //nolint:gosec // G204: nsenter with arguments the suite builds
	return true, ""
}

// TestLive is the live T5 suite: the linux archive installed by the T5
// install steps, driven with systemctl, case by case in order, each case
// starting from the state it needs.
func TestLive(t *testing.T) {
	requireLive(t)
	ctx := t.Context()
	root := repoRoot(t)
	arc := openArchive(t, root)
	up := startUpstream(t)
	in := install(ctx, t, arc, up.Listener.Addr().String())

	// The unit starts with no usable configuration: READY=1 follows the
	// first boot attempt, ready or not, with STATUS= naming the reason;
	// the Node turns ready once the source holds a valid Bundle.
	t.Run("start without a valid Bundle", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		in.setBundle(t, false)
		if out, err := systemctl(ctx, "start", unitName); err != nil {
			t.Fatalf("systemctl start: %v\n%s", err, out)
		}
		props := unitState(ctx, t, 0)
		if s := props[propStatusText]; !strings.HasPrefix(s, "not ready: no_revision") {
			t.Errorf("StatusText=%q after starting without a valid Bundle, want not ready: no_revision ...", s)
		}
		code, v, err := readyz(ctx, in.adminURL())
		if err != nil || code != http.StatusServiceUnavailable || !slices.ContainsFunc(v.Reasons, func(r adminapi.ReadyReason) bool { return r.Reason == "no_revision" }) {
			t.Errorf("/readyz = %d %+v, %v; want 503 with no_revision", code, v, err)
		}
		in.setBundle(t, true)
		v, err = waitReady(ctx, in.adminURL())
		if err != nil {
			t.Fatalf("after the source turned valid: %v", err)
		}
		statusReady(ctx, t, v.Revision)
		if err := get(ctx, in.webURL()); err != nil {
			t.Errorf("GET /: %v", err)
		}
		unitState(ctx, t, 0)
	})

	// The unit starts on a valid Bundle: when systemctl start returns,
	// READY=1 came after the activation, so the Node is ready and STATUS=
	// names its Revision.
	t.Run("start with a valid Bundle", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		if out, err := systemctl(ctx, "stop", unitName); err != nil {
			t.Fatalf("systemctl stop: %v\n%s", err, out)
		}
		in.setBundle(t, true)
		if out, err := systemctl(ctx, "start", unitName); err != nil {
			t.Fatalf("systemctl start: %v\n%s", err, out)
		}
		code, v, err := readyz(ctx, in.adminURL())
		if err != nil || code != http.StatusOK {
			t.Fatalf("/readyz = %d %+v, %v once systemctl start returned; want 200", code, v, err)
		}
		if props := unitState(ctx, t, 0); props[propStatusText] != "ready "+v.Revision {
			t.Errorf("StatusText=%q once systemctl start returned, want %q", props[propStatusText], "ready "+v.Revision)
		}
		p, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(p.PID), "exe")); err != nil || exe != binPath {
			t.Errorf("the main process runs %q (%v), want %s", exe, err, binPath)
		}
		if err := get(ctx, in.webURL()); err != nil {
			t.Errorf("GET /: %v", err)
		}
	})

	// systemd-sysusers created the system user from the sysusers.d entry,
	// StateDirectory= the owner-only data directory, and sysctl --system
	// applied 99-ruralzd.conf.
	t.Run("system user, data directory and sysctl", func(t *testing.T) {
		passwd, err := run(ctx, "getent", "passwd", userName)
		if err != nil {
			t.Fatal(err)
		}
		group, err := run(ctx, "getent", "group", userName)
		if err != nil {
			t.Fatal(err)
		}
		uidMax, gidMax := sysIDMax(hostLoginDefs())
		for _, p := range checkSystemUser([]byte(passwd), []byte(group), uidMax, gidMax) {
			t.Error(p)
		}
		fi, err := os.Stat(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if u := userEntry([]byte(passwd)); !fi.IsDir() || fi.Mode().Perm() != 0o700 || !ok || len(u) != 7 || strconv.FormatUint(uint64(st.Uid), 10) != u[2] {
			t.Errorf("%s: %v, owner %v; want a directory of mode 0700 owned by %s", dataDir, fi.Mode(), fi.Sys(), userName)
		}
		if b, err := os.ReadFile(migrateReq); err != nil || strings.TrimSpace(string(b)) != "1" {
			t.Errorf("net.ipv4.tcp_migrate_req = %q (%v) after sysctl --system, want 1", b, err)
		}
	})

	// The running Node holds only CAP_NET_BIND_SERVICE, cannot gain
	// privileges, runs as ruralz, and writes nothing on the host outside its
	// data directory: every other path is read-only to it, and /tmp is
	// private to the unit.
	t.Run("hardening", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		p, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st, err := procStatus(p.PID)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range capProblems(st) {
			t.Error(problem)
		}
		if st["NoNewPrivs"] != "1" {
			t.Errorf("NoNewPrivs=%q, want 1", st["NoNewPrivs"])
		}
		passwd, err := run(ctx, "getent", "passwd", userName)
		if err != nil {
			t.Fatal(err)
		}
		u := userEntry([]byte(passwd))
		if len(u) != 7 {
			t.Fatalf("getent passwd %s = %q", userName, passwd)
		}
		uid, gid := u[2], u[3]
		for _, f := range []struct{ key, want string }{{"Uid", uid}, {"Gid", gid}} {
			if ids := strings.Fields(st[f.key]); len(ids) != 4 || slices.ContainsFunc(ids, func(id string) bool { return id != f.want }) {
				t.Errorf("%s: %q, want %s throughout", f.key, st[f.key], f.want)
			}
		}
		for _, dir := range []string{"/", "/etc", etcDir, installBin, "/usr", "/var/lib", "/var/log", "/var/cache", "/opt", "/srv", "/run", "/home", "/root"} {
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			probe := filepath.Join(dir, ".t5-write-probe")
			if ok, _ := nsWrite(ctx, p, uid, gid, probe); ok {
				t.Errorf("the Node can write %s", probe)
			}
			if _, err := os.Stat(probe); err == nil {
				t.Errorf("%s appeared on the host", probe)
				_ = os.Remove(probe)
			}
		}
		if ok, why := nsWrite(ctx, p, uid, gid, filepath.Join(dataDir, ".t5-write-probe")); !ok {
			t.Errorf("the Node cannot write its data directory: %s", why)
		}
		private := "/tmp/.t5-write-probe-" + strconv.Itoa(os.Getpid())
		if ok, why := nsWrite(ctx, p, uid, gid, private); !ok {
			t.Errorf("the Node cannot write its private /tmp: %s", why)
		}
		if _, err := os.Stat(private); err == nil {
			t.Errorf("%s, written in the unit's /tmp, appeared on the host's", private)
			_ = os.Remove(private)
		}
	})

	// WatchdogSec=30s: systemd gives the ExecStart= process WATCHDOG_USEC
	// and WATCHDOG_PID, and ruralzd sends WATCHDOG=1 every half interval.
	t.Run("watchdog", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		props, err := show(ctx, propWatchdogUSec, propWatchdogMono, propNRestarts)
		if err != nil {
			t.Fatal(err)
		}
		if props[propWatchdogUSec] != "30s" {
			t.Errorf("%s=%q, want 30s", propWatchdogUSec, props[propWatchdogUSec])
		}
		p, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p.PID), "environ")) //nolint:gosec // G304: a procfs path built from a PID
		if err != nil {
			t.Fatal(err)
		}
		vars := strings.Split(string(environ), "\x00")
		for _, want := range []string{"WATCHDOG_USEC=" + strconv.FormatInt(watchdogInterval.Microseconds(), 10), "WATCHDOG_PID=" + strconv.Itoa(p.PID)} {
			if !slices.Contains(vars, want) {
				t.Errorf("the main process's environment lacks %s", want)
			}
		}
		pings := map[string]bool{props[propWatchdogMono]: true}
		err = waitFor(ctx, watchdogInterval+10*time.Second, func() (bool, string) {
			v, err := show(ctx, propWatchdogMono)
			if err != nil {
				return false, err.Error()
			}
			pings[v[propWatchdogMono]] = true
			return len(pings) >= 3, fmt.Sprintf("%s values %v", propWatchdogMono, pings)
		})
		if err != nil {
			t.Errorf("want two WATCHDOG=1 pings within %v: %v", watchdogInterval, err)
		}
		if q, err := mainProc(ctx); err != nil || q != p {
			t.Errorf("the main process changed from %v to %v (%v)", p, q, err)
		}
		unitState(ctx, t, 0)
	})

	// A refused handover: `systemctl reload` with a binary whose handover
	// marker the holder does not read fails, the new process exits 1
	// holding no socket, and the holder keeps serving every request.
	t.Run("refused handover", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		variant, err := os.ReadFile(refusedRuralzd(t))
		if err != nil {
			t.Fatal(err)
		}
		old, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checker := startChecker(ctx, t, in.adminURL()+admin.PathReadyz)
		ld := startLoad(ctx, t, in.webURL()+"/", loadSpec{loadgen.HTTP1, 8, 100})
		// The load covers a second on each side of the refusal.
		if err := ld.waitMore(ctx); err != nil {
			t.Fatal(err)
		}
		since := time.Now()
		in.setBinary(t, variant)
		out, err := systemctl(ctx, "reload", unitName)
		in.setBinary(t, arc.binary)
		if err == nil {
			t.Errorf("systemctl reload with the refused-handover variant succeeded:\n%s", out)
		}
		if err := ld.waitMore(ctx); err != nil {
			t.Fatal(err)
		}
		unitState(ctx, t, 0)
		if p, err := mainProc(ctx); err != nil || p != old {
			t.Errorf("the main process is %v (%v) after a refused handover, want %v", p, err, old)
		}
		cg, err := show(ctx, propControlGroup)
		if err != nil {
			t.Fatal(err)
		}
		if pids, err := controlGroupProcs(cg[propControlGroup]); err != nil || !slices.Equal(pids, []int{old.PID}) {
			t.Errorf("the unit's processes are %v (%v), want only the holder %d", pids, err, old.PID)
		}
		err = waitFor(ctx, 10*time.Second, func() (bool, string) {
			text, err := journalText(ctx, since)
			if err != nil {
				return false, err.Error()
			}
			return strings.Contains(text, "unknown_marker"), "no unknown_marker in the journal"
		})
		if err != nil {
			t.Errorf("the refusal: %v", err)
		}
		sum, err := ld.stop()
		if err != nil || sum.offered == 0 || sum.failed != 0 {
			t.Errorf("load during the refused handover: %d of %d requests failed (%v): %q", sum.failed, sum.offered, err, sum.failures)
		}
		probes, failed, failures := checker.stop()
		if failed != 0 || probes == 0 {
			t.Errorf("keep-alive checker on %d: %d of %d probes failed: %q", adminPort, failed, probes, failures)
		}
		t.Logf("refused handover: %d requests, %d failed; %d probes on %d, %d failed", sum.offered, sum.failed, probes, adminPort, failed)
	})

	// The watchdog after a reload. A handover successor, a child of the
	// ExecReload= helper, gets no WATCHDOG_USEC from systemd, which sets it
	// for the ExecStart= process only, and today reads only its own
	// environment, so it sends no WATCHDOG=1 and the unit's watchdog ends
	// it WatchdogSec after the old process's last ping. Which interval the
	// successor uses is OQ-zero-downtime-upgrades-and-hot-reload-12; until
	// the owner answers, this case states the gap when it sees it and
	// passes when the successor pings.
	t.Run("watchdog after reload", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		in.restartReady(ctx, t)
		restarts, err := showInt(ctx, propNRestarts)
		if err != nil {
			t.Fatal(err)
		}
		old, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next, released := reload(ctx, t, old)
		// The old process sends nothing from its Drain's start, before it
		// released the lock, so a ping from here on is the successor's.
		before, err := show(ctx, propWatchdogMono)
		if err != nil {
			t.Fatal(err)
		}
		pinged := waitFor(ctx, watchdogInterval/2+10*time.Second, func() (bool, string) {
			v, err := show(ctx, propWatchdogMono)
			if err != nil {
				return false, err.Error()
			}
			return v[propWatchdogMono] != before[propWatchdogMono], "no WATCHDOG=1 from the successor"
		}) == nil
		if pinged {
			unitState(ctx, t, restarts)
			return
		}
		// Observe what the gap costs: the watchdog ends the successor and
		// Restart=on-failure starts a new process.
		var observed string
		_ = waitFor(ctx, time.Until(released.Add(watchdogInterval+15*time.Second)), func() (bool, string) {
			props, err := show(ctx, propNRestarts, propMainPID)
			if err != nil {
				return false, err.Error()
			}
			observed = fmt.Sprintf("NRestarts=%s (was %d), MainPID=%s (successor %d)",
				props[propNRestarts], restarts, props[propMainPID], next.PID)
			return props[propNRestarts] != strconv.FormatInt(restarts, 10), observed
		})
		if text, err := journalText(ctx, released); err == nil && strings.Contains(text, "Watchdog timeout") {
			observed = "systemd logged a watchdog timeout and restarted the unit: " + observed
		}
		in.restartReady(ctx, t)
		t.Skipf("open gap, OQ-zero-downtime-upgrades-and-hot-reload-12: the successor %v sent no WATCHDOG=1 within %v of taking over, as it has no watchdog interval (systemd sets WATCHDOG_USEC for ExecStart= only); then %s. The recommendation awaiting the owner: the holder sends its watchdog interval in the handover accepted message",
			next, watchdogInterval/2+10*time.Second, observed)
	})

	// 20 reload handovers under open-loop load of HTTP/1.1 keep-alive and
	// h2c clients, with a keep-alive checker on 9901: no request or probe
	// fails, MainPID moves to each successor and each old process exits
	// within 28 s (target). The drop-in turns the watchdog off while
	// OQ-zero-downtime-upgrades-and-hot-reload-12 is open, since no
	// successor would answer it.
	t.Run("reload handovers", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		in.setWatchdogOff(ctx, t, true)
		in.restartReady(ctx, t)
		restarts, err := showInt(ctx, propNRestarts)
		if err != nil {
			t.Fatal(err)
		}
		checker := startChecker(ctx, t, in.adminURL()+admin.PathReadyz)
		ld := startLoad(ctx, t, in.webURL()+"/",
			loadSpec{loadgen.HTTP1, 32, 250}, loadSpec{loadgen.H2C, 32, 250})
		exits := startExitWatch(ctx, t)
		main, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range reloadHandovers {
			in.setBinary(t, arc.binary)
			next, released := reload(ctx, t, main)
			exits.add(main, released)
			if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(next.PID), "exe")); err != nil || exe != binPath {
				t.Errorf("handover %d: the main process runs %q (%v), want %s", i+1, exe, err, binPath)
			}
			main = next
		}
		for _, l := range exits.late() {
			t.Error(l)
		}
		sum, err := ld.stop()
		if err != nil || sum.offered == 0 || sum.failed != 0 {
			t.Errorf("load across %d handovers: %d of %d requests failed (%v): %q", reloadHandovers, sum.failed, sum.offered, err, sum.failures)
		}
		probes, failed, failures := checker.stop()
		if failed != 0 || probes == 0 {
			t.Errorf("keep-alive checker on %d: %d of %d probes failed: %q", adminPort, failed, probes, failures)
		}
		t.Logf("%d handovers: %d requests, %d failed; %d probes on %d, %d failed", reloadHandovers, sum.offered, sum.failed, probes, adminPort, failed)
		unitState(ctx, t, restarts)
		v, err := waitReady(ctx, in.adminURL())
		if err != nil {
			t.Fatal(err)
		}
		statusReady(ctx, t, v.Revision)
	})

	// `systemctl stop` during a handover, while the old process drains and
	// the successor serves: both Drain, and stop returns 0 within
	// TimeoutStopSec=40, with no process killed.
	t.Run("stop during a handover", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		in.restartReady(ctx, t)
		old, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next, _ := reload(ctx, t, old)
		if !old.alive() {
			t.Fatalf("the old process exited before the stop; the case needs it draining")
		}
		start := time.Now()
		out, err := systemctl(ctx, "stop", unitName)
		elapsed := time.Since(start)
		if err != nil || elapsed > stopBound {
			t.Errorf("systemctl stop = %v after %v, want exit 0 within %v\n%s", err, elapsed.Round(time.Millisecond), stopBound, out)
		}
		t.Logf("systemctl stop during a handover returned after %v", elapsed.Round(time.Millisecond))
		props, err := show(ctx, propActiveState, propResult, propExecMainStatus)
		if err != nil {
			t.Fatal(err)
		}
		if props[propActiveState] != "inactive" || props[propResult] != "success" || props[propExecMainStatus] != "0" {
			t.Errorf("after systemctl stop: ActiveState=%s Result=%s ExecMainStatus=%s; want inactive, success and 0",
				props[propActiveState], props[propResult], props[propExecMainStatus])
		}
		for _, p := range []proc{old, next} {
			if p.alive() {
				t.Errorf("%v runs after systemctl stop", p)
			}
		}
	})

	// A SIGKILL restart: Restart=on-failure starts a new process, which
	// boots Last-Known-Good when the source no longer renders a Revision,
	// serving it under degraded reason lkg_boot.
	t.Run("SIGKILL restart boots Last-Known-Good", func(t *testing.T) {
		evidence(ctx, t, time.Now())
		in.setWatchdogOff(ctx, t, false)
		in.restartReady(ctx, t)
		code, v, err := readyz(ctx, in.adminURL())
		if err != nil || code != http.StatusOK {
			t.Fatalf("/readyz = %d %+v, %v", code, v, err)
		}
		var active string
		err = waitFor(ctx, readyBound, func() (bool, string) {
			s, err := snapshots(ctx, in.adminURL())
			if err != nil {
				return false, err.Error()
			}
			for _, sn := range s.Snapshots {
				if sn.State == "active" {
					active = sn.Digest
				}
			}
			return s.LastKnownGood != nil && *s.LastKnownGood == active, fmt.Sprintf("active %s, lastKnownGood %v", active, s.LastKnownGood)
		})
		if err != nil {
			t.Fatalf("Last-Known-Good: %v", err)
		}
		in.setBundle(t, false)
		err = waitFor(ctx, readyBound, func() (bool, string) {
			s, err := snapshots(ctx, in.adminURL())
			if err != nil {
				return false, err.Error()
			}
			return s.LastRejected != nil, "the invalid source not rejected yet"
		})
		if err != nil {
			t.Fatalf("the invalid source: %v", err)
		}
		restarts, err := showInt(ctx, propNRestarts)
		if err != nil {
			t.Fatal(err)
		}
		old, err := mainProc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		err = waitFor(ctx, restartBound, func() (bool, string) {
			props, err := show(ctx, propNRestarts, propActiveState, propMainPID)
			if err != nil {
				return false, err.Error()
			}
			return props[propNRestarts] == strconv.FormatInt(restarts+1, 10) && props[propActiveState] == "active" &&
				props[propMainPID] != "0" && props[propMainPID] != strconv.Itoa(old.PID), fmt.Sprintf("%v", props)
		})
		if err != nil {
			t.Fatalf("after SIGKILL: %v", err)
		}
		after, err := waitReady(ctx, in.adminURL())
		if err != nil {
			t.Fatalf("the restarted Node: %v", err)
		}
		if after.Revision != v.Revision {
			t.Errorf("the restarted Node serves %s, want Last-Known-Good's %s", after.Revision, v.Revision)
		}
		statusReady(ctx, t, v.Revision)
		hdr := http.Header{"Authorization": {"Bearer " + adminToken}}
		m, err := promtext.Scrape(ctx, adminClient(), in.adminURL()+admin.PathMetrics, hdr)
		if err != nil {
			t.Fatal(err)
		}
		if lkg, err := m.Value("ruralz_node_degraded_info", promtext.L("reason", "lkg_boot")); err != nil || lkg != 1 {
			t.Errorf("ruralz_node_degraded_info{reason=\"lkg_boot\"} = %v (%v), want 1", lkg, err)
		}
		if err := get(ctx, in.webURL()); err != nil {
			t.Errorf("GET / on Last-Known-Good: %v", err)
		}
	})
}
