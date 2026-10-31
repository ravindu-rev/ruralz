// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Distribution draws the delay a Behavior waits before answering.
// Implementations clamp at 0 and must be safe to call with the Server's
// generator (Sample runs under the Server's generator lock).
type Distribution interface {
	Sample(r *rand.Rand) time.Duration
}

// Fixed always waits the same duration.
type Fixed time.Duration

// Sample returns the fixed duration.
func (f Fixed) Sample(*rand.Rand) time.Duration { return max(time.Duration(f), 0) }

// String names the distribution.
func (f Fixed) String() string { return "fixed(" + time.Duration(f).String() + ")" }

// Uniform draws uniformly from [Min, Max].
type Uniform struct{ Min, Max time.Duration }

// Sample draws a duration in [Min, Max].
func (u Uniform) Sample(r *rand.Rand) time.Duration {
	if u.Max <= u.Min {
		return max(u.Min, 0)
	}
	span := int64(u.Max - u.Min)
	if span < math.MaxInt64 {
		span++
	}
	return max(u.Min+time.Duration(r.Int64N(span)), 0)
}

// String names the distribution.
func (u Uniform) String() string { return fmt.Sprintf("uniform(%v..%v)", u.Min, u.Max) }

// Normal draws from a normal distribution with mean Mean and standard
// deviation StdDev, clamped at 0 (PBB S5x: 500 µs ± 100 µs).
type Normal struct{ Mean, StdDev time.Duration }

// Sample draws a normally distributed duration, clamped at 0.
func (n Normal) Sample(r *rand.Rand) time.Duration {
	v := float64(n.Mean) + r.NormFloat64()*float64(n.StdDev)
	if v <= 0 {
		return 0
	}
	if v >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(v)
}

// String names the distribution.
func (n Normal) String() string { return fmt.Sprintf("normal(%v±%v)", n.Mean, n.StdDev) }

// Exponential draws from an exponential distribution with mean Mean.
type Exponential struct{ Mean time.Duration }

// Sample draws an exponentially distributed duration.
func (e Exponential) Sample(r *rand.Rand) time.Duration {
	v := r.ExpFloat64() * float64(e.Mean)
	if v <= 0 {
		return 0
	}
	if v >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(v)
}

// String names the distribution.
func (e Exponential) String() string { return fmt.Sprintf("exponential(%v)", e.Mean) }

func validDistribution(d Distribution) error {
	switch d := d.(type) {
	case nil:
		return nil
	case Fixed:
		if d < 0 {
			return fmt.Errorf("negative delay %v", d)
		}
	case Uniform:
		if d.Min < 0 || d.Max < d.Min {
			return fmt.Errorf("invalid delay %v", d)
		}
	case Normal:
		if d.Mean < 0 || d.StdDev < 0 {
			return fmt.Errorf("negative delay %v", d)
		}
	case Exponential:
		if d.Mean < 0 {
			return fmt.Errorf("negative delay %v", d)
		}
	}
	return nil
}

// ResetKind is how a Behavior breaks the response.
type ResetKind int

// Reset kinds.
const (
	// NoReset answers normally.
	NoReset ResetKind = iota
	// ResetConn closes the connection carrying the request with a TCP
	// RST (SO_LINGER 0); on HTTP/2 every stream of that connection ends.
	ResetConn
	// ResetStream aborts this response only: RST_STREAM on HTTP/2; on
	// HTTP/1.1 the connection is closed.
	ResetStream
)

// String names the reset kind as the X-Mockup-Reset header spells it.
func (k ResetKind) String() string {
	switch k {
	case NoReset:
		return "none"
	case ResetConn:
		return "conn"
	case ResetStream:
		return "stream"
	}
	return fmt.Sprintf("ResetKind(%d)", int(k))
}

// Behavior programs one answer of the mock Upstream. The zero value
// answers 200 with an empty body.
type Behavior struct {
	// Status is the response status, 200 to 599; 0 means 200.
	Status int
	// Header is added to the response (after ContentType, so it can
	// replace it).
	Header http.Header
	// Body is the literal response body. When nil, the body is BodySize
	// bytes of GeneratedBody.
	Body []byte
	// BodySize is the generated body's length when Body is nil.
	BodySize int64
	// ContentType is the Content-Type of a body; default
	// "application/octet-stream" ("application/json" for Echo).
	ContentType string
	// Chunked omits Content-Length, so HTTP/1.1 uses chunked transfer
	// coding.
	Chunked bool
	// Delay is waited before the response headers; nil waits nothing.
	Delay Distribution
	// ChunkInterval, when positive, makes a slow body: ChunkSize bytes
	// are written and flushed, then the next after ChunkInterval.
	ChunkInterval time.Duration
	// ChunkSize is the slow body's chunk; default DefaultChunkSize.
	ChunkSize int
	// Reset breaks the response instead of completing it.
	Reset ResetKind
	// ResetAfterHeaders writes and flushes the status line and headers,
	// then ResetAfterBytes body bytes, before the reset; otherwise the
	// reset comes before anything is written.
	ResetAfterHeaders bool
	// ResetAfterBytes is the body bytes written before a reset after
	// headers.
	ResetAfterBytes int64
	// Echo answers with an Echo JSON document describing the request
	// (method, path, headers, body, ...), replacing Body and BodySize.
	Echo bool
}

