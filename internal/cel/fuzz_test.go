// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	celgo "cel.dev/cel-go/cel"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fuzzSeeds are the example and negative expressions every target starts
// from (docs/engineering/03-testing-and-quality-strategy.md "Fuzzing").
func fuzzSeeds() []string {
	seeds := []string{
		`request.headers["x-tenant"] == "acme"`,
		`request.body.items.all(i, i.tags.all(t, t.tags.all(u, u == "x")))`,
		`json.encode(request.body)`,
		`request.headers.exists(k, request.headers[k].split(",").exists(v, v == "x"))`,
		`steps.order.body.id == 1 && steps["stock"].status == 200`,
		`now.getHours("Europe/Oslo") == 1`,
		`ruralz.Request{method: "GET"}.method == "GET"`,
		`string(response.body.total)`,
		`{"id": request.body.id, "tags": request.body.items.map(i, i)}`,
		`request.method ==`,
		`size(request.path) > 2 ? request.path.substring(1) : request.path`,
		`consumer.tags.filter(t, t.startsWith("b")).size() > 0`,
		`request.query.tag.lowerAscii() + base64.encode(bytes(request.host))`,
		`consumer != null && "daily-tokens" in consumer.quotas`,
		`consumer.tags.exists(t, t.contains("eu")) && consumer.name.startsWith("a")`,
		`request.headers["x-tenant"].contains(request.headers["x-h0"])`,
		`request.headers.exists(k, k == "x-h1") && request.pathParams.p0 != ""`,
		`auth.claims.sub.contains(auth.claims.scope)`,
		`",".split(",").exists(s, consumer.tags.exists(t, t == "b"))`,
		`route.labels.k.contains(route.name) || route.labels.exists(l, l.startsWith("k"))`,
		`request.body.items.filter(i, i.ok).exists(i, i.a.b == "z")`,
	}
	for _, ex := range examples() {
		seeds = append(seeds, ex.src)
	}
	return seeds
}

// fuzzSite derives a site from a place index and variant bits: the first
// Phase and gate of Policy.spec.when, Upstream scope, and the composition
// mode with one earlier step named "order".
func fuzzSite(place, variant uint8) expr.Site {
	places := expr.Places()
	site := expr.Site{Place: places[int(place)%len(places)].ID, EarlierSteps: []string{"order"}}
	if variant&1 != 0 {
		site.FirstPhase = phase.OnResponse
		site.PolicyType = v1alpha1.PolicyTypeTransformResponse
	}
	site.GatesBody = variant&2 != 0
	if variant&4 != 0 {
		site.Scope = phase.ScopeUpstream
	}
	modes := []v1alpha1.CompositionMode{v1alpha1.CompositionModeSequential, v1alpha1.CompositionModeAggregate, v1alpha1.CompositionModeConditional}
	site.Mode = modes[int(variant>>3)%len(modes)]
	return site
}

// FuzzCompile is the compile oracle: Check never panics, reports only
// RZ-CFG-014 and RZ-CFG-015 with positions inside the source, agrees with
// Builder.Compile, is deterministic, and a compiled program has a finite
// estimate within the ceiling.
func FuzzCompile(f *testing.F) {
	for i, s := range fuzzSeeds() {
		f.Add(s, uint8(i), uint8(i*5))
	}
	c := newCompiler(f)
	f.Fuzz(func(t *testing.T, src string, place, variant uint8) {
		site := fuzzSite(place, variant)
		iss := c.Check(site, src)
		for _, is := range iss {
			if is.Code != "RZ-CFG-014" && is.Code != "RZ-CFG-015" {
				t.Errorf("Check(%q) code %q", src, is.Code)
			}
			if !strings.HasPrefix(is.Message, "CEL "+string(site.Place)+": ") {
				t.Errorf("Check(%q) message %q", src, is.Message)
			}
			if is.Offset < -1 || is.Offset > len(src) || (is.Offset >= 0) != (is.Line > 0) {
				t.Errorf("Check(%q) position %d at %d:%d", src, is.Offset, is.Line, is.Column)
			}
		}
		if again := c.Check(site, src); !reflect.DeepEqual(again, iss) {
			t.Errorf("Check(%q) is not deterministic:\n%+v\n%+v", src, iss, again)
		}
		prg, biss := c.NewBuilder(nil, expr.InputCaps{}).Compile(site, src)
		if (prg == nil) != (len(iss) > 0) && src != "" {
			t.Fatalf("Compile(%q) = %v, %+v; Check = %+v", src, prg, biss, iss)
		}
		if prg != nil && prg.Cost().Max > staticCostCeiling {
			t.Errorf("Compile(%q) cost %+v over the ceiling", src, prg.Cost())
		}
	})
}

// gen is a deterministic generator of activation contents from fuzz bytes.
type gen struct{ state uint64 }

// newGen seeds a generator from data.
func newGen(data []byte) *gen {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return &gen{state: h.Sum64() | 1}
}

