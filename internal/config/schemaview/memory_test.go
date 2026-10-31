// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// memoryCap is the diagnostic cap of the memory cases: the bound of what
// the mapping holds is a multiple of the cap, so a small cap shows that
// it does not grow with the errors.
const memoryCap = 10

// memoryBound bounds the steps and interned texts the memo holds at once
// (memo.peak) at cap memoryCap: the growth between two sweeps, at least
// sweepMin, plus what a sweep keeps, the paths of at most 2*(cap+1)
// diagnostics, the bound and 2*(cap+1) pending secret findings at a depth
// of at most 8 with their texts, and the costly steps, a few per
// document here. Without the sweeps the memo held a step and a text per
// error.
const memoryBound = 2*sweepMin + 1024

// pendingBound bounds the pending secret findings (mapper.secretPeak) at
// cap memoryCap: pruneSecrets runs once they reach twice the keep groups.
const pendingBound = 2 * (memoryCap + 1)

// memoryDocs are resources whose n errors would each add a step, an
// interned text or a pending secret finding, by the class of their
// errors: type errors of a list (one location each), type errors in n
// keyed entries (two steps each), typed-map type errors (reported in map
// order), n unknown members of one object (one error, n texts), n
// duplicate set elements and n duplicate keyed entries (the uniqueness
// walk), n reserved label keys, n literal secrets and n secret objects
// with two errors each (pending findings), keyed entries whose errors
// sort before every earlier one (each new error is kept, then pruned),
// and distinct hostnames without source positions, where the path alone
// orders the diagnostics.
func memoryDocs(n int) map[string]string {
	join := func(each func(i int) string) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(each(i))
		}
		return b.String()
	}
	ones := join(func(int) string { return "1" })
	return map[string]string{
		"list type errors": envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": [`+ones+`]}]}`),
		"keyed entry type errors": envelope("Gateway", `{"listeners": [`+join(func(i int) string {
			return `{"name": "l` + strconv.Itoa(i) + `", "protocol": "http", "port": "x"}`
		})+`]}`),
		"typed-map type errors": largeLabels(n),
		"unknown members":       largeRoute(n),
		"duplicate set elements": envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": [`+join(func(int) string {
			return `"h.example"`
		})+`]}]}`),
		"duplicate keyed entries": envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [`+join(func(int) string {
			return `{"name": "u"}`
		})+`]}`),
		"reserved label keys": `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {` + join(func(i int) string {
			return `"ruralz.io/k` + strconv.Itoa(i) + `": "v"`
		}) + `}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
		"literal secrets": envelope("Consumer", `{"credentials": {"apiKeys": [`+join(func(i int) string {
			return `{"name": "k` + strconv.Itoa(i) + `", "secretRef": "literal"}`
		})+`]}}`),
		"secret objects": envelope("Consumer", `{"credentials": {"apiKeys": [`+join(func(i int) string {
			return `{"name": "k` + strconv.Itoa(i) + `", "secretRef": {"provider": "vaults", "name": ""}}`
		})+`]}}`),
		"errors in descending order": siblingListeners(n, 8, 1, false, descending),
		"no source positions":        "stripped:" + envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": [`+join(strconv.Itoa)+`]}]}`),
	}
}

// memoryResource parses a memoryDocs document; one marked "stripped:"
// loses every source position.
func memoryResource(t testing.TB, files *tree.FileTable, src string) *tree.Resource {
	t.Helper()
	body, stripped := strings.CutPrefix(src, "stripped:")
	res := mustResource(t, files, "r.json", body)
	if stripped {
		stripPositions(res)
	}
	return res
}

// TestMappingMemoryBounded checks that what the mapping holds does not
// grow with the validation errors at a fixed cap: for each class of
// memoryDocs at 20,000 and 100,000 errors, and at 1,000,000 type errors
// of one list, the steps and interned texts the memo holds at once
// (memo.peak) stay within memoryBound, the pending secret findings within
// pendingBound, and the result is the first cap+1 of the findings.
// Holding one step and one text per error would cost about 1 KB per error
// at any cap, so a 2 MB JSON document would hold 1 GB. The counts are
// deterministic: no heap is sampled.
func TestMappingMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("validates documents with up to 1,000,000 errors")
	}
	if raceEnabled {
		t.Skip("validates documents with up to 1,000,000 errors; the run without -race checks the counts")
	}
	v := testView(t)
	for _, n := range []int{20_000, 100_000} {
		for name, src := range memoryDocs(n) {
			t.Run(name+"/"+strconv.Itoa(n), func(t *testing.T) {
				checkMemory(t, v, src, n)
			})
		}
	}
	t.Run("list type errors/1000000", func(t *testing.T) {
		checkMemory(t, v, memoryDocs(1_000_000)["list type errors"], 1_000_000)
	})
}

