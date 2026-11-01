// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func traceparentSeeds() []string {
	tid, pid := testTraceIDHex, testParentIDHex
	return []string{
		tpSampled, tpUnsampled,
		"00-" + tid + "-" + pid + "-09",
		tpSampled + "-",
		"cc-" + tid + "-" + pid + "-01-",
		"cc-" + tid + "-" + pid + "-01-future",
		"cc-" + tid + "-" + pid + "-01.future",
		"ff-" + tid + "-" + pid + "-01",
		"00-" + strings.Repeat("0", 32) + "-" + pid + "-01",
		"00-" + tid + "-" + strings.Repeat("0", 16) + "-01",
		" \t" + tpSampled + " ",
		"00-" + strings.ToUpper(tid) + "-" + pid + "-01",
		"", "-", "00", "00-", strings.Repeat("-", 55),
	}
}

// FuzzTraceparent checks checkTraceparent on any value.
func FuzzTraceparent(f *testing.F) {
	for _, s := range traceparentSeeds() {
		f.Add(s)
	}
	f.Fuzz(checkTraceparent)
}

// checkTraceparent checks ParseTraceparent on the field value in. It
// never panics. An accepted value re-formats to its own lowercase bytes,
// and the value AppendTraceparent, the format Inject writes, makes of its
// IDs and sampled flag reads back to them here and in the SDK, so what a
// Node injects falls in neither class below. Acceptance, IDs and the
// sampled flag agree with the SDK's propagation.TraceContext extraction
// of the same field, except in the two classes where the SDK reads W3C
// Trace Context otherwise and ParseTraceparent follows the specification
// (docs/architecture/10-observability.md "Propagation"): version 00 with
// flag bits above 03 is valid here only, and version 00 followed by one
// "-" is valid in the SDK only, which reads the 55 bytes before the "-".
func checkTraceparent(t *testing.T, in string) {
	tid, pid, flags, ok := ParseTraceparent(in)
	v := trimOWS(in)
	if ok {
		formatted := v[:2] + "-" + string(AppendTraceID(nil, tid)) + "-" + string(AppendSpanID(nil, pid)) + "-" + string(appendHex(nil, []byte{flags}))
		if formatted != v[:TraceparentLen] {
			t.Fatalf("accepted %q re-formats to %q", in, formatted)
		}
		sampled := flags&FlagSampled != 0
		out := string(AppendTraceparent(nil, tid, pid, sampled))
		otid, opid, oflags, ook := ParseTraceparent(out)
		osc := sdkTraceparent(out)
		if !ook || otid != tid || opid != pid || oflags != flags&FlagSampled ||
			!osc.IsValid() || osc.TraceID() != tid || osc.SpanID() != pid || osc.IsSampled() != sampled {
			t.Fatalf("%q: the injected %q does not read back to its IDs and sampled flag", in, out)
		}
	}
	sc := sdkTraceparent(v)
	sdkOK := sc.IsValid()
	switch {
	case ok && !sdkOK && v[:2] == "00" && flags > 3:
		return // flag bits above 03, which the SDK rejects
	case !ok && sdkOK && len(v) == TraceparentLen+1 && v[:2] == "00" && v[TraceparentLen] == '-':
		// One "-" after the flags, which the SDK skips: it must read
		// exactly the value before the "-".
		if tid, pid, flags, ok = ParseTraceparent(v[:TraceparentLen]); !ok {
			t.Fatalf("%q: the SDK accepts it, but the %d bytes before the \"-\" are invalid", in, TraceparentLen)
		}
	case ok != sdkOK:
		t.Fatalf("%q: ParseTraceparent ok = %v, SDK valid = %v", in, ok, sdkOK)
	}
	if ok && (sc.TraceID() != tid || sc.SpanID() != pid || sc.IsSampled() != (flags&FlagSampled != 0)) {
		t.Fatalf("%q: parsed %x %x %02x, SDK %s", in, tid, pid, flags, sc.TraceID())
	}
}

