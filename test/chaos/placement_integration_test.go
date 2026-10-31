// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package chaos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// loopbackPlacement returns the target placement on the loopback
// inventory rooted in a test directory, which runs the RH-1 hosts'
// services as child processes of the harness on one host, with no scopes:
// labhost built and copied to each host's directory, the State Store
// server the host has, else the pinned image with Docker. It skips
// without either, unless a State Store flavor is selected.
func loopbackPlacement(t *testing.T) *Placement {
	t.Helper()
	work := t.TempDir()
	p := harness.Loopback(work)
	if p.Services.StateStoreServer == "" {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
			if v := os.Getenv(redisserver.EnvStateStore); v != "" {
				t.Fatalf("%s=%s selects a State Store, and this host has no valkey-server or redis-server nor a Docker daemon for the pinned image: %v", redisserver.EnvStateStore, v, err)
			}
			t.Skipf("no valkey-server or redis-server (%s) and no Docker daemon for the pinned image: %v", harness.EnvStateStoreServer, err)
		}
	}
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := NewPlacement(t.Context(), p, root, work)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// TestPlacementLoopbackServices places the target profile's services on
// the loopback inventory: three named mock Upstreams under labhost mock;
// a sentinel State Store, TLS and an ACL user, each server under labhost
// run, whose primary SIGSTOP freezes and SIGCONT resumes, and which the
// sentinel fails over to its replica once the primary is SIGKILLed; and
// the load under labhost load, on a clock whose offset from the harness's
// is within its bound on one host, its records returned once its load
// ended, and a saturation step.
func TestPlacementLoopbackServices(t *testing.T) {
	pl := loopbackPlacement(t)
	ctx := t.Context()
	addrs, err := pl.StartMocks(ctx, t, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range addrs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a+PathPlain, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if want := "backend-" + strconv.Itoa(i+1); resp.StatusCode != http.StatusOK || resp.Header.Get(mockup.HeaderServer) != want {
			t.Errorf("mock %d answered %d as %q, want 200 as %s", i, resp.StatusCode, resp.Header.Get(mockup.HeaderServer), want)
		}
	}

	pw := make([]byte, 12)
	if _, err := rand.Read(pw); err != nil {
		t.Fatal(err)
	}
	dep := redisserver.StartOn(t, pl.StateStoreHost(), redisserver.ProcessSentinel, redisserver.Options{
		TLS: true, Users: []redisserver.User{{Name: "ruralz", Password: hex.EncodeToString(pw)}},
	})
	primary, replica := dep.Primary(), dep.Replica()
	if !strings.Contains(primary.Output(), "Ready to accept connections") {
		t.Errorf("the primary's output through labhost = %q", primary.Output())
	}
	if err := primary.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	frozen := func() bool {
		pctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := primary.Do(pctx, "PING")
		return err != nil
	}
	if !eventually(ctx, frozen) {
		t.Error("the primary still answers after SIGSTOP")
	}
	if err := primary.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Do(ctx, "PING"); err != nil {
		t.Errorf("PING after SIGCONT: %v", err)
	}
	if err := primary.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dep.AwaitFailover(ctx); err != nil {
		t.Fatal(err)
	}
	if dep.Primary() != replica {
		t.Errorf("after the failover the primary is %s, want the replica %s", dep.Primary().Name(), replica.Name())
	}

	s, err := pl.startLoadSession(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if off := s.offset; off > s.bound+time.Millisecond || off < -s.bound-time.Millisecond {
		t.Errorf("the load host's clock offset on one host = %v, beyond its bound %v", off, s.bound)
	}
	l, err := s.startLoad(LoadSpec{Warmup: 100 * time.Millisecond, Duration: time.Second, Streams: []StreamSpec{
		{Name: "limited", Path: PathLimited, Rate: 50, APIKeys: true},
	}}, loadTarget{http: addrs[0]}, testAPIKeys(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	st := l.Streams[0]
	res, reqs := st.Result(), st.Requests()
	if offered := res.Warmup.Offered + res.Measured.Offered; int64(len(reqs)) != offered || offered == 0 {
		t.Errorf("%d records returned, %d requests offered", len(reqs), offered)
	}
	for _, q := range reqs {
		if q.Status != http.StatusOK || !slices.Contains(Consumers(), q.Key) {
			t.Fatalf("request %+v: want 200 keyed by its Consumer", q)
		}
	}
	step, err := s.runStep(saturationStream(addrs[0], 100), 50*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if step.Measured.Offered == 0 {
		t.Errorf("saturation step offered nothing: %+v", step.Summary())
	}
}

// TestPlacementLoopbackRun runs a chaos run on the target placement on the
// loopback inventory: the Nodes, on the controllable-clock build, each
// with GOMAXPROCS 1, behind balancers on the gateway host's address; the
// mock Upstreams labhost's on the upstream host; the State Store's servers
// labhost's on the State Store host; and the load from the load host,
// whose records return once it ended, attributed and keyed as on the
// harness's host, into a record that names the load host's clock offset.
func TestPlacementLoopbackRun(t *testing.T) {
	pl := loopbackPlacement(t)
	ctx, cancel := context.WithTimeout(t.Context(), BuildTimeout)
	defer cancel()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := binaries.Build(ctx, root, filepath.Join(t.TempDir(), "functional"), binaries.BuildOptions{Commands: []string{binaries.Ruralzd}, TestClock: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{
		Profile:    Profile{Name: Reduced, Nodes: 2, CE5Nodes: 2, LoadShare: 0.5, HotRatio: 40},
		Functional: bin, Measurement: bin, Commit: gitCommit(ctx, root), Records: t.TempDir(), Root: root,
		binDir: t.TempDir(), variants: map[string]binaries.Set{}, place: pl,
	}
	if h.pl, err = newPipeline(); err != nil {
		t.Fatal(err)
	}
	r := h.Start(t, Spec{Name: "placement", StateStore: topology.StateStoreOptions{Replica: true}, SampleEvery: 250 * time.Millisecond})
	for i, m := range r.Mocks {
		if m != nil {
			t.Errorf("mock %d runs in the harness, want labhost's on the upstream host", i)
		}
		if host, _, err := net.SplitHostPort(r.Params.Backends[i]); err != nil || host != "127.0.0.1" {
			t.Errorf("backend %d = %q, want the upstream host's mock", i, r.Params.Backends[i])
		}
	}
	if host, _, _ := net.SplitHostPort(r.HTTP.Addr()); host != "127.0.0.1" {
		t.Errorf("the balancer listens on %s, want the gateway host's address", r.HTTP.Addr())
	}
	for _, n := range r.Top.Nodes() {
		env, err := os.ReadFile("/proc/" + strconv.Itoa(n.Process().PID()) + "/environ")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(bytes.Split(env, []byte{0}), func(kv []byte) bool { return string(kv) == "GOMAXPROCS=1" }) {
			t.Errorf("%s runs without GOMAXPROCS=1", n.Name())
		}
	}
	if !strings.Contains(r.Store.Deployment().Primary().Output(), "Ready to accept connections") {
		t.Errorf("the State Store's primary is not labhost's: output %q", r.Store.Deployment().Primary().Output())
	}
	plan := PlanLimits(40, r.Nodes)
	l := r.StartLoad(LoadSpec{Warmup: 500 * time.Millisecond, Duration: 3 * time.Second, Streams: plan.Streams()})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, 1500*time.Millisecond)
	r.Hold(l, PhaseRecovery, 1500*time.Millisecond)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		res := st.Result()
		reqs := st.Requests()
		if offered := res.Warmup.Offered + res.Measured.Offered; int64(len(reqs)) != offered || offered == 0 {
			t.Errorf("%s: %d records returned, %d requests offered", st.Spec.Name, len(reqs), offered)
		}
		for _, q := range reqs {
			switch {
			case st.Spec.APIKeys && !slices.Contains(Consumers(), q.Key):
				t.Fatalf("%s: request %d keyed %q, want its Consumer", st.Spec.Name, q.Seq, q.Key)
			case st.Spec.Keys != nil && !slices.Contains(plan.UndeclaredKeys, q.Key):
				t.Fatalf("%s: request %d keyed %q, want one of %q", st.Spec.Name, q.Seq, q.Key, plan.UndeclaredKeys)
			case st.Failed(q) && q.Local != "" && q.Node == "":
				t.Fatalf("%s: failed request %+v attributed to no Node", st.Spec.Name, q)
			}
		}
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	r.Finish()
	b, err := os.ReadFile(filepath.Join(h.Records, (&Record{Experiment: "placement"}).FileName()))
	if err != nil {
		t.Fatal(err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Breakdowns["load_clock_offset_us"]; !ok || len(rec.Phases) != 2 {
		t.Errorf("record breakdowns %v, phases %v: want the load host's clock offset and both phases", rec.Breakdowns, rec.Phases)
	}
}

// eventually polls cond until it holds or ten seconds pass.
func eventually(ctx context.Context, cond func() bool) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // a hang guard only
	defer cancel()
	for !cond() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(topology.PollInterval):
		}
	}
	return true
}