// checkMemory validates src at cap memoryCap and checks the memo's peak,
// the pending secret findings and the size of the result.
func checkMemory(t *testing.T, v *View, src string, n int) {
	t.Helper()
	files := &tree.FileTable{}
	res := memoryResource(t, files, src)
	m := v.check(res, files, memoryCap, Options{})
	if m.memo.peak > memoryBound {
		t.Errorf("%d errors: the memo held %d steps and texts at once, want at most %d", n, m.memo.peak, memoryBound)
	}
	// What the memo holds at the end, counted in the trie and the table
	// themselves, so that a miscounted peak cannot hide them.
	held := len(m.memo.texts)
	if m.memo.top != nil {
		held += countSteps(m.memo.top)
	}
	if held > memoryBound {
		t.Errorf("%d errors: the memo holds %d steps and texts at the end, want at most %d", n, held, memoryBound)
	}
	if m.secretPeak > pendingBound {
		t.Errorf("%d errors: %d secret findings pending at once, want at most %d", n, m.secretPeak, pendingBound)
	}
	if got := m.result(); len(got) != memoryCap+1 {
		t.Errorf("%d errors: %d diagnostics, want the cap plus one (%d)", n, len(got), memoryCap+1)
	}
	t.Logf("%d errors: memo peak %d, %d sweeps, %d secret findings pending at most", n, m.memo.peak, m.memo.sweeps, m.secretPeak)
}

// forced validates res with sweeps as frequent as the memo allows
// (memo.minSweep 1) and returns the mapper.
func forced(v *View, res *tree.Resource, files *tree.FileTable, limit int) *mapper {
	m := newMapper(v, res, files, limit, Options{})
	m.memo.minSweep = 1
	m.run()
	return m
}

