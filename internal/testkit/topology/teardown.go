// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/testkit/canary"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// teardown ends everything the topology started (package documentation,
// Teardown); registered as several cleanups, it runs once, at the first.
func (t *Topology) teardown() { t.teardownOnce.Do(t.tearDown) }

func (t *Topology) tearDown() {
	t.mu.Lock()
	t.torn = true
	nodes := slices.Clone(t.nodes)
	stores := make([]*StateStore, 0, len(t.stores))
	for _, s := range t.stores {
		stores = append(stores, s)
	}
	slices.SortFunc(stores, func(a, b *StateStore) int { return strings.Compare(a.name, b.name) })
	mocks, idps, sinks := slices.Clone(t.mocks), slices.Clone(t.idps), slices.Clone(t.sinks)
	balancers, transports := slices.Clone(t.balancers), slices.Clone(t.transports)
	t.mu.Unlock()
	ctx := t.ctx

	// The evidence of a failure includes each Node's /debug/snapshots,
	// which only a running Node answers.
	for _, n := range nodes {
		if p := n.Process(); p != nil && p.State() == nil {
			if resp, err := n.Admin(ctx, "/debug/snapshots"); err == nil {
				n.mu.Lock()
				n.snapshots = resp.Body
				n.mu.Unlock()
			}
		}
	}
	var running []*proc.Process
	for _, n := range nodes {
		for _, p := range n.Processes() {
			if p.State() == nil {
				running = append(running, p)
			}
		}
	}
	t.drain(ctx, running)

	for _, err := range t.dumpCaptures(ctx, stores, mocks, sinks) {
		t.t.Errorf("%v", err)
	}
	findings, errs := t.scan(nodes)
	for _, err := range errs {
		t.t.Errorf("%v", err)
	}
	for _, f := range findings {
		t.t.Errorf("topology: secret leak: %s", f)
	}
	if t.t.Failed() {
		artifacts := t.cfg.ArtifactDir
		if artifacts == "" {
			artifacts = os.Getenv(ArtifactDirEnv)
		}
		if artifacts == "" {
			t.t.Logf("topology: %s is unset: no failure evidence kept", ArtifactDirEnv)
		} else if dir, err := t.writeEvidence(artifacts, nodes, sinks, balancers); err != nil {
			t.t.Errorf("topology: failure evidence: %v", err)
		} else {
			t.t.Logf("topology: failure evidence in %s", dir)
		}
	}

	closeAll(balancers)
	for _, s := range stores {
		s.closeLinks()
	}
	closeAll(mocks)
	closeAll(idps)
	closeAll(sinks)
	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
	t.clock.close()
	t.cancel()
	t.watchers.Wait()
	if err := os.RemoveAll(t.root); err != nil {
		t.t.Errorf("topology: %v", err)
	}
}

// dumpCaptures writes what the dependencies recorded into the capture
// store for the scan: the OTLP sinks' requests as sent, the mock
// Upstreams' request logs and the State Stores' keyspaces.
func (t *Topology) dumpCaptures(ctx context.Context, stores []*StateStore, mocks []*mockup.Server, sinks []*otlpsink.Sink) []error {
	var errs []error
	for i, s := range sinks {
		var b bytes.Buffer
		for _, r := range s.Requests() {
			b.Write(r.Raw)
			b.WriteByte('\n')
		}
		if err := t.captures.add("otlp-"+strconv.Itoa(i+1), b.Bytes()); err != nil {
			errs = append(errs, err)
		}
	}
	for i, m := range mocks {
		data, err := dumpMock(m)
		if err != nil {
			errs = append(errs, err)
		}
		if err := t.captures.add("mock-upstream-"+strconv.Itoa(i+1)+"-"+m.Name(), data); err != nil {
			errs = append(errs, err)
		}
	}
	for _, s := range stores {
		var b bytes.Buffer
		if err := s.dumpKeyspace(ctx, &b); err != nil {
			errs = append(errs, err)
		}
		if err := t.captures.add("statestore-"+s.name, b.Bytes()); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// scan looks for every canary in the capture store, each Node's logs and
// each RURALZ_DATA_DIR. A finding's Source is its file's slash path below
// the topology's directory, such as "node-1/logs/node-1.out" or
// "captures/capture-cli".
func (t *Topology) scan(nodes []*Node) ([]canary.Finding, []error) {
	roots := []string{t.captures.dir}
	for _, n := range nodes {
		roots = append(roots, n.logDir, n.dataDir)
	}
	var findings []canary.Finding
	var errs []error
	for _, c := range t.canaries.list() {
		for _, root := range roots {
			if root == "" {
				continue
			}
			f, err := c.ScanDir(root)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("topology: leak scan of %s: %w", root, err))
			}
			rel, rerr := filepath.Rel(t.root, root)
			for _, x := range f {
				if rerr == nil {
					x.Source = filepath.ToSlash(filepath.Join(rel, x.Source))
				}
				findings = append(findings, x)
			}
		}
	}
	return findings, errs
}

