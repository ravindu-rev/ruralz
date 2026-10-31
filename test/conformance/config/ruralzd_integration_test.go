// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// nodeBootTimeout bounds one boot attempt of the built ruralzd, which on
// the controllable clock never waits on a timer; it only stops a hung run.
const nodeBootTimeout = time.Minute

// nodeOutputBytes bounds what the harness keeps of ruralzd's output, far
// above what one boot attempt writes.
const nodeOutputBytes = 16 << 20

// fetchAllowLoopback keeps a Node that activates a corpus Bundle from
// fetching anything outside the test machine (RURALZ_FETCH_ALLOW).
const fetchAllowLoopback = "127.0.0.0/8,::1"

// ruralzdBinary is the built ruralzd in file mode
// (docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites"): one process per run, over an empty environment but for its
// settings, so the Bundle's ${VAR} take only their defaults, with fresh
// data and secret directories, every listener and the admin port on
// loopback, and the controllable-clock build's clock started at the
// CLI's epoch and never moved. Its outcome comes from the records of the
// process's first boot attempt (nodeOutcome), whose rejection and
// activation-warning records carry every diagnostic in its JSON form; the
// process is killed once the attempt ended. A file-mode Node reads no
// environments file and links no stand-in.
type ruralzdBinary struct {
	set binaries.Set
}

func (ruralzdBinary) binary() string  { return binaryNode }
func (ruralzdBinary) inProcess() bool { return false }

func (b ruralzdBinary) validate(ctx context.Context, r run) (outcome, error) {
	switch {
	case r.standIns:
		return outcome{}, errBuiltStandIns
	case r.environments != "":
		return outcome{}, errNodeEnvironments
	}
	stdout, err := b.boot(ctx, r.bundle)
	if err != nil {
		return outcome{}, err
	}
	o, err := nodeOutcome(stdout)
	if err != nil {
		return outcome{}, fmt.Errorf("ruralzd on %s: %w", r.bundle, err)
	}
	return o, nil
}

// boot runs ruralzd on bundle and returns its standard output up to the
// record that ends its first boot attempt (bootEnded).
func (b ruralzdBinary) boot(ctx context.Context, bundle string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeBootTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "ruralz-conformance-node-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	data, secrets := filepath.Join(dir, "data"), filepath.Join(dir, "secrets")
	for _, d := range []string{data, secrets} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	spec := proc.Spec{
		Name: binaryNode, Path: b.set.Ruralzd, RingBytes: nodeOutputBytes,
		Env: []string{
			"RURALZ_CONFIG=" + bundle,
			"RURALZ_DATA_DIR=" + data,
			"RURALZ_SECRET_ROOT=" + secrets,
			"RURALZ_LOG_LEVEL=debug",
			"RURALZ_BIND_LOOPBACK=true",
			"RURALZ_FETCH_ALLOW=" + fetchAllowLoopback,
		},
	}
	var (
		conn  net.Conn
		child *os.File
	)
	if b.set.TestClock {
		if conn, child, err = commandSocket(); err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		spec.ExtraFiles = []*os.File{child}
	}
	p, err := proc.Start(ctx, spec)
	// The child has its own copy now; closing this one at once ends the
	// socket when the child exits, so a dying ruralzd fails the clock's
	// start rather than hanging it.
	if child != nil {
		_ = child.Close()
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = p.Kill()
		<-p.Done()
	}()
	if conn != nil {
		if err := startClock(ctx, conn, cliEpoch()); err != nil {
			return nil, fmt.Errorf("ruralzd on %s: %w%s", bundle, err, outputTail(p))
		}
	}
	if _, err := p.WaitOutput(ctx, regexp.MustCompile(bootEnded())); err != nil {
		return nil, fmt.Errorf("ruralzd on %s: %w%s", bundle, err, outputTail(p))
	}
	stdout, _ := p.Output()
	return stdout, nil
}

