// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// orderSchema reads the corpus schema of the 1 KiB budget.
func orderSchema(t testing.TB) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "schemas", "order.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// order1KiB returns a valid order body of about 1 KiB.
func order1KiB() []byte {
	var b strings.Builder
	b.WriteString(`{"id":"ord_0123abcd","customer":{"email":"buyer@example.com","name":"A. Buyer"},"items":[`)
	for i := range 12 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"sku":"SKU-%05d-BLUE","qty":%d,"price":%d.99}`, i, i+1, 10+i)
	}
	b.WriteString(`],"total":1234.56,"notes":"leave at the door, ring twice","status":"paid"}`)
	return []byte(b.String())
}

// BenchmarkValidate1KiB is the 07 req 84 budget: onRequestBody with
// validation.json-schema on a 1 KiB body (25 µs p50, 80 µs p99, target).
// Decoder and library allocations are not Ruralz-owned (07 section 9 item
// 20); TestHandleAddsNoAllocations checks the Filter adds none.
func BenchmarkValidate1KiB(b *testing.B) {
	f := buildFilter(b, orderSchema(b), testLimit)
	body := order1KiB()
	x, _ := exchange("application/json", body)
	ctx := b.Context()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if r := f.Handle(ctx, phase.OnRequestBody, x); r.Outcome != filter.Continue {
			b.Fatalf("Handle = %+v", r)
		}
	}
}

// BenchmarkValidate1KiBMismatch measures the refusal path, which builds
// the detail.
func BenchmarkValidate1KiBMismatch(b *testing.B) {
	f := buildFilter(b, orderSchema(b), testLimit)
	body := []byte(strings.Replace(string(order1KiB()), `"qty":12`, `"qty":"12"`, 1))
	x, _ := exchange("application/json", body)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if r := f.Handle(ctx, phase.OnRequestBody, x); r.Outcome != filter.Respond {
			b.Fatalf("Handle = %+v", r)
		}
	}
}

// BenchmarkValidateNested validates a body nested 60 deep against a
// recursive schema the evaluation bound accepts; its cost grows linearly
// with the nesting.
func BenchmarkValidateNested(b *testing.B) {
	f := buildFilter(b, linearTree, testLimit)
	x, _ := exchange("application/json", []byte(nested(60)))
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if r := f.Handle(ctx, phase.OnRequestBody, x); r.Outcome != filter.Continue {
			b.Fatalf("Handle = %+v", r)
		}
	}
}

// TestHandleAddsNoAllocations checks the 07 req 84 allocation target for
// Ruralz code: Handle allocates nothing beyond the strict decode and the
// library's validation.
func TestHandleAddsNoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under -race")
	}
	f := buildFilter(t, orderSchema(t), testLimit)
	body := order1KiB()
	x, _ := exchange("application/json", body)
	ctx := t.Context()
	handleAllocs := testing.AllocsPerRun(200, func() { f.Handle(ctx, phase.OnRequestBody, x) })
	baseAllocs := testing.AllocsPerRun(200, func() {
		v, _, _ := f.decode(body)
		f.schema.Validate(v)
	})
	if handleAllocs > baseAllocs {
		t.Errorf("Handle allocates %.0f, decode and validate %.0f: the Filter adds allocations", handleAllocs, baseAllocs)
	}
}

// schema256KiB returns a schema of about 256 KiB in canonical form
// within MaxSubschemas: 340 documented object properties of four
// subschemas each, sharing one definition.
func schema256KiB() string {
	var b strings.Builder
	b.WriteString(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
		`"$defs":{"money":{"type":"string","pattern":"^[0-9]+\\.[0-9]{2}$"}},"properties":{`)
	desc := strings.Repeat("Describes the field for API consumers and documentation. ", 10)
	for i := 0; b.Len() < MaxSchemaBytes-900; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field_%04d":{"description":%q,"type":"object","required":["name"],"properties":{"name":{"type":"string","maxLength":64},"amount":{"$ref":"#/$defs/money"},"count":{"type":"integer","minimum":0}}}`, i, desc)
	}
	b.WriteString(`}}`)
	return b.String()
}

