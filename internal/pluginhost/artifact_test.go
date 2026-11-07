// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// headersSpec is a Plugin.spec in onRequestHeaders with caps.
func headersSpec(caps ...v1alpha1.Capability) *v1alpha1.PluginSpec {
	return &v1alpha1.PluginSpec{ABI: v1alpha1.PluginABIV1, Phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}, Capabilities: caps}
}

// continueGuest returns a guest importing imports whose
// rz_on_request_headers returns CONTINUE.
func continueGuest(imports ...string) []byte {
	return simple(imports, nil, func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(0)} })
}

// TestCheckArtifact covers the checks before any compile: structure
// limits, the ABI's memory, required and Phase exports, implemented
// imports and the Capability subset.
func TestCheckArtifact(t *testing.T) {
	noExport := func(drop string) []byte {
		g := ht.NewGuest()
		g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
		bin := g.Encode()
		// Rename the export so the required one is missing.
		return []byte(strings.Replace(string(bin), drop, "xx"+drop[2:], 1))
	}
	cases := []struct {
		name string
		bin  []byte
		spec *v1alpha1.PluginSpec
		code string
		msg  string
	}{
		{name: "a guest that matches its spec", bin: continueGuest("log"), spec: headersSpec(v1alpha1.CapabilityLogWrite)},
		{name: "a malformed binary is a compile error", bin: []byte("\x00asm\x01\x00\x00\x00\x01\xff"), spec: headersSpec(), code: CodeCompile},
		{name: "not WebAssembly", bin: []byte("hello"), spec: headersSpec(), code: CodeCompile},
		{name: "an import beyond the granted Capabilities", bin: continueGuest("log", "request_header_get"), spec: headersSpec(v1alpha1.CapabilityLogWrite), code: CodeCapability, msg: "request.headers.read"},
		{name: "an import the host does not implement", bin: func() []byte {
			g := ht.NewGuest()
			g.M.ImportFunc(ht.ABIModule, "http_call", ht.Sig(ht.Params(ht.I32), ht.I32))
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "ruralz.plugin.v1.http_call"},
		{name: "an import of another module", bin: func() []byte {
			g := ht.NewGuest()
			g.M.ImportFunc("env", "abort", ht.Sig(nil))
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "env.abort"},
		{name: "a WASI function outside the shim", bin: func() []byte {
			g := ht.NewGuest("wasi.sched_yield")
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "sched_yield"},
		{name: "a Host Function imported with the wrong signature", bin: func() []byte {
			g := ht.NewGuest()
			g.M.ImportFunc(ht.ABIModule, "log", ht.Sig(ht.Params(ht.I32), ht.I32))
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(v1alpha1.CapabilityLogWrite), code: CodeArtifact, msg: "wrong signature"},
		{name: "a Phase export spec.phases does not list", bin: func() []byte {
			g := ht.NewGuest()
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			g.Phase("rz_on_response", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "onResponse"},
		{name: "a Phase spec.phases lists that is not exported", bin: continueGuest(), spec: &v1alpha1.PluginSpec{
			ABI: v1alpha1.PluginABIV1, Phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnLog},
		}, code: CodeArtifact, msg: "onLog"},
		{name: "rz_alloc missing", bin: noExport("rz_alloc"), spec: headersSpec(), code: CodeArtifact, msg: "rz_alloc"},
		{name: "rz_configure missing", bin: noExport("rz_configure"), spec: headersSpec(), code: CodeArtifact, msg: "rz_configure"},
		{name: "rz_abi_version missing", bin: noExport("rz_abi_version"), spec: headersSpec(), code: CodeArtifact, msg: "rz_abi_version"},
		{name: "a declared memory minimum above the limit", bin: func() []byte {
			g := ht.NewGuest()
			g.M.Memory(300, 0, false)
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "minimum"},
		{name: "a declared memory maximum above the limit", bin: func() []byte {
			g := ht.NewGuest()
			g.M.Memory(2, 65536, true)
			g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
			return g.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "maximum"},
		{name: "an imported memory", bin: func() []byte {
			var m ht.Module
			m.ImportMemory("env", "memory", 1)
			return m.Encode()
		}(), spec: headersSpec(), code: CodeArtifact, msg: "imports its memory"},
		{name: "no memory", bin: func() []byte { var m ht.Module; return m.Encode() }(), spec: headersSpec(), code: CodeArtifact, msg: "defines 0 memories"},
		{name: "another abi", bin: continueGuest(), spec: &v1alpha1.PluginSpec{ABI: "proxy-wasm", Phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}}, code: CodeArtifact},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkArtifact(tc.bin, tc.spec, 256)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("checkArtifact = %v, want nil", err)
				}
				return
			}
			wantCode(t, err, tc.code)
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("checkArtifact = %q, want it to mention %q", err, tc.msg)
			}
		})
	}
}

// TestStructureLimits covers the size and structure limits checked before
// compiling (RZ-CFG-048): 32 MiB, 100,000 functions, 50,000 locals per
// function and 16 MiB of data.
func TestStructureLimits(t *testing.T) {
	cases := []struct {
		name string
		bin  func() []byte
		msg  string
	}{
		{name: "a binary over 32 MiB", bin: func() []byte {
			var m ht.Module
			m.Custom("pad", make([]byte, maxArtifactBytes))
			return m.Encode()
		}, msg: "32"},
		{name: "more than 100,000 functions", bin: func() []byte {
			var m ht.Module
			payload := ht.U32(maxFunctions + 1)
			for range maxFunctions + 1 {
				payload = append(payload, 0)
			}
			m.Type(ht.Sig(nil))
			m.RawSection(ht.SectionFunction, payload)
			return m.Encode()
		}, msg: "100000"},
		{name: "a function over 50,000 locals", bin: func() []byte {
			var m ht.Module
			m.Func(ht.Sig(nil), []ht.Locals{{N: 30000, Type: ht.I32}, {N: 20001, Type: ht.I64}})
			return m.Encode()
		}, msg: "50000"},
		{name: "data over 16 MiB", bin: func() []byte {
			var m ht.Module
			m.Memory(1, 0, false)
			m.Data(0, make([]byte, maxDataBytes/2+1))
			m.Data(0, make([]byte, maxDataBytes/2+1))
			return m.Encode()
		}, msg: "16777216"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkArtifact(tc.bin(), headersSpec(), 256)
			wantCode(t, err, CodeStructure)
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("checkArtifact = %q, want it to mention %q", err, tc.msg)
			}
		})
	}
}

