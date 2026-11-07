// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// handle runs one Phase call.
func handle(f *Filter, ph phase.Phase, x filter.Exchange) filter.Result {
	return f.Handle(context.Background(), ph, x)
}

// TestHeaderCopy: a guest reads a request header into its buffer and sets
// another header to the bytes it read; CONTINUE passes the request on.
func TestHeaderCopy(t *testing.T) {
	h := newHarness(t)
	bin := simple([]string{"request_header_get", "request_header_set"}, []ht.Locals{{N: 1, Type: ht.I32}}, func(g *ht.Guest) [][]byte {
		return [][]byte{
			g.Call("request_header_get", g.Str("x-in"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(int32(ht.ScratchLen))), ht.LocalSet(0),
			ht.LocalGet(0), ht.I32Const(0), ht.Op(ht.OpI32LtS), ht.If(ht.BlockEmpty), ht.LocalGet(0), ht.Op(ht.OpReturn), ht.End(),
			ht.Check(g.Call("request_header_set", g.Str("x-out"), ht.I32Const(int32(ht.Scratch)), ht.LocalGet(0)), 0),
			ht.I32Const(0),
		}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityRequestHeadersRead, v1alpha1.CapabilityRequestHeadersWrite), buildOpts{})
	x := exchange(http.Header{"X-In": {"hello"}})
	r := handle(f, phase.OnRequestHeaders, x)
	if r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if got := x.Hdr.Get("X-Out"); got != "hello" {
		t.Errorf("X-Out = %q, want %q", got, "hello")
	}
}

// TestOutputBufferTooSmall: an output write above the capacity writes
// nothing and returns the full length.
func TestOutputBufferTooSmall(t *testing.T) {
	h := newHarness(t)
	bin := simple([]string{"request_header_get"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{
			ht.I32Const(int32(ht.Scratch)), ht.I32Const(0x77), ht.I32Store8(0),
			ht.Check(g.Call("request_header_get", g.Str("x-in"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(3)), 11),
			ht.CheckMem(ht.Scratch, []byte{0x77}),
			ht.I32Const(0),
		}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityRequestHeadersRead), buildOpts{})
	if r := handle(f, phase.OnRequestHeaders, exchange(http.Header{"X-In": {"hello world"}})); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
}

// TestRespond: RESPOND after an accepted response_send short-circuits
// with the guest's status and body.
func TestRespond(t *testing.T) {
	h := newHarness(t)
	bin := simple([]string{"response_send"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{ht.Check(g.Call("response_send", ht.I32Const(403), g.Str("denied")), 0), ht.I32Const(1)}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityResponseSend), buildOpts{})
	r := handle(f, phase.OnRequestHeaders, exchange(nil))
	if r.Outcome != filter.Respond || r.Response == nil || r.Response.Status != 403 || string(r.Response.Body) != "denied" {
		t.Fatalf("Handle = %+v, want Respond 403 denied", r)
	}
}

// TestResults covers the Phase results and failure codes of a call
// (docs/architecture/05-wasm-plugin-system.md "Failure semantics and the
// RZ-PLG registry").
func TestResults(t *testing.T) {
	cases := []struct {
		name    string
		imports []string
		caps    []v1alpha1.Capability
		body    func(g *ht.Guest) [][]byte
		code    string
		outcome filter.Outcome
	}{
		{name: "CONTINUE", body: func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(0)} }, outcome: filter.Continue},
		{name: "RESPOND without response_send", body: func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(1)} }, code: CodeInvalidResult},
		{name: "END_STREAM outside onChunk", body: func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(2)} }, code: CodeInvalidResult},
		{name: "an unknown result", body: func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(7)} }, code: CodeInvalidResult},
		{name: "cannot decide", body: func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(-1)} }, code: CodeUndecided},
		{name: "a trap", body: func(*ht.Guest) [][]byte { return [][]byte{ht.Op(ht.OpUnreachable)} }, code: CodeTrap},
		{name: "proc_exit traps", imports: []string{"wasi.proc_exit"}, body: func(g *ht.Guest) [][]byte {
			return [][]byte{g.Call("wasi.proc_exit", ht.I32Const(0)), ht.I32Const(0)}
		}, code: CodeTrap},
		{
			name: "an out-of-bounds guest pointer traps the guest", imports: []string{"request_header_get"}, caps: []v1alpha1.Capability{v1alpha1.CapabilityRequestHeadersRead},
			body: func(g *ht.Guest) [][]byte {
				return [][]byte{g.Call("request_header_get", ht.I32Const(-16), ht.I32Const(4), ht.I32Const(0), ht.I32Const(0)), ht.Op(ht.OpDrop), ht.I32Const(0)}
			}, code: CodeTrap,
		},
		{
			name: "an output buffer past memory traps the guest", imports: []string{"request_header_get"}, caps: []v1alpha1.Capability{v1alpha1.CapabilityRequestHeadersRead},
			body: func(g *ht.Guest) [][]byte {
				return [][]byte{g.Call("request_header_get", g.Str("x-in"), ht.I32Const(131070), ht.I32Const(64)), ht.Op(ht.OpDrop), ht.I32Const(0)}
			}, code: CodeTrap,
		},
		{
			name: "a wrong Phase is sticky whatever the guest returns", imports: []string{"request_body_read"}, caps: []v1alpha1.Capability{v1alpha1.CapabilityRequestBodyRead},
			body: func(g *ht.Guest) [][]byte {
				return [][]byte{ht.Check(g.Call("request_body_read", ht.I64Const(0), ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), errPhase), ht.I32Const(0)}
			}, code: CodeMisuse,
		},
		{
			name: "the call budget is sticky", imports: []string{"clock_now"}, caps: []v1alpha1.Capability{v1alpha1.CapabilityClockRead},
			body: func(g *ht.Guest) [][]byte {
				return [][]byte{
					ht.Loop(ht.BlockEmpty),
					g.Call("clock_now", ht.I32Const(int32(ht.Scratch))), ht.I32Const(errBudget), ht.Op(ht.OpI32Ne), ht.BrIf(0),
					ht.End(),
					ht.I32Const(0),
				}
			}, code: CodeMisuse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bin := simple(tc.imports, nil, tc.body)
			f := h.one(headersPlugin(bin, tc.caps...), buildOpts{})
			r := handle(f, phase.OnRequestHeaders, exchange(http.Header{"X-In": {"v"}}))
			if tc.code == "" {
				if r.Outcome != tc.outcome {
					t.Fatalf("Handle = %+v, want outcome %d", r, tc.outcome)
				}
				return
			}
			if r.Outcome != filter.CannotDecide || codeOf(r) != tc.code {
				t.Fatalf("Handle = %+v (%v), want CannotDecide %s", r, r.Err, tc.code)
			}
			if got := h.rec("pol").failureCount(tc.code); got != 1 {
				t.Errorf("ruralz_plugin_failures_total{code=%s} = %d, want 1", tc.code, got)
			}
		})
	}
}

