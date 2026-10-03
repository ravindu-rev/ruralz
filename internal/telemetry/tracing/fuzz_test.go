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

// FuzzTraceparent is spec 09 test 27: ParseTraceparent never panics; an
// accepted value re-formats to its own lowercase bytes; acceptance, IDs
// and the sampled flag agree with the SDK's propagation.TraceContext
// extraction of the same header, except that version 00 flags with
// reserved bits set (above 03) are accepted here (W3C Trace Context
// ignores unknown flags; the SDK rejects them).
func FuzzTraceparent(f *testing.F) {
	for _, s := range traceparentSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		tid, pid, flags, ok := ParseTraceparent(in)
		v := trimOWS(in)
		if ok {
			formatted := v[:2] + "-" + string(AppendTraceID(nil, tid)) + "-" + string(AppendSpanID(nil, pid)) + "-" + string(appendHex(nil, []byte{flags}))
			if formatted != v[:TraceparentLen] {
				t.Fatalf("accepted %q re-formats to %q", in, formatted)
			}
		}
		h := http.Header{}
		h.Set(HeaderTraceparent, v)
		sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h)))
		sdkOK := sc.IsValid()
		if ok && !sdkOK && v[:2] == "00" && flags > 3 {
			return // the documented divergence
		}
		if ok != sdkOK {
			t.Fatalf("%q: ParseTraceparent ok = %v, SDK valid = %v", in, ok, sdkOK)
		}
		if ok && (sc.TraceID() != tid || sc.SpanID() != pid || sc.IsSampled() != (flags&FlagSampled != 0)) {
			t.Fatalf("%q: parsed %x %x %02x, SDK %s", in, tid, pid, flags, sc.TraceID())
		}
	})
}

// FuzzTracestate is spec 09 test 28: a tracestate is passed through
// byte-exact (after trimming outer OWS) or dropped, never modified; field
// lines validated in place keep exactly what validating their combined
// value would keep.
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
