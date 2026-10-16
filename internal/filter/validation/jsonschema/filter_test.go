// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// update rewrites the goldens under testdata: go test
// ./internal/filter/validation/jsonschema -run Golden -update.
var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// matrixSchema is the schema of the T13 matrix.
const matrixSchema = `{"type":"object","required":["id"],"properties":{"id":{"type":"integer","minimum":1},"tags":{"type":"array","items":{"type":"string"}}}}`

// TestHandleMatrix is 07 T13: media types (req 78), empty bodies (req 79),
// strict decoding (req 80), schema mismatches (req 81) and success (req
// 82) in onRequestBody.
func TestHandleMatrix(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	deep := strings.Repeat("[", 63) + strings.Repeat("]", 63)
	tooDeep := strings.Repeat("[", 64) + strings.Repeat("]", 64)
	for _, tc := range []struct {
		name, ct, body string
		detail         string // "" is success
	}{
		{"json", "application/json", `{"id":1}`, ""},
		{"json charset", "application/json; charset=UTF-8", `{"id":1}`, ""},
		{"json quoted charset", `application/json;charset="utf-8"`, `{"id":1}`, ""},
		{"json empty parameter", "application/json;", `{"id":1}`, ""},
		{"suffix json", "application/vnd.api+json", `{"id":1}`, ""},
		{"upper case", "Application/JSON", `{"id":1}`, ""},
		{"text/json", "text/json", `{"id":1}`, DetailContentType},
		{"utf-16", "application/json; charset=utf-16", `{"id":1}`, DetailContentType},
		{"other parameter", "application/json; profile=x", `{"id":1}`, DetailContentType},
		{"charset spaces", "application/json; charset = utf-8", `{"id":1}`, DetailContentType},
		{"text/plain", "text/plain", `{"id":1}`, DetailContentType},
		{"missing", "-", `{"id":1}`, DetailContentType},
		{"empty field", "", `{"id":1}`, DetailContentType},
		{"list", "application/json, text/plain", `{"id":1}`, DetailContentType},
		{"empty body", "application/json", ``, DetailEmpty},
		{"two values", "application/json", `{} {}`, DetailInvalid},
		{"duplicate names", "application/json", `{"id":1,"id":2}`, DetailInvalid},
		{"depth 64", "application/json", `{"id":1,"x":` + deep + `}`, ""},
		{"depth 65", "application/json", `{"id":1,"x":` + tooDeep + `}`, DetailInvalid},
		{"invalid UTF-8", "application/json", "{\"id\":1,\"s\":\"\xff\"}", DetailInvalid},
		{"lone surrogate", "application/json", `{"id":1,"s":"\ud800"}`, DetailInvalid},
		{"trailing comma", "application/json", `{"id":1,}`, DetailInvalid},
		{"whitespace only", "application/json", " \n", DetailInvalid},
		{"trailing whitespace", "application/json", "{\"id\":1}\r\n\t ", ""},
		{"missing required", "application/json", `{"tags":[]}`, DetailMismatch + " at /required"},
		{"wrong item", "application/json", `{"id":1,"tags":["a",2]}`, DetailMismatch + " at /properties/tags/items/type"},
		{"minimum", "application/json", `{"id":0}`, DetailMismatch + " at /properties/id/minimum"},
		{"not an integer", "application/json", `{"id":1.5}`, DetailMismatch + " at /properties/id/type"},
		{"big integer", "application/json", `{"id":12345678901234567890}`, ""},
		{"1e400", "application/json", `{"id":1e400}`, ""},
		{"number out of range", "application/json", `{"id":1e1001}`, DetailNumber},
		{"long number", "application/json", `{"id":` + strings.Repeat("7", MaxNumberBytes+1) + `}`, DetailNumber},
		{"exponent math/big refuses", "application/json", `{"id":1E1000001}`, DetailNumber},
		// A number no keyword parses is never refused (minor review
		// finding): "x" meets no schema, the tags' items compare no
		// number.
		{"unparsed number", "application/json", `{"id":1,"x":` + strings.Repeat("9", 130) + `,"tags":[]}`, ""},
		{"number below unparsed members", "application/json", `{"id":1,"x":{"y":[1e5000]}}`, ""},
		{"number where a string is due", "application/json", `{"id":1,"tags":[1e5000]}`, DetailMismatch + " at /properties/tags/items/type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, m := exchange(tc.ct, []byte(tc.body))
			before := m.Hdr.Clone()
			r := f.Handle(t.Context(), phase.OnRequestBody, x)
			if tc.detail == "" {
				if r.Outcome != filter.Continue || r.Response != nil || r.Err != nil {
					t.Fatalf("Handle = %+v, want Continue", r)
				}
			} else if got := detailOf(t, r); got != tc.detail {
				t.Fatalf("detail = %q, want %q", got, tc.detail)
			}
			// 07 req 82: the body, headers and framing pass unchanged.
			if string(m.Raw) != tc.body {
				t.Errorf("body changed to %q", m.Raw)
			}
			if fmt.Sprint(m.Hdr) != fmt.Sprint(before) {
				t.Errorf("headers changed: %v, want %v", m.Hdr, before)
			}
			if x.Budget.Reserved != 0 {
				t.Errorf("reservation left after Handle: %d", x.Budget.Reserved)
			}
		})
	}
}

