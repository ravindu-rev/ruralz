// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The bodies of TestHandleMeasuredFit: documents of typical shapes, each
// valid under its schema.
const (
	fitParagraph = `{"type":"paragraph","content":[{"type":"text","text":"Lorem ipsum dolor sit amet, consectetur adipiscing elit."},{"type":"text","text":"bold part","marks":[{"type":"bold"}]}]}`
	fitHeading   = `{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Section title"}]}`
	fitPolygon   = `{"type":"Feature","id":"f1","properties":{"name":"Parcel 12","area":1234.5},"geometry":{"type":"Polygon","coordinates":[[[-122.4194,37.7749],[-122.4184,37.7749],[-122.4184,37.7759],[-122.4194,37.7759],[-122.4194,37.7749]]]}}`
)

// geoJSON is a GeoJSON FeatureCollection schema: a feature's geometry is
// null or one of six geometry types.
const geoJSON = `{"type":"object","required":["type","features"],"properties":{"type":{"const":"FeatureCollection"},"features":{"type":"array","items":{"$ref":"#/$defs/feature"}},"bbox":{"$ref":"#/$defs/bbox"}},` +
	`"$defs":{"bbox":{"type":"array","minItems":4,"items":{"type":"number"}},` +
	`"pos":{"type":"array","minItems":2,"items":{"type":"number"}},` +
	`"ring":{"type":"array","minItems":4,"items":{"$ref":"#/$defs/pos"}},` +
	`"Point":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"Point"},"coordinates":{"$ref":"#/$defs/pos"},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"LineString":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"LineString"},"coordinates":{"type":"array","minItems":2,"items":{"$ref":"#/$defs/pos"}},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"Polygon":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"Polygon"},"coordinates":{"type":"array","items":{"$ref":"#/$defs/ring"}},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"MultiPoint":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"MultiPoint"},"coordinates":{"type":"array","items":{"$ref":"#/$defs/pos"}},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"MultiLineString":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"MultiLineString"},"coordinates":{"type":"array","items":{"type":"array","minItems":2,"items":{"$ref":"#/$defs/pos"}}},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"MultiPolygon":{"type":"object","required":["type","coordinates"],"properties":{"type":{"const":"MultiPolygon"},"coordinates":{"type":"array","items":{"type":"array","items":{"$ref":"#/$defs/ring"}}},"bbox":{"$ref":"#/$defs/bbox"}}},` +
	`"feature":{"type":"object","required":["type","properties","geometry"],"properties":{"type":{"const":"Feature"},"id":{"oneOf":[{"type":"number"},{"type":"string"}]},` +
	`"properties":{"oneOf":[{"type":"null"},{"type":"object"}]},` +
	`"geometry":{"oneOf":[{"type":"null"},{"$ref":"#/$defs/Point"},{"$ref":"#/$defs/LineString"},{"$ref":"#/$defs/Polygon"},{"$ref":"#/$defs/MultiPoint"},{"$ref":"#/$defs/MultiLineString"},{"$ref":"#/$defs/MultiPolygon"}]},"bbox":{"$ref":"#/$defs/bbox"}}}}}`

// composedSchema is an array of orders, each the allOf composition of a
// base and its own properties under unevaluatedProperties false, the
// draft 2020-12 way to close a composed object.
const composedSchema = `{"type":"array","items":{"$ref":"#/$defs/order"},"$defs":{"base":{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"created":{"type":"string"}}},` +
	`"order":{"allOf":[{"$ref":"#/$defs/base"},{"properties":{"total":{"type":"number"},"status":{"enum":["new","paid","shipped"]}}}],"unevaluatedProperties":false}}}`

// conditionalSchema is an array of contacts whose required member depends
// on their kind, through if, then and else.
const conditionalSchema = `{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string"},"email":{"type":"string"},"phone":{"type":"string"}},` +
	`"if":{"properties":{"kind":{"const":"email"}}},"then":{"required":["email"]},"else":{"required":["phone"]}}}`

