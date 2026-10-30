// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
)

// session runs svc behind Serve on pipes and returns its client and a
// function that ends the session and returns the final Response.
func session(t *testing.T, svc Service) (*Client, func() Response) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = Serve(t.Context(), svc, inR, outW)
		_ = outW.Close()
	})
	c, err := NewClient(inW, outR)
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	end := func() Response {
		ended = true
		_ = inW.Close()
		last, err := c.Last()
		if err != nil {
			t.Fatalf("final response: %v", err)
		}
		_, _ = io.Copy(io.Discard, outR)
		wg.Wait()
		return last
	}
	t.Cleanup(func() {
		if !ended {
			end()
		}
	})
	return c, end
}

// TestMockSession checks the mock service over the protocol: the ready
// line, the 1 KiB body, the counters and the excess histogram, reset, the
// refusal of an unknown operation, and the final counters once stdin ends.
func TestMockSession(t *testing.T) {
	m, err := StartMock(t.Context(), MockConfig{BodySize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	c, end := session(t, m)
	if c.Ready.Service != "mock" || c.Ready.Addr == "" || c.Ready.CPUs < 1 {
		t.Fatalf("ready = %+v", c.Ready)
	}
	for range 3 {
		resp, err := http.Get("http://" + c.Ready.Addr + "/bench/1k") //nolint:noctx // a loopback test request
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if len(b) != 1024 {
			t.Fatalf("body = %d bytes, want 1024", len(b))
		}
	}
	st, err := c.Do(Request{Op: OpStats})
	if err != nil {
		t.Fatal(err)
	}
	if st.Mock == nil || st.Mock.Requests != 3 {
		t.Fatalf("stats = %+v", st.Mock)
	}
	h, err := DecodeHistogram(st.Mock.Excess)
	if err != nil || h.Count() != 3 {
		t.Fatalf("excess histogram: %v, %d values", err, h.Count())
	}
	if _, err := c.Do(Request{Op: OpReset}); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Do(Request{Op: OpStats}); st.Mock.Requests != 0 {
		t.Errorf("after reset: %d requests", st.Mock.Requests)
	}
	if _, err := c.Do(Request{Op: "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("unknown operation: %v", err)
	}
	if last := end(); !last.OK || last.Mock == nil {
		t.Errorf("final response = %+v", last)
	}
}

// TestSinkSession checks the sink's windows over the protocol: a window is
// declared, its result answered (empty here), removed, and a bad window
// refused.
func TestSinkSession(t *testing.T) {
	s, err := StartSink(t.Context(), SinkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := session(t, s)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, err := c.Do(Request{Op: OpWindow, Name: "run-1", Start: base, End: base.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Do(Request{Op: OpResult, Name: "run-1"})
	if err != nil || r.Window == nil || r.Window.Count != 0 {
		t.Fatalf("result = %+v, %v", r.Window, err)
	}
	if _, err := c.Do(Request{Op: OpWindow, Name: "run-1", Start: base, End: base.Add(time.Minute)}); err == nil {
		t.Error("duplicate window accepted")
	}
	if _, err := c.Do(Request{Op: OpRemove, Name: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Op: OpResult, Name: "run-1"}); err == nil {
		t.Error("removed window answered")
	}
	st, err := c.Do(Request{Op: OpStats})
	if err != nil || st.Sink == nil {
		t.Fatalf("stats = %+v, %v", st, err)
	}
}

// TestLoadAgainstMock runs the load generator command's run against a
// loopback server: every request offered, achieved and counted, with the key header
// and minted bearer tokens the mock receives.
func TestLoadAgainstMock(t *testing.T) {
	var mu sync.Mutex
	var auth, keys []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth, keys = append(auth, r.Header.Get("Authorization")), append(keys, r.Header.Get("X-Bench-Key"))
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}), ReadHeaderTimeout: 10 * time.Second}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	kf, err := NewKeyFile(mockidp.RS256, 2)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunLoad(t.Context(), LoadSpec{
		URL: "http://" + ln.Addr().String() + "/bench/1k", Protocol: "http1", Connections: 4,
		Rate: 200, Duration: 200 * time.Millisecond,
		KeyHeader: "x-bench-key", Keys: &KeySpec{Kind: KeysCycle, Prefix: "k", N: 10},
		Tokens: &TokenSpec{Count: 3, Keys: *kf, Issuer: "https://idp.bench.example", Audience: "bench", Expires: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Offered != 40 || res.Achieved != 40 || res.RateError != 0 {
		t.Errorf("offered %d, achieved %d, rate error %v; want 40, 40, 0", res.Offered, res.Achieved, res.RateError)
	}
	h, err := DecodeHistogram(res.Histogram)
	if err != nil || h.Count() != 40 {
		t.Errorf("measured histogram: %v, %d values", err, h.Count())
	}
	if res.KeyCardinality != 10 || res.KeyDistribution != "uniform" || res.IntervalLog == "" {
		t.Errorf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	distinct := slices.Compact(slices.Sorted(slices.Values(auth)))
	if len(distinct) != 3 || !strings.HasPrefix(distinct[0], "Bearer ") {
		t.Errorf("tokens seen = %d distinct (%v), want 3 bearer tokens", len(distinct), distinct[:min(1, len(distinct))])
	}
	if !slices.Contains(keys, "k0") || !slices.Contains(keys, "k9") {
		t.Errorf("keys seen = %v", keys[:min(5, len(keys))])
	}
}

// TestKeyFileRoundTrip checks that a key file's keys come back as signers
// whose tokens verify against the published JWKS.
func TestKeyFileRoundTrip(t *testing.T) {
	kf, err := NewKeyFile(mockidp.ES256, 2)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	b, _ := json.Marshal(kf)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := ReadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := back.Signers()
	if err != nil || len(keys) != 2 || keys[0].ID != kf.Keys[0].Kid {
		t.Fatalf("signers: %v %+v", err, keys)
	}
	idp, err := StartIdP(t.Context(), IdPConfig{Issuer: "https://idp.bench.example", Keys: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idp.Close() }()
	if !bytes.Contains(idp.s.JWKS(), []byte(keys[1].ID)) {
		t.Error("the IdP does not publish the key file's keys")
	}
	bad := KeyFile{Keys: []KeyEntry{{Alg: "RS256", Kid: "x", PEM: "not pem"}}}
	if _, err := bad.Signers(); err == nil {
		t.Error("a key without PEM accepted")
	}
}

// TestEchoRoundTrip checks the round-trip check against the echo service.
func TestEchoRoundTrip(t *testing.T) {
	e, err := StartEcho(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	h, err := RoundTrip(t.Context(), e.ln.Addr().String(), 50)
	if err != nil || h.Count() != 50 {
		t.Fatalf("RoundTrip: %v, %d values", err, h.Count())
	}
}

// TestHostFacts reads a fake /proc and /sys tree.
func TestHostFacts(t *testing.T) {
	root := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/sys/kernel/osrelease", "6.12.48-1\n")
	write("proc/sys/kernel/hostname", "rh-1\n")
	write("proc/sys/net/ipv4/ip_local_port_range", "1024\t65535\n")
	write("proc/cpuinfo", "processor\t: 0\ncpu family\t: 25\nmodel\t\t: 17\nmodel name\t: AMD EPYC 9124\n")
	write("proc/self/status", "Name:\tx\nCpus_allowed_list:\t2-5\n")
	write("sys/devices/system/cpu/smt/active", "0\n")
	write("sys/kernel/mm/transparent_hugepage/enabled", "always [madvise] never\n")
	write("sys/devices/system/cpu/cpu0/cpufreq/scaling_governor", "performance\n")
	write("sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq", "3000000\n")
	write("sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq", "3000000\n")
	write("sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq", "2999000\n")
	write("sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count", "7\n")
	write("sys/class/net/eth0/speed", "25000\n")
	write("sys/class/net/eth0/mtu", "1500\n")
	f := ReadHostFacts(root)
	if f.Kernel != "6.12.48-1" || f.Hostname != "rh-1" || f.SMT != "0" || f.THP != "madvise" || f.Cpuset != "2-5" {
		t.Errorf("facts = %+v", f)
	}
	if f.Sysctls["net.ipv4.ip_local_port_range"] != "1024 65535" {
		t.Errorf("sysctls = %v", f.Sysctls)
	}
	if !strings.HasPrefix(f.Microarchitecture, "AMD EPYC 9124 (family 25, model 17)") {
		t.Errorf("microarchitecture = %q", f.Microarchitecture)
	}
	if len(f.CPUs) != 1 || f.CPUs[0].Governor != "performance" || f.CPUs[0].MaxKHz != 3000000 || f.CPUs[0].CoreThrottle != 7 {
		t.Errorf("cpus = %+v", f.CPUs)
	}
	if f.NIC["eth0"] != "speed=25000 mtu=1500" {
		t.Errorf("nic = %v", f.NIC)
	}
}

// TestParseCPUList parses cpuset lists.
func TestParseCPUList(t *testing.T) {
	for in, want := range map[string][]int{"0-3": {0, 1, 2, 3}, "2,4-5": {2, 4, 5}, "7": {7}} {
		if got, ok := ParseCPUList(in); !ok || !slices.Equal(got, want) {
			t.Errorf("ParseCPUList(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "3-1", "x", "-1"} {
		if _, ok := ParseCPUList(in); ok {
			t.Errorf("ParseCPUList(%q) accepted", in)
		}
	}
}

// TestMainUsage checks Main's usage errors and the facts command.
func TestMainUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(t.Context(), nil, strings.NewReader(""), &out, &errb); code != ExitUsage {
		t.Errorf("no command: %d", code)
	}
	if code := Main(t.Context(), []string{"bogus"}, strings.NewReader(""), &out, &errb); code != ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code := Main(t.Context(), []string{"mock", "extra"}, strings.NewReader(""), &out, &errb); code != ExitUsage {
		t.Errorf("extra argument: %d", code)
	}
	if code := Main(t.Context(), []string{"loadgen"}, strings.NewReader(""), &out, &errb); code != ExitUsage {
		t.Errorf("loadgen without a spec: %d", code)
	}
	out.Reset()
	if code := Main(t.Context(), []string{"facts", "-root", t.TempDir()}, strings.NewReader(""), &out, &errb); code != ExitOK {
		t.Fatalf("facts: %d %s", code, errb.String())
	}
	var f HostFacts
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		t.Errorf("facts output: %v", err)
	}
	// A service started through Main stops at the end of stdin.
	out.Reset()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if code := Main(ctx, []string{"echo"}, strings.NewReader(""), &out, &errb); code != ExitOK {
		t.Fatalf("echo service: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"service":"echo"`) || strings.Count(out.String(), "\n") != 2 {
		t.Errorf("echo session output = %q, want the ready line and the final response", out.String())
	}
}
