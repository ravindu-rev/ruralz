// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const (
	gatewayDoc  = `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge", "labels": {"team": "platform"}}, "spec": {"listeners": [{"name": "http", "port": 8080, "protocol": "http"}], "policies": [{"name": "cors"}]}}`
	routeDoc    = `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "orders"}, "spec": {"match": {"path": {"prefix": "/orders"}}, "upstreams": [{"name": "orders", "weight": 1}]}}`
	upstreamDoc = `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "orders"}, "spec": {"protocol": "http", "endpoints": [{"address": "orders:8080"}]}}`
	policyDoc   = `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "cors"}, "spec": {"type": "cors", "config": {"allowOrigins": ["https://shop.example"]}}}`
)

// TestEnvelope covers the document shape and resource order: format then
// resources, kind/metadata/spec only, no apiVersion, Gateway, Upstream,
// Plugin, Policy, Consumer, AIProvider, AIModel, Route then name order,
// no whitespace and no trailing newline.
func TestEnvelope(t *testing.T) {
	got := encode(t, routeDoc, policyDoc, upstreamDoc, gatewayDoc,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a-first"}, "spec": {}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "AIModel", "metadata": {"name": "m"}, "spec": {}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "AIProvider", "metadata": {"name": "p"}, "spec": {}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "g"}, "spec": {}}`)
	want := `{"format":"ruralz.canonical.v1","resources":[` +
		`{"kind":"Gateway","metadata":{"labels":{"team":"platform"},"name":"edge"},"spec":{"listeners":[{"name":"http","port":8080,"protocol":"http"}],"policies":[{"name":"cors"}]}},` +
		`{"kind":"Upstream","metadata":{"name":"orders"},"spec":{"endpoints":[{"address":"orders:8080"}],"protocol":"http"}},` +
		`{"kind":"Plugin","metadata":{"name":"g"},"spec":{}},` +
		`{"kind":"Policy","metadata":{"name":"cors"},"spec":{"config":{"allowOrigins":["https://shop.example"]},"type":"cors"}},` +
		`{"kind":"Consumer","metadata":{"name":"c"},"spec":{}},` +
		`{"kind":"AIProvider","metadata":{"name":"p"},"spec":{}},` +
		`{"kind":"AIModel","metadata":{"name":"m"},"spec":{}},` +
		`{"kind":"Route","metadata":{"name":"a-first"},"spec":{}},` +
		`{"kind":"Route","metadata":{"name":"orders"},"spec":{"match":{"path":{"prefix":"/orders"}},"upstreams":[{"name":"orders","weight":1}]}}` +
		`]}`
	sameBytes(t, "canonical document", got, []byte(want))
	if strings.Contains(string(got), "apiVersion") {
		t.Error("canonical form carries apiVersion")
	}
}

// TestEmptyBundle: a Bundle without resources is the bare envelope.
func TestEmptyBundle(t *testing.T) {
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), hub.NewBundle(nil), Options{Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(rev.Content) != `{"format":"ruralz.canonical.v1","resources":[]}` {
		t.Errorf("Encode(empty) = %s", rev.Content)
	}
}

// TestDigest: the digest is SHA-256 over the exact bytes, which carry no
// trailing newline, so sha256sum of a build output prints it.
func TestDigest(t *testing.T) {
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), bundleOf(t, gatewayDoc, routeDoc), Options{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rev.Content)
	if rev.Digest.Hex() != hex.EncodeToString(sum[:]) || rev.Digest != revision.Sum(rev.Content) {
		t.Errorf("Digest = %s, want sha256 %x", rev.Digest, sum)
	}
	if strings.HasSuffix(string(rev.Content), "\n") {
		t.Error("content ends with a newline")
	}
	if !strings.HasPrefix(rev.Digest.Short(), "rev-") || len(rev.Digest.Short()) != 16 {
		t.Errorf("Short() = %q", rev.Digest.Short())
	}
}

