// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// The harness's own tests run a stand-in Node
// (docs/engineering/03-testing-and-quality-strategy.md "Process harness"):
// this test binary, re-executed as a fake ruralzd. It reads RURALZ_CONFIG,
// resolves its file secrets under RURALZ_SECRET_ROOT, binds the rendered
// ports, serves /healthz, /readyz, /metrics and /debug/snapshots with the
// real admin authentication, takes the data-directory lock, Hot Reloads on
// a file swap or SIGHUP, Drains on SIGTERM, hands over to a successor over
// handover.sock, and follows the clock commands on descriptor 3. So every
// part of the harness is tested before ruralzd serves.

// Environment of the stand-in, set through Config.Env or NodeOptions.Env.
const (
	// standinEnv "1" runs the test binary as the stand-in.
	standinEnv = "RURALZ_TOPOLOGY_STANDIN"
	// standinClockEnv "1" makes it the controllable-clock build: it needs
	// its clock command socket and exits 2 without it.
	standinClockEnv = "RURALZ_TOPOLOGY_STANDIN_CLOCK"
	// standinLeakEnv "1" logs the operator token: a deliberate leak.
	standinLeakEnv = "RURALZ_TOPOLOGY_STANDIN_LEAK"
	// standinStubbornEnv "1" ignores SIGTERM, so only SIGKILL ends it.
	standinStubbornEnv = "RURALZ_TOPOLOGY_STANDIN_STUBBORN"
	// standinHangReadyzEnv "1" answers the first /readyz request and
	// holds every later one until its client gives up.
	standinHangReadyzEnv = "RURALZ_TOPOLOGY_STANDIN_HANG_READYZ"
)

// The stand-in's timeline: the Drain timeline defaults and the handover
// steps of docs/operations/02-zero-downtime-upgrades-and-hot-reload.md.
const (
	standinAcceptWindow     = 5 * time.Second
	standinDeadline         = 25 * time.Second
	standinLinger           = 3 * time.Second
	standinHandoverDeadline = 20 * time.Second
	standinHandoverGate     = 60 * time.Second
	standinWatch            = time.Second
	// standinTimer is a timer armed at boot whose firing the client
	// responses report, for the clock tests.
	standinTimer = 10 * time.Second
)

func TestMain(m *testing.M) {
	if os.Getenv(standinEnv) == "1" {
		os.Exit(runStandIn())
	}
	os.Exit(m.Run())
}