// TestSweepKeepsResults checks that sweeping the memo never changes a
// result: with a sweep at nearly every new step and text, every
// document of memoryDocs, the yield and span cases, the long sibling
// segments, the scale cases, the keyed zeros and the error-mapping golden
// inputs, with and without source positions, at the default cap and at
// caps 1 and 10, returns what Validate returns without them, so a path
// built again after its text was dropped interns consistently (sweep.go).
func TestSweepKeepsResults(t *testing.T) {
	if raceEnabled {
		t.Skip("validates every scale document twice; the run without -race checks it")
	}
	v := testView(t)
	docs := map[string]string{}
	for name, src := range memoryDocs(300) {
		docs["memory "+name] = src
	}
	for name, src := range yieldDocs() {
		docs["yield "+name] = src
	}
	for name, src := range spanDocs(300) {
		docs["span "+name] = src
	}
	for _, tc := range scaleCases() {
		docs["scale "+tc.name] = tc.src
	}
	for _, tc := range keyedZeros() {
		docs["keyed zero "+tc.name] = tc.src
	}
	docs["long sibling keys"] = siblingListeners(64, 300, 10, false, permuted)
	docs["long quoted hostnames"] = siblingHostnames(64, 300, true)
	docs["long label keys"] = siblingLabels(64, 300, false)
	docs["long set elements"] = siblingSubjects(16, 300, 20, permuted)
	docs["large set element"] = largeSetElement(300)
	docs["large keyed entry"] = largeKeyedEntry(300)
	inputs, err := filepath.Glob(filepath.Join("testdata", "mapping", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range inputs {
		if strings.HasSuffix(in, ".diag.json") {
			continue
		}
		data, err := os.ReadFile(in) //nolint:gosec // G304: a golden input under testdata.
		if err != nil {
			t.Fatal(err)
		}
		docs["golden "+filepath.Base(in)] = string(data)
	}
	sweeps := 0
	for name, src := range docs {
		for _, stripped := range []bool{false, true} {
			for _, limit := range []int{MaxDiagnostics, 10, 1} {
				label := name + "/cap " + strconv.Itoa(limit)
				if stripped {
					label += "/no positions"
				}
				t.Run(label, func(t *testing.T) {
					files := &tree.FileTable{}
					res := memoryResource(t, files, src)
					if stripped {
						stripPositions(res)
					}
					want := texts(v.check(res, files, limit, Options{}).result())
					m := forced(v, res, files, limit)
					if got := texts(m.result()); got != want {
						t.Errorf("with sweeps:\n%s\nwant\n%s", got, want)
					}
					sweeps += m.memo.sweeps
				})
			}
		}
	}
	if sweeps == 0 {
		t.Error("no case swept the memo")
	}
}

// TestCostlyStepsKept checks that a step whose resolution read at least
// keepCost members, elements and bytes is marked costly and kept by every
// sweep although no diagnostic lies below it: a Policy spec of n members
// (its dispatch reads them all), a listener with n members before its key
// (its key-aware element scans them) and a set element with n members
// (its element is its canonical JSON). After sweeps that pin other paths,
// resolving a location below it again finds the same step and reads none
// of its members; a cheap step beside it is dropped and resolved again.
// Dropping a costly step would resolve it again after each sweep, reading
// the large object once per error (step.costly).
func TestCostlyStepsKept(t *testing.T) {
	const n = 2000
	v := testView(t)
	for _, tc := range []struct {
		name, src string
		// costly is the location of the costly step, below a location
		// resolved through it, cheap one of a cheap step.
		costly, below, cheap []string
	}{
		{"dispatch", largePolicy(n), []string{"spec"}, []string{"spec", "config", "allow", "7"}, []string{"metadata", "name"}},
		{"keyed entry", largeListener(n), []string{"spec", "listeners", "0"}, []string{"spec", "listeners", "0", "hostnames", "7"}, []string{"metadata", "name"}},
		{"set element", largeSetElement(n), []string{"spec", "config", "subjects", "0"}, []string{"spec", "config", "subjects", "0", "u7"}, []string{"metadata", "name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", tc.src)
			m := newMapper(v, res, nil, MaxDiagnostics, Options{})
			m.memo.minSweep = 1
			m.locate(tc.below)
			m.locate(tc.cheap)
			costly, cheap := findStep(m.memo.top, tc.costly), findStep(m.memo.top, tc.cheap)
			if !costly.costly || cheap.costly {
				t.Fatalf("costly = %v and %v, want a costly step and a cheap one", costly.costly, cheap.costly)
			}
			// Resolve locations elsewhere until the memo has swept.
			for i := 0; m.memo.sweeps < 3 && i < 1000; i++ {
				m.locate([]string{"metadata", "labels", "k" + strconv.Itoa(i)})
			}
			if m.memo.sweeps < 3 {
				t.Fatalf("%d sweeps after 1000 new steps, want 3", m.memo.sweeps)
			}
			read := m.memo.read
			m.locate(tc.below)
			if findStep(m.memo.top, tc.costly) != costly {
				t.Error("the costly step was dropped and resolved again")
			}
			if again := m.memo.read - read; again >= n {
				t.Errorf("resolving below the costly step again read %d members and elements, want fewer than its %d", again, n)
			}
			m.locate(tc.cheap)
			if findStep(m.memo.top, tc.cheap) == cheap {
				t.Error("the cheap step was kept by the sweeps")
			}
		})
	}
}

// TestCausesReleased checks that mapping releases each validation error
// once it is mapped (mapper.visitCauses): when the last error of a
// resource with many is visited, only that error's own ancestors are
// still reachable from the root, so the validator's error tree is freed
// while it is mapped, and the stage's peak is the larger of the tree and
// the mapping, not their sum. An observer of the work units sees the tree
// at each error visited; nothing is sampled from the heap.
func TestCausesReleased(t *testing.T) {
	v := testView(t)
	for name, src := range map[string]string{
		"list type errors": memoryDocs(200)["list type errors"],
		"secret objects":   memoryDocs(200)["secret objects"],
		"unknown members":  largeListener(200),
	} {
		t.Run(name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", src)
			verr := validationError(t, v, res)
			total := countErrors(verr)
			m := newMapper(v, res, nil, memoryCap, Options{Yield: func() {}})
			var reachable []int
			m.work.observe = func(int) { reachable = append(reachable, countErrors(verr)) }
			m.visit(verr)
			if len(reachable) == 0 {
				t.Fatal("no work observed")
			}
			if total < 100 {
				t.Fatalf("validation reported %d errors, want at least 100", total)
			}
			// The last observation is made while the last error is
			// visited: only it and its ancestors are reachable, every
			// earlier error is released.
			if last := reachable[len(reachable)-1]; last > 16 {
				t.Errorf("%d of %d errors reachable while the last one is mapped, want at most its ancestors", last, total)
			}
			if after := countErrors(verr); after != 1 {
				t.Errorf("%d of %d errors reachable after mapping, want the root alone", after, total)
			}
		})
	}
}

