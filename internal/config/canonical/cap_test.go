// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"cmp"
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// capCase is one member of the class of findings a decode caps: gen
// returns content with exactly n findings of code, and want(i) the
// resource and path of finding i, in content order.
type capCase struct {
	name string
	code string
	gen  func(n int) string
	want func(i int) string
}

// num is the i-th of a run of names that sort as they are numbered.
func num(i int) string { return strconv.Itoa(100000 + i) }

// resources returns canonical content of n resources made by one.
func resources(n int, one func(i int) string) string {
	rs := make([]string, n)
	for i := range n {
		rs[i] = one(i)
	}
	return doc(rs...)
}

// members returns n members "<prefix><num(i)>":1 joined by commas.
func members(n int, prefix string) string {
	ms := make([]string, n)
	for i := range n {
		ms[i] = `"` + prefix + num(i) + `":1`
	}
	return strings.Join(ms, ",")
}

// capCases is the class of findings a decode caps: every kind of finding
// that one piece of content can repeat, at every level (document,
// resource, field, list element), of both codes.
func capCases() []capCase {
	route := func(name string) string { return "Route/r" + name }
	return []capCase{
		{
			"non-object elements", CodeMismatch,
			func(n int) string { return header + strings.TrimSuffix(strings.Repeat("1,", n), ",") + trailer },
			func(i int) string { return "resources[" + strconv.Itoa(i) + "]" },
		},
		{
			"resources without kind", CodeMismatch,
			func(n int) string {
				return resources(n, func(i int) string { return `{"metadata":{"name":"r` + num(i) + `"},"spec":{}}` })
			},
			func(i int) string { return "resources[" + strconv.Itoa(i) + "]" },
		},
		{
			"resources without metadata.name", CodeMismatch,
			func(n int) string {
				return resources(n, func(int) string { return `{"kind":"Route","metadata":{},"spec":{}}` })
			},
			func(i int) string { return "resources[" + strconv.Itoa(i) + "]" },
		},
		{
			"apiVersion member on every resource", CodeMismatch,
			func(n int) string {
				return resources(n, func(i int) string {
					return `{"apiVersion":"x","kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{}}`
				})
			},
			func(i int) string { return route(num(i)) + " apiVersion" },
		},
		{
			"Environment kind on every resource", CodeMismatch,
			func(n int) string {
				return resources(n, func(i int) string { return `{"kind":"Environment","metadata":{"name":"e` + num(i) + `"},"spec":{}}` })
			},
			func(i int) string { return "Environment/e" + num(i) + " kind" },
		},
		{
			"unknown resource members", CodeNewer,
			func(n int) string {
				return resources(n, func(i int) string {
					return `{"kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{},"status":{}}`
				})
			},
			func(i int) string { return route(num(i)) + " status" },
		},
		{
			"unknown kinds", CodeNewer,
			func(n int) string {
				return resources(n, func(i int) string { return `{"kind":"Widget","metadata":{"name":"w` + num(i) + `"}}` })
			},
			func(i int) string { return "Widget/w" + num(i) + " kind" },
		},
		{
			"document members after resources", CodeNewer,
			func(n int) string {
				if n == 0 {
					return doc()
				}
				return `{"format":"ruralz.canonical.v1","resources":[],` + members(n, "x") + `}`
			},
			func(i int) string { return "x" + num(i) },
		},
		{
			"document members before format", CodeNewer,
			func(n int) string {
				if n == 0 {
					return doc()
				}
				return `{` + members(n, "a") + `,"format":"ruralz.canonical.v1","resources":[]}`
			},
			func(i int) string { return "a" + num(i) },
		},
		{
			"unknown spec fields of one resource", CodeNewer,
			func(n int) string {
				return doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{` + members(n, "x") + `}}`)
			},
			func(i int) string { return "Route/r spec.x" + num(i) },
		},
		{
			"unknown metadata fields of every resource", CodeNewer,
			func(n int) string {
				return resources(n, func(i int) string {
					return `{"kind":"Route","metadata":{"name":"r` + num(i) + `","owner":"x"},"spec":{}}`
				})
			},
			func(i int) string { return route(num(i)) + " metadata.owner" },
		},
		{
			"unknown members of one list element", CodeNewer,
			func(n int) string {
				sep := ""
				if n > 0 {
					sep = ","
				}
				return doc(`{"kind":"Upstream","metadata":{"name":"u"},"spec":{"endpoints":[{` + members(n, "x") + sep + `"address":"a:1"}]}}`)
			},
			func(i int) string { return "Upstream/u spec.endpoints[address=a:1].x" + num(i) },
		},
		{
			"unknown members of every list element", CodeNewer,
			func(n int) string {
				es := make([]string, n)
				for i := range n {
					es[i] = `{"address":"a` + num(i) + `:1","zz":1}`
				}
				return doc(`{"kind":"Upstream","metadata":{"name":"u"},"spec":{"endpoints":[` + strings.Join(es, ",") + `]}}`)
			},
			func(i int) string { return "Upstream/u spec.endpoints[address=a" + num(i) + ":1].zz" },
		},
		{
			"newer fields of resources without a name", CodeNewer,
			func(n int) string {
				return resources(n, func(int) string { return `{"kind":"Route","metadata":{"title":"x"},"spec":{}}` })
			},
			func(i int) string { return "resources[" + strconv.Itoa(i) + "].metadata.title" },
		},
	}
}

// decodeIn decodes content through mode: Decode, Verify against its own
// digest, or ReadDump of a dump document whose digest and revision match
// it, so verification passes and the decode is reached.
func decodeIn(t *testing.T, dec *Decoder, mode string, content string) error {
	t.Helper()
	ctx := context.Background()
	src := Source{Name: "c.json"}
	switch mode {
	case "Decode":
		_, err := dec.Decode(ctx, []byte(content), src)
		return err
	case "Verify":
		_, err := dec.Verify(ctx, []byte(content), revision.Sum([]byte(content)), src)
		return err
	case "ReadDump":
		d := revision.Sum([]byte(content))
		body := `{"content":` + content + `,"digest":"` + d.String() + `","revision":"` + d.Short() + `"}`
		_, _, err := dec.ReadDump(ctx, strings.NewReader(body), 0, src)
		return err
	default:
		t.Fatalf("mode %q", mode)
		return nil
	}
}

// TestDecodeCapDefault: each member of the class, repeated 10,005 times,
// gives the first 10,000 findings and one RZ-CFG-001
// "too many diagnostics; stopped after 10000", with one and with three
// workers, for Decode, Verify and ReadDump of a hostile dump whose
// digest and revision match its content.
func TestDecodeCapDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("cap test")
	}
	const limit = 10_000
	for _, tc := range capCases() {
		content := tc.gen(limit + 5)
		modes := []string{"Decode"}
		if tc.name == "unknown spec fields of one resource" || tc.name == "non-object elements" {
			modes = append(modes, "Verify", "ReadDump")
		}
		for _, mode := range modes {
			for _, w := range []int{1, 3} {
				err := decodeIn(t, NewDecoder(hubIndex(t), DecoderOptions{Workers: w}), mode, content)
				ds := DiagnosticsOf(err)
				if codeOf(err) != tc.code || len(ds) != limit+1 {
					t.Errorf("%s, %s, workers %d: %s with %d diagnostics, want %s with %d", tc.name, mode, w, codeOf(err), len(ds), tc.code, limit+1)
					continue
				}
				if last := ds[limit]; last.Code != "RZ-CFG-001" || last.Message != "too many diagnostics; stopped after 10000" {
					t.Errorf("%s, %s, workers %d: last diagnostic %+v, want RZ-CFG-001", tc.name, mode, w, last)
				}
				if got := describeOne(ds[limit-1]); got != tc.want(limit-1) {
					t.Errorf("%s, %s, workers %d: last kept finding %q, want %q", tc.name, mode, w, got, tc.want(limit-1))
				}
			}
		}
	}
}

// describeOne returns the resource and path of a finding, as want does.
func describeOne(d diag.Diagnostic) string {
	if r := resText(d); r != "" {
		return r + " " + d.Path.String()
	}
	return d.Path.String()
}

// TestDecodeCap: below, at and past the cap (DecoderOptions.MaxDiagnostics
// 40, across chunk boundaries), every member of the class keeps the first
// findings in content order, appends one CodeTooMany only past the cap,
// marks the Findings truncated, and gives the same findings for every
// worker count and entry point.
func TestDecodeCap(t *testing.T) {
	const limit = 40
	for _, tc := range capCases() {
		for _, n := range []int{1, limit - 1, limit, limit + 1, 3*streamChunk + 7, 300} {
			content := tc.gen(n)
			var first string
			for _, mode := range []string{"Decode", "Verify", "ReadDump"} {
				for _, w := range []int{1, 2, 3, 8} {
					dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: limit})
					err := decodeIn(t, dec, mode, content)
					ds := DiagnosticsOf(err)
					kept := min(n, limit)
					want := kept
					if n > limit {
						want++
					}
					if codeOf(err) != tc.code || len(ds) != want {
						t.Fatalf("%s, n %d, %s, workers %d: %s with %d diagnostics, want %s with %d: %v", tc.name, n, mode, w, codeOf(err), len(ds), tc.code, want, err)
					}
					for i := range kept {
						if got := describeOne(ds[i]); got != tc.want(i) || ds[i].Code != tc.code {
							t.Fatalf("%s, n %d, %s, workers %d: finding %d is %s %q, want %q", tc.name, n, mode, w, i, ds[i].Code, got, tc.want(i))
						}
					}
					f := findingsOf(err)
					truncated := n > limit
					if f.Truncated != truncated {
						t.Errorf("%s, n %d: Truncated = %v, want %v", tc.name, n, f.Truncated, truncated)
					}
					if truncated {
						last := ds[limit]
						if last.Code != CodeTooMany || last.Message != "too many diagnostics; stopped after 40" || last.File != "c.json" || last.Resource != nil || last.Path != nil {
							t.Errorf("%s, n %d: last diagnostic %+v", tc.name, n, last)
						}
						if msg := err.Error(); !strings.HasSuffix(msg, " (and 39 more; too many diagnostics, stopped after 40)") {
							t.Errorf("%s, n %d: Error() = %q", tc.name, n, msg)
						}
					}
					// Positions differ in a dump; the rest does not.
					if mode == "Decode" {
						if got := describe(ds); first == "" {
							first = got
						} else if got != first {
							t.Errorf("%s, n %d, workers %d: findings differ from one worker's", tc.name, n, w)
						}
					}
				}
			}
		}
	}
}

// findingsOf returns the Findings of a decode error.
func findingsOf(err error) *Findings {
	var f *Findings
	if !errors.As(err, &f) {
		return &Findings{}
	}
	return f
}

// TestDecodeCapOutcome: the outcome is a function of the content, whatever
// the worker count, at every point where the parser can stop: more newer
// findings than the cap before invalid JSON or another format give the
// first findings and RZ-CFG-001, also when the findings past the cap lie in
// the elements the parser completed after its last full chunk; fewer give
// the invalid JSON or the format finding alone; a format naming another
// format first is reported alone; form findings past the cap neither stop
// the parse nor hide a later newer finding.
func TestDecodeCapOutcome(t *testing.T) {
	const limit = 40
	status := func(n int) string {
		rs := make([]string, n)
		for i := range n {
			rs[i] = `{"kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{},"status":{}}`
		}
		return strings.Join(rs, ",")
	}
	ones := func(n int) string { return strings.TrimSuffix(strings.Repeat("1,", n), ",") }
	for _, tc := range []struct {
		name, content, code string
		count               int
	}{
		{"newer past the cap, then invalid JSON", header + status(100) + `],}`, CodeNewer, limit + 1},
		{"newer at the cap, then invalid JSON", header + status(limit) + `],}`, CodeMismatch, 1},
		{"newer one past the cap, then invalid JSON", header + status(limit+1) + `],}`, CodeNewer, limit + 1},
		{"newer under the cap, then invalid JSON", header + status(10) + `],}`, CodeMismatch, 1},
		{"newer past the cap in the last elements, then invalid JSON", header + status(50) + `,]}`, CodeNewer, limit + 1},
		{"newer past the cap, then another format", `{"resources":[` + status(100) + `],"format":"ruralz.canonical.v2"}`, CodeNewer, limit + 1},
		{"newer past the cap in the last elements, then another format", `{"resources":[` + status(50) + `],"format":"ruralz.canonical.v2"}`, CodeNewer, limit + 1},
		{"newer under the cap, then another format", `{"resources":[` + status(10) + `],"format":"ruralz.canonical.v2"}`, CodeNewer, 1},
		{"another format, then newer past the cap", `{"format":"ruralz.canonical.v2","resources":[` + status(100) + `]}`, CodeNewer, 1},
		{"another format, then invalid JSON", `{"format":"ruralz.canonical.v2","resources":[,]}`, CodeNewer, 1},
		{"document members past the cap, then invalid JSON", `{` + members(100, "a") + `,}`, CodeNewer, limit + 1},
		{"document members past the cap, then another format", `{` + members(100, "a") + `,"format":"ruralz.canonical.v2"}`, CodeNewer, limit + 1},
		{"form past the cap, then one newer finding", header + ones(100) + `,` + status(1) + trailer, CodeNewer, 1},
		{"form past the cap, then newer past the cap", header + ones(100) + `,` + status(100) + trailer, CodeNewer, limit + 1},
		{"form past the cap, then invalid JSON", header + ones(100) + `,]}`, CodeMismatch, 1},
		{"form past the cap", header + ones(100) + trailer, CodeMismatch, limit + 1},
	} {
		var first string
		for _, w := range []int{1, 2, 3, 8} {
			_, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: limit}).Decode(context.Background(), []byte(tc.content), Source{Name: "c.json"})
			ds := DiagnosticsOf(err)
			if codeOf(err) != tc.code || len(ds) != tc.count {
				t.Errorf("%s, workers %d: %s with %d diagnostics, want %s with %d: %v", tc.name, w, codeOf(err), len(ds), tc.code, tc.count, err)
				continue
			}
			if got := describe(ds); first == "" {
				first = got
			} else if got != first {
				t.Errorf("%s, workers %d: findings differ from one worker's", tc.name, w)
			}
		}
	}
}