// TestHandleBooleanSchemas covers T13's boolean schemas: true accepts any
// JSON body, false refuses every body at the root.
func TestHandleBooleanSchemas(t *testing.T) {
	yes := buildFilter(t, `true`, testLimit)
	no := buildFilter(t, `false`, testLimit)
	for _, body := range []string{`{}`, `[]`, `"s"`, `0`, `null`, `true`} {
		if r := handle(t, yes, body); r.Outcome != filter.Continue {
			t.Errorf("true schema, body %s: %+v", body, r)
		}
		if got := detailOf(t, handle(t, no, body)); got != DetailMismatch {
			t.Errorf("false schema, body %s: detail %q", body, got)
		}
	}
	// Without numeric keywords the number bounds do not apply.
	if r := handle(t, yes, `[1e999999]`); r.Outcome != filter.Continue {
		t.Errorf("true schema refused a large exponent: %+v", r)
	}
}

// TestHandleOversize covers R-63 and 07 reqs 15 and 80: decoded values
// past filter.DecodedLimitFactor times maxRequestBodyBytes, and (proposed)
// a decision pass that may hold more than WorkingSetFactor times it besides
// them, are filter.ErrTooLarge (413 RZ-RT-003 through the executor), each
// counted on its own (review finding: the decoded limit counts the decoded
// values only); at either budget the body is decided; an unset limit is
// the 10 MiB default.
func TestHandleOversize(t *testing.T) {
	body := `[` + strings.Repeat(`1,`, 999) + `1]` // 1,000 one-digit numbers
	// Each element costs CostValue + CostString + 1 = 33; the array 40.
	const cost = 40 + 1000*33
	atBudget := buildFilter(t, `{"type":"array"}`, cost/filter.DecodedLimitFactor)
	if got := atBudget.limits.decoded; got != cost {
		t.Fatalf("decoded limit = %d, want %d", got, cost)
	}
	if r := handle(t, atBudget, body); r.Outcome != filter.Continue {
		t.Fatalf("body at the decoded budget: %+v", r)
	}
	over := buildFilter(t, `{"type":"array"}`, cost/filter.DecodedLimitFactor-1)
	r := handle(t, over, body)
	if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, filter.ErrTooLarge) || !errors.Is(r.Err, errDecodedTooLarge) {
		t.Fatalf("decoded values over the decoded budget: %+v, want CannotDecide with ErrTooLarge", r)
	}
	// Every item may fail 8 branches: the decision pass's working set is
	// far above the decoded values, under its own limit, which is that of
	// MinScaledLimit below it (TestHandleSmallLimits).
	body = `[` + strings.Repeat(`1,`, 4999) + `1]`
	schema := `{"items":` + oneOfConsts(8) + `}`
	_, scan := scanBody(t, mustCompile(t, schema), body)
	limit := (scan.decideMemory() + WorkingSetFactor - 1) / WorkingSetFactor
	if 5*cost > filter.DecodedLimitFactor*(limit-1) || limit-1 < MinScaledLimit {
		t.Fatalf("the working set %d does not exceed the decoded values %d, or its limit %d is below %d", scan.decideMemory(), 5*cost, limit, MinScaledLimit)
	}
	if got := detailOf(t, handle(t, buildFilter(t, schema, limit), body)); !strings.HasPrefix(got, DetailMismatch) {
		t.Fatalf("working set at its limit: detail %q, want a mismatch", got)
	}
	r = handle(t, buildFilter(t, schema, limit-1), body)
	if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, filter.ErrTooLarge) || !errors.Is(r.Err, errWorkingSetTooLarge) {
		t.Fatalf("working set over its limit: %+v, want CannotDecide with ErrTooLarge", r)
	}
	// An unset limit fails closed: the configuration default of 10 MiB
	// (07 section 9 item 24), never an unbounded decode.
	if unset := buildFilter(t, `{"type":"array"}`, 0); unset.limits != limitsOf(10<<20) || unset.limits.decoded != filter.DecodedLimitFactor*10<<20 {
		t.Errorf("an unset limit gives %+v, want the limits of 10 MiB", unset.limits)
	}
	if got := decodedLimit(-1); got != filter.DecodedLimitFactor*10<<20 {
		t.Errorf("decodedLimit(-1) = %d", got)
	}
	if got := decodedLimit(1 << 62); got != math.MaxInt64/filter.DecodedLimitFactor*filter.DecodedLimitFactor {
		t.Errorf("decodedLimit overflow = %d, want the largest budget", got)
	}
	if got := limitsOf(testLimit); got.workingSet != WorkingSetFactor*testLimit || got.work != WorkPerByte*testLimit {
		t.Errorf("limitsOf(%d) = %+v", testLimit, got)
	}
}

