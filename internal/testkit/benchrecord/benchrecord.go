// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package benchrecord defines the ruralz.bench.v1 result record every macro
// and component benchmark run writes (11 req 77; PBB "Result record"), with
// a JSON writer, a strict reader and the schema check.
//
// The record carries Identity (scenario, commit, baseline, Revision digest,
// toolchain), Environment (hardware profile and microarchitecture, kernel,
// cpuset, GOMAXPROCS, GOGC, GOMEMLIMIT, sysctls, NIC settings), Load (tool,
// version, command line, offered and achieved rate, connections, key
// cardinality and distribution, injected delay distribution), Latency
// (external and internal percentiles, raw histogram logs), Resources (CPU
// per request, GC CPU share, RSS at load and idle settle, live heap,
// alloc/op) and Validity and budgets (rate error, generator and mock CPU,
// drops, noise, verdict; each budget's value, result, SLO and verdict).
//
// Durations are integer nanoseconds in JSON (members ending in "Ns").
// Schema returns the JSON Schema (draft 2020-12) of the same format, which
// the benchmark harness publishes as test/bench/record.schema.json. Decode
// applies every rule that schema states (member presence and the absence
// of null, which Go's decoder cannot see, then Validate for every value
// rule), so records are checked without a JSON Schema library. Relative
// file paths follow the same rule on every platform: slash-separated, no
// leading "/", no backslash and no ".." element.
package benchrecord