// patchSchema is a JSON Patch (RFC 6902) of three kinds of operations
// under a oneOf.
const patchSchema = `{"type":"array","items":{"oneOf":[` +
	`{"type":"object","required":["op","path","value"],"properties":{"op":{"enum":["add","replace","test"]},"path":{"type":"string"},"value":true},"additionalProperties":false},` +
	`{"type":"object","required":["op","path"],"properties":{"op":{"const":"remove"},"path":{"type":"string"}},"additionalProperties":false},` +
	`{"type":"object","required":["op","from","path"],"properties":{"op":{"enum":["move","copy"]},"from":{"type":"string"},"path":{"type":"string"}},"additionalProperties":false}]}}`

// patchOperation returns the i-th operation of a patch: a replace, a
// remove or a move.
func patchOperation(i int) string {
	switch i % 3 {
	case 0:
		return fmt.Sprintf(`{"op":"replace","path":"/items/%d/qty","value":%d}`, i, i%10)
	case 1:
		return fmt.Sprintf(`{"op":"remove","path":"/items/%d"}`, i)
	}
	return fmt.Sprintf(`{"op":"move","from":"/items/%d","path":"/archive/%d"}`, i, i)
}

// eventsSchema is an array of events under a k-way oneOf of object
// variants, discriminated by a const.
func eventsSchema(k int) string {
	variants := make([]string, k)
	for i := range variants {
		variants[i] = fmt.Sprintf(`{"type":"object","required":["type","at"],"properties":{"type":{"const":"e%d"},"at":{"type":"string"},"value":{"type":"number"},"label":{"type":"string"}}}`, i)
	}
	return `{"type":"object","required":["events"],"properties":{"events":{"type":"array","items":{"oneOf":[` + strings.Join(variants, ",") + `]}}}}`
}

// joined returns n bodies made by item, joined by commas.
func joined(n int, item func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = item(i)
	}
	return strings.Join(parts, ",")
}

// bulletList returns a rich-text bullet list depth levels deep with width
// items per level, each a paragraph and the next level.
func bulletList(depth, width int) string {
	inner := fitParagraph
	if depth > 1 {
		inner += "," + bulletList(depth-1, width)
	}
	return `{"type":"bulletList","content":[` + joined(width, func(int) string { return `{"type":"listItem","content":[` + inner + `]}` }) + `]}`
}

// richDocument returns a rich-text document of n groups of four
// paragraphs, a heading, a list two levels deep of three items, and a
// blockquote.
func richDocument(n int) string {
	group := strings.Join([]string{fitParagraph, fitParagraph, fitParagraph, fitParagraph, fitHeading, bulletList(2, 3), `{"type":"blockquote","content":[` + fitParagraph + `]}`}, ",")
	return `{"type":"doc","content":[` + joined(n, func(int) string { return group }) + `]}`
}

// uiTree returns a component tree depth levels deep, width children per
// node, rows and columns alternating.
func uiTree(depth, width int, row bool) string {
	if depth == 0 {
		return `{"text":"Label text"}`
	}
	kind := map[bool]string{true: "row", false: "column"}[row]
	return `{"kind":"` + kind + `","children":[` + joined(width, func(int) string { return uiTree(depth-1, width, !row) }) + `]}`
}

