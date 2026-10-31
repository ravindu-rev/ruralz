// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// The hostile-input set of docs/engineering/03-testing-and-quality-
// strategy.md "Conformance suites", generated at test time and never
// committed: whole loads, stages A to E, at the source, resource and node
// limits of docs/architecture/02-configuration-model.md "Restricted YAML
// profile", and canonical re-entry at the resource limit.
const (
	mib = 1 << 20
	// heapTarget bounds the heap growth of a whole load at any of the
	// limits or at all three together: 1,040 MiB (target).
	heapTarget = 1040 * mib
	// reentryFactor bounds the peak memory of canonical re-entry: four
	// times the canonical size (target, configuration model "Validation
	// and diff semantics").
	reentryFactor = 4
	// perFile is the documents of a generated file.
	perFile = 200
	// gatewayNodes and upstreamNodes are the YAML nodes of the generated
	// Gateway and Upstream documents, padding and annotations aside.
	gatewayNodes, upstreamNodes = 20, 18
)

// gatewayDoc returns ruralz.yaml's Gateway, whose annotations, when fill
// is not empty, hold one double-quoted string of fill[j] characters each,
// and its YAML nodes: the root, apiVersion, kind and metadata.name with
// their keys (9), the spec with its listener (11), and with annotations
// their key, their mapping and a key and a value per string.
func gatewayDoc(fill []int) (string, int) {
	var b strings.Builder
	nodes := gatewayNodes
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\n")
	if len(fill) > 0 {
		b.WriteString("  annotations:\n")
		nodes += 2
		for j, n := range fill {
			b.WriteString("    f" + strconv.Itoa(j) + `: "` + strings.Repeat("x", n) + "\"\n")
			nodes += 2
		}
	}
	b.WriteString("spec:\n  listeners:\n    - name: http\n      protocol: http\n      port: 8080\n")
	return b.String(), nodes
}

// upstreamDoc returns the Upstream u<i>, whose spec, when pad is not
// negative, holds a list of pad one-character items: its YAML nodes are
// upstreamNodes, plus the list's key, the list and its items.
func upstreamDoc(i, pad int) string {
	name := "u" + strconv.Itoa(i)
	doc := "---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: " + name +
		"\nspec:\n  protocol: http\n  endpoints:\n    - address: \"" + name + ".shop.svc:8080\"\n"
	if pad >= 0 {
		doc += "  pad: [" + strings.TrimSuffix(strings.Repeat("0,", pad), ",") + "]\n"
	}
	return doc
}

// hostileLoad returns a generated Bundle of resources documents: the
// Gateway of ruralz.yaml and resources-1 Upstreams, perFile to a file
// under u/. A positive nodes pads every Upstream so the load holds exactly
// that many YAML nodes, mappings, lists and scalars, keys included, as the
// parser counts them; a positive size fills the Gateway's annotations with
// size/MiB strings that bring the source to exactly size bytes. Zero
// leaves a measure where the documents put it.
func hostileLoad(t testing.TB, resources, nodes int, size int64) fstest.MapFS {
	t.Helper()
	docs := resources - 1
	fill := make([]int, size/mib)
	_, gwNodes := gatewayDoc(fill)
	pads := make([]int, docs)
	for i := range pads {
		pads[i] = -1
	}
	if nodes > 0 {
		items := nodes - gwNodes - (upstreamNodes+2)*docs
		if docs == 0 || items < 0 {
			t.Fatalf("%d documents cannot hold %d nodes", resources, nodes)
		}
		for i := range pads {
			pads[i] = items / docs
			if i < items%docs {
				pads[i]++
			}
		}
	}
	fsys := fstest.MapFS{}
	var total int64
	for f := 0; f*perFile < docs; f++ {
		var b strings.Builder
		for i := f * perFile; i < min(docs, (f+1)*perFile); i++ {
			b.WriteString(upstreamDoc(i+1, pads[i]))
		}
		fsys[fmt.Sprintf("u/%03d.yaml", f)] = &fstest.MapFile{Data: []byte(b.String())}
		total += int64(b.Len())
	}
	root, _ := gatewayDoc(fill)
	if len(fill) > 0 {
		rest := size - total - int64(len(root))
		if rest < 0 {
			t.Fatalf("the documents alone pass %d bytes", size)
		}
		for j := range fill {
			fill[j] = int(rest / int64(len(fill)))
			if int64(j) < rest%int64(len(fill)) {
				fill[j]++
			}
		}
		root, _ = gatewayDoc(fill)
	}
	fsys[bundleRoot] = &fstest.MapFile{Data: []byte(root)}
	if total += int64(len(root)); size > 0 && total != size {
		t.Fatalf("generated %d bytes, want %d", total, size)
	}
	return fsys
}

// peakHeap runs f and returns the largest growth of live heap objects and
// goroutine stacks over their size before f, sampling
// /memory/classes/heap/objects:bytes and /memory/classes/heap/stacks:bytes
// every millisecond as the parser's hostile-input tests do. A sample can
// miss a peak but never invents one, so a bound it checks never fails
// spuriously, whatever the timing.
func peakHeap(f func()) uint64 {
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/heap/stacks:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64() + s[1].Value.Uint64()
	}
	runtime.GC()
	base := read()
	peak := base
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				peak = max(peak, read())
			}
		}
	}()
	f()
	close(stop)
	<-done
	return max(peak, read()) - base
}

