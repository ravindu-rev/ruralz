// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// The log records of a file-mode ruralzd's first boot attempt that the
// built ruralzd's runner reads (internal/gateway/reload and
// docs/architecture/03-data-plane.md "Activation"): a rejected load and
// the findings past the ten its record holds, an activation and the
// warnings of its load, and the records that end the attempt.
const (
	recRejected        = "revision rejected"
	recRejectedFinding = "revision rejected: finding"
	recActivated       = "revision activated"
	recWarnings        = "activated revision has warnings"
	recWarning         = "activated revision warning"
	// recNoRevision ends an attempt that activated nothing.
	recNoRevision = "no usable configuration: the Node stays not ready until a Revision activates"
	// recLKGWritten and recLKGFailed end an attempt that activated a
	// Revision: its candidate write follows the activation's records.
	recLKGWritten = "Last-Known-Good written"
	recLKGFailed  = "Last-Known-Good write failed"
)

// bootEnded returns the pattern of the record that ends the first boot
// attempt, a whole line: every record of the attempt is written before it.
func bootEnded() string {
	return `"msg":"(?:` + recNoRevision + `|` + recLKGWritten + `|` + recLKGFailed + `)"[^\n]*\n`
}

// nodeRecord is the part of a log record the runner reads.
type nodeRecord struct {
	Msg         string           `json:"msg"`
	Digest      string           `json:"digest"`
	Diagnostics int              `json:"diagnostics"`
	Findings    diag.List        `json:"findings"`
	Finding     *diag.Diagnostic `json:"finding"`
}

// errNoBootEnd reports output without the record that ends the first boot
// attempt.
var errNoBootEnd = errors.New("ruralzd's output does not end its first boot attempt")

// nodeOutcome returns the outcome of a file-mode ruralzd's first boot
// attempt from its standard output, the JSON log lines up to the record
// that ends the attempt (bootEnded):
//
//   - a load rejected before the digest: every diagnostic of the
//     configuration pipeline, from the rejection's records, no digest;
//   - a load rejected after the digest: the Revision's digest, the
//     pipeline's warnings, which the rejection's records hold beside the
//     failing step's findings, and those findings, by their codes
//     (postDigestCodes), as after;
//   - an activation: the Revision's digest and the pipeline's warnings,
//     from the activation-warning records.
//
// The records hold each diagnostic in its JSON form, which reads back
// losslessly (diag.Diagnostic.UnmarshalJSON); records that hold fewer
// diagnostics than they count are an error.
func nodeOutcome(stdout []byte) (outcome, error) {
	var (
		head        string
		digest      string
		want        int
		found       diag.List
		ended       bool
		recordsSeen []string
	)
	for line := range bytes.SplitSeq(stdout, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec nodeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return outcome{}, fmt.Errorf("ruralzd log line %q: %w", line, err)
		}
		recordsSeen = append(recordsSeen, rec.Msg)
		switch rec.Msg {
		case recRejected, recActivated:
			if head != "" {
				return outcome{}, fmt.Errorf("ruralzd logs %q after %q in one boot attempt", rec.Msg, head)
			}
			head, digest = rec.Msg, rec.Digest
			if rec.Msg == recRejected {
				want, found = rec.Diagnostics, append(found, rec.Findings...)
			}
		case recWarnings:
			if head != recActivated {
				return outcome{}, fmt.Errorf("ruralzd logs %q without %q", rec.Msg, recActivated)
			}
			want, found = rec.Diagnostics, append(found, rec.Findings...)
		case recRejectedFinding, recWarning:
			if rec.Finding == nil {
				return outcome{}, fmt.Errorf("ruralzd logs %q without its finding", rec.Msg)
			}
			found = append(found, *rec.Finding)
		case recNoRevision, recLKGWritten, recLKGFailed:
			ended = true
		}
		if ended {
			break
		}
	}
	switch {
	case !ended:
		return outcome{}, fmt.Errorf("%w: %q", errNoBootEnd, recordsSeen)
	case head == "":
		return outcome{}, fmt.Errorf("ruralzd neither rejected nor activated a Revision: %q", recordsSeen)
	case len(found) != want:
		return outcome{}, fmt.Errorf("ruralzd's records carry %d of the %d diagnostics they count", len(found), want)
	}
	var ds, after diag.List
	for _, d := range found {
		if digest != "" && slices.Contains(postDigestCodes(), d.Code) {
			after = append(after, d)
		} else {
			ds = append(ds, d)
		}
	}
	if head == recRejected && digest != "" && len(after) == 0 {
		return outcome{}, fmt.Errorf("ruralzd rejects the Revision %s without a finding after the digest:\n%s", digest, texts(found))
	}
	o, err := outcomeOf(ds, digest)
	for _, d := range after {
		o.after = append(o.after, string(d.AppendText(nil)))
	}
	return o, err
}

