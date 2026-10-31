// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// Process settings the harness gives every Node (pack section 2).
//
//nolint:gosec // G101: environment variable names, not credentials.
const (
	envConfig           = "RURALZ_CONFIG"
	envDataDir          = "RURALZ_DATA_DIR"
	envSecretRoot       = "RURALZ_SECRET_ROOT"
	envAdminTokenFile   = "RURALZ_ADMIN_TOKEN_FILE"
	envMetricsTokenFile = "RURALZ_ADMIN_METRICS_TOKEN_FILE"
	envAdminTLSDir      = "RURALZ_ADMIN_TLS_DIR"
	envMACKeyFile       = "RURALZ_STATE_STORE_MAC_KEY_FILE"
	envBindLoopback     = "RURALZ_BIND_LOOPBACK"
	envFetchAllow       = "RURALZ_FETCH_ALLOW"
	// fetchAllowLoopback lets Nodes reach the loopback mocks, as ruralz
	// dev run sets it.
	fetchAllowLoopback = "127.0.0.0/8,::1"
)

// NodeOptions configure Nodes.
type NodeOptions struct {
	// Bundle is the rendered Bundle the Node serves, the YAML output of
	// ruralz bundle render, holding exactly one Gateway. Required.
	Bundle []byte
	// Secrets maps secretRef references, "provider:name" or
	// "provider:name#key" as internal/secret.Ref prints them, to values,
	// each carrying a canary (Topology.Canary, WithCanary,
	// ServerCertificate). The value of "provider:name" is the whole file,
	// which keyed references read as a JSON object; otherwise the file is
	// a JSON object of the keyed references' values. A reference without a
	// value gets a fresh canary, which SecretValue returns.
	Secrets map[string][]byte
	// MissingSecrets lists references whose value the harness leaves out,
	// a "provider:name" file or one key of it, so the Node cannot resolve
	// them (RZ-CFG-026).
	MissingSecrets []string
	// AuthoredSecrets lists references the test render keeps as authored,
	// "provider:name#key" for one reference or "provider:name" for every
	// key of a name: the harness neither rewrites them to files under
	// RURALZ_SECRET_ROOT nor writes their values, so a case can exercise
	// the env provider and the secret root and file mode rules. The test
	// supplies their values, an environment variable through Env or a
	// file it writes, each carrying a canary.
	AuthoredSecrets []string
	// StateStoreURLs maps references to State Store names
	// (StartStateStore): each holds that State Store's URL through the
	// Node's own proxies, with the ACL user's password canary.
	StateStoreURLs map[string]string
	// MACKey, when set, is the content of the Node's
	// RURALZ_STATE_STORE_MAC_KEY_FILE; it must carry a canary (a
	// Topology.Canary alone is a 41-byte key).
	MACKey []byte
	// Env is added to the Node process's environment after Config.Env.
	Env []string
	// GOMAXPROCS overrides max(1, CPUs / Nodes), CPUs being the
	// Config.CPUs cpuset's when set; 0 keeps the rule.
	GOMAXPROCS int
	// NoWait returns once the process started, without waiting for
	// /readyz.
	NoWait bool
}

// Node is one ruralzd of the topology: its private directory, its test
// render and its processes, a handover successor beside the holder
// included. Its methods are safe for concurrent use.
type Node struct {
	t    *Topology
	name string
	opts NodeOptions

	dir, configPath, dataDir, secretRoot, logDir, adminTLSDir string
	tokenPath, metricsTokenPath, macKeyPath                   string
	operatorToken, metricsToken                               string
	gomaxprocs                                                int

	mu         sync.Mutex
	ports      map[string]int // listener name to port, kept across renders
	adminPort  int
	render     *Render
	secrets    map[string][]byte // reference to value
	storeURLs  map[string]string // reference to State Store URL
	links      map[string]*storeLink
	cur        *proc.Process // the process serving the Node
	procs      []*proc.Process
	successors int
	snapshots  []byte // /debug/snapshots fetched at teardown
}

