// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// StateStoreUser is the Valkey ACL user of every run; the password is
// fresh per run.
const StateStoreUser = "bench"

// BuildLabhost builds test/bench/labhost from repoRoot into out, static
// and trimmed, for the lab hosts.
func BuildLabhost(ctx context.Context, repoRoot, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./test/bench/labhost") //nolint:gosec // fixed build command
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harness: build labhost: %w: %s", err, b)
	}
	return nil
}

// Lab is one run's lab services on the profile's hosts.
type Lab struct {
	p    *Profile
	pki  *RunPKI
	bins map[Role]string
	// keyFile is the local key file the IdP publishes and loadgen signs
	// tokens with.
	keyFile string
	keys    *lab.KeyFile

	Mock, Sink, IdP, Valkey, Proxy *Proc
	// StateStorePassword is the run's Valkey ACL password.
	StateStorePassword string
}

// NewLab copies labhost and the run's signing keys to the hosts. dir is a
// local working directory.
func NewLab(ctx context.Context, p *Profile, labhost string, rp *RunPKI, dir string) (*Lab, error) {
	l := &Lab{p: p, pki: rp, bins: map[Role]string{}}
	for _, role := range Roles() {
		dst := filepath.Join(p.Hosts[role].Dir, "bin", "labhost")
		if err := p.Runner(role).Put(ctx, labhost, dst, 0o700); err != nil {
			return nil, err
		}
		l.bins[role] = dst
	}
	// Two RS256 keys sign S2's 10,000 tokens (target); the IdP publishes
	// both (OQ-performance-budgets-and-benchmarking-8 keeps RS256).
	kf, err := lab.NewKeyFile(mockidp.RS256, 2)
	if err != nil {
		return nil, err
	}
	l.keys = kf
	b, err := json.Marshal(kf)
	if err != nil {
		return nil, err
	}
	l.keyFile = filepath.Join(dir, "keys.json")
	if err := os.WriteFile(l.keyFile, b, 0o600); err != nil {
		return nil, err
	}
	if err := p.Runner(RoleUpstream).Put(ctx, l.keyFile, l.remoteKeys(), 0o600); err != nil {
		return nil, err
	}
	pw := make([]byte, 24)
	if _, err := rand.Read(pw); err != nil {
		return nil, err
	}
	l.StateStorePassword = hex.EncodeToString(pw)
	return l, nil
}

func (l *Lab) remoteKeys() string { return filepath.Join(l.p.Hosts[RoleUpstream].Dir, "keys.json") }

func (l *Lab) tls(name string) string { return filepath.Join(l.p.TLSDir, name) }

// StartUpstream starts the mock Upstream on RH-1U's measured cpuset and
// the OTLP sink and the mock IdP on its auxiliary one.
func (l *Lab) StartUpstream(ctx context.Context, w Workload) error {
	up := l.p.Runner(RoleUpstream)
	h := l.p.Hosts[RoleUpstream]
	var err error
	body := w.BodySize
	if body <= 0 {
		body = 1024
	}
	if l.Mock, err = up.Start(ctx, "mock", []string{l.bins[RoleUpstream], "mock", "-listen", l.p.Services.Mock,
		"-body-size", strconv.FormatInt(body, 10), "-delay", w.MockDelay.String()}, h.CPUs); err != nil {
		return err
	}
	if l.Sink, err = up.Start(ctx, "sink", []string{l.bins[RoleUpstream], "sink", "-listen", l.p.Services.Sink,
		"-cert", l.tls(LeafSink + ".crt"), "-key", l.tls(LeafSink + ".key")}, h.AuxCPUs); err != nil {
		return err
	}
	if l.IdP, err = up.Start(ctx, "idp", []string{l.bins[RoleUpstream], "idp", "-listen", l.p.Services.IdP,
		"-cert", l.tls(LeafIdP + ".crt"), "-key", l.tls(LeafIdP + ".key"), "-issuer", BenchIssuer, "-keys", l.remoteKeys()}, h.AuxCPUs); err != nil {
		return err
	}
	return nil
}

// ValkeyArgs returns the State Store server's arguments (PBB "Reference
// hardware" settings baseline): TLS only from the run's CA, one ACL user,
// io-threads 4, persistence off, maxclients 20,000 (target).
func (l *Lab) ValkeyArgs() []string {
	host, port, _ := net.SplitHostPort(l.p.Services.StateStore)
	return []string{
		"--bind", host, "--port", "0", "--tls-port", port,
		"--tls-cert-file", l.tls(LeafValkey + ".crt"), "--tls-key-file", l.tls(LeafValkey + ".key"),
		"--tls-ca-cert-file", l.tls(FileCA), "--tls-auth-clients", "no",
		"--io-threads", "4", "--save", "", "--appendonly", "no", "--maxclients", "20000",
		"--user", "default", "off",
		"--user", StateStoreUser, "on", ">" + l.StateStorePassword, "~*", "&*", "+@all",
	}
}