// standinDigest is the stand-in's Revision digest: SHA-256 of the file it
// loads. The harness tests pass it as Config.Digest, so the test digest
// the harness computes is the one the stand-in reports.
func standinDigest(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// standinConfig is a loaded test render.
type standinConfig struct {
	digest     string
	adminPort  int
	listeners  []standinListener
	unresolved []string // references that did not resolve
}

type standinListener struct {
	name, protocol string
	port           int
	proxyProtocol  bool
	cert           *tls.Certificate
}

// standinServer is one bound port.
type standinServer struct {
	spec standinListener
	ln   net.Listener
	srv  *http.Server
}

// standIn is the stand-in process.
type standIn struct {
	clk clock.Clock
	// start is the clock's first reading, which a ruralzd process takes
	// as its start.
	start      time.Time
	cfgPath    string
	secretRoot string
	auth       *adminauth.Authenticator
	adminTLS   *tls.Config
	dir        *nodedir.Dir
	nodeID     ulid.ULID
	exit       chan int
	logMu      sync.Mutex

	mu          sync.Mutex
	lock        *nodedir.Lock
	cfg         *standinConfig
	stamp       os.FileInfo
	servers     map[string]*standinServer
	admin       *standinServer
	handover    net.Listener
	draining    bool
	everReady   bool
	activations int
	timerFired  bool
	exited      bool
	readyzCalls int
}

func runStandIn() int {
	s := &standIn{exit: make(chan int, 1), servers: map[string]*standinServer{}}
	s.clk = clock.Real()
	if os.Getenv(standinClockEnv) == "1" {
		c, err := clock.OpenCommanded(clock.CommandFD)
		if err != nil {
			s.logf("%v", err)
			return 2
		}
		s.clk = c
	}
	s.start = s.clk.Now()
	s.cfgPath, s.secretRoot = os.Getenv(envConfig), os.Getenv(envSecretRoot)
	dataDir := os.Getenv(envDataDir)
	if s.cfgPath == "" || s.secretRoot == "" || dataDir == "" || os.Getenv(envBindLoopback) != "true" {
		s.logf("RURALZ_CONFIG, RURALZ_SECRET_ROOT, RURALZ_DATA_DIR and RURALZ_BIND_LOOPBACK=true are required")
		return 2
	}
	s.logf("env GOMAXPROCS=%s procs=%d %s=%s %s=%s SSL_CERT_FILE=%s HOME=%s",
		os.Getenv("GOMAXPROCS"), runtime.GOMAXPROCS(0), envBindLoopback, os.Getenv(envBindLoopback),
		envFetchAllow, os.Getenv(envFetchAllow), os.Getenv("SSL_CERT_FILE"), os.Getenv("HOME"))
	settings, err := adminauth.LoadSettings(os.Getenv)
	if err == nil {
		s.auth, err = adminauth.New(settings, adminauth.Options{})
	}
	if err == nil && settings.TLS() {
		s.adminTLS, err = tlsconf.Admin(settings.TLSDir)
	}
	if err != nil {
		s.logf("admin settings: %v", err)
		return 2
	}
	if os.Getenv(standinLeakEnv) == "1" {
		token, _ := os.ReadFile(settings.TokenFile)
		s.logf("operator token %s", token)
	}
	if s.dir, err = nodedir.Open(dataDir); err == nil {
		s.nodeID, err = s.dir.NodeID(func() (ulid.ULID, error) { return ulid.New(s.clk.Now(), nil) })
	}
	if err != nil {
		s.logf("data directory: %v", err)
		return 2
	}
	sigs := make(chan os.Signal, 4)
	if os.Getenv(standinStubbornEnv) == "1" {
		signal.Ignore(syscall.SIGTERM)
		signal.Notify(sigs, syscall.SIGHUP)
	} else {
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	}
	defer signal.Stop(sigs)
	cfg, err := s.load()
	if err != nil {
		s.logf("%v", err)
		return 2
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	lock, err := s.dir.TryLock()
	switch {
	case errors.Is(err, nodedir.ErrLocked):
		if code := s.handoverIn(); code != 0 {
			return code
		}
	case err != nil:
		s.logf("lock: %v", err)
		return 1
	default:
		if code := s.boot(lock); code != 0 {
			return code
		}
	}
	s.clk.AfterFunc(standinTimer, func() {
		s.mu.Lock()
		s.timerFired = true
		s.mu.Unlock()
	})
	watch := s.clk.NewTimer(standinWatch)
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				s.reload("SIGHUP")
			} else {
				s.drain(false)
			}
		case <-watch.C():
			s.reload("")
			watch.Reset(standinWatch)
		case code := <-s.exit:
			s.mu.Lock()
			if s.lock != nil {
				_ = s.lock.Release()
			}
			s.mu.Unlock()
			s.logf("exit %d", code)
			return code
		}
	}
}

// boot serves as the lock holder.
func (s *standIn) boot(lock *nodedir.Lock) int {
	h, _ := nodedir.NewHolder(s.nodeID, "0.0.0-standin")
	if err := lock.WriteHolder(h); err != nil {
		s.logf("holder: %v", err)
		return 1
	}
	s.mu.Lock()
	s.lock = lock
	s.mu.Unlock()
	if err := s.bindAll(); err != nil {
		s.logf("%v", err)
		return 1
	}
	// A socket left by a killed holder is stale: this process holds the
	// lock.
	_ = os.Remove(s.dir.HandoverSocketPath()) //nolint:gosec // G703: a fixed name under RURALZ_DATA_DIR
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", s.dir.HandoverSocketPath())
	if err != nil {
		s.logf("handover socket: %v", err)
		return 1
	}
	s.mu.Lock()
	s.handover = ln
	s.mu.Unlock()
	go s.acceptHandovers(ln)
	s.reportReady("ready")
	return 0
}

