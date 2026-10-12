// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The bodies of TestHandleMeasuredFit: documents of the shapes the review
// measured, each valid under its schema.
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
// draft 2020-12 way to close a composed object (minor review finding).
const composedSchema = `{"type":"array","items":{"$ref":"#/$defs/order"},"$defs":{"base":{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"created":{"type":"string"}}},` +
	`"order":{"allOf":[{"$ref":"#/$defs/base"},{"properties":{"total":{"type":"number"},"status":{"enum":["new","paid","shipped"]}}}],"unevaluatedProperties":false}}}`

// conditionalSchema is an array of contacts whose required member depends
// on their kind, through if, then and else (minor review finding).
const conditionalSchema = `{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string"},"email":{"type":"string"},"phone":{"type":"string"}},` +
	`"if":{"properties":{"kind":{"const":"email"}}},"then":{"required":["email"]},"else":{"required":["phone"]}}}`

// patchSchema is a JSON Patch (RFC 6902) of three kinds of operations
// under a oneOf (minor review finding).
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

// TestHandleMeasuredFit pins the measured fit of the review findings
// (architecture R-77: typical records fit up to about 70 to 84% of the
// limit, dense enum or union arrays less, operators raise
// maxRequestBodyBytes on validated Routes). Through Factory.Build and
// Handle at the 10 MiB default, each valid body passes at the fraction of
// the limit it names, which the package documentation states ("Working
// set"), and reserves what the model charges, which covers the heap
// Handle holds at its peak within the collector's pacing; the log gives
// its reservation against that peak. The decision pass is reserved its
// worst case over the library's member order, in which every alternative
// of a union may descend into the members of a value that fails it: a
// valid document of unions and recursive blocks reserves several to tens
// of times its measured peak.
//
// Requirements: 07 reqs 15, 80 and 82, architecture R-63 and R-77.
func TestHandleMeasuredFit(t *testing.T) {
	// The fractions do not depend on the limit: under -race, which slows
	// the library tenfold, a tenth of the default limit and of the bodies,
	// and a tree nested two levels less (about a tenth of the bytes and
	// of the working set).
	limit, scale, raw, depth := int64(10<<20), 1, int64(0), 6
	if raceEnabled {
		limit, scale, raw, depth = 1<<20, 10, 1<<20, 4
	}
	member := func(i int) string { return fmt.Sprintf(`"m%07d":%d`, i, i%1000) }
	for _, tc := range []struct {
		name, schema, body string
		fraction           float64
	}{
		{"events under an 8-way oneOf", eventsSchema(8), `{"events":[` + joined(46000/scale, func(i int) string {
			return fmt.Sprintf(`{"type":"e%d","at":"2026-10-11T09:00:00Z","value":%d.5,"label":"event"}`, i%8, i%1000)
		}) + `]}`, 0.31},
		{"events under a 20-way oneOf", eventsSchema(20), `{"events":[` + joined(19000/scale, func(i int) string {
			return fmt.Sprintf(`{"type":"e%d","at":"2026-10-11T09:00:00Z","value":%d.5,"label":"event"}`, i%20, i%1000)
		}) + `]}`, 0.13},
		{"rich text with nested lists", richText, richDocument(130 / scale), 0.045},
		{"rich text of paragraphs", richText, `{"type":"doc","content":[` + joined(11500/scale, func(int) string { return fitParagraph }) + `]}`, 0.19},
		{"component tree of a row of columns", componentTree, `{"kind":"row","children":[` + joined(6500/scale, func(int) string {
			return `{"kind":"column","children":[{"text":"Label text"},{"text":"Label text"},{"text":"Label text"}]}`
		}) + `]}`, 0.06},
		{"component tree nested deep", componentTree, uiTree(depth, 3, true), 0},
		{"GeoJSON features", geoJSON, `{"type":"FeatureCollection","features":[` + joined(6700/scale, func(int) string { return fitPolygon }) + `]}`, 0.14},
		// Minor review finding: composed objects, conditional records and
		// unions of operations, at the working-set, reservation and
		// working-set limits.
		{"composed orders under unevaluatedProperties", composedSchema, "[" + joined(72000/scale, func(i int) string {
			return fmt.Sprintf(`{"id":"ord-%06d","created":"2026-10-11","total":%d.5,"status":"paid"}`, i, i%1000)
		}) + "]", 0.49},
		{"conditional records", conditionalSchema, "[" + joined(145000/scale, func(i int) string {
			if i%2 == 0 {
				return fmt.Sprintf(`{"kind":"email","email":"user%d@example.com"}`, i)
			}
			return fmt.Sprintf(`{"kind":"sms","phone":"+1555%07d"}`, i)
		}) + "]", 0.59},
		{"JSON Patch", patchSchema, "[" + joined(76000/scale, patchOperation) + "]", 0.35},
		// Review finding: a member name that cannot fail propertyNames holds
		// no error.
		{"map under propertyNames", `{"type":"object","propertyNames":{"maxLength":64},"additionalProperties":{"type":"integer"}}`, `{` + joined(450000/scale, member) + `}`, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.3f of the limit, want at least %.3f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, raw)
			if loc, ok, err := f.schema.validate(decodeInstance(t, tc.body), nil); !ok || err != nil {
				t.Fatalf("the body is not valid: %q, %v", loc, err)
			}
			x, _ := exchange("application/json", []byte(tc.body))
			px := &peakExchange{Exchange: x}
			var r filter.Result
			peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle(%.3f of the limit) = %+v, want Continue", float64(len(tc.body))/float64(limit), r)
			}
			t.Logf("body %.3f of the limit: reserved %.2f times the limit, %.1f times the peak %.0f", float64(len(tc.body))/float64(limit), float64(px.peak)/float64(limit), float64(px.peak)/peak, peak)
			// Minor review finding: Handle refuses a reservation over the
			// reservation limit before it reserves, so that bound cannot
			// fail here. What can: Handle reserves what the model charges,
			// the decoded values with what their maps, arrays and strings
			// hold beyond the decoder's charge and the decision pass's
			// working set, and that covers the heap it holds at its peak
			// within the collector's pacing (package documentation
			// "Working set": at most about 1.8 times for a wide object).
			v, cost, err := f.decode([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			scan := bodyScan{ws: &f.schema.work, numbers: f.schema.numbers, workLimit: f.limits.work}
			scan.scan(v)
			if want := cost + scan.extra + scan.decideMemory(); px.peak != want {
				t.Errorf("reserved %d, the model charges %d", px.peak, want)
			}
			if peak > float64(2*px.peak+gcSlack) {
				t.Errorf("Handle held %.0f bytes at its peak, more than twice its reservation %d", peak, px.peak)
			}
		})
	}
}