import (
	"bytes"
	_ "embed" // the published JSON Schema
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Format is the record format identifier.
const Format = "ruralz.bench.v1"

// Record kinds.
const (
	KindMacro     = "macro"
	KindComponent = "component"
)

// Run verdicts (Validity.Verdict).
const (
	VerdictValid        = "valid"
	VerdictInvalid      = "invalid"
	VerdictInconclusive = "inconclusive"
)

// Budget verdicts (BudgetResult.Verdict).
const (
	BudgetPass    = "pass"
	BudgetFail    = "fail"
	BudgetSkipped = "skipped"
)

// MaxRecordBytes bounds a record read by Decode.
const MaxRecordBytes = 16 << 20

//go:embed record.schema.json
var schema []byte

// Schema returns a copy of the record's JSON Schema document.
func Schema() []byte { return bytes.Clone(schema) }

// Record is one benchmark run's result.
type Record struct {
	// Format is always "ruralz.bench.v1".
	Format      string         `json:"format"`
	Identity    Identity       `json:"identity"`
	Environment Environment    `json:"environment"`
	Load        Load           `json:"load"`
	Latency     Latency        `json:"latency"`
	Resources   Resources      `json:"resources"`
	Validity    Validity       `json:"validity"`
	Budgets     []BudgetResult `json:"budgets"`
}

// Identity says what ran.
type Identity struct {
	// Scenario is the PBB scenario (S1, S2, O1a...) or the component
	// benchmark name.
	Scenario string `json:"scenario"`
	// Kind is "macro" or "component".
	Kind string `json:"kind"`
	// Commit is the full commit under test.
	Commit string `json:"commit"`
	// Version is the Ruralz product version of the binary under test.
	Version string `json:"version"`
	// Baseline names the baseline run (commit or release); empty when the
	// run has none.
	Baseline string `json:"baseline"`
	// RevisionDigest is the scenario Bundle's Revision digest
	// ("sha256:<64 hex>"); empty for component runs without one.
	RevisionDigest string `json:"revisionDigest"`
	// Toolchain is the Go toolchain, for example "go1.27.1".
	Toolchain string `json:"toolchain"`
	// StartedAt is when the measured run started (UTC).
	StartedAt time.Time `json:"startedAt"`
}

// Environment fingerprints the host.
type Environment struct {
	HardwareProfile   string `json:"hardwareProfile"`
	Microarchitecture string `json:"microarchitecture"`
	Kernel            string `json:"kernel"`
	Cpuset            string `json:"cpuset"`
	NIC               string `json:"nic"`
	GOMAXPROCS        int    `json:"gomaxprocs"`
	GOGC              string `json:"gogc"`
	GOMEMLIMIT        string `json:"gomemlimit"`
	// Sysctls holds the recorded kernel settings, including
	// net.ipv4.ip_local_port_range.
	Sysctls map[string]string `json:"sysctls"`
}

// Load describes the offered load.
type Load struct {
	Tool            string  `json:"tool"`
	Version         string  `json:"version"`
	CommandLine     string  `json:"commandLine"`
	OfferedRate     float64 `json:"offeredRate"`
	AchievedRate    float64 `json:"achievedRate"`
	Connections     int     `json:"connections"`
	KeyCardinality  int     `json:"keyCardinality"`
	KeyDistribution string  `json:"keyDistribution"`
	InjectedDelay   string  `json:"injectedDelay"`
}

// Percentiles are latency percentiles; zero means not measured.
type Percentiles struct {
	P50  time.Duration `json:"p50Ns"`
	P90  time.Duration `json:"p90Ns"`
	P99  time.Duration `json:"p99Ns"`
	P999 time.Duration `json:"p999Ns"`
	Max  time.Duration `json:"maxNs"`
}

// Latency holds the external (load generator, from intended send time) and
// internal (Node metrics, access log) views.
type Latency struct {
	External Percentiles `json:"external"`
	Internal Percentiles `json:"internal"`
	// HistogramFiles are the raw histogram logs, relative to the record.
	HistogramFiles []string `json:"histogramFiles"`
}

// Resources holds the resource measurements.
type Resources struct {
	CPUPerRequest time.Duration `json:"cpuPerRequestNs"`
	// GCCPUShare is the share of Node CPU spent in GC, 0 to 1.
	GCCPUShare    float64 `json:"gcCpuShare"`
	RSSLoadBytes  int64   `json:"rssLoadBytes"`
	RSSIdleBytes  int64   `json:"rssIdleBytes"`
	LiveHeapBytes int64   `json:"liveHeapBytes"`
	AllocsPerOp   float64 `json:"allocsPerOp"`
	// CPUProfileFiles are the CPU profiles published with the record
	// (never heap profiles), relative to the record.
	CPUProfileFiles []string `json:"cpuProfileFiles"`
}

// Validity holds the run's validity checks and verdict.
type Validity struct {
	// RateError is |achieved - offered| / offered.
	RateError float64 `json:"rateError"`
	// GeneratorCPU and MockCPU are the CPU shares (0 to 1 of their cpuset)
	// of the load generator and the mock Upstream.
	GeneratorCPU float64 `json:"generatorCpu"`
	MockCPU      float64 `json:"mockCpu"`
	// NoiseCV is the baseline p99 coefficient of variation.
	NoiseCV float64 `json:"noiseCv"`
	// Drops counts dropped telemetry records during the run.
	Drops int64 `json:"drops"`
	// Verdict is "valid", "invalid" or "inconclusive".
	Verdict string `json:"verdict"`
}

// BudgetResult is one budget checked by the run.
type BudgetResult struct {
	// ID names the budget, for example "PB-2".
	ID string `json:"id"`
	// Value is the budget as stated, for example "<= 1ms (target)".
	Value string `json:"value"`
	// Result is the measured value, for example "0.84ms".
	Result string `json:"result"`
	// SLO names the linked SLO, for example "SLO-GW-2"; empty when none.
	SLO string `json:"slo"`
	// Verdict is "pass", "fail" or "skipped".
	Verdict string `json:"verdict"`
}

// isLowerHex reports whether s is made of lowercase hex digits only.
func isLowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; !isDigit(c) && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isCommit matches the schema pattern ^[0-9a-f]{7,64}$.
func isCommit(s string) bool { return len(s) >= 7 && len(s) <= 64 && isLowerHex(s) }

// isDigest matches the schema pattern ^sha256:[0-9a-f]{64}$.
func isDigest(s string) bool {
	hex, ok := strings.CutPrefix(s, "sha256:")
	return ok && len(hex) == 64 && isLowerHex(hex)
}

// isBudgetID matches the schema pattern ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$.
func isBudgetID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		alnum := isDigit(c) || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return false
		}
	}
	return true
}

