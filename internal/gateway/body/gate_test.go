// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for spec 04 reqs 34, 37, 46 and 47 (gate reader within
// maxRequestBodyBytes, 413 RZ-RT-003 without reading on a declared
// Content-Length, reservation as bytes arrive, ErrBudget 503 RZ-RT-004,
// limited reader), spec 07 reqs 15 and 72 (oversize by side) and 73
// (pooled scratch buffers at most 64 KiB); test plan item 8.

// spyReader counts Read calls and checks, at each call, that the budget
// grows with the bytes delivered so far: the gate reserves the capacity it
// is about to fill, which doubles as bytes arrive, so it holds at most one
// increment before the first byte and at most twice the increments covering
// the bytes delivered afterwards (spec 04 req 47).
type spyReader struct {
	t     *testing.T
	r     io.Reader
	bud   *Budget
	calls int
	given int64
}

func (s *spyReader) Read(p []byte) (int, error) {
	s.calls++
	if s.bud != nil {
		if used, most := s.bud.GateUsed(), max(1, 2*increments(s.given))*Increment; used > most {
			s.t.Errorf("reserved %d with %d bytes delivered; at most %d (reserve as bytes arrive)", used, s.given, most)
		}
	}
	n, err := s.r.Read(p)
	s.given += int64(n)
	return n, err
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestReq46ReadGate(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		declared int64
		limit    int64
		wrap     func(io.Reader) io.Reader
		err      error
		reserved int64
	}{
		{"1 KiB declared", 1 * kib, 1 * kib, 10 << 20, nil, nil, 32 * kib},
		{"100 KiB chunked", 100 * kib, -1, 10 << 20, nil, nil, 128 * kib},
		// Doubling reaches 256 KiB; the reservation covers that capacity.
		{"200 KiB chunked", 200 * kib, -1, 10 << 20, nil, nil, 256 * kib},
		// The declared length caps the growth at 200 KiB (7 increments).
		{"200 KiB declared", 200 * kib, 200 * kib, 10 << 20, nil, nil, 224 * kib},
		// The limit caps the growth: 8 MiB + 1 byte under a 10 MiB limit
		// never reaches a 16 MiB array.
		{"8 MiB + 1 chunked", 8<<20 + 1, -1, 10 << 20, nil, nil, 10 << 20},
		{"exactly one increment", 32 * kib, -1, 10 << 20, nil, nil, 32 * kib},
		{"one byte past an increment", 32*kib + 1, -1, 10 << 20, nil, nil, 64 * kib},
		{"exactly the limit", 64 * kib, -1, 64 * kib, nil, nil, 64 * kib},
		{"limit not a multiple of an increment", 40 * kib, -1, 40 * kib, nil, nil, 64 * kib},
		{"one byte over the limit (chunked)", 64*kib + 1, -1, 64 * kib, nil, ErrTooLarge, 0},
		{"far over the limit", 1 << 20, -1, 64 * kib, nil, ErrTooLarge, 0},
		{"declared understates the body", 3 * kib, 1 * kib, 10 * kib, nil, nil, 32 * kib},
		{"one byte at a time", 40 * kib, -1, 1 << 20, iotest.OneByteReader, nil, 64 * kib},
		{"half reads", 70 * kib, 70 * kib, 1 << 20, iotest.HalfReader, nil, 96 * kib},
		{"data with EOF", 5 * kib, -1, 1 << 20, iotest.DataErrReader, nil, 32 * kib},
		{"over the limit, one byte at a time", 9, -1, 8, iotest.OneByteReader, ErrTooLarge, 0},
		{"zero limit, empty body", 0, -1, 0, nil, nil, 0},
		{"zero limit, one byte", 1, -1, 0, nil, ErrTooLarge, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bud := NewBudget(64<<20, nil)
			data := pattern(tt.size)
			var r io.Reader = bytes.NewReader(data)
			if tt.wrap != nil {
				r = tt.wrap(r)
			}
			spy := &spyReader{t: t, r: r, bud: bud}
			g, err := ReadGate(context.Background(), spy, tt.declared, tt.limit, bud, 0)
			if tt.err != nil {
				if !errors.Is(err, tt.err) || g != nil {
					t.Fatalf("ReadGate = %v, %v; want %v", g, err, tt.err)
				}
				if bud.Used() != 0 {
					t.Fatalf("a failed gate kept %d bytes reserved", bud.Used())
				}
				if Code(err, SideRequest) != "RZ-RT-003" {
					t.Fatalf("code %q", Code(err, SideRequest))
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadGate: %v", err)
			}
			if !bytes.Equal(g.Bytes(), data) || g.Len() != len(data) {
				t.Fatalf("read %d bytes, want %d", g.Len(), len(data))
			}
			if g.Reserved() != tt.reserved || bud.GateUsed() != tt.reserved {
				t.Fatalf("reserved %d (budget %d), want %d", g.Reserved(), bud.GateUsed(), tt.reserved)
			}
			// The reservation covers the memory the gate holds.
			if int64(cap(g.b)) > g.Reserved() {
				t.Fatalf("capacity %d over the reservation %d", cap(g.b), g.Reserved())
			}
			g.Release()
			if g.Len() != 0 || g.Reserved() != 0 || g.Bytes() != nil {
				t.Fatal("a released Buffer is not inert")
			}
			g.Release() // the handle is inert: a second Release is a no-op
			if bud.Used() != 0 {
				t.Fatalf("Used = %d after Release", bud.Used())
			}
		})
	}
}

