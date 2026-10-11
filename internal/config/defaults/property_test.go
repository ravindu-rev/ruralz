// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// permute reorders, below n, every object's members and the elements of
// every set and map list (whose order carries no meaning), and leaves
// orderedMap, atomic and unannotated lists alone. s is n's schema.
func permute(r *rand.Rand, n *tree.Node, s *schemaidx.Node) {
	switch n.Kind {
	case tree.KindMap:
		sel := s.Select(n)
		if sel.Def() == defJSONSchemaDocument {
			sel = nil
		}
		r.Shuffle(len(n.Members), func(i, j int) { n.Members[i], n.Members[j] = n.Members[j], n.Members[i] })
		for _, m := range n.Members {
			permute(r, m.Value, memberSchema(sel, m.Key))
		}
	case tree.KindList:
		if lt, _ := s.List(); lt == schemaidx.ListSet || lt == schemaidx.ListMap {
			r.Shuffle(len(n.Items), func(i, j int) { n.Items[i], n.Items[j] = n.Items[j], n.Items[i] })
		}
		it := itemSchema(s)
		for _, item := range n.Items {
			permute(r, item, it)
		}
	default:
	}
}

// TestReorderInvariance is 01 property P1 and 02 property 27 at stage G:
// permuting object members and the entries of set and map lists, and the
// order of resources, yields the same hub trees, so the canonical form and
// the digest cannot depend on authored order.
func TestReorderInvariance(t *testing.T) {
	f, rs := loadBundle(t)
	s := newStage(t)
	b, ds, err := s.Run(t.Context(), rs, f.files, 2)
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run = %q, %v", texts(ds), err)
	}
	want := bundleText(b)
	idx := hubIndex(t)
	for seed := range uint64(20) {
		r := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // G404: seeded permutations make the property reproducible.
		g, again := loadBundle(t)
		for _, res := range again {
			k, _ := idx.Resource(string(res.ID.Kind))
			permute(r, res.Root, k)
		}
		r.Shuffle(len(again), func(i, j int) { again[i], again[j] = again[j], again[i] })
		b2, ds, err := s.Run(t.Context(), again, g.files, 3)
		if err != nil || len(ds) != 0 {
			t.Fatalf("seed %d: Run = %q, %v", seed, texts(ds), err)
		}
		if got := bundleText(b2); got != want {
			t.Errorf("seed %d: permuted Bundle differs\n%s\nwant\n%s", seed, got, want)
		}
	}
}

// FuzzStageIdempotent is 01 property P6 and 02 property 20 with the
// stage's robustness: any resource document stage G accepts holds no null
// (02 req 19) and is a fixed point of stage G, materialization is
// idempotent on its own output, and no input panics. The input is a JSON
// resource envelope.
func FuzzStageIdempotent(f *testing.F) {
	for _, seed := range []string{
		route(`{"match": {"hosts": ["B.example", "a.example"]}, "timeout": "90s", "upstreams": [{"name": "b", "weight": 2.0}, {"name": "a"}]}`),
		gateway(`{"listeners": [{"name": "h", "port": 80, "protocol": "http"}], "limits": {"maxRequestBodyBytes": "10Mi"}, "stateStore": {}}`),
		policyDoc("validation.json-schema", `{"schema": {"enum": [2, 1, null], "b": {"z": 1e2, "a": 2}}}`),
		policyDoc("quota", `{"consumerQuota": "c", "x": [1e999]}`),
		policyDoc("quota", `{"consumerQuota": "c", "x": {"y": null}}`),
		policyDoc("plugin", `{"x": [null]}`),
		policyDoc("ratelimit", `{"limits": [{"requests": 1, "window": "1x"}]}`),
		`{"apiVersion": "ruralz/v1alpha1", "kind": "AIProvider", "metadata": {"name": "p"}, "spec": {"dialect": "openai", "pricing": {"models": [{"model": "m", "inputPerMillionTokens": "03.50"}]}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {}, "tags": ["b", "a", "b"]}}`,
		`[]`,
	} {
		f.Add(seed)
	}
	s := newStage(f)
	idx := hubIndex(f)
	reg := registry.New()
	f.Fuzz(func(t *testing.T, src string) {
		root, err := parse(src, 0)
		if err != nil {
			t.Skip()
		}
		res := envelope(root)
		if res.ID.Kind == "" || res.APIVersion == "" {
			res.ID.Kind, res.APIVersion = "Route", v1
		}
		h, ds := s.Resource(res, nil)
		for _, d := range ds {
			if d.Code != CodeSchema && d.Code != CodeUnserved {
				t.Fatalf("unexpected code %s", d.Code)
			}
		}
		if h == nil {
			return
		}
		if p, ok := nullAt(h.Tree, ""); ok {
			t.Fatalf("accepted a null at %s: %s", p, dump(h.Tree))
		}
		once := dump(h.Tree)
		if n := Materialize(idx, reg, &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree}); n != 0 {
			t.Fatalf("Materialize added %d members to its own output %s", n, once)
		}
		again := envelope(mustParse(t, once))
		again.ID, again.APIVersion = h.ID, v1
		h2, ds := s.Resource(again, nil)
		if h2 == nil || len(ds) != 0 {
			t.Fatalf("stage G rejected its own output %s: %q", once, texts(ds))
		}
		if got := dump(h2.Tree); got != once {
			t.Fatalf("stage G changed its own output\n%s\nwant\n%s", got, once)
		}
	})
}

// nullAt returns the dotted path of the first null below n.
func nullAt(n *tree.Node, path string) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.Kind == tree.KindNull {
		return path, true
	}
	for _, m := range n.Members {
		if p, ok := nullAt(m.Value, path+"."+m.Key); ok {
			return p, true
		}
	}
	for i, it := range n.Items {
		if p, ok := nullAt(it, path+"["+strconv.Itoa(i)+"]"); ok {
			return p, true
		}
	}
	return "", false
}

// FuzzIntegerText checks integerText against the definition: when it
// reports an integer, the literal and the integer text are equal numbers,
// and a short integral literal is always recognized.
func FuzzIntegerText(f *testing.F) {
	for _, seed := range []string{"1", "1.0", "1e3", "-0.0", "1.5", "100e-2", "1e16", "9007199254740991.000", "0e999999999999999999999", "1e-999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		text, ok, tooLarge := integerText(s)
		if ok && tooLarge {
			t.Fatalf("integerText(%q) both ok and too large", s)
		}
		if !ok {
			return
		}
		if text == "" || strings.HasPrefix(text, "-0") || (len(text) > 1 && text[0] == '0') {
			t.Fatalf("integerText(%q) = %q, not normalized integer text", s, text)
		}
		if n := len(strings.TrimPrefix(text, "-")); n > maxIntegerDigits {
			t.Fatalf("integerText(%q) = %q, more digits than allowed", s, text)
		}
		if strings.Trim(text, "-0123456789") != "" {
			t.Fatalf("integerText(%q) = %q", s, text)
		}
		// Both name the same real number, so they round to one double.
		a, errA := strconv.ParseFloat(s, 64)
		b, errB := strconv.ParseFloat(text, 64)
		if errA == nil && errB == nil && a != b {
			t.Fatalf("integerText(%q) = %q: %v != %v", s, text, a, b)
		}
	})
}