// TestTrapDiscardsTheInstance: a trapped instance is destroyed and the
// pool manager refills the pool, so the next call runs on a fresh one.
func TestTrapDiscardsTheInstance(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest()
	counter := g.M.Global(ht.I32, true, 0)
	g.Phase("rz_on_request_headers", nil,
		ht.GlobalGet(counter), ht.I32Const(1), ht.Op(ht.OpI32Add), ht.GlobalSet(counter),
		ht.GlobalGet(counter), ht.I32Const(1), ht.Op(ht.OpI32Ne), ht.If(ht.BlockEmpty), ht.Op(ht.OpUnreachable), ht.End(),
		ht.I32Const(0))
	f := h.one(headersPlugin(g.Encode()), buildOpts{})
	for i := range 3 {
		waitIdle(t, f.pool)
		r := handle(f, phase.OnRequestHeaders, exchange(nil))
		// Each instance's first call continues; a reused instance would
		// trap on its second.
		if r.Outcome != filter.Continue {
			t.Fatalf("call %d = %+v, want Continue on a fresh or first-used instance", i, r)
		}
		drainIdle(f.pool)
	}
}

// waitIdle waits until the pool holds an idle instance, which the pool
// manager's goroutine fills: it polls with a real-time bound, only for a
// goroutine the test started, never for fake time.
func waitIdle(t *testing.T, p *pool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for len(p.idle) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for an idle instance")
		case <-time.After(time.Millisecond):
		}
	}
}