// TestReq34DeclaredOverLimitNotRead: a declared Content-Length over the
// limit is 413 RZ-RT-003 without calling the reader, so net/http sends no
// 100 Continue.
func TestReq34DeclaredOverLimitNotRead(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	spy := &spyReader{t: t, r: bytes.NewReader(pattern(10))}
	g, err := ReadGate(context.Background(), spy, 11, 10, bud, 0)
	if !errors.Is(err, ErrTooLarge) || g != nil {
		t.Fatalf("ReadGate = %v, %v", g, err)
	}
	if spy.calls != 0 {
		t.Fatalf("the reader was called %d times", spy.calls)
	}
	if code := Code(err, SideRequest); code != "RZ-RT-003" || errcode.Status(code) != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %q", code)
	}
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

func TestReq46ReadGateEmpty(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	spy := &spyReader{t: t, r: bytes.NewReader(pattern(10))}
	for _, tc := range []struct {
		name     string
		r        io.Reader
		declared int64
	}{
		{"declared zero", spy, 0},
		{"nil reader", nil, -1},
		{"http.NoBody", http.NoBody, -1},
		{"empty stream", bytes.NewReader(nil), -1},
	} {
		g, err := ReadGate(context.Background(), tc.r, tc.declared, 10, bud, 0)
		if err != nil || g.Len() != 0 || g.Reserved() != 0 {
			t.Fatalf("%s: ReadGate = %v, %v", tc.name, g, err)
		}
		g.Release()
	}
	if spy.calls != 0 || bud.Used() != 0 {
		t.Fatalf("calls %d, used %d", spy.calls, bud.Used())
	}
}

// TestReq47ReadGateBudget: the budget refusing an increment mid-body is
// ErrBudget (503 RZ-RT-004) and returns what the gate had reserved.
func TestReq47ReadGateBudget(t *testing.T) {
	// 128 KiB total, 32 KiB share: gates get 96 KiB.
	bud := NewBudget(128*kib, nil)
	g, err := ReadGate(context.Background(), bytes.NewReader(pattern(100*kib)), -1, 1<<20, bud, 0)
	if !errors.Is(err, ErrBudget) || g != nil {
		t.Fatalf("ReadGate = %v, %v; want ErrBudget", g, err)
	}
	if bud.Used() != 0 {
		t.Fatalf("Used = %d after a refused gate", bud.Used())
	}
	for _, side := range []Side{SideRequest, SideResponse, SideStep} {
		if code := Code(err, side); code != "RZ-RT-004" || errcode.Status(code) != http.StatusServiceUnavailable {
			t.Fatalf("side %d: code %q", side, code)
		}
	}
	// A body within the gate part of the budget passes.
	g, err = ReadGate(context.Background(), bytes.NewReader(pattern(96*kib)), -1, 1<<20, bud, 0)
	if err != nil {
		t.Fatalf("ReadGate within the budget: %v", err)
	}
	g.Release()
}