// TestSecretFindingsPruned checks that pending secret findings are pruned
// to the first cap+1 groups of equal location and path while the result
// stays the first cap+1 of every finding: literal secrets and secret
// objects with two errors each, in source order and without source
// positions (the path alone orders them), and pairs of keyed entries with
// one key whose secret fields share a location and a path, a group of two
// that the prune keeps whole. In the last case the pairs' keys keep their
// positions, so the duplicate-key findings sort after the secrets, and
// caps of 2 and 4 put a pair across the cap: the object sorts after the
// literal of its pair but is reported first, so a prune that kept the
// first cap+1 findings rather than groups would keep the object and drop
// the literal the result needs.
func TestSecretFindingsPruned(t *testing.T) {
	v := testView(t)
	pairs := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			name := strconv.Itoa(i)
			b.WriteString(`{"name": "k` + name + `", "secretRef": {"provider": "vaults", "name": ""}}, {"name": "k` + name + `", "secretRef": "literal"}`)
		}
		return envelope("Consumer", `{"credentials": {"apiKeys": [`+b.String()+`]}}`)
	}
	// atList strips the positions of the apiKeys entries and of
	// everything below them but their keys: each secret field's finding
	// points at the list.
	atList := func(res *tree.Resource) {
		keys, _ := res.Root.At(diag.Path{diag.Field("spec"), diag.Field("credentials"), diag.Field("apiKeys")})
		for _, it := range keys.Items {
			it.Pos = tree.Pos{}
			for i := range it.Members {
				if it.Members[i].Key == "secretRef" {
					it.Members[i].KeyPos = tree.Pos{}
					stripPositions(&tree.Resource{Root: it.Members[i].Value})
				}
			}
		}
	}
	docs := memoryDocs(500)
	for _, tc := range []struct {
		name  string
		src   string
		strip func(res *tree.Resource)
		caps  []int
		// secretsFirst is whether the first cap+1 findings are secret
		// findings.
		secretsFirst bool
	}{
		{"literal secrets", docs["literal secrets"], nil, []int{1, 3, memoryCap}, true},
		{"secret objects", docs["secret objects"], nil, []int{1, 3, memoryCap}, true},
		{"literal secrets without positions", docs["literal secrets"], stripPositions, []int{1, 3, memoryCap}, true},
		{"secret objects without positions", docs["secret objects"], stripPositions, []int{1, 3, memoryCap}, true},
		{"keyed pairs with one key", pairs(250), nil, []int{1, 3, memoryCap}, false},
		{"keyed pairs with one key without positions", pairs(250), stripPositions, []int{1, 3, memoryCap}, false},
		{"keyed pairs with one key at the list", pairs(250), atList, []int{2, 4, memoryCap}, true},
	} {
		for _, limit := range tc.caps {
			t.Run(tc.name+"/cap "+strconv.Itoa(limit), func(t *testing.T) {
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", tc.src)
				if tc.strip != nil {
					tc.strip(res)
				}
				all := v.check(res, files, 1<<30, Options{}).result()
				if len(all) < 2*limit+2 {
					t.Fatalf("Validate() returned %d diagnostics, want more than twice the cap", len(all))
				}
				if tc.secretsFirst && all[limit].Code != CodeLiteral {
					t.Fatalf("the first %d diagnostics are not all secret findings:\n%s", limit+1, texts(all[:limit+1]))
				}
				m := v.check(res, files, limit, Options{})
				if got := m.result(); !slices.EqualFunc(got, all[:limit+1], sameDiagnostic) {
					t.Errorf("check(cap %d) =\n%s\nwant the first %d of the full result:\n%s", limit, texts(got), limit+1, texts(all[:limit+1]))
				}
				// A group holds at most two findings here.
				if bound := 2 * 2 * (limit + 1); m.secretPeak > bound {
					t.Errorf("%d secret findings pending at once, want at most %d", m.secretPeak, bound)
				}
				if m.secretHeld == 0 {
					t.Error("no secret finding was pruned")
				}
			})
		}
	}
}

