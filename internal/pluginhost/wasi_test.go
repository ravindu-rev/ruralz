// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestWASIShim covers the shim table: args and environ with zero entries,
// fd_write to stdout as one log line and to another fd as EBADF, the host
// clock and random source, and ENOSYS for path calls.
func TestWASIShim(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("wasi.args_sizes_get", "wasi.environ_sizes_get", "wasi.fd_write", "wasi.clock_time_get", "wasi.random_get", "wasi.path_open", "wasi.fd_close")
	msg := g.Bytes([]byte("hello from wasi"))
	// One iovec: the message's address and its 15 bytes.
	iov := g.Bytes(binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, msg), 15))
	g.Phase("rz_on_request_headers", nil,
		ht.Check(g.Call("wasi.args_sizes_get", ht.I32Const(int32(ht.Scratch)), ht.I32Const(int32(ht.Scratch)+4)), wasiSuccess),
		ht.CheckMem(ht.Scratch, []byte{0, 0, 0, 0, 0, 0, 0, 0}),
		ht.Check(g.Call("wasi.environ_sizes_get", ht.I32Const(int32(ht.Scratch)), ht.I32Const(int32(ht.Scratch)+4)), wasiSuccess),
		ht.Check(g.Call("wasi.fd_write", ht.I32Const(1), ht.Addr(iov), ht.I32Const(1), ht.I32Const(int32(ht.Scratch))), wasiSuccess),
		ht.CheckMem(ht.Scratch, []byte{15, 0, 0, 0}),
		ht.Check(g.Call("wasi.fd_write", ht.I32Const(3), ht.Addr(iov), ht.I32Const(1), ht.I32Const(int32(ht.Scratch))), wasiBadf),
		ht.Check(g.Call("wasi.clock_time_get", ht.I32Const(0), ht.I64Const(1), ht.I32Const(int32(ht.Scratch))), wasiSuccess),
		ht.Check(g.Call("wasi.clock_time_get", ht.I32Const(7), ht.I64Const(1), ht.I32Const(int32(ht.Scratch))), wasiInval),
		ht.Check(g.Call("wasi.random_get", ht.I32Const(int32(ht.Scratch)), ht.I32Const(2)), wasiSuccess),
		ht.CheckMem(ht.Scratch, []byte{0, 1}),
		ht.Check(g.Call("wasi.path_open", ht.I32Const(3), ht.I32Const(0), ht.I32Const(0), ht.I32Const(0), ht.I32Const(0),
			ht.I64Const(0), ht.I64Const(0), ht.I32Const(0), ht.I32Const(int32(ht.Scratch))), wasiNosys),
		ht.Check(g.Call("wasi.fd_close", ht.I32Const(3)), wasiBadf),
		ht.I32Const(0))
	f := h.one(headersPlugin(g.Encode(), v1alpha1.CapabilityLogWrite, v1alpha1.CapabilityClockRead, v1alpha1.CapabilityRandomRead), buildOpts{})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if !strings.Contains(h.logs.String(), "hello from wasi") {
		t.Error("fd_write to stdout wrote no guest log line")
	}
}

// TestWASICapabilities: fd_write needs log.write, clock_time_get
// clock.read and random_get random.read; args, environ and proc_exit need
// none.
func TestWASICapabilities(t *testing.T) {
	for _, fn := range []string{"wasi.fd_write", "wasi.clock_time_get", "wasi.random_get"} {
		_, err := checkArtifact(continueGuest(fn), headersSpec(), 256)
		wantCode(t, err, CodeCapability)
	}
	if _, err := checkArtifact(continueGuest("wasi.args_get", "wasi.environ_get", "wasi.proc_exit"), headersSpec(), 256); err != nil {
		t.Errorf("args, environ and proc_exit need no Capability: %v", err)
	}
}
