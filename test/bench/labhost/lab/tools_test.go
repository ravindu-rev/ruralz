// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ohaCSV is oha 1.16.0's --output-format csv output, captured.
const ohaCSV = `request-start,DNS,DNS+dialup,Response-delay,request-duration,bytes,status
0.00169812,0.000027786,0.000219972,0.004102446,0.004140148,1024,200
0.050936956,0.000021855,0.000121907,0.000373951,0.000407664,1024,200
0.100570516,0,0,0.00129679,0.001323465,1024,503
0.151159509,0,0,0.000221232,0.000243066,1024,200
`

func TestParseOhaCSV(t *testing.T) {
	var got []sample
	if err := ParseOhaCSV(strings.NewReader(ohaCSV), func(s sample) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].latency != 4140148*time.Nanosecond || got[2].status != 503 || got[3].start != 151159509*time.Nanosecond {
		t.Errorf("ParseOhaCSV = %+v", got)
	}
	for name, in := range map[string]string{
		"missing column": "request-start,status\n1,200\n",
		"bad number":     "request-start,request-duration,status\nx,1,200\n",
	} {
		if err := ParseOhaCSV(strings.NewReader(in), func(sample) {}); err == nil {
			t.Errorf("ParseOhaCSV(%s) = nil error", name)
		}
	}
	if err := ParseOhaCSV(strings.NewReader(""), func(sample) {}); err != nil {
		t.Errorf("ParseOhaCSV(empty) = %v", err)
	}
}

// TestVegetaParser covers vegeta's CSV encoding and the latency from the
// intended send time: request seq is due at seq / rate after the first,
// and a late start adds to its latency.
func TestVegetaParser(t *testing.T) {
	const t0 = int64(1793363291773621389)
	lines := fmt.Sprintf("%d,200,1000000,0,1024,,,,0,GET,http://x/,\n%d,200,1000000,0,1024,,,,1,GET,http://x/,\n%d,0,0,0,0,dial error,,,2,GET,http://x/,\n",
		t0, t0+int64(60*time.Millisecond), t0+int64(100*time.Millisecond))
	var got []sample
	v := &vegetaParser{rate: 20, add: func(s sample) { got = append(got, s) }}
	if err := v.parse(strings.NewReader(lines)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d samples", len(got))
	}
	if got[0].latency != time.Millisecond || got[1].start != 50*time.Millisecond || got[1].latency != 11*time.Millisecond || got[2].status != 0 {
		t.Errorf("samples = %+v; seq 1 was due at 50ms, started 10ms late", got)
	}
	if err := (&vegetaParser{rate: 1, add: func(sample) {}}).parse(strings.NewReader("1,2,3\n")); err == nil {
		t.Error("a short line = nil error")
	}
}

func TestToolArgs(t *testing.T) {
	s := LoadSpec{URL: "https://10.0.0.1:8443/bench/1k", Rate: 40000, Connections: 256, Protocol: "h2", Host: "g.example", Header: map[string][]string{"X-B": {"1"}, "X-A": {"2"}}}
	oha := OhaArgs(s, 180*time.Second, "/ca.pem")
	want := []string{
		"-z", "3m0s", "-q", "40000", "-c", "256", "--latency-correction", "--no-tui", "--output-format", "csv",
		"-H", "Host: g.example", "-H", "X-A: 2", "-H", "X-B: 1", "--http2", "--cacert", "/ca.pem", "https://10.0.0.1:8443/bench/1k",
	}
	if !slices.Equal(oha, want) {
		t.Errorf("OhaArgs = %q\nwant %q", oha, want)
	}
	veg := VegetaArgs(LoadSpec{Rate: 40000, Connections: 256, Protocol: "http1"}, 180*time.Second, "")
	if !slices.Contains(veg, "-rate") || !slices.Contains(veg, "40000/s") || !slices.Contains(veg, "-http2=false") || !slices.Contains(veg, "-keepalive=true") {
		t.Errorf("VegetaArgs = %q", veg)
	}
}

