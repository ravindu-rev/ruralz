// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// parseYAML parses YAML text with default options.
func parseYAML(t testing.TB, src string) ([]Document, diag.List) {
	t.Helper()
	docs, diags, err := Parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return docs, diags
}

// parseJSON parses JSON text with default options.
func parseJSON(t testing.TB, src string) ([]Document, diag.List) {
	t.Helper()
	docs, diags, err := Parse(context.Background(), []byte(src), 0, "f.json", FormatJSON, Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return docs, diags
}

// show renders a tree compactly: {k:v,...}, [a,...], n, b:true, i:7,
// f:0.5, s:"x". With pos, every key and value carries @line:column.
func show(n *tree.Node, pos bool) string {
	var b strings.Builder
	shower{pos: pos}.write(&b, n)
	return b.String()
}

// showValues renders a tree like show without positions, writing every
// number as its exact value (num:1/2), so 1, 1.0 and 1e0 render alike.
func showValues(n *tree.Node) string {
	var b strings.Builder
	shower{values: true}.write(&b, n)
	return b.String()
}

// shower renders trees for show and showValues.
type shower struct {
	pos, values bool
}

func (sh shower) write(b *strings.Builder, n *tree.Node) {
	if n == nil {
		b.WriteString("<nil>")
		return
	}
	at := func(p tree.Pos) {
		if sh.pos {
			fmt.Fprintf(b, "@%d:%d", p.Line, p.Column)
		}
	}
	switch n.Kind {
	case tree.KindMap:
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(m.Key)
			at(m.KeyPos)
			b.WriteByte(':')
			sh.write(b, m.Value)
		}
		b.WriteByte('}')
	case tree.KindList:
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			sh.write(b, it)
		}
		b.WriteByte(']')
	case tree.KindNull:
		b.WriteString("n")
	case tree.KindBool:
		b.WriteString("b:" + strconv.FormatBool(n.Bool))
	case tree.KindInt, tree.KindFloat:
		if r, ok := new(big.Rat).SetString(n.Text); sh.values && ok {
			b.WriteString("num:" + r.RatString())
		} else if n.Kind == tree.KindInt {
			b.WriteString("i:" + n.Text)
		} else {
			b.WriteString("f:" + n.Text)
		}
	case tree.KindString:
		b.WriteString("s:" + strconv.Quote(n.Text))
	}
	if n.Kind != tree.KindMap && n.Kind != tree.KindList {
		at(n.Pos)
	}
}

// codes lists the diagnostics as "code@line:column".
func codes(l diag.List) string {
	var parts []string
	for _, d := range l {
		parts = append(parts, fmt.Sprintf("%s@%d:%d", d.Code, d.Line, d.Column))
	}
	return strings.Join(parts, " ")
}

// wantOne asserts exactly one diagnostic with code at line:column and
// returns it.
func wantOne(t *testing.T, l diag.List, code string, line, col int) diag.Diagnostic {
	t.Helper()
	if len(l) != 1 || l[0].Code != code || l[0].Line != line || l[0].Column != col {
		t.Fatalf("diagnostics = %q, want one %s@%d:%d", codes(l), code, line, col)
	}
	if l[0].Severity != diag.SeverityError || l[0].File == "" {
		t.Fatalf("diagnostic %+v: want an error with a file", l[0])
	}
	return l[0]
}

// suiteDataDir holds the vendored YAML Test Suite cases.
const suiteDataDir = "../../../test/fixtures/yaml-test-suite/data"

// suiteInputs returns the in.yaml text of every YAML Test Suite case.
func suiteInputs(t testing.TB) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(suiteDataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "in.yaml" {
			data, err := os.ReadFile(path) //nolint:gosec // G304: a file under the vendored suite directory
			if err != nil {
				return err
			}
			out = append(out, string(data))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the YAML Test Suite: %v", err)
	}
	return out
}

// allocBytes returns the bytes allocated by the process so far. A P adds
// the objects it allocates from a cached span to the count only when the
// span leaves its cache, so the value lags by up to the cached spans,
// hundreds of KiB: it suits bounds with MiB of slack, and allocatedBy
// measures smaller amounts.
func allocBytes() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// allocatedBy returns the bytes allocated while f runs, counted exactly.
// Three things beside f would land in the count, and none does:
//
//   - a P adds the objects it allocates from a cached span only when the
//     span leaves its cache, so runtime.GC flushes every P's cached spans
//     into the count before and after f;
//   - the process's first metrics.Read builds the runtime's table of
//     metrics, about 19 KB on Go 1.26, and each Read needs its sample, so
//     the sample is allocated and read once before the first runtime.GC,
//     and the reads around f allocate nothing;
//   - on many Ps, a collection's mark workers finish together and queue on
//     the runtime's mark-done semaphore, and each worker that waits
//     allocates a 112-byte sudog when its P has none cached, so the
//     measurement runs at GOMAXPROCS 1, as testing.AllocsPerRun does.
//
// The package's tests run one at a time, so f is then all that allocates
// in between.
func allocatedBy(f func()) uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	runtime.GC()
	metrics.Read(s)
	before := s[0].Value.Uint64()
	f()
	runtime.GC()
	metrics.Read(s)
	return s[0].Value.Uint64() - before
}