// TestSweep checks one sweep of a memo: it keeps the pinned path, the
// paths the mapper holds (memo.roots) and everything above them, the
// costly steps and the steps above any kept step, and the texts and
// common prefixes between kept texts; it drops every other step, text
// and remembered prefix; a dropped path built again gets a new, interned
// text node while a kept one resolves to the same node; and it counts
// what it kept.
func TestSweep(t *testing.T) {
	v := testView(t)
	res := mustResource(t, &tree.FileTable{}, "r.json", envelope("Gateway", `{"listeners": [{"name": "a", "port": "x", "protocol": "http", "hostnames": [1, 2, 3]}, {"name": "b", "port": "y", "protocol": "http"}]}`))
	m := newMapper(v, res, nil, MaxDiagnostics, Options{})
	hosts := func(i string) []string { return []string{"spec", "listeners", "0", "hostnames", i} }
	held := m.locate([]string{"spec", "listeners", "0", "port"})
	pinned := m.locate([]string{"spec", "listeners", "1", "port"})
	dropped := m.locate(hosts("2"))
	m.locate(hosts("1"))
	costly := m.locate(hosts("0"))
	findStep(m.memo.top, hosts("0")).costly = true
	m.emit(CodeSchema, held, held.pos, "held", "")
	// Two long sibling texts compared, the first held by a diagnostic.
	long := strings.Repeat("a", lcpMin)
	keptLong := m.memo.child(nil, diag.Field(long+"1"))
	droppedLong := m.memo.child(nil, diag.Field(long+"2"))
	m.emit(CodeUnknownField, position{path: keptLong}, tree.Pos{}, "held", "")
	a, b := keptLong.textOf(), droppedLong.textOf()
	m.memo.commonPrefix(a, b)
	m.memo.lcp = map[textPair]int{{a, b}: len(long)}
	if a.lcp.peer != b {
		t.Fatal("commonPrefix() did not remember the pair")
	}
	m.memo.sweep(pinned.path)
	for _, tc := range []struct {
		name string
		loc  []string
		kept bool
	}{
		{"held", []string{"spec", "listeners", "0", "port"}, true},
		{"pinned", []string{"spec", "listeners", "1", "port"}, true},
		{"above the held", []string{"spec", "listeners", "0"}, true},
		{"costly", hosts("0"), true},
		{"above the costly", hosts("")[:4], true},
		{"dropped", hosts("2"), false},
		{"dropped sibling", hosts("1"), false},
	} {
		if got := findStep(m.memo.top, tc.loc) != nil; got != tc.kept {
			t.Errorf("%s: step kept = %v, want %v", tc.name, got, tc.kept)
		}
	}
	if kept := countSteps(m.memo.top) + len(m.memo.texts); m.memo.kept != kept || m.memo.grown != 0 {
		t.Errorf("sweep() counted %d kept and %d grown, want %d and 0", m.memo.kept, m.memo.grown, kept)
	}
	if len(m.memo.lcp) != 0 || a.lcp.peer != nil {
		t.Errorf("the prefix with a dropped text is still remembered: %d pairs, peer %p", len(m.memo.lcp), a.lcp.peer)
	}
	if at := (textAt{text: a.text}); m.memo.texts[at] != a {
		t.Error("the held long text was dropped")
	}
	if at := (textAt{text: b.text}); m.memo.texts[at] != nil {
		t.Error("the long text no path holds was kept")
	}
	if again := m.locate([]string{"spec", "listeners", "0", "port"}); again.path != held.path {
		t.Error("the held path was resolved again to a new node")
	}
	if again := m.locate(hosts("0")); again.path != costly.path {
		t.Error("the costly step was resolved again to a new node")
	}
	again := m.locate(hosts("2"))
	if again.path == dropped.path || again.path.textOf() == dropped.path.textOf() {
		t.Error("the dropped path resolved to its old node")
	}
	if at := (textAt{parent: again.path.parent.textOf(), text: again.path.textOf().text}); m.memo.texts[at] != again.path.textOf() {
		t.Error("the path built again is not interned")
	}
}

// findStep returns the step at loc below top in the trie, or nil.
func findStep(top *step, loc []string) *step {
	s := top
	for _, tok := range loc {
		if s == nil {
			return nil
		}
		c, _, _, ok := s.find(tok)
		if !ok {
			return nil
		}
		s = c
	}
	return s
}