// TestReq46ReadGateErrors: a read error or a done context ends the gate and
// returns the reservation.
func TestReq46ReadGateErrors(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	boom := errors.New("client reset")
	r := io.MultiReader(bytes.NewReader(pattern(40*kib)), iotest.ErrReader(boom))
	if _, err := ReadGate(context.Background(), r, -1, 1<<20, bud, 0); !errors.Is(err, boom) {
		t.Fatalf("ReadGate = %v, want the read error", err)
	}
	if Code(boom, SideRequest) != "" {
		t.Fatal("a read error got a body code")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadGate(ctx, bytes.NewReader(pattern(10)), -1, 1<<20, bud, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadGate = %v, want context.Canceled", err)
	}
	// An error on the probe past the limit.
	r = io.MultiReader(bytes.NewReader(pattern(10)), iotest.ErrReader(boom))
	if _, err := ReadGate(context.Background(), r, -1, 10, bud, 0); !errors.Is(err, boom) {
		t.Fatalf("probe error = %v", err)
	}
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// zeroReader returns (0, nil) n times before delegating.
type zeroReader struct {
	n int
	r io.Reader
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n > 0 {
		z.n--
		return 0, nil
	}
	return z.r.Read(p)
}

// TestReq46ReadGateZeroReads tolerates (0, nil) reads, also at the limit.
func TestReq46ReadGateZeroReads(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	data := pattern(10)
	g, err := ReadGate(context.Background(), &zeroReader{n: 3, r: iotest.OneByteReader(bytes.NewReader(data))}, -1, 10, bud, 0)
	if err != nil || !bytes.Equal(g.Bytes(), data) {
		t.Fatalf("ReadGate = %v", err)
	}
	g.Release()
	// (0, nil) on the probe past an exact-limit body, then EOF.
	r := io.MultiReader(bytes.NewReader(data), &zeroReader{n: 2, r: bytes.NewReader(nil)})
	g, err = ReadGate(context.Background(), r, -1, 10, bud, 0)
	if err != nil || g.Len() != 10 {
		t.Fatalf("ReadGate = %v", err)
	}
	g.Release()
}

// TestReq47ReadGateStaleRelease: a Buffer is never reused, so a second
// Release by a stale owner after another gate took its array from the pool
// changes nothing (spec 07 req 73).
func TestReq47ReadGateStaleRelease(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	g1, err := ReadGate(context.Background(), bytes.NewReader(bytes.Repeat([]byte("A"), kib)), -1, 1<<20, bud, 0)
	if err != nil {
		t.Fatal(err)
	}
	g1.Release()
	want := bytes.Repeat([]byte("B"), kib)
	g2, err := ReadGate(context.Background(), bytes.NewReader(want), -1, 1<<20, bud, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g1 == g2 {
		t.Fatal("ReadGate handed out a released Buffer again")
	}
	g1.Release()
	if !bytes.Equal(g2.Bytes(), want) || bud.GateUsed() != Increment || g2.Reserved() != Increment {
		t.Fatalf("a stale Release changed another gate: %d bytes, budget %d", g2.Len(), bud.GateUsed())
	}
	g2.Release()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq47ReadGateNilBudget: without a budget a non-empty body is
// ErrBudget (503 RZ-RT-004), never a panic; an empty one needs no budget.
func TestReq47ReadGateNilBudget(t *testing.T) {
	tests := []struct {
		name     string
		r        io.Reader
		declared int64
		limit    int64
		err      error
	}{
		{"chunked body", bytes.NewReader(pattern(10)), -1, 1 << 20, ErrBudget},
		{"declared body", bytes.NewReader(pattern(10)), 10, 1 << 20, ErrBudget},
		{"empty stream", bytes.NewReader(nil), -1, 1 << 20, nil},
		{"declared zero", bytes.NewReader(pattern(10)), 0, 1 << 20, nil},
		{"nil reader", nil, -1, 1 << 20, nil},
		{"zero limit, empty", bytes.NewReader(nil), -1, 0, nil},
		{"zero limit, one byte", bytes.NewReader(pattern(1)), -1, 0, ErrTooLarge},
		{"(0, nil) then a byte", &zeroReader{n: 2, r: bytes.NewReader(pattern(1))}, -1, 1 << 20, ErrBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := ReadGate(context.Background(), tt.r, tt.declared, tt.limit, nil, 0)
			if tt.err != nil {
				if !errors.Is(err, tt.err) || g != nil {
					t.Fatalf("ReadGate = %v, %v; want %v", g, err, tt.err)
				}
				return
			}
			if err != nil || g.Len() != 0 || g.Reserved() != 0 {
				t.Fatalf("ReadGate = %v, %v", g, err)
			}
			g.Release()
		})
	}
	var q Request
	err := q.Prepare(context.Background(), bytes.NewReader(pattern(10)), -1, true, 1<<20, nil, 0)
	if !errors.Is(err, ErrBudget) || Code(err, SideRequest) != "RZ-RT-004" {
		t.Fatalf("Prepare without a budget = %v", err)
	}
}

// TestReq47ReadGateGrowthNearCeiling: near the ceiling the gate grows in
// smaller steps (halving) instead of failing, and still never holds more
// memory than it reserved.
func TestReq47ReadGateGrowthNearCeiling(t *testing.T) {
	// 512 KiB total, 128 KiB share: 384 KiB (12 increments) for gates.
	bud := NewBudget(512*kib, nil)
	var other Account
	other.Bind(bud, Gate, 0)
	// Another holder takes 5 increments, leaving 7: doubling to 256 KiB
	// (8 increments) does not fit, so the gate takes what is left.
	if err := other.Reserve(5 * Increment); err != nil {
		t.Fatal(err)
	}
	data := pattern(224 * kib)
	g, err := ReadGate(context.Background(), bytes.NewReader(data), -1, 1<<20, bud, 0)
	if err != nil {
		t.Fatalf("ReadGate: %v", err)
	}
	if !bytes.Equal(g.Bytes(), data) || g.Reserved() != 224*kib || int64(cap(g.b)) > g.Reserved() {
		t.Fatalf("read %d bytes, reserved %d, capacity %d", g.Len(), g.Reserved(), cap(g.b))
	}
	g.Release()
	// One byte more than the free budget is ErrBudget.
	if _, err := ReadGate(context.Background(), bytes.NewReader(pattern(224*kib+1)), -1, 1<<20, bud, 0); !errors.Is(err, ErrBudget) {
		t.Fatalf("ReadGate past the ceiling = %v, want ErrBudget", err)
	}
	other.ReleaseAll()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq47ReadGateDeclaredGrowth: a client declaring a large body but
// sending little holds a buffer sized to what arrived.
func TestReq47ReadGateDeclaredGrowth(t *testing.T) {
	bud := NewBudget(64<<20, nil)
	g, err := ReadGate(context.Background(), bytes.NewReader([]byte("x")), 10<<20, 10<<20, bud, 0)
	if err != nil {
		t.Fatalf("ReadGate: %v", err)
	}
	if cap(g.b) > Increment || g.Reserved() != Increment {
		t.Fatalf("capacity %d, reserved %d for one byte", cap(g.b), g.Reserved())
	}
	g.Release()
}

// TestReq73GatePool: arrays of one or two increments (at most 64 KiB) are
// pooled, larger ones and odd sizes are left to the garbage collector.
func TestReq73GatePool(t *testing.T) {
	bud := NewBudget(64<<20, nil)
	for _, c := range []int{0, kib, 3 * Increment, 200 * kib} {
		bud.recycle(make([]byte, 0, c))
	}
	if bud.inc1.Get() != nil || bud.inc2.Get() != nil {
		t.Fatal("an array that is not one or two increments was pooled")
	}
	for _, c := range []int64{Increment, PoolMax, 3 * Increment} {
		a := bud.array(c)
		if len(a) != 0 || int64(cap(a)) != c {
			t.Fatalf("array(%d): len %d cap %d", c, len(a), cap(a))
		}
		bud.recycle(a)
	}
	// A pooled array comes back empty with its exact capacity (sync.Pool
	// may drop it, in which case a new one is made).
	for _, c := range []int64{Increment, PoolMax} {
		if a := bud.array(c); len(a) != 0 || int64(cap(a)) != c {
			t.Fatalf("array(%d) after recycle: len %d cap %d", c, len(a), cap(a))
		}
	}
	g, err := ReadGate(context.Background(), bytes.NewReader(pattern(200*kib)), -1, 1<<20, bud, 0)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	if g.b != nil {
		t.Fatal("a released gate kept its array")
	}
}

func TestReq37Limited(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		limit int64
		wrap  func(io.Reader) io.Reader
		want  int
		err   error
	}{
		{"under", 10, 20, nil, 10, nil},
		{"exactly the limit", 20, 20, nil, 20, nil},
		{"one over", 21, 20, nil, 20, ErrTooLarge},
		{"far over", 1 << 16, 20, nil, 20, ErrTooLarge},
		{"one byte at a time, exact", 20, 20, iotest.OneByteReader, 20, nil},
		{"one byte at a time, over", 21, 20, iotest.OneByteReader, 20, ErrTooLarge},
		{"zero limit, empty", 0, 0, nil, 0, nil},
		{"zero limit, one byte", 1, 0, nil, 0, ErrTooLarge},
		{"negative limit is zero", 1, -4, nil, 0, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r io.Reader = bytes.NewReader(pattern(tt.size))
			if tt.wrap != nil {
				r = tt.wrap(r)
			}
			l := LimitReader(r, tt.limit)
			got, err := io.ReadAll(l)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("ReadAll error %v, want %v", err, tt.err)
			}
			if len(got) != tt.want || l.Count() != int64(tt.want) || !bytes.Equal(got, pattern(tt.size)[:tt.want]) {
				t.Fatalf("read %d bytes (count %d), want %d", len(got), l.Count(), tt.want)
			}
			if l.Exceeded() != (tt.err != nil) {
				t.Fatalf("Exceeded = %v", l.Exceeded())
			}
			// The error is sticky.
			if n, err2 := l.Read(make([]byte, 4)); n != 0 || (tt.err != nil && !errors.Is(err2, tt.err)) || (tt.err == nil && !errors.Is(err2, io.EOF)) {
				t.Fatalf("Read after the end = %d, %v", n, err2)
			}
		})
	}
	var zero Limited
	if n, err := zero.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("zero Limited = %d, %v", n, err)
	}
	l := LimitReader(bytes.NewReader([]byte("abc")), 5)
	if n, err := l.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty Read = %d, %v", n, err)
	}
}