// TestConversionDataStripped: ruralz.io/conversion-data never reaches the
// canonical form, and annotations that held only it disappear, so a
// down-converted resource keeps its source's Revision.
func TestConversionDataStripped(t *testing.T) {
	plain := encode(t, `{"kind": "Route", "metadata": {"name": "r"}, "spec": {}}`)
	only := encode(t, `{"kind": "Route", "metadata": {"name": "r", "annotations": {"ruralz.io/conversion-data": "{\"x\":1}"}}, "spec": {}}`)
	sameBytes(t, "conversion-data only", only, plain)
	mixed := encode(t, `{"kind": "Route", "metadata": {"name": "r", "annotations": {"ruralz.io/conversion-data": "x", "owner": "team-a"}}, "spec": {}}`)
	want := `{"format":"ruralz.canonical.v1","resources":[{"kind":"Route","metadata":{"annotations":{"owner":"team-a"},"name":"r"},"spec":{}}]}`
	sameBytes(t, "mixed annotations", mixed, []byte(want))
	// An authored empty annotations object is presence and stays.
	empty := encode(t, `{"kind": "Route", "metadata": {"name": "r", "annotations": {}}, "spec": {}}`)
	if !strings.Contains(string(empty), `"annotations":{}`) {
		t.Errorf("empty annotations dropped: %s", empty)
	}
}