// TestNodeOutcome checks the reading of ruralzd's records against the
// outcomes they stand for.
func TestNodeOutcome(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc := func(v any) string {
		var buf bytes.Buffer
		e := json.NewEncoder(&buf)
		e.SetEscapeHTML(false)
		if err := e.Encode(v); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	rec := func(msg string, members map[string]any) string {
		m := map[string]any{"time": "2026-01-01T00:00:00Z", "level": "WARN", "msg": msg}
		for k, v := range members {
			m[k] = v
		}
		return enc(m)
	}
	warning := diag.Diagnostic{Code: "RZ-CFG-013", Severity: diag.SeverityWarning, Location: diag.Location{File: "ruralz.yaml", Line: 9, Column: 3}, Message: "a secret-like variable"}
	served := diag.Diagnostic{Code: "RZ-CFG-040", Severity: diag.SeverityError, Location: diag.Location{File: "plugins/a.yaml", Line: 2, Column: 1}, Message: "kind Plugin is not served"}
	var errs diag.List
	for i := range 12 {
		errs = append(errs, diag.Diagnostic{Code: "RZ-CFG-009", Severity: diag.SeverityError, Location: diag.Location{File: "ruralz.yaml", Line: i + 1, Column: 5}, Message: fmt.Sprint("missing ", i)})
	}
	start, end := rec("ruralzd starting", nil), rec(recNoRevision, nil)
	debugFinding := func(msg string, d diag.Diagnostic) string { return rec(msg, map[string]any{"finding": d}) }

	tests := []struct {
		name   string
		out    string
		want   diag.List
		digest string
		after  []string
		err    string
	}{
		{
			name: "rejected before the digest, past ten findings",
			out: start + rec(recRejected, map[string]any{"code": "RZ-CFG-009", "diagnostics": 12, "findings": errs[:10]}) +
				debugFinding(recRejectedFinding, errs[10]) + debugFinding(recRejectedFinding, errs[11]) + rec("no Last-Known-Good to boot", nil) + end,
			want: errs,
		},
		{
			name: "rejected after the digest, with a warning",
			out: start + rec(recRejected, map[string]any{"code": "RZ-CFG-040", "diagnostics": 2, "digest": digest, "findings": diag.List{served, warning}}) +
				end + "a partial line after the end",
			want: diag.List{warning}, digest: digest, after: []string{string(served.AppendText(nil))},
		},
		{
			name: "activated with warnings",
			out: start + rec(recActivated, map[string]any{"digest": digest}) + rec(recWarnings, map[string]any{"diagnostics": 2, "findings": diag.List{warning}}) +
				debugFinding(recWarning, warning) + rec(recLKGWritten, nil),
			want: diag.List{warning, warning}, digest: digest,
		},
		{
			name:   "activated without warnings",
			out:    start + rec(recActivated, map[string]any{"digest": digest}) + rec(recLKGFailed, nil),
			digest: digest,
		},
		{
			name: "fewer findings than counted",
			out:  start + rec(recRejected, map[string]any{"diagnostics": 12, "findings": errs[:10]}) + end,
			err:  "carry 10 of the 12",
		},
		{name: "no end", out: start + rec(recActivated, map[string]any{"digest": digest}), err: errNoBootEnd.Error()},
		{name: "no outcome", out: start + rec("configuration source cannot be read", nil) + end, err: "neither rejected nor activated"},
		{name: "not a record", out: "panic: boom\n" + end, err: "ruralzd log line"},
		{
			name: "rejected after the digest without a finding past it",
			out:  start + rec(recRejected, map[string]any{"diagnostics": 1, "digest": digest, "findings": diag.List{warning}}) + end,
			err:  "without a finding after the digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := nodeOutcome([]byte(tt.out))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("nodeOutcome = %v, want an error with %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := outcomeOf(tt.want, tt.digest)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(o.text, want.text) || !bytes.Equal(o.json, want.json) || o.digest != tt.digest || !slices.Equal(o.after, tt.after) {
				t.Errorf("nodeOutcome = %q %q after %q, want %q %q after %q", o.text, o.digest, o.after, want.text, tt.digest, tt.after)
			}
		})
	}
}