// AddNodes adds n Nodes on the same options, named node-1, node-2 and so
// on in creation order, starts them and, unless o.NoWait, waits until each
// is ready. Nodes added after a balancer started join it. With more than
// one Node in the topology, each new Node runs with GOMAXPROCS
// max(1, CPUs / Nodes), CPUs being those of the Config.CPUs cpuset when
// set, unless o.GOMAXPROCS overrides it.
func (t *Topology) AddNodes(ctx context.Context, n int, o NodeOptions) ([]*Node, error) {
	if n < 1 {
		return nil, fmt.Errorf("topology: AddNodes(%d)", n)
	}
	if len(o.Bundle) == 0 {
		return nil, errors.New("topology: NodeOptions.Bundle is empty")
	}
	if err := t.checkSecretEnv(slices.Concat(t.cfg.Env, o.Env)); err != nil {
		return nil, err
	}
	if o.MACKey != nil && !t.canaries.carries(o.MACKey) {
		return nil, errors.New("topology: NodeOptions.MACKey carries no canary from Topology.Canary")
	}
	t.mu.Lock()
	if t.torn {
		t.mu.Unlock()
		return nil, errors.New("topology: torn down")
	}
	first := len(t.nodes)
	nodes := make([]*Node, n)
	for i := range nodes {
		nodes[i] = &Node{
			t: t, name: "node-" + strconv.Itoa(first+i+1), opts: o,
			ports: map[string]int{}, secrets: map[string][]byte{}, storeURLs: map[string]string{}, links: map[string]*storeLink{},
		}
	}
	t.nodes = append(t.nodes, nodes...)
	count := len(t.nodes)
	t.mu.Unlock()
	for _, node := range nodes {
		node.gomaxprocs = o.GOMAXPROCS
		if node.gomaxprocs == 0 && count > 1 {
			node.gomaxprocs = max(1, t.cpuCount()/count)
		}
		if err := node.prepare(ctx); err != nil {
			return nil, err
		}
		if err := node.start(ctx); err != nil {
			return nil, err
		}
	}
	if !o.NoWait {
		for _, node := range nodes {
			if err := node.WaitReady(ctx); err != nil {
				return nil, err
			}
		}
	}
	for _, b := range t.balancersCopy() {
		for _, node := range nodes {
			if err := b.add(ctx, node); err != nil {
				return nil, err
			}
		}
	}
	return nodes, nil
}

// prepare creates the Node's directory, credentials, State Store links
// and first test render.
func (n *Node) prepare(ctx context.Context) error {
	t := n.t
	n.dir = filepath.Join(t.root, n.name)
	n.configPath = filepath.Join(n.dir, "bundle.yaml")
	n.dataDir = filepath.Join(n.dir, "data")
	n.secretRoot = filepath.Join(n.dir, "secrets")
	n.logDir = filepath.Join(n.dir, "logs")
	n.tokenPath = filepath.Join(n.dir, "admin-token")
	n.metricsTokenPath = filepath.Join(n.dir, "metrics-token")
	for _, d := range []string{n.dir, n.dataDir, n.secretRoot, n.logDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
	}
	var err error
	if n.operatorToken, err = t.newCanary(); err != nil {
		return err
	}
	if n.metricsToken, err = t.newCanary(); err != nil {
		return err
	}
	for path, v := range map[string]string{n.tokenPath: n.operatorToken, n.metricsTokenPath: n.metricsToken} {
		if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
	}
	if !t.cfg.Transport.CleartextAdmin {
		if err := n.writeAdminTLS(); err != nil {
			return err
		}
	}
	if n.opts.MACKey != nil {
		n.macKeyPath = filepath.Join(n.dir, "mac-key")
		if err := os.WriteFile(n.macKeyPath, n.opts.MACKey, 0o600); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
	}
	for ref, store := range n.opts.StateStoreURLs {
		s, ok := t.stateStore(store)
		if !ok {
			return fmt.Errorf("topology: %s: secret %s names no State Store %q", n.name, ref, store)
		}
		link, err := s.link(ctx, n)
		if err != nil {
			return err
		}
		n.mu.Lock()
		n.links[store] = link
		n.storeURLs[ref] = link.url
		n.mu.Unlock()
	}
	_, err = n.Reload(ctx, n.opts.Bundle)
	return err
}