func TestReq47Codes(t *testing.T) {
	other := errors.New("other")
	tests := []struct {
		err  error
		side Side
		want string
	}{
		{nil, SideRequest, ""},
		{other, SideRequest, ""},
		{ErrBudget, SideRequest, "RZ-RT-004"},
		{ErrBudget, SideResponse, "RZ-RT-004"},
		{ErrBudget, SideStep, "RZ-RT-004"},
		{ErrTooLarge, SideRequest, "RZ-RT-003"},
		{ErrTooLarge, SideResponse, "RZ-UP-010"},
		{ErrTooLarge, SideStep, "RZ-RT-015"},
		{fmt.Errorf("decode: %w", ErrTooLarge), SideResponse, "RZ-UP-010"},
		{fmt.Errorf("gate: %w", ErrBudget), SideStep, "RZ-RT-004"},
	}
	statuses := map[string]int{"RZ-RT-003": 413, "RZ-RT-004": 503, "RZ-UP-010": 502, "RZ-RT-015": 502}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v/%d", tt.err, tt.side), func(t *testing.T) {
			if got := Code(tt.err, tt.side); got != tt.want {
				t.Fatalf("Code = %q, want %q", got, tt.want)
			}
			w := Wrap(tt.err, tt.side)
			code, ok := errcode.CodeOf(w)
			if tt.want == "" {
				if ok || (tt.err != nil && !errors.Is(w, tt.err)) {
					t.Fatalf("Wrap = %v", w)
				}
				return
			}
			if !ok || code != tt.want || !errors.Is(w, tt.err) {
				t.Fatalf("Wrap = %v (code %q)", w, code)
			}
			if s := errcode.Status(code); s != statuses[code] {
				t.Fatalf("status %d, want %d", s, statuses[code])
			}
		})
	}
}