// load reads and parses RURALZ_CONFIG and resolves its secrets.
func (s *standIn) load() (*standinConfig, error) {
	data, err := os.ReadFile(s.cfgPath) //nolint:gosec // G703: RURALZ_CONFIG, the test render the harness wrote
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if fi, err := os.Stat(s.cfgPath); err == nil { //nolint:gosec // G703: see above
		s.mu.Lock()
		s.stamp = fi
		s.mu.Unlock()
	}
	docs, diags, err := profile.Parse(context.Background(), data, 1, s.cfgPath, profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		return nil, fmt.Errorf("config does not parse: %v %s", err, diagText(diags))
	}
	cfg := &standinConfig{adminPort: DefaultAdminPort}
	cfg.digest, _ = standinDigest(data)
	for _, d := range docs {
		var refs []*tree.Node
		collectRefs(d.Root, &refs)
		for _, ref := range refs {
			if _, err := s.resolve(ref); err != nil {
				cfg.unresolved = append(cfg.unresolved, refString(text(ref, "provider"), text(ref, "name"), text(ref, "key")))
			}
		}
		if text(d.Root, "kind") != "Gateway" {
			continue
		}
		spec, _ := d.Root.Get("spec")
		if admin, ok := spec.Get("admin"); ok {
			cfg.adminPort, _ = intMember(admin, "port", DefaultAdminPort)
		}
		ls, _ := spec.Get("listeners")
		if ls == nil {
			continue
		}
		for _, l := range ls.Items {
			port, _ := intMember(l, "port", 0)
			sl := standinListener{name: text(l, "name"), protocol: text(l, "protocol"), port: port}
			if pp, ok := l.Get("proxyProtocol"); ok && pp.Kind == tree.KindBool {
				sl.proxyProtocol = pp.Bool
			}
			if sl.protocol == "https" {
				cert, err := s.listenerCert(l)
				switch {
				case errors.Is(err, errUnresolved):
					// The unresolved references keep the Node unready.
				case err != nil:
					return nil, fmt.Errorf("listener %s: %w", sl.name, err)
				default:
					sl.cert = cert
				}
			}
			cfg.listeners = append(cfg.listeners, sl)
		}
	}
	return cfg, nil
}

// errUnresolved reports a listener certificate whose secrets did not
// resolve.
var errUnresolved = errors.New("certificate secrets unresolved")

// listenerCert builds an https listener's first certificate from its
// secrets.
func (s *standIn) listenerCert(l *tree.Node) (*tls.Certificate, error) {
	tlsNode, _ := l.Get("tls")
	certs, _ := tlsNode.Get("certificates")
	if certs == nil || len(certs.Items) == 0 {
		return nil, errors.New("no certificate")
	}
	c := certs.Items[0]
	certRef, _ := c.Get("certificate")
	keyRef, _ := c.Get("privateKey")
	cr, _ := certRef.Get("secretRef")
	kr, _ := keyRef.Get("secretRef")
	certPEM, err := s.resolve(cr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnresolved, err)
	}
	keyPEM, err := s.resolve(kr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnresolved, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &pair, nil
}

// collectRefs appends every secretRef object under n.
func collectRefs(n *tree.Node, out *[]*tree.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case tree.KindMap:
		for _, m := range n.Members {
			if m.Key == "secretRef" && m.Value.Kind == tree.KindMap {
				*out = append(*out, m.Value)
				continue
			}
			collectRefs(m.Value, out)
		}
	case tree.KindList:
		for _, it := range n.Items {
			collectRefs(it, out)
		}
	default:
	}
}

// resolve reads a file secret under RURALZ_SECRET_ROOT, a JSON member for
// a keyed reference, as a Node's file provider does.
func (s *standIn) resolve(ref *tree.Node) ([]byte, error) {
	provider, name, key := text(ref, "provider"), text(ref, "name"), text(ref, "key")
	if provider != "file" {
		return nil, fmt.Errorf("provider %q is not file", provider)
	}
	rel, err := filepath.Rel(s.secretRoot, name)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s lies outside %s", name, envSecretRoot)
	}
	data, err := os.ReadFile(name) //nolint:gosec // G304: a path under the secret root, checked above
	if err != nil {
		return nil, err
	}
	if key == "" {
		return data, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	v, ok := obj[key].(string)
	if !ok {
		return nil, fmt.Errorf("no string member %q", key)
	}
	return []byte(v), nil
}

