// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// benchFormat is the format of benchmark result records
// (docs/architecture/12-performance-budgets-and-benchmarking.md, "Result
// record").
const benchFormat = "ruralz.bench.v1"

// maxRecordBytes bounds one record, as internal/testkit/benchrecord's
// MaxRecordBytes does for its own reader.
const maxRecordBytes = 16 << 20

// stateFile is the rotation state the Latency job writes beside its
// records for the bench-records branch; it is no record and never ships.
const stateFile = "state.json"

// benchBudget is one budget result of a record.
type benchBudget struct {
	ID      string `json:"id"`
	Value   string `json:"value"`
	Result  string `json:"result"`
	SLO     string `json:"slo"`
	Verdict string `json:"verdict"`
}

// Record kinds: every run writes a macro or component record, and every
// scenario's comparison, rate ladder or soak adds a summary record, whose
// budgets hold the pooled results the notes list.
const (
	kindMacro     = "macro"
	kindComponent = "component"
	kindSummary   = "summary"
)

// benchSummary is the part of a summary record's summary the notes show.
type benchSummary struct {
	// Of is comparison, ladder or soak.
	Of string `json:"of"`
	// Verdict is the A/B verdict: pass, fail, inconclusive, or none
	// without a baseline.
	Verdict string `json:"verdict"`
}

// benchRecord is the part of a ruralz.bench.v1 record the notes show.
type benchRecord struct {
	Format   string `json:"format"`
	Identity struct {
		Scenario  string `json:"scenario"`
		Kind      string `json:"kind"`
		Commit    string `json:"commit"`
		Version   string `json:"version"`
		Baseline  string `json:"baseline"`
		Toolchain string `json:"toolchain"`
	} `json:"identity"`
	Environment struct {
		HardwareProfile string `json:"hardwareProfile"`
	} `json:"environment"`
	Validity struct {
		Verdict string `json:"verdict"`
	} `json:"validity"`
	Budgets []benchBudget `json:"budgets"`
	Summary *benchSummary `json:"summary"`
	// file is the record's path relative to the records directory.
	file string
}

// readBudgets reads every record (*.json) under dir and returns the
// summary records, sorted by scenario, kind and file: the notes list each
// budget's pooled result from them, never a single run's. A missing
// directory holds no record; any other JSON file there, a record of
// another kind and a summary record without its summary are errors, so a
// result never goes missing silently.
func readBudgets(dir string) ([]benchRecord, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []benchRecord
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
			return err
		}
		f, err := os.Open(p) //nolint:gosec // A record under the records directory given on the command line.
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
		_ = f.Close()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if len(data) > maxRecordBytes {
			return fmt.Errorf("%s exceeds %d bytes", rel, maxRecordBytes)
		}
		var r benchRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if r.Format != benchFormat {
			if filepath.Base(rel) == stateFile {
				return nil
			}
			return fmt.Errorf("%s: format %q, want %q", rel, r.Format, benchFormat)
		}
		switch r.Identity.Kind {
		case kindMacro, kindComponent:
			return nil
		case kindSummary:
			if r.Summary == nil {
				return fmt.Errorf("%s: a summary record without its summary", rel)
			}
		default:
			return fmt.Errorf("%s: kind %q, want %s, %s or %s", rel, r.Identity.Kind, kindMacro, kindComponent, kindSummary)
		}
		r.file = filepath.ToSlash(rel)
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("benchmark records: %w", err)
	}
	slices.SortFunc(out, func(a, b benchRecord) int {
		if c := strings.Compare(a.Identity.Scenario, b.Identity.Scenario); c != 0 {
			return c
		}
		if c := strings.Compare(a.Identity.Kind, b.Identity.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.file, b.file)
	})
	return out, nil
}

// skippedFormat identifies the skipped-gates input of the notes, which
// internal/tool/releasegate writes from the release gates' outcome.
const skippedFormat = "ruralz.skippedgates.v1"

// skippedGate is one release gate a maintainer skipped.
type skippedGate struct {
	// Gate is a row of the testing document's Release gates table.
	Gate string `json:"gate"`
	// By is the maintainer's GitHub login; Role is admin or maintain.
	By   string `json:"by"`
	Role string `json:"role"`
	// Reason is the release-notes entry the skip requires.
	Reason string `json:"reason"`
}

// skippedFile is the skipped-gates document.
type skippedFile struct {
	Format  string        `json:"format"`
	Skipped []skippedGate `json:"skipped"`
}

// releaseGateRows are the rows of the testing document's Release gates
// table that apply from M0 and M1, as gate and evidence in plain text (link
// targets and code marks dropped); a maintainer may skip a gate with a
// release-notes entry, and the notes print the skipped gate's row.
func releaseGateRows() [][2]string {
	return [][2]string{
		{"All CI stages green", "Every stage re-run on the candidate commit, with floor and release jobs"},
		{"Golden corpus", "Byte-exact, except entries recording a deliberate change"},
		{"Conformance", "100% of cases for shipped features (target); every PDK passes the Plugin ABI suite of every supported release line"},
		{"Fuzzing", "Zero open crashers; every fixed crasher has a regression seed"},
		{"Chaos", "Every chaos scenario, CE experiment, game day drill and failure catalog row shipped by the candidate's milestone passed on the candidate commit"},
		{"Benchmarks", "Stage 9's alloc/op, size and idle RSS gates; from M6, within 5% p99 and 3% alloc/op of the previous release (target), " +
			"or absolute budgets alone without a previous release, every seed budget met from the milestone introducing its component, " +
			"read from the release run's summary records, and the 2-hour S2 soak at half saturation under the soak rule: " +
			"RSS drift 2% or less and flat goroutines (target), S2's validity rules, zero telemetry drops, PB-2 and PB-3"},
		{"Security scanning", "Every check in Security scanning green"},
		{"Release candidate", "For vX.Y.0, a vX.Y.0-rc.N tag on the same commit whose release run passed, published at least 14 days earlier (target); " +
			"a patch vX.Y.Z needs no candidate (Release cadence)"},
	}
}

// releaseGates are the gates of releaseGateRows.
func releaseGates() []string {
	var out []string
	for _, r := range releaseGateRows() {
		out = append(out, r[0])
	}
	return out
}

// readSkipped reads and validates the skipped-gates document.
func readSkipped(path string) ([]skippedGate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The skipped-gates file given on the command line.
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f skippedFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: data after the document", path)
	}
	if f.Format != skippedFormat {
		return nil, fmt.Errorf("%s: format %q, want %q", path, f.Format, skippedFormat)
	}
	login := regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	var seen []string
	for i, g := range f.Skipped {
		switch {
		case !slices.Contains(releaseGates(), g.Gate):
			return nil, fmt.Errorf("%s: skipped[%d]: gate %q is not one of %s", path, i, g.Gate, strings.Join(releaseGates(), ", "))
		case slices.Contains(seen, g.Gate):
			return nil, fmt.Errorf("%s: skipped[%d]: gate %q is listed twice", path, i, g.Gate)
		case !login.MatchString(g.By) || strings.Contains(g.By, "--"):
			return nil, fmt.Errorf("%s: skipped[%d]: %q is not a GitHub login", path, i, g.By)
		case g.Role != "admin" && g.Role != "maintain":
			return nil, fmt.Errorf("%s: skipped[%d]: role %q; only a maintainer (admin or maintain) may skip a gate", path, i, g.Role)
		case strings.TrimSpace(g.Reason) == "":
			return nil, fmt.Errorf("%s: skipped[%d]: a skip needs a reason", path, i)
		}
		seen = append(seen, g.Gate)
	}
	return f.Skipped, nil
}
