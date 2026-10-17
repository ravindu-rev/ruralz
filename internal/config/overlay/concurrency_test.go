// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestConcurrentCheckBase covers the concurrency contract of 01 req 53
// (review finding): the loader runs CheckBase per resource in its worker
// pool with one Options, so calls on many goroutines over resources that
// exercise dispatch (Policy types, a JSON Schema document) give exactly
// the diagnostics of a sequential run; -race checks the shared state.
func TestConcurrentCheckBase(t *testing.T) {
	files := &tree.FileTable{}
	var rs []*tree.Resource
	for _, typ := range []string{"validation.json-schema", "cors", "plugin", "headers", "auth.jwt", "ratelimit", "quota", "transform.request"} {
		src := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"` + typ +
			`","config":{"schema":{"$patch":1},"x":{"$patch":2},"l":[{"$patch":3}]}}}`
		for i := range 16 {
			rs = append(rs, docs(t, files, tree.RoleBase, typ+strconv.Itoa(i)+".yaml", src)...)
		}
	}
	var yields atomic.Int64
	opts := Options{Schemas: schemas(t), Files: files, Yield: func() { yields.Add(1) }}
	want := make([][]string, len(rs))
	for i, r := range rs {
		want[i] = texts(CheckBase(r, opts))
	}
	sequential := yields.Swap(0)
	got := make([]diag.List, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Go(func() { got[i] = CheckBase(r, opts) })
	}
	wg.Wait()
	if y := yields.Load(); y != sequential {
		t.Errorf("a Yield shared by concurrent calls ran %d times, want the sequential %d", y, sequential)
	}
	for i := range rs {
		if g := texts(got[i]); !slices.Equal(g, want[i]) {
			t.Errorf("%s: concurrent diagnostics\n%q\nwant the sequential\n%q", files.File(rs[i].Start.File).Path, g, want[i])
		}
	}
	if len(want[0]) == 0 || len(want[1*16]) == len(want[0]) {
		t.Errorf("the fixtures do not exercise dispatch: %q and %q", want[0], want[16])
	}
}

// TestCheckBaseWorkerYield covers 01 req 53 for the loader's worker pool
// (review finding: the Yield doc did not say who may share it): each
// worker passes its own copy of Options whose Yield keeps per-worker
// state without a lock, and each worker's function runs exactly as often
// as a sequential run of its resources makes it; -race checks that no
// call touches another worker's function.
func TestCheckBaseWorkerYield(t *testing.T) {
	files := &tree.FileTable{}
	var keys []string
	for i := range 600 {
		keys = append(keys, `"k`+strconv.Itoa(i)+`":{"$patch":1}`)
	}
	src := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"cors","config":{` + strings.Join(keys, ",") + `}}}`
	base := Options{Schemas: schemas(t), Files: files}
	want := 0
	one := base
	one.Yield = func() { want++ }
	_ = CheckBase(docs(t, files, tree.RoleBase, "a.yaml", src)[0], one)
	if want < 2 {
		t.Fatalf("the fixture yields %d times", want)
	}
	const workers, each = 3, 4
	counts := make([]int, workers)
	var wg sync.WaitGroup
	for k := range workers {
		rs := make([]*tree.Resource, each)
		for i := range rs {
			rs[i] = docs(t, files, tree.RoleBase, "w"+strconv.Itoa(k)+"-"+strconv.Itoa(i)+".yaml", src)[0]
		}
		opts := base
		opts.Yield = func() { counts[k]++ }
		wg.Go(func() {
			for _, r := range rs {
				_ = CheckBase(r, opts)
			}
		})
	}
	wg.Wait()
	for k, c := range counts {
		if c != each*want {
			t.Errorf("worker %d: %d Yield calls, want %d", k, c, each*want)
		}
	}
}