// composedOrder is an OpenAPI-style order: a base, money, an address and
// line items composed with allOf and $ref four levels deep, with patterns
// on identifiers, amounts, codes and the e-mail address.
const composedOrder = `{"$ref":"#/$defs/order","$defs":{` +
	`"base":{"type":"object","required":["id"],"properties":{"id":{"type":"string","pattern":"^[a-z]{3}_[0-9a-f]{8}$"},"created":{"type":"string"}}},` +
	`"money":{"type":"object","required":["amount","currency"],"properties":{"amount":{"type":"string","pattern":"^-?[0-9]+\\.[0-9]{2}$"},` +
	`"currency":{"type":"string","pattern":"^[A-Z]{3}$"}}},` +
	`"address":{"type":"object","required":["line1","country"],"properties":{"line1":{"type":"string","maxLength":100},"city":{"type":"string"},` +
	`"postal":{"type":"string","pattern":"^[A-Z0-9 -]{3,10}$"},"country":{"type":"string","pattern":"^[A-Z]{2}$"}}},` +
	`"item":{"allOf":[{"$ref":"#/$defs/base"},{"type":"object","required":["sku","qty","price"],"properties":{` +
	`"sku":{"type":"string","pattern":"^[A-Z0-9-]{4,32}$"},"qty":{"type":"integer","minimum":1},"price":{"$ref":"#/$defs/money"}}}]},` +
	`"order":{"allOf":[{"$ref":"#/$defs/base"},{"type":"object","required":["items","total"],"properties":{` +
	`"items":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/item"}},"total":{"$ref":"#/$defs/money"},` +
	`"shipping":{"$ref":"#/$defs/address"},"email":{"type":"string","pattern":"` + emailPattern + `"}}}]}}}`

// composedOrderBody is a valid body of composedOrder.
const composedOrderBody = `{"id":"ord_0123abcd","items":[{"id":"itm_0000aaaa","sku":"SKU-0001","qty":2,"price":{"amount":"9.99","currency":"EUR"}}],` +
	`"total":{"amount":"19.98","currency":"EUR"},"shipping":{"line1":"1 Main St","city":"Paris","postal":"75001","country":"FR"},"email":"ann@example.com"}`