// TestDerivedCapabilities: the Capabilities an artifact requests are those
// of its imports, WASI's args and environ needing none.
func TestDerivedCapabilities(t *testing.T) {
	bin := continueGuest("log", "wasi.fd_write", "wasi.args_sizes_get", "wasi.clock_time_get", "state_get", "crypto_key_sign")
	a, err := checkArtifact(bin, headersSpec(v1alpha1.CapabilityLogWrite, v1alpha1.CapabilityClockRead, v1alpha1.CapabilityStateRead, v1alpha1.CapabilityCryptoUse), 256)
	if err != nil {
		t.Fatal(err)
	}
	want := capSet(0).add(capLogWrite).add(capClockRead).add(capStateRead).add(capCryptoUse)
	if a.requested != want {
		t.Errorf("requested = %s, want %s", a.requested, want)
	}
}

// TestPhaseExportNames pins the snake-case Phase exports.
func TestPhaseExportNames(t *testing.T) {
	cases := map[v1alpha1.Phase]string{
		v1alpha1.PhaseOnRequestHeaders:          "rz_on_request_headers",
		v1alpha1.PhaseOnRequestBody:             "rz_on_request_body",
		v1alpha1.PhaseOnRoute:                   "rz_on_route",
		v1alpha1.PhaseOnUpstreamRequest:         "rz_on_upstream_request",
		v1alpha1.PhaseOnUpstreamResponseHeaders: "rz_on_upstream_response_headers",
		v1alpha1.PhaseOnUpstreamResponseBody:    "rz_on_upstream_response_body",
		v1alpha1.PhaseOnResponse:                "rz_on_response",
		v1alpha1.PhaseOnLog:                     "rz_on_log",
		v1alpha1.PhaseOnChunk:                   "rz_on_chunk",
	}
	for v, want := range cases {
		p, ok := phase.Parse(v)
		if !ok {
			t.Fatalf("no Phase %q", v)
		}
		if got := phaseExport(p); got != want {
			t.Errorf("phaseExport(%s) = %q, want %q", v, got, want)
		}
	}
}
