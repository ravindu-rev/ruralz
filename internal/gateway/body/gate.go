// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Buffer is a gated body: the whole body, read before anything was
// forwarded. Its backing array is reserved from the budget (Gate kind) in
// whole increments before it is allocated, so the reservation always
// covers the memory the gate holds (cap(Bytes()) <= Reserved()). The
// reservation lasts until Release.
type Buffer struct {
	bud    *Budget
	b      []byte
	held   int64 // increments reserved; cap(b) == held*Increment
	stripe emit.Stripe
	probe  [1]byte
}

// Bytes returns the body; nil or empty for an empty body. The slice is
// valid until Release.
func (g *Buffer) Bytes() []byte { return g.b }

// Len returns the body length.
func (g *Buffer) Len() int { return len(g.b) }

// Reserved returns the bytes reserved from the budget (whole increments);
// it is the capacity of the backing array.
func (g *Buffer) Reserved() int64 { return g.held * Increment }

// Release returns the reservation and recycles the backing array (one of
// one or two increments, at most PoolMax, goes back to the budget's pool,
// where the next gate on the Node writes another body into it). No slice
// obtained from Bytes may be used after Release. The Buffer is inert
// afterwards: Bytes is empty and a second Release does nothing.
func (g *Buffer) Release() {
	bud := g.bud
	if bud == nil {
		g.b, g.held = nil, 0
		return
	}
	bud.release(Gate, g.held, g.stripe)
	bud.recycle(g.b)
	g.bud, g.b, g.held = nil, nil, 0
}

// ReadGate reads the whole body from r within limit bytes (spec 04 reqs 46
// and 47): declared is the declared length (-1 when unknown). A declared
// length over limit is ErrTooLarge before r is read, so net/http sends no
// 100 Continue; a declared length of 0 or a nil or http.NoBody reader is an
// empty body. The buffer grows as bytes arrive, geometrically and never
// past limit or, while the body is within it, its declared length; each
// growth first reserves the increments covering the new capacity from bud
// (halving the step near the ceiling), and ErrBudget (503 RZ-RT-004) ends
// the gate when not even one increment is left, or when bud is nil and the
// body is not empty. A body passing limit is ErrTooLarge, detected by
// reading at most one byte past it. ctx is checked between reads; the read
// deadline of the client connection bounds a stalled client. The gauge
// records the reservation on the request's stripe s. On any error the
// reservation is returned and no Buffer is returned.
func ReadGate(ctx context.Context, r io.Reader, declared, limit int64, bud *Budget, s emit.Stripe) (*Buffer, error) {
	g := &Buffer{}
	if err := g.fill(ctx, r, declared, limit, bud, s); err != nil {
		return nil, err
	}
	return g, nil
}

// fill is ReadGate into g, which must be empty (new or released); on error
// g is released.
func (g *Buffer) fill(ctx context.Context, r io.Reader, declared, limit int64, bud *Budget, s emit.Stripe) error {
	if err := CheckDeclared(declared, limit); err != nil {
		return err
	}
	g.bud, g.b, g.held, g.stripe = bud, nil, 0, s
	if declared == 0 || r == nil || r == http.NoBody {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			g.Release()
			return err
		}
		n := int64(len(g.b))
		switch {
		case n >= limit:
			done, err := g.probeEnd(r)
			if err != nil {
				g.Release()
				return err
			}
			if done {
				return nil
			}
			continue
		case n == int64(cap(g.b)):
			// The buffer is full. Unless nothing has arrived yet, read one
			// byte before growing, so a body ending exactly at the
			// capacity needs no more budget.
			done, err := g.more(r, n > 0, declared, limit)
			if err != nil {
				g.Release()
				return err
			}
			if done {
				g.dropEmpty()
				return nil
			}
			continue
		}
		m, err := r.Read(g.b[n:min(int64(cap(g.b)), limit)])
		g.b = g.b[:n+int64(m)]
		if errors.Is(err, io.EOF) {
			g.dropEmpty()
			return nil
		}
		if err != nil {
			g.Release()
			return err
		}
	}
}

// more makes room in a full buffer. With probe it first reads one byte:
// io.EOF ends the body (done), a byte grows the buffer and is kept, and
// (0, nil) asks again. Without probe it grows at once. A growth the budget
// refuses is ErrBudget, except that a body found to end there needs none.
func (g *Buffer) more(r io.Reader, probe bool, declared, limit int64) (done bool, err error) {
	if !probe {
		if g.grow(declared, limit) {
			return false, nil
		}
		// Not even one increment: an empty body still passes.
		done, err = g.probeEnd(r)
		switch {
		case done:
			return true, nil
		case err == nil:
			return false, nil // (0, nil): ask again
		case errors.Is(err, ErrTooLarge):
			return false, ErrBudget
		default:
			return false, err
		}
	}
	m, err := r.Read(g.probe[:])
	if m > 0 {
		if !g.grow(declared, limit) {
			return false, ErrBudget
		}
		g.b = append(g.b, g.probe[0])
	}
	switch {
	case errors.Is(err, io.EOF):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// probeEnd reads one byte past the limit: io.EOF ends the body (done), a
// byte is ErrTooLarge, (0, nil) asks again.
func (g *Buffer) probeEnd(r io.Reader) (done bool, err error) {
	m, err := r.Read(g.probe[:])
	switch {
	case m > 0:
		return false, ErrTooLarge
	case errors.Is(err, io.EOF):
		return true, nil
	default:
		return false, err
	}
}

// dropEmpty returns the array and reservation of a body that turned out
// empty, so an empty gate holds nothing.
func (g *Buffer) dropEmpty() {
	if len(g.b) == 0 && g.held > 0 {
		g.bud.release(Gate, g.held, g.stripe)
		g.bud.recycle(g.b)
		g.b, g.held = nil, 0
	}
}

// grow raises the capacity geometrically (doubling, at least one
// increment), never past what limit needs and, while the body is within
// its declared length, never past that length, so a client declaring a
// large body but sending little holds little memory. It reserves the
// increments covering the new capacity before allocating it; when the
// budget refuses them it asks for fewer (halving, at least one) and
// allocates only what it reserved, so the capacity never exceeds the
// reservation. It reports false, changing nothing, when not even one
// increment can be reserved or there is no budget.
func (g *Buffer) grow(declared, limit int64) bool {
	if g.bud == nil {
		return false
	}
	c := int64(cap(g.b))
	want := max(2*c, Increment)
	if declared > c {
		want = min(want, declared)
	}
	want = min(want, limit)
	got := g.bud.reserveUpTo(Gate, 1, increments(want)-g.held, g.stripe)
	if got == 0 {
		return false
	}
	g.held += got
	nb := append(g.bud.array(g.held*Increment), g.b...)
	g.bud.recycle(g.b)
	g.b = nb
	return true
}
