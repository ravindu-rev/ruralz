// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"encoding/binary"

	"github.com/tetratelabs/wazero/api"
)

// WASI errno values of wasi_snapshot_preview1.
const (
	wasiSuccess int32 = 0
	wasiBadf    int32 = 8
	wasiInval   int32 = 28
	wasiNosys   int32 = 52
)

// WASI clock identifiers.
const (
	wasiClockRealtime  = 0
	wasiClockMonotonic = 1
)

// wasiFuncs returns the WASI shim (docs/architecture/05-wasm-plugin-
// system.md "Host Function table"): args_* and environ_* succeed with zero
// entries and need no Capability; fd_write to fd 1 or 2 is one guest log
// line under log.write; clock_time_get and clock_res_get read the host
// clock under clock.read; random_get reads the host random source under
// random.read; proc_exit traps (RZ-PLG-001); every other fd_* returns
// EBADF and path_*, sock_* and poll_oneoff return ENOSYS. An import of any
// other WASI name fails activation (RZ-CFG-028).
func wasiFuncs() []hostFunc {
	all := allPhases()
	none := func(name string, params []api.ValueType, fn func(*call, api.Memory, []uint64) int32) hostFunc {
		return hostFunc{name: name, params: params, capability: capNone, phases: all, configure: true, fn: fn}
	}
	badf := func(*call, api.Memory, []uint64) int32 { return wasiBadf }
	nosys := func(*call, api.Memory, []uint64) int32 { return wasiNosys }
	return []hostFunc{
		none("args_get", sig(i32, i32), wasiNoArgs),
		none("args_sizes_get", sig(i32, i32), wasiZeroSizes),
		none("environ_get", sig(i32, i32), wasiNoArgs),
		none("environ_sizes_get", sig(i32, i32), wasiZeroSizes),
		{name: "clock_res_get", params: sig(i32, i32), capability: capClockRead, phases: all, configure: true, fn: wasiClockRes},
		{name: "clock_time_get", params: sig(i32, i64, i32), capability: capClockRead, phases: all, configure: true, fn: wasiClockTime},
		{name: "fd_write", params: sig(i32, i32, i32, i32), capability: capLogWrite, phases: all, configure: true, fn: wasiFdWrite},
		{name: "random_get", params: sig(i32, i32), capability: capRandomRead, phases: all, configure: true, fn: wasiRandom},
		{name: "proc_exit", params: sig(i32), capability: capNone, phases: all, configure: true, fn: wasiProcExit, void: true},
		none("fd_advise", sig(i32, i64, i64, i32), badf),
		none("fd_allocate", sig(i32, i64, i64), badf),
		none("fd_close", sig(i32), badf),
		none("fd_datasync", sig(i32), badf),
		none("fd_fdstat_get", sig(i32, i32), badf),
		none("fd_fdstat_set_flags", sig(i32, i32), badf),
		none("fd_fdstat_set_rights", sig(i32, i64, i64), badf),
		none("fd_filestat_get", sig(i32, i32), badf),
		none("fd_filestat_set_size", sig(i32, i64), badf),
		none("fd_filestat_set_times", sig(i32, i64, i64, i32), badf),
		none("fd_pread", sig(i32, i32, i32, i64, i32), badf),
		none("fd_prestat_get", sig(i32, i32), badf),
		none("fd_prestat_dir_name", sig(i32, i32, i32), badf),
		none("fd_pwrite", sig(i32, i32, i32, i64, i32), badf),
		none("fd_read", sig(i32, i32, i32, i32), badf),
		none("fd_readdir", sig(i32, i32, i32, i64, i32), badf),
		none("fd_renumber", sig(i32, i32), badf),
		none("fd_seek", sig(i32, i64, i32, i32), badf),
		none("fd_sync", sig(i32), badf),
		none("fd_tell", sig(i32, i32), badf),
		none("path_create_directory", sig(i32, i32, i32), nosys),
		none("path_filestat_get", sig(i32, i32, i32, i32, i32), nosys),
		none("path_filestat_set_times", sig(i32, i32, i32, i32, i64, i64, i32), nosys),
		none("path_link", sig(i32, i32, i32, i32, i32, i32, i32), nosys),
		none("path_open", sig(i32, i32, i32, i32, i32, i64, i64, i32, i32), nosys),
		none("path_readlink", sig(i32, i32, i32, i32, i32, i32), nosys),
		none("path_remove_directory", sig(i32, i32, i32), nosys),
		none("path_rename", sig(i32, i32, i32, i32, i32, i32), nosys),
		none("path_symlink", sig(i32, i32, i32, i32, i32), nosys),
		none("path_unlink_file", sig(i32, i32, i32), nosys),
		none("poll_oneoff", sig(i32, i32, i32, i32), nosys),
		none("sock_accept", sig(i32, i32, i32), nosys),
		none("sock_recv", sig(i32, i32, i32, i32, i32, i32), nosys),
		none("sock_send", sig(i32, i32, i32, i32, i32), nosys),
		none("sock_shutdown", sig(i32, i32), nosys),
	}
}

