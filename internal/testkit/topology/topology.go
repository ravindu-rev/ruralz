// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Runtime defaults (docs/engineering/03-testing-and-quality-strategy.md
// "Process harness", Runtime).
const (
	// DefaultReadyTimeout is the real time the harness waits for a Node's
	// /readyz to answer 200 (target), as ruralz dev run --ready-timeout.
	DefaultReadyTimeout = 30 * time.Second
	// DefaultKillAfter is the real time after SIGTERM at which a Node
	// still running gets SIGKILL (target), as systemd's TimeoutStopSec.
	DefaultKillAfter = 40 * time.Second
	// DrainStep is how far Stop advances a controllable-clock Node's clock
	// at a time while it drains, giving the Node real time to act on each
	// step's timers before the next.
	DrainStep = time.Second
	// DrainStepWait bounds the real time Stop waits for a draining Node to
	// exit after each DrainStep before advancing again.
	DrainStepWait = 50 * time.Millisecond
	// PollInterval is the period of the harness's condition polls, such as
	// the /readyz wait; it only spaces the polls, never decides an outcome.
	PollInterval = 20 * time.Millisecond
	// ArtifactDirEnv names the directory that receives a failed test's
	// evidence.
	ArtifactDirEnv = "RURALZ_TEST_ARTIFACT_DIR"
)

// Errors.
var (
	// ErrNoTestClock is returned by clock operations of a topology whose
	// Nodes run on real time (Config.TestClock unset): a test that needs
	// controlled time skips against such a build.
	ErrNoTestClock = errors.New("topology: the Nodes run on real time, not the controllable clock")
	// ErrNotRunning is returned by operations on a Node without a running
	// process.
	ErrNotRunning = errors.New("topology: the Node has no running process")
)

// Config configures a Topology.
type Config struct {
	// Binary is the Node executable: the ruralzd of
	// internal/testkit/binaries, the controllable-clock build for the
	// correctness suites, or a stand-in. Required.
	Binary string
	// Args are the Node's arguments; ruralzd takes none.
	Args []string
	// TestClock reports that Binary follows the clock commands on file
	// descriptor 3 (binaries.Set.TestClock): the harness passes each Node
	// process its command socket and keeps one clock for them all.
	TestClock bool
	// Digest returns the Revision digest ("sha256:<64 hex>") of a rendered
	// Bundle as ruralzd computes it, the configuration pipeline's. It gives
	// each Node's Revision and test digest. Required.
	Digest func(rendered []byte) (string, error)
	// Env is added to every Node process's environment after the harness's
	// own settings.
	Env []string
	// ReadyTimeout bounds the real-time wait for /readyz; default
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// KillAfter is the real time after SIGTERM at which a still running
	// Node gets SIGKILL, and the most Stop advances a controllable-clock
	// Node's clock; default DefaultKillAfter.
	KillAfter time.Duration
	// Transport turns off the TLS and credentials the harness otherwise
	// chooses on its own hops.
	Transport Transport
	// ArtifactDir receives a failed test's evidence; default
	// $RURALZ_TEST_ARTIFACT_DIR as the teardown finds it, and none when
	// both are empty.
	ArtifactDir string
}

// Transport turns off what the harness otherwise chooses on the hops it
// owns. The zero value is TLS and credentials everywhere.
type Transport struct {
	// CleartextAdmin serves the admin port without TLS: no
	// RURALZ_ADMIN_TLS_DIR. The tokens still apply, from loopback.
	CleartextAdmin bool
	// CleartextStateStore runs process State Stores without TLS
	// (redis://).
	CleartextStateStore bool
	// NoStateStoreUser runs process State Stores without an ACL user.
	NoStateStoreUser bool
	// CleartextOTLP serves OTLP sinks over h2c, an http endpoint.
	CleartextOTLP bool
}

// Topology is the Nodes and dependencies of one test. Its methods are safe
// for concurrent use.
type Topology struct {
	t   testing.TB
	cfg Config
	// ctx lives until the end of teardown: processes, proxies, mocks and
	// balancers end with it.
	ctx    context.Context
	cancel context.CancelFunc
	root   string
	ca     *pki.CA
	bundle string // SSL_CERT_FILE: the CAs Nodes trust
	clock  *Clock
	// canaries are the values the leak scan looks for.
	canaries *canarySet
	captures *captureStore

	mu    sync.Mutex
	nodes []*Node
	ports map[int]bool // every port handed out
	// held keeps the ports handed out bound until the teardown
	// (freePort).
	held       []*reuseport.Socket
	stores     map[string]*StateStore
	balancers  []*Balancer
	mocks      []*mockup.Server
	idps       []*mockidp.Server
	sinks      []*otlpsink.Sink
	transports []*http.Transport
	admin      *http.Client
	torn       bool
	// watchers detach exited processes from the clock.
	watchers sync.WaitGroup
	// schemas indexes the served apiVersions for the test render's
	// secret positions, loaded once.
	schemasOnce sync.Once
	schemas     *schemaidx.Set
	schemasErr  error

	teardownOnce sync.Once
}