// TestHandleMeasuredFit pins the measured fit (typical records fit up to about
// 70 to 84% of the limit, dense enum or union arrays less, operators raise
// maxRequestBodyBytes on validated Routes). Through Factory.Build and Handle at
// the 10 MiB default, each valid body passes at the fraction of the limit it
// names, which the package documentation states ("Working set"), and reserves
// what the model charges, which covers its decoded tree and the live heap
// Handle holds at its peak, both measured after a collection, the peak with the
// copies the passes make between two marks on top (the uniqueItems duplicates
// map among them), but for heapNoise (heapMeter); the log gives its reservation
// against that peak. The decision pass is reserved its worst case over the
// library's member order, in which every alternative of a union may descend
// into the members of a value that fails it: a valid document of unions and
// recursive blocks reserves several to tens of times its measured peak.
func TestHandleMeasuredFit(t *testing.T) {
	requireMeter(t)
	// The fractions are of the 10 MiB default.
	const limit = 10 << 20
	member := func(i int) string { return fmt.Sprintf(`"m%07d":%d`, i, i%1000) }
	// Distinct prices of the same length.
	price := func(i int) string { return fmt.Sprintf("%d.%02d", 10000+i/100, i%100) }
	codes := make([]string, 250)
	for i := range codes {
		codes[i] = fmt.Sprintf(`"%c%c"`, 'a'+i/26, 'a'+i%26)
	}
	for _, tc := range []struct {
		name, schema, body string
		fraction           float64
	}{
		{"events under an 8-way oneOf", eventsSchema(8), `{"events":[` + joined(46000, func(i int) string {
			return fmt.Sprintf(`{"type":"e%d","at":"2026-10-11T09:00:00Z","value":%d.5,"label":"event"}`, i%8, i%1000)
		}) + `]}`, 0.31},
		{"events under a 20-way oneOf", eventsSchema(20), `{"events":[` + joined(19000, func(i int) string {
			return fmt.Sprintf(`{"type":"e%d","at":"2026-10-11T09:00:00Z","value":%d.5,"label":"event"}`, i%20, i%1000)
		}) + `]}`, 0.13},
		{"rich text with nested lists", richText, richDocument(130), 0.045},
		{"rich text of paragraphs", richText, `{"type":"doc","content":[` + joined(11500, func(int) string { return fitParagraph }) + `]}`, 0.19},
		{"component tree of a row of columns", componentTree, `{"kind":"row","children":[` + joined(6500, func(int) string {
			return `{"kind":"column","children":[{"text":"Label text"},{"text":"Label text"},{"text":"Label text"}]}`
		}) + `]}`, 0.06},
		{"component tree nested deep", componentTree, uiTree(6, 3, true), 0},
		{"GeoJSON features", geoJSON, `{"type":"FeatureCollection","features":[` + joined(6700, func(int) string { return fitPolygon }) + `]}`, 0.14},
		// Composed objects, conditional records and unions of operations,
		// at the working-set, reservation and working-set limits.
		{"composed orders under unevaluatedProperties", composedSchema, "[" + joined(79000, func(i int) string {
			return fmt.Sprintf(`{"id":"ord-%06d","created":"2026-10-11","total":%d.5,"status":"paid"}`, i, i%1000)
		}) + "]", 0.54},
		{"conditional records", conditionalSchema, "[" + joined(145000, func(i int) string {
			if i%2 == 0 {
				return fmt.Sprintf(`{"kind":"email","email":"user%d@example.com"}`, i)
			}
			return fmt.Sprintf(`{"kind":"sms","phone":"+1555%07d"}`, i)
		}) + "]", 0.59},
		{"JSON Patch", patchSchema, "[" + joined(76000, patchOperation) + "]", 0.35},
		// A member name that cannot fail propertyNames holds no error.
		{"map under propertyNames", `{"type":"object","propertyNames":{"maxLength":64},"additionalProperties":{"type":"integer"}}`, `{` + joined(450000, member) + `}`, 0.5},
		// Enum arrays, at the work and working-set limits, and dense
		// numeric and uniqueItems arrays, whose fit the package
		// documentation lists so that operators know to raise
		// maxRequestBodyBytes on Routes that validate them.
		{"two-letter codes under a 250-value enum", `{"type":"array","items":{"enum":[` + strings.Join(codes, ",") + `]}}`, "[" + joined(706000, func(i int) string { return codes[i%250] }) + "]", 0.33},
		{"one-character enum values", `{"type":"array","items":{"enum":["a","b","c","d"]}}`, "[" + joined(788000, func(i int) string { return fmt.Sprintf(`"%c"`, 'a'+i%4) }) + "]", 0.3},
		{"decimals within bounds", `{"type":"array","items":{"type":"number","minimum":-1000,"maximum":1000}}`, "[" + joined(790000, func(i int) string { return fmt.Sprintf("%d.25", i%1000) }) + "]", 0.51},
		// A multipleOf check is charged its division, which for a short
		// divisor stays far below the working set.
		{"prices under multipleOf", `{"type":"array","items":{"type":"number","multipleOf":0.01}}`, "[" + joined(790000, price) + "]", 0.66},
		{"timestamp and value pairs", `{"type":"array","items":{"type":"array","prefixItems":[{"type":"integer"},{"type":"number"}]}}`, "[" + joined(345000, func(i int) string { return fmt.Sprintf("[%d,%d.5]", 1760173200+i, i%1000) }) + "]", 0.61},
		{"longitude and latitude pairs", `{"type":"array","items":{"type":"array","minItems":2,"maxItems":2,"items":{"type":"number","minimum":-180,"maximum":180}}}`, denseArray(197000, `[-122.4194,37.7749]`, 0), 0.37},
		{"time series of closed objects", `{"type":"object","required":["points"],"properties":{"points":{"type":"array","items":{"type":"object","required":["t","v"],"properties":{"t":{"type":"integer"},"v":{"type":"number"}},"additionalProperties":false}}}}`, `{"points":[` + joined(200000, func(i int) string { return fmt.Sprintf(`{"t":%d,"v":%d.5}`, 1760173200+i, i%1000) }) + "]}", 0.5},
		{"unique integer ids", `{"type":"array","uniqueItems":true,"items":{"type":"integer"}}`, "[" + joined(427000, func(i int) string { return fmt.Sprint(1000000 + i) }) + "]", 0.32},
		{"unique tags", `{"type":"array","uniqueItems":true,"items":{"type":"string"}}`, "[" + joined(428000, func(i int) string { return fmt.Sprintf(`"t%06d"`, i) }) + "]", 0.4},
		// Distinct decimals, each charged the numbers that may share its
		// hash: prices and bounded readings stop at the working-set limit,
		// not at the work limit.
		{"unique prices", `{"type":"array","uniqueItems":true,"items":{"type":"number"}}`, "[" + joined(430000, price) + "]", 0.36},
		{"unique bounded readings", `{"type":"array","uniqueItems":true,"items":{"type":"number","minimum":-1000,"maximum":1000}}`, "[" + joined(280000, func(i int) string { return fmt.Sprintf("%d.%03d", 100+i/1000, i%1000) }) + "]", 0.21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.3f of the limit, want at least %.3f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, 0)
			m := &heapMeter{}
			r, reserved := handlePeak(t, f, tc.schema, tc.body, m)
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle(%.3f of the limit) = %+v, want Continue", float64(len(tc.body))/float64(limit), r)
			}
			// Handle refuses a reservation over the reservation limit before
			// it reserves, so that bound cannot fail here. What can: Handle
			// reserves what the model charges, the decoded values with what
			// their maps, arrays and strings hold beyond the decoder's
			// charge and the decision pass's working set, and that covers
			// the decoded tree, measured after a collection, and the live
			// heap Handle holds at its peak, measured at the reservation and
			// in the pass as heapMeter schedules it, with the copies the
			// pass makes between two marks on top, both but for heapNoise.
			v, cost, err := f.decode([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			scan := bodyScan{ws: &f.schema.work, numbers: f.schema.numbers, workLimit: f.limits.work}
			scan.scan(v)
			if want := cost + scan.extra + scan.decideMemory(); reserved != want {
				t.Errorf("reserved %d, the model charges %d", reserved, want)
			}
			peak := m.peak()
			t.Logf("body %.3f of the limit: reserved %.2f times the limit, %.2f times the live peak %d, copies %v", float64(len(tc.body))/float64(limit), float64(reserved)/float64(limit), float64(reserved)/float64(peak), peak, m.copies)
			base := liveHeap()
			v, cost, _ = f.decode([]byte(tc.body))
			live := growth(liveHeap(), base)
			runtime.KeepAlive(v)
			if exceeds(live, cost+scan.extra) {
				t.Errorf("the decoded tree holds %d bytes, Handle reserves %d for it", live, cost+scan.extra)
			}
			if exceeds(peak, reserved) {
				t.Errorf("Handle held %d bytes at its peak, reserved %d", peak, reserved)
			}
		})
	}
}