// drainIdle destroys the idle instances, so the next call takes a new
// one.
func drainIdle(p *pool) {
	for {
		select {
		case inst := <-p.idle:
			p.destroy(context.Background(), inst)
		default:
			return
		}
	}
}

// TestDeadline: a call past limits.timeout ends with RZ-PLG-002 when the
// host timer fires.
func TestDeadline(t *testing.T) {
	h := newHarness(t)
	bin := simple(nil, nil, func(*ht.Guest) [][]byte {
		return [][]byte{ht.Loop(ht.BlockEmpty), ht.Br(0), ht.End(), ht.I32Const(0)}
	})
	f := h.one(headersPlugin(bin), buildOpts{})
	done := make(chan filter.Result, 1)
	go func() { done <- handle(f, phase.OnRequestHeaders, exchange(nil)) }()
	if err := h.clk.BlockUntilDue(t.Context(), h.clk.Now().Add(DefaultTimeout)); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(DefaultTimeout)
	r := <-done
	if r.Outcome != filter.CannotDecide || codeOf(r) != CodeDeadline {
		t.Fatalf("Handle = %+v, want CannotDecide %s", r, CodeDeadline)
	}
}

// TestMemoryLimit: a guest that grows its memory to the limit and traps
// when memory.grow fails gets RZ-PLG-003.
func TestMemoryLimit(t *testing.T) {
	h := newHarness(t)
	bin := simple(nil, nil, func(*ht.Guest) [][]byte {
		return [][]byte{
			ht.Loop(ht.BlockEmpty),
			ht.I32Const(1), ht.MemoryGrow(), ht.I32Const(-1), ht.Op(ht.OpI32Ne), ht.BrIf(0),
			ht.End(),
			ht.Op(ht.OpUnreachable),
		}
	})
	p := headersPlugin(bin)
	p.memBytes = 4 * pageBytes
	f := h.one(p, buildOpts{})
	r := handle(f, phase.OnRequestHeaders, exchange(nil))
	if r.Outcome != filter.CannotDecide || codeOf(r) != CodeMemory {
		t.Fatalf("Handle = %+v, want CannotDecide %s", r, CodeMemory)
	}
}