// writeAdminTLS writes RURALZ_ADMIN_TLS_DIR: a loopback server certificate
// from the harness's CA, its key carrying a canary, and the CA as ca.crt,
// which admits client certificates it issued.
func (n *Node) writeAdminTLS() error {
	n.adminTLSDir = filepath.Join(n.dir, "admin-tls")
	if err := os.Mkdir(n.adminTLSDir, 0o700); err != nil {
		return fmt.Errorf("topology: %s: %w", n.name, err)
	}
	leaf, err := n.t.ca.Server(loopbackHosts()...)
	if err != nil {
		return fmt.Errorf("topology: %s: %w", n.name, err)
	}
	for name, data := range map[string][]byte{
		"tls.crt": leaf.CertPEM,
		"tls.key": n.t.WithCanary(leaf.KeyPEM),
		"ca.crt":  n.t.ca.CertPEM,
	} {
		if err := os.WriteFile(filepath.Join(n.adminTLSDir, name), data, 0o600); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
	}
	return nil
}

// env returns the Node process's complete environment.
func (n *Node) env() []string {
	t := n.t
	env := []string{
		envConfig + "=" + n.configPath,
		envDataDir + "=" + n.dataDir,
		envSecretRoot + "=" + n.secretRoot,
		envAdminTokenFile + "=" + n.tokenPath,
		envMetricsTokenFile + "=" + n.metricsTokenPath,
		envBindLoopback + "=true",
		envFetchAllow + "=" + fetchAllowLoopback,
		"SSL_CERT_FILE=" + t.bundle,
	}
	if n.adminTLSDir != "" {
		env = append(env, envAdminTLSDir+"="+n.adminTLSDir)
	}
	if n.macKeyPath != "" {
		env = append(env, envMACKeyFile+"="+n.macKeyPath)
	}
	if n.gomaxprocs > 0 {
		env = append(env, "GOMAXPROCS="+strconv.Itoa(n.gomaxprocs))
	}
	// Windows sockets need SYSTEMROOT; elsewhere it is unset.
	if v := os.Getenv("SYSTEMROOT"); v != "" {
		env = append(env, "SYSTEMROOT="+v)
	}
	n.mu.Lock()
	extra := slices.Clone(n.opts.Env)
	n.mu.Unlock()
	env = append(env, t.cfg.Env...)
	return append(env, extra...)
}

// Setenv sets the variable key to value in the environment the Node's
// next process starts with (NodeOptions.Env, after Config.Env), replacing
// an earlier value, as an operator changes a Node's setting before a
// restart: a running process keeps the environment it started with, so a
// value read from the environment, such as a State Store URL from an env
// secret, changes only with a restart. A secret variable's value must
// carry a canary, as in NodeOptions.Env.
func (n *Node) Setenv(key, value string) error {
	if key == "" || strings.ContainsRune(key, '=') {
		return fmt.Errorf("topology: %s: variable name %q", n.name, key)
	}
	kv := key + "=" + value
	if err := n.t.checkSecretEnv([]string{kv}); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	env := slices.DeleteFunc(slices.Clone(n.opts.Env), func(e string) bool {
		k, _, _ := strings.Cut(e, "=")
		return k == key
	})
	env = append(env, kv)
	n.opts.Env = env
	return nil
}

// startProcess starts one process of the Node from binary, in a scope on
// Config.CPUs when set, passes it its clock command socket on a
// controllable-clock topology and attaches it to the topology's clock.
func (n *Node) startProcess(ctx context.Context, binary, name string) (*proc.Process, error) {
	t := n.t
	spec := proc.Spec{Name: name, Path: binary, Args: t.cfg.Args, Env: n.env(), LogDir: n.logDir}
	if sc := t.scope; sc != nil {
		// systemd-run executes the Node in place, so the process proc
		// starts, signals and samples is the Node's, file descriptor 3
		// included; the Node's own settings follow the user manager's
		// variables, so they win.
		spec.Path, spec.Args = sc.run, ScopeArgs(t.cfg.CPUs, append([]string{binary}, t.cfg.Args...)...)
		spec.Env = append(slices.Clone(sc.env), spec.Env...)
	}
	var conn net.Conn
	var child *os.File
	if t.cfg.TestClock {
		var err error
		if conn, child, err = commandSocket(); err != nil {
			return nil, err
		}
		spec.ExtraFiles = []*os.File{child}
	}
	p, err := proc.Start(t.ctx, spec) //nolint:contextcheck // The process outlives the call: it runs until the teardown cancels the topology's context.
	// The child has its own copy now. Closing this one at once makes the
	// socket end when the child exits, which tells a dying Node from a
	// slow one.
	if child != nil {
		_ = child.Close()
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, fmt.Errorf("topology: %w", err)
	}
	if conn != nil {
		if err := t.clock.attach(ctx, name, p, conn); err != nil {
			_ = p.Kill()
			<-p.Done()
			_ = conn.Close()
			return nil, fmt.Errorf("%w%s", err, outputTail(p))
		}
	}
	n.mu.Lock()
	n.procs = append(n.procs, p)
	n.mu.Unlock()
	t.watchers.Add(1)
	go func() {
		defer t.watchers.Done()
		<-p.Done()
		t.clock.detach(p)
	}()
	return p, nil
}