// TestHandleFitEdges pins the edges of the measured fit that the package
// documentation states as counts ("Working set"):
// at the 10 MiB default, a uniqueItems array of about 6,500 objects of two
// string members or 1,700 of one number member passes its limits and
// one of 6,700 or 1,800 is refused at the work limit, every pair being
// charged a comparison; a component tree nested 6 deep with 3 children per
// node passes and one 7 deep is refused at the working-set limit. A body
// passes its limits when Handle reaches the request's context, which has
// ended, so the library never runs.
func TestHandleFitEdges(t *testing.T) {
	objects := func(n int, item string) string {
		return "[" + joined(n, func(i int) string { return fmt.Sprintf(item, i) }) + "]"
	}
	const twoStrings, oneNumber = `{"sku":"SKU-%05d","name":"item"}`, `{"id":%d}`
	for _, tc := range []struct {
		name, schema, pass, refused string
		want                        error
	}{
		{"uniqueItems objects of two strings", `{"type":"array","uniqueItems":true}`, objects(6500, twoStrings), objects(6700, twoStrings), errWorkTooLarge},
		{"uniqueItems objects of a number", `{"type":"array","uniqueItems":true}`, objects(1700, oneNumber), objects(1800, oneNumber), errWorkTooLarge},
		{"component tree", componentTree, uiTree(6, 3, true), uiTree(7, 3, true), errWorkingSetTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(t, tc.schema, 0)
			// A refusal comes before Handle reads the context.
			x, _ := exchange("application/json", []byte(tc.pass))
			if r := f.Handle(&endingContext{Context: t.Context()}, phase.OnRequestBody, x); !errors.Is(r.Err, context.Canceled) {
				t.Errorf("Handle(%d bytes) = %+v, want its limits passed", len(tc.pass), r)
			}
			x, _ = exchange("application/json", []byte(tc.refused))
			if r := f.Handle(&endingContext{Context: t.Context()}, phase.OnRequestBody, x); !errors.Is(r.Err, tc.want) {
				t.Errorf("Handle(%d bytes) = %+v, want %v", len(tc.refused), r, tc.want)
			}
		})
	}
}