// bindAll binds the admin port and, when every secret resolved, the
// client listeners.
func (s *standIn) bindAll() error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if err := s.bindAdmin(cfg.adminPort); err != nil {
		return err
	}
	if len(cfg.unresolved) > 0 {
		return nil
	}
	for _, l := range cfg.listeners {
		if err := s.bindListener(l); err != nil {
			return err
		}
	}
	return nil
}

func (s *standIn) bindAdmin(port int) error {
	ln, err := reuseport.Listen(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("admin port %d: %w", port, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	hang := os.Getenv(standinHangReadyzEnv) == "1"
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.readyzCalls++
		held := hang && s.readyzCalls > 1
		s.mu.Unlock()
		if held {
			<-r.Context().Done()
			return
		}
		doc := s.readyz()
		status := http.StatusOK
		if doc.Status != "ready" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, doc)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		n := s.activations
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n%s{result=\"success\"} %d\n", catalog.ConfigActivationsTotal, catalog.ConfigActivationsTotal, n)
	})
	mux.HandleFunc("/debug/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		digest := s.cfg.digest
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, adminapi.Snapshots{Snapshots: []adminapi.SnapshotInfo{{
			State: adminapi.StateActive, Revision: DisplayRevision(digest), Digest: digest,
			ActivatedAt: s.clk.Now().UTC().Format(time.RFC3339),
		}}})
	})
	mux.HandleFunc("/", http.NotFound)
	srv := &http.Server{Handler: s.auth.Middleware(mux), ReadHeaderTimeout: 10 * time.Second, TLSConfig: s.adminTLS}
	s.serve(srv, ln, s.adminTLS != nil)
	s.mu.Lock()
	s.admin = &standinServer{ln: ln, srv: srv}
	s.mu.Unlock()
	return nil
}

func (s *standIn) bindListener(l standinListener) error {
	tcp, err := reuseport.Listen(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(l.port)))
	if err != nil {
		return fmt.Errorf("listener %s port %d: %w", l.name, l.port, err)
	}
	var ln net.Listener = tcp
	if l.proxyProtocol {
		if ln, err = clientaddr.NewListener(tcp, clientaddr.ListenerOptions{Clock: clock.Real()}); err != nil {
			_ = tcp.Close()
			return err
		}
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext:       func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
	}
	if l.cert != nil {
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*l.cert}, MinVersion: tls.VersionTLS12}
	}
	s.serve(srv, ln, l.cert != nil)
	s.mu.Lock()
	s.servers[l.name] = &standinServer{spec: l, ln: ln, srv: srv}
	s.mu.Unlock()
	return nil
}

// connKey keys a request's connection in its context.
type connKey struct{}

// serve runs srv on ln until the server closes.
func (s *standIn) serve(srv *http.Server, ln net.Listener, useTLS bool) {
	go func() {
		if useTLS {
			_ = srv.ServeTLS(ln, "", "")
			return
		}
		_ = srv.Serve(ln)
	}()
}

// handle answers client requests with what the harness tests check.
func (s *standIn) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rev, fired := DisplayRevision(s.cfg.digest), s.timerFired
	s.mu.Unlock()
	client := r.RemoteAddr
	if c, ok := r.Context().Value(connKey{}).(net.Conn); ok {
		if peer := clientaddr.PeerOf(c); peer.IsValid() {
			client = peer.String()
		}
	}
	timer := "pending"
	if fired {
		timer = "fired"
	}
	w.Header().Set("X-Standin-Revision", rev)
	w.Header().Set("X-Standin-Pid", strconv.Itoa(os.Getpid()))
	w.Header().Set("X-Standin-Now", s.clk.Now().UTC().Format(clock.CommandTimeLayout))
	w.Header().Set("X-Standin-Start", s.start.UTC().Format(clock.CommandTimeLayout))
	w.Header().Set("X-Standin-Timer", timer)
	w.Header().Set("X-Standin-Client", client)
	_, _ = io.WriteString(w, "stand-in "+rev+"\n")
}

