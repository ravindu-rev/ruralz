// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// gateStep is one validation gate step a file-mode load fails at, with
// the code its rejection counts.
type gateStep struct {
	step int
	code string
	// mutate returns the invalid content of a Node's test render.
	mutate func(n *topology.Node, render []byte) ([]byte, error)
}

// LKGReason is the degraded reason of a Node that booted
// Last-Known-Good.
const LKGReason = "lkg_boot"

// TestV1GateRejections is the gate half of V-1 at 50% load: a file-mode
// load failing each validation gate step a file-mode Node runs in M1 is
// rejected with that step's code on every Node, with nothing the Nodes
// serve changing, and a Node restarted onto an invalid Bundle boots
// Last-Known-Good under lkg_boot
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Verification runbook" and "Validation gate"): steps 1, 5, 6, 7, 9
// with both its codes (an unresolvable secretRef, a refused State Store
// URL), 10 and 11. Steps 2 and 3 skip a watched directory, which is
// unsigned, steps 4 and 8 check Control-mode and Plugin content, Planned
// (M2), and step 12 refuses nothing; the R1 reload ladder is a
// measurement of the Latency job.
func TestV1GateRejections(t *testing.T) {
	s := saturation(t)
	r := suite.Start(t, Spec{Name: V1, SampleEvery: 250 * time.Millisecond})
	rate := suite.Profile.LoadShare * s
	gate := 3 * time.Minute
	l := r.StartLoad(LoadSpec{Duration: Baseline + gate + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: rate / 2},
		{Name: "limited", Path: PathLimited, Rate: rate / 2, APIKeys: true, Counted: Decisions()},
	}})
	ctx := r.Context()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	steps := []gateStep{
		{1, "RZ-CFG-001", func(_ *topology.Node, b []byte) ([]byte, error) {
			// One byte past the Node's 64 MiB source limit.
			pad := bytes.Repeat([]byte("#"), 64<<20)
			return append(append(append([]byte(nil), b...), '\n'), pad...), nil
		}},
		{5, "RZ-CFG-007", func(_ *topology.Node, b []byte) ([]byte, error) {
			return replaceOnce(b, "apiVersion: ruralz/v1alpha1", "apiVersion: ruralz/v1alpha9")
		}},
		{6, "RZ-CFG-009", func(_ *topology.Node, b []byte) ([]byte, error) {
			return replaceOnce(b, "- name: chaos-backend", "- name: chaos-missing")
		}},
		{7, "RZ-CFG-040", func(_ *topology.Node, b []byte) ([]byte, error) {
			return replaceInDoc(b, "kind: Upstream", "protocol: http", "protocol: grpc")
		}},
		{9, "RZ-CFG-026", func(n *topology.Node, b []byte) ([]byte, error) {
			p, ok := n.SecretPath(RefTLSCertificate)
			if !ok {
				return nil, fmt.Errorf("%s has no certificate file", n.Name())
			}
			return replaceOnce(b, p, p+".missing")
		}},
		{9, "RZ-CFG-042", func(n *topology.Node, b []byte) ([]byte, error) {
			// A State Store URL that resolves but that the transport rules
			// refuse: a parameter that would skip certificate checks.
			return withRefusedStateStoreURL(n, b, r.Top.Canary())
		}},
		{10, "RZ-CFG-026", func(n *topology.Node, b []byte) ([]byte, error) {
			// The Upstream layer refuses, at compile, a CA bundle that
			// resolves but holds no certificate.
			return withBadUpstreamCA(n, b, r.Top.Canary())
		}},
		{11, "RZ-CFG-039", func(n *topology.Node, b []byte) ([]byte, error) {
			port, ok := n.Port("https")
			if !ok {
				return nil, fmt.Errorf("%s has no https port", n.Name())
			}
			return replaceOnce(b, "port: "+strconv.Itoa(port), "port: "+strconv.Itoa(busyPort))
		}},
	}
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	before := revisions(ctx, t, r)
	for _, st := range steps {
		at := time.Now()
		r.Inject(fmt.Sprintf("gate-step-%d", st.step), "every-node", func() error { return loadAll(ctx, r, st.mutate) })
		rejected := waitRejected(ctx, r, st.code, at)
		if err := loadAll(ctx, r, nil); err != nil {
			t.Fatal(err)
		}
		r.Add(Check(fmt.Sprintf("Nodes rejecting a load failing gate step %d with %s", st.step, st.code), Functional, float64(rejected), EQ, float64(r.Nodes)))
		r.Add(Check(fmt.Sprintf("Nodes whose active Revision changed at gate step %d", st.step), Functional, float64(changed(before, revisions(ctx, t, r))), EQ, 0))
	}

	// A restart onto an invalid Bundle boots Last-Known-Good.
	n := r.Top.Nodes()[0]
	lkgAt := r.Inject("restart-onto-invalid", n.Name(), func() error {
		if _, err := n.Stop(ctx); err != nil {
			return err
		}
		bad, err := replaceOnce(n.Render().Bytes, "apiVersion: ruralz/v1alpha1", "apiVersion: ruralz/v1alpha9")
		if err != nil {
			return err
		}
		if err := writeConfig(n, bad); err != nil {
			return err
		}
		return n.Start(ctx)
	})
	rz, _, err := n.Readyz(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.Add(Holds("restarted Node serves its Last-Known-Good Revision", Functional, rz.Revision == before[n.Name()]))
	waitReason(ctx, r, n.Name(), LKGReason, lkgAt)
	r.Add(Holds("restarted Node raises lkg_boot", Functional, len(r.Sampler.ReasonRaised([]string{n.Name()}, LKGReason, lkgAt, time.Now().Add(time.Second))) == 1))
	if err := loadAll(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	r.Hold(l, "gate", gate)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	// The gate steps change nothing the Nodes serve: no request fails and
	// every probe answers 200 until the restart, which drains its Node.
	transitions := 0
	for _, tr := range r.HTTP.Transitions() {
		if tr.At.After(base.Start) && tr.At.Before(lkgAt) {
			transitions++
		}
	}
	r.Add(
		Check("failed requests during the gate steps", Functional, float64(LoadStats(l, base.End, lkgAt).Failed), EQ, 0),
		Check("balancer health transitions during the gate steps", Functional, float64(transitions), EQ, 0),
		Check("recovery errors", Functional, float64(LoadStats(l, rec.Start, rec.End).Failed), EQ, 0),
	)
	r.Breakdown("failed_requests_around_the_restart", float64(LoadStats(l, lkgAt, rec.Start).Failed))
	r.Finish()
}

// loadAll writes each Node's test render, changed by mutate unless nil,
// to its RURALZ_CONFIG and sends SIGHUP, so the Node loads it at once.
func loadAll(ctx context.Context, r *Run, mutate func(*topology.Node, []byte) ([]byte, error)) error {
	for _, n := range r.Top.Nodes() {
		b := n.Render().Bytes
		if mutate != nil {
			var err error
			if b, err = mutate(n, b); err != nil {
				return err
			}
		}
		if err := writeConfig(n, b); err != nil {
			return err
		}
		if err := n.Signal(ctx, syscall.SIGHUP); err != nil {
			return err
		}
	}
	return nil
}

// writeConfig replaces a Node's RURALZ_CONFIG by an atomic rename.
func writeConfig(n *topology.Node, b []byte) error {
	tmp := n.ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.ConfigPath())
}

