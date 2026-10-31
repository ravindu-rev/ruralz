// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Format is the format identifier of a chaos result record.
const Format = "ruralz.chaos.v1"

// Verdicts of a criterion.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
)

// Record is one ruralz.chaos.v1 result record: what one run of an
// experiment, drill or row injected and measured
// (docs/engineering/03-testing-and-quality-strategy.md "Result records").
type Record struct {
	// Format is Format.
	Format string `json:"format"`
	// Experiment names the run: an experiment, drill or row identifier,
	// with its variant after a slash, such as "CE-5/cluster".
	Experiment string `json:"experiment"`
	// Profile is the scale profile.
	Profile string `json:"profile"`
	// Commit is the commit under test.
	Commit string `json:"commit"`
	// Nodes is the run's Node count.
	Nodes int `json:"nodes"`
	// Phases are the timeline's phases in order.
	Phases []Phase `json:"phases"`
	// Injections are the faults in order.
	Injections []Injection `json:"injections"`
	// Attribution counts the failed requests per Node, as the L4
	// balancer attributes their connections; a failure on no Node's
	// connection counts under "unattributed".
	Attribution map[string]int64 `json:"attribution"`
	// Criteria are the pass criteria in order.
	Criteria []Criterion `json:"criteria"`
	// Breakdowns are ungated values, such as the State Store's
	// cmdstat_evalsha microseconds per second.
	Breakdowns map[string]float64 `json:"breakdowns"`
}

// Phase is one phase of a run's timeline.
type Phase struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Injection is one fault.
type Injection struct {
	// Kind names the fault, such as "sigkill" or "blackhole".
	Kind string `json:"kind"`
	// Target names what it hit, such as "node-3" or "state-store".
	Target string `json:"target"`
	// Time is when it was injected.
	Time time.Time `json:"time"`
}

// Criterion is one pass criterion and its outcome.
type Criterion struct {
	Name string `json:"name"`
	// Value is the measured value.
	Value float64 `json:"value"`
	// Bound is the comparison the value must meet, such as "<= 120".
	Bound string `json:"bound"`
	// Verdict is VerdictPass or VerdictFail.
	Verdict string `json:"verdict"`
	// kind decides whether the profile gates the criterion.
	kind Kind
}

// Kind returns the criterion's kind.
func (c Criterion) Kind() Kind { return c.kind }

// Op is a criterion's comparison.
type Op string

// Comparisons.
const (
	LE Op = "<="
	LT Op = "<"
	GE Op = ">="
	GT Op = ">"
	EQ Op = "=="
)

// Check evaluates value against bound with op as a criterion of kind k.
// A value that is not a finite number fails.
func Check(name string, k Kind, value float64, op Op, bound float64) Criterion {
	pass := false
	if !math.IsNaN(value) && !math.IsInf(value, 0) {
		switch op {
		case LE:
			pass = value <= bound
		case LT:
			pass = value < bound
		case GE:
			pass = value >= bound
		case GT:
			pass = value > bound
		case EQ:
			pass = value == bound
		}
	} else {
		value = 0
	}
	c := Criterion{Name: name, Value: value, Bound: string(op) + " " + formatNumber(bound), Verdict: VerdictFail, kind: k}
	if pass {
		c.Verdict = VerdictPass
	}
	return c
}

// Holds is a criterion of kind k that a condition met (value 1) or not
// (value 0), for a criterion without a natural number.
func Holds(name string, k Kind, ok bool) Criterion {
	v := 0.0
	if ok {
		v = 1
	}
	return Check(name, k, v, EQ, 1)
}

// formatNumber writes a bound without float noise: integers bare, else
// the shortest decimal.
func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Failed returns the criteria of r that failed and that profile p gates.
func (r *Record) Failed(p Profile) []Criterion {
	var out []Criterion
	for _, c := range r.Criteria {
		if c.Verdict == VerdictFail && p.Gates(c.kind) {
			out = append(out, c)
		}
	}
	return out
}

// FileName returns the record's file name in the records directory: the
// experiment with every character but letters, digits, '.', '-' and '_'
// turned into '-', then ".json".
func (r *Record) FileName() string { return fileStem(r.Experiment) + ".json" }