// readyz returns the /readyz document.
func (s *standIn) readyz() adminapi.Readyz {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.draining:
		return adminapi.Readyz{Status: "not_ready", Reasons: []adminapi.ReadyReason{{Reason: adminapi.ReasonDraining}}}
	case len(s.cfg.unresolved) > 0:
		reasons := []adminapi.ReadyReason{{Reason: adminapi.ReasonNoRevision}}
		for _, ref := range s.cfg.unresolved {
			reasons = append(reasons, adminapi.ReadyReason{Reason: adminapi.ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: ref})
		}
		return adminapi.Readyz{Status: "not_ready", Reasons: reasons}
	case len(s.servers) < len(s.cfg.listeners):
		return adminapi.Readyz{Status: "not_ready", Reasons: []adminapi.ReadyReason{{Reason: adminapi.ReasonListenersUnbound, Code: "RZ-CFG-039"}}}
	}
	s.everReady = true
	return adminapi.Readyz{Status: "ready", Revision: DisplayRevision(s.cfg.digest)}
}

// reportReady logs the readiness after a boot or an activation.
func (s *standIn) reportReady(what string) {
	doc := s.readyz()
	s.logf("%s %s %s", what, doc.Status, doc.Revision)
}

// reload loads RURALZ_CONFIG again when it changed (or always on
// SIGHUP), or when secrets were unresolved, and activates it.
func (s *standIn) reload(why string) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return
	}
	prev, cur := s.stamp, s.cfg
	s.mu.Unlock()
	if why == "" && len(cur.unresolved) == 0 {
		fi, err := os.Stat(s.cfgPath) //nolint:gosec // G703: RURALZ_CONFIG, the test render the harness wrote
		if err != nil || (prev != nil && os.SameFile(prev, fi) && fi.ModTime().Equal(prev.ModTime()) && fi.Size() == prev.Size()) {
			return
		}
	}
	cfg, err := s.load()
	if err != nil {
		s.logf("rejected: %v", err)
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.activations++
	s.mu.Unlock()
	if len(cfg.unresolved) == 0 {
		for _, l := range cfg.listeners {
			s.mu.Lock()
			bound := s.servers[l.name]
			s.mu.Unlock()
			if bound != nil && bound.spec.port == l.port {
				continue
			}
			if err := s.bindListener(l); err != nil {
				s.logf("activate: %v", err)
			}
		}
	}
	s.reportReady("active")
}

// drain runs the Drain: a first stop signal fails /readyz, releases the
// lock and stops accepting after the accept window; a second jumps to the
// deadline. A handover's Drain starts after the linger with listeners
// already closed.
func (s *standIn) drain(handover bool) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		s.finish()
		return
	}
	s.draining = true
	lock := s.lock
	s.lock = nil
	skip := !s.everReady && len(s.servers) == 0
	s.mu.Unlock()
	if lock != nil {
		_ = lock.Release()
	}
	s.logf("draining")
	if skip {
		s.finish()
		return
	}
	deadline := standinDeadline
	if handover {
		deadline = standinHandoverDeadline
		s.shutdownClients()
	} else {
		s.clk.AfterFunc(standinAcceptWindow, func() {
			s.logf("stop accepting")
			s.closeListeners(false)
			s.shutdownClients()
		})
	}
	s.clk.AfterFunc(deadline, s.finish)
}

// closeListeners stops accepting on the client listeners and, for a
// handover, the admin port too.
func (s *standIn) closeListeners(admin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.servers {
		_ = srv.ln.Close()
	}
	if admin && s.admin != nil {
		_ = s.admin.ln.Close()
	}
}

// shutdownClients waits for the client servers' connections to end, then
// finishes the Drain.
func (s *standIn) shutdownClients() {
	s.mu.Lock()
	servers := make([]*standinServer, 0, len(s.servers))
	for _, srv := range s.servers {
		servers = append(servers, srv)
	}
	s.mu.Unlock()
	go func() {
		for _, srv := range servers {
			_ = srv.srv.Shutdown(context.Background())
		}
		s.finish()
	}()
}

// finish ends the process with exit code 0.
func (s *standIn) finish() {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	if s.handover != nil {
		_ = s.handover.Close()
	}
	s.mu.Unlock()
	s.exit <- 0
}

