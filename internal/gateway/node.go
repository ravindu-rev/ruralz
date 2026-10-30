// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/drain"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/reload"
	"github.com/ravindu-rev/ruralz/internal/gateway/replay"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/sdnotify"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/forward"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/secret/resolver"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/statestore/redis"
	"github.com/ravindu-rev/ruralz/internal/telemetry"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// Process log components of the Node's parts.
const (
	componentNode        = "ruralzd"
	componentAdmin       = "admin"
	componentSecret      = "secret"
	componentFilter      = "filter"
	componentStateStore  = "statestore"
	componentComposition = "composition"
	componentRetire      = "retire"
	componentCompile     = "compile"
	componentHandler     = "handler"
	componentListener    = "listener"
	componentLKG         = "lkg"
	componentSource      = "source"
	componentReload      = "reload"
	componentDrain       = "drain"
	componentHandover    = "handover"
	componentWatchdog    = "watchdog"
)

// handoverSupported reports whether a process that finds a live lock
// holder runs the in-place handover: Linux only, since elsewhere
// SO_PEERCRED is missing and the holder refuses every peer; such a process
// exits 1 at once instead (internal/nodedir).
func handoverSupported() bool { return runtime.GOOS == "linux" }

// node is one running Node: its parts and the goroutines it owns.
type node struct {
	p       process
	clk     clock.Clock
	s       setting.Settings
	vars    subst.Source
	version string
	log     *slog.Logger
	rt      *telemetry.Runtime
	dir     *nodedir.Dir
	nodeID  ulid.ULID

	// successor is set for a process that found a live lock holder at
	// start: a handover's new process.
	successor bool
	// lock is the data-directory lock as the Drain releases it.
	lock *handover.Lock
	// hosts are the hosts every listener and the admin port bind: none
	// for every interface, LoopbackHosts under RURALZ_BIND_LOOPBACK.
	hosts []string

	pipe      *pipeline.Pipeline
	res       *resolver.Resolver
	filters   *filter.Registry
	stores    *manager.Manager
	ups       *upstream.Manager
	units     *admission.Units
	conns     *admission.ConnLimiter
	budget    *body.Budget
	retirer   *retire.Holder
	tracker   *readiness.Tracker
	set       *listener.Set
	auth      *adminauth.Authenticator
	adminTLS  *tls.Config
	adminSrv  *admin.Server
	adminSock *reload.AdminSocket
	store     *lkg.Store
	watcher   *source.Watcher
	loader    *reload.Loader
	notifier  *sdnotify.Notifier
	interval  sdnotifyInterval
	drainer   *drain.Drainer

	// loops are the background goroutines: they end with loopCtx.
	loops   *supervisor
	loopCtx context.Context
	stop    context.CancelFunc
	// components are the Filter Components: they end at the Drain's
	// components step, once the last snapshot retired.
	components *supervisor
	compCtx    context.Context
	stopComps  context.CancelFunc

	mu sync.Mutex
	// holderSrv serves the handover socket while the process holds the
	// lock; successorRun is a successor's handover.
	holderSrv *handover.Holder
	succ      *handover.Successor
	// closers run in reverse order when the Node ends.
	closers []func()
}

// sdnotifyInterval is the watchdog interval the service manager asks for
// and why it has none.
type sdnotifyInterval struct {
	env sdnotify.Env
	err error
}

// refusal is a start the Node refuses: its exit code and the message
// standard error and the process log get.
type refusal struct {
	code int
	err  error
}