// sdkTraceparent returns the span context that the SDK's
// propagation.TraceContext extracts from the traceparent field value v,
// invalid when the SDK rejects v.
func sdkTraceparent(v string) trace.SpanContext {
	h := http.Header{}
	h.Set(HeaderTraceparent, v)
	return trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h)))
}

// TestTraceparentSDKDifferences pins the two classes where the SDK's
// propagation.TraceContext reads W3C Trace Context otherwise than
// ParseTraceparent, beside values next to them on which the two agree,
// and checks each value with checkTraceparent: version 00 with flag bits
// above 03 is valid here only, and version 00 followed by one "-" in the
// SDK only. A changed SDK result means an SDK release reads the class
// otherwise, so checkTraceparent and docs/architecture/10-observability.md
// "Propagation" change with it.
func TestTraceparentSDKDifferences(t *testing.T) {
	tid, pid := testTraceIDHex, testParentIDHex
	for _, tc := range []struct {
		name    string
		in      string
		ok, sdk bool // valid for ParseTraceparent, valid for the SDK
	}{
		{"version 00", tpSampled, true, true},
		{"version 00 random flag", "00-" + tid + "-" + pid + "-03", true, true},
		{"version 00 flags above 03", "00-" + tid + "-" + pid + "-09", true, false},
		{"version 00 all flags", "00-" + tid + "-" + pid + "-ff", true, false},
		{"version 00 trailing dash", tpSampled + "-", false, true},
		{"version 00 unsampled trailing dash", tpUnsampled + "-", false, true},
		{"version 00 trailing dash and OWS", " " + tpSampled + "-\t", false, true},
		{"version 00 smallest IDs trailing dash", "00-" + strings.Repeat("0", 31) + "1-" + strings.Repeat("0", 15) + "1-00-", false, true},
		{"version 00 two trailing dashes", tpSampled + "--", false, false},
		{"version 00 trailing field", tpSampled + "-x", false, false},
		{"version 00 flags above 03 trailing dash", "00-" + tid + "-" + pid + "-09-", false, false},
		{"version 00 zero trace ID trailing dash", "00-" + strings.Repeat("0", 32) + "-" + pid + "-01-", false, false},
		{"version 00 uppercase flags trailing dash", "00-" + tid + "-" + pid + "-0A-", false, false},
		{"version cc trailing dash", "cc-" + tid + "-" + pid + "-01-", true, true},
		{"version cc flags above 03", "cc-" + tid + "-" + pid + "-09", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, ok := ParseTraceparent(tc.in); ok != tc.ok {
				t.Errorf("ParseTraceparent(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if got := sdkTraceparent(trimOWS(tc.in)).IsValid(); got != tc.sdk {
				t.Errorf("SDK extraction of %q valid = %v, want %v", tc.in, got, tc.sdk)
			}
			checkTraceparent(t, tc.in)
		})
	}
}

// FuzzTracestate checks that a tracestate is passed through byte-exact
// (after trimming outer OWS) or dropped, never modified; field lines
// validated in place keep exactly what validating their combined value
// would keep.
func FuzzTracestate(f *testing.F) {
	for _, s := range []string{
		"rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", "foo=1 \t , \t bar=2", "foo=1,foo=2",
		"t@s=1", "FOO=1", "foo=", "=1", ",,,", strings.Repeat("a=1,", 40), "",
	} {
		f.Add(s, "")
		f.Add(s, "extra=1")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		for _, lines := range [][]string{{a}, {a, b}} {
			h := http.Header{HeaderTracestate: lines}
			got := tracestate(h)
			joined := trimOWS(strings.Join(lines, ","))
			total := len(lines) - 1
			for _, l := range lines {
				total += len(l)
			}
			keep := ValidTracestate(joined) && (len(lines) == 1 || total <= MaxTracestateBytes)
			if (got != "") != keep || (keep && got != joined) {
				t.Fatalf("tracestate(%q) = %q, want %q kept %v", lines, got, joined, keep)
			}
			if got != "" && (!ValidTracestate(got) || len(got) > MaxTracestateBytes) {
				t.Fatalf("kept an invalid tracestate %q", got)
			}
		}
	})
}
