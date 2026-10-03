// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenHolder is the record in testdata/holder.golden.json.
func goldenHolder(t testing.TB) Holder {
	t.Helper()
	return Holder{
		Format:       HolderFormat,
		PID:          4242,
		StartTime:    1234567,
		NodeID:       testULID(t),
		Version:      "0.1.0",
		PIDNamespace: "pid:[4026531836]",
	}
}

// TestHolderGolden covers spec 04 requirement 5, spec 10 requirement 95
// and test plan item 3 "holder.json golden": member order format, pid,
// startTime, nodeId, version, pidNamespace; one line.
func TestHolderGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "holder.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := goldenHolder(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode:\n got %s\nwant %s", got, want)
	}
	h, err := DecodeHolder(want)
	if err != nil {
		t.Fatal(err)
	}
	if h != goldenHolder(t) {
		t.Fatalf("DecodeHolder = %+v", h)
	}
	// pidNamespace is omitted where /proc does not exist.
	noNS := goldenHolder(t)
	noNS.PIDNamespace = ""
	got, err = noNS.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "pidNamespace") {
		t.Errorf("empty pidNamespace encoded: %s", got)
	}
}

// TestHolderRoundTrip is the "holder round trip" of the work package:
// WriteHolder under the lock, ReadHolder and ReadHolderAt read it back,
// mode 0600, and Release removes it (spec 04 requirement 63 "lock and
// holder.json released").
func TestHolderRoundTrip(t *testing.T) {
	d, lk := openLocked(t)
	h := goldenHolder(t)
	h.Format = "" // WriteHolder fills it
	if err := lk.WriteHolder(h); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, d.HolderPath(), 0o600)
	want, _ := os.ReadFile(filepath.Join("testdata", "holder.golden.json"))
	if got, _ := os.ReadFile(d.HolderPath()); !bytes.Equal(got, want) {
		t.Errorf("holder.json = %s, want the golden bytes", got)
	}
	for _, read := range []func() (Holder, error){d.ReadHolder, func() (Holder, error) { return ReadHolderAt(d.Root()) }} {
		got, err := read()
		if err != nil || got != goldenHolder(t) {
			t.Fatalf("read = %+v, %v", got, err)
		}
	}
	// A rewrite replaces the record atomically.
	h.PID = 4343
	if err := lk.WriteHolder(h); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReadHolder(); err != nil || got.PID != 4343 {
		t.Fatalf("after rewrite = %+v, %v", got, err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadHolder(); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("after Release err = %v, want ErrNoHolder", err)
	}
}

// TestReadHolderAt covers the CLI's reading rules (spec 10 requirement
// 96.1): missing is ErrNoHolder (and fs.ErrNotExist); unparsable, another
// format, no PID or no node.id is ErrHolderInvalid; unknown members are
// ignored.
func TestReadHolderAt(t *testing.T) {
	const ok = `{"format":"ruralz.holder.v1","pid":7,"startTime":9,"nodeId":"01ARYZ6S41TSV4RRFFQ69G5FAV","version":"v"`
	cases := []struct {
		name    string
		content *string
		want    error
		pid     int
	}{
		{"missing", nil, ErrNoHolder, 0},
		{"minimal", ptr(ok + "}"), nil, 7},
		{"additive member", ptr(ok + `,"future":{"x":[1]}}`), nil, 7},
		{"no pidNamespace", ptr(ok + "}\n"), nil, 7},
		{"empty", ptr(""), ErrHolderInvalid, 0},
		{"garbage", ptr("pid=7"), ErrHolderInvalid, 0},
		{"truncated", ptr(ok), ErrHolderInvalid, 0},
		{"trailing data", ptr(ok + "}{}"), ErrHolderInvalid, 0},
		{"array", ptr("[]"), ErrHolderInvalid, 0},
		{"other format", ptr(strings.Replace(ok, "v1", "v2", 1) + "}"), ErrHolderInvalid, 0},
		{"no format", ptr(`{"pid":7,"nodeId":"01ARYZ6S41TSV4RRFFQ69G5FAV"}`), ErrHolderInvalid, 0},
		{"pid zero", ptr(strings.Replace(ok, `"pid":7`, `"pid":0`, 1) + "}"), ErrHolderInvalid, 0},
		{"pid negative", ptr(strings.Replace(ok, `"pid":7`, `"pid":-1`, 1) + "}"), ErrHolderInvalid, 0},
		{"pid string", ptr(strings.Replace(ok, `"pid":7`, `"pid":"7"`, 1) + "}"), ErrHolderInvalid, 0},
		{"startTime negative", ptr(strings.Replace(ok, `"startTime":9`, `"startTime":-9`, 1) + "}"), ErrHolderInvalid, 0},
		{"bad nodeId", ptr(strings.Replace(ok, "01ARYZ6S41TSV4RRFFQ69G5FAV", "01ARYZ6S41TSV4RRFFQ69G5FAI", 1) + "}"), ErrHolderInvalid, 0},
		{"no nodeId", ptr(`{"format":"ruralz.holder.v1","pid":7}`), ErrHolderInvalid, 0},
		{"too large", ptr(ok + `,"pad":"` + strings.Repeat("x", MaxHolderBytes) + `"}`), ErrHolderInvalid, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.content != nil {
				if err := os.WriteFile(filepath.Join(root, HolderFile), []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h, err := ReadHolderAt(root)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				if errors.Is(tc.want, ErrNoHolder) && !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("err = %v, want it to match fs.ErrNotExist", err)
				}
				return
			}
			if err != nil || h.PID != tc.pid {
				t.Fatalf("ReadHolderAt = %+v, %v", h, err)
			}
		})
	}
	// Reading creates nothing (spec 10 requirement 95).
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := ReadHolderAt(root); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadHolderAt created %s", root)
	}
}

