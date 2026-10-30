// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// External load tools a LoadSpec may name (PBB "Open-loop load and
// coordinated omission"): oha drives S1 and its ladder, vegeta S1's
// cross-check. loadgen, the default, drives everything else.
const (
	ToolLoadgen = "loadgen"
	ToolOha     = "oha"
	ToolVegeta  = "vegeta"
)

// Settle rule windows (PBB "Warm-up, duration and percentiles"): 10 s
// windows, three compared, 5% (target); 1 s interval log windows.
const (
	settleWindow   = 10 * time.Second
	intervalWindow = time.Second
)

// sample is one request a tool reported: its intended start offset from
// the phase start, its latency from that intended start and its status (0
// for a failed request).
type sample struct {
	start   time.Duration
	latency time.Duration
	status  int
}

// phaseResult aggregates one tool invocation.
type phaseResult struct {
	hist     *loadgen.Histogram
	windows  map[int64]*loadgen.Histogram // by window index
	window   time.Duration
	ok       int64
	statuses map[int]int64
	rows     int64
}

func newPhase(window time.Duration) *phaseResult {
	return &phaseResult{hist: newHistogram(), windows: map[int64]*loadgen.Histogram{}, window: window, statuses: map[int]int64{}}
}

func (p *phaseResult) add(s sample) {
	p.rows++
	p.statuses[s.status]++
	if s.status < 200 || s.status > 299 {
		return
	}
	p.ok++
	p.hist.Record(s.latency)
	i := int64(s.start / p.window)
	w, ok := p.windows[i]
	if !ok {
		w = newHistogram()
		p.windows[i] = w
	}
	w.Record(s.latency)
}

// p99s returns the p99 of every complete window in order.
func (p *phaseResult) p99s(d time.Duration) []time.Duration {
	var out []time.Duration
	for i := range int64(d / p.window) {
		if w, ok := p.windows[i]; ok {
			out = append(out, w.ValueAtPercentile(99))
		} else {
			out = append(out, 0)
		}
	}
	return out
}

// toolPhase runs one tool invocation of d and parses its per-request
// lines.
type toolPhase func(ctx context.Context, d time.Duration, window time.Duration) (*phaseResult, time.Duration, error)

// RunTool runs s with an external tool, each phase its own invocation:
// the warm-up, its 30 s extensions until the settle rule holds or 180 s
// passed, then the measured run. It builds the histograms, the windows
// and the interval log from the per-request lines.
func RunTool(ctx context.Context, s LoadSpec) (*LoadResult, error) {
	var run toolPhase
	switch s.Tool {
	case ToolOha:
		run = func(ctx context.Context, d, window time.Duration) (*phaseResult, time.Duration, error) {
			return runOha(ctx, s, d, window)
		}
	case ToolVegeta:
		run = func(ctx context.Context, d, window time.Duration) (*phaseResult, time.Duration, error) {
			return runVegeta(ctx, s, d, window)
		}
	default:
		return nil, fmt.Errorf("lab: unknown tool %q", s.Tool)
	}
	res := &LoadResult{KeyDistribution: "none"}
	var cpu time.Duration
	start := time.Now()
	warm := newHistogram()
	settle := loadgen.BenchmarkSettle()
	var p99s []time.Duration
	elapsed := time.Duration(0)
	next := s.Warmup
	for next > 0 {
		ph, used, err := run(ctx, next, settleWindow)
		cpu += used
		if err != nil {
			return nil, err
		}
		_ = warm.Merge(ph.hist)
		p99s = append(p99s, ph.p99s(next)...)
		elapsed += next
		next = 0
		if s.Settle && !(len(p99s) >= settle.Windows && loadgen.Settled(p99s[len(p99s)-settle.Windows:], settle.MaxSpread)) &&
			elapsed+settle.Step <= settle.Max {
			next = settle.Step
		}
	}
	res.Settled = !s.Settle || (len(p99s) >= settle.Windows && loadgen.Settled(p99s[len(p99s)-settle.Windows:], settle.MaxSpread))
	res.MeasureStart = time.Now()
	ph, used, err := run(ctx, s.Duration, intervalWindow)
	cpu += used
	if err != nil {
		return nil, err
	}
	res.MeasureEnd = res.MeasureStart.Add(s.Duration)
	offered := int64(s.Rate * s.Duration.Seconds())
	res.Offered, res.Achieved = offered, ph.ok
	if s.Duration > 0 {
		res.AchievedRate = float64(ph.ok) / s.Duration.Seconds()
	}
	if offered > 0 {
		res.RateError = max(float64(offered-ph.ok)/float64(offered), 0)
		res.ErrorRate = max(float64(offered-ph.ok)/float64(offered), 0)
	}
	res.Statuses = map[int]int64{}
	maps.Copy(res.Statuses, ph.statuses)
	delete(res.Statuses, 0)
	res.Outcomes = map[string]int64{"success": ph.ok}
	if failed := ph.statuses[0]; failed > 0 {
		res.Outcomes["failed"] = failed
	}
	if missing := offered - ph.rows; missing > 0 {
		res.Outcomes["missed"] = missing
	}
	res.Histogram, res.Warmup = EncodeHistogram(ph.hist), EncodeHistogram(warm)
	var log bytes.Buffer
	if lw, err := loadgen.NewLogWriter(&log, res.MeasureStart, "ruralz bench interval log ("+s.Tool+")"); err == nil {
		for _, i := range slices.Sorted(maps.Keys(ph.windows)) {
			ws := res.MeasureStart.Add(time.Duration(i) * intervalWindow)
			_ = lw.Write("", ws, ws.Add(intervalWindow), ph.windows[i])
		}
	}
	res.IntervalLog = base64.StdEncoding.EncodeToString(log.Bytes())
	switch {
	case !res.Settled:
		res.Invalid = "the warm-up did not settle within 180 s"
	case res.RateError > loadgen.DefaultMaxRateError:
		res.Invalid = fmt.Sprintf("achieved %d of %d offered requests (rate error %.4f)", ph.ok, offered, res.RateError)
	default:
		res.Valid = true
	}
	wall := time.Since(start).Seconds()
	res.Process = ProcessStats{CPUSeconds: cpu.Seconds(), WallSeconds: wall, CPUs: newCPUMeter().stats().CPUs}
	return res, nil
}

