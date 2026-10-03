// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package promtext

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Tests for 11 test plan item 9 (promtext: parser golden, histogram
// quantile interpolation, label matching) and the scrape path the chaos and
// end-to-end assertions use (11 req 38, H 49: every degraded state visible
// as a metric).

func golden(t *testing.T) Set {
	t.Helper()
	f, err := os.Open("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	s, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseGoldenFamilies(t *testing.T) { // 11 test plan item 9
	s := golden(t)
	want := map[string]string{
		"ruralz_config_activations_total":      TypeCounter,
		"ruralz_node_degraded_info":            TypeGauge,
		"ruralz_http_gateway_duration_seconds": TypeHistogram,
		"ruralz_runtime_goroutines":            TypeGauge,
		"rpc_latency_seconds":                  TypeSummary,
		"target_info":                          TypeGauge,
		"untyped_metric":                       TypeUntyped,
		"another_untyped":                      TypeUntyped,
	}
	if got := s.Names(); len(got) != len(want) {
		t.Fatalf("Names = %v, want %d families", got, len(want))
	}
	for name, typ := range want {
		f, ok := s.Family(name)
		if !ok {
			t.Fatalf("family %s missing", name)
		}
		if f.Type != typ {
			t.Errorf("%s type = %q, want %q", name, f.Type, typ)
		}
	}
	h, _ := s.Family("ruralz_http_gateway_duration_seconds")
	if len(h.Samples) != 12 {
		t.Fatalf("histogram samples = %d, want 12", len(h.Samples))
	}
	sum, _ := s.Family("rpc_latency_seconds")
	if len(sum.Samples) != 4 {
		t.Fatalf("summary samples = %d, want 4", len(sum.Samples))
	}
	ti, _ := s.Family("target_info")
	if ti.Help != "Target metadata with an escaped \"help\" \\ line\nsecond line." {
		t.Fatalf("help = %q", ti.Help)
	}
	if got := ti.Samples[0].Labels["note"]; got != "quote \" backslash \\ newline \n end" {
		t.Fatalf("escaped label = %q", got)
	}
	u := s.Samples("untyped_metric", nil)
	if len(u) != 1 || !math.IsInf(u[0].Value, -1) || !u[0].HasTimestamp || u[0].Timestamp != 1700000000000 {
		t.Fatalf("untyped sample = %+v", u)
	}
	if v, err := s.Value("another_untyped", nil); err != nil || !math.IsNaN(v) {
		t.Fatalf("NaN sample = %v, %v", v, err)
	}
}

func TestLabelMatching(t *testing.T) { // 11 test plan item 9: label matching
	s := golden(t)
	tests := []struct {
		name  string
		match Labels
		value float64
		err   bool
		sum   float64
	}{
		{"exact", L("result", "activated", "code", ""), 3, false, 3},
		{"subset ambiguous", L("result", "rejected"), 0, true, 1},
		{"code", L("code", "RZ-CFG-005"), 1, false, 1},
		{"absent label matches empty", L("code", "", "result", "activated"), 3, false, 3},
		{"no match", L("result", "nope"), 0, true, 0},
		{"all", nil, 0, true, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := s.Value("ruralz_config_activations_total", tt.match)
			if (err != nil) != tt.err {
				t.Fatalf("Value err = %v, want error %v", err, tt.err)
			}
			if err == nil && v != tt.value {
				t.Fatalf("Value = %v, want %v", v, tt.value)
			}
			if got := s.Sum("ruralz_config_activations_total", tt.match); got != tt.sum {
				t.Fatalf("Sum = %v, want %v", got, tt.sum)
			}
			if got, want := s.Has("ruralz_config_activations_total", tt.match), tt.name != "no match"; got != want {
				t.Fatalf("Has = %v, want %v", got, want)
			}
		})
	}
	if v, err := s.Value("ruralz_node_degraded_info", L("reason", "lkg_boot")); err != nil || v != 1 {
		t.Fatalf("degraded lkg_boot = %v, %v", v, err)
	}
}

func TestDelta(t *testing.T) {
	before, err := ParseBytes([]byte("c_total{a=\"x\"} 5\nc_total{a=\"y\"} 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseBytes([]byte("c_total{a=\"x\"} 9\nc_total{a=\"y\"} 1\nc_total{a=\"z\"} 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := Delta(before, after, "c_total", nil); d != 6 {
		t.Fatalf("Delta = %v, want 6", d)
	}
	if d := Delta(before, after, "c_total", L("a", "z")); d != 2 {
		t.Fatalf("Delta new series = %v, want 2", d)
	}
}

func TestHistogramQuantile(t *testing.T) { // 11 test plan item 9: quantile interpolation
	s := golden(t)
	orders, err := s.Histogram("ruralz_http_gateway_duration_seconds", L("route", "orders"))
	if err != nil {
		t.Fatal(err)
	}
	if orders.Count != 100 || orders.Sum != 0.123 || len(orders.Buckets) != 4 {
		t.Fatalf("histogram = %+v", orders)
	}
	both, err := s.Histogram("ruralz_http_gateway_duration_seconds", nil)
	if err != nil {
		t.Fatal(err)
	}
	if both.Count != 200 || both.Buckets[1].Count != 100 {
		t.Fatalf("aggregated histogram = %+v", both)
	}
	tests := []struct {
		h    Histogram
		q    float64
		want float64
	}{
		{orders, 0.05, 0.00025}, // first bucket starts at 0: 5/10 of 0.0005
		{orders, 0.1, 0.0005},   // exactly the first bucket's count
		{orders, 0.5, 0.0009},   // 0.0005 + 0.0005*(40/50)
		{orders, 0.9, 0.005},    // exactly the third bucket
		{orders, 0.99, 0.005},   // in +Inf: highest finite bound
		{orders, 0, 0},          // rank 0 in the first bucket
		{orders, 1, 0.005},      // rank = total, in +Inf
		{both, 0.5, 0.001},      // buckets 10, 100, 180, 200: rank 100 fills the second
		{orders, -0.1, math.Inf(-1)},
		{orders, 1.1, math.Inf(1)},
	}
	for _, tt := range tests {
		if got := tt.h.Quantile(tt.q); math.Abs(got-tt.want) > 1e-12 && got != tt.want {
			t.Errorf("Quantile(%v) = %v, want %v", tt.q, got, tt.want)
		}
	}
	if !math.IsNaN(orders.Quantile(math.NaN())) {
		t.Fatal("Quantile(NaN) is not NaN")
	}
}

func TestHistogramEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		h    Histogram
		q    float64
		nan  bool
		want float64
	}{
		{"no buckets", Histogram{}, 0.5, true, 0},
		{"no +Inf", Histogram{Buckets: []Bucket{{1, 1}, {2, 2}}}, 0.5, true, 0},
		{"empty", Histogram{Buckets: []Bucket{{1, 0}, {math.Inf(1), 0}}}, 0.5, true, 0},
		{"negative first bound", Histogram{Buckets: []Bucket{{-1, 5}, {math.Inf(1), 10}}}, 0.1, false, -1},
		{"non-monotonic fixed", Histogram{Buckets: []Bucket{{1, 5}, {2, 3}, {math.Inf(1), 10}}}, 0.4, false, 0.8},
		{"empty middle bucket", Histogram{Buckets: []Bucket{{1, 5}, {2, 5}, {math.Inf(1), 10}}}, 0.5, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.h.Quantile(tt.q)
			if tt.nan != math.IsNaN(got) || (!tt.nan && math.Abs(got-tt.want) > 1e-12) {
				t.Fatalf("Quantile = %v, want %v (NaN %v)", got, tt.want, tt.nan)
			}
		})
	}
	s, err := ParseBytes([]byte("h_bucket{le=\"1\"} 1\nh_bucket{le=\"x\"} 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Histogram("h", nil); err == nil {
		t.Fatal("bad le: want error")
	}
	s, _ = ParseBytes([]byte("h_bucket{le=\"1\"} 1\n"))
	if _, err := s.Histogram("h", nil); err == nil {
		t.Fatal("no +Inf bucket: want error")
	}
	s, _ = ParseBytes([]byte("h_bucket{x=\"1\"} 1\n"))
	if _, err := s.Histogram("h", nil); err == nil {
		t.Fatal("no le: want error")
	}
	if _, err := s.Histogram("missing", nil); err == nil {
		t.Fatal("missing histogram: want error")
	}
}

func TestParseOpenMetricsAndUTF8(t *testing.T) {
	in := `# TYPE req counter
# UNIT req requests
# HELP req Requests.
req_total{code="200"} 7 # {trace_id="abc"} 1 1.5
req_created{code="200"} 1.7e9
# TYPE "my.metric" gauge
{"my.metric", "service.name"="x"} 2
{"other.metric"} 3 1.25
# EOF
`
	s, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.Family("req")
	if f.Type != TypeCounter || f.Unit != "requests" || len(f.Samples) != 2 {
		t.Fatalf("req family = %+v", f)
	}
	if v, err := s.Value("req_total", L("code", "200")); err != nil || v != 7 {
		t.Fatalf("req_total = %v, %v", v, err)
	}
	if v, err := s.Value("my.metric", L("service.name", "x")); err != nil || v != 2 {
		t.Fatalf("my.metric = %v, %v", v, err)
	}
	o := s.Samples("other.metric", nil)
	if len(o) != 1 || o[0].Timestamp != 1250 {
		t.Fatalf("other.metric = %+v", o)
	}
	if _, err := ParseBytes([]byte(in + "x 1\n")); err == nil {
		t.Fatal("content after # EOF: want error")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []string{
		"x{a=\"1\" 1\n",
		"x{a=1} 1\n",
		"x{a=\"1\",a=\"2\"} 1\n",
		"x{1a=\"1\"} 1\n",
		"x{a\"1\"} 1\n",
		"x{a=\"\\q\"} 1\n",
		"x{a=\"unterminated} 1\n",
		"x{a=\"1\" b=\"2\"} 1\n",
		"x notanumber\n",
		"x 1 2 3\n",
		"x 1 notatime\n",
		"x\n",
		"{} 1\n",
		"{\"a\",\"b\"} 1\n",
		"# TYPE x bogus\n",
		"# TYPE x gauge\n# TYPE x gauge\n",
		"x 1\n# TYPE x gauge\n",
		"# TYPE 1x gauge\n",
		"x{a=\"1\"}1\n",
		"{\"unterminated 1\n",
		"x{a=\"1\\\n",
	}
	for _, in := range tests {
		if _, err := ParseBytes([]byte(in)); err == nil {
			t.Errorf("Parse(%q): want error", in)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("Parse(%q) error %q lacks the line", in, err)
		}
	}
}

func TestParseTolerances(t *testing.T) {
	in := "\n  \n# just a comment\n# HELP only_help Help without type.\nonly_help 1\r\nx{a=\"1\",} 2\n\tspaced   3   \n"
	s, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Family("only_help"); f.Type != TypeUntyped || f.Help != "Help without type." {
		t.Fatalf("only_help = %+v", f)
	}
	if v, err := s.Value("x", L("a", "1")); err != nil || v != 2 {
		t.Fatalf("trailing comma: %v, %v", v, err)
	}
	if v, err := s.Value("spaced", nil); err != nil || v != 3 {
		t.Fatalf("spaced: %v, %v", v, err)
	}
	if got := L("b", "2", "a", "q\"\\\n", "odd").String(); got != `{a="q\"\\\n",b="2"}` {
		t.Fatalf("Labels.String = %s", got)
	}
}

func TestScrape(t *testing.T) { // 11 req 38: /metrics parsed during e2e assertions
	body, err := os.ReadFile("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "version=0.0.4") {
			http.Error(w, "bad accept", http.StatusNotAcceptable)
			return
		}
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write(body)
		default:
			_, _ = w.Write([]byte("broken{ 1\n"))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	h := http.Header{"Authorization": {"Bearer tok"}}
	s, err := Scrape(ctx, srv.Client(), srv.URL+"/metrics", h)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value("ruralz_runtime_goroutines", nil); err != nil || v != 42 {
		t.Fatalf("goroutines = %v, %v", v, err)
	}
	if _, err := Scrape(ctx, nil, srv.URL+"/metrics", nil); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("unauthorized scrape err = %v", err)
	}
	if _, err := Scrape(ctx, srv.Client(), srv.URL+"/broken", h); err == nil {
		t.Fatal("broken body: want error")
	}
	if _, err := Scrape(ctx, srv.Client(), "http://127.0.0.1:1/metrics", h); err == nil {
		t.Fatal("refused scrape: want error")
	}
	if _, err := Scrape(ctx, srv.Client(), "://bad", h); err == nil {
		t.Fatal("bad URL: want error")
	}
}

func FuzzParse(f *testing.F) {
	golden, err := os.ReadFile("testdata/metrics.txt")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(golden)
	f.Add([]byte(`{"a.b","c"="d"} 1 # {x="y"} 2`))
	f.Add([]byte("# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseBytes(data)
		if err != nil {
			return
		}
		// Every sample is reachable through its family and its name.
		n := 0
		for _, name := range s.Names() {
			fam, _ := s.Family(name)
			n += len(fam.Samples)
			for _, smp := range fam.Samples {
				if len(s.Samples(smp.Name, smp.Labels)) == 0 {
					t.Fatalf("sample %s%s not indexed", smp.Name, smp.Labels)
				}
			}
			if h, err := s.Histogram(name, nil); err == nil {
				_ = h.Quantile(0.99)
			}
		}
		total := 0
		for _, v := range s.bySample {
			total += len(v)
		}
		if n != total {
			t.Fatalf("family samples %d != indexed samples %d", n, total)
		}
	})
}

func BenchmarkParse(b *testing.B) {
	var sb strings.Builder
	for i := range 2000 {
		sb.WriteString("ruralz_http_requests_total{route=\"r")
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString("\",code=\"200\",method=\"GET\"} 12345\n")
	}
	data := []byte(sb.String())
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseBytes(data); err != nil {
			b.Fatal(err)
		}
	}
}