// Validate checks every schema rule and returns all violations joined, each
// naming its JSON path.
func (r *Record) Validate() error {
	var errs []error
	bad := func(path, format string, args ...any) {
		errs = append(errs, fmt.Errorf("benchrecord: %s: %s", path, fmt.Sprintf(format, args...)))
	}
	if r.Format != Format {
		bad("format", "want %q, got %q", Format, r.Format)
	}
	id := r.Identity
	if id.Scenario == "" {
		bad("identity.scenario", "required")
	}
	if id.Kind != KindMacro && id.Kind != KindComponent {
		bad("identity.kind", "want %q or %q, got %q", KindMacro, KindComponent, id.Kind)
	}
	if !isCommit(id.Commit) {
		bad("identity.commit", "want 7 to 64 lowercase hex digits, got %q", id.Commit)
	}
	if id.Version == "" {
		bad("identity.version", "required")
	}
	if id.RevisionDigest != "" && !isDigest(id.RevisionDigest) {
		bad("identity.revisionDigest", "want sha256:<64 hex>, got %q", id.RevisionDigest)
	}
	if id.Kind == KindMacro && id.RevisionDigest == "" {
		bad("identity.revisionDigest", "required for a macro run")
	}
	if id.Toolchain == "" {
		bad("identity.toolchain", "required")
	}
	if id.StartedAt.IsZero() {
		bad("identity.startedAt", "required")
	}
	env := r.Environment
	if env.GOMAXPROCS < 1 {
		bad("environment.gomaxprocs", "want at least 1, got %d", env.GOMAXPROCS)
	}
	if env.GOGC == "" {
		bad("environment.gogc", "required")
	}
	if env.Kernel == "" {
		bad("environment.kernel", "required")
	}
	ld := r.Load
	if id.Kind == KindMacro {
		if ld.Tool == "" {
			bad("load.tool", "required for a macro run")
		}
		if !(ld.OfferedRate > 0) {
			bad("load.offeredRate", "want > 0 for a macro run, got %v", ld.OfferedRate)
		}
		if ld.Connections < 1 {
			bad("load.connections", "want at least 1 for a macro run, got %d", ld.Connections)
		}
	}
	nonNegFloat(bad, "load.offeredRate", ld.OfferedRate)
	nonNegFloat(bad, "load.achievedRate", ld.AchievedRate)
	if ld.Connections < 0 {
		bad("load.connections", "negative")
	}
	if ld.KeyCardinality < 0 {
		bad("load.keyCardinality", "negative")
	}
	checkPercentiles(bad, "latency.external", r.Latency.External)
	checkPercentiles(bad, "latency.internal", r.Latency.Internal)
	checkFiles(bad, "latency.histogramFiles", r.Latency.HistogramFiles)
	res := r.Resources
	if res.CPUPerRequest < 0 {
		bad("resources.cpuPerRequestNs", "negative")
	}
	if !(res.GCCPUShare >= 0 && res.GCCPUShare <= 1) {
		bad("resources.gcCpuShare", "want 0 to 1, got %v", res.GCCPUShare)
	}
	for path, v := range map[string]int64{
		"resources.rssLoadBytes": res.RSSLoadBytes, "resources.rssIdleBytes": res.RSSIdleBytes, "resources.liveHeapBytes": res.LiveHeapBytes,
	} {
		if v < 0 {
			bad(path, "negative")
		}
	}
	nonNegFloat(bad, "resources.allocsPerOp", res.AllocsPerOp)
	checkFiles(bad, "resources.cpuProfileFiles", res.CPUProfileFiles)
	v := r.Validity
	nonNegFloat(bad, "validity.rateError", v.RateError)
	nonNegFloat(bad, "validity.noiseCv", v.NoiseCV)
	for path, x := range map[string]float64{"validity.generatorCpu": v.GeneratorCPU, "validity.mockCpu": v.MockCPU} {
		if !(x >= 0 && x <= 1) {
			bad(path, "want 0 to 1, got %v", x)
		}
	}
	if v.Drops < 0 {
		bad("validity.drops", "negative")
	}
	switch v.Verdict {
	case VerdictValid, VerdictInvalid, VerdictInconclusive:
	default:
		bad("validity.verdict", "want valid, invalid or inconclusive, got %q", v.Verdict)
	}
	seen := map[string]bool{}
	for i, b := range r.Budgets {
		path := fmt.Sprintf("budgets[%d]", i)
		if !isBudgetID(b.ID) {
			bad(path+".id", "want an identifier such as PB-2, got %q", b.ID)
		} else if seen[b.ID] {
			bad(path+".id", "duplicate %q", b.ID)
		}
		seen[b.ID] = true
		if b.Value == "" {
			bad(path+".value", "required")
		}
		switch b.Verdict {
		case BudgetPass, BudgetFail:
			if b.Result == "" {
				bad(path+".result", "required when the verdict is %q", b.Verdict)
			}
		case BudgetSkipped:
		default:
			bad(path+".verdict", "want pass, fail or skipped, got %q", b.Verdict)
		}
	}
	return errors.Join(errs...)
}

func nonNegFloat(bad func(string, string, ...any), path string, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		bad(path, "want a finite value >= 0, got %v", v)
	}
}

func checkPercentiles(bad func(string, string, ...any), path string, p Percentiles) {
	vals := []struct {
		name string
		v    time.Duration
	}{{"p50Ns", p.P50}, {"p90Ns", p.P90}, {"p99Ns", p.P99}, {"p999Ns", p.P999}, {"maxNs", p.Max}}
	prev := time.Duration(0)
	for _, x := range vals {
		switch {
		case x.v < 0:
			bad(path+"."+x.name, "negative")
		case x.v != 0 && x.v < prev:
			bad(path+"."+x.name, "below a lower percentile (%v < %v)", x.v, prev)
		}
		if x.v > prev {
			prev = x.v
		}
	}
}