// waitRejected waits, at most 30 s, until every Node counted a rejected
// activation with code after since, and returns how many did.
func waitRejected(ctx context.Context, r *Run, code string, since time.Time) int {
	deadline := time.Now().Add(30 * time.Second)
	for {
		r.Sampler.Round(ctx)
		n := 0
		for _, name := range r.NodeNames() {
			tr := r.Sampler.Trace(name, "ruralz_config_activations_total", promtext.L("result", "rejected", "code", code))
			if Increase(tr, since.Add(-time.Second), time.Now().Add(time.Second)) > 0 || (len(tr) > 0 && tr[0].Real.After(since) && tr[len(tr)-1].Value > 0) {
				n++
			}
		}
		if n == r.Nodes || time.Now().After(deadline) || ctx.Err() != nil {
			return n
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitReason waits, at most 30 s, until node's samples show reason.
func waitReason(ctx context.Context, r *Run, node, reason string, since time.Time) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		r.Sampler.Round(ctx)
		if len(r.Sampler.ReasonRaised([]string{node}, reason, since, time.Now().Add(time.Second))) == 1 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// revisions returns each Node's active Revision from /readyz.
func revisions(ctx context.Context, t *testing.T, r *Run) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range r.Top.Nodes() {
		rz, _, err := n.Readyz(ctx)
		if err != nil {
			t.Fatalf("%s: %v", n.Name(), err)
		}
		out[n.Name()] = rz.Revision
	}
	return out
}

// changed counts the Nodes whose Revision differs between two readings.
func changed(a, b map[string]string) int {
	n := 0
	for k, v := range a {
		if b[k] != v {
			n++
		}
	}
	return n
}

// replaceOnce replaces the first old in b by new.
func replaceOnce(b []byte, old, new string) ([]byte, error) {
	i := bytes.Index(b, []byte(old))
	if i < 0 {
		return nil, fmt.Errorf("chaos: %q not in the test render", old)
	}
	return append(append(append([]byte(nil), b[:i]...), new...), b[i+len(old):]...), nil
}

// badCAFile is the secret file, under a Node's secret root, of
// withBadUpstreamCA's CA bundle.
const badCAFile = "chaos-bad-upstream-ca.pem"

// withBadUpstreamCA gives the Upstream of b a tls.caCertificate whose
// secret file, which it writes under n's secret root with a canary,
// resolves but parses to no certificate: gate step 9 resolves it and the
// compile of step 10 refuses it (RZ-CFG-026).
func withBadUpstreamCA(n *topology.Node, b []byte, canaryValue string) ([]byte, error) {
	path := filepath.Join(n.SecretRoot(), badCAFile)
	if err := os.WriteFile(path, []byte(canaryValue+"\nnot a certificate\n"), 0o600); err != nil {
		return nil, err
	}
	i := bytes.Index(b, []byte("kind: Upstream"))
	if i < 0 {
		return nil, errors.New("chaos: the test render holds no Upstream")
	}
	j := bytes.Index(b[i:], []byte("protocol: http"))
	if j < 0 {
		return nil, errors.New("chaos: the Upstream of the test render has no protocol")
	}
	j += i
	indent := j - (bytes.LastIndexByte(b[:j], '\n') + 1)
	pad := strings.Repeat(" ", indent)
	block := "protocol: http\n" + pad + "tls:\n" + pad + "  caCertificate: {secretRef: {provider: file, name: " + strconv.Quote(path) + "}}"
	return append(append(append([]byte(nil), b[:j]...), block...), b[j+len("protocol: http"):]...), nil
}

// refusedURLFile is the secret file, under a Node's secret root, of
// withRefusedStateStoreURL's State Store URL.
const refusedURLFile = "chaos-refused-state-store-url"

// withRefusedStateStoreURL points the main State Store connection's url
// secretRef of b at a file it writes under n's secret root: a URL whose
// password is a canary and whose skip_verify parameter the transport
// rules refuse, so gate step 9 resolves it and refuses it (RZ-CFG-042).
func withRefusedStateStoreURL(n *topology.Node, b []byte, canaryValue string) ([]byte, error) {
	cur, ok := n.SecretPath(RefStateStoreURL)
	if !ok {
		return nil, fmt.Errorf("%s has no State Store URL file", n.Name())
	}
	path := filepath.Join(n.SecretRoot(), refusedURLFile)
	u := "rediss://" + topology.StateStoreUser + ":" + canaryValue + "@127.0.0.1:1/?skip_verify=true"
	if err := os.WriteFile(path, []byte(u), 0o600); err != nil {
		return nil, err
	}
	return replaceOnce(b, cur, path)
}

// replaceInDoc replaces the first old after marker by new.
func replaceInDoc(b []byte, marker, old, new string) ([]byte, error) {
	i := bytes.Index(b, []byte(marker))
	if i < 0 {
		return nil, fmt.Errorf("chaos: %q not in the test render", marker)
	}
	rest, err := replaceOnce(b[i:], old, new)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), b[:i]...), rest...), nil
}