// refuse returns a refusal with exit 2.
func refuse(format string, args ...any) *refusal {
	return &refusal{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

// fail returns a refusal with exit 1.
func fail(format string, args ...any) *refusal {
	return &refusal{code: ExitRuntime, err: fmt.Errorf(format, args...)}
}

// start builds a Node from its settings and starts it, up to the boot: it
// returns the Node, or nil and the exit code after writing why to stderr.
func start(ctx context.Context, p process, clk clock.Clock, s setting.Settings, vars subst.Source, stdout, stderr io.Writer) (*node, int) {
	n := &node{p: p, clk: clk, s: s, vars: vars, version: buildinfo.Get().Version}
	if r := n.build(ctx, stdout); r != nil {
		_, _ = fmt.Fprintf(stderr, "ruralzd: %v\n", r.err)
		if n.log != nil {
			n.log.ErrorContext(ctx, "ruralzd refused to start", catalog.KeyError, r.err.Error(), "exit_code", r.code)
		}
		n.close(ctx)
		return nil, r.code
	}
	return n, ExitOK
}

// onClose adds f to what the Node runs when it ends, in reverse order.
func (n *node) onClose(f func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closers = append(n.closers, f)
}

// close runs the closers, last added first, once.
func (n *node) close(ctx context.Context) {
	n.mu.Lock()
	cs := n.closers
	n.closers = nil
	n.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
	if n.rt != nil {
		// Flush what the process logged; a Drain already did.
		_ = n.rt.Shutdown(context.WithoutCancel(ctx))
	}
}

// build constructs every part of the Node; on a refusal what it opened is
// left in the closers.
func (n *node) build(ctx context.Context, stdout io.Writer) *refusal {
	s, clk := n.s, n.clk
	dir, err := nodedir.Open(s.DataDir)
	if err != nil {
		return refuse("%s: %w", setting.EnvDataDir, err)
	}
	n.dir = dir
	n.nodeID, err = dir.NodeID(func() (ulid.ULID, error) { return ulid.New(clk.Now(), nil) })
	if err != nil {
		return refuse("%s: node.id: %w", setting.EnvDataDir, err)
	}
	n.rt, err = telemetry.New(telemetry.Options{
		Service: telemetry.ServiceGateway, NodeID: n.nodeID.String(), Version: n.version,
		Stdout: stdout, Level: s.LogLevel.Slog(), Clock: clk,
	})
	if err != nil {
		return fail("telemetry: %w", err)
	}
	if n.p.libraryErrors {
		n.rt.RouteLibraryErrors()
	}
	n.log = n.rt.Logger(componentNode)
	info := buildinfo.Get()
	n.log.InfoContext(ctx, "ruralzd starting", "version", info.Version, "commit", info.Commit, "flavor", info.Flavor,
		catalog.KeyNodeID, n.nodeID.String(), "mode", s.Mode.String(), catalog.KeyPath, s.Config, "pid", n.p.pid)
	for _, name := range s.Unused {
		n.log.WarnContext(ctx, "process setting unused in file mode", "setting", name)
	}
	if r := n.buildIdentity(ctx); r != nil {
		return r
	}
	if r := n.takeLock(ctx); r != nil {
		return r
	}
	return n.buildParts(ctx)
}

// buildIdentity reads the admin credentials and the secret root, each of
// which refuses start when unusable.
func (n *node) buildIdentity(ctx context.Context) *refusal {
	s := n.s
	var err error
	n.auth, err = adminauth.New(adminauth.Settings{
		TokenFile: s.AdminTokenFile, MetricsTokenFile: s.AdminMetricsTokenFile, TLSDir: s.AdminTLSDir,
	}, adminauth.Options{Logger: n.rt.Logger(componentAdmin), RequestID: admin.RequestID(n.rt.Tracer())})
	if err != nil {
		return refuse("admin credentials: %w", err)
	}
	if s.AdminTLSDir != "" {
		if n.adminTLS, err = tlsconf.Admin(s.AdminTLSDir); err != nil {
			return refuse("%s: %w", setting.EnvAdminTLSDir, err)
		}
	}
	protected := make([]resolver.Protected, 0, 6)
	for _, pp := range s.Protected() {
		protected = append(protected, resolver.Protected{Setting: pp.Setting, Path: pp.Path})
	}
	meter := n.rt.Meter()
	n.res, err = resolver.NewResolver(resolver.Config{
		Root: s.SecretRoot, Protected: protected, LookupEnv: n.p.lookupEnv, Clock: n.clk,
		Logger: n.rt.Logger(componentSecret), RotationFailures: meter.Node().Config.SecretRotationFail, Status: n.rt,
	})
	if err != nil {
		return refuse("%s: %w", setting.EnvSecretRoot, err)
	}
	if s.BindLoopback {
		n.hosts = listener.LoopbackHosts(ctx)
	}
	return nil
}

// takeLock takes the data-directory lock, or finds a live holder: then the
// process is a handover successor (Linux) or exits 1. The holder writes
// holder.json at once.
func (n *node) takeLock(ctx context.Context) *refusal {
	n.lock = handover.NewLock()
	nl, err := n.dir.TryLock()
	switch {
	case err == nil:
	case errors.Is(err, nodedir.ErrLocked) && handoverSupported():
		n.successor = true
		n.log.InfoContext(ctx, "a live process holds the data-directory lock: starting a handover", catalog.KeyPath, n.dir.Root())
		return nil
	default:
		return fail("%s: %w", setting.EnvDataDir, err)
	}
	if !n.lock.Take(nl) {
		return fail("%s: the data-directory lock could not be recorded", setting.EnvDataDir)
	}
	n.onClose(func() { _ = n.lock.Release() })
	rec, err := nodedir.NewHolder(n.nodeID, n.version)
	if err != nil {
		n.log.WarnContext(ctx, "holder.json record incomplete", catalog.KeyError, err.Error())
	}
	if err := nl.WriteHolder(rec); err != nil {
		return fail("%s: holder.json: %w", setting.EnvDataDir, err)
	}
	return nil
}

// buildParts builds the Node-wide parts, the activation loader and the
// Drain.
func (n *node) buildParts(ctx context.Context) *refusal {
	s, clk, rt := n.s, n.clk, n.rt
	meter := rt.Meter()
	nm := meter.Node()
	guard, err := egress.ParseAllow(s.FetchAllow)
	if err != nil {
		return refuse("%s: %w", setting.EnvFetchAllow, err)
	}
	celc, err := cel.New()
	if err != nil {
		return fail("cel: %w", err)
	}
	n.pipe, err = pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: n.p.wall.Now})
	if err != nil {
		return fail("configuration pipeline: %w", err)
	}
	n.filters, err = builtin.New(builtin.Options{
		Clock: clk, Logger: rt.Logger(componentFilter), Status: rt, Counter: meter.Counter, Guard: guard,
	})
	if err != nil {
		return fail("filters: %w", err)
	}
	n.stores = manager.New(ctx, manager.Config{
		Deps: statestore.Deps{
			Clock: clk, Metrics: &nm.State, Status: rt, NodeID: n.nodeID.String(), MACKey: s.MACKey.Bytes(),
			Logger: rt.Logger(componentStateStore), Zone: s.Zone,
		},
		Openers: manager.Openers{Memory: memory.Open, Redis: redis.Open},
	})
	n.onClose(func() { _ = n.stores.Close(context.WithoutCancel(ctx)) })
	n.ups = upstream.NewManager(upstream.Options{Clock: clk, Guard: guard, Status: rt, Tracer: rt.Tracer()})
	n.units = admission.NewUnits(admission.DefaultUnits)
	n.conns = admission.NewConnLimiter(admission.DefaultConnections)
	n.budget = body.NewBudget(body.DefaultMaxBufferedBytes, nm.BufferedBytes)
	engine := composition.New(composition.Deps{
		Clock: clk, Budget: n.budget, Units: n.units, Decoder: celc, Logger: rt.Logger(componentComposition),
	})
	exchanges := exchange.NewPool(exchange.Deps{Budget: n.budget, Decoder: celc})
	exec := executor.New(executor.Deps{Clock: n.p.wall, Tracer: rt.Tracer(), Logger: rt.Logger(componentFilter)})
	n.retirer = retire.New(retire.Config{
		Clock: clk, Wall: n.p.wall, Logger: rt.Logger(componentRetire), Stripes: meter.Stripes(), NewStripe: meter.NewStripe,
		RetiredSnapshots: nm.Config.RetiredSnapshots, RetirementEnded: nm.Config.RetirementEnded, Status: rt,
	})
	tracer := rt.Tracer()
	requestID := admin.RequestID(tracer)
	replayer, err := replay.New(replay.Deps{
		Snapshots: n.retirer, Units: n.units, Exchanges: exchanges, Executor: exec, Tracer: tracer, Clock: clk,
	})
	if err != nil {
		return fail("replay: %w", err)
	}
	compiler, err := compile.New(compile.Deps{
		Pipeline: n.pipe, CEL: celc, Filters: n.filters, Secrets: n.res, LookupEnv: n.p.lookupEnv,
		StateStores: n.stores, Upstreams: n.ups, Composition: engine, Meter: meter, Status: rt, Clock: clk,
		Logger: rt.Logger(componentCompile), Replayer: replayer,
	})
	if err != nil {
		return fail("snapshot compiler: %w", err)
	}
	hub := tap.New(tap.Config{Dropped: nm.TapEventsDropped, RequestID: requestID, Clock: n.p.wall})
	h, err := handler.New(handler.Deps{
		Snapshots: n.retirer, Units: n.units, PreRouting: admission.NewPreRouting(admission.DefaultPreRouting, n.p.wall),
		Exchanges: exchanges, Executor: exec, Tracer: tracer, AccessLog: rt.AccessLog(), Node: nm, Tap: hub,
		Clock: n.p.wall, Logger: rt.Logger(componentHandler),
	})
	if err != nil {
		return fail("request handler: %w", err)
	}
	n.tracker = readiness.New()
	n.set, err = listener.New(listener.Options{
		Handler: h, Conns: n.conns, Clock: n.p.wall, ConnStripe: n.retirer.ConnStripe,
		ErrorLog: rt.ErrorLog(componentListener), Logger: rt.Logger(componentListener), Status: rt,
		Hosts: n.hosts, Steer: !n.successor,
	})
	if err != nil {
		return fail("listeners: %w", err)
	}
	n.onClose(func() { _ = n.set.Close() })
	n.store, err = lkg.New(lkg.Config{
		Dir: n.dir, Version: n.version, MaxBytes: n.pipe.Limits().MaxSourceBytes, Clock: clk,
		Logger: rt.Logger(componentLKG), Status: rt,
	})
	if err != nil {
		return fail("last-known-good: %w", err)
	}
	if r := n.buildLoader(ctx, compiler, hub, requestID); r != nil {
		return r
	}
	return n.buildDrain(ctx)
}