// TestStrings is the RFC 8785 string table: only '"', '\' and controls
// are escaped, with short escapes or lowercase \u00xx; '/', U+007F,
// U+2028, U+2029, non-BMP characters and HTML characters stay literal.
func TestStrings(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"\b\t\n\f\r", `"\b\t\n\f\r"`},
		{"\x00\x01\x1f", `"\u0000\u0001\u001f"`},
		{"\x0b\x0e", `"\u000b\u000e"`},
		{`"\`, `"\"\\"`},
		{"/\x7f\u2028\u2029", "\"/\x7f\u2028\u2029\""},
		{"\U0001F600 é €", "\"\U0001F600 é €\""},
		{"<script>&amp;</script>", `"<script>&amp;</script>"`},
		{"", `""`},
	} {
		got, err := AppendNode(nil, &tree.Node{Kind: tree.KindString, Text: tc.in})
		if err != nil || string(got) != tc.want {
			t.Errorf("AppendNode(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := AppendNode(nil, &tree.Node{Kind: tree.KindString, Text: "a\xffb"}); !errors.Is(err, ErrEncode) || !errors.Is(err, jsonval.ErrInvalidUTF8) {
		t.Errorf("invalid UTF-8: error = %v, want ErrEncode and ErrInvalidUTF8", err)
	}
	if _, err := AppendNode(nil, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "\xfe", Value: &tree.Node{Kind: tree.KindBool}}}}); !errors.Is(err, ErrEncode) {
		t.Errorf("invalid UTF-8 member name: error = %v", err)
	}
}

// TestCELByteExact: CEL text with '<', '>' and '&' is written byte for
// byte, never HTML-escaped (CEL text byte-exact).
func TestCELByteExact(t *testing.T) {
	rule := `response.status >= 400 && duration > duration("1s") || request.headers["x"] < "<&>"`
	doc := fmt.Sprintf(`{"kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "authz.cel", "config": {"rule": %s}}}`, strconv.Quote(rule))
	got := string(encode(t, doc))
	if !strings.Contains(got, `"rule":"response.status >= 400 && duration > duration(\"1s\") || request.headers[\"x\"] < \"<&>\""`) {
		t.Errorf("CEL text changed: %s", got)
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u0026`) {
		t.Errorf("HTML escaping in %s", got)
	}
}

// TestNumbers is the RFC 8785 number table (RFC 8785 Appendix B). Integer
// literals outside ±(2^53−1) are rounded through the double as RFC 8785
// prescribes; refusing them in authored configuration is stage G's rule.
func TestNumbers(t *testing.T) {
	for _, tc := range []struct {
		lit  string
		kind tree.Kind
		want string
	}{
		{"0", tree.KindInt, "0"},
		{"-0", tree.KindInt, "0"},
		{"100", tree.KindInt, "100"},
		{"-42", tree.KindInt, "-42"},
		{"9007199254740991", tree.KindInt, "9007199254740991"},
		{"9007199254740993", tree.KindInt, "9007199254740992"},
		{"123456789012345678901", tree.KindInt, "123456789012345680000"},
		{"1.0", tree.KindFloat, "1"},
		{"-0.0", tree.KindFloat, "0"},
		{"1E2", tree.KindFloat, "100"},
		{"0.05", tree.KindFloat, "0.05"},
		{"0.000001", tree.KindFloat, "0.000001"},
		{"1e-7", tree.KindFloat, "1e-7"},
		{"1e21", tree.KindFloat, "1e+21"},
		{"1e20", tree.KindFloat, "100000000000000000000"},
		{"333333333.3333333", tree.KindFloat, "333333333.3333333"},
		{"5e-324", tree.KindFloat, "5e-324"},
		{"1.7976931348623157e308", tree.KindFloat, "1.7976931348623157e+308"},
		{"1.2345678901234568e20", tree.KindFloat, "123456789012345680000"},
		{"4.50", tree.KindFloat, "4.5"},
		{"0.92", tree.KindFloat, "0.92"},
		{"-1.5e-10", tree.KindFloat, "-1.5e-10"},
	} {
		got, err := AppendNode(nil, &tree.Node{Kind: tc.kind, Text: tc.lit})
		if err != nil || string(got) != tc.want {
			t.Errorf("AppendNode(%s) = %s, %v; want %s", tc.lit, got, err, tc.want)
		}
	}
	for _, bad := range []string{"1e400", "-1e309", "NaN", "Infinity", "01", "1.", ""} {
		if _, err := AppendNode(nil, &tree.Node{Kind: tree.KindFloat, Text: bad}); !errors.Is(err, ErrEncode) {
			t.Errorf("AppendNode(%q) error = %v, want ErrEncode", bad, err)
		}
	}
}

// TestMemberOrder is RFC 8785 section 3.2.3: members sort by UTF-16 code
// units, which puts U+1F600 (a surrogate pair) before U+FB33 although its
// UTF-8 bytes sort after.
func TestMemberOrder(t *testing.T) {
	n := jsonTree(t, `{"\u20ac": 1, "\r": 2, "\ufb33": 3, "1": 4, "\ud83d\ude00": 5, "\u0080": 6, "\u00f6": 7}`)
	got, err := AppendNode(nil, n)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"\\r\":2,\"1\":4,\"\u0080\":6,\"\u00f6\":7,\"\u20ac\":1,\"\U0001F600\":5,\"\ufb33\":3}"
	if string(got) != want {
		t.Errorf("order = %s\nwant    %s", got, want)
	}
	dup := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
		{Key: "b", Value: &tree.Node{Kind: tree.KindBool}}, {Key: "a", Value: &tree.Node{Kind: tree.KindBool}}, {Key: "b", Value: &tree.Node{Kind: tree.KindBool, Bool: true}},
	}}
	if _, err := AppendNode(nil, dup); !errors.Is(err, ErrEncode) || !strings.Contains(err.Error(), "duplicate member name") {
		t.Errorf("duplicate names: error = %v", err)
	}
}

// TestValues covers the remaining value kinds: booleans, nested lists in
// order (stage G already sorted them), empty containers, and null and
// missing values, which have no canonical form.
func TestValues(t *testing.T) {
	got, err := AppendNode(nil, jsonTree(t, `{"z": [true, false, [], {}, "", [3, 1, 2]], "a": {"y": 1, "x": {"k": "v"}}}`))
	if err != nil || string(got) != `{"a":{"x":{"k":"v"},"y":1},"z":[true,false,[],{},"",[3,1,2]]}` {
		t.Errorf("AppendNode = %s, %v", got, err)
	}
	for name, n := range map[string]*tree.Node{
		"null":           jsonTree(t, `{"a": [1, {"b": null}]}`),
		"nil item":       {Kind: tree.KindList, Items: []*tree.Node{nil}},
		"nil member":     {Kind: tree.KindMap, Members: []tree.Member{{Key: "a"}}},
		"nil root":       nil,
		"unknown kind":   {Kind: tree.Kind(99)},
		"invalid number": {Kind: tree.KindInt, Text: "x"},
	} {
		if _, err := AppendNode(nil, n); !errors.Is(err, ErrEncode) {
			t.Errorf("%s: error = %v, want ErrEncode", name, err)
		}
	}
	_, err = AppendNode(nil, jsonTree(t, `{"a": [1, {"b": null}]}`))
	if err == nil || !strings.Contains(err.Error(), "a[1].b: null has no ruralz.canonical.v1 form") {
		t.Errorf("null path: error = %v", err)
	}
}

// TestDepthLimit: a tree deeper than the encoder bound fails instead of
// exhausting the stack.
func TestDepthLimit(t *testing.T) {
	root := &tree.Node{Kind: tree.KindList}
	cur := root
	for range maxEncodeDepth + 1 {
		next := &tree.Node{Kind: tree.KindList}
		cur.Items = []*tree.Node{next}
		cur = next
	}
	if _, err := AppendNode(nil, root); !errors.Is(err, jsonval.ErrDepth) {
		t.Errorf("deep tree: error = %v, want ErrDepth", err)
	}
}

// TestLevel: the encoder renders only the index's own schema level.
func TestLevel(t *testing.T) {
	b := bundleOf(t, routeDoc)
	if _, err := NewEncoder(hubIndex(t)).Encode(context.Background(), b, Options{Level: 1}); !errors.Is(err, ErrEncode) {
		t.Errorf("Level 1 on a level-0 schema: error = %v, want ErrEncode", err)
	}
	if _, err := NewEncoder(syntheticIndex(t)).Encode(context.Background(), hub.NewBundle(nil), Options{}); !errors.Is(err, ErrEncode) {
		t.Errorf("Level 0 on a level-2 schema: error = %v, want ErrEncode", err)
	}
	if _, err := NewEncoder(nil).Encode(context.Background(), b, Options{}); err != nil {
		t.Errorf("nil index: error = %v", err)
	}
	if _, err := NewEncoder(nil).Encode(context.Background(), nil, Options{}); !errors.Is(err, ErrEncode) {
		t.Errorf("nil Bundle: error = %v, want ErrEncode", err)
	}
}

// TestEncodeErrors: trees the pipeline never hands over fail with
// ErrEncode, naming the resource and path.
func TestEncodeErrors(t *testing.T) {
	ctx := context.Background()
	enc := NewEncoder(hubIndex(t))
	for _, tc := range []struct {
		name string
		res  tree.Resource
		want string
	}{
		{"not a mapping", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: &tree.Node{Kind: tree.KindList}}, "Route/r: resource tree is not a mapping"},
		{"nil tree", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}}, "resource tree is not a mapping"},
		{"extra member", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "r"}, "status": {}}`)}, "Route/r status: member has no place"},
		{"kind mismatch", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Upstream", "metadata": {"name": "r"}}`)}, "Route/r kind: kind does not match"},
		{"name mismatch", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "x"}}`)}, "Route/r metadata.name: metadata.name does not match"},
		{"no metadata", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route"}`)}, "metadata is not a mapping"},
		{"environment", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindEnvironment, Name: "prod"}, Root: jsonTree(t, `{"kind": "Environment", "metadata": {"name": "prod"}}`)}, `kind "Environment" never appears`},
		{"cluster", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindCluster, Name: "c"}, Root: jsonTree(t, `{"kind": "Cluster", "metadata": {"name": "c"}}`)}, `kind "Cluster" never appears`},
		{"null in spec", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "r"}, "spec": {"timeout": null}}`)}, "Route/r spec.timeout: null"},
		{"null in metadata", tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "r", "labels": null}}`)}, "Route/r metadata.labels: null"},
	} {
		_, err := enc.EncodeResources(ctx, []tree.Resource{tc.res}, Options{})
		if !errors.Is(err, ErrEncode) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want ErrEncode containing %q", tc.name, err, tc.want)
		}
	}
	// A kind outside the schema of a non-nil index.
	synth := NewEncoder(nil)
	_, err := synth.EncodeResources(ctx, []tree.Resource{{ID: tree.ID{Kind: "Widget", Name: "w"}, Root: jsonTree(t, `{"kind": "Widget", "metadata": {"name": "w"}}`)}}, Options{})
	if !errors.Is(err, ErrEncode) {
		t.Errorf("unknown kind: error = %v", err)
	}
	// Duplicate identities.
	r := tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "r"}}`)}
	if _, err := enc.EncodeResources(ctx, []tree.Resource{r, r}, Options{}); !errors.Is(err, ErrEncode) || !strings.Contains(err.Error(), "duplicate resource Route/r") {
		t.Errorf("duplicate: error = %v", err)
	}
}

// TestEncodeResourcesSorts: EncodeResources drops apiVersion and sorts
// like Encode.
func TestEncodeResourcesSorts(t *testing.T) {
	var rs []tree.Resource
	for _, d := range []string{routeDoc, gatewayDoc, upstreamDoc, policyDoc} {
		r := resourceOf(t, jsonTree(t, d))
		rs = append(rs, tree.Resource{ID: r.ID, Root: r.Tree})
	}
	rev, err := NewEncoder(hubIndex(t)).EncodeResources(context.Background(), rs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "EncodeResources", rev.Content, encode(t, gatewayDoc, routeDoc, upstreamDoc, policyDoc))
}

// largeBundle returns a Bundle of one Gateway and n Routes, Upstreams and
// Policies, about a PBB ladder step of 3n+1 resources.
func largeBundle(t testing.TB, n int) *hub.Bundle {
	t.Helper()
	rs := []*hub.Resource{resourceOf(t, jsonTree(t, gatewayDoc))}
	for i := range n {
		s := strconv.Itoa(i)
		rs = append(rs,
			resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "route-`+s+`", "labels": {"team": "t`+s+`", "tier": "gold"}}, "spec": {"match": {"hosts": ["api.shop.example"], "path": {"template": "/v1/orders/{id}/`+s+`"}, "methods": ["GET", "POST"]}, "policies": [{"name": "policy-`+s+`"}], "upstreams": [{"name": "upstream-`+s+`", "weight": 1}], "timeout": "4s"}}`)),
			resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "upstream-`+s+`"}, "spec": {"protocol": "http", "endpoints": [{"address": "orders-`+s+`.shop.svc:8080", "weight": 1}], "loadBalancing": {"algorithm": "least-request"}, "retries": {"attempts": 2, "retryOn": "error != null ? error.kind in [\"connect\"] : response.status == 503"}, "timeout": "3s"}}`)),
			resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "policy-`+s+`"}, "spec": {"type": "ratelimit", "slot": "policy-`+s+`", "failureMode": "open", "overridable": true, "config": {"key": "consumer == null ? source.ip : consumer.name", "limits": [{"requests": 50, "window": "1s"}], "localOnly": false}}}`)),
		)
	}
	return hub.NewBundle(rs)
}

