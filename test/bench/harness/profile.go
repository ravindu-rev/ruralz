// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Profile names (PBB "Reference hardware" and "Scenarios").
const (
	ProfileLoopback = "loopback"
	ProfileRH1      = "rh-1"
	ProfileRH3      = "rh-3"
)

// Role is a lab host's role.
type Role string

// Roles: RH-1 runs ruralzd and the harness, RH-1L the load tools, RH-1U
// the mock Upstream, the mock IdP and the OTLP sink, RH-1S the State Store
// and the fault proxy.
const (
	RoleGateway    Role = "gateway"
	RoleLoad       Role = "load"
	RoleUpstream   Role = "upstream"
	RoleStateStore Role = "statestore"
)

// Roles returns every role.
func Roles() []Role { return []Role{RoleGateway, RoleLoad, RoleUpstream, RoleStateStore} }

// Host is one lab host of a profile.
type Host struct {
	// SSH is the ssh destination of a remote host; "" is the host the
	// harness runs on.
	SSH string `json:"ssh,omitempty"`
	// Address is the host's address on the lab network, which the
	// profile's Environment overlay names.
	Address string `json:"address"`
	// CPUs is the cpuset of the host's measured process (ruralzd, the
	// load tools, the mock, Valkey); AuxCPUs that of its other processes
	// (the OTLP sink and the IdP on RH-1U, the fault proxy on RH-1S).
	CPUs    string `json:"cpus,omitempty"`
	AuxCPUs string `json:"auxCpus,omitempty"`
	// Dir is the host's working directory for the run's binaries, keys
	// and certificates.
	Dir string `json:"dir"`
}

// Profile is a host profile: the lab hosts, how the harness reaches them
// and the fixed paths every run uses, so a profile's Revision digests stay
// equal across runs.
type Profile struct {
	// Name is loopback, rh-1 or rh-3.
	Name string `json:"profile"`
	// Hosts maps every role to its host.
	Hosts map[Role]Host `json:"hosts"`
	// SSHUser, SSHKey and KnownHosts reach the remote hosts as the
	// dedicated user deploy/benchhost provisions.
	SSHUser    string `json:"sshUser,omitempty"`
	SSHKey     string `json:"sshKey,omitempty"`
	KnownHosts string `json:"knownHosts,omitempty"`
	// SecretRoot is the fixed RURALZ_SECRET_ROOT of the Node.
	SecretRoot string `json:"secretRoot"`
	// TLSDir holds the run's certificates at fixed paths on every host.
	TLSDir string `json:"tlsDir"`
	// Scope wraps every started process in a systemd-run scope on its
	// cpuset; off on the loopback profile.
	Scope bool `json:"scope"`
	// GOMAXPROCS is the Node's; 4 on RH-1 (target).
	GOMAXPROCS int `json:"gomaxprocs"`
	// Checks runs the pre-run host checks (fixed frequency, throttle
	// counters, round trip); off on the loopback profile.
	Checks bool `json:"checks"`
	// MaxRoundTrip is the host check's bound on the p99 round trip
	// between hosts; 50 µs (hypothesis).
	MaxRoundTripMicros int `json:"maxRoundTripMicros,omitempty"`
	// AdminPort is the Node's admin port, the scenario Bundles' 9901 when
	// zero.
	AdminPort int `json:"adminPort,omitempty"`
	// Services are the lab services' listen addresses, the ones the
	// profile's Environment overlay names.
	Services Services `json:"services"`
}

// Services are the listen addresses (host:port) of the lab services.
type Services struct {
	// Mock is the mock Upstream on RH-1U.
	Mock string `json:"mock"`
	// IdP is the mock IdP (HTTPS) on RH-1U.
	IdP string `json:"idp"`
	// Sink is the OTLP sink (gRPC over TLS) on RH-1U.
	Sink string `json:"sink"`
	// StateStore is Valkey (TLS) on RH-1S; FaultProxy is S5x's fault
	// proxy in front of it.
	StateStore string `json:"stateStore"`
	FaultProxy string `json:"faultProxy"`
	// EchoPort is the round-trip check's echo port on every host.
	EchoPort int `json:"echoPort"`
	// StateStoreServer is the valkey-server or redis-server executable
	// on the State Store host; empty runs the pinned Valkey 9 image with
	// docker and host networking.
	StateStoreServer string `json:"stateStoreServer,omitempty"`
}

// Fixed paths of the loopback profile: the scenario Bundles' loopback
// overlays name the run's listener key pair under this secret root and its
// CA under the TLS directory inside it, so the profile's Revision digests
// stay equal across runs (PBB "Scenarios"). Like the profile's fixed
// ports, they let one loopback run at a time use a host.
const (
	LoopbackSecretRoot = "/tmp/ruralz-bench/secrets"
	LoopbackTLSDir     = LoopbackSecretRoot + "/tls"
)

// EnvStateStoreServer names the valkey-server or redis-server executable
// the loopback profile supervises, as the State Store suites read it.
const EnvStateStoreServer = "RURALZ_TEST_REDIS_SERVER"