// BenchmarkCompileSchema256KiB is 07's compile benchmark for a schema at
// the size bound (one compile per Policy per snapshot, carried over).
func BenchmarkCompileSchema256KiB(b *testing.B) {
	raw := []byte(schema256KiB())
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CompileSchema(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompileSchemaWorstCase compiles MaxSubschemas positions that
// each refer back to the root, the quadratic case of the library.
func BenchmarkCompileSchemaWorstCase(b *testing.B) {
	raw := []byte(refsToRoot(MaxSubschemas - 1))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CompileSchema(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// refsToRoot returns a schema with n properties that each refer to the
// root.
func refsToRoot(n int) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%x":{"$ref":"#"}`, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// analysisWorstCase returns a schema near MaxSubschemas that makes the
// evaluation analysis do the most work: props properties that refer to the
// root, pats patterns (past maxPatternMatches pairs every pattern counts at
// every member), and a linear-growth chain that keeps all 64 levels
// running. With mixins the coarse pass crosses the bound and the precise
// pass decides, building a class per member name at every level.
func analysisWorstCase(props, pats int, mixins bool) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range props {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"p%d":{"$ref":"#"}`, i)
	}
	b.WriteString(`},"patternProperties":{`)
	for i := range pats {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"^q%d$":true`, i)
	}
	b.WriteString(`},"items":{"$ref":"#"},"contains":{"$ref":"#/$defs/s"},"$defs":{"s":{"items":{"$ref":"#/$defs/s"}}`)
	if mixins {
		b.WriteString(`,"base":{"properties":{"owner":{"$ref":"#/$defs/person"}}},"person":{"properties":{"pets":{"items":{"$ref":"#"}}}}},` +
			`"allOf":[{"$ref":"#/$defs/base"},{"properties":{"friends":{"items":{"$ref":"#"}}}}]}`)
	} else {
		b.WriteString(`}}`)
	}
	return b.String()
}

// analysisCases are the worst cases of the evaluation analysis.
func analysisCases() []struct {
	name        string
	props, pats int
	mixins      bool
} {
	return []struct {
		name        string
		props, pats int
		mixins      bool
	}{
		{"patterns coarse", 2000, 2000, false},
		{"patterns precise", 2000, 2000, true},
		{"members precise", 4000, 0, true},
	}
}

// BenchmarkEvaluationAnalysis times the evaluation analysis alone (both of
// its runs) on its worst cases; 07 F6 allows 1 s for a whole compile.
func BenchmarkEvaluationAnalysis(b *testing.B) {
	for _, tc := range analysisCases() {
		b.Run(tc.name, func(b *testing.B) {
			root := libraryCompile(b, analysisWorstCase(tc.props, tc.pats, tc.mixins))
			b.ReportAllocs()
			for b.Loop() {
				if _, issues := analyze([]*sjs.Schema{root}); len(issues) > 0 {
					b.Fatal(issues)
				}
			}
		})
	}
}

// BenchmarkCompileAnalysisWorstCase compiles the analysis worst cases
// whole.
func BenchmarkCompileAnalysisWorstCase(b *testing.B) {
	for _, tc := range analysisCases() {
		b.Run(tc.name, func(b *testing.B) {
			raw := []byte(analysisWorstCase(tc.props, tc.pats, tc.mixins))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := CompileSchema(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkValidateDense measures the validator's throughput per
// subschema evaluation (07 req 84, section 9 item 21) on dense arrays
// against an 8-way oneOf of consts, failing and passing: 9 evaluations per
// item. The work bound charges each such evaluation at least evalUnits
// (package documentation "Work bound"; TestWorkCoversTime compares the
// whole model with the library).
func BenchmarkValidateDense(b *testing.B) {
	const items = 5000
	for _, tc := range []struct{ name, item, outcome string }{
		{"mismatch", `"x"`, "respond"},
		{"match", `"c7"`, "continue"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			f := buildFilter(b, `{"items":`+oneOfConsts(8)+`}`, 4<<20)
			x, _ := exchange("application/json", []byte(denseArray(items, tc.item, 0)))
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				if r := f.Handle(ctx, phase.OnRequestBody, x); (r.Outcome == filter.Continue) != (tc.outcome == "continue") {
					b.Fatalf("Handle = %+v", r)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*items*9), "ns/evaluation")
		})
	}
}