// fakeOha writes a shell script that prints rate × duration CSV lines of
// 1 ms each, as oha would for a run of -z at -q.
func fakeOha(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oha")
	script := `#!/bin/sh
d=""; q=""
while [ $# -gt 0 ]; do
  case "$1" in
    -z) d="$2"; shift ;;
    -q) q="$2"; shift ;;
  esac
  shift
done
secs=${d%s}
n=$(( ${q%.*} * ${secs%.*} ))
echo "request-start,DNS,DNS+dialup,Response-delay,request-duration,bytes,status"
i=0
while [ $i -lt $n ]; do
  echo "0.$(printf '%03d' $((i % 1000))),0,0,0.001,0.001,1024,200"
  i=$((i + 1))
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the test's own executable script
		t.Fatal(err)
	}
	return path
}

// TestRunToolPhases runs an external tool through every phase: the
// warm-up, then the measured run, building the histograms, the interval
// log and the counts from its per-request lines.
func TestRunToolPhases(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	s := LoadSpec{Tool: ToolOha, ToolPath: fakeOha(t), URL: "http://127.0.0.1:1/", Rate: 50, Connections: 1, Warmup: time.Second, Duration: 2 * time.Second}
	res, err := RunLoad(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Valid || res.Offered != 100 || res.Achieved != 100 || res.RateError != 0 || !res.Settled {
		t.Errorf("RunLoad = %+v", res)
	}
	h, err := DecodeHistogram(res.Histogram)
	if err != nil || h.Count() != 100 || h.ValueAtPercentile(99) < 999*time.Microsecond {
		t.Errorf("measured histogram %v, %v", h, err)
	}
	w, err := DecodeHistogram(res.Warmup)
	if err != nil || w.Count() != 50 {
		t.Errorf("warm-up histogram count %d, %v", w.Count(), err)
	}
	if res.IntervalLog == "" || res.Statuses[200] != 100 {
		t.Errorf("interval log %q statuses %v", res.IntervalLog, res.Statuses)
	}
	s.Rate = 100 // the script prints fewer lines than a doubled rate needs
	s.ToolPath = writeScript(t, "#!/bin/sh\necho request-start,request-duration,status\necho 0.1,0.001,200\n")
	res, err = RunLoad(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Valid || res.Outcomes["missed"] == 0 || !strings.Contains(res.Invalid, "rate error") {
		t.Errorf("a short run = %+v, want invalid with missed requests", res)
	}
	if _, err := RunLoad(t.Context(), LoadSpec{Tool: ToolOha}); err == nil {
		t.Error("RunLoad(oha without a path) = nil error")
	}
	if _, err := RunLoad(t.Context(), LoadSpec{Tool: "wrk"}); err == nil {
		t.Error("RunLoad(wrk) = nil error")
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // G306: the test's own executable script
		t.Fatal(err)
	}
	return path
}

// TestHelperListener is a helper process: a TCP listener on
// LAB_TEST_LISTEN that writes a ready line once it listens and runs until
// killed, or exits 3 when the address is taken.
func TestHelperListener(t *testing.T) {
	addr := os.Getenv("LAB_TEST_LISTEN")
	if addr == "" {
		t.Skip("helper process")
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "helper: %v\n", err)
		os.Exit(3)
	}
	_, _ = fmt.Fprintf(os.Stdout, "helper: listening on %s\n", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = c.Close()
	}
}

// TestChild covers the supervised child: ready once its address accepts,
// its CPU in stats, stopped by Close; a child that exits before serving
// fails to start.
func TestChild(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	t.Setenv("LAB_TEST_LISTEN", addr)
	c, err := StartChild(t.Context(), ChildConfig{Argv: []string{os.Args[0], "-test.run", "^TestHelperListener$"}, ReadyAddr: addr, ReadyTimeout: 30 * time.Second, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Ready(); r.Addr != addr || r.Service != "run" || r.PID == os.Getpid() {
		t.Errorf("Ready = %+v", r)
	}
	if resp := c.Handle(context.Background(), Request{Op: OpStats}); !resp.OK {
		t.Errorf("stats = %+v", resp)
	}
	if resp := c.Handle(context.Background(), Request{Op: "nope"}); resp.OK {
		t.Error("unknown op answered OK")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if resp := c.Handle(context.Background(), Request{Op: OpStats}); resp.OK {
		t.Error("stats after the child exited answered OK")
	}
	t.Setenv("LAB_TEST_LISTEN", "")
	if _, err := StartChild(t.Context(), ChildConfig{Argv: []string{os.Args[0], "-test.run", "^TestHelperListener$"}, ReadyAddr: addr, ReadyTimeout: 30 * time.Second}); err == nil {
		t.Error("a child that exits before serving started")
	}
	if _, err := StartChild(t.Context(), ChildConfig{}); err == nil {
		t.Error("StartChild without a command = nil error")
	}
}
