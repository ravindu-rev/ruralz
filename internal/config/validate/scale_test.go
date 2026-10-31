// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
)

// scaleFiles returns the base Bundle plus n Upstreams with names of one
// length and an address without port, n Routes that each miss an
// Upstream of a similar name and repeat the criteria of the first Route,
// and n Policies whose shared secret goes to n origins: about n findings
// of every stage H path that reads a name set, a list, the match keys or
// the secret table.
func scaleFiles(t testing.TB, n int) map[string]string {
	t.Helper()
	files := readDirs(t, baseDir)
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: up-%06d
spec:
  protocol: http
  endpoints:
    - address: "up-%06d.svc"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: rt-%06d
spec:
  match:
    hosts: ["x.example"]
    path: {prefix: /same}
  upstreams:
    - name: up-%06dx
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: oauth-%06d
spec:
  type: auth.upstream-oauth2
  config:
    tokenUrl: https://auth-%06d.example/token
    clientId: c
    clientSecret: {secretRef: {provider: env, name: RURALZ_SECRET_SHARED}}
`, i, i, i, i, i, i)
	}
	files["scale.yaml"] = strings.TrimPrefix(b.String(), "---\n")
	return files
}

// unitsOf runs stage H on one worker of a counting Validator and returns
// the work units the run charged and its findings.
func unitsOf(t *testing.T, n int) (int, diag.List) {
	t.Helper()
	b, files := loadFiles(t, scaleFiles(t, n))
	v := newValidator(t, func(o *Options) { o.MaxDiagnostics = 1 << 30 })
	ix := v.index(b, files, &work{})
	w := &work{}
	keep := newKeeper(1 << 30)
	l := locator{files: files}
	var all []hub.SecretUse
	for i, r := range ix.resources {
		u := &unit{v: v, ix: ix, l: l, w: w, r: r, idx: i, f: l.newFinder(r, w), out: sink{k: keep, unit: uint64(i)}}
		u.validate()
		all = append(all, u.secrets...)
	}
	v.checkCredentials(b, l, w, &sink{k: keep})
	v.checkMatches(ix, l, w, &sink{k: keep})
	checkSecrets(all, w, &sink{k: keep})
	return w.total, keep.list()
}

// TestScaleLinear pins the growth of stage H with counters, not a clock:
// four times the resources, each with findings that read the name sets,
// the lists and the secret table, cost about four times the work units.
func TestScaleLinear(t *testing.T) {
	n := 200
	if testing.Short() {
		n = 50
	}
	small, ds := unitsOf(t, n)
	if len(ds) != 4*n-2 {
		t.Fatalf("%d findings for n=%d; the generator lost its findings", len(ds), n)
	}
	large, _ := unitsOf(t, 4*n)
	t.Logf("work units: %d for n=%d, %d for 4n", small, n, large)
	if ratio := float64(large) / float64(small); ratio > 4.4 {
		t.Errorf("work units %d for n=%d and %d for 4n: ratio %.2f, want at most 4.4", small, n, large, ratio)
	}
}

// TestHintCandidatesBounded: a hint compares at most maxCandidates names
// whatever the size of the set, so many missing references among many
// names cost linear time.
func TestHintCandidatesBounded(t *testing.T) {
	names := make([]string, 50_000)
	for i := range names {
		names[i] = fmt.Sprintf("name-%06d", i)
	}
	s := newNameSet(names)
	if got := len(s.candidates("name-x00001")); got != maxCandidates {
		t.Fatalf("candidates = %d, want %d", got, maxCandidates)
	}
	if got := s.hint("name-0000001"); got != `did you mean "name-000000"?` {
		t.Errorf("hint = %q", got)
	}
	if got := s.hint("NAME-000042"); got != `did you mean "name-000042"?` {
		t.Errorf("case-insensitive hint = %q", got)
	}
}