// next returns the next value of a xorshift sequence.
func (g *gen) next() uint64 {
	g.state ^= g.state << 13
	g.state ^= g.state >> 7
	g.state ^= g.state << 17
	return g.state
}

// upto returns a value in [0, n].
func (g *gen) upto(n int) int {
	return int(g.next() % uint64(n+1)) //nolint:gosec // n is a small non-negative size, so both conversions are exact
}

// str returns a string of at most n code points, some multi-byte.
func (g *gen) str(n int) string {
	alphabet := []string{"a", "b", ",", " ", "x", "1", ":", "é", "orders:read", ""}
	var b strings.Builder
	for size := 0; size < n; {
		s := alphabet[g.upto(len(alphabet)-1)]
		if c := len([]rune(s)); size+c <= n {
			b.WriteString(s)
			size += max(c, 1)
		} else {
			size++
		}
	}
	return b.String()
}

// json returns a JSON tree of depth at most depth, containers of at most
// size entries and strings of at most strLen code points.
func (g *gen) json(depth, size, strLen int) any {
	switch k := g.upto(5); {
	case depth > 0 && k == 0:
		m := map[string]any{}
		for n := g.upto(size); len(m) < n; {
			m[g.str(8)+strconv.Itoa(len(m))] = g.json(depth-1, size, strLen)
		}
		return m
	case depth > 0 && k == 1:
		l := make([]any, g.upto(size))
		for i := range l {
			l[i] = g.json(depth-1, size, strLen)
		}
		return l
	case k == 2:
		return json.Number(strconv.Itoa(g.upto(1000)))
	case k == 3:
		return g.upto(1) == 1
	default:
		return g.str(strLen)
	}
}

// fuzzVars returns an activation whose strings hold at most strLen code
// points and whose maps and lists at most size entries.
func fuzzVars(data []byte, strLen, size int) *expr.Vars {
	g := newGen(data)
	h := http.Header{}
	// At most size fields, X-Tenant included.
	for i := range g.upto(size - 1) {
		h.Set("X-H"+strconv.Itoa(i), g.str(strLen))
	}
	h.Set("X-Tenant", g.str(strLen))
	req := &expr.Request{
		Method: "GET", Scheme: "https", Host: g.str(strLen), Path: "/" + g.str(strLen-1),
		Header: h, Body: celtypes.FromNative(g.json(3, size, strLen)),
	}
	for i := range g.upto(size) {
		req.PathParams = append(req.PathParams, expr.Param{Name: "p" + strconv.Itoa(i), Value: g.str(strLen)})
	}
	tags := make([]string, g.upto(size))
	for i := range tags {
		tags[i] = g.str(strLen)
	}
	steps := &expr.Steps{}
	steps.Add("order", expr.Step{Status: 200, Header: http.Header{"Etag": {g.str(strLen)}}, Body: celtypes.FromNative(g.json(2, size, strLen))})
	return &expr.Vars{
		Request:  req,
		Source:   &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 443},
		Route:    &expr.Route{Name: g.str(strLen), Labels: map[string]string{"k": g.str(strLen)}},
		Consumer: &expr.Consumer{Name: g.str(strLen), Tier: g.str(strLen), Tags: tags, Quotas: tags},
		Auth:     &expr.Auth{Method: "jwt", Claims: celtypes.FromNative(map[string]any{"scope": g.str(strLen), "sub": g.str(strLen)})},
		Response: &expr.Response{Status: 200 + g.upto(399), Header: http.Header{"X-R": {g.str(strLen)}}, Body: celtypes.FromNative(g.json(3, size, strLen))},
		Upstream: &expr.Upstream{Name: g.str(strLen), Endpoint: "10.0.0.1:80"},
		Attempt:  1 + g.upto(3),
		Steps:    steps,
		AI:       &expr.AI{Model: g.str(strLen), EstimatedInputTokens: int64(g.upto(1 << 20))},
		Now:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Duration: time.Duration(g.upto(5000)) * time.Millisecond,
	}
}

// FuzzCostWithinEstimate checks the estimator: at nominal input sizes
// (strings of 256 code points, lists and maps of 32 entries), the actual
// cost of an evaluation stays within the static estimate.
func FuzzCostWithinEstimate(f *testing.F) {
	for i, s := range fuzzSeeds() {
		f.Add(s, uint8(i), uint8(i*3), []byte(s))
	}
	c := newCompiler(f)
	f.Fuzz(func(t *testing.T, src string, place, variant uint8, data []byte) {
		cc, _ := c.compile(fuzzSite(place, variant), src)
		if cc == nil {
			return
		}
		prg, err := cc.env.Program(cc.ast, celgo.EvalOptions(celgo.OptTrackCost))
		if err != nil {
			t.Fatalf("plan(%q): %v", src, err)
		}
		_, det, _ := prg.Eval(celtypes.NewActivation(fuzzVars(data, nominalStringSize, nominalContainerSize)))
		if det == nil || det.ActualCost() == nil {
			t.Fatalf("Eval(%q) tracked no cost", src)
		}
		if actual := *det.ActualCost(); actual > cc.cost.Max {
			t.Errorf("%q: actual cost %d above the estimate %+v", src, actual, cc.cost)
		}
	})
}