// TestHandleSmallLimits is the regression test of the review finding that
// the parts of the working set that do not grow with the body (the
// decision pass's wrapper, a call-stack chain per value, a regexp matcher)
// exceeded WorkingSetFactor times a maxRequestBodyBytes of a few KiB, so
// tiny valid bodies got 413: {} under true at 1 and 2 KiB, a five-digit
// string under a pattern up to 8 KiB, and a composed order up to 16 KiB.
// The working-set, reservation and work limits scale from MinScaledLimit
// at least (proposed, architecture R-77), so at limits of 1 to 16 KiB
// small valid bodies continue, a schema whose root takes 2,000
// evaluations included, while the decoded limit still scales from the raw
// limit (R-63).
//
// Requirements: 07 reqs 80 and 82, architecture R-63 and R-77.
func TestHandleSmallLimits(t *testing.T) {
	login := `{"type":"object","required":["username","password"],"additionalProperties":false,"properties":{` +
		`"username":{"type":"string","minLength":3,"maxLength":32,"pattern":"^[A-Za-z0-9_.-]+$"},"password":{"type":"string","minLength":12,"maxLength":128}}}`
	for _, tc := range []struct{ name, schema, body string }{
		{"true", `true`, `{}`},
		{"an object", `{"type":"object"}`, `{}`},
		{"a bounded integer", `{"type":"integer","minimum":0}`, `1`},
		{"a pattern", `{"type":"string","pattern":"^[0-9]{5}$"}`, `"12345"`},
		{"a login", login, `{"username":"alice_01","password":"correct horse battery"}`},
		{"an e-mail address", `{"type":"object","properties":{"email":{"type":"string","pattern":"` + emailPattern + `"}}}`, `{"email":"ann@example.com"}`},
		{"a composed order", composedOrder, composedOrderBody},
		{"2,000 evaluations at the root", allOfRefs(400, `{"minProperties":0}`), `{}`},
	} {
		for _, limit := range []int64{1 << 10, 2 << 10, 4 << 10, 8 << 10, 16 << 10} {
			f := buildFilter(t, tc.schema, limit)
			if r := handle(t, f, tc.body); r.Outcome != filter.Continue {
				t.Errorf("%s at %d bytes: Handle(%d bytes) = %+v, want Continue", tc.name, limit, len(tc.body), r)
			}
		}
	}
	for _, limit := range []int64{1, 1 << 10, MinScaledLimit - 1, MinScaledLimit} {
		if got, want := limitsOf(limit), limitsOf(MinScaledLimit); got.workingSet != want.workingSet || got.reserve != want.reserve ||
			got.work != want.work || got.decoded != decodedLimit(limit) {
			t.Errorf("limitsOf(%d) = %+v, want the working-set, reservation and work limits of %d and the decoded limit of %d", limit, got, MinScaledLimit, limit)
		}
	}
	if got := limitsOf(MinScaledLimit + 1); got.workingSet != WorkingSetFactor*(MinScaledLimit+1) {
		t.Errorf("limitsOf(%d) = %+v, want its own multiples", MinScaledLimit+1, got)
	}
}

// TestHandleBudget covers 07 reqs 15 and 80: what Handle reserves from
// limits.maxBufferedBytes (the decoded values at the decoder's charge, what
// their maps, arrays and strings hold beyond it, and the decision pass's
// working set; TestHandleLocationPass pins the sum) is refused with
// filter.ErrBudget (503 RZ-RT-004 through the executor), and released when
// validation ends, pass or fail.
func TestHandleBudget(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	x, _ := exchange("application/json", []byte(`{"id":1}`))
	x.Budget.Exhausted = true
	r := f.Handle(t.Context(), phase.OnRequestBody, x)
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, filter.ErrBudget) {
		t.Fatalf("refused reservation: %+v, want CannotDecide with ErrBudget", r)
	}
	var seen int64
	rec := &recordingExchange{Exchange: &filtertest.Exchange{}, onReserve: func(n int64) { seen = n }}
	rec.Msg = &filtertest.Message{Hdr: http.Header{"Content-Type": {"application/json"}}, Raw: []byte(`{"id":0}`)}
	detailOf(t, f.Handle(t.Context(), phase.OnRequestBody, rec))
	if seen <= 0 || rec.Budget.Reserved != 0 {
		t.Errorf("reserved %d, left %d; want a positive reservation, released", seen, rec.Budget.Reserved)
	}
	other := &recordingExchange{Exchange: &filtertest.Exchange{}, reserveErr: errors.New("boom")}
	other.Msg = rec.Msg
	if r := f.Handle(t.Context(), phase.OnRequestBody, other); !errors.Is(r.Err, filter.ErrBudget) {
		t.Errorf("a non-sentinel reservation error: %+v, want it wrapped in ErrBudget", r)
	}
}

// TestMismatchDetailCap covers the detail bound (proposed, 07 req 81): a
// keyword location longer than maxDetailLocation is cut back to its
// longest ancestor that fits, at a token boundary, so the detail stays a
// location of the schema.
func TestMismatchDetailCap(t *testing.T) {
	long := strings.Repeat("k", 200)
	schema := `{"properties":{"` + long + `":{"properties":{"` + long + `":{"type":"string"}}}}}`
	f := buildFilter(t, schema, testLimit)
	got := detailOf(t, handle(t, f, `{"`+long+`":{"`+long+`":1}}`))
	if want := DetailMismatch + " at /properties/" + long + "/properties"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	for _, tc := range []struct{ loc, want string }{
		{"", DetailMismatch},
		{"/" + strings.Repeat("x", maxDetailLocation-1), DetailMismatch + " at /" + strings.Repeat("x", maxDetailLocation-1)},
		{"/" + strings.Repeat("x", maxDetailLocation), DetailMismatch},
		{"/a/" + strings.Repeat("x", maxDetailLocation-3) + "/b", DetailMismatch + " at /a/" + strings.Repeat("x", maxDetailLocation-3)},
	} {
		if got := mismatchDetail(tc.loc); got != tc.want {
			t.Errorf("mismatchDetail(%d bytes) = %q, want %q", len(tc.loc), got, tc.want)
		}
	}
}