func TestReq47DecodedCap(t *testing.T) {
	tests := []struct{ raw, want int64 }{
		{0, 0},
		{-1, 0},
		{1, 4},
		{10 << 20, 40 << 20},
		{maxInt64 / 4, maxInt64 / 4 * 4},
		{maxInt64/4 + 1, maxInt64},
		{maxInt64, maxInt64},
	}
	for _, tt := range tests {
		if got := DecodedCap(tt.raw); got != tt.want {
			t.Fatalf("DecodedCap(%d) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestReq34CheckDeclared(t *testing.T) {
	tests := []struct {
		declared, limit int64
		err             error
	}{
		{-1, 10, nil},
		{0, 10, nil},
		{10, 10, nil},
		{11, 10, ErrTooLarge},
		{1, 0, ErrTooLarge},
		{0, -1, nil},
		{1, -1, ErrTooLarge},
	}
	for _, tt := range tests {
		if err := CheckDeclared(tt.declared, tt.limit); !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
			t.Fatalf("CheckDeclared(%d, %d) = %v", tt.declared, tt.limit, err)
		}
	}
}

// chunkReader returns data in chunks whose sizes cycle through sizes; a
// size of 0 is one (0, nil) read, never two in a row.
type chunkReader struct {
	data  []byte
	sizes []byte
	i     int
	zero  bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := 1
	if len(c.sizes) > 0 {
		n = int(c.sizes[c.i%len(c.sizes)])
		c.i++
	}
	if n == 0 && !c.zero {
		c.zero = true
		return 0, nil
	}
	c.zero = false
	n = min(max(n, 1), len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// FuzzReadGate reads arbitrary bodies in arbitrary chunks against a limit
// and a small budget: the gate returns the body exactly, or ErrTooLarge
// past the limit, or ErrBudget; the budget returns to zero every time.
func FuzzReadGate(f *testing.F) {
	f.Add(pattern(100), uint32(64), []byte{1, 7, 200}, int64(-1), uint8(4))
	f.Add(pattern(5000), uint32(5000), []byte{0, 255}, int64(5000), uint8(1))
	f.Add([]byte{}, uint32(0), []byte{}, int64(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, limit uint32, sizes []byte, declared int64, budInc uint8) {
		bud := NewBudget(int64(budInc)*Increment, nil)
		r := &chunkReader{data: data, sizes: sizes}
		lim := int64(limit % (1 << 20))
		declared = max(declared, -1)
		g, err := ReadGate(context.Background(), r, declared, lim, bud, 0)
		switch {
		case err == nil:
			if int64(cap(g.b)) > g.Reserved() || bud.GateUsed() != g.Reserved() {
				t.Fatalf("capacity %d, reserved %d, budget %d", cap(g.b), g.Reserved(), bud.GateUsed())
			}
			if declared == 0 {
				if g.Len() != 0 {
					t.Fatal("a declared-empty body was read")
				}
			} else if !bytes.Equal(g.Bytes(), data) {
				t.Fatalf("gate returned %d bytes of %d", g.Len(), len(data))
			}
			if int64(g.Len()) > lim {
				t.Fatal("the gate passed its limit")
			}
			g.Release()
		case errors.Is(err, ErrTooLarge):
			if int64(len(data)) <= lim && declared <= lim {
				t.Fatalf("ErrTooLarge for %d bytes under limit %d", len(data), lim)
			}
		case errors.Is(err, ErrBudget):
			if int64(len(data)) <= bud.Effective()-bud.StreamShare() {
				t.Fatalf("ErrBudget for %d bytes with %d of gate budget", len(data), bud.Effective()-bud.StreamShare())
			}
		default:
			t.Fatalf("ReadGate: %v", err)
		}
		if bud.Used() != 0 {
			t.Fatalf("Used = %d at the end", bud.Used())
		}
	})
}

// FuzzLimited cuts arbitrary bodies read in arbitrary chunks.
func FuzzLimited(f *testing.F) {
	f.Add(pattern(30), uint16(20), []byte{3})
	f.Add(pattern(20), uint16(20), []byte{0, 1})
	f.Fuzz(func(t *testing.T, data []byte, limit uint16, sizes []byte) {
		l := LimitReader(&chunkReader{data: data, sizes: sizes}, int64(limit))
		got, err := io.ReadAll(l)
		if len(data) > int(limit) {
			if !errors.Is(err, ErrTooLarge) || len(got) > int(limit) {
				t.Fatalf("over the limit: %d bytes, %v", len(got), err)
			}
		} else if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("under the limit: %d bytes, %v", len(got), err)
		}
		if l.Count() != int64(len(got)) {
			t.Fatalf("Count %d, read %d", l.Count(), len(got))
		}
	})
}

// BenchmarkReadGate1KiB gates a 1 KiB body (the validation.json-schema
// budget of spec 07 req 84 starts from a gated body).
func BenchmarkReadGate1KiB(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, &sumGauge{})
	data := pattern(kib)
	r := bytes.NewReader(data)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		g, err := ReadGate(ctx, r, int64(len(data)), 10<<20, bud, 0)
		if err != nil {
			b.Fatal(err)
		}
		g.Release()
	}
}

// BenchmarkReadGate1MiB gates a 1 MiB chunked body.
func BenchmarkReadGate1MiB(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, &sumGauge{})
	data := pattern(1 << 20)
	r := bytes.NewReader(data)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		g, err := ReadGate(ctx, r, -1, 10<<20, bud, 0)
		if err != nil {
			b.Fatal(err)
		}
		g.Release()
	}
}

// BenchmarkLimited streams 1 MiB through the limited reader.
func BenchmarkLimited(b *testing.B) {
	data := pattern(1 << 20)
	r := bytes.NewReader(data)
	var l Limited
	buf := make([]byte, Increment)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		l.Reset(r, 10<<20)
		for {
			if _, err := l.Read(buf); err != nil {
				break
			}
		}
	}
}