func checkFiles(bad func(string, string, ...any), path string, files []string) {
	for i, f := range files {
		if !isRelativePath(f) {
			bad(fmt.Sprintf("%s[%d]", path, i), "want a slash-separated relative path inside the record directory, got %q", f)
		}
	}
}

// isRelativePath implements the schema's relativePath rule the same way on
// every platform: not empty, no leading "/", no backslash and no ".."
// element.
func isRelativePath(f string) bool {
	if f == "" || strings.HasPrefix(f, "/") || strings.Contains(f, `\`) {
		return false
	}
	for elem := range strings.SplitSeq(f, "/") {
		if elem == ".." {
			return false
		}
	}
	return true
}

// Write validates r and writes it as indented JSON with a trailing newline.
// Nil slices and maps are written as empty arrays and objects.
func Write(w io.Writer, r *Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	out := *r
	if out.Budgets == nil {
		out.Budgets = []BudgetResult{}
	}
	if out.Environment.Sysctls == nil {
		out.Environment.Sysctls = map[string]string{}
	}
	if out.Latency.HistogramFiles == nil {
		out.Latency.HistogramFiles = []string{}
	}
	if out.Resources.CPUProfileFiles == nil {
		out.Resources.CPUProfileFiles = []string{}
	}
	out.Identity.StartedAt = out.Identity.StartedAt.UTC()
	b, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("benchrecord: encode: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("benchrecord: write: %w", err)
	}
	return nil
}

// WriteFile writes r to path atomically (a temporary file renamed into
// place).
func WriteFile(path string, r *Record) error {
	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".record-*.json")
	if err != nil {
		return fmt.Errorf("benchrecord: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("benchrecord: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("benchrecord: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("benchrecord: %w", err)
	}
	return nil
}

// Decode reads one record strictly: unknown members, trailing data, a
// missing member and any Validate violation are errors.
func Decode(r io.Reader) (*Record, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("benchrecord: read: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("benchrecord: record exceeds %d bytes", MaxRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("benchrecord: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("benchrecord: decode: trailing data after the record")
	}
	if err := checkRequired(data); err != nil {
		return nil, err
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ReadFile decodes the record at path.
func ReadFile(path string) (*Record, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the caller names the record to read
	if err != nil {
		return nil, fmt.Errorf("benchrecord: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Decode(f)
}

// checkRequired verifies that every member the schema requires is present
// and that no value is null (Go's decoder would otherwise read a missing
// member or a null as its zero value, which the schema rejects: no type in
// it admits null).
func checkRequired(data []byte) error {
	var s schemaNode
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("benchrecord: embedded schema: %w", err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("benchrecord: decode: %w", err)
	}
	var errs []error
	s.required("", doc, &s, &errs)
	return errors.Join(errs...)
}

// schemaNode is the subset of JSON Schema the record schema uses.
type schemaNode struct {
	Type       any                    `json:"type"`
	Required   []string               `json:"required"`
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Ref        string                 `json:"$ref"`
	Defs       map[string]*schemaNode `json:"$defs"`
}

// resolve follows a local "#/$defs/<name>" reference.
func (n *schemaNode) resolve(root *schemaNode) *schemaNode {
	if n.Ref == "" {
		return n
	}
	name, ok := strings.CutPrefix(n.Ref, "#/$defs/")
	if !ok || root.Defs[name] == nil {
		return n
	}
	return root.Defs[name]
}

// required walks doc along the schema and reports missing members and
// null values. A node without a schema (a sysctls value) is checked for
// null only.
func (n *schemaNode) required(path string, doc any, root *schemaNode, errs *[]error) {
	if doc == nil {
		*errs = append(*errs, fmt.Errorf("benchrecord: %s: null is not allowed", orRoot(path)))
		return
	}
	if n != nil {
		n = n.resolve(root)
	}
	switch v := doc.(type) {
	case map[string]any:
		if n != nil {
			for _, name := range n.Required {
				if _, ok := v[name]; !ok {
					*errs = append(*errs, fmt.Errorf("benchrecord: %s: required member missing", join(path, name)))
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(v)) {
			var child *schemaNode
			if n != nil {
				child = n.Properties[name]
			}
			child.required(join(path, name), v[name], root, errs)
		}
	case []any:
		var items *schemaNode
		if n != nil {
			items = n.Items
		}
		for i, c := range v {
			items.required(fmt.Sprintf("%s[%d]", path, i), c, root, errs)
		}
	}
}

func orRoot(path string) string {
	if path == "" {
		return "$"
	}
	return path
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
