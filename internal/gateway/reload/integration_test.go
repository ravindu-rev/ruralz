// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/secret/resolver"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
)

// An end-to-end run of the real parts in process: the configuration
// pipeline with the offline checks, the snapshot compiler with the
// built-in Filters, the source watcher, the retirer, the client
// listeners and Last-Known-Good, on a fake clock: boot, Hot Reload, a
// rejected change, /config/dump as ruralz node dump checks it, and a
// restart that boots Last-Known-Good.

// sharedPipeline is the real configuration pipeline the tests share:
// compiling the embedded schema views takes a while.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 1})
})

// bundleYAML is a Gateway with one http listener on port and an Upstream,
// with one Route per path.
func bundleYAML(port int, paths ...string) string {
	var b bytes.Buffer
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  listeners:\n    - name: web\n      protocol: http\n      port: " + strconv.Itoa(port) + "\n")
	b.WriteString("---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n  endpoints:\n    - address: \"127.0.0.1:9\"\n")
	for i, p := range paths {
		b.WriteString("---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: r" + strconv.Itoa(i) + "\nspec:\n  match:\n    path:\n      prefix: " + p + "\n  upstreams:\n    - name: orders\n")
	}
	return b.String()
}

// node is one process's activation parts over a data directory.
type node struct {
	t      *testing.T
	clk    *clocktest.Fake
	loader *Loader
	holder *retire.Holder
	ready  *readiness.Tracker
	set    *listener.Set
	store  *lkg.Store
	w      *source.Watcher
	status *fakeStatus
	logs   *logRecorder
	cancel context.CancelFunc
	done   sync.WaitGroup
}