// New returns an empty topology for t; Nodes and dependencies are added
// with its methods, and everything ends with t (see the package
// documentation, Teardown). It fails t on an invalid Config.
func New(t testing.TB, c Config) *Topology {
	t.Helper()
	topo, err := newTopology(t, c)
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	return topo
}

func newTopology(t testing.TB, c Config) (*Topology, error) {
	switch {
	case c.Binary == "":
		return nil, errors.New("Config.Binary is empty")
	case c.Digest == nil:
		return nil, errors.New("Config.Digest is nil")
	case c.ReadyTimeout < 0, c.KillAfter < 0:
		return nil, errors.New("negative ReadyTimeout or KillAfter")
	}
	if _, err := os.Stat(c.Binary); err != nil {
		return nil, fmt.Errorf("Config.Binary: %w", err)
	}
	if c.ReadyTimeout == 0 {
		c.ReadyTimeout = DefaultReadyTimeout
	}
	if c.KillAfter == 0 {
		c.KillAfter = DefaultKillAfter
	}
	// A short root keeps each data directory's handover.sock within the
	// 108-byte limit of a Unix socket path.
	root, err := os.MkdirTemp("", "rz-topo-")
	if err != nil {
		return nil, err
	}
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Topology Test CA"})
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	topo := &Topology{
		t:        t,
		cfg:      c,
		ctx:      ctx,
		cancel:   cancel,
		root:     root,
		ca:       ca,
		bundle:   filepath.Join(root, "ca-bundle.pem"),
		clock:    newClock(c.TestClock, c.ReadyTimeout),
		canaries: &canarySet{},
		captures: &captureStore{dir: filepath.Join(root, "captures")},
		ports:    map[int]bool{},
		stores:   map[string]*StateStore{},
	}
	if err := os.Mkdir(topo.captures.dir, 0o700); err != nil {
		cancel()
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := topo.writeBundle(); err != nil {
		cancel()
		_ = os.RemoveAll(root)
		return nil, err
	}
	t.Cleanup(topo.teardown)
	return topo, nil
}

// armTeardown registers the teardown again after a dependency registered
// its own cleanup with t, so the teardown, which runs once, still runs
// first: cleanups run last registered first.
func (t *Topology) armTeardown() { t.t.Cleanup(t.teardown) }

// Dir returns the topology's private directory.
func (t *Topology) Dir() string { return t.root }

// Clock returns the topology's one controllable clock.
func (t *Topology) Clock() *Clock { return t.clock }

// TestClock reports whether the Nodes follow the controllable clock.
func (t *Topology) TestClock() bool { return t.cfg.TestClock }

// Advance moves the clock of every Node forward by d; see Clock.Advance.
func (t *Topology) Advance(ctx context.Context, d time.Duration) error {
	return t.clock.Advance(ctx, d)
}

// Now returns the topology's clock reading; see Clock.Now.
func (t *Topology) Now() time.Time { return t.clock.Now() }

// CA returns the harness's test CA, which issues the admin, mock and sink
// certificates and which every Node trusts through SSL_CERT_FILE.
func (t *Topology) CA() *pki.CA { return t.ca }

// ClientTLS returns a client configuration trusting the harness's CA and
// every State Store's.
func (t *Topology) ClientTLS() *tls.Config {
	pool := t.ca.Pool()
	t.mu.Lock()
	for _, s := range t.stores {
		if s.caPEM != nil {
			pool.AppendCertsFromPEM(s.caPEM)
		}
	}
	t.mu.Unlock()
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// ServerTLS returns a server configuration with a certificate from the
// harness's CA for hosts (default localhost, 127.0.0.1 and ::1), for a mock
// that a Bundle reaches over TLS.
func (t *Topology) ServerTLS(hosts ...string) (*tls.Config, error) {
	if len(hosts) == 0 {
		hosts = loopbackHosts()
	}
	leaf, err := t.ca.Server(hosts...)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	cfg, err := leaf.ServerTLS(nil)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	return cfg, nil
}

// loopbackHosts are the names a loopback server answers to.
func loopbackHosts() []string { return []string{"localhost", "127.0.0.1", "::1"} }

// writeBundle writes the SSL_CERT_FILE bundle: the harness's CA and every
// State Store's.
func (t *Topology) writeBundle() error {
	b := append([]byte(nil), t.ca.CertPEM...)
	t.mu.Lock()
	for _, s := range t.stores {
		b = append(b, s.caPEM...)
	}
	t.mu.Unlock()
	tmp := t.bundle + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("topology: CA bundle: %w", err)
	}
	if err := os.Rename(tmp, t.bundle); err != nil {
		return fmt.Errorf("topology: CA bundle: %w", err)
	}
	return nil
}

// freePort returns a loopback port free on 127.0.0.1 and, where the host
// has IPv6, on ::1 (a Node under RURALZ_BIND_LOOPBACK binds both) that the
// topology has not handed out before. On Linux the topology holds the port
// until its teardown with sockets bound to it that never listen
// (reservePort): the kernel gives a bound port to no connection's
// ephemeral port and no bind of port 0, while a Node, whose sockets set
// SO_REUSEADDR, binds it and listens all the same. So no other socket
// takes the port between the render that names it and the Node's bind, or
// between a Node's exit and its restart.
func (t *Topology) freePort(ctx context.Context) (int, error) {
	const attempts = 100
	for range attempts {
		port, held, err := reservePort(ctx)
		if errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("topology: free port: %w", err)
		}
		t.mu.Lock()
		torn, taken := t.torn, t.ports[port]
		if !torn && !taken {
			t.ports[port] = true
			t.held = append(t.held, held...)
		}
		t.mu.Unlock()
		switch {
		case torn:
			closeAll(held)
			return 0, errTorn
		case !taken:
			return port, nil
		}
		closeAll(held)
	}
	return 0, fmt.Errorf("topology: no free loopback port in %d attempts", attempts)
}