// OhaArgs returns oha's arguments for one phase of d: a fixed rate with
// latency correction and per-request CSV lines.
func OhaArgs(s LoadSpec, d time.Duration, caFile string) []string {
	a := []string{"-z", d.String(), "-q", strconv.FormatFloat(s.Rate, 'f', -1, 64), "-c", strconv.Itoa(max(s.Connections, 1)),
		"--latency-correction", "--no-tui", "--output-format", "csv"}
	if s.Method != "" && s.Method != "GET" {
		a = append(a, "-m", s.Method)
	}
	if s.Host != "" {
		a = append(a, "-H", "Host: "+s.Host)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Header)) {
		for _, v := range s.Header[k] {
			a = append(a, "-H", k+": "+v)
		}
	}
	if s.Protocol == "h2" || s.Protocol == "h2c" {
		a = append(a, "--http2")
	}
	if caFile != "" {
		a = append(a, "--cacert", caFile)
	}
	return append(a, s.URL)
}

// writeCA writes the spec's CA bundle to a temporary file; "" without
// one. The caller removes the returned directory.
func writeCA(s LoadSpec) (string, string, error) {
	if s.CA == "" {
		return "", "", nil
	}
	dir, err := os.MkdirTemp("", "labhost-ca-")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte(s.CA), 0o600); err != nil {
		return dir, "", err
	}
	return dir, path, nil
}

// runOha runs one oha phase and parses its CSV lines.
func runOha(ctx context.Context, s LoadSpec, d, window time.Duration) (*phaseResult, time.Duration, error) {
	dir, ca, err := writeCA(s)
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }()
	}
	if err != nil {
		return nil, 0, err
	}
	cmd := exec.CommandContext(ctx, s.ToolPath, OhaArgs(s, d, ca)...) //nolint:gosec // the harness names the tool
	ph := newPhase(window)
	used, err := streamCommand(cmd, func(r io.Reader) error { return ParseOhaCSV(r, ph.add) })
	return ph, used, err
}

// ParseOhaCSV parses oha's --output-format csv lines: request-start and
// request-duration in seconds, which --latency-correction measures from
// the intended send time, and status. Columns are found by header name.
func ParseOhaCSV(r io.Reader, add func(sample)) error {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.ReuseRecord = true
	head, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lab: oha csv header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	si, ok1 := col["request-start"]
	di, ok2 := col["request-duration"]
	ti, ok3 := col["status"]
	if !ok1 || !ok2 || !ok3 {
		return fmt.Errorf("lab: oha csv header %q lacks request-start, request-duration or status", strings.Join(head, ","))
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lab: oha csv: %w", err)
		}
		start, err1 := strconv.ParseFloat(rec[si], 64)
		dur, err2 := strconv.ParseFloat(rec[di], 64)
		status, err3 := strconv.Atoi(rec[ti])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("lab: oha csv line %q: bad number", strings.Join(rec, ","))
		}
		if err3 != nil {
			status = 0
		}
		add(sample{start: secondsDuration(start), latency: secondsDuration(dur), status: status})
	}
}

// secondsDuration converts seconds to a duration, rounded to the
// nanosecond so a decimal value keeps its digits.
func secondsDuration(s float64) time.Duration {
	return time.Duration(math.Round(s * float64(time.Second)))
}