// handoverMsg is a line of the stand-in's handover exchange, the message
// shapes of ruralz.handover.v1.
type handoverMsg struct {
	Type         string `json:"type"`
	PID          int    `json:"pid,omitempty"`
	ActiveDigest string `json:"activeDigest,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// acceptHandovers serves successors on handover.sock: ready is accepted
// unless draining; after listening the holder lingers, closes its
// listeners, starts its handover Drain and sends released.
func (s *standIn) acceptHandovers(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.serveHandover(c)
	}
}

func (s *standIn) serveHandover(c net.Conn) {
	r := bufio.NewScanner(c)
	enc := json.NewEncoder(c)
	for r.Scan() {
		var m handoverMsg
		if json.Unmarshal(r.Bytes(), &m) != nil {
			_ = c.Close()
			return
		}
		switch m.Type {
		case "ready":
			s.mu.Lock()
			draining := s.draining
			s.mu.Unlock()
			if draining {
				_ = enc.Encode(handoverMsg{Type: "refused", Reason: "draining"})
				_ = c.Close()
				return
			}
			_ = enc.Encode(handoverMsg{Type: "accepted"})
		case "listening":
			s.clk.AfterFunc(standinLinger, func() {
				s.closeListeners(true)
				// Closing the socket unlinks its file, before the
				// successor creates its own.
				s.mu.Lock()
				if s.handover != nil {
					_ = s.handover.Close()
					s.handover = nil
				}
				s.mu.Unlock()
				s.drain(true)
				_ = enc.Encode(handoverMsg{Type: "released"})
				_ = c.Close()
			})
			s.logf("lingering")
		}
	}
}

// handoverIn takes over from the lock holder as its successor.
func (s *standIn) handoverIn() int {
	var c net.Conn
	start := s.clk.Now()
	for {
		var err error
		c, err = (&net.Dialer{}).DialContext(context.Background(), "unix", s.dir.HandoverSocketPath())
		if err == nil {
			break
		}
		if s.clk.Since(start) >= standinHandoverGate {
			s.logf("handover gate: %v", err)
			return 1
		}
		_ = s.clk.Sleep(context.Background(), time.Second)
	}
	defer func() { _ = c.Close() }()
	r := bufio.NewScanner(c)
	enc := json.NewEncoder(c)
	s.mu.Lock()
	digest := s.cfg.digest
	s.mu.Unlock()
	if err := enc.Encode(handoverMsg{Type: "ready", PID: os.Getpid(), ActiveDigest: digest}); err != nil {
		return 1
	}
	var m handoverMsg
	if !r.Scan() || json.Unmarshal(r.Bytes(), &m) != nil || m.Type != "accepted" {
		s.logf("handover refused: %+v", m)
		return 1
	}
	if err := s.bindAll(); err != nil {
		s.logf("%v", err)
		return 1
	}
	if err := enc.Encode(handoverMsg{Type: "listening"}); err != nil {
		return 1
	}
	s.logf("listening as successor")
	for r.Scan() {
		if json.Unmarshal(r.Bytes(), &m) == nil && m.Type == "released" {
			break
		}
	}
	lock, err := s.dir.TryLock()
	if err != nil {
		s.logf("lock after release: %v", err)
		return 1
	}
	if code := s.takeLock(lock); code != 0 {
		return code
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", s.dir.HandoverSocketPath())
	if err == nil {
		s.mu.Lock()
		s.handover = ln
		s.mu.Unlock()
		go s.acceptHandovers(ln)
	}
	s.reportReady("holder")
	return 0
}

// takeLock records the lock and writes holder.json.
func (s *standIn) takeLock(lock *nodedir.Lock) int {
	h, _ := nodedir.NewHolder(s.nodeID, "0.0.0-standin")
	_ = os.Remove(s.dir.HandoverSocketPath()) //nolint:gosec // G703: a fixed name under RURALZ_DATA_DIR
	if err := lock.WriteHolder(h); err != nil {
		s.logf("holder: %v", err)
		return 1
	}
	s.mu.Lock()
	s.lock = lock
	s.mu.Unlock()
	return 0
}

// logf writes one stdout line; it never writes a secret value unless the
// leak mode asks for one.
func (s *standIn) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = fmt.Fprintf(os.Stdout, "standin: "+format+"\n", args...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