// start starts the Node's serving process under the Node's name, whose
// log files a restart appends to.
func (n *Node) start(ctx context.Context) error {
	n.mu.Lock()
	running := n.cur != nil && n.cur.State() == nil
	n.mu.Unlock()
	if running {
		return fmt.Errorf("topology: %s is already running", n.name)
	}
	p, err := n.startProcess(ctx, n.t.cfg.Binary, n.name)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.cur = p
	n.mu.Unlock()
	return nil
}

// Start starts the Node again after Stop or Kill, on the same directory,
// ports and test render, so it boots its Last-Known-Good as a restarted
// Node does, and waits until it is ready unless NodeOptions.NoWait.
func (n *Node) Start(ctx context.Context) error {
	if err := n.start(ctx); err != nil {
		return err
	}
	if n.opts.NoWait {
		return nil
	}
	return n.WaitReady(ctx)
}

// Restart stops the Node (Stop) and starts it again (Start).
func (n *Node) Restart(ctx context.Context) error {
	if _, err := n.Stop(ctx); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	return n.Start(ctx)
}

// Stop ends the Node's serving process as a process manager does: SIGTERM,
// which starts its Drain, then, on a controllable-clock topology, the
// topology's clock advanced DrainStep at a time until the process exits,
// at most Config.KillAfter, and SIGKILL when it still runs Config.KillAfter
// of real time after the SIGTERM. Advancing moves every Node's clock, as
// the one clock does. It returns the process's exit code, -1 when a
// signal ended it.
func (n *Node) Stop(ctx context.Context) (int, error) {
	p := n.Process()
	if p == nil {
		return -1, ErrNotRunning
	}
	n.t.drain(ctx, []*proc.Process{p})
	return p.State().ExitCode(), nil
}

