// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Lab host names of the scenario Bundles, which every leaf also carries
// so a profile's overlay may name a host by either form.
const (
	NameGateway    = "gateway.bench.example"
	NameMock       = "upstream-mock.bench.example"
	NameIDP        = "idp.bench.example"
	NameSink       = "otel-sink.bench.example"
	NameStateStore = "statestore.bench.example"
)

// TLS file names under Profile.TLSDir on every host, and the listener's
// under Profile.SecretRoot on the gateway host.
const (
	FileCA       = "ca.pem"
	LeafListener = "listener"
	LeafIDP      = "idp"
	LeafSink     = "sink"
	LeafValkey   = "valkey"
)

// RunPKI is one run's fresh PKI (PBB "Reproducibility"): an ECDSA P-256
// CA and the listener's, the IdP's, the OTLP sink's and Valkey's
// certificates, written locally under Dir.
type RunPKI struct {
	CA  *pki.CA
	Dir string
}

// NewRunPKI makes the PKI under dir for the profile's hosts.
func NewRunPKI(dir string, p *Profile) (*RunPKI, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Bench CA"})
	if err != nil {
		return nil, err
	}
	rp := &RunPKI{CA: ca, Dir: dir}
	if err := ca.WriteFile(filepath.Join(dir, FileCA)); err != nil {
		return nil, err
	}
	leaves := []struct {
		name string
		role Role
		dns  string
	}{
		{LeafListener, RoleGateway, NameGateway},
		{LeafIDP, RoleUpstream, NameIDP},
		{LeafSink, RoleUpstream, NameSink},
		{LeafValkey, RoleStateStore, NameStateStore},
	}
	for _, l := range leaves {
		hosts := []string{l.dns, p.Hosts[l.role].Address, "localhost", "127.0.0.1"}
		slices.Sort(hosts)
		leaf, err := ca.Server(slices.Compact(hosts)...)
		if err != nil {
			return nil, err
		}
		if _, _, err := leaf.WriteFiles(dir, l.name); err != nil {
			return nil, err
		}
	}
	return rp, nil
}

// Path returns the local path of a PKI file.
func (rp *RunPKI) Path(name string) string { return filepath.Join(rp.Dir, name) }

// Distribute copies the CA and every role's leaves to the profile's fixed
// TLS directory on each host, and the listener's key pair to the gateway
// host's secret root.
func (rp *RunPKI) Distribute(ctx context.Context, p *Profile) error {
	files := map[Role][]string{
		RoleGateway:    {FileCA, LeafListener + ".crt", LeafListener + ".key"},
		RoleLoad:       {FileCA},
		RoleUpstream:   {FileCA, LeafIDP + ".crt", LeafIDP + ".key", LeafSink + ".crt", LeafSink + ".key"},
		RoleStateStore: {FileCA, LeafValkey + ".crt", LeafValkey + ".key"},
	}
	for _, role := range Roles() {
		r := p.Runner(role)
		for _, f := range files[role] {
			if err := r.Put(ctx, rp.Path(f), filepath.Join(p.TLSDir, f), 0o600); err != nil {
				return fmt.Errorf("harness: PKI to %s: %w", role, err)
			}
		}
	}
	g := p.Runner(RoleGateway)
	for _, f := range []string{LeafListener + ".crt", LeafListener + ".key"} {
		if err := g.Put(ctx, rp.Path(f), filepath.Join(p.SecretRoot, f), 0o600); err != nil {
			return fmt.Errorf("harness: PKI to the secret root: %w", err)
		}
	}
	return nil
}