// Loopback returns the loopback profile rooted at dir: every role on the
// harness's own host, its processes started without scopes, no host
// checks, the fixed secret root and TLS directory, and the State Store
// server that LocalStateStoreServer finds.
func Loopback(dir string) *Profile {
	p := &Profile{
		Name: ProfileLoopback, Hosts: map[Role]Host{},
		SecretRoot: LoopbackSecretRoot, TLSDir: LoopbackTLSDir, GOMAXPROCS: 0,
	}
	for _, r := range Roles() {
		p.Hosts[r] = Host{Address: "127.0.0.1", Dir: filepath.Join(dir, "hosts", string(r))}
	}
	p.Services = LoopbackServices()
	p.Services.StateStoreServer = LocalStateStoreServer(os.Getenv, exec.LookPath)
	return p
}

// LocalStateStoreServer returns the State Store server a loopback run
// supervises: EnvStateStoreServer, else valkey-server or redis-server on
// PATH, else "", which runs the pinned Valkey 9 image with docker.
func LocalStateStoreServer(getenv func(string) string, lookPath func(string) (string, error)) string {
	if v := getenv(EnvStateStoreServer); v != "" {
		return v
	}
	for _, name := range []string{"valkey-server", "redis-server"} {
		if p, err := lookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// LoopbackServices are the loopback profile's fixed service addresses,
// which its Environment overlay names so its Revision digests stay equal
// across runs.
func LoopbackServices() Services {
	return Services{
		Mock: "127.0.0.1:18080", IdP: "127.0.0.1:18443", Sink: "127.0.0.1:14317",
		StateStore: "127.0.0.1:16379", FaultProxy: "127.0.0.1:16380", EchoPort: 17000,
	}
}

// ReadProfile reads a profile inventory, the JSON file deploy/benchhost
// keeps per profile, strictly.
func ReadProfile(path string) (*Profile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the inventory the job names
	if err != nil {
		return nil, fmt.Errorf("harness: profile: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("harness: profile %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("harness: profile %s: data after the inventory", path)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("harness: profile %s: %w", path, err)
	}
	return &p, nil
}

// Validate checks a profile: a known name, every role with an address and
// a directory, cpusets that parse, ssh settings for remote hosts, and
// absolute fixed paths.
func (p *Profile) Validate() error {
	var errs []error
	switch p.Name {
	case ProfileLoopback, ProfileRH1, ProfileRH3:
	default:
		errs = append(errs, fmt.Errorf("profile %q is not loopback, rh-1 or rh-3", p.Name))
	}
	remote := false
	for _, r := range Roles() {
		h, ok := p.Hosts[r]
		if !ok {
			errs = append(errs, fmt.Errorf("no %s host", r))
			continue
		}
		if h.Address == "" || h.Dir == "" || !filepath.IsAbs(h.Dir) {
			errs = append(errs, fmt.Errorf("%s host needs an address and an absolute dir", r))
		}
		for _, c := range []string{h.CPUs, h.AuxCPUs} {
			if c != "" {
				if _, ok := lab.ParseCPUList(c); !ok {
					errs = append(errs, fmt.Errorf("%s host cpuset %q does not parse", r, c))
				}
			}
		}
		if r == RoleGateway && h.SSH != "" {
			errs = append(errs, errors.New("the gateway host is the harness's own: no ssh destination"))
		}
		if h.SSH != "" {
			remote = true
		}
	}
	if remote && (p.SSHUser == "" || p.SSHKey == "" || p.KnownHosts == "") {
		errs = append(errs, errors.New("remote hosts need sshUser, sshKey and knownHosts"))
	}
	for _, f := range []struct{ name, v string }{{"secretRoot", p.SecretRoot}, {"tlsDir", p.TLSDir}} {
		if !filepath.IsAbs(f.v) {
			errs = append(errs, fmt.Errorf("%s %q is not absolute", f.name, f.v))
		}
	}
	if p.GOMAXPROCS < 0 || p.MaxRoundTripMicros < 0 {
		errs = append(errs, errors.New("negative gomaxprocs or round trip bound"))
	}
	for _, sv := range []struct{ name, addr string }{
		{"mock", p.Services.Mock}, {"idp", p.Services.IdP}, {"sink", p.Services.Sink},
		{"stateStore", p.Services.StateStore}, {"faultProxy", p.Services.FaultProxy},
	} {
		if _, port, err := net.SplitHostPort(sv.addr); err != nil || port == "" || port == "0" {
			errs = append(errs, fmt.Errorf("services.%s %q is not a fixed host:port", sv.name, sv.addr))
		}
	}
	if p.Services.EchoPort < 1 || p.Services.EchoPort > 65535 {
		errs = append(errs, fmt.Errorf("services.echoPort %d is not a port", p.Services.EchoPort))
	}
	return errors.Join(errs...)
}

// Host returns the host of role r.
func (p *Profile) Host(r Role) Host { return p.Hosts[r] }

// HardwareProfile is the record's hardware profile name: RH-1, RH-3 or
// loopback.
func (p *Profile) HardwareProfile() string {
	if p.Name == ProfileLoopback {
		return p.Name
	}
	return strings.ToUpper(p.Name)
}

// adminPort returns the Node's admin port, 9901 by default.
func (p *Profile) adminPort() int {
	if p.AdminPort > 0 {
		return p.AdminPort
	}
	return AdminPort
}

// roundTripBound returns the host check's round-trip bound, 50 µs by
// default (hypothesis).
func (p *Profile) roundTripBound() int {
	if p.MaxRoundTripMicros > 0 {
		return p.MaxRoundTripMicros
	}
	return 50
}