// signUpSchema is a sign-up form: an e-mail address under the WHATWG
// e-mail pattern, a name, and free text under a pattern refusing angle
// brackets.
func signUpSchema() string {
	return `{"type":"object","required":["email","name"],"properties":{"email":{"type":"string","maxLength":254,"pattern":"` + emailPattern +
		`"},"name":{"type":"string","maxLength":100},"bio":{"type":"string","maxLength":5000,"pattern":"^[^<>]*$"}}}`
}

// TestHandlePatternFit pins the backtracker's job stack charge: small records
// under ordinary patterns pass at small limits, invalid ones get 400 with
// their keyword location, and the reservation stays within a small multiple
// of what Handle holds at its peak and covers it, but for heapNoise: the live
// heap at the reservation and in each pass as heapMeter schedules it, with
// the largest copy or matcher's own peak on top. One job per alternation of
// the document's costliest program, charged for every string a pattern or a
// uniqueItems pass reaches, would refuse a 149-byte sign-up body at a 64 KiB
// limit, reserve 6.15 MB for a 1,049-byte one at 1 MiB against a peak of
// about 0.13 MB, and answer an invalid one 413 instead of its keyword
// location.
func TestHandlePatternFit(t *testing.T) {
	requireMeter(t)
	signUp := func(bio string) string {
		return `{"email":"ann@example.com","name":"Ann","bio":"` + bio + `"}`
	}
	host := `{"type":"object","properties":{"host":{"type":"string","pattern":` + fmt.Sprintf("%q", hostnamePattern) + `}}}`
	tags := `{"type":"object","properties":{"host":{"type":"string","pattern":` + fmt.Sprintf("%q", hostnamePattern) +
		`},"tags":{"type":"array","uniqueItems":true,"items":{"type":"string"}}}}`
	for _, tc := range []struct {
		name, schema string
		limit        int64
		body, detail string
		// most is the largest reservation allowed.
		most int64
	}{
		{"sign-up at 64 KiB", signUpSchema(), 64 << 10, signUp(strings.Repeat("x", 100)), "", 128 << 10},
		{"longer sign-up at 64 KiB", signUpSchema(), 64 << 10, signUp(strings.Repeat("x", 1000)), "", 192 << 10},
		{"sign-up at 1 MiB", signUpSchema(), 1 << 20, signUp(strings.Repeat("x", 1000)), "", 192 << 10},
		{"long sign-up at 1 MiB", signUpSchema(), 1 << 20, signUp(strings.Repeat("x", 4000)), "", 384 << 10},
		{"sign-up without a name", signUpSchema(), 64 << 10, `{"email":"ann@example.com","bio":"` + strings.Repeat("x", 300) + `"}`, "/required", 192 << 10},
		{"sign-up with an angle bracket", signUpSchema(), 64 << 10, signUp("<" + strings.Repeat("x", 300)), "/properties/bio/pattern", 192 << 10},
		{"host at 64 KiB", host, 64 << 10, `{"host":"` + strings.Repeat("a", 60) + `.example.com"}`, "", 128 << 10},
		{"unique tags beside a host at 64 KiB", tags, 64 << 10, `{"host":"example.com","tags":["` + strings.Repeat("t", 100) + `"]}`, "", 128 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(t, tc.schema, tc.limit)
			m := &heapMeter{}
			r, reserved := handlePeak(t, f, tc.schema, tc.body, m)
			if tc.detail == "" {
				if r.Outcome != filter.Continue {
					t.Fatalf("Handle(%d bytes) = %+v, want Continue", len(tc.body), r)
				}
			} else if got := detailOf(t, r); got != DetailMismatch+" at "+tc.detail {
				t.Fatalf("detail = %q, want the location %s", got, tc.detail)
			}
			if reserved > tc.most {
				t.Errorf("reserved %d, want at most %d", reserved, tc.most)
			}
			t.Logf("%d bytes: reserved %d, live peak %d, of which the matcher %d", len(tc.body), reserved, m.peak(), m.matcher)
			if exceeds(m.peak(), reserved) {
				t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
			}
		})
	}
	// The job stack binds at a small limit only for the inputs short
	// enough for the backtracker (package documentation "Working set"): at
	// 64 KiB, whose working-set limit is MinScaledLimit's 1 MiB, a string
	// under ^[^<>]*$ (7 instructions, one job per position) passes up to
	// about 20,400 bytes, is refused at the working-set limit up to
	// 37,448, and passes again past it on the NFA simulation; at 256 KiB it
	// passes throughout. A body passes its limits when Handle reaches the
	// request's context, which has ended.
	for _, tc := range []struct {
		limit int64
		n     int
		want  error
	}{
		{64 << 10, 20000, context.Canceled},
		{64 << 10, 21000, errWorkingSetTooLarge},
		{64 << 10, maxBacktrackVector/7 - 1, errWorkingSetTooLarge},
		{64 << 10, maxBacktrackVector / 7, context.Canceled},
		{256 << 10, maxBacktrackVector/7 - 1, context.Canceled},
	} {
		f := buildFilter(t, `{"type":"string","pattern":"^[^<>]*$"}`, tc.limit)
		x, _ := exchange("application/json", []byte(`"`+strings.Repeat("x", tc.n)+`"`))
		if r := f.Handle(&endingContext{Context: t.Context()}, phase.OnRequestBody, x); !errors.Is(r.Err, tc.want) {
			t.Errorf("limit %d, a string of %d bytes: Handle = %+v, want %v", tc.limit, tc.n, r, tc.want)
		}
	}
}
