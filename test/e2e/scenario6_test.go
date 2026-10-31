// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Scenario 6 of docs/engineering/03-testing-and-quality-strategy.md
// "End-to-end tests": ruralz bundle diff compares the Node's own render,
// with one field changed, against the Node's admin URL and against a
// saved ruralz node dump file: exit 1 and exactly that operation, with its
// path and impact class, in the human form and in ruralz.diff.v1 JSON,
// secrets only by reference. The unchanged render exits 0 against both.
// The Node serves the REST example Bundle, whose secret fields (TLS keys,
// the API key, the OAuth client secret, the State Store URL) all hold
// canaries, so the leak scan also covers every diff.

// diffDoc is the part of a ruralz.diff.v1 document the scenario checks.
type diffDoc struct {
	Format string       `json:"format"`
	From   diffSide     `json:"from"`
	To     diffSide     `json:"to"`
	Res    []diffChange `json:"resources"`
	Eff    []any        `json:"effective"`
	Sum    struct {
		Changed  int      `json:"changed"`
		Added    int      `json:"added"`
		Removed  int      `json:"removed"`
		Modified int      `json:"modified"`
		Chains   int      `json:"routeChainsChanged"`
		Impact   []string `json:"impact"`
	} `json:"summary"`
}

// diffSide is a diff's from or to member.
type diffSide struct {
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	Bundle   string `json:"bundle"`
	Dump     string `json:"dump"`
	Admin    string `json:"admin"`
}

// diffChange is one changed resource.
type diffChange struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Change string   `json:"change"`
	Source string   `json:"source"`
	Impact []string `json:"impact"`
	Ops    []struct {
		Op   string `json:"op"`
		Path []any  `json:"path"`
		From any    `json:"from"`
		To   any    `json:"to"`
	} `json:"ops"`
}

// decodeDiff decodes the JSON output of ruralz bundle diff.
func decodeDiff(t testing.TB, r cliRun) diffDoc {
	t.Helper()
	var d diffDoc
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatalf("decode ruralz.diff.v1: %v\n%s", err, r)
	}
	if d.Format != "ruralz.diff.v1" {
		t.Errorf("format %q, want ruralz.diff.v1", d.Format)
	}
	// resources and effective are [] without changes, never null or absent
	// (docs/reference/01-cli-and-api-surface.md "Diff JSON compatibility").
	if d.Res == nil || d.Eff == nil {
		t.Errorf("resources %v, effective %v: want arrays, [] without changes\n%s", d.Res, d.Eff, r)
	}
	return d
}