// validate checks b; errors carry no package prefix (callers add it).
func (b Behavior) validate() error {
	if b.Status != 0 && (b.Status < 200 || b.Status > 599) {
		return fmt.Errorf("status %d outside 200..599", b.Status)
	}
	if b.BodySize < 0 || b.ResetAfterBytes < 0 || b.ChunkSize < 0 || b.ChunkInterval < 0 {
		return errors.New("negative body size, chunk or reset offset")
	}
	switch b.Reset {
	case NoReset, ResetConn, ResetStream:
	default:
		return fmt.Errorf("unknown reset kind %v", b.Reset)
	}
	return validDistribution(b.Delay)
}

// clone copies the mutable parts so later caller edits do not reach the
// server.
func (b Behavior) clone() Behavior {
	b.Header = b.Header.Clone()
	b.Body = slices.Clone(b.Body)
	return b
}

// Header names of the per-request overrides honored when Config.Overrides
// is set, and of the response header naming the mock.
const (
	// HeaderStatus sets Status ("503").
	HeaderStatus = "X-Mockup-Status"
	// HeaderDelay sets a Fixed Delay (a Go duration, "250ms").
	HeaderDelay = "X-Mockup-Delay"
	// HeaderBodySize sets BodySize and drops a literal Body ("1024").
	HeaderBodySize = "X-Mockup-Body-Size"
	// HeaderEcho sets Echo ("true").
	HeaderEcho = "X-Mockup-Echo"
	// HeaderReset sets Reset ("conn", "stream" or "none").
	HeaderReset = "X-Mockup-Reset"
	// HeaderResetAfter sets ResetAfterHeaders and ResetAfterBytes ("0").
	HeaderResetAfter = "X-Mockup-Reset-After"
	// HeaderChunkSize sets ChunkSize ("512").
	HeaderChunkSize = "X-Mockup-Chunk-Size"
	// HeaderChunkInterval sets ChunkInterval (a Go duration, "100ms").
	HeaderChunkInterval = "X-Mockup-Chunk-Interval"
	// HeaderServer is set on every response of a named Server to its
	// Config.Name, so balancing tests see which mock answered.
	HeaderServer = "X-Mockup-Server"
)

// applyOverrides returns b changed by the request's override headers.
func applyOverrides(b Behavior, h http.Header) (Behavior, error) {
	bad := func(name, v string, err error) (Behavior, error) {
		return b, fmt.Errorf("invalid %s %q: %w", name, v, err)
	}
	if v := h.Get(HeaderStatus); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return bad(HeaderStatus, v, err)
		}
		b.Status = n
	}
	if v := h.Get(HeaderDelay); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return bad(HeaderDelay, v, err)
		}
		b.Delay = Fixed(d)
	}
	if v := h.Get(HeaderBodySize); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return bad(HeaderBodySize, v, err)
		}
		b.Body, b.BodySize = nil, n
	}
	if v := h.Get(HeaderEcho); v != "" {
		e, err := strconv.ParseBool(v)
		if err != nil {
			return bad(HeaderEcho, v, err)
		}
		b.Echo = e
	}
	if v := h.Get(HeaderReset); v != "" {
		switch strings.ToLower(v) {
		case "none":
			b.Reset = NoReset
		case "conn":
			b.Reset = ResetConn
		case "stream":
			b.Reset = ResetStream
		default:
			return bad(HeaderReset, v, errors.New(`want "conn", "stream" or "none"`))
		}
	}
	if v := h.Get(HeaderResetAfter); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return bad(HeaderResetAfter, v, err)
		}
		b.ResetAfterHeaders, b.ResetAfterBytes = true, n
	}
	if v := h.Get(HeaderChunkSize); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return bad(HeaderChunkSize, v, err)
		}
		b.ChunkSize = n
	}
	if v := h.Get(HeaderChunkInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return bad(HeaderChunkInterval, v, err)
		}
		b.ChunkInterval = d
	}
	if err := b.validate(); err != nil {
		return b, fmt.Errorf("invalid X-Mockup-* override: %w", err)
	}
	return b, nil
}
