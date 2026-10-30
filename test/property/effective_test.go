// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// effectiveRow is one row of ruralz bundle render --effective --output
// json without --route.
type effectiveRow struct {
	Route       string `json:"route"`
	Phase       string `json:"phase"`
	Leg         string `json:"leg"`
	Policy      string `json:"policy"`
	Type        string `json:"type"`
	FilterClass string `json:"filterClass"`
	Slot        string `json:"slot"`
	From        string `json:"from"`
	Reason      string `json:"reason"`
}

// FuzzEffectiveChain is the Effective chain row of the Required
// properties: for random Bundles, the snapshot compiler's per-Phase
// arrays, per Route and leg, equal the rows ruralz bundle render
// --effective prints (docs/reference/01-cli-and-api-surface.md "Bundle
// verbs in detail", docs/architecture/03-data-plane.md "Filter Chain
// execution"). A random valid Bundle, written to a directory, is compiled
// as a Node compiles it (the configuration pipeline, then the snapshot
// compiler with the built-in Filters), and ruralz bundle render
// --effective --output json runs on the same directory in process: for
// every Route, the Policies of each Phase's array on the client leg and
// on each upstream leg, in order, with their type, Filter class and scope,
// are the rows of that Route, Phase and leg in the order printed, the
// client leg before the legs in leg order, and the Policies the Route
// removed follow with Phase and leg none.
func FuzzEffectiveChain(f *testing.F) {
	for i := range uint64(16) {
		f.Add(i*0x9e3779b97f4a7c15 + 7)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		tc := tools(t)
		clk := clocktest.New(epoch())
		node := newNodeCompiler(t, clk, nodeParts{})
		g := generate(newRand(seed), genOptions{mode: genValid, node: true})
		dir := filepath.Join(t.TempDir(), "bundle")
		files := g.files(node.secretPath())
		writeTree(t, dir, files)
		res, err := tc.pipe.Run(t.Context(), pipeline.Request{Source: loader.Source{Path: dir}, Variables: subst.Map{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Validated == nil || res.HasErrors() {
			t.Fatalf("a generated valid Bundle does not build:\n%s\n%s", texts(res.Diagnostics), dumpFiles(files))
		}
		compiled, ds, err := node.comp.Compile(t.Context(), compile.Input{Validated: res.Validated, Files: res.Files})
		if err != nil || ds.HasErrors() || compiled == nil {
			t.Fatalf("the snapshot compiler refuses the Bundle: %v\n%s\n%s", err, texts(ds), dumpFiles(files))
		}
		defer compiled.Discard()
		stdout, stderr, code := ruralz(t.Context(), nil, "bundle", "render", "--effective", "--output", "json", dir)
		if code != cli.ExitOK {
			t.Fatalf("ruralz bundle render --effective exits %d: %s", code, stderr)
		}
		var rows []effectiveRow
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("ruralz bundle render --effective prints no JSON rows: %v\n%s", err, stdout)
		}
		checkEffective(t, compiled.Snapshot, rows, g)
	})
}

// checkEffective holds the rows of every Route equal to the compiled
// snapshot's arrays: first the Phases in execution order, each the client
// leg's array, then each leg's in name order (the order of a map list of
// Upstreams), then one none row per Policy the Route removed.
func checkEffective(t *testing.T, snap *snapshot.Snapshot, rows []effectiveRow, g *genBundle) {
	t.Helper()
	byRoute := map[string][]effectiveRow{}
	var order []string
	for _, r := range rows {
		if _, ok := byRoute[r.Route]; !ok {
			order = append(order, r.Route)
		}
		byRoute[r.Route] = append(byRoute[r.Route], r)
	}
	if !slices.IsSorted(order) {
		t.Fatalf("render --effective prints the Routes in the order %q, not in canonical order", order)
	}
	var compiled []string
	for _, rt := range snap.Routes {
		want := arraysOf(rt)
		got := byRoute[rt.Name]
		var running []string
		removed := 0
		for i, r := range got {
			if r.Phase == "none" {
				if r.Leg != "none" {
					t.Fatalf("Route %s: a removed Policy %s on leg %s", rt.Name, r.Policy, r.Leg)
				}
				removed++
				continue
			}
			if removed > 0 {
				t.Fatalf("Route %s: row %d, %s in %s, follows a removed Policy", rt.Name, i, r.Policy, r.Phase)
			}
			running = append(running, fmt.Sprintf("%s %s %s %s %s %s", r.Phase, r.Leg, r.Policy, r.Type, r.FilterClass, r.From))
		}
		if !slices.Equal(running, want) {
			t.Fatalf("Route %s: render --effective prints\n  %s\nthe compiled snapshot runs\n  %s", rt.Name,
				strings.Join(running, "\n  "), strings.Join(want, "\n  "))
		}
		if wantRemoved := removedOf(g, rt.Name); removed != wantRemoved {
			t.Fatalf("Route %s: %d removed Policies printed, the Route removes %d: %+v", rt.Name, removed, wantRemoved, got)
		}
		if len(got) > 0 {
			compiled = append(compiled, rt.Name)
		}
	}
	slices.Sort(compiled)
	if printed := slices.Sorted(maps.Keys(byRoute)); !slices.Equal(printed, compiled) {
		t.Fatalf("render --effective prints Routes %q, the snapshot runs Policies on %q", printed, compiled)
	}
}

// arraysOf returns a compiled Route's per-Phase arrays as rows: per Phase,
// the client leg's Policies, then each upstream leg's in name order.
func arraysOf(rt *snapshot.Route) []string {
	legs := slices.Sorted(maps.Keys(rt.Chain.Legs))
	var out []string
	row := func(ph phase.Phase, leg string, p *snapshot.Policy) string {
		from := p.Scope.String()
		if p.Scope == phase.ScopeUpstream {
			from += " " + leg
		}
		return fmt.Sprintf("%s %s %s %s %s %s", ph, leg, p.Name, p.Type, p.Class, from)
	}
	for ph := phase.OnRequestHeaders; ph < phase.Count; ph++ {
		for _, p := range rt.Chain.Client[ph] {
			out = append(out, row(ph, "client", p))
		}
		for _, leg := range legs {
			if lc := rt.Chain.Legs[leg]; lc != nil {
				for _, p := range lc.Phases[ph] {
					out = append(out, row(ph, leg, p))
				}
			}
		}
	}
	return out
}

// removedOf returns how many Gateway Policies the generated Route removes:
// excluded, or replaced by a Route Policy in their slot. Excluding a
// Policy the Gateway does not attach removes nothing.
func removedOf(g *genBundle, route string) int {
	for _, rt := range g.routes {
		if rt.name != route {
			continue
		}
		n := len(rt.exclude) - len(slices.DeleteFunc(slices.Clone(rt.exclude), func(name string) bool { return slices.Contains(g.gateway, name) }))
		for _, name := range g.remaining(rt) {
			slot := g.policy(name).effectiveSlot()
			if slices.ContainsFunc(rt.policies, func(p string) bool { return g.policy(p).effectiveSlot() == slot }) {
				n++
			}
		}
		return n
	}
	return 0
}
