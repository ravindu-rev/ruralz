// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The Host Function fuzz targets, one per group
// (docs/engineering/03-testing-and-quality-strategy.md "Fuzzing"): a guest
// passes the fuzzed arguments to one of the group's Host Functions as
// given; every out-of-bounds pointer or length traps the guest
// (RZ-PLG-001) and never the host, and any other call returns a result the
// ABI defines, so the call continues, responds or is a misuse.

// fuzzParams are the parameter kinds of each fuzzed Host Function: i for
// i32, I for i64.
func fuzzParams() map[string]string {
	return map[string]string{
		"request_header_get": "iiii", "request_header_set": "iiii", "request_headers_list": "ii", "request_header_remove": "ii",
		"response_header_set": "iiii", "response_send": "iii", "context_get": "iiii", "context_set": "iiii",
		"log": "iii", "metric_add": "iiI", "clock_now": "i", "random_bytes": "ii", "request_info": "iii",
		"crypto_digest": "iiiii", "crypto_hmac": "iiiiiii", "crypto_key_sign": "iiiii", "crypto_key_verify": "iiiii",
		"consumer_info": "iii", "state_get": "iiii", "state_incr": "iiIIi",
	}
}

// argGuest returns a guest whose rz_on_request_headers calls fn with its
// i32 arguments read from memory at 0, 4, … and its i64 ones from 64, 72,
// …, and returns CONTINUE whatever fn returned.
func argGuest(fn string) []byte {
	g := ht.NewGuest(fn)
	var args [][]byte
	ni, nl := 0, 0
	for _, c := range fuzzParams()[fn] {
		if c == 'I' {
			args = append(args, ht.Code(ht.I32Const(int32(64+8*nl)), ht.I64Load(0)))
			nl++
			continue
		}
		args = append(args, ht.Code(ht.I32Const(int32(4*ni)), ht.I32Load(0)))
		ni++
	}
	g.Phase("rz_on_request_headers", nil, g.Call(fn, args...), ht.Op(ht.OpDrop), ht.I32Const(0))
	return g.Encode()
}

// fuzzGroup is one group's target over fns, granted caps.
func fuzzGroup(f *testing.F, fns []string, caps ...v1alpha1.Capability) {
	f.Add(uint8(0), []byte{}, []byte{})
	f.Add(uint8(1), []byte{0xff, 0xff, 0xff, 0xff, 4, 0, 0, 0, 0, 0, 1, 0, 64, 0, 0, 0}, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	f.Add(uint8(2), []byte{0, 1, 0, 0, 16, 0, 0, 0, 0, 2, 0, 0, 0x10, 0, 0, 0}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Add(uint8(3), []byte{0, 0, 2, 0, 0, 0, 0, 0x80, 0, 0, 0, 0, 0xff, 0xff, 0, 0}, []byte{})
	f.Fuzz(func(t *testing.T, pick uint8, i32s, i64s []byte) {
		h := newHarness(t)
		fn := fns[int(pick)%len(fns)]
		p := headersPlugin(argGuest(fn), caps...)
		ff := h.one(p, buildOpts{store: stateStore(nil, nil)})
		waitIdle(t, ff.pool)
		var args [128]byte
		copy(args[:64], i32s)
		copy(args[64:], i64s)
		for range len(ff.pool.idle) {
			inst := <-ff.pool.idle
			inst.mod.Memory().Write(0, args[:])
			ff.pool.idle <- inst
		}
		x := exchange(http.Header{"A": {"b"}})
		x.Msg = &filtertest.Message{Hdr: x.Hdr}
		r := handle(ff, phase.OnRequestHeaders, x)
		switch {
		case r.Outcome == filter.Continue, r.Outcome == filter.Respond:
		case r.Outcome == filter.CannotDecide && (r.Code == CodeTrap || r.Code == CodeMisuse || r.Code == CodeInvalidResult):
		default:
			t.Fatalf("%s(%x, %x) = %+v (%v), want a result, a trap or a misuse", fn, i32s, i64s, r, r.Err)
		}
	})
}

func FuzzHostHeaders(f *testing.F) {
	fuzzGroup(f, []string{"request_header_get", "request_header_set", "request_headers_list", "request_header_remove", "response_header_set"},
		v1alpha1.CapabilityRequestHeadersRead, v1alpha1.CapabilityRequestHeadersWrite, v1alpha1.CapabilityResponseHeadersWrite)
}

func FuzzHostResponse(f *testing.F) {
	fuzzGroup(f, []string{"response_send"}, v1alpha1.CapabilityResponseSend)
}

func FuzzHostContext(f *testing.F) {
	fuzzGroup(f, []string{"context_get", "context_set"}, v1alpha1.CapabilityContextUse)
}

func FuzzHostTelemetry(f *testing.F) {
	fuzzGroup(f, []string{"log", "metric_add"}, v1alpha1.CapabilityLogWrite, v1alpha1.CapabilityMetricsWrite)
}

func FuzzHostEnvironment(f *testing.F) {
	fuzzGroup(f, []string{"clock_now", "random_bytes", "request_info", "consumer_info"},
		v1alpha1.CapabilityClockRead, v1alpha1.CapabilityRandomRead, v1alpha1.CapabilityRequestMetadataRead, v1alpha1.CapabilityConsumerRead)
}

func FuzzHostCrypto(f *testing.F) {
	fuzzGroup(f, []string{"crypto_digest", "crypto_hmac", "crypto_key_sign", "crypto_key_verify"}, v1alpha1.CapabilityCryptoUse)
}

func FuzzHostState(f *testing.F) {
	fuzzGroup(f, []string{"state_get", "state_incr"}, v1alpha1.CapabilityStateRead, v1alpha1.CapabilityStateWrite)
}

// FuzzStructure: the structure check and the Plugin.spec checks never
// panic on any binary.
func FuzzStructure(f *testing.F) {
	f.Add(continueGuest("log"))
	f.Add([]byte("\x00asm\x01\x00\x00\x00"))
	f.Add([]byte("\x00asm\x01\x00\x00\x00\x0a\x05\x01\x03\x01\x7f\x0b"))
	f.Fuzz(func(_ *testing.T, bin []byte) {
		_, _ = readModule(bin)
		_, _ = checkArtifact(bin, headersSpec(v1alpha1.CapabilityLogWrite), 256)
	})
}