// TestCredentialHeaders: credential headers read as absent and leave the
// list without credentials.read, and read normally with it.
func TestCredentialHeaders(t *testing.T) {
	for _, grant := range []bool{false, true} {
		t.Run(map[bool]string{false: "without credentials.read", true: "with credentials.read"}[grant], func(t *testing.T) {
			h := newHarness(t)
			want := errAbsent
			if grant {
				want = int32(len("Bearer secret-token"))
			}
			bin := simple([]string{"request_header_get", "request_headers_list"}, nil, func(g *ht.Guest) [][]byte {
				return [][]byte{
					ht.Check(g.Call("request_header_get", g.Str("Authorization"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), want),
					ht.I32Const(0),
				}
			})
			caps := []v1alpha1.Capability{v1alpha1.CapabilityRequestHeadersRead}
			if grant {
				caps = append(caps, v1alpha1.CapabilityCredentialsRead)
			}
			f := h.one(headersPlugin(bin, caps...), buildOpts{})
			x := exchange(http.Header{"Authorization": {"Bearer secret-token"}, "Accept": {"*/*"}})
			if r := handle(f, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
		})
	}
}

// TestHeaderListEncoding pins request_headers_list: little-endian lengths,
// lowercase names in byte order, each line once, credentials left out.
func TestHeaderListEncoding(t *testing.T) {
	h := newHarness(t)
	want := []byte("\x06\x00\x00\x00accept\x03\x00\x00\x00*/*\x01\x00\x00\x00b\x01\x00\x00\x001\x01\x00\x00\x00b\x01\x00\x00\x002")
	bin := simple([]string{"request_headers_list"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{
			ht.Check(g.Call("request_headers_list", ht.I32Const(int32(ht.Scratch)), ht.I32Const(int32(ht.ScratchLen))), ht.Size(len(want))),
			ht.CheckMem(ht.Scratch, want),
			ht.I32Const(0),
		}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityRequestHeadersRead), buildOpts{})
	x := exchange(http.Header{"B": {"1", "2"}, "Accept": {"*/*"}, "Cookie": {"session=1"}})
	if r := handle(f, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
}

// TestHeaderWriteRules: names the Node manages, invalid tokens and values
// with control bytes are ERR_INVALID, advisory: the call continues.
func TestHeaderWriteRules(t *testing.T) {
	cases := []struct{ name, value string }{
		{"host", "evil"},
		{"content-length", "1"},
		{"transfer-encoding", "chunked"},
		{"connection", "close"},
		{":path", "/x"},
		{"traceparent", "00-x"},
		{"bad name", "v"},
		{"x-ok", "a\r\nb"},
		{"x-ok", "a\x00b"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+strings.ReplaceAll(tc.value, "\r\n", "CRLF"), func(t *testing.T) {
			h := newHarness(t)
			bin := simple([]string{"request_header_set"}, nil, func(g *ht.Guest) [][]byte {
				return [][]byte{ht.Check(g.Call("request_header_set", g.Str(tc.name), g.Str(tc.value)), errInvalid), ht.I32Const(0)}
			})
			f := h.one(headersPlugin(bin, v1alpha1.CapabilityRequestHeadersWrite), buildOpts{})
			x := exchange(nil)
			if r := handle(f, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue after the advisory ERR_INVALID", r)
			}
		})
	}
}

// TestRequestInfo pins request_info's fields.
func TestRequestInfo(t *testing.T) {
	fields := []struct {
		field int32
		want  string
	}{
		{requestMethod, "GET"},
		{requestScheme, "https"},
		{requestHost, "api.example"},
		{requestPath, "/orders"},
		{requestRoute, "orders"},
	}
	for _, fc := range fields {
		h := newHarness(t)
		bin := simple([]string{"request_info"}, nil, func(g *ht.Guest) [][]byte {
			return [][]byte{
				ht.Check(g.Call("request_info", ht.I32Const(fc.field), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), ht.Size(len(fc.want))),
				ht.CheckMem(ht.Scratch, []byte(fc.want)),
				ht.I32Const(0),
			}
		})
		f := h.one(headersPlugin(bin, v1alpha1.CapabilityRequestMetadataRead), buildOpts{})
		if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
			t.Errorf("request_info(%d) Handle = %+v, want Continue with %q", fc.field, r, fc.want)
		}
	}
}

// TestConsumerInfo: ERR_ABSENT without a Consumer; the Consumer's fields
// with one; ERR_PHASE in onRequestHeaders for an auth-class Policy.
func TestConsumerInfo(t *testing.T) {
	consumer := &filter.Identity{Method: filter.MethodAPIKey, Consumer: &expr.Consumer{Name: "partner", Tier: "gold", Tags: []string{"a", "b"}}}
	cases := []struct {
		name  string
		ident *filter.Identity
		field int32
		want  int32
		mem   string
		class v1alpha1.FilterClass
		code  string
	}{
		{name: "no Consumer", field: consumerName, want: errAbsent},
		{name: "name", ident: consumer, field: consumerName, want: 7, mem: "partner"},
		{name: "Tier", ident: consumer, field: consumerTier, want: 4, mem: "gold"},
		{name: "tags", ident: consumer, field: consumerTags, want: 9, mem: `["a","b"]`},
		{name: "method", ident: consumer, field: consumerMethod, want: 7, mem: "api-key"},
		{name: "claims of a non-JWT method", ident: consumer, field: consumerClaims, want: 2, mem: "{}"},
		{name: "an auth-class Policy in onRequestHeaders", ident: consumer, field: consumerName, want: errPhase, class: v1alpha1.FilterClassAuth, code: CodeMisuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bin := simple([]string{"consumer_info"}, nil, func(g *ht.Guest) [][]byte {
				return [][]byte{
					ht.Check(g.Call("consumer_info", ht.I32Const(tc.field), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), tc.want),
					ht.CheckMem(ht.Scratch, []byte(tc.mem)),
					ht.I32Const(0),
				}
			})
			p := headersPlugin(bin, v1alpha1.CapabilityConsumerRead)
			p.class = tc.class
			f := h.one(p, buildOpts{})
			x := exchange(nil)
			x.Ident = tc.ident
			r := handle(f, phase.OnRequestHeaders, x)
			if tc.code != "" {
				if codeOf(r) != tc.code {
					t.Fatalf("Handle = %+v, want %s", r, tc.code)
				}
				return
			}
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
		})
	}
}

// TestBodies: onRequestBody reads the body in fixed-size pieces and
// replaces it.
func TestBodies(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("request_body_read", "request_body_replace")
	g.Phase("rz_on_request_body", nil,
		ht.Check(g.Call("request_body_read", ht.I64Const(0), ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), 4),
		ht.CheckMem(ht.Scratch, []byte("abcd")),
		ht.Check(g.Call("request_body_read", ht.I64Const(4), ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), 2),
		ht.CheckMem(ht.Scratch, []byte("ef")),
		ht.Check(g.Call("request_body_read", ht.I64Const(6), ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), 0),
		ht.Check(g.Call("request_body_replace", g.Str("replaced")), 0),
		ht.I32Const(0))
	p := pluginOpts{
		bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestBody},
		caps: []v1alpha1.Capability{v1alpha1.CapabilityRequestBodyRead, v1alpha1.CapabilityRequestBodyWrite},
	}
	f := h.one(p, buildOpts{limits: filter.Limits{MaxRequestBodyBytes: 1 << 20}})
	x := exchange(nil)
	msg := &filtertest.Message{Hdr: x.Hdr, Raw: []byte("abcdef")}
	x.Msg = msg
	if r := handle(f, phase.OnRequestBody, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if string(msg.Raw) != "replaced" {
		t.Errorf("body = %q, want %q", msg.Raw, "replaced")
	}
}

// TestContext: context_set and context_get carry values between Phases of
// one request, within 4 KiB per Policy, and context_set is not valid in
// onLog.
func TestContext(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("context_get", "context_set")
	g.Phase("rz_on_request_headers", nil, ht.Check(g.Call("context_set", g.Str("k"), g.Str("value")), 0), ht.I32Const(0))
	g.Phase("rz_on_response", nil,
		ht.Check(g.Call("context_get", g.Str("k"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(16)), 5),
		ht.CheckMem(ht.Scratch, []byte("value")),
		ht.Check(g.Call("context_set", g.Str("big"), ht.I32Const(0), ht.I32Const(contextBytes)), errLimit),
		ht.I32Const(0))
	g.Phase("rz_on_log", nil, ht.Check(g.Call("context_set", g.Str("k"), g.Str("v")), errPhase), ht.I32Const(0))
	p := pluginOpts{
		bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnResponse, v1alpha1.PhaseOnLog},
		caps: []v1alpha1.Capability{v1alpha1.CapabilityContextUse},
	}
	f := h.one(p, buildOpts{})
	x := exchange(nil)
	if r := handle(f, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
		t.Fatalf("onRequestHeaders = %+v, want Continue", r)
	}
	if r := handle(f, phase.OnResponse, x); r.Outcome != filter.Continue {
		t.Fatalf("onResponse = %+v, want Continue", r)
	}
	if r := handle(f, phase.OnLog, x); codeOf(r) != CodeMisuse {
		t.Fatalf("onLog = %+v, want %s", r, CodeMisuse)
	}
	f.Finish(context.Background(), x)
	if x.State != nil {
		t.Errorf("PolicyState after Finish = %v, want freed", x.State)
	}
}

// TestMetricAdd: names bind first come up to 16; an invalid name is
// ERR_INVALID, a negative value or a 17th name ERR_LIMIT.
func TestMetricAdd(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("metric_add")
	var body [][]byte
	for i := range metricNames {
		body = append(body, ht.Check(g.Call("metric_add", g.Str("e"+string(rune('a'+i))), ht.I64Const(2)), 0))
	}
	body = append(body,
		ht.Check(g.Call("metric_add", g.Str("ea"), ht.I64Const(3)), 0),
		ht.Check(g.Call("metric_add", g.Str("overflow"), ht.I64Const(1)), errLimit),
		ht.Check(g.Call("metric_add", g.Str("Bad"), ht.I64Const(1)), errInvalid),
		ht.Check(g.Call("metric_add", g.Str("ea"), ht.I64Const(-1)), errLimit),
		ht.I32Const(0))
	g.Phase("rz_on_request_headers", nil, body...)
	f := h.one(headersPlugin(g.Encode(), v1alpha1.CapabilityMetricsWrite), buildOpts{})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if got := h.rec("pol").events["ea"]; got != 5 {
		t.Errorf("events[ea] = %d, want 5", got)
	}
}

// TestGuestLogs: a message is cut at 4 KiB, a call drops what passes 16
// KiB, and a credential value is redacted.
func TestGuestLogs(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("log")
	long := g.Bytes([]byte(strings.Repeat("x", 6000)))
	secret := "Bearer abcdefghijkl"
	body := [][]byte{
		ht.Check(g.Call("log", ht.I32Const(levelInfo), g.Str("token is "+secret)), 0),
		ht.Check(g.Call("log", ht.I32Const(levelInfo), ht.Addr(long), ht.I32Const(6000)), 0),
		ht.Check(g.Call("log", ht.I32Const(9), g.Str("bad level")), errInvalid),
	}
	for range 4 {
		body = append(body, ht.Check(g.Call("log", ht.I32Const(levelInfo), ht.Addr(long), ht.I32Const(4096)), 0))
	}
	body = append(body, ht.I32Const(0))
	g.Phase("rz_on_request_headers", nil, body...)
	f := h.one(headersPlugin(g.Encode(), v1alpha1.CapabilityLogWrite), buildOpts{})
	x := exchange(http.Header{"Authorization": {secret}})
	if r := handle(f, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	logs := h.logs.String()
	if strings.Contains(logs, secret) {
		t.Errorf("a guest log carries the credential %q", secret)
	}
	rec := h.rec("pol")
	if rec.truncated < 1 {
		t.Errorf("truncated = %d, want at least 1", rec.truncated)
	}
	if rec.dropped < 1 {
		t.Errorf("dropped = %d, want at least 1 past the 16 KiB call cap", rec.dropped)
	}
}

// TestClockAndRandom: clock_now reads the host clock and random_bytes the
// host random source.
func TestClockAndRandom(t *testing.T) {
	h := newHarness(t)
	le, err := binary.Append(nil, binary.LittleEndian, epoch().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	bin := simple([]string{"clock_now", "random_bytes"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{
			ht.Check(g.Call("clock_now", ht.I32Const(int32(ht.Scratch))), 0),
			ht.CheckMem(ht.Scratch, le),
			ht.Check(g.Call("random_bytes", ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), 4),
			ht.CheckMem(ht.Scratch, []byte{0, 1, 2, 3}),
			ht.I32Const(0),
		}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityClockRead, v1alpha1.CapabilityRandomRead), buildOpts{})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
}
