// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// pluginDoc returns a plugin Policy with config, whose members get no
// schema checks and no defaults.
func pluginDoc(config string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": ` + config + `}}`
}

// TestHugeCaps covers Options.MaxDiagnostics at the top of the int range,
// where neither the cap plus one nor twice the keeper's size may wrap (a
// wrap would drop every diagnostic while the resource still fails, or
// sort all the keeper's entries after every diagnostic). Every
// non-negative cap is valid, and each of these keeps both errors of a
// resource, through Resource and through Run with one and with two
// workers.
func TestHugeCaps(t *testing.T) {
	src := pluginDoc(`{"a": null, "b": null}`)
	for _, cap := range []int{math.MaxInt, math.MaxInt - 1, math.MaxInt / 2, 1 << 62, 1<<62 - 1} {
		t.Run(strconv.Itoa(cap), func(t *testing.T) {
			s := stageWith(t, Options{MaxDiagnostics: cap})
			if h, ds := s.Resource(envelope(mustParse(t, src)), nil); h != nil || len(ds) != 2 {
				t.Errorf("Resource = %v, %q; want both errors", h != nil, texts(ds))
			}
			for _, workers := range []int{1, 2} {
				rs := []*tree.Resource{envelope(mustParse(t, src)), envelope(mustParse(t, src))}
				if b, ds, err := s.Run(t.Context(), rs, nil, workers); b != nil || err != nil || len(ds) != 4 {
					t.Errorf("Run with %d workers = %v, %d diagnostics, %v; want the 4 errors", workers, b != nil, len(ds), err)
				}
			}
		})
	}
	if got := keepOf(math.MaxInt); got != math.MaxInt {
		t.Errorf("keepOf(math.MaxInt) = %d, want math.MaxInt", got)
	}
	if got := keepOf(0); got != 1 {
		t.Errorf("keepOf(0) = %d, want 1", got)
	}
}

// TestKeeperPrunes covers the keeper's memory bound without overflow, so
// 2n never wraps to a negative number for n of 2^62 or more: it prunes at
// most once per n entries added, plus one, for every n, and keeps the
// first n in order.
func TestKeeperPrunes(t *testing.T) {
	const adds = 600
	ds := make(diag.List, adds)
	for i := range ds {
		// In reverse order, so a prune would reorder them.
		ds[i] = diag.Diagnostic{Code: CodeSchema, Location: diag.Location{Line: adds - i}}
	}
	for _, n := range []int{1, 7, adds / 2, adds, math.MaxInt / 2, 1 << 62, math.MaxInt - 1, math.MaxInt} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			k := newKeeper(n)
			for i, d := range ds {
				k.add(i, diag.List{d})
			}
			if limit := adds/n + 1; k.prunes > limit {
				t.Errorf("%d prunes for %d adds, want at most %d", k.prunes, adds, limit)
			}
			got := k.list()
			if want := min(n, adds); len(got) != want {
				t.Fatalf("kept %d, want %d", len(got), want)
			}
			for i := range got {
				if got[i].Line != i+1 {
					t.Fatalf("kept[%d] at line %d, want %d", i, got[i].Line, i+1)
				}
			}
		})
	}
}

// panicky is a converter of the hub apiVersion whose ToHub panics on a
// resource named boom, with value, or calls runtime.Goexit there when
// goexit is set, and counts its calls and the calls in flight; on a
// resource named wait it returns once release is closed.
type panicky struct {
	value         any
	goexit        bool
	calls, active *atomic.Int64
	release       chan struct{}
}

func (panicky) APIVersion() string { return v1 }

func (c panicky) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	c.calls.Add(1)
	c.active.Add(1)
	defer c.active.Add(-1)
	meta, _ := doc.Get("metadata")
	name, _ := meta.Get("name")
	switch {
	case name == nil:
	case name.Text == "boom" && c.goexit:
		runtime.Goexit()
	case name.Text == "boom":
		panic(c.value)
	case name.Text == "wait":
		<-c.release
	}
	return doc, nil
}

func (panicky) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