// buildLoader builds the admin server and its sockets, the source watcher,
// sd_notify and the activation loader, which the admin server serves
// /config/dump and /debug/snapshots from.
func (n *node) buildLoader(ctx context.Context, compiler *compile.Compiler, hub *tap.Hub, requestID func(*http.Request) string) *refusal {
	s, clk, rt := n.s, n.clk, n.rt
	meter := rt.Meter()
	var err error
	// The loader is built below: the handlers reach it once the admin
	// server serves, after the boot started.
	var loader *reload.Loader
	n.adminSrv, err = admin.New(admin.Config{
		Auth: n.auth, TLS: n.adminTLS, Readiness: n.tracker, Tap: hub, Metrics: rt.MetricsHandler(),
		ConfigDump: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { loader.ConfigDump().ServeHTTP(w, r) }),
		Upstreams:  forward.DebugHandler(n.ups),
		Snapshots:  func() adminapi.Snapshots { return loader.Snapshots() },
		RequestID:  requestID, Status: rt, ErrorLog: rt.ErrorLog(componentAdmin), Version: n.version,
	})
	if err != nil {
		return fail("admin server: %w", err)
	}
	n.adminSock, err = reload.NewAdminSocket(reload.AdminSocketConfig{
		Server: n.adminSrv, Hosts: n.hosts, Steer: !n.successor, Fail: n.failed,
		Logger: rt.Logger(componentAdmin),
	})
	if err != nil {
		return fail("admin socket: %w", err)
	}
	n.onClose(func() { _ = n.adminSock.Close() })
	n.watcher, err = source.New(source.Config{
		Path: s.Config, Pipeline: n.pipe, Variables: n.vars, Clock: clk, Logger: rt.Logger(componentSource),
		Sink: func(ld source.Load) { loader.Offer(ld) },
	})
	if err != nil {
		return fail("configuration source: %w", err)
	}
	n.interval.env, n.interval.err = sdnotify.ReadEnv(func(k string) string { v, _ := n.p.lookupEnv(k); return v }, n.p.pid)
	if n.interval.err != nil {
		n.log.WarnContext(ctx, "watchdog settings unreadable: no watchdog", catalog.KeyError, n.interval.err.Error())
	}
	n.notifier, err = sdnotify.New(sdnotify.Options{Socket: n.interval.env.Socket, Held: n.successor, Readiness: n.tracker.Readyz})
	if err != nil {
		n.log.WarnContext(ctx, "service manager socket unusable: no notifications", catalog.KeyError, err.Error())
		n.notifier, _ = sdnotify.New(sdnotify.Options{Held: n.successor})
		n.interval.env = sdnotify.Env{}
	}
	boot := reload.BootHolder
	if n.successor {
		boot = reload.BootSuccessor
	}
	loader, err = reload.New(reload.Config{
		Boot: boot, Source: n.watcher, Pipeline: n.pipe, Compiler: reload.CompilerOf(compiler), Holder: n.retirer,
		Readiness: n.tracker, LKG: n.store, Lock: n.lock.Held,
		Parts: []reload.Part{
			reload.ListenerPart(n.set), n.adminSock, reload.TelemetryPart(rt), budgetPart{budget: n.budget},
		},
		Meter: meter, Metrics: &meter.Node().Config, Status: rt, Notifier: n.notifier, RequestID: requestID,
		Clock: clk, Rand: n.p.rand, Logger: rt.Logger(componentReload), FileLimit: n.p.fileLimit,
	})
	if err != nil {
		return fail("activation loader: %w", err)
	}
	n.loader = loader
	return nil
}

