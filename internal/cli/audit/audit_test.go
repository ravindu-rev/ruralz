// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// sample is a finding with every member set.
func sample() Finding {
	return Finding{
		Rule: RuleCleartextHop, Severity: diag.SeverityWarning,
		Location: diag.Location{File: "ruralz.yaml", Line: 9, Column: 17},
		Resource: &diag.ResourceID{Kind: "Gateway", Name: "edge"},
		Path:     diag.Path{diag.Field("spec"), diag.Field("listeners"), diag.Keyed("name", "http"), diag.Field("protocol")},
		Message:  `listener "http" serves cleartext HTTP`, Hint: "serve https",
		Related:     []diag.Related{{Location: diag.Location{File: "routes/a.yaml", Line: 2, Column: 3}, Message: "used by"}},
		Environment: "prod",
	}
}

// TestFindingText: the text form is the diagnostic text form with the
// rule in the code position, hint, related locations and Environment
// included; a finding without location, resource or path keeps the form.
func TestFindingText(t *testing.T) {
	tests := []struct {
		name string
		f    Finding
		want string
	}{
		{
			name: "every member",
			f:    sample(),
			want: `ruralz.yaml:9:17 warning cleartext-hop Gateway/edge spec.listeners[name=http].protocol: listener "http" serves cleartext HTTP` +
				` (serve https) (used by routes/a.yaml:2:3) [environment=prod]`,
		},
		{
			name: "a resource-level finding",
			f: Finding{
				Rule: RuleRouteWithoutAuth, Severity: diag.SeverityWarning, Location: diag.Location{File: "routes/orders.yaml", Line: 1, Column: 1},
				Resource: &diag.ResourceID{Kind: "Route", Name: "orders"}, Message: `Route "orders" runs no authentication Policy`,
			},
			want: `routes/orders.yaml:1:1 warning route-without-auth Route/orders: Route "orders" runs no authentication Policy`,
		},
		{
			name: "an error without a location",
			f:    Finding{Rule: "RZ-CFG-016", Severity: diag.SeverityError, Message: "no ruralz.yaml"},
			want: `- error RZ-CFG-016: no ruralz.yaml`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(tc.f.AppendText(nil)); got != tc.want {
				t.Errorf("AppendText = %q, want %q", got, tc.want)
			}
			var buf bytes.Buffer
			if err := WriteText(&buf, []Finding{tc.f}); err != nil || buf.String() != tc.want+"\n" {
				t.Errorf("WriteText = %q, %v; want %q", buf.String(), err, tc.want+"\n")
			}
		})
	}
}