// counting is a context that counts its Err calls, from any goroutine.
type counting struct {
	context.Context
	calls atomic.Int64
}

func (c *counting) Err() error {
	c.calls.Add(1)
	return nil
}

// TestDecodeCapStopsParse: once the newer findings passed the cap, the
// parser stops reading elements: a decode of 3,000 resources with a newer
// member each asks the context, once per element read and once per element
// checked, only for the first few chunks, with one worker and with several,
// where reading on would ask it 6,001 times.
func TestDecodeCapStopsParse(t *testing.T) {
	const limit, n = 40, 3000
	content := []byte(capCases()[5].gen(n))
	for _, w := range []int{1, 3} {
		ctx := &counting{Context: context.Background()}
		_, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: limit}).Decode(ctx, content, Source{})
		if codeOf(err) != CodeNewer || len(DiagnosticsOf(err)) != limit+1 {
			t.Fatalf("workers %d: error = %v", w, err)
		}
		// The parser hands out at most w-1 chunks to the channel and w-1 to
		// the workers before checking chunks itself, and stops once the
		// chunks checked hold more than limit findings.
		bound := 1 + 2*(limit+1+2*streamChunk*(2*w+1))
		if got := ctx.calls.Load(); got > int64(bound) {
			t.Errorf("workers %d: the context was asked %d times, want at most %d", w, got, bound)
		}
	}
}