// buildDrain builds the Drain and its steps after the deadline: the admin
// server, the Filter Components, the State Store manager and the
// telemetry runtime, in that order.
func (n *node) buildDrain(ctx context.Context) *refusal {
	n.loopCtx, n.stop = context.WithCancel(context.WithoutCancel(ctx))
	n.loops = newSupervisor(n.loopCtx, n.log, n.failed)
	n.compCtx, n.stopComps = context.WithCancel(context.WithoutCancel(ctx))
	n.components = newSupervisor(n.compCtx, n.log, n.failed)
	steps := []drain.Step{
		{Name: "admin server", Run: n.stopAdmin},
		{Name: "filter components", Run: func(ctx context.Context) error {
			n.stopComps()
			return n.components.wait(ctx)
		}},
		{Name: "state store", Run: n.stores.Close},
		{Name: "telemetry", Run: n.rt.Shutdown},
	}
	var err error
	n.drainer, err = drain.New(drain.Config{
		Readiness: n.tracker, Listeners: n.set, Retirer: n.retirer, Notifier: n.notifier, Lock: n.lock,
		StopSource: n.loader.Stop, Connections: n.conns.Open, Steps: steps, Clock: n.clk, Rand: n.p.rand,
		Logger: n.rt.Logger(componentDrain),
	})
	if err != nil {
		return fail("drain: %w", err)
	}
	return nil
}

// stopAdmin is the Drain's admin server step: every /tap stream and
// profile window ends, the sockets stop accepting, the remaining requests
// finish within ctx, and the Serve goroutines end.
func (n *node) stopAdmin(ctx context.Context) error {
	err := n.adminSrv.Shutdown(ctx)
	_ = n.adminSock.Close()
	return err
}

// failed reports a part of the Node that stopped after boot: a Drain on
// the stop-signal timeline that ends with exit 1. Before the Drain exists
// it is only logged.
func (n *node) failed(err error) {
	if n.drainer == nil {
		n.log.Error("a part of the Node stopped before the Drain exists", catalog.KeyError, err.Error())
		return
	}
	n.drainer.Fail(err)
}