// TestWriteJSON: one array, [] when empty, each object's members in the
// order severity, rule, resource, message, file, line, column, path, hint,
// related, environment, those not set left out.
func TestWriteJSON(t *testing.T) {
	tests := []struct {
		name string
		fs   []Finding
		want string
	}{
		{name: "no findings", want: "[]\n"},
		{
			name: "every member",
			fs:   []Finding{sample()},
			want: `[
  {
    "severity": "warning",
    "rule": "cleartext-hop",
    "resource": {
      "kind": "Gateway",
      "name": "edge"
    },
    "message": "listener \"http\" serves cleartext HTTP",
    "file": "ruralz.yaml",
    "line": 9,
    "column": 17,
    "path": [
      "spec",
      "listeners",
      {
        "name": "http"
      },
      "protocol"
    ],
    "hint": "serve https",
    "related": [
      {
        "file": "routes/a.yaml",
        "line": 2,
        "column": 3,
        "message": "used by"
      }
    ],
    "environment": "prod"
  }
]
`,
		},
		{
			name: "the members of a bare finding",
			fs:   []Finding{{Rule: "RZ-CFG-016", Severity: diag.SeverityError, Message: "a <Gateway> & more"}},
			want: `[
  {
    "severity": "error",
    "rule": "RZ-CFG-016",
    "message": "a <Gateway> & more"
  }
]
`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteJSON(&buf, tc.fs); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tc.want {
				t.Errorf("WriteJSON =\n%s\nwant\n%s", buf.String(), tc.want)
			}
		})
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestWriteErrors: a failed write is an error of both forms.
func TestWriteErrors(t *testing.T) {
	if err := WriteJSON(failingWriter{}, []Finding{sample()}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteJSON to a failing writer = %v, want its error", err)
	}
	if err := WriteText(failingWriter{}, []Finding{sample()}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteText to a failing writer = %v, want its error", err)
	}
}

// TestFromDiagnostic: a diagnostic becomes the finding whose rule is its
// code, every other member kept, and Diagnostic turns it back.
func TestFromDiagnostic(t *testing.T) {
	d := diag.Diagnostic{
		Code: "RZ-CFG-013", Severity: diag.SeverityWarning, Location: diag.Location{File: "policies.yaml", Line: 7, Column: 9},
		Resource: &diag.ResourceID{Kind: "Policy", Name: "geo"}, Path: diag.Path{diag.Field("spec"), diag.Field("config")},
		Message: "credential-shaped literal", Hint: "use a secretRef",
		Related:     []diag.Related{{Location: diag.Location{File: "a.yaml", Line: 1}, Message: "x"}},
		Environment: "staging",
	}
	f := FromDiagnostic(d)
	if f.Rule != "RZ-CFG-013" || f.Severity != d.Severity || f.Location != d.Location || f.Resource != d.Resource ||
		f.Path.String() != d.Path.String() || f.Message != d.Message || f.Hint != d.Hint || len(f.Related) != 1 || f.Environment != "staging" {
		t.Errorf("FromDiagnostic(%+v) = %+v", d, f)
	}
	if got, want := string(f.AppendText(nil)), string(d.AppendText(nil)); got != want {
		t.Errorf("the finding's text %q differs from the diagnostic's %q", got, want)
	}
	if got := FromDiagnostics(diag.List{d, d}); len(got) != 2 || got[1].Rule != "RZ-CFG-013" {
		t.Errorf("FromDiagnostics = %+v", got)
	}
	if got := FromDiagnostics(nil); got == nil || len(got) != 0 {
		t.Errorf("FromDiagnostics(nil) = %#v, want an empty slice", got)
	}
}

// TestSort: findings sort by environment, file, line, column, rule, path
// and message, as diagnostics do.
func TestSort(t *testing.T) {
	at := func(env, file string, line, col int, rule, field, msg string) Finding {
		return Finding{
			Rule: rule, Severity: diag.SeverityWarning, Environment: env, Message: msg,
			Location: diag.Location{File: file, Line: line, Column: col}, Path: diag.Path{diag.Field(field)},
		}
	}
	want := []Finding{
		at("", "a.yaml", 1, 1, "r", "x", "m"),
		at("", "a.yaml", 1, 2, "r", "x", "m"),
		at("", "a.yaml", 2, 1, "r", "x", "m"),
		at("", "b.yaml", 1, 1, "r", "x", "m"),
		at("prod", "a.yaml", 1, 1, "a", "x", "m"),
		at("prod", "a.yaml", 1, 1, "b", "x", "m"),
		at("prod", "a.yaml", 1, 1, "b", "y", "m"),
		at("prod", "a.yaml", 1, 1, "b", "y", "n"),
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	Sort(got)
	text := func(fs []Finding) []string {
		out := make([]string, 0, len(fs))
		for _, f := range fs {
			out = append(out, string(f.AppendText(nil)))
		}
		return out
	}
	if !slices.Equal(text(got), text(want)) {
		t.Errorf("Sort =\n%s\nwant\n%s", strings.Join(text(got), "\n"), strings.Join(text(want), "\n"))
	}
}

// TestHasErrors: any finding but a warning is an error, as diagnostics
// count them.
func TestHasErrors(t *testing.T) {
	w := Finding{Rule: RuleCleartextHop, Severity: diag.SeverityWarning}
	e := Finding{Rule: RulePluginPoolSmall, Severity: diag.SeverityError}
	zero := Finding{Rule: "RZ-CFG-005"}
	tests := []struct {
		name string
		fs   []Finding
		want bool
	}{
		{name: "none"},
		{name: "warnings only", fs: []Finding{w, w}},
		{name: "an error", fs: []Finding{w, e}, want: true},
		{name: "a finding without a severity", fs: []Finding{zero}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasErrors(tc.fs); got != tc.want {
				t.Errorf("HasErrors = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRules: every rule name appears once, lower-case words joined by
// hyphens, and only plugin-enforcing-open and plugin-pool-small are
// errors.
func TestRules(t *testing.T) {
	seen := map[string]bool{}
	var errorsOnly []string
	for _, r := range Rules() {
		if seen[r.Name] {
			t.Errorf("rule %s listed twice", r.Name)
		}
		seen[r.Name] = true
		for _, w := range strings.Split(r.Name, "-") {
			if w == "" || strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
				t.Errorf("rule name %q is not lower-case words joined by hyphens", r.Name)
			}
		}
		if r.Severity == diag.SeverityError {
			errorsOnly = append(errorsOnly, r.Name)
		} else if r.Severity != diag.SeverityWarning {
			t.Errorf("rule %s has severity %v", r.Name, r.Severity)
		}
	}
	if want := []string{RulePluginEnforcingOpen, RulePluginPoolSmall}; !slices.Equal(errorsOnly, want) {
		t.Errorf("error rules = %q, want %q", errorsOnly, want)
	}
	if len(seen) != 11 {
		t.Errorf("%d rules, want 11", len(seen))
	}
	if got := severityOf("no-such-rule"); got != diag.SeverityError {
		t.Errorf("severityOf an unknown rule = %v, want error", got)
	}
}
