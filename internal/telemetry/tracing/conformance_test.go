// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// field is one request header field line as a client sends it; the name
// is canonicalized as net/http does.
type field struct{ name, value string }

// TestPropagationConformance is the WP-10 propagation conformance table:
// the cases of the W3C Trace Context test suite
// (github.com/w3c/trace-context test/test.py) run through Decide and
// Inject as one proxy hop (spec 09 reqs 27, 28 and 31). "parent" means
// the outgoing traceparent continues the client's trace ID with the
// client's sampled flag; otherwise a new trace starts. state is the
// outgoing tracestate ("" means none).
func TestPropagationConformance(t *testing.T) {
	const (
		tid   = testTraceIDHex
		pid   = testParentIDHex
		other = "4bf92f3577b34da6a3ce929d0e0e4737"
	)
	tp := func(v string) field { return field{"traceparent", v} }
	ts := func(v string) field { return field{"tracestate", v} }
	valid := tp("00-" + tid + "-" + pid + "-01")
	var m32, m33 []string
	for i := range 33 {
		m := "k" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "=1"
		if i < 32 {
			m32 = append(m32, m)
		}
		m33 = append(m33, m)
	}
	for _, tc := range []struct {
		name    string
		fields  []field
		parent  bool
		sampled bool // the client's flag when parent
		state   string
	}{
		{"both missing", nil, false, false, ""},
		{"traceparent without tracestate", []field{valid}, true, true, ""},
		{"traceparent unsampled", []field{tp("00-" + tid + "-" + pid + "-00")}, true, false, ""},
		{"traceparent duplicated", []field{valid, tp("00-" + other + "-" + pid + "-01")}, false, false, ""},
		{"header name trace-parent", []field{{"trace-parent", valid.value}}, false, false, ""},
		{"header name trace.parent", []field{{"trace.parent", valid.value}}, false, false, ""},
		{"header name TraceParent", []field{{"TraceParent", valid.value}}, true, true, ""},
		{"header name TRACEPARENT", []field{{"TRACEPARENT", valid.value}}, true, true, ""},
		{"version 00 trailing dot", []field{tp("00-" + tid + "-" + pid + "-01.")}, false, false, ""},
		{"version 00 future suffix", []field{tp("00-" + tid + "-" + pid + "-01-what-the-future-will-be-like")}, false, false, ""},
		{"version cc", []field{tp("cc-" + tid + "-" + pid + "-01")}, true, true, ""},
		{"version cc future suffix", []field{tp("cc-" + tid + "-" + pid + "-01-what-the-future-will-not-be-like")}, true, true, ""},
		{"version cc bad suffix", []field{tp("cc-" + tid + "-" + pid + "-01.what-the-future-will-not-be-like")}, false, false, ""},
		{"version ff", []field{tp("ff-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"version .0", []field{tp(".0-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"version 0.", []field{tp("0.-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"version too long 000", []field{tp("000-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"version too long 0000", []field{tp("0000-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"version too short", []field{tp("0-" + tid + "-" + pid + "-01")}, false, false, ""},
		{"trace ID all zero", []field{tp("00-" + strings.Repeat("0", 32) + "-" + pid + "-01")}, false, false, ""},
		{"trace ID illegal first", []field{tp("00-." + tid[1:] + "-" + pid + "-01")}, false, false, ""},
		{"trace ID illegal last", []field{tp("00-" + tid[:31] + ".-" + pid + "-01")}, false, false, ""},
		{"trace ID too long", []field{tp("00-0" + tid + "-" + pid + "-01")}, false, false, ""},
		{"trace ID too short", []field{tp("00-" + tid[1:] + "-" + pid + "-01")}, false, false, ""},
		{"parent ID all zero", []field{tp("00-" + tid + "-" + strings.Repeat("0", 16) + "-01")}, false, false, ""},
		{"parent ID illegal first", []field{tp("00-" + tid + "-." + pid[1:] + "-01")}, false, false, ""},
		{"parent ID illegal last", []field{tp("00-" + tid + "-" + pid[:15] + ".-01")}, false, false, ""},
		{"parent ID too long", []field{tp("00-" + tid + "-0" + pid + "-01")}, false, false, ""},
		{"parent ID too short", []field{tp("00-" + tid + "-" + pid[1:] + "-01")}, false, false, ""},
		{"flags illegal first", []field{tp("00-" + tid + "-" + pid + "-.0")}, false, false, ""},
		{"flags illegal last", []field{tp("00-" + tid + "-" + pid + "-0.")}, false, false, ""},
		{"flags too long", []field{tp("00-" + tid + "-" + pid + "-001")}, false, false, ""},
		{"flags too short", []field{tp("00-" + tid + "-" + pid + "-1")}, false, false, ""},
		{"OWS leading space", []field{tp(" " + valid.value)}, true, true, ""},
		{"OWS leading tab", []field{tp("\t" + valid.value)}, true, true, ""},
		{"OWS trailing space", []field{tp(valid.value + " ")}, true, true, ""},
		{"OWS trailing tab", []field{tp(valid.value + "\t")}, true, true, ""},
		{"OWS both", []field{tp("\t " + valid.value + " \t")}, true, true, ""},
		{"tracestate without traceparent", []field{ts("foo=1")}, false, false, ""},
		{"tracestate empty", []field{valid, ts("")}, true, true, ""},
		{"tracestate kept", []field{valid, ts("foo=1,bar=2")}, true, true, "foo=1,bar=2"},
		{"tracestate lines combined", []field{valid, ts("foo=1,bar=2"), ts("rojo=1,congo=2"), ts("baz=3")}, true, true, "foo=1,bar=2,rojo=1,congo=2,baz=3"},
		{"tracestate duplicated keys", []field{valid, ts("foo=1,foo=1")}, true, true, ""},
		{"tracestate duplicated keys values", []field{valid, ts("foo=1,foo=2")}, true, true, ""},
		{"tracestate duplicated across lines", []field{valid, ts("foo=1"), ts("foo=1")}, true, true, ""},
		{"tracestate all allowed characters", []field{valid, ts("abcdefghijklmnopqrstuvwxyz0123456789_-*/= !\"#$%&'()*+-./0123456789:;<>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~")}, true, true, "abcdefghijklmnopqrstuvwxyz0123456789_-*/= !\"#$%&'()*+-./0123456789:;<>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"},
		{"tracestate multi-tenant key", []field{valid, ts("abcdefghijklmnopqrstuvwxyz0123456789_-*/@a-z0-9_-*/=1")}, true, true, "abcdefghijklmnopqrstuvwxyz0123456789_-*/@a-z0-9_-*/=1"},
		{"tracestate OWS", []field{valid, ts("foo=1 \t , \t bar=2, \t baz=3")}, true, true, "foo=1 \t , \t bar=2, \t baz=3"},
		{"tracestate key with space", []field{valid, ts("foo =1")}, true, true, ""},
		{"tracestate key uppercase", []field{valid, ts("FOO=1")}, true, true, ""},
		{"tracestate key with dot", []field{valid, ts("foo.bar=1")}, true, true, ""},
		{"tracestate vendor empty system", []field{valid, ts("foo@=1,bar=2")}, true, true, ""},
		{"tracestate vendor empty tenant", []field{valid, ts("@foo=1,bar=2")}, true, true, ""},
		{"tracestate vendor double @", []field{valid, ts("foo@@bar=1,bar=2")}, true, true, ""},
		{"tracestate vendor two @", []field{valid, ts("foo@bar@baz=1,bar=2")}, true, true, ""},
		{"tracestate 32 members", []field{valid, ts(strings.Join(m32, ","))}, true, true, strings.Join(m32, ",")},
		{"tracestate 33 members", []field{valid, ts(strings.Join(m33, ","))}, true, true, ""},
		{"tracestate key 256", []field{valid, ts("z" + strings.Repeat("a", 255) + "=1")}, true, true, "z" + strings.Repeat("a", 255) + "=1"},
		{"tracestate key 257", []field{valid, ts("z" + strings.Repeat("a", 256) + "=1")}, true, true, ""},
		{"tracestate value with =", []field{valid, ts("foo=bar=baz")}, true, true, ""},
		{"tracestate empty value", []field{valid, ts("foo=,bar=3")}, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTracer(t, Options{})
			in := http.Header{}
			for _, f := range tc.fields {
				in.Add(f.name, f.value)
			}
			var d emit.Decision
			tr.Decide(in, 1, &d)
			// The proxy forwards a copy of the client's fields; Inject
			// must replace them.
			out := in.Clone()
			tr.Inject(context.Background(), &d, out)

			got := out.Values(HeaderTraceparent)
			if len(got) != 1 {
				t.Fatalf("outgoing traceparent = %q, want one value", got)
			}
			outTID, outPID, flags, ok := ParseTraceparent(got[0])
			if !ok || len(got[0]) != TraceparentLen || got[0][:3] != "00-" {
				t.Fatalf("outgoing traceparent %q is not a version 00 value", got[0])
			}
			if outTID != d.TraceID || outPID != d.ServerSpanID {
				t.Fatalf("outgoing traceparent %q does not carry the decision %x/%x", got[0], d.TraceID, d.ServerSpanID)
			}
			continued := FormatTraceID(outTID) == tid
			if continued != tc.parent || d.Remote != tc.parent {
				t.Fatalf("continued trace = %v (Remote %v), want %v", continued, d.Remote, tc.parent)
			}
			if tc.parent {
				if d.ParentSpanID != mustHex8(t, pid) {
					t.Fatalf("parent span ID = %x", d.ParentSpanID)
				}
				if outPID == d.ParentSpanID {
					t.Fatal("outgoing parent ID is the client's span ID")
				}
				if want := tc.sampled; (flags&FlagSampled != 0) != want || d.Sampled != want {
					t.Fatalf("flags %02x, Sampled %v, want sampled %v", flags, d.Sampled, want)
				}
			} else if !d.Sampled || flags != FlagSampled {
				// A new trace at ratio 1 is sampled within the root cap.
				t.Fatalf("new trace: flags %02x, Sampled %v, want sampled", flags, d.Sampled)
			}
			if st := out.Values(HeaderTracestate); strings.Join(st, ",") != tc.state || (tc.state != "" && len(st) != 1) {
				t.Fatalf("outgoing tracestate = %q, want %q", st, tc.state)
			}
			if d.TraceState != tc.state {
				t.Fatalf("Decision.TraceState = %q, want %q", d.TraceState, tc.state)
			}
			if out.Get("Baggage") != in.Get("Baggage") {
				t.Fatal("baggage changed")
			}
		})
	}
}
