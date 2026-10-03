// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// defaultPerShard is the number of targets per shard: eight 15-minute
// targets make a 2-hour shard inside the 3-hour job limit (11 req 28).
const defaultPerShard = 8

// target is one fuzz target.
type target struct {
	Package string `json:"package"` // import path
	Name    string `json:"name"`    // FuzzXxx
}

// testEvent is the subset of a `go test -json` event the plan reads. Since
// Go 1.24 compiler messages arrive as "build-output" events keyed by
// ImportPath (such as "example.com/m/p [example.com/m/p.test]"), and the
// package's "fail" event names that build in FailedBuild.
type testEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	ImportPath  string `json:"ImportPath"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	FailedBuild string `json:"FailedBuild"`
}

// failure is a package that failed while listing, and the build that
// failed for it, if any.
type failure struct {
	pkg, build string
}

// buildKey trims the " [pkg.test]" suffix of a build-output ImportPath.
func buildKey(importPath string) string {
	p, _, _ := strings.Cut(importPath, " [")
	return p
}

// errList reports a package that failed while listing its targets.
var errList = errors.New("listing fuzz targets failed")

// parseList reads `go test -json -list '^Fuzz' <packages>` output and
// returns the targets sorted by package and name. A package that fails to
// build or list is an error, so a broken package never silently drops its
// targets from the nightly plan; the error carries the package's output and
// the compiler messages of its failed build.
func parseList(data []byte) ([]target, error) {
	fuzzName := regexp.MustCompile(`^Fuzz\w*$`)
	seen := map[target]bool{}
	var out []target
	output := map[string]*strings.Builder{}
	builds := map[string]*strings.Builder{} // build-output by ImportPath
	var buildOrder []string
	var failed []failure
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue // build output printed outside the JSON stream
		}
		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("%w: %w", errList, err)
		}
		switch ev.Action {
		case "build-output":
			b := builds[ev.ImportPath]
			if b == nil {
				b = &strings.Builder{}
				builds[ev.ImportPath] = b
				buildOrder = append(buildOrder, ev.ImportPath)
			}
			b.WriteString(ev.Output)
		case "output":
			b := output[ev.Package]
			if b == nil {
				b = &strings.Builder{}
				output[ev.Package] = b
			}
			b.WriteString(ev.Output)
			name := strings.TrimSpace(ev.Output)
			if ev.Test == "" && fuzzName.MatchString(name) {
				t := target{Package: ev.Package, Name: name}
				if !seen[t] {
					seen[t] = true
					out = append(out, t)
				}
			}
		case "fail":
			if ev.Test == "" {
				failed = append(failed, failure{pkg: ev.Package, build: ev.FailedBuild})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		return nil, listError(failed, output, builds, buildOrder)
	}
	sortTargets(out)
	return out, nil
}

// listError reports the failed packages with their output. The compiler
// messages of a failed build are attached to the package whose fail event
// names that build (FailedBuild) or, without one, whose path the build's
// ImportPath has; build output no failed package claims follows in a
// general section, so no compiler message is dropped.
func listError(failed []failure, output, builds map[string]*strings.Builder, buildOrder []string) error {
	claimed := map[string]bool{}
	var b strings.Builder
	for _, f := range failed {
		fmt.Fprintf(&b, "\n%s:\n", f.pkg)
		for _, ip := range buildOrder {
			match := ip == f.build || (f.build == "" && buildKey(ip) == f.pkg)
			if claimed[ip] || !match {
				continue
			}
			claimed[ip] = true
			b.WriteString(builds[ip].String())
		}
		if o := output[f.pkg]; o != nil {
			b.WriteString(o.String())
		}
	}
	var rest strings.Builder
	for _, ip := range buildOrder {
		if !claimed[ip] {
			rest.WriteString(builds[ip].String())
		}
	}
	if rest.Len() > 0 {
		b.WriteString("\nbuild output:\n")
		b.WriteString(rest.String())
	}
	return fmt.Errorf("%w in %d package(s):%s", errList, len(failed), b.String())
}

// sortTargets orders targets by package, then name: the deterministic
// order the shards are cut from.
func sortTargets(ts []target) {
	slices.SortFunc(ts, func(a, b target) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// shardCount returns the number of shards of per targets needed for n.
func shardCount(n, per int) int {
	return (n + per - 1) / per
}

// shardOf returns shard index of the sorted targets: targets
// [index*per, (index+1)*per). An index beyond the last shard is empty.
func shardOf(ts []target, per, index int) []target {
	lo := index * per
	if index < 0 || lo >= len(ts) {
		return nil
	}
	return ts[lo:min(lo+per, len(ts))]
}

// checkShards validates a fixed matrix size: shards must hold every target.
func checkShards(n, per, shards int) error {
	if need := shardCount(n, per); shards > 0 && shards < need {
		return fmt.Errorf("%d targets need %d shards of %d, but only %d are configured", n, need, per, shards)
	}
	return nil
}