// recordingExchange records Reserve calls and can fail them: every one,
// or from the reserveFrom-th on.
type recordingExchange struct {
	*filtertest.Exchange
	onReserve   func(int64)
	reserveErr  error
	reserveFrom int
	calls       int
}

func (x *recordingExchange) Reserve(n int64) error {
	x.calls++
	if x.onReserve != nil {
		x.onReserve(n)
	}
	if x.reserveErr != nil && x.calls >= x.reserveFrom {
		return x.reserveErr
	}
	return x.Exchange.Reserve(n)
}

// TestHandleNumberPaths covers the narrowed number bounds (proposed,
// minor review finding): a body number is refused only where the library
// may parse it: a numeric keyword or the integer type at its own path, a
// uniqueItems array above it, or a const or enum value holding arrays or
// objects compared with a value above it.
//
// Requirements: 07 req 80.
func TestHandleNumberPaths(t *testing.T) {
	for _, tc := range []struct {
		schema, body string
		refused      bool
	}{
		{`{"properties":{"price":{"minimum":0}}}`, `{"price":1,"id":` + strings.Repeat("9", 130) + `}`, false},
		{`{"properties":{"price":{"minimum":0}}}`, `{"price":1e1001}`, true},
		{`{"properties":{"n":{"type":"integer"}}}`, `{"n":1e1001}`, true},
		{`{"properties":{"n":{"type":"number"}}}`, `{"n":1e1001}`, false},
		{`{"properties":{"l":{"uniqueItems":true}}}`, `{"l":[{"a":[1e1001]}]}`, true},
		{`{"properties":{"c":{"const":{"a":[1]}}}}`, `{"c":{"a":[1e1001]}}`, true},
		{`{"properties":{"c":{"const":"a"}}}`, `{"c":1e1001,"d":1e1001}`, false},
		{`{"properties":{"c":{"enum":[1,2]}}}`, `{"c":1e1001}`, true},
		// Review finding (a mutant that survived): a const or enum value
		// holding arrays compares the items of an array body too.
		{`{"const":[1,1,1]}`, `[1e999999,1e999999,1e999999]`, true},
		{`{"enum":[[1],[2]]}`, `[1e1001]`, true},
	} {
		got := handle(t, buildFilter(t, tc.schema, testLimit), tc.body)
		refused := got.Outcome == filter.Respond && got.Response.Detail == DetailNumber
		if refused != tc.refused {
			t.Errorf("schema %s, body %.60s: %+v, want refused %v", tc.schema, tc.body, got.Response, tc.refused)
		}
	}
}

// TestHandleCanceled covers the minor review finding that validation,
// which the library cannot stop, started for a request that had ended
// (07 section 9 item 21): a canceled context is cannot decide with its
// error, before anything is reserved.
func TestHandleCanceled(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	x, _ := exchange("application/json", []byte(`{"id":1}`))
	px := &peakExchange{Exchange: x}
	r := f.Handle(ctx, phase.OnRequestBody, px)
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, context.Canceled) {
		t.Errorf("Handle with a canceled context = %+v, want CannotDecide with context.Canceled", r)
	}
	if px.peak != 0 {
		t.Errorf("reserved %d", px.peak)
	}
}

// TestHandleUnavailable covers 07 req 13: a body the message cannot
// supply is cannot decide with the class default code; the gate's own
// sentinels pass through to the executor.
func TestHandleUnavailable(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	x, m := exchange("application/json", nil)
	m.Unavailable = true
	r := f.Handle(t.Context(), phase.OnRequestBody, x)
	if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, filter.ErrNotAvailable) {
		t.Errorf("unavailable body: %+v", r)
	}
	tooLarge := &failingMessage{Message: m, err: filter.ErrTooLarge}
	x.Msg = tooLarge
	if r := f.Handle(t.Context(), phase.OnRequestBody, x); !errors.Is(r.Err, filter.ErrTooLarge) {
		t.Errorf("body over its limit: %+v", r)
	}
	x.Msg = nil
	if r := f.Handle(t.Context(), phase.OnRequestBody, x); r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errNoMessage) {
		t.Errorf("no message: %+v", r)
	}
}

