// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli"
)

// The one field the REST project's diff changes: partner-orders' Route
// timeout, from the rendered 5s to 6s.
const (
	renderedTimeout = "\n  timeout: \"5s\"\n"
	changedTimeout  = "\n  timeout: \"6s\"\n"
)

// changeTimeout returns the rendered REST Bundle with partner-orders'
// timeout changed, the one field ruralz bundle diff must report.
func changeTimeout(rendered []byte) ([]byte, error) {
	if n := bytes.Count(rendered, []byte(renderedTimeout)); n != 1 {
		return nil, fmt.Errorf("the rendered Bundle holds %q %d times, want once (partner-orders)", strings.TrimSpace(renderedTimeout), n)
	}
	return bytes.Replace(rendered, []byte(renderedTimeout), []byte(changedTimeout), 1), nil
}

// diffDoc is the part of a ruralz.diff.v1 document the suite checks.
type diffDoc struct {
	Format    string            `json:"format"`
	From      map[string]string `json:"from"`
	To        map[string]string `json:"to"`
	Resources []struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name"`
		Change string   `json:"change"`
		Impact []string `json:"impact"`
		Ops    []struct {
			Op   string `json:"op"`
			Path []any  `json:"path"`
			From any    `json:"from"`
			To   any    `json:"to"`
		} `json:"ops"`
	} `json:"resources"`
	Effective []json.RawMessage `json:"effective"`
	Summary   struct {
		Changed int      `json:"changed"`
		Impact  []string `json:"impact"`
	} `json:"summary"`
}

// timeoutDiffJSON checks a ruralz.diff.v1 document from the changed
// render to a state with the rendered timeout: exactly that operation,
// with its path and impact class; toMember names the TO side's source
// form ("admin" or "dump"). It returns every problem.
func timeoutDiffJSON(out []byte, toMember string) []string {
	var d diffDoc
	if err := json.Unmarshal(out, &d); err != nil {
		return []string{fmt.Sprintf("diff JSON %q: %v", out, err)}
	}
	var problems []string
	if d.Format != "ruralz.diff.v1" {
		problems = append(problems, fmt.Sprintf("format %q, want ruralz.diff.v1", d.Format))
	}
	if d.From["bundle"] == "" || d.To[toMember] == "" {
		problems = append(problems, fmt.Sprintf("sides from %v to %v, want a bundle FROM and a %s TO", d.From, d.To, toMember))
	}
	if len(d.Resources) != 1 {
		return append(problems, fmt.Sprintf("%d resources changed, want 1: %s", len(d.Resources), out))
	}
	r := d.Resources[0]
	if r.Kind != "Route" || r.Name != routePartner || r.Change != "modified" || !slices.Equal(r.Impact, []string{"traffic"}) {
		problems = append(problems, fmt.Sprintf("resource %s/%s %s impact %v, want Route/%s modified [traffic]", r.Kind, r.Name, r.Change, r.Impact, routePartner))
	}
	if len(r.Ops) != 1 || r.Ops[0].Op != "replace" || fmt.Sprint(r.Ops[0].Path) != "[spec timeout]" || r.Ops[0].From != "6s" || r.Ops[0].To != "5s" {
		problems = append(problems, fmt.Sprintf("operations %+v, want one replace of spec.timeout from 6s to 5s", r.Ops))
	}
	if len(d.Effective) != 0 || d.Summary.Changed != 1 || !slices.Equal(d.Summary.Impact, []string{"traffic"}) {
		problems = append(problems, fmt.Sprintf("effective %s, summary %+v; want no chain change, 1 change, impact [traffic]", d.Effective, d.Summary))
	}
	return problems
}

// timeoutDiffText checks the human form of the same diff.
func timeoutDiffText(out string) []string {
	var problems []string
	for _, want := range []string{"~ Route/" + routePartner, "[traffic]", "~ spec.timeout: 6s -> 5s", "1 resource changed"} {
		if !strings.Contains(out, want) {
			problems = append(problems, fmt.Sprintf("diff text lacks %q:\n%s", want, out))
		}
	}
	if n := strings.Count(out, "\n    "); n != 1 {
		problems = append(problems, fmt.Sprintf("diff text holds %d operations, want 1:\n%s", n, out))
	}
	return problems
}

// emptyDiffJSON checks the JSON diff of two equal states.
func emptyDiffJSON(out []byte) []string {
	var d diffDoc
	if err := json.Unmarshal(out, &d); err != nil {
		return []string{fmt.Sprintf("diff JSON %q: %v", out, err)}
	}
	if len(d.Resources) != 0 || d.Summary.Changed != 0 {
		return []string{fmt.Sprintf("diff of equal states: %s", out)}
	}
	return nil
}

func TestTimeoutDiffChecks(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	bundle, envs := restSource(t, root, dir)
	rendered := filepath.Join(dir, "rendered.yaml")
	if code, _, stderr := runCLI(t, "bundle", "render", "--env", composeEnv, "--environments", envs, "--output-file", rendered, bundle); code != cli.ExitOK {
		t.Fatalf("render: exit %d: %s", code, stderr)
	}
	data, err := os.ReadFile(rendered) //nolint:gosec // G304: a file the test wrote
	if err != nil {
		t.Fatal(err)
	}
	changedData, err := changeTimeout(data)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(dir, "changed.yaml")
	if err := os.WriteFile(changed, changedData, 0o600); err != nil { //nolint:gosec // G703: a changed copy of the test's render under t.TempDir
		t.Fatal(err)
	}
	if _, err := changeTimeout(changedData); err == nil {
		t.Error("changeTimeout of an already changed render succeeded")
	}
	// The file-to-file diff has the shape the REST project checks against
	// the Node: a rendered Bundle on the TO side stands in for its dump.
	code, text, stderr := runCLI(t, "bundle", "diff", changed, rendered)
	if code != cli.ExitNegative {
		t.Fatalf("diff changed rendered: exit %d, want %d: %s", code, cli.ExitNegative, stderr)
	}
	if p := timeoutDiffText(text); len(p) > 0 {
		t.Errorf("timeoutDiffText of the CLI's output: %v", p)
	}
	code, js, stderr := runCLI(t, "bundle", "diff", "--output", "json", changed, rendered)
	if code != cli.ExitNegative {
		t.Fatalf("diff --output json: exit %d: %s", code, stderr)
	}
	if p := timeoutDiffJSON([]byte(js), "bundle"); len(p) > 0 {
		t.Errorf("timeoutDiffJSON of the CLI's output: %v", p)
	}
	if p := timeoutDiffJSON([]byte(js), "admin"); len(p) == 0 {
		t.Error("timeoutDiffJSON accepted a bundle TO side for an admin one")
	}
	code, js, stderr = runCLI(t, "bundle", "diff", "--output", "json", rendered, rendered)
	if code != cli.ExitOK {
		t.Fatalf("diff of one render with itself: exit %d: %s", code, stderr)
	}
	if p := emptyDiffJSON([]byte(js)); len(p) > 0 {
		t.Errorf("emptyDiffJSON: %v", p)
	}
	if p := emptyDiffJSON([]byte(`{"resources":[{"kind":"Route"}],"summary":{"changed":1}}`)); len(p) == 0 {
		t.Error("emptyDiffJSON accepted a change")
	}
}
