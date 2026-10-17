// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestConcurrentApply covers the Substituter contract of 01 req 53
// (review finding): one Substituter serves every loader worker, so Apply
// on many goroutines over resources that exercise dispatch (a Policy
// spec.type of ${T}, credential headers, undefined variables with hints)
// gives exactly the diagnostics and trees of a sequential run; -race
// checks the shared state.
func TestConcurrentApply(t *testing.T) {
	var srcs []string
	for _, typ := range []string{"headers", "authz.cel", "cors", "plugin", "validation.json-schema", "ratelimit", "quota", "auth.jwt"} {
		srcs = append(srcs,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"${T_`+typ+`}","config":{"rule":"${A}","schema":{"enum":["${A}"]},"request":{"set":[{"name":"authorization","value":"${UNSET_X:-pw}"}]},"x":"${UNDEFINED}"}}}`)
	}
	vars := Map{"A": "a"}
	var hints []string
	for _, typ := range []string{"headers", "authz.cel", "cors", "plugin", "validation.json-schema", "ratelimit", "quota", "auth.jwt"} {
		vars["T_"+typ] = typ
		hints = append(hints, "T_"+typ)
	}
	hints = append(hints, "UNDEFINEX")
	files := &tree.FileTable{}
	parse := func() []*tree.Resource {
		var rs []*tree.Resource
		for i, src := range srcs {
			for j := range 8 {
				rs = append(rs, docs(t, files, "p"+strconv.Itoa(i)+"-"+strconv.Itoa(j)+".yaml", src)...)
			}
		}
		return rs
	}
	s := New(Options{Schemas: schemas(t), Files: files, Variables: vars, HintNames: hints})
	seq, conc := parse(), parse()
	want := make([][]string, len(seq))
	for i, r := range seq {
		want[i] = texts(s.Apply(r))
	}
	got := make([]diag.List, len(conc))
	var wg sync.WaitGroup
	for i, r := range conc {
		wg.Go(func() { got[i] = s.Apply(r) })
	}
	wg.Wait()
	for i := range conc {
		if g := texts(got[i]); !slices.Equal(g, want[i]) {
			t.Errorf("resource %d: concurrent diagnostics\n%q\nwant the sequential\n%q", i, g, want[i])
		}
		if g, w := dump(conc[i].Root), dump(seq[i].Root); g != w {
			t.Errorf("resource %d: concurrent tree\n%s\nwant\n%s", i, g, w)
		}
	}
	if len(want[0]) == 0 || slices.Equal(want[0], want[8]) {
		t.Errorf("the fixtures do not exercise dispatch: %q and %q", want[0], want[8])
	}
}