// reservePort binds sockets that never listen to 127.0.0.1 on a port the
// kernel picks and, where the host has IPv6, to ::1 on the same port, and
// returns the port and the sockets holding it; an error matching
// syscall.EADDRINUSE means ::1 already uses the port. Where only listening
// sockets bind (reuseport.Bind is Linux only), it probes the port with
// listeners it closes at once and holds nothing.
func reservePort(ctx context.Context) (int, []*reuseport.Socket, error) {
	v4, err := reuseport.Bind(ctx, "127.0.0.1:0")
	if errors.Is(err, errors.ErrUnsupported) {
		port, err := probePort(ctx)
		return port, nil, err
	}
	if err != nil {
		return 0, nil, err
	}
	port := v4.Addr().(*net.TCPAddr).Port
	v6, err := reuseport.Bind(ctx, net.JoinHostPort("::1", strconv.Itoa(port)))
	switch {
	case err == nil:
		return port, []*reuseport.Socket{v4, v6}, nil
	case errors.Is(err, syscall.EADDRINUSE):
		_ = v4.Close()
		return 0, nil, err
	default:
		// Any other failure means the host has no IPv6 loopback.
		return port, []*reuseport.Socket{v4}, nil
	}
}

// probePort returns a port free on 127.0.0.1 and, where the host has
// IPv6, on ::1, found with listeners it closes at once; an error matching
// syscall.EADDRINUSE means ::1 already uses the port.
func probePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	v6, err := (&net.ListenConfig{}).Listen(ctx, "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	switch {
	case err == nil:
		_ = v6.Close()
	case errors.Is(err, syscall.EADDRINUSE):
		return 0, err
	}
	// Any other failure means the host has no IPv6 loopback.
	return port, nil
}

// Nodes returns the Nodes in creation order.
func (t *Topology) Nodes() []*Node {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*Node(nil), t.nodes...)
}

// Node returns the Node named name.
func (t *Topology) Node(name string) (*Node, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, n := range t.nodes {
		if n.name == name {
			return n, true
		}
	}
	return nil, false
}

// track records a started dependency for the teardown; it reports false
// once the teardown started, when the caller closes it itself.
func (t *Topology) track(add func()) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.torn {
		return false
	}
	add()
	return true
}

// balancersCopy returns the balancers.
func (t *Topology) balancersCopy() []*Balancer {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*Balancer(nil), t.balancers...)
}

// closeAll closes a dependency list, ignoring errors of already closed
// ones.
func closeAll[T interface{ Close() error }](xs []T) {
	for _, x := range xs {
		_ = x.Close()
	}
}