// TestWorkers: the output does not depend on the worker count, and a
// canceled context stops the encoder.
func TestWorkers(t *testing.T) {
	b := largeBundle(t, 200)
	enc := NewEncoder(hubIndex(t))
	one, err := enc.Encode(context.Background(), b, Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{0, 2, 3, 8, 1000} {
		got, err := enc.Encode(context.Background(), b, Options{Workers: w})
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "workers "+strconv.Itoa(w), got.Content, one.Content)
		if got.Digest != one.Digest {
			t.Errorf("workers %d: digest %s, want %s", w, got.Digest, one.Digest)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, w := range []int{1, 4} {
		if _, err := enc.Encode(ctx, b, Options{Workers: w}); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled, workers %d: error = %v", w, err)
		}
	}
}

// TestParallelErrors: the error of the lowest failing chunk wins, whatever
// the scheduling, and a canceled context stops the hand-out mid-way.
func TestParallelErrors(t *testing.T) {
	b := largeBundle(t, 50)
	rs := b.Resources()
	bad := make([]*hub.Resource, len(rs))
	copy(bad, rs)
	// Two broken Routes; the first in canonical order must be reported.
	for _, i := range []int{len(rs) - 1, len(rs) - 30} {
		r := *bad[i]
		r.Tree = r.Tree.Clone()
		spec, _ := r.Tree.Get("spec")
		spec.Set("timeout", tree.Pos{}, &tree.Node{Kind: tree.KindNull})
		bad[i] = &r
	}
	first := bad[len(rs)-30].String()
	for _, w := range []int{1, 2, 7, 64} {
		_, err := NewEncoder(hubIndex(t)).Encode(context.Background(), hub.NewBundle(bad), Options{Workers: w})
		if !errors.Is(err, ErrEncode) || !strings.Contains(err.Error(), first+" spec.timeout") {
			t.Errorf("workers %d: error = %v, want %s", w, err, first)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := parallel(ctx, 100, 1, 10, func(_, _, _, _ int) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Errorf("parallel after cancel: %v after %d calls", err, calls)
	}
}

// TestReorderInvariance is the reorder property over the example sizes:
// shuffling the members of every object and the order of resources
// leaves the bytes unchanged.
func TestReorderInvariance(t *testing.T) {
	b := largeBundle(t, 20)
	want, err := NewEncoder(hubIndex(t)).Encode(context.Background(), b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test data
	for range 20 {
		rs := b.Resources()
		out := make([]*hub.Resource, len(rs))
		for i, p := range r.Perm(len(rs)) {
			c := *rs[p]
			c.Tree = shuffled(rs[p].Tree, r)
			out[i] = &c
		}
		got, err := NewEncoder(hubIndex(t)).Encode(context.Background(), hub.NewBundle(out), Options{Workers: 3})
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "shuffled", got.Content, want.Content)
	}
}

// TestOverridableFalseSurvives: an explicit overridable: false is written
// and changes the Revision (RL "Presence and defaults").
func TestOverridableFalseSurvives(t *testing.T) {
	doc := `{"kind": "Policy", "metadata": {"name": "ratelimit-global"}, "spec": {"type": "ratelimit", "overridable": %s, "config": {"key": "source.ip"}}}`
	f := encode(t, fmt.Sprintf(doc, "false"))
	tr := encode(t, fmt.Sprintf(doc, "true"))
	if !strings.Contains(string(f), `"overridable":false`) || revision.Sum(f) == revision.Sum(tr) {
		t.Errorf("overridable false lost or digest unchanged:\n%s\n%s", f, tr)
	}
}
