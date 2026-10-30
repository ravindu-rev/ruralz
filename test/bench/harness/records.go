// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Recorder writes one job's records under its directory: one directory
// per scenario, holding each run's record with its raw histogram logs and
// CPU profiles, and each summary record. Paths inside records are
// relative to the record's directory.
type Recorder struct {
	Dir string
}

// fileName matches the names the Recorder writes.
func fileName() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) }

// scenarioDir returns the directory of a scenario's records, creating it.
func (r *Recorder) scenarioDir(scenario string) (string, error) {
	if !fileName().MatchString(scenario) {
		return "", fmt.Errorf("harness: scenario %q is not a file name", scenario)
	}
	dir := filepath.Join(r.Dir, scenario)
	return dir, os.MkdirAll(dir, 0o750)
}

// WriteFile writes a supporting file (a histogram log, a CPU profile) of a
// scenario and returns its name relative to the scenario's records.
func (r *Recorder) WriteFile(scenario, name string, data []byte) (string, error) {
	if !fileName().MatchString(name) {
		return "", fmt.Errorf("harness: %q is not a file name", name)
	}
	dir, err := r.scenarioDir(scenario)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// Write validates rec and writes it as <name>.json in its scenario's
// directory; it returns the record's name relative to that directory. Every
// file the record names must already exist beside it.
func (r *Recorder) Write(name string, rec *benchrecord.Record) (string, error) {
	if err := rec.Validate(); err != nil {
		return "", err
	}
	dir, err := r.scenarioDir(rec.Identity.Scenario)
	if err != nil {
		return "", err
	}
	files := append(append(append([]string{}, rec.Latency.HistogramFiles...), rec.Resources.CPUProfileFiles...), summaryRuns(rec)...)
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			return "", fmt.Errorf("harness: record %s names %s: %w", name, f, err)
		}
	}
	file := name + ".json"
	if !fileName().MatchString(file) {
		return "", fmt.Errorf("harness: %q is not a file name", file)
	}
	if err := benchrecord.WriteFile(filepath.Join(dir, file), rec); err != nil {
		return "", err
	}
	return file, nil
}

func summaryRuns(rec *benchrecord.Record) []string {
	if rec.Summary == nil {
		return nil
	}
	return rec.Summary.Runs
}

// ReadRecord reads a record the Recorder wrote, scenario-relative.
func (r *Recorder) ReadRecord(scenario, name string) (*benchrecord.Record, error) {
	return benchrecord.ReadFile(filepath.Join(r.Dir, scenario, filepath.FromSlash(path.Clean(name))))
}

// intervalLog writes h as a one-interval HdrHistogram V2 log over
// [start, end).
func intervalLog(h *loadgen.Histogram, start, end time.Time, comment string) ([]byte, error) {
	var b bytes.Buffer
	lw, err := loadgen.NewLogWriter(&b, start, comment)
	if err != nil {
		return nil, err
	}
	if h != nil && h.Count() > 0 {
		if err := lw.Write("", start, end, h); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// decodeLog decodes a base64 interval log from a LoadResult.
func decodeLog(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