// allocOf returns the bytes one decode of content allocates, the least of
// three runs, so a run that met a stray allocation of another goroutine
// does not count.
func allocOf(dec *Decoder, content []byte) uint64 {
	least := ^uint64(0)
	for range 3 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, _ = dec.Decode(context.Background(), content, Source{})
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least
}

// TestDecodeCapAllocation: past the cap a finding costs no allocation: the
// bytes a decode allocates per repeated element grow no faster with
// findings than without them. Each case measures the slope, in bytes per
// element, between 2,000 and 10,000 elements with the cap at 100, and
// compares it with a control of the same tree shape whose elements give no
// finding (one finding elsewhere keeps the re-encoding off in both), or,
// where the parse stops at the cap, with the bytes of content per element,
// which the parser copies once. What is counted is bytes allocated, not
// time or a sampled heap, and one worker runs, so the measure does not
// depend on the machine or on scheduling. Without the cap each finding past
// it costs 1,400 to 3,000 bytes.
func TestDecodeCapAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation test")
	}
	const n1, n2, margin = 2_000, 10_000, 24
	plugin := func(spec string) string {
		return doc(`{"kind":"Plugin","metadata":{"name":"p","owner":"x"},"spec":` + spec + `}`)
	}
	cases := capCases()
	byName := func(name string) func(int) string {
		for _, c := range cases {
			if c.name == name {
				return c.gen
			}
		}
		t.Fatalf("no case %q", name)
		return nil
	}
	newerFirst := func(gen func(int) string) func(int) string {
		return func(n int) string { return `{"aa":1,` + strings.TrimPrefix(gen(n), "{") }
	}
	for _, tc := range []struct {
		name string
		gen  func(n int) string
		// control has the same tree shape without a finding per element;
		// nil compares with the content bytes per element.
		control func(n int) string
		// limit is the cap gen is decoded with, 100 when zero; the
		// control is decoded with 100.
		limit int
	}{
		{
			"unknown spec fields (the walk stops)", func(n int) string { return plugin(`{` + members(n, "x") + `}`) },
			func(n int) string { return plugin(`{"configSchema":{` + members(n, "x") + `}}`) }, 0,
		},
		{
			"unknown members of one list element (the walk stops)",
			func(n int) string {
				return doc(`{"kind":"Upstream","metadata":{"name":"u","owner":"x"},"spec":{"endpoints":[{` + members(n, "x") + `,"address":"a:1"}]}}`)
			},
			func(n int) string {
				return plugin(`{"configSchema":{"endpoints":[{` + members(n, "x") + `,"address":"a:1"}]}}`)
			}, 0,
		},
		{
			"unknown members of one resource (the member loop stops)",
			func(n int) string {
				return doc(`{"kind":"Plugin","metadata":{"owner":"x"},"spec":{},` + members(n, "x") + `}`)
			},
			func(n int) string {
				return doc(`{"kind":"Plugin","metadata":{"owner":"x"},"spec":{"configSchema":{` + members(n, "x") + `}}}`)
			}, 0,
		},
		{"non-object elements (no form finding is built)", byName("non-object elements"), newerFirst(byName("non-object elements")), 0},
		{"apiVersion members (no form finding is built)", byName("apiVersion member on every resource"), newerFirst(byName("apiVersion member on every resource")), 0},
		{"unknown resource members (the parse stops)", byName("unknown resource members"), nil, 0},
		{"unknown kinds (the parse stops)", byName("unknown kinds"), nil, 0},
		{"document members (the parse stops)", byName("document members after resources"), nil, 0},
		// Under a cap no content reaches, a newer finding still keeps the
		// form findings from being built, since they could never be
		// reported: the slope is that of the same elements past the cap.
		{"non-object elements after a newer finding (no form finding is built)", newerFirst(byName("non-object elements")), byName("non-object elements"), 1 << 20},
	} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1, MaxDiagnostics: cmp.Or(tc.limit, 100)})
		c1, c2 := []byte(tc.gen(n1)), []byte(tc.gen(n2))
		slope := (float64(allocOf(dec, c2)) - float64(allocOf(dec, c1))) / (n2 - n1)
		var base float64
		if tc.control != nil {
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1, MaxDiagnostics: 100})
			k1, k2 := []byte(tc.control(n1)), []byte(tc.control(n2))
			base = (float64(allocOf(dec, k2)) - float64(allocOf(dec, k1))) / (n2 - n1)
		} else {
			base = float64(len(c2)-len(c1)) / (n2 - n1)
		}
		t.Logf("%s: %.1f bytes per element, %.1f without findings", tc.name, slope, base)
		if slope > base+margin {
			t.Errorf("%s: %.1f bytes allocated per element, want at most %.1f + %d: findings past the cap allocate", tc.name, slope, base, margin)
		}
	}
}