// newNode builds and runs a lock holder's parts.
func newNode(t *testing.T, clk *clocktest.Fake, dir *nodedir.Dir, lock *nodedir.Lock, configPath string) *node {
	t.Helper()
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{t: t, clk: clk, cancel: cancel, status: &fakeStatus{}, logs: &logRecorder{}, ready: readiness.New()}
	res, err := resolver.NewResolver(resolver.Config{Root: t.TempDir(), LookupEnv: func(string) (string, bool) { return "", false }, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(ctx, manager.Config{Deps: statestore.Deps{Clock: clk}, Openers: manager.Openers{Memory: memory.Open}})
	meter, err := aggregate.New(aggregate.Options{Clock: clk, Stripes: 1})
	if err != nil {
		t.Fatal(err)
	}
	filters, err := builtin.New(builtin.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	comp, err := compile.New(compile.Deps{
		Pipeline: p, CEL: c, Filters: filters, Secrets: res, LookupEnv: func(string) (string, bool) { return "", false },
		StateStores: mgr, Upstreams: upstream.NewManager(upstream.Options{Clock: clk}),
		Composition: composition.New(composition.Deps{Clock: clk}), Meter: meter, Clock: clk, Workers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	n.holder = retire.New(retire.Config{Clock: clk, Stripes: 1})
	n.set, err = listener.New(listener.Options{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "served") }),
		Conns:   admission.NewConnLimiter(admission.DefaultConnections), Clock: clk, Hosts: []string{"127.0.0.1"}, Steer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	n.store, err = lkg.New(lkg.Config{Dir: dir, Version: "0.1.0-test", Clock: clk, Status: n.status})
	if err != nil {
		t.Fatal(err)
	}
	var l *Loader
	w, err := source.New(source.Config{Path: configPath, Pipeline: p, Clock: clk, Sink: func(ld source.Load) { l.Offer(ld) }})
	if err != nil {
		t.Fatal(err)
	}
	l, err = New(Config{
		Boot: BootHolder, Source: w, Pipeline: p, Compiler: CompilerOf(comp), Holder: n.holder,
		Readiness: n.ready, LKG: n.store, Lock: func() *nodedir.Lock { return lock },
		Parts: []Part{ListenerPart(n.set)}, Meter: meter, Status: n.status,
		RequestID: func(*http.Request) string { return testRequestID }, Clock: clk, Logger: slog.New(n.logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	n.loader, n.w = l, w
	n.done.Go(func() { _ = n.holder.Run(ctx) })
	n.done.Go(func() { _ = l.Run(ctx) })
	t.Cleanup(n.stop)
	return n
}

// stop ends the node's goroutines and closes what it opened.
func (n *node) stop() {
	n.loader.Stop()
	n.cancel()
	n.done.Wait()
	_ = n.set.Close()
}

// booted waits for the first boot attempt.
func (n *node) booted() {
	n.t.Helper()
	select {
	case <-n.loader.Booted():
	case <-time.After(bound):
		n.t.Fatal("the boot did not end")
	}
}

// reload makes the watcher load the source at once, as SIGHUP does; the
// watcher's polling and settle debounce have their own tests.
func (n *node) reload() {
	n.t.Helper()
	waitFor(n.t, "the watcher to run", func() bool {
		n.loader.mu.Lock()
		defer n.loader.mu.Unlock()
		return n.loader.watching
	})
	n.w.Reload()
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the embedded schemas")
	}
	clk := clocktest.New(epoch())
	dir, err := nodedir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := dir.TryLock()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no flock on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bundle, "ruralz.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(bundleYAML(port, "/orders"))

	// Boot: the source activates, the listener serves, lkg/ names it.
	n := newNode(t, clk, dir, lock, bundle)
	n.booted()
	if !n.ready.Ready() {
		t.Fatalf("/readyz %+v after the boot", n.ready.Readyz())
	}
	first := n.loader.Active()
	if got := get(t, n.set.Addr("web").String()); got != "served" {
		t.Fatalf("the listener answers %q", got)
	}
	if cand, last, _ := n.store.Pointers(); cand != first || last != first {
		t.Fatalf("lkg/ names %s and %s, want %s", cand.Short(), last.Short(), first.Short())
	}

	// /config/dump passes the check ruralz node dump makes before it
	// writes anything (canonical.Decoder.ReadDump).
	p, _ := sharedPipeline()
	w := httptest.NewRecorder()
	n.loader.ConfigDump().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config/dump", nil))
	dump, rs, err := p.Decoder().ReadDump(t.Context(), w.Body, 0, canonical.Source{Name: "/config/dump"})
	if err != nil || dump.Digest != first || dump.LastKnownGood != first || len(rs) != 3 {
		t.Fatalf("ReadDump = digest %s, lkg %s, %d resources, %v", dump.Digest.Short(), dump.LastKnownGood.Short(), len(rs), err)
	}

	// Hot Reload: a changed source activates its Revision.
	write(bundleYAML(port, "/orders", "/carts"))
	n.reload()
	waitFor(t, "the second Revision", func() bool { a := n.loader.Active(); return !a.IsZero() && a != first })
	second := n.loader.Active()
	if n.holder.Active().Revision.Digest != second || len(n.holder.Active().Routes) != 2 {
		t.Fatal("the published snapshot is not the second Revision")
	}
	if got := get(t, n.set.Addr("web").String()); got != "served" {
		t.Fatalf("the listener answers %q after the Hot Reload", got)
	}

	// A broken change is rejected and changes nothing.
	write("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: [unclosed\n")
	n.reload()
	waitFor(t, "the rejection", func() bool { return n.loader.Snapshots().LastRejected != nil })
	if lr := n.loader.Snapshots().LastRejected; lr.Digest != nil || lr.Code != "RZ-CFG-001" {
		t.Fatalf("lastRejected = %+v, want a parse failure without digest", lr)
	}
	if n.loader.Active() != second || !n.ready.Ready() {
		t.Fatal("the rejected change changed what the Node serves")
	}

	// Restart without a source: Last-Known-Good boots under lkg_boot.
	waitFor(t, "lkg/ to name the second Revision", func() bool {
		cand, last, _ := n.store.Pointers()
		return cand == second && last == second
	})
	n.stop()
	if err := os.RemoveAll(bundle); err != nil {
		t.Fatal(err)
	}
	m := newNode(t, clk, dir, lock, bundle)
	m.booted()
	if m.loader.Active() != second || !m.ready.Ready() {
		t.Fatalf("restart booted %s, want Last-Known-Good %s", m.loader.Active().Short(), second.Short())
	}
	if !m.status.raised("lkg_boot") {
		t.Fatal("lkg_boot is not raised after booting Last-Known-Good")
	}
	// The source comes back with another Revision: it activates and
	// clears lkg_boot.
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	write(bundleYAML(port, "/payments"))
	m.reload()
	waitFor(t, "the source Revision", func() bool {
		a := m.loader.Active()
		return a != second && !a.IsZero()
	})
	waitFor(t, "lkg_boot to clear", func() bool { return !m.status.raised("lkg_boot") })
}
