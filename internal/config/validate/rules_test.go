// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestClipAndCause: messages quote at most 64 code points of an authored
// value and stay bounded whatever the value, and RE2 errors name the
// failure without quoting the whole expression.
func TestClipAndCause(t *testing.T) {
	if got := clip("short"); got != "short" {
		t.Errorf("clip(short) = %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := clip(long); utf8.RuneCountInString(got) != maxQuoted+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("clip(100 é) = %q", got)
	}
	if got := clip("a\xffb"); got != "a�b" {
		t.Errorf("clip(invalid UTF-8) = %q", got)
	}
	host := strings.Repeat("a", 100_000) + ".*.example"
	err := routematch.CheckHost(host)
	if err == nil {
		t.Fatal("CheckHost accepted a misplaced *")
	}
	msg := causeText(err, host)
	if len(msg) > maxMessage+3 || strings.Contains(msg, "RZ-CFG-005") {
		t.Errorf("causeText of a 100 kB host: %d bytes, %.80q", len(msg), msg)
	}
	if got := limitMessage(strings.Repeat("é", maxMessage)); len(got) > maxMessage+3 || !utf8.ValidString(got) {
		t.Errorf("limitMessage cut inside a code point or past the bound: %d bytes", len(got))
	}
	_, rerr := routematch.CompileRegex("(" + strings.Repeat("a", 10_000))
	if !isRegexError(rerr) {
		t.Fatal("isRegexError missed a regexp/syntax error")
	}
	if got := regexText(rerr); len(got) > 200 || !strings.HasPrefix(got, "invalid RE2 expression: missing closing ): ") {
		t.Errorf("regexText = %.120q (%d bytes)", got, len(got))
	}
	if isRegexError(fmt.Errorf("plain")) {
		t.Error("isRegexError accepted a plain error")
	}
}

// TestHostileValuesBounded: stage H on a Route whose host is 1 MB long
// reports one finding whose message stays bounded.
func TestHostileValuesBounded(t *testing.T) {
	files := readDirs(t, baseDir)
	files["extra.yaml"] = "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: hostile\nspec:\n  match:\n    hosts: [\"" +
		strings.Repeat("a", 1<<20) + ".*.example\"]\n  upstreams:\n    - name: orders\n"
	b, table := loadFiles(t, files)
	_, ds := stageH(t, newValidator(t, nil), b, table, 2)
	if len(ds) != 1 || len(ds[0].Message) > maxMessage+3 {
		t.Fatalf("%d findings, message of %d bytes", len(ds), len(ds[0].Message))
	}
}

// TestNameHints: a hint names the nearest candidate within 3 bytes of the
// input's length, the same length first; names past maxHintBytes get none.
func TestNameHints(t *testing.T) {
	s := newNameSet([]string{"orders", "inventory", "order", "orders-v2", strings.Repeat("x", 200)})
	tests := []struct{ in, want string }{
		{"ordrs", `did you mean "orders"?`},
		{"inventroy", `did you mean "inventory"?`},
		{"ORDERS", `did you mean "orders"?`},
		{"zzzzzz", ""},
		{strings.Repeat("x", 199), ""},
	}
	for _, tt := range tests {
		if got := s.hint(tt.in); got != tt.want {
			t.Errorf("hint(%.20q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	var nilSet *nameSet
	if got := nilSet.hint("x"); got != "" {
		t.Errorf("nil set hint = %q", got)
	}
}

// TestKeeper: the keeper holds the first findings in sort order, ties in
// report order, whatever the order and goroutine of the adds.
func TestKeeper(t *testing.T) {
	var all []*finding
	for i := range 200 {
		d := diag.Diagnostic{Code: "RZ-CFG-009", Location: diag.Location{File: "f", Line: i % 17}, Message: "m"}
		all = append(all, &finding{d: d, path: "", seq: uint64(i)})
	}
	k := newKeeper(25)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := g; i < len(all); i += 4 {
				k.add(all[i])
			}
		})
	}
	wg.Wait()
	got := k.list()
	if len(got) != 25 {
		t.Fatalf("kept %d, want 25", len(got))
	}
	sorted := slices.Clone(all)
	slices.SortFunc(sorted, compareFindings)
	for i, d := range got {
		if d.Line != sorted[i].d.Line {
			t.Fatalf("finding %d on line %d, want %d: not the first 25 in order", i, d.Line, sorted[i].d.Line)
		}
	}
	// Lines 0 and 1 hold 12 findings each, so the last kept is line 2's
	// first report.
	if got[24].Line != 2 {
		t.Errorf("last kept line %d, want 2", got[24].Line)
	}
	if newKeeper(0).limit != 1 {
		t.Error("a keeper limit below 1 is not raised to 1")
	}
}

// TestFinder: a path is located at its node, or at the nearest authored
// ancestor for a missing node or one stage G materialized; keyed and item
// steps find the first matching entry.
func TestFinder(t *testing.T) {
	pos := func(line int32) tree.Pos { return tree.Pos{Line: line, Column: 1} }
	str := func(s string, line int32) *tree.Node {
		return &tree.Node{Kind: tree.KindString, Text: s, Pos: pos(line)}
	}
	entry := func(name string, line int32) *tree.Node {
		return &tree.Node{Kind: tree.KindMap, Pos: pos(line), Members: []tree.Member{{Key: "name", Value: str(name, line)}}}
	}
	big := &tree.Node{Kind: tree.KindMap, Pos: pos(30)}
	for i := range 20 {
		big.Members = append(big.Members, tree.Member{Key: fmt.Sprintf("k%02d", i), Value: str("v", 31+int32(i))})
	}
	defaulted := &tree.Node{Kind: tree.KindMap, Style: tree.StyleDefaulted, Members: []tree.Member{
		{Key: "port", Value: &tree.Node{Kind: tree.KindInt, Text: "9901", Style: tree.StyleDefaulted}},
	}}
	root := &tree.Node{Kind: tree.KindMap, Pos: pos(1), Members: []tree.Member{
		{Key: "spec", Value: &tree.Node{Kind: tree.KindMap, Pos: pos(2), Members: []tree.Member{
			{Key: "list", Value: &tree.Node{Kind: tree.KindList, Pos: pos(3), Items: []*tree.Node{entry("a", 4), entry("b", 5), entry("a", 6)}}},
			{Key: "set", Value: &tree.Node{Kind: tree.KindList, Pos: pos(7), Items: []*tree.Node{str("x", 8), str("y", 9)}}},
			{Key: "admin", Value: defaulted},
			{Key: "big", Value: big},
		}}},
	}}
	r := &hub.Resource{
		ID: hub.ID{Kind: "Gateway", Name: "edge"}, Tree: root,
		Source: hub.Source{File: "g.yaml", Start: diag.Location{File: "g.yaml", Line: 1, Column: 1}},
	}
	f := locator{}.newFinder(r, &work{})
	spec := diag.Path{diag.Field("spec")}
	tests := []struct {
		name string
		p    diag.Path
		line int
	}{
		{"keyed first match", spec.Append(diag.Field("list"), diag.Keyed("name", "a")), 4},
		{"keyed second", spec.Append(diag.Field("list"), diag.Keyed("name", "b"), diag.Field("name")), 5},
		{"index", spec.Append(diag.Field("list"), diag.Index(2)), 6},
		{"item", spec.Append(diag.Field("set"), diag.Item("y")), 9},
		{"missing keyed falls back to the list", spec.Append(diag.Field("list"), diag.Keyed("name", "zz")), 3},
		{"missing member falls back", spec.Append(diag.Field("nothing"), diag.Field("deeper")), 2},
		{"defaulted falls back to the authored parent", spec.Append(diag.Field("admin"), diag.Field("port")), 2},
		{"indexed map", spec.Append(diag.Field("big"), diag.Field("k17")), 48},
		{"out of range index", spec.Append(diag.Field("list"), diag.Index(9)), 3},
	}
	for _, tt := range tests {
		if got := f.path(tt.p); got.Line != tt.line || got.File != "g.yaml" {
			t.Errorf("%s: path(%s) = %+v, want g.yaml line %d", tt.name, tt.p, got, tt.line)
		}
	}
	if n := f.node(spec.Append(diag.Field("admin"), diag.Field("port"))); n == nil || n.Text != "9901" {
		t.Errorf("node of a defaulted field = %v", n)
	}
	noTree := &hub.Resource{Source: hub.Source{Start: diag.Location{File: "s.yaml", Line: 7}}}
	if got := (locator{}).newFinder(noTree, &work{}).path(spec); got.File != "s.yaml" || got.Line != 7 {
		t.Errorf("a resource without a tree is located at %+v, want its start", got)
	}
}

// TestDestinationReadsTreeWithoutConfig: a clientSecret's destination
// comes from the decoded config, or from spec.config.tokenUrl in the tree
// when the config is not decoded, so the binding never falls back to
// local.
func TestDestinationReadsTreeWithoutConfig(t *testing.T) {
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	cfg := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "tokenUrl", Value: str("https://Auth.Example/token")}}}
	spec := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "config", Value: cfg}}}
	r := &hub.Resource{ID: hub.ID{Kind: "Policy", Name: "p"}, Tree: &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "spec", Value: spec}}}}
	u := &unit{r: r, f: locator{}.newFinder(r, &work{})}
	if got := u.destination(secretClass{dest: destTokenURL}); got != "https://auth.example:443" {
		t.Errorf("destination from the tree = %q", got)
	}
	if got := u.destination(secretClass{dest: destStateStore}); got != hub.DestinationStateStore {
		t.Errorf("State Store destination = %q", got)
	}
	if got := u.destination(secretClass{dest: destLocal}); got != hub.DestinationLocal {
		t.Errorf("local destination = %q", got)
	}
}