// TestBundleDiffAgainstNode is scenario 6 on one Node.
func TestBundleDiffAgainstNode(t *testing.T) {
	s := startShopRest(t, shopRestOptions{nodes: 1})
	n := s.nodes[0]
	dir := t.TempDir()
	render := readFile(t, n.ConfigPath())
	secretPath, ok := n.SecretPath(refClientSecret)
	if !ok || strings.Count(string(render), secretPath) != 1 {
		t.Fatalf("the Node's render names the OAuth client secret's file %q %d times, want once", secretPath, strings.Count(string(render), secretPath))
	}
	// The one field: the file upstream-oauth's clientSecret reference
	// names, a secret field shown only by its reference.
	nextPath := secretPath + "-next"
	unchanged := writeFile(t, dir, "render.yaml", render)
	changed := writeFile(t, dir, "changed.yaml", []byte(strings.Replace(string(render), secretPath, nextPath, 1)))
	changedDigest, err := revisionDigest(readFile(t, changed))
	if err != nil {
		t.Fatal(err)
	}
	admin := []string{"--admin-token-file", n.TokenFile(), "--ca-file", s.ca}
	dump := filepath.Join(dir, "dump.json")
	if r := cli(t, s.top, cliOptions{dir: dir}, append([]string{"node", "dump", "--admin", n.AdminURL(), "--output-file", dump}, admin...)...); r.code != 0 {
		t.Fatalf("save the dump: %s", r)
	}
	nodeRev, changedRev := displayRevision(t, n.TestDigest()), displayRevision(t, changedDigest)
	for _, from := range []struct {
		name, arg string
		flags     []string
		member    func(diffSide) string
	}{
		{"admin", n.AdminURL(), admin, func(d diffSide) string { return d.Admin }},
		{"dump", dump, nil, func(d diffSide) string { return d.Dump }},
	} {
		t.Run(from.name, func(t *testing.T) {
			diff := func(to string, json bool) cliRun {
				args := append([]string{"bundle", "diff", from.arg, to}, from.flags...)
				if json {
					args = append(args, "--output", "json")
				}
				return cli(t, s.top, cliOptions{dir: dir}, args...)
			}

			r := diff(unchanged, false)
			want := []string{
				"ruralz bundle diff: " + nodeRev + " -> " + nodeRev, "",
				"0 resources changed: 0 added, 0 removed, 0 modified; 0 Route chains changed; impact: none",
			}
			if r.code != 0 || !slices.Equal(lines(r.stdout), want) || r.stderr != "" {
				t.Errorf("the unchanged render: %s\nwant exit 0 and\n%s", r, strings.Join(want, "\n"))
			}
			d := decodeDiff(t, diff(unchanged, true))
			if len(d.Res) != 0 || len(d.Eff) != 0 || d.Sum.Changed != 0 || len(d.Sum.Impact) != 0 ||
				d.From.Digest != n.TestDigest() || d.To.Digest != n.TestDigest() || from.member(d.From) != from.arg || d.To.Bundle != unchanged {
				t.Errorf("the unchanged render in JSON: %+v", d)
			}

			r = diff(changed, false)
			want = []string{
				"ruralz bundle diff: " + nodeRev + " -> " + changedRev, "",
				fmt.Sprintf("%-48s%s  [security]", "~ Policy/upstream-oauth", changed),
				"    ~ spec.config.clientSecret.secretRef.name: " + secretPath + " -> " + nextPath, "",
				"1 resource changed: 0 added, 0 removed, 1 modified; 0 Route chains changed; impact: security",
			}
			if r.code != 1 || !slices.Equal(lines(r.stdout), want) {
				t.Errorf("the changed render: %s\nwant exit 1 and\n%s", r, strings.Join(want, "\n"))
			}
			if strings.Contains(r.stdout, s.clientSecret) {
				t.Error("the diff shows the client secret's value")
			}
			d = decodeDiff(t, diff(changed, true))
			if d.From.Digest != n.TestDigest() || d.To.Digest != changedDigest || from.member(d.From) != from.arg || d.To.Bundle != changed ||
				d.From.Revision != nodeRev || d.To.Revision != changedRev {
				t.Errorf("sides: from %+v, to %+v; want the Node's %s by its %s and the changed render's %s", d.From, d.To, n.TestDigest(), from.name, changedDigest)
			}
			if len(d.Res) != 1 || len(d.Eff) != 0 || d.Sum.Changed != 1 || d.Sum.Modified != 1 || !slices.Equal(d.Sum.Impact, []string{"security"}) {
				t.Fatalf("the changed render in JSON: %+v, want one modified resource of impact security", d)
			}
			c := d.Res[0]
			wantPath := []any{"spec", "config", "clientSecret", "secretRef", "name"}
			if c.Kind != "Policy" || c.Name != "upstream-oauth" || c.Change != "modified" || !slices.Equal(c.Impact, []string{"security"}) ||
				len(c.Ops) != 1 || c.Ops[0].Op != "replace" || !slices.Equal(c.Ops[0].Path, wantPath) || c.Ops[0].From != secretPath || c.Ops[0].To != nextPath {
				t.Errorf("the change: %+v, want exactly the replace of %v from %s to %s", c, wantPath, secretPath, nextPath)
			}
		})
	}
}