// VegetaArgs returns vegeta attack's arguments for one phase of d.
func VegetaArgs(s LoadSpec, d time.Duration, caFile string) []string {
	a := []string{"attack", "-rate", strconv.FormatFloat(s.Rate, 'f', -1, 64) + "/s", "-duration", d.String(),
		"-connections", strconv.Itoa(max(s.Connections, 1)), "-max-connections", strconv.Itoa(max(s.Connections, 1)),
		"-keepalive=true", "-max-body", "0", "-http2=" + strconv.FormatBool(s.Protocol == "h2")}
	if s.Protocol == "h2c" {
		a = append(a, "-h2c")
	}
	if caFile != "" {
		a = append(a, "-root-certs", caFile)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Header)) {
		for _, v := range s.Header[k] {
			a = append(a, "-header", k+": "+v)
		}
	}
	if s.Host != "" {
		a = append(a, "-header", "Host: "+s.Host)
	}
	return a
}

// runVegeta runs one vegeta phase: attack piped into encode -to csv.
func runVegeta(ctx context.Context, s LoadSpec, d, window time.Duration) (*phaseResult, time.Duration, error) {
	dir, ca, err := writeCA(s)
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }()
	}
	if err != nil {
		return nil, 0, err
	}
	method := s.Method
	if method == "" {
		method = "GET"
	}
	attack := exec.CommandContext(ctx, s.ToolPath, VegetaArgs(s, d, ca)...) //nolint:gosec // the harness names the tool
	attack.Stdin = strings.NewReader(method + " " + s.URL + "\n")
	encode := exec.CommandContext(ctx, s.ToolPath, "encode", "-to", "csv") //nolint:gosec // the harness names the tool
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, 0, err
	}
	attack.Stdout, encode.Stdin = pw, pr
	attackErr := &bytes.Buffer{}
	attack.Stderr = attackErr
	if err := attack.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, 0, fmt.Errorf("lab: vegeta attack: %w", err)
	}
	_ = pw.Close()
	ph := newPhase(window)
	enc := &vegetaParser{rate: s.Rate, add: ph.add}
	used, encErr := streamCommand(encode, enc.parse)
	_ = pr.Close()
	waitErr := attack.Wait()
	if attack.ProcessState != nil {
		used += attack.ProcessState.UserTime() + attack.ProcessState.SystemTime()
	}
	if waitErr != nil {
		return ph, used, fmt.Errorf("lab: vegeta attack: %w: %s", waitErr, attackErr.String())
	}
	return ph, used, encErr
}

// vegetaParser parses vegeta's CSV encoding: timestamp (Unix ns), status,
// latency (ns), bytes out, bytes in, error, body, attack, seq, method, URL,
// headers. vegeta schedules request seq at seq / rate after the first; the
// latency from that intended time adds any lateness of the actual start.
type vegetaParser struct {
	rate float64
	add  func(sample)
	t0   int64
	seen bool
}

func (v *vegetaParser) parse(r io.Reader) error {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lab: vegeta csv: %w", err)
		}
		if len(rec) < 9 {
			return fmt.Errorf("lab: vegeta csv line has %d fields, want at least 9", len(rec))
		}
		ts, err1 := strconv.ParseInt(rec[0], 10, 64)
		status, err2 := strconv.Atoi(rec[1])
		lat, err3 := strconv.ParseInt(rec[2], 10, 64)
		seq, err4 := strconv.ParseInt(rec[8], 10, 64)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return fmt.Errorf("lab: vegeta csv line: %w", err)
		}
		v.sample(ts, status, lat, seq)
	}
}

// sample converts one vegeta line.
func (v *vegetaParser) sample(ts int64, status int, lat, seq int64) {
	if !v.seen || seq == 0 {
		v.t0, v.seen = ts-int64(float64(seq)/v.rate*float64(time.Second)), true
	}
	intended := v.t0 + int64(float64(seq)/v.rate*float64(time.Second))
	late := max(ts-intended, 0)
	v.add(sample{start: time.Duration(intended - v.t0), latency: time.Duration(late + lat), status: status})
}

// streamCommand starts cmd, hands its standard output to parse and waits;
// it returns the command's CPU time.
func streamCommand(cmd *exec.Cmd, parse func(io.Reader) error) (time.Duration, error) {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var errb bytes.Buffer
	if cmd.Stderr == nil {
		cmd.Stderr = &errb
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("lab: %s: %w", filepath.Base(cmd.Path), err)
	}
	perr := parse(out)
	if perr != nil {
		_, _ = io.Copy(io.Discard, out)
	}
	werr := cmd.Wait()
	var used time.Duration
	if cmd.ProcessState != nil {
		used = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	if werr != nil {
		return used, fmt.Errorf("lab: %s: %w: %s", filepath.Base(cmd.Path), werr, strings.TrimSpace(errb.String()))
	}
	return used, perr
}