// wasiNoArgs is args_get and environ_get: zero entries, nothing to write.
func wasiNoArgs(*call, api.Memory, []uint64) int32 { return wasiSuccess }

// wasiZeroSizes is args_sizes_get and environ_sizes_get: zero entries of
// zero bytes.
func wasiZeroSizes(_ *call, m api.Memory, stack []uint64) int32 {
	countOut, sizeOut := u32(stack, 0), u32(stack, 1)
	checkRange(m, countOut, 4)
	checkRange(m, sizeOut, 4)
	m.WriteUint32Le(countOut, 0)
	m.WriteUint32Le(sizeOut, 0)
	return wasiSuccess
}

// wasiClockRes is clock_res_get: one nanosecond for the realtime and
// monotonic clocks.
func wasiClockRes(_ *call, m api.Memory, stack []uint64) int32 {
	id, out := u32(stack, 0), u32(stack, 1)
	if id != wasiClockRealtime && id != wasiClockMonotonic {
		return wasiInval
	}
	checkRange(m, out, 8)
	m.WriteUint64Le(out, 1)
	return wasiSuccess
}

// wasiClockTime is clock_time_get: the host clock in Unix nanoseconds for
// the realtime clock, and nanoseconds since the engine started for the
// monotonic one.
func wasiClockTime(c *call, m api.Memory, stack []uint64) int32 {
	id, out := u32(stack, 0), u32(stack, 2)
	var ns int64
	switch id {
	case wasiClockRealtime:
		ns = c.e.clk.Now().UnixNano()
	case wasiClockMonotonic:
		ns = int64(c.e.clk.Since(c.e.started))
	default:
		return wasiInval
	}
	checkRange(m, out, 8)
	m.WriteUint64Le(out, uint64(ns))
	return wasiSuccess
}

// wasiFdWrite is fd_write: to fd 1 or 2, the iovecs' bytes are one guest
// log line (info for stdout, error for stderr); any other fd is EBADF.
func wasiFdWrite(c *call, m api.Memory, stack []uint64) int32 {
	fd, iovs, n, out := u32(stack, 0), u32(stack, 1), u32(stack, 2), u32(stack, 3)
	if fd != 1 && fd != 2 {
		return wasiBadf
	}
	if uint64(n)*8 > uint64(m.Size()) {
		panic(errOutOfBounds)
	}
	table := mem(m, iovs, n*8)
	var line []byte
	var total uint64
	for i := range n {
		p := binary.LittleEndian.Uint32(table[i*8:])
		l := binary.LittleEndian.Uint32(table[i*8+4:])
		checkRange(m, p, l)
		total += uint64(l)
		if len(line) < logMessageBytes+1 {
			chunk := mem(m, p, min(l, uint32(logMessageBytes+1-len(line)))) //nolint:gosec // G115: the remaining room is below the message cap.
			line = append(line, chunk...)
		}
	}
	checkRange(m, out, 4)
	level := levelInfo
	if fd == 2 {
		level = levelError
	}
	c.guestLog(level, line, total)
	m.WriteUint32Le(out, uint32(min(total, 1<<32-1)))
	return wasiSuccess
}

// wasiRandom is random_get: bytes from the host random source.
func wasiRandom(c *call, m api.Memory, stack []uint64) int32 {
	buf, n := u32(stack, 0), u32(stack, 1)
	checkRange(m, buf, n)
	b := make([]byte, n)
	c.e.randomBytes(b)
	if n > 0 {
		m.Write(buf, b)
	}
	return wasiSuccess
}

// wasiProcExit is proc_exit: a trap, RZ-PLG-001.
func wasiProcExit(*call, api.Memory, []uint64) int32 { panic(errProcExit) }
