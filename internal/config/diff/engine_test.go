// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestNewNeedsIndex: an Engine needs the hub schema.
func TestNewNeedsIndex(t *testing.T) {
	if _, err := New(nil, Options{}); !errors.Is(err, ErrInput) {
		t.Errorf("New(nil) error = %v, want ErrInput", err)
	}
	for _, w := range []int{-1, 0, 1, 7, 100000} {
		e := engine(t, w)
		if e.workers < 1 || e.workers > MaxWorkers {
			t.Errorf("Workers %d: engine runs %d", w, e.workers)
		}
	}
}

// TestCompareRefusesBadSides: a side without a state, Bundle, digest or
// tree is ErrInput; content that does not hash to its digest is
// RZ-CFG-027.
func TestCompareRefusesBadSides(t *testing.T) {
	good := render(t, shop(t, "to"))
	mismatch := *good
	mismatch.Revision = revision.Revision{Digest: revision.Sum([]byte("other")), Content: good.Revision.Content}
	noDigest := *good
	noDigest.Revision = revision.Revision{Content: good.Revision.Content}
	noTree := *good
	rs := good.Bundle.Resources()
	broken := *rs[0]
	broken.Tree = nil
	noTree.Bundle = hub.NewBundle(append([]*hub.Resource{&broken}, rs[1:]...))
	cases := []struct {
		name string
		side Side
		code string
	}{
		{"no state", Side{}, ""},
		{"no Bundle", Side{State: &hub.Validated{Revision: good.Revision}}, ""},
		{"no digest", Side{State: &noDigest}, ""},
		{"no tree", Side{State: &noTree}, ""},
		{"digest mismatch", Side{State: &mismatch}, CodeDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range [][2]Side{{tc.side, {State: good}}, {{State: good}, tc.side}} {
				_, err := engine(t, 1).Compare(context.Background(), order[0], order[1])
				switch {
				case tc.code != "" && codeOf(err) != tc.code:
					t.Errorf("Compare() error = %v, want %s", err, tc.code)
				case tc.code == "" && !errors.Is(err, ErrInput):
					t.Errorf("Compare() error = %v, want ErrInput", err)
				}
			}
		})
	}
}

// TestCompareContext: a canceled context stops Compare with its error.
func TestCompareContext(t *testing.T) {
	a, b := render(t, shop(t, "from")), render(t, shop(t, "to"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, w := range []int{1, 4} {
		if _, err := engine(t, w).Compare(ctx, Side{State: a}, Side{State: b}); !errors.Is(err, context.Canceled) {
			t.Errorf("workers %d: Compare() error = %v, want context.Canceled", w, err)
		}
	}
}

// TestRunLowestUnitDecides: the lowest failing unit's error or panic is
// the outcome whatever the worker count, and every unit before it runs.
func TestRunLowestUnitDecides(t *testing.T) {
	boom := errors.New("boom")
	for _, workers := range []int{1, 2, 8} {
		var ran atomic.Int64
		err := run(context.Background(), 100, workers, func(_, i int) error {
			ran.Add(1)
			if i == 40 || i == 70 {
				return fmt.Errorf("unit %d: %w", i, boom)
			}
			return nil
		})
		if err == nil || err.Error() != "unit 40: boom" || ran.Load() < 41 {
			t.Errorf("workers %d: run() = %v after %d units, want unit 40's error", workers, err, ran.Load())
		}
		func() {
			defer func() {
				v := recover()
				wp, ok := v.(*workPanic)
				if !ok || wp.value != "unit 20" || wp.Unwrap() != nil || !strings.Contains(wp.Error(), "panic: unit 20") {
					t.Errorf("workers %d: recovered %v, want unit 20's panic", workers, v)
				}
			}()
			_ = run(context.Background(), 100, workers, func(_, i int) error {
				switch i {
				case 20:
					panic("unit 20")
				case 30:
					return boom
				case 60:
					panic("unit 60")
				}
				return nil
			})
			t.Errorf("workers %d: run() returned instead of panicking", workers)
		}()
	}
}

// TestPanicWrapsError: a panic with an error value unwraps to it.
func TestPanicWrapsError(t *testing.T) {
	cause := errors.New("cause")
	wp, err := capture(func() error { panic(cause) })
	if err != nil || !errors.Is(wp, cause) {
		t.Errorf("capture() = %v, %v; want a panic wrapping cause", wp, err)
	}
}

// bulk returns a Bundle with n Routes and Upstreams on top of the example
// Bundle, each Route's timeout set by timeout(i).
func bulk(t *testing.T, n int, timeout func(i int) string) []source {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: svc-%03d
spec:
  protocol: http
  endpoints:
    - address: "svc-%03d.shop.svc:8080"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: route-%03d
spec:
  match:
    path: {prefix: /svc/%03d}
  upstreams:
    - name: svc-%03d
  timeout: %s
`, i, i, i, i, i, timeout(i))
	}
	return with(shop(t, "to"), "bulk.yaml", b.String())
}

// TestWorkersDeterministic: every worker count gives the same output.
func TestWorkersDeterministic(t *testing.T) {
	a := render(t, bulk(t, 60, func(int) string { return "1s" }))
	b := render(t, edit(t, bulk(t, 60, func(i int) string { return fmt.Sprintf("%ds", 1+i%3) }), "ruralz.yaml", "    - name: geo-block-default\n", ""))
	var want []byte
	for _, w := range []int{1, 2, 3, 16} {
		d, err := engine(t, w).Compare(context.Background(), Side{State: a}, Side{State: b})
		if err != nil {
			t.Fatal(err)
		}
		got := append(d.AppendText(nil), d.AppendJSON(nil)...)
		if want == nil {
			want = got
			if d.Summary.Modified != 41 || d.Summary.RouteChainsChanged != 63 {
				t.Fatalf("summary %+v, want 40 Routes and the Gateway modified, 63 chains changed", d.Summary)
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("workers %d changed the output", w)
		}
	}
}

// TestUnknownKindComparesWithoutSchema: a resource kind the schema does
// not know compares without list types or classes rather than failing.
func TestUnknownKindComparesWithoutSchema(t *testing.T) {
	c := newComparer(hubIndex(t), registry.New(), nil, nil)
	mk := func(port string) *hub.Resource {
		root := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: "kind", Value: &tree.Node{Kind: tree.KindString, Text: "Widget"}},
			{Key: "metadata", Value: &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "name", Value: &tree.Node{Kind: tree.KindString, Text: "w"}}}}},
			{Key: "spec", Value: &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "ports", Value: &tree.Node{Kind: tree.KindList, Items: []*tree.Node{{Kind: tree.KindInt, Text: port}}}}}}},
		}}
		return &hub.Resource{ID: tree.ID{Kind: "Widget", Name: "w"}, Tree: root}
	}
	r, err := c.pair(pair{from: mk("1"), to: mk("2")})
	if err != nil || r == nil || len(r.Ops) != 1 || r.Ops[0].Kind != OpReplace || r.Impact != 0 {
		t.Fatalf("pair() = %+v, %v; want one whole-list replace without impact", r, err)
	}
}