// fuzzing reports whether the test binary runs the fuzzing engine (go
// test -fuzz), where a wall-clock bound catches a parse that hangs; the
// seed corpus that a plain go test runs is checked without one.
func fuzzing() bool {
	f := flag.Lookup("test.fuzz")
	return f != nil && f.Value.String() != ""
}

// TestAllocatedByIsExact checks the measure the linearity tests compare:
// a known number of small allocations is counted in full, one or many,
// where the unflushed counter reports none until a span leaves its
// cache.
func TestAllocatedByIsExact(t *testing.T) {
	for _, n := range []uint64{1, 100, 10000} {
		keep := make([][]byte, n)
		got := allocatedBy(func() {
			for i := range keep {
				keep[i] = make([]byte, 64)
			}
		})
		runtime.KeepAlive(keep)
		if want := 64 * n; got < want || got > want+16<<10 {
			t.Errorf("allocatedBy(%d allocations of 64 bytes) = %d, want %d to %d", n, got, want, want+16<<10)
		}
	}
}

// TestAllocatedByCountsOnlyF checks that the measure adds nothing of its
// own: no sample, no metrics table and no mark worker on another P
// reaches the count, so a function that allocates nothing measures 0 and
// a tiny allocation measures its size class. A stray runtime allocation,
// such as the scavenger growing its timer heap, may still land in one
// measurement in hundreds, so the smallest of three is checked; a measure
// that counts its own sample never reaches 0.
func TestAllocatedByCountsOnlyF(t *testing.T) {
	smallest := func(f func()) uint64 {
		low := allocatedBy(f)
		for range 2 {
			low = min(low, allocatedBy(f))
		}
		return low
	}
	if got := smallest(func() {}); got != 0 {
		t.Errorf("allocatedBy(nothing) = %d, want 0", got)
	}
	var keep []byte
	if got := smallest(func() { keep = make([]byte, 48) }); got != 48 {
		t.Errorf("allocatedBy(one 48-byte allocation) = %d, want 48", got)
	}
	runtime.KeepAlive(keep)
}

func itoa(i int) string { return strconv.Itoa(i) }

// peakHeap runs f and returns the largest growth of live heap objects and
// goroutine stacks over their size before f, sampling
// /memory/classes/heap/objects:bytes (the measure heapBudget bounds) and
// /memory/classes/heap/stacks:bytes every millisecond: a parser that
// recurses once per node spends its memory on the stack, not in heap
// objects.
func peakHeap(f func()) uint64 {
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/heap/stacks:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64() + s[1].Value.Uint64()
	}
	runtime.GC()
	base := read()
	peak := base
	stop := make(chan struct{})
	done := make(chan struct{})
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
	peak = max(peak, read())
	return peak - base
}

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }

// nest returns YAML nested depth levels deep in the given style.
func nest(style string, depth int) string {
	var b strings.Builder
	switch style {
	case "block map":
		for i := range depth {
			b.WriteString(strings.Repeat("  ", i) + "k" + strconv.Itoa(i) + ":\n")
		}
		b.WriteString(strings.Repeat("  ", depth) + "leaf\n")
		// Replace the deepest key's empty value with a scalar.
		s := b.String()
		return strings.TrimSuffix(s[:strings.LastIndex(s, "\n"+strings.Repeat("  ", depth)+"leaf")], ":") + ": leaf\n"
	case "block seq":
		for i := range depth {
			b.WriteString(strings.Repeat("  ", i) + "-\n")
		}
		b.WriteString(strings.Repeat("  ", depth) + "leaf\n")
		return b.String()
	case "compact seq":
		b.WriteString(strings.Repeat("- ", depth) + "leaf\n")
		return b.String()
	case "flow":
		return "a: " + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + "\n"
	case "mixed":
		// map > seq > map > seq ... in block style, then flow.
		ind := 0
		for i := 0; i < depth; i++ {
			if i%2 == 0 {
				b.WriteString(strings.Repeat(" ", ind) + "k:\n")
			} else {
				b.WriteString(strings.Repeat(" ", ind) + "-\n")
			}
			ind += 2
		}
		b.WriteString(strings.Repeat(" ", ind) + "leaf\n")
		return b.String()
	}
	panic(style)
}

// parseWith parses YAML text with default options, keeping documents
// whose root is not a mapping, and returns the documents and the findings
// as codes.
func parseWith(t testing.TB, src string) ([]Document, string) {
	t.Helper()
	docs, diags, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, true)
	if err != nil {
		t.Fatal(err)
	}
	return docs, codes(diags)
}

// mustRoot parses YAML text that must give one document without findings
// and returns its root.
func mustRoot(t *testing.T, src string) *tree.Node {
	t.Helper()
	docs, diags := parseYAML(t, src)
	if len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("%q: %q", src, codes(diags))
	}
	return docs[0].Root
}