// TestRunPanic covers a panic inside stage G during Run, which Run's
// workers recover, so a panicking converter never ends the process
// whatever the caller deferred: for one and for four workers, the panic
// of a Converter reaches the caller's recover in the calling goroutine,
// as an error that names the resource and wraps an error value, after
// every started resource has returned (no ToHub call is in flight), and
// no resource is started after it: one worker stops at the panicking
// resource, and of two, the other finishes the resource it has (wait,
// held until the panic stopped the dispatch) and takes no other. A
// converter that calls runtime.Goexit is handled the same way, so Run
// never returns a Bundle without that resource and every one the worker
// had not reached, with no error and no diagnostic: Run raises an error
// that wraps errGoexit, for every worker count.
func TestRunPanic(t *testing.T) {
	sentinel := errors.New("converter bug")
	for _, tc := range []struct {
		name    string
		value   any
		workers int
		// wraps reports a value that is the sentinel error.
		wraps bool
		// goexit makes the converter call runtime.Goexit.
		goexit bool
	}{
		{"string, one worker", "boom", 1, false, false},
		{"string, two workers", "boom", 2, false, false},
		{"string, four workers", "boom", 4, false, false},
		{"error, four workers", sentinel, 4, true, false},
		{"Goexit, one worker", errGoexit, 1, false, true},
		{"Goexit, two workers", errGoexit, 2, false, true},
		{"Goexit, four workers", errGoexit, 4, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, active atomic.Int64
			release := make(chan struct{})
			conv, err := convert.NewRegistry(convert.Lifecycle{}, panicky{value: tc.value, goexit: tc.goexit, calls: &calls, active: &active, release: release})
			if err != nil {
				t.Fatal(err)
			}
			s := stageWith(t, Options{Converters: conv})
			boom := 5
			if tc.workers == 2 {
				boom = 0
			}
			stops := 0
			s.stopped = func(i int) {
				stops++
				if i != boom {
					t.Errorf("stopped at resource %d, want %d", i, boom)
				}
				close(release)
			}
			var rs []*tree.Resource
			for i := range 12 {
				name := "r" + strconv.Itoa(i)
				switch {
				case i == boom:
					name = "boom"
				case tc.workers == 2 && i == 1:
					name = "wait"
				}
				rs = append(rs, envelope(mustParse(t, strings.Replace(route(`{"match": {}}`), `"name": "r"`, `"name": "`+name+`"`, 1))))
			}
			got := func() (v any) {
				defer func() { v = recover() }()
				_, _, _ = s.Run(t.Context(), rs, nil, tc.workers)
				return nil
			}()
			if got == nil {
				t.Fatal("Run returned; want the converter's panic in the caller")
			}
			if n := active.Load(); n != 0 {
				t.Errorf("%d ToHub calls still in flight after Run panicked", n)
			}
			if n := calls.Load(); (tc.workers == 1 && n != 6) || (tc.workers == 2 && n > 2) {
				t.Errorf("%d resources converted by %d workers: dispatch goes on after the panic", n, tc.workers)
			}
			if stops != 1 {
				t.Errorf("stop hook called %d times, want once", stops)
			}
			p, ok := got.(*resourcePanic)
			if !ok {
				t.Fatalf("recovered %T %v, want a *resourcePanic", got, got)
			}
			verb := map[bool]string{false: "panicked", true: "stopped"}[tc.goexit]
			if p.index != boom || p.value != tc.value || p.goexit != tc.goexit || !strings.Contains(p.Error(), verb+" on resource "+strconv.Itoa(boom)) || !strings.Contains(p.Error(), "panicky.ToHub") {
				t.Errorf("panic = index %d, value %v, goexit %v, text %.300q", p.index, p.value, p.goexit, p.Error())
			}
			if errors.Is(p, sentinel) != tc.wraps {
				t.Errorf("errors.Is(panic, sentinel) = %v, want %v", !tc.wraps, tc.wraps)
			}
			if errors.Is(p, errGoexit) != tc.goexit {
				t.Errorf("errors.Is(panic, errGoexit) = %v, want %v", !tc.goexit, tc.goexit)
			}
		})
	}
}