// Kill sends SIGKILL to the Node's serving process and waits for its end.
func (n *Node) Kill(ctx context.Context) error {
	p := n.Process()
	if p == nil {
		return ErrNotRunning
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	if _, err := p.Wait(ctx); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	n.t.clock.detach(p)
	return nil
}

// Signal sends sig to the Node's serving process. SIGSTOP and SIGCONT keep
// the clock consistent: a stopped Node misses advances and catches up when
// resumed, so send them through Signal rather than to the process.
func (n *Node) Signal(ctx context.Context, sig os.Signal) error {
	p := n.Process()
	if p == nil {
		return ErrNotRunning
	}
	return n.t.signal(ctx, p, sig)
}

// signal sends sig to p, holding p's clock across SIGSTOP.
func (t *Topology) signal(ctx context.Context, p *proc.Process, sig os.Signal) error {
	if isStop(sig) {
		t.clock.hold(p)
	}
	if err := p.Signal(sig); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	if isCont(sig) {
		return t.clock.release(ctx, p)
	}
	return nil
}

// drain stops ps together: SIGTERM, the clock advanced through their
// Drain, SIGKILL at Config.KillAfter of real time (see Node.Stop).
func (t *Topology) drain(ctx context.Context, ps []*proc.Process) {
	if len(ps) == 0 {
		return
	}
	killAt := time.Now().Add(t.cfg.KillAfter)
	for _, p := range ps {
		_ = p.Signal(syscall.SIGTERM)
	}
	if t.cfg.TestClock {
		for advanced := time.Duration(0); advanced < t.cfg.KillAfter && !allDone(ps) && time.Now().Before(killAt); advanced += DrainStep {
			if err := t.clock.Advance(ctx, DrainStep); err != nil {
				break // a broken clock leaves the real-time bound
			}
			waitDone(ctx, ps, time.Now().Add(DrainStepWait))
		}
	}
	waitDone(ctx, ps, killAt)
	for _, p := range ps {
		select {
		case <-p.Done():
		default:
			_ = p.Kill()
		}
	}
	for _, p := range ps {
		<-p.Done()
		t.clock.detach(p)
	}
}

// allDone reports whether every process exited.
func allDone(ps []*proc.Process) bool {
	for _, p := range ps {
		if p.State() == nil {
			return false
		}
	}
	return true
}

// waitDone waits until every process exited, deadline passed or ctx
// ended.
func waitDone(ctx context.Context, ps []*proc.Process, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for _, p := range ps {
		select {
		case <-p.Done():
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// HandoverOptions configure a handover successor.
type HandoverOptions struct {
	// Binary is the successor's executable, such as an N-1 variant from
	// internal/testkit/binaries; default Config.Binary. On a
	// controllable-clock topology it must follow the clock commands too.
	Binary string
}

// Handover starts a successor process of the Node, as an in-place
// Zero-Downtime Upgrade does: the same binary unless o.Binary, the same
// environment, directory, ports and test render, and the topology's clock.
// It returns once the process started; the processes then hand over
// between themselves, the test advancing the clock through the linger and
// the Drain and waiting with WaitHolder and the old process's Wait.
func (n *Node) Handover(ctx context.Context, o HandoverOptions) (*proc.Process, error) {
	binary := o.Binary
	if binary == "" {
		binary = n.t.cfg.Binary
	}
	n.mu.Lock()
	n.successors++
	name := n.name + "-successor-" + strconv.Itoa(n.successors)
	n.mu.Unlock()
	return n.startProcess(ctx, binary, name)
}

// WaitHolder waits, within Config.ReadyTimeout, until p holds the Node's
// data-directory lock, read from holder.json, and makes p the Node's
// serving process: the end of a handover to p.
func (n *Node) WaitHolder(ctx context.Context, p *proc.Process) error {
	ctx, cancel := context.WithTimeout(ctx, n.t.cfg.ReadyTimeout)
	defer cancel()
	for {
		h, err := nodedir.ReadHolderAt(n.dataDir)
		if err == nil && h.PID == p.PID() {
			n.mu.Lock()
			n.cur = p
			n.mu.Unlock()
			return nil
		}
		select {
		case <-p.Done():
			return fmt.Errorf("topology: %s exited (%v) before holding the lock%s", p.Name(), p.State(), outputTail(p))
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("topology: %s does not hold the lock of %s: %w (holder.json: %w)", p.Name(), n.name, ctx.Err(), err)
			}
			return fmt.Errorf("topology: %s does not hold the lock of %s: %w (holder.json: %+v)", p.Name(), n.name, ctx.Err(), h)
		case <-time.After(PollInterval):
		}
	}
}

// Holder reads the Node's holder.json.
func (n *Node) Holder() (nodedir.Holder, error) { return nodedir.ReadHolderAt(n.dataDir) }

// Reload re-renders the Node's test render from rendered, keeping each
// listener's port and adding ports for new listeners, writes any new
// secret file, then replaces RURALZ_CONFIG by an atomic rename, as ruralz
// dev run swaps its render. A controllable-clock Node sees the swap at its
// watcher's next poll, which the test reaches by advancing the clock 1 s,
// or at once on SIGHUP; WaitRevision then waits for the activation.
func (n *Node) Reload(ctx context.Context, rendered []byte) (*Render, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	r, err := n.renderBundle(ctx, rendered)
	if err != nil {
		return nil, err
	}
	if err := n.writeSecrets(r.Secrets); err != nil {
		return nil, err
	}
	tmp := n.configPath + ".tmp"
	if err := os.WriteFile(tmp, r.Bytes, 0o600); err != nil {
		return nil, fmt.Errorf("topology: %s: %w", n.name, err)
	}
	if err := os.Rename(tmp, n.configPath); err != nil {
		return nil, fmt.Errorf("topology: %s: %w", n.name, err)
	}
	n.render = r
	n.t.t.Logf("topology: %s: %s (test render %s)\n%s", n.name, DisplayRevision(r.Revision), DisplayRevision(r.TestDigest), r.Mapping())
	return r, nil
}

// Name returns the Node's name, such as "node-1".
func (n *Node) Name() string { return n.name }

// Dir returns the Node's private directory.
func (n *Node) Dir() string { return n.dir }

// ConfigPath returns RURALZ_CONFIG, the test render's path.
func (n *Node) ConfigPath() string { return n.configPath }

// DataDir returns RURALZ_DATA_DIR.
func (n *Node) DataDir() string { return n.dataDir }

// SecretRoot returns RURALZ_SECRET_ROOT.
func (n *Node) SecretRoot() string { return n.secretRoot }

// LogDir returns the directory of the Node's process logs.
func (n *Node) LogDir() string { return n.logDir }

// Render returns the current test render.
func (n *Node) Render() *Render {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.render
}

// Revision returns the current Revision digest, of the unmodified render.
func (n *Node) Revision() string { return n.Render().Revision }

// TestDigest returns the current test render's digest, which the Node
// reports.
func (n *Node) TestDigest() string { return n.Render().TestDigest }

// Port returns the port of the listener name.
func (n *Node) Port(name string) (int, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.ports[name]
	return p, ok
}

// AdminPort returns the admin port.
func (n *Node) AdminPort() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.adminPort
}

// Addr returns the loopback address of the listener name, "" for none.
func (n *Node) Addr(name string) string {
	p, ok := n.Port(name)
	if !ok {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
}

// URL returns the base URL of the listener name, by its protocol, "" for
// none.
func (n *Node) URL(name string) string {
	r := n.Render()
	for _, l := range r.Listeners {
		if l.Name == name {
			return l.Protocol + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port))
		}
	}
	return ""
}

// AdminURL returns the admin port's base URL, https unless
// Transport.CleartextAdmin.
func (n *Node) AdminURL() string {
	scheme := "https"
	if n.adminTLSDir == "" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(n.AdminPort()))
}