func ptr(s string) *string { return &s }

// TestHolderValidate covers Encode's refusal of invalid records.
func TestHolderValidate(t *testing.T) {
	for _, mutate := range []func(*Holder){
		func(h *Holder) { h.Format = "ruralz.holder.v2" },
		func(h *Holder) { h.PID = 0 },
		func(h *Holder) { h.NodeID = [16]byte{} },
	} {
		h := goldenHolder(t)
		mutate(&h)
		if _, err := h.Encode(); !errors.Is(err, ErrHolderInvalid) {
			t.Errorf("Encode(%+v) err = %v, want ErrHolderInvalid", h, err)
		}
	}
}

// TestNewHolder covers the calling process's record (spec 10 requirement
// 95): PID, and on Linux the start time and PID namespace from /proc.
func TestNewHolder(t *testing.T) {
	h, err := NewHolder(testULID(t), "1.2.3")
	if err != nil {
		t.Logf("NewHolder: %v (proc not readable here)", err)
	}
	if h.Format != HolderFormat || h.PID != os.Getpid() || h.NodeID != testULID(t) || h.Version != "1.2.3" {
		t.Fatalf("NewHolder = %+v", h)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	checkProcIdentity(t, h)
}

// TestParseStatStartTime covers field 22 of /proc/<pid>/stat, counted
// after the last ')' because the command name may hold both (spec 10
// requirement 96.4).
func TestParseStatStartTime(t *testing.T) {
	rest := " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20 21\n"
	cases := []struct {
		name string
		line string
		want uint64
		ok   bool
	}{
		{"plain", "123 (ruralzd)" + rest, 424242, true},
		{"spaces and parens in comm", "123 (a) b ) (c)" + rest, 424242, true},
		{"empty comm", "1 ()" + rest, 424242, true},
		{"no paren", "123 ruralzd" + rest, 0, false},
		{"short", "123 (x) S 1 2 3", 0, false},
		{"not a number", "123 (x) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 x", 0, false},
		{"overflow", "123 (x) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 99999999999999999999", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatStartTime([]byte(tc.line))
			if tc.ok != (err == nil) || got != tc.want {
				t.Fatalf("parseStatStartTime = %d, %v; want %d, ok=%v", got, err, tc.want, tc.ok)
			}
			if err != nil && !errors.Is(err, errStat) {
				t.Errorf("err = %v, want errStat", err)
			}
		})
	}
}

// FuzzReadHolder checks that any holder.json bytes either decode to a
// valid record that re-encodes and re-decodes to itself, or fail with
// ErrHolderInvalid; never a panic.
func FuzzReadHolder(f *testing.F) {
	golden, err := os.ReadFile(filepath.Join("testdata", "holder.golden.json"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(golden)
	f.Add([]byte(`{"format":"ruralz.holder.v1","pid":1,"nodeId":"7ZZZZZZZZZZZZZZZZZZZZZZZZZ","x":null}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := DecodeHolder(data)
		if err != nil {
			if !errors.Is(err, ErrHolderInvalid) {
				t.Fatalf("err = %v, want ErrHolderInvalid", err)
			}
			return
		}
		enc, err := h.Encode()
		if err != nil {
			t.Fatalf("valid record does not encode: %v", err)
		}
		back, err := DecodeHolder(enc)
		if err != nil || back != h {
			t.Fatalf("re-decode = %+v, %v; want %+v", back, err, h)
		}
	})
}

// FuzzParseStatStartTime checks the /proc stat parser never panics and
// only accepts lines with a command name.
func FuzzParseStatStartTime(f *testing.F) {
	f.Add([]byte("123 (ruralzd) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20 21\n"))
	f.Add([]byte("1 (a) b)) S"))
	f.Fuzz(func(t *testing.T, line []byte) {
		if _, err := parseStatStartTime(line); err == nil && !bytes.Contains(line, []byte(")")) {
			t.Fatalf("accepted %q without a command name", line)
		}
	})
}