type failingMessage struct {
	*filtertest.Message
	err error
}

func (m *failingMessage) Body(context.Context) ([]byte, error) { return nil, m.err }

// TestHandleOtherPhases checks that only onRequestBody acts (07 req 1).
func TestHandleOtherPhases(t *testing.T) {
	f := buildFilter(t, `false`, testLimit)
	x, _ := exchange("text/plain", nil)
	for p := range phase.Count {
		if p == phase.OnRequestBody {
			continue
		}
		if r := f.Handle(t.Context(), p, x); r.Outcome != filter.Continue {
			t.Errorf("%s: %+v, want Continue", p, r)
		}
	}
}

// TestHandleConcurrent runs one Filter from many goroutines (07 req 91).
func TestHandleConcurrent(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				body, want := fmt.Sprintf(`{"id":%d}`, i+j+1), filter.Continue
				if j%3 == 0 {
					body, want = `{"id":"x"}`, filter.Respond
				}
				x, _ := exchange("application/json", []byte(body))
				if r := f.Handle(context.Background(), phase.OnRequestBody, x); r.Outcome != want {
					select {
					case errs <- fmt.Sprintf("body %s: %+v", body, r):
					default:
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestJSONContentType covers 07 req 78 directly.
func TestJSONContentType(t *testing.T) {
	for _, tc := range []struct {
		lines []string
		want  bool
	}{
		{[]string{"application/json"}, true},
		{[]string{" application/json ; charset=utf-8 ; "}, true},
		{[]string{"application/problem+json;CHARSET=UTF-8"}, true},
		{[]string{"application/+json"}, false},
		{[]string{"application/json; charset"}, false},
		{[]string{`application/json; charset="utf-8`}, false},
		{[]string{"application/json; charset=utf8"}, false},
		{[]string{"application/json", "application/json"}, false},
		{nil, false},
	} {
		if got := jsonContentType(tc.lines); got != tc.want {
			t.Errorf("jsonContentType(%q) = %v, want %v", tc.lines, got, tc.want)
		}
	}
}

// fakeShared is a filter.Shared that counts builds.
type fakeShared struct {
	mu     sync.Mutex
	m      map[any]any
	builds int
}

func (s *fakeShared) Get(key any, build func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v, nil
	}
	s.builds++
	v, err := build()
	if err != nil {
		return nil, err
	}
	if s.m == nil {
		s.m = map[any]any{}
	}
	s.m[key] = v
	return v, nil
}

// TestBuild covers the Factory (07 reqs 75, 77): RZ-CFG-005 errors, carry
// over through Previous, sharing within a snapshot, also of a Schema
// carried over unchanged, in either build order (minor review findings),
// and the status taken from the registry.
func TestBuild(t *testing.T) {
	ctx := t.Context()
	env := func(schema string, limit int64) filter.BuildEnv {
		return filter.BuildEnv{
			Config: &v1alpha1.ValidationJSONSchemaConfig{Schema: v1alpha1.JSONSchemaDocument(schema)},
			Limits: filter.Limits{MaxRequestBodyBytes: limit},
		}
	}
	for _, tc := range []struct {
		name string
		env  filter.BuildEnv
		want string
	}{
		{"wrong config type", filter.BuildEnv{Config: &v1alpha1.CORSConfig{}}, "want *v1alpha1.ValidationJSONSchemaConfig"},
		{"nil config", filter.BuildEnv{Config: (*v1alpha1.ValidationJSONSchemaConfig)(nil)}, "want *v1alpha1.ValidationJSONSchemaConfig"},
		{"empty schema", env("", 1), "config.schema is required"},
		{"invalid JSON", env(`{`, 1), "not valid JSON"},
		{"invalid schema", env(`{"pattern":"(?=x)"}`, 1), "RE2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Factory{}.Build(ctx, tc.env)
			if f != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build = %v, %v, want an error containing %q", f, err, tc.want)
			}
			if code, _ := errcode.CodeOf(err); code != CodeConfig {
				t.Errorf("code = %q, want %s", code, CodeConfig)
			}
		})
	}

	first := buildFilter(t, matrixSchema, testLimit)
	if first.status != errcode.Status(CodeRejected) || first.status != http.StatusBadRequest {
		t.Errorf("status = %d, want the registered 400", first.status)
	}
	if first.Schema() == nil {
		t.Fatal("Schema() = nil")
	}
	// Same document (reformatted), same limits: the previous Filter itself.
	e := env(" "+matrixSchema+" ", testLimit)
	e.Previous = first
	got, err := Factory{}.Build(ctx, e)
	if err != nil || got != filter.Filter(first) {
		t.Errorf("carry over = %v, %v, want the previous Filter", got, err)
	}
	// Same document, new limits: a new Filter sharing the compiled Schema.
	e = env(matrixSchema, 2*testLimit)
	e.Previous = first
	got, err = Factory{}.Build(ctx, e)
	if err != nil || got == filter.Filter(first) || got.(*Filter).schema != first.schema || got.(*Filter).limits.decoded != 2*filter.DecodedLimitFactor*testLimit {
		t.Errorf("carry over with new limits = %+v, %v", got, err)
	}
	// A changed document compiles anew.
	e = env(`{"type":"string"}`, testLimit)
	e.Previous = first
	if got, err := (Factory{}).Build(ctx, e); err != nil || got.(*Filter).schema == first.schema {
		t.Errorf("changed document = %+v, %v", got, err)
	}
	// Within a snapshot, equal documents share one compile.
	shared := &fakeShared{}
	var schemas []*Schema
	for _, doc := range []string{matrixSchema, " " + matrixSchema, `{"type":"null"}`} {
		e := env(doc, testLimit)
		e.Shared = shared
		f, err := Factory{}.Build(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		schemas = append(schemas, f.(*Filter).schema)
	}
	if shared.builds != 2 || schemas[0] != schemas[1] || schemas[0] == schemas[2] {
		t.Errorf("shared builds = %d, schemas %p %p %p", shared.builds, schemas[0], schemas[1], schemas[2])
	}
	e = env(`{"pattern":"(?=x)"}`, testLimit)
	e.Shared = shared
	if _, err := (Factory{}).Build(ctx, e); !errors.Is(err, ErrSchema) {
		t.Errorf("shared compile failure = %v, want ErrSchema", err)
	}
	bad := &fakeShared{m: map[any]any{}}
	p, _ := prepare([]byte(matrixSchema))
	bad.m[sharedKey{p.sum}] = "not a schema"
	e = env(matrixSchema, testLimit)
	e.Shared = bad
	if _, err := (Factory{}).Build(ctx, e); err == nil {
		t.Error("a foreign shared value was accepted")
	}
	// Minor review findings: the previous Schema reused under new limits
	// is shared with the snapshot's other Policies of the same document,
	// and an error of Shared itself is not a schema finding.
	shared = &fakeShared{}
	e = env(matrixSchema, 2*testLimit)
	e.Previous, e.Shared = first, shared
	if _, err := (Factory{}).Build(ctx, e); err != nil {
		t.Fatal(err)
	}
	e = env(" "+matrixSchema, testLimit)
	e.Shared = shared
	if got, err := (Factory{}).Build(ctx, e); err != nil || got.(*Filter).schema != first.schema || shared.builds != 1 {
		t.Errorf("another Policy of the same document = %v, builds %d, want the previous Schema", err, shared.builds)
	}
	// The same with the limits unchanged: the previous Filter itself is
	// carried over, and its Schema still serves the others.
	shared = &fakeShared{}
	e = env(matrixSchema, testLimit)
	e.Previous, e.Shared = first, shared
	if got, err := (Factory{}).Build(ctx, e); err != nil || got != filter.Filter(first) {
		t.Fatalf("unchanged carry over = %v, %v, want the previous Filter", got, err)
	}
	e = env(" "+matrixSchema, 2*testLimit)
	e.Shared = shared
	if got, err := (Factory{}).Build(ctx, e); err != nil || got.(*Filter).schema != first.schema || shared.builds != 1 {
		t.Errorf("another Policy of a carried-over document = %v, builds %d, want the previous Schema", err, shared.builds)
	}
	// Minor review finding: when a new Policy of the same document builds
	// first, it compiles the document (it has no Previous), and the
	// carried-over Policy then takes that Schema, so the snapshot holds
	// one Schema per document.
	shared = &fakeShared{}
	e = env(" "+matrixSchema, 2*testLimit)
	e.Shared = shared
	added, err := Factory{}.Build(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	e = env(matrixSchema, testLimit)
	e.Previous, e.Shared = first, shared
	if got, err := (Factory{}).Build(ctx, e); err != nil || got.(*Filter).schema != added.(*Filter).schema || got.(*Filter).limits != first.limits || shared.builds != 1 {
		t.Errorf("a carry over after a new Policy of its document = %v, builds %d, want the snapshot's Schema", err, shared.builds)
	}
	e = env(matrixSchema, testLimit)
	e.Previous, e.Shared = first, failingShared{err: errors.New("snapshot compile canceled")}
	if _, err := (Factory{}).Build(ctx, e); err == nil {
		t.Error("an unchanged carry over hid a Shared failure")
	}
	canceled := errors.New("snapshot compile canceled")
	e = env(`{"type":"boolean"}`, testLimit)
	e.Shared = failingShared{err: canceled}
	_, err = Factory{}.Build(ctx, e)
	if code, coded := errcode.CodeOf(err); !errors.Is(err, canceled) || coded {
		t.Errorf("a Shared failure = %v (code %q), want it unchanged", err, code)
	}
}

// failingShared is a filter.Shared that fails.
type failingShared struct{ err error }

func (s failingShared) Get(any, func() (any, error)) (any, error) { return nil, s.err }

// TestGoldenProblemDocuments is 07 T14: the RZ-RT-009 problem document of
// every detail, as internal/problem writes it, byte for byte.
func TestGoldenProblemDocuments(t *testing.T) {
	f := buildFilter(t, matrixSchema, testLimit)
	for _, tc := range []struct{ name, ct, body string }{
		{"content-type", "text/plain", `{"id":1}`},
		{"empty", "application/json", ``},
		{"invalid-json", "application/json", `{"id":1,"id":1}`},
		{"number-range", "application/json", `{"id":1e5000}`},
		{"mismatch", "application/json", `{"id":"<script>"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, _ := exchange(tc.ct, []byte(tc.body))
			r := f.Handle(t.Context(), phase.OnRequestBody, x)
			detailOf(t, r)
			p := problem.New(r.Response.Code, x.ID)
			p.Detail = r.Response.Detail
			got := append(problem.Append(nil, p), '\n')
			golden(t, filepath.Join("testdata", "problem", tc.name+".json"), got)
		})
	}
	root := buildFilter(t, `false`, testLimit)
	x, _ := exchange("application/json", []byte(`{}`))
	r := root.Handle(t.Context(), phase.OnRequestBody, x)
	p := problem.New(r.Response.Code, x.ID)
	p.Detail = r.Response.Detail
	golden(t, filepath.Join("testdata", "problem", "mismatch-root.json"), append(problem.Append(nil, p), '\n'))
}

// golden compares got with the file at path, rewriting it under -update.
func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// TestBuildStopsWhenCanceled covers the minor review finding that Build
// never read its context: a canceled or superseded snapshot compile still
// compiled every validation Policy it reached. An ended context is its
// error, unchanged and without a code, at the start of Build, before the
// library compiles the document and before the evaluation analysis; a
// context that lasts builds the Filter.
//
// Requirements: 07 req 90 (each Factory call one unit of work between the
// loader's yields) and the filter.Factory contract (an RZ-CFG code only
// for a finding about the Policy).
func TestBuildStopsWhenCanceled(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{
		canceled,
		&endingContext{Context: t.Context(), live: 0},
		&endingContext{Context: t.Context(), live: 1},
		&endingContext{Context: t.Context(), live: 2},
	} {
		f, err := Factory{}.Build(ctx, filter.BuildEnv{Config: configOf(matrixSchema)})
		if code, coded := errcode.CodeOf(err); f != nil || !errors.Is(err, context.Canceled) || coded {
			t.Errorf("Build = %v, %v (code %q), want context.Canceled without a code", f, err, code)
		}
	}
	ctx := &endingContext{Context: t.Context(), live: 3}
	if f, err := (Factory{}).Build(ctx, filter.BuildEnv{Config: configOf(matrixSchema)}); err != nil || f == nil || ctx.live != 0 {
		t.Errorf("Build = %v, %v with %d checks left, want a Filter after three", f, err, ctx.live)
	}
	// A schema the walk refuses is a finding, whatever the context.
	ctx = &endingContext{Context: t.Context(), live: 1}
	if _, err := (Factory{}).Build(ctx, filter.BuildEnv{Config: configOf(`{"pattern":"(?=x)"}`)}); !errors.Is(err, ErrSchema) {
		t.Errorf("Build of a refused schema = %v, want ErrSchema", err)
	}
}