// OperatorToken returns the admin operator token (a canary).
func (n *Node) OperatorToken() string { return n.operatorToken }

// MetricsToken returns the admin metrics token (a canary).
func (n *Node) MetricsToken() string { return n.metricsToken }

// TokenFile returns RURALZ_ADMIN_TOKEN_FILE, for a ruralz command's
// --admin-token-file.
func (n *Node) TokenFile() string { return n.tokenPath }

// SecretValue returns the value the harness wrote for the reference ref,
// supplied or generated.
func (n *Node) SecretValue(ref string) ([]byte, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.secrets[ref]
	return v, ok
}

// SecretPath returns the file the test render maps the reference ref to.
func (n *Node) SecretPath(ref string) (string, bool) {
	r := n.Render()
	for _, s := range r.Secrets {
		if s.Ref == ref {
			return s.Path, true
		}
	}
	return "", false
}

// StateStoreProxy returns the fault proxies in front of the State Store
// store for this Node, one per URL endpoint.
func (n *Node) StateStoreProxy(store string) ([]*faultproxy.Proxy, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.links[store]
	if !ok {
		return nil, false
	}
	return slices.Clone(l.fault), true
}

// StateStoreURL returns the URL of the State Store store through this
// Node's own proxies, the value its NodeOptions.StateStoreURLs references
// hold, carrying the ACL user's password canary: what a test writes into
// a file secret to move the Node to that State Store.
func (n *Node) StateStoreURL(store string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.links[store]
	if !ok {
		return "", false
	}
	return l.url, true
}

// RESPProxy returns the counting RESP proxies in front of the State Store
// store for this Node, with StateStoreOptions.RESPProxy.
func (n *Node) RESPProxy(store string) ([]*respproxy.Proxy, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.links[store]
	if !ok || len(l.resp) == 0 {
		return nil, false
	}
	return slices.Clone(l.resp), true
}

// Process returns the process serving the Node, nil before the first
// start.
func (n *Node) Process() *proc.Process {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cur
}

// Processes returns every process the Node started, restarts and
// handover successors included, in start order.
func (n *Node) Processes() []*proc.Process {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.procs)
}

// outputTail returns the ends of p's standard output and error, for an
// error message.
func outputTail(p *proc.Process) string {
	const keep = 4 << 10
	stdout, stderr := p.Output()
	var out string
	for _, s := range []struct {
		name string
		b    []byte
	}{{"stdout", stdout}, {"stderr", stderr}} {
		if len(s.b) == 0 {
			continue
		}
		if len(s.b) > keep {
			s.b = s.b[len(s.b)-keep:]
		}
		out += "\n" + s.name + ":\n" + string(s.b)
	}
	return out
}