// skipHeavy skips a hostile case under -short, and under -race, whose
// detector multiplies the memory it measures.
func skipHeavy(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("generates loads of up to 64 MiB and 4,000,000 nodes")
	}
	if raceEnabled {
		t.Skip("measures memory, which the race detector multiplies")
	}
}

// TestHostileLoads checks the whole loads of the hostile-input set,
// stages A to E with the CLI's limits and workers: a load at the 64 MiB
// source limit, at the 20,000-resource limit, at the 4,000,000-node budget
// and at all three together loads without a finding and grows the heap by
// at most heapTarget, and one byte, resource or node past a limit stops
// the load with RZ-CFG-001.
func TestHostileLoads(t *testing.T) {
	skipHeavy(t)
	s := mustSuite(t)
	l, err := loader.New(loader.Options{Schemas: s.cli.Schemas(), Converters: s.cli.Converters(), Limits: s.cli.Limits(), Workers: s.cli.Workers()})
	if err != nil {
		t.Fatal(err)
	}
	limits := s.cli.Limits()
	size, resources, nodes := limits.MaxSourceBytes, limits.MaxResources, limits.MaxNodes()
	if size != loader.DefaultMaxSourceBytes || resources != loader.DefaultMaxResources || nodes != 4_000_000 {
		t.Fatalf("the CLI's limits are %d bytes, %d resources and %d nodes, not the defaults", size, resources, nodes)
	}
	for _, tc := range []struct {
		name             string
		resources, nodes int
		size             int64
		// past names the limit the load passes, "" for a load at the
		// limits.
		past string
	}{
		{"source limit", 1, 0, size, ""},
		{"one byte past the source limit", 1, 0, size + 1, "byte"},
		{"resource limit", resources, 0, 0, ""},
		{"one resource past the resource limit", resources + 1, 0, 0, "resource"},
		{"node budget", 1000, nodes, 0, ""},
		{"one node past the node budget", 1000, nodes + 1, 0, "node"},
		{"all three limits", resources, nodes, size, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := hostileLoad(t, tc.resources, tc.nodes, tc.size)
			var res *loader.Result
			grew := peakHeap(func() {
				res, err = l.Load(t.Context(), loader.Request{Source: loader.Source{FS: fsys, Path: "."}})
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the heap grew by %d MiB", grew/mib)
			if tc.past != "" {
				i := len(res.Diagnostics) - 1
				if !res.Stopped || i < 0 || res.Diagnostics[i].Code != loader.CodeLimit || !strings.Contains(res.Diagnostics[i].Message, tc.past) {
					t.Errorf("one %s past the limit: stopped %v, diagnostics\n%s\nwant a stop at RZ-CFG-001", tc.past, res.Stopped, texts(res.Diagnostics))
				}
				return
			}
			if res.Stopped || len(res.Diagnostics) > 0 || len(res.Resources) != tc.resources {
				t.Fatalf("stopped %v with %d resources, want %d:\n%s", res.Stopped, len(res.Resources), tc.resources, texts(firstFive(res.Diagnostics)))
			}
			if grew > heapTarget {
				t.Errorf("the heap grew by %d MiB, above the %d MiB target", grew/mib, heapTarget/mib)
			}
		})
	}
}

// firstFive returns the first five diagnostics of ds, enough to show why a
// generated load failed.
func firstFive(ds diag.List) diag.List { return ds[:min(5, len(ds))] }

// resourceLimitRevision returns the Revision the Node's pipeline renders
// for a Bundle of resources documents, keeping nothing else of the run.
func resourceLimitRevision(t *testing.T, s *suite, resources int) revision.Revision {
	t.Helper()
	res, err := s.node.Run(t.Context(), pipeline.Request{Source: loader.Source{FS: hostileLoad(t, resources, 0, 0), Path: "."}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Validated == nil || len(res.Validated.Bundle.Resources()) != resources {
		t.Fatalf("the %d-resource Bundle has no Revision:\n%s", resources, texts(firstFive(res.Diagnostics)))
	}
	return res.Validated.Revision
}

// TestCanonicalReentryAtResourceLimit checks canonical re-entry at the
// resource limit: the Revision of a 20,000-resource Bundle re-enters on
// the Node's pipeline (stages F to M on its canonical content, as a Node
// loads Last-Known-Good) to its own digest without a finding. It reports
// the peak heap growth beside reentryFactor times the canonical size, the
// configuration model's target for it.
func TestCanonicalReentryAtResourceLimit(t *testing.T) {
	skipHeavy(t)
	s := mustSuite(t)
	rev := resourceLimitRevision(t, s, s.node.Limits().MaxResources)
	var (
		back *pipeline.Result
		err  error
	)
	grew := peakHeap(func() {
		back, err = s.node.FromCanonical(t.Context(), pipeline.CanonicalRequest{Content: rev.Content, Digest: rev.Digest, Name: "content.json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.Validated == nil || back.Validated.Revision.Digest != rev.Digest || len(back.Diagnostics) > 0 {
		t.Fatalf("re-entry reached a Revision %v:\n%s", back.Validated != nil, texts(firstFive(back.Diagnostics)))
	}
	t.Logf("re-entry of %d canonical bytes grew the heap by %d MiB, %.1f times the canonical size (target: %d times)",
		len(rev.Content), grew/mib, float64(grew)/float64(len(rev.Content)), reentryFactor)
}