// FuzzCostWithinCappedBound checks the premise of the tracking rule: for
// inputs within a snapshot's caps, the actual cost of an evaluation stays
// within the estimate under those caps, so a program whose capped estimate
// is within the runtime limit cannot reach it untracked
// (docs/architecture/02-configuration-model.md "Limits"). The caps are the
// tightest the generated inputs fit (fuzzCaps).
func FuzzCostWithinCappedBound(f *testing.F) {
	for i, s := range fuzzSeeds() {
		f.Add(s, uint8(i), uint8(i*5), []byte(s))
	}
	c := newCompiler(f)
	f.Fuzz(func(t *testing.T, src string, place, variant uint8, data []byte) {
		cc, _ := c.compile(fuzzSite(place, variant), src)
		if cc == nil {
			return
		}
		v := fuzzVars(data, 64, 16)
		caps := fuzzCaps(v)
		bound, err := cc.cappedBound(caps)
		if err != nil {
			t.Fatalf("cappedBound(%q): %v", src, err)
		}
		prg, err := cc.env.Program(cc.ast, celgo.EvalOptions(celgo.OptTrackCost))
		if err != nil {
			t.Fatalf("plan(%q): %v", src, err)
		}
		_, det, _ := prg.Eval(celtypes.NewActivation(v))
		if det == nil || det.ActualCost() == nil {
			t.Fatalf("Eval(%q) tracked no cost", src)
		}
		if actual := *det.ActualCost(); actual > bound {
			t.Errorf("%q: actual cost %d above the bound %d under caps %+v", src, actual, bound, caps)
		}
	})
}

// fuzzCaps returns the tightest caps v fits: the bytes of its request
// line and header fields, four per field line for the separators, the
// longest string or largest container of its claims, and its Route's and
// Consumer's own sizes.
func fuzzCaps(v *expr.Vars) expr.InputCaps {
	r := v.Request
	n := len(r.Method) + len(r.Scheme) + len(r.Host) + len(r.Path) + len(r.RawQuery())
	for _, p := range r.PathParams {
		n += len(p.Name) + len(p.Value)
	}
	for k, vs := range r.Header {
		for _, s := range vs {
			n += len(k) + len(s) + 4
		}
	}
	caps := expr.InputCaps{RequestHeaderBytes: int64(max(n, 1))}
	if claims, err := v.Auth.Claims.Native(); err == nil {
		caps.ClaimsBytes = int64(max(treeCap(claims), 1))
	}
	caps.Routes.Add(v.Route)
	caps.Consumers.Add(v.Consumer)
	return caps
}

// treeCap returns the longest string, in bytes, and the largest container
// of a JSON tree.
func treeCap(x any) int {
	switch x := x.(type) {
	case string:
		return len(x)
	case map[string]any:
		n := len(x)
		for k, e := range x {
			n = max(n, len(k), treeCap(e))
		}
		return n
	case []any:
		n := len(x)
		for _, e := range x {
			n = max(n, treeCap(e))
		}
		return n
	default:
		return 0
	}
}

// FuzzEvalCapped checks evaluation at the Gateway caps: a 64 KiB header
// value (limits.maxRequestHeaderBytes) and large containers. Evaluation
// returns a value or an *expr.EvalError, so the place's runtime-error rule
// applies; the cost limit and the comprehension stop are such errors.
func FuzzEvalCapped(f *testing.F) {
	for i, s := range fuzzSeeds() {
		f.Add(s, uint8(i), uint8(i*7), []byte(s))
	}
	c := newCompiler(f)
	f.Fuzz(func(t *testing.T, src string, place, variant uint8, data []byte) {
		site := fuzzSite(place, variant)
		prg, _ := c.NewBuilder(nil, expr.InputCaps{}).Compile(site, src)
		if prg == nil {
			return
		}
		v := fuzzVars(data, 64, 256)
		v.Request.Header.Set("X-Tenant", strings.Repeat("a,", 32<<10))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p, _ := expr.LookupPlace(site.Place)
		var err error
		switch p.Result {
		case expr.ResultBool:
			_, err = prg.EvalBool(ctx, v)
		case expr.ResultString:
			_, err = prg.EvalString(ctx, v)
		case expr.ResultDyn:
			_, err = prg.EvalValue(ctx, v)
		case expr.ResultInt:
			_, err = prg.EvalInt(ctx, v)
		}
		var ee *expr.EvalError
		if err != nil && !errors.As(err, &ee) {
			t.Errorf("Eval(%q) error %T %v, want *expr.EvalError", src, err, err)
		}
	})
}