// StartStateStore starts Valkey, empty, on RH-1S's measured cpuset (the
// pinned image with host networking, or the profile's server executable),
// and S5x's fault proxy in front of it when the scenario asks. It returns
// the State Store URL the Node uses.
func (l *Lab) StartStateStore(ctx context.Context, s Scenario) (string, error) {
	ss := l.p.Runner(RoleStateStore)
	h := l.p.Hosts[RoleStateStore]
	server := []string{l.p.Services.StateStoreServer}
	if l.p.Services.StateStoreServer == "" {
		server = []string{"docker", "run", "--rm", "--network", "host", "-v", l.p.TLSDir + ":" + l.p.TLSDir + ":ro", redisserver.ImageValkey9, "valkey-server"}
	}
	argv := append([]string{l.bins[RoleStateStore], "run", "-ready", l.p.Services.StateStore, "--"}, append(server, l.ValkeyArgs()...)...)
	var err error
	if l.Valkey, err = ss.Start(ctx, "statestore", argv, h.CPUs); err != nil {
		return "", err
	}
	addr := l.p.Services.StateStore
	if s.Proxy != nil {
		if l.Proxy, err = ss.Start(ctx, "faultproxy", []string{l.bins[RoleStateStore], "faultproxy", "-listen", l.p.Services.FaultProxy,
			"-upstream", l.p.Services.StateStore, "-mean", s.Proxy.Mean.String(), "-stddev", s.Proxy.StdDev.String()}, h.AuxCPUs); err != nil {
			return "", err
		}
		addr = l.p.Services.FaultProxy
	}
	return "rediss://" + StateStoreUser + ":" + l.StateStorePassword + "@" + addr, nil
}

// LoadSpec returns the loadgen spec of w against target.
func (l *Lab) LoadSpec(w Workload, target string, warmup, duration time.Duration, settle bool) (lab.LoadSpec, error) {
	scheme := "http"
	spec := lab.LoadSpec{
		Method: w.Method, Protocol: w.Protocol, Connections: w.Connections, Streams: w.Streams,
		Rate: w.Rate, Warmup: warmup, Duration: duration, Settle: settle, KeyHeader: w.KeyHeader, Keys: w.Keys, Count: w.Count,
	}
	if w.Protocol == "h2" {
		scheme = "https"
		ca, err := os.ReadFile(l.pki.Path(FileCA)) //nolint:gosec // G304: the run's own CA file
		if err != nil {
			return spec, err
		}
		spec.CA = string(ca)
	}
	spec.URL = scheme + "://" + net.JoinHostPort(target, strconv.Itoa(w.Port)) + w.Path
	if w.Tokens > 0 {
		spec.Tokens = &lab.TokenSpec{
			Count: w.Tokens, Keys: *l.keys, Issuer: BenchIssuer, Audience: BenchAudience,
			// Real time by design: the run ends long before.
			Expires: time.Now().Add(24 * time.Hour).Truncate(time.Second),
		}
	}
	return spec, nil
}

// RunLoad runs one loadgen spec on RH-1L's measured cpuset.
func (l *Lab) RunLoad(ctx context.Context, spec lab.LoadSpec) (*lab.LoadResult, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	r := l.p.Runner(RoleLoad)
	out, err := r.OutputOn(ctx, []string{l.bins[RoleLoad], "loadgen"}, append(b, '\n'), l.p.Hosts[RoleLoad].CPUs)
	if err != nil {
		return nil, err
	}
	var res lab.LoadResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("harness: loadgen result: %w", err)
	}
	return &res, nil
}

// Reset starts a new measurement on every running service.
func (l *Lab) Reset() error {
	var errs []error
	for _, p := range l.procs() {
		if _, err := p.Do(lab.Request{Op: lab.OpReset}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (l *Lab) procs() []*Proc {
	var out []*Proc
	for _, p := range []*Proc{l.Mock, l.Sink, l.IdP, l.Valkey, l.Proxy} {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Close stops every service, the State Store last.
func (l *Lab) Close() error {
	var errs []error
	for _, p := range []*Proc{l.Proxy, l.Mock, l.IdP, l.Sink, l.Valkey} {
		if p == nil {
			continue
		}
		if _, err := p.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	l.Mock, l.Sink, l.IdP, l.Valkey, l.Proxy = nil, nil, nil, nil, nil
	return errors.Join(errs...)
}