// TestNetaddrShared: stage H accepts exactly the Endpoint addresses, dns
// discovery names and ports, and OTLP endpoints that internal/netaddr,
// and so the Upstream layer and the Node's telemetry, accept, and states
// netaddr's reason for each one it refuses.
func TestNetaddrShared(t *testing.T) {
	v := newValidator(t, nil)
	// check runs stage H on one typed resource and compares its findings
	// with want, the one message netaddr's verdict implies, or none.
	check := func(what string, r *hub.Resource, want string) {
		t.Helper()
		_, ds := stageH(t, v, hub.NewBundle([]*hub.Resource{r}), nil, 1)
		switch {
		case want == "" && len(ds) > 0:
			t.Errorf("%s: stage H refuses what netaddr accepts:\n%s", what, diagText(t, ds))
		case want != "" && (len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Message != want):
			t.Errorf("%s: findings:\n%swant one RZ-CFG-005 %q", what, diagText(t, ds), want)
		}
	}
	upstream := func(sp v1alpha1.UpstreamSpec) *hub.Resource {
		sp.Protocol = v1alpha1.UpstreamProtocolHTTP
		return typedResource(v1alpha1.KindUpstream, "u", &v1alpha1.Upstream{Spec: sp}, 1)
	}
	for _, a := range []string{
		"10.0.0.1:8080", "[2001:db8::1]:443", "[::ffff:10.0.0.1]:80", "[fe80::1%eth0]:80",
		"api.internal:80", "API.internal.:080", "_svc.example:80", "api.internal",
		"api.internal:0", "api.internal:65536", "api.internal:+80", "api.internal:http",
		":80", "a b:80", "a..b:80", strings.Repeat("a", 64) + ".x:80", "[::1]", "::1:80",
	} {
		want := ""
		if _, err := netaddr.ParseHostPort(a); err != nil {
			want = "endpoint address " + quote(a) + ": " + err.Error()
		}
		check(fmt.Sprintf("endpoint %q", a), upstream(v1alpha1.UpstreamSpec{Endpoints: []v1alpha1.Endpoint{{Address: a}}}), want)
	}
	dns := func(service string, port v1alpha1.IntOrString) *hub.Resource {
		return upstream(v1alpha1.UpstreamSpec{Discovery: &v1alpha1.Discovery{Type: v1alpha1.DiscoveryTypeDNS, Service: service, Port: &port}})
	}
	for _, n := range []string{
		"orders.svc", "orders.svc.", "_http._tcp.orders", "10.0.0.1", "", ".", "a..b", "bad name",
		strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a.", 126) + "a", strings.Repeat("a.", 127) + "a",
	} {
		want := ""
		if err := netaddr.CheckName(n); err != nil {
			want = "dns discovery needs a service DNS name: " + err.Error()
		}
		check(fmt.Sprintf("service %q", n), dns(n, v1alpha1.IntOrString{Int: 80}), want)
	}
	for _, p := range []string{"http", "grpc-web", "h2c", "h_ttp", "8080", "-", "a", "abcdefghijklmno", "abcdefghijklmnop"} {
		want := ""
		if err := netaddr.CheckPortName(p); err != nil {
			want = "discovery.port " + quote(p) + ": " + err.Error()
		}
		check(fmt.Sprintf("port name %q", p), dns("orders.svc", v1alpha1.IntOrString{Str: p, IsString: true}), want)
	}
	for _, n := range []int32{-1, 0, 1, 80, 65535, 65536} {
		want := ""
		if err := netaddr.CheckPort(int64(n)); err != nil {
			want = fmt.Sprintf("discovery.port %d: %v", n, err)
		}
		check(fmt.Sprintf("port %d", n), dns("orders.svc", v1alpha1.IntOrString{Int: n}), want)
	}
	for _, e := range []string{
		"https://collector:4317", "https://collector", "https://collector/", "HTTPS://Collector:4318",
		"http://127.0.0.1:9000", "https://[::1]:4317", "https://[fe80::1%25eth0]:4317",
		"grpc://collector", "https://user@collector", "https://collector/v1/traces", "https://collector?",
		"https://collector?x=1", "https://collector#f", "https://collector:", "collector:4317",
		"https://", "https:collector", "https://%zz", " https://collector",
	} {
		want := ""
		if _, err := netaddr.ParseOTLPEndpoint(e); err != nil {
			want = err.Error()
		}
		gw := &v1alpha1.Gateway{Spec: v1alpha1.GatewaySpec{Telemetry: &v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{Endpoint: e}}}}
		check(fmt.Sprintf("otlp endpoint %q", e), typedResource(v1alpha1.KindGateway, "edge", gw, 1), want)
	}
}