// fileStem turns a run name into a file name stem.
func fileStem(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Validate checks the record's shape: the format, a named experiment and
// profile, a positive Node count, phases in order, criteria with a
// verdict.
func (r *Record) Validate() error {
	var errs []error
	if r.Format != Format {
		errs = append(errs, fmt.Errorf("format %q, want %s", r.Format, Format))
	}
	if r.Experiment == "" || r.Profile == "" || r.Commit == "" {
		errs = append(errs, errors.New("experiment, profile and commit are required"))
	}
	if r.Nodes < 1 {
		errs = append(errs, fmt.Errorf("nodes %d", r.Nodes))
	}
	for i, p := range r.Phases {
		if p.Name == "" || p.End.Before(p.Start) {
			errs = append(errs, fmt.Errorf("phase %d %q ends before it starts", i, p.Name))
		}
		if i > 0 && p.Start.Before(r.Phases[i-1].Start) {
			errs = append(errs, fmt.Errorf("phase %q starts before %q", p.Name, r.Phases[i-1].Name))
		}
	}
	for _, c := range r.Criteria {
		if c.Name == "" || (c.Verdict != VerdictPass && c.Verdict != VerdictFail) {
			errs = append(errs, fmt.Errorf("criterion %q has verdict %q", c.Name, c.Verdict))
		}
	}
	return errors.Join(errs...)
}

// Marshal returns the record as indented JSON with maps never null.
func (r *Record) Marshal() ([]byte, error) {
	out := *r
	if out.Phases == nil {
		out.Phases = []Phase{}
	}
	if out.Injections == nil {
		out.Injections = []Injection{}
	}
	if out.Attribution == nil {
		out.Attribution = map[string]int64{}
	}
	if out.Criteria == nil {
		out.Criteria = []Criterion{}
	}
	if out.Breakdowns == nil {
		out.Breakdowns = map[string]float64{}
	}
	for k, v := range out.Breakdowns {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("chaos: breakdown %s is %v", k, v)
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("chaos: record %s: %w", r.Experiment, err)
	}
	return b.Bytes(), nil
}

// Write writes the record into dir as FileName, creating dir.
func (r *Record) Write(dir string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", fmt.Errorf("chaos: record %s: %w", r.Experiment, err)
	}
	b, err := r.Marshal()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("chaos: records: %w", err)
	}
	path := filepath.Join(dir, r.FileName())
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", fmt.Errorf("chaos: records: %w", err)
	}
	return path, nil
}

// IntervalLog is one load stream's HdrHistogram V2 interval log beside a
// record: its 1 s windows by intended send time, each tagged with the
// phase it starts in.
type IntervalLog struct {
	// Stream names the load stream, such as "http1".
	Stream string
	// Result is the stream's load generator result.
	Result *loadgen.Result
}

// WriteIntervalLogs writes each log into dir as
// "<record stem>.<stream>.hlog" and returns their paths.
func (r *Record) WriteIntervalLogs(dir string, logs []IntervalLog) ([]string, error) {
	var paths []string
	for _, l := range logs {
		if l.Result == nil || len(l.Result.Windows) == 0 {
			continue
		}
		var b bytes.Buffer
		lw, err := loadgen.NewLogWriter(&b, l.Result.Start,
			"ruralz chaos "+r.Experiment+" "+r.Profile+" stream "+l.Stream,
			"commit "+r.Commit)
		if err != nil {
			return paths, fmt.Errorf("chaos: interval log %s: %w", l.Stream, err)
		}
		for _, w := range l.Result.Windows {
			if w.Latency == nil || w.Latency.Count() == 0 {
				continue
			}
			if err := lw.Write(r.phaseAt(w.Start), w.Start, w.Start.Add(l.Result.Window), w.Latency); err != nil {
				return paths, fmt.Errorf("chaos: interval log %s: %w", l.Stream, err)
			}
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return paths, fmt.Errorf("chaos: records: %w", err)
		}
		path := filepath.Join(dir, fileStem(r.Experiment)+"."+fileStem(l.Stream)+".hlog")
		if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
			return paths, fmt.Errorf("chaos: records: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// phaseAt returns the name of the phase holding t, "warmup" before the
// first and the last phase's name after it; an interval log tag carries
// no space.
func (r *Record) phaseAt(t time.Time) string {
	name := "warmup"
	for _, p := range r.Phases {
		if t.Before(p.Start) {
			break
		}
		name = p.Name
	}
	return strings.ReplaceAll(name, " ", "-")
}

// PhaseNamed returns the phase named name.
func (r *Record) PhaseNamed(name string) (Phase, bool) {
	i := slices.IndexFunc(r.Phases, func(p Phase) bool { return p.Name == name })
	if i < 0 {
		return Phase{}, false
	}
	return r.Phases[i], true
}