// TestBuiltRuralzdRunner checks that the built ruralzd runs the corpus
// where it can start, and that its outcome comes losslessly from its
// records, as the in-process Node path's does: a fixture with more
// diagnostics than one rejection record holds; a fixture whose only
// diagnostic is a warning, which the Node's load reports however its
// activation ends; and an example Bundle that this release refuses after
// the digest, whose digest and serve-check findings the rejection
// carries.
func TestBuiltRuralzdRunner(t *testing.T) {
	s := mustSuite(t)
	var node runner
	for _, p := range binaryRunners(t, s) {
		if p.binary() == binaryNode {
			node = p
		}
	}
	if node == nil {
		if builtBinaries.TestClock && !clockSockets {
			t.Skip("the controllable-clock ruralzd starts only where a process inherits its clock socket")
		}
		t.Fatal("the built ruralzd is not among the runners")
	}
	for _, name := range []string{"RZ-CFG-003-alias-expansion", "RZ-CFG-043-hot-key"} {
		e := entryNamed(s, corpus.NegativeDir+"/"+name)
		if e == nil {
			t.Fatalf("the corpus holds no %s", name)
		}
		o, err := node.validate(t.Context(), fixtureRun(s, e))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(o.text, e.ExpectedText) || !bytes.Equal(o.json, e.Expected) {
			t.Errorf("%s: ruralzd reports\n%s\nexpected.txt holds\n%s", name, o.text, e.ExpectedText)
		}
	}
	if e := entryNamed(s, corpus.NegativeDir+"/RZ-CFG-003-alias-expansion"); bytes.Count(e.ExpectedText, []byte("\n")) <= maxRecordFindings {
		t.Errorf("RZ-CFG-003-alias-expansion holds %d diagnostics, want more than one rejection record holds", bytes.Count(e.ExpectedText, []byte("\n")))
	}
	g := newest(s.golden(), "shop-bundle")
	if g == nil {
		t.Fatal("the corpus holds no shop-bundle entry")
	}
	r := run{bundle: filepath.Join(s.corpusPath(g.Name), corpus.BundleDir)}
	want, err := nodePath{s}.validate(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := node.validate(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if want.digest == "" || len(want.after) == 0 {
		t.Fatalf("shop-bundle: the Node path reports digest %q after %q, want a Revision this release refuses to serve", want.digest, want.after)
	}
	if got.digest != want.digest || !bytes.Equal(got.text, want.text) || !slices.Equal(got.after, want.after) {
		t.Errorf("shop-bundle: ruralzd reports %q after %q\n%s\nthe Node path %q after %q\n%s", got.digest, got.after, got.text, want.digest, want.after, want.text)
	}
}

// maxRecordFindings is how many diagnostics a rejection record holds
// before the rest go to debug records (internal/gateway/reload).
const maxRecordFindings = 10

// startClock starts the controllable-clock build's clock at at: the first
// command on its socket (internal/clock.Commanded), answered with that
// time.
func startClock(ctx context.Context, conn net.Conn, at time.Time) error {
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	line := clock.CommandStart + " " + at.UTC().Format(clock.CommandTimeLayout)
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		return fmt.Errorf("start the clock: %w", err)
	}
	answer, err := bufio.NewReaderSize(conn, clock.MaxCommandBytes).ReadString('\n')
	if err != nil {
		return fmt.Errorf("start the clock: %w", err)
	}
	got, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(answer, "\n"))
	if err != nil || !got.Equal(at) {
		return fmt.Errorf("start the clock at %v: answered %q", at, answer)
	}
	return nil
}

// outputTail returns the end of p's output for an error message.
func outputTail(p *proc.Process) string {
	const keep = 4 << 10
	stdout, stderr := p.Output()
	tail := func(b []byte) []byte { return b[max(0, len(b)-keep):] }
	return fmt.Sprintf("\nstdout:\n%s\nstderr:\n%s", tail(stdout), tail(stderr))
}