// writeEvidence copies a failed test's evidence into a directory named
// after the test under dir and returns it: each process's logs, a listing
// of each RURALZ_DATA_DIR, each Node's /debug/snapshots, the OTLP records
// and each balancer's transition log.
func (t *Topology) writeEvidence(dir string, nodes []*Node, sinks []*otlpsink.Sink, balancers []*Balancer) (string, error) {
	base := filepath.Join(dir, fileName(t.t.Name()))
	if err := os.MkdirAll(base, 0o750); err != nil { //nolint:gosec // G703: the artifact directory the test run names
		return "", err
	}
	var errs []error
	for _, n := range nodes {
		nd := filepath.Join(base, n.name)
		errs = append(errs, copyDir(n.logDir, filepath.Join(nd, "logs")))
		listing, err := listDir(n.dataDir)
		errs = append(errs, err, writeEvidenceFile(filepath.Join(nd, "data-dir.txt"), listing))
		n.mu.Lock()
		snap := n.snapshots
		n.mu.Unlock()
		if snap != nil {
			errs = append(errs, writeEvidenceFile(filepath.Join(nd, "debug-snapshots.json"), snap))
		}
	}
	for i, s := range sinks {
		sd := filepath.Join(base, "otlp-"+strconv.Itoa(i+1))
		var summary bytes.Buffer
		for _, r := range s.Requests() {
			name := fmt.Sprintf("%06d-%s.pb", r.Seq, r.Signal)
			errs = append(errs, writeEvidenceFile(filepath.Join(sd, name), r.Raw))
			fmt.Fprintf(&summary, "%d %s from %s at %s, %d bytes\n", r.Seq, r.Signal, r.Peer, r.Received.Format("15:04:05.000000"), len(r.Raw))
		}
		for _, id := range s.TraceIDs() {
			fmt.Fprintf(&summary, "\ntrace %s\n%s", id, s.FormatTree(id))
		}
		for _, l := range s.Logs() {
			fmt.Fprintf(&summary, "log %s\n", l.Body())
		}
		for _, name := range s.MetricNames() {
			fmt.Fprintf(&summary, "metric %s\n", name)
		}
		errs = append(errs, writeEvidenceFile(filepath.Join(sd, "summary.txt"), summary.Bytes()))
	}
	for _, b := range balancers {
		var log bytes.Buffer
		for _, tr := range b.Transitions() {
			fmt.Fprintf(&log, "%s %s %s -> %s status %d reason %q err %v\n",
				tr.At.Format("2006-01-02T15:04:05.000000Z07:00"), tr.Backend, tr.From, tr.To, tr.Probe.Status, tr.Probe.Reason, tr.Probe.Err)
		}
		errs = append(errs, writeEvidenceFile(filepath.Join(base, "balancer-"+fileName(b.listener)+".txt"), log.Bytes()))
	}
	return base, errors.Join(errs...)
}

// copyDir copies the regular files of src into dst.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // G304: a log file the harness wrote
		if err != nil {
			return err
		}
		if err := writeEvidenceFile(filepath.Join(dst, e.Name()), data); err != nil {
			return err
		}
	}
	return nil
}

// listDir lists every entry under root: mode, size and slash path.
func listDir(root string) ([]byte, error) {
	var b bytes.Buffer
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "error %s: %v\n", path, err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(&b, "error %s: %v\n", path, err)
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(&b, "%v %d %s\n", info.Mode(), info.Size(), filepath.ToSlash(rel))
		return nil
	})
	return b.Bytes(), err
}

// writeEvidenceFile writes one evidence file, creating its directory.
func writeEvidenceFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { //nolint:gosec // G703: a path under the artifact directory the test run names
		return err
	}
	return os.WriteFile(path, data, 0o600) //nolint:gosec // G703: a path under the artifact directory; the data are the harness's own captures
}
