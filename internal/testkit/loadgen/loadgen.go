// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package loadgen is the open-loop load generator of the test kit. It
// offers a fixed arrival rate to a Target: request i is due at
// start + i/Rate on an injected clock whatever the target answers, so a
// stalled system shows up as failed or Missed requests instead of as a
// lower rate. The Node integration suites drive Nodes with it
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests").
//
// A free worker sends a due request at once; when MaxInFlight requests are
// in flight it waits in a bounded backlog, and when the backlog is full it
// is Missed: counted, never sent. Each request ends with an Outcome (an
// HTTP status class, a transport failure, Timeout, Missed or Canceled),
// and the RZ code of a problem document is counted.
//
// The HTTP target sends GET requests over a bounded pool of HTTP/1.1
// keep-alive connections.
//
// Goroutines: Start runs one scheduler and at most MaxInFlight workers,
// all owned by the Generator; Wait returns once every one has ended.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Defaults.
const (
	// DefaultMaxInFlight bounds concurrent requests (target).
	DefaultMaxInFlight = 4096
	// DefaultMaxBacklog bounds requests waiting for a free worker
	// (target); with DefaultMaxInFlight it absorbs a stall of about
	// 70 s at 1,000 requests per second (hypothesis).
	DefaultMaxBacklog = 65536
	// DefaultTimeout bounds each request from its send (target).
	DefaultTimeout = 30 * time.Second
)

// ErrTimeout is the cancel cause of a request's context when Timeout
// expires; targets that classify their own errors check
// context.Cause(ctx) for it.
var ErrTimeout = errors.New("loadgen: request timed out")

// Outcome classifies how a request ended.
type Outcome uint8

// Outcomes. A Response whose Outcome is Success (the zero value) but
// carries an Err or a Status is classified from them.
const (
	// Success is a response with status 200 to 399.
	Success Outcome = iota
	// ClientError is a response with status 400 to 499.
	ClientError
	// ServerError is a response with status 500 to 599.
	ServerError
	// Refused is a connection refused while dialing.
	Refused
	// Reset is a connection reset or aborted by the peer.
	Reset
	// Closed is a connection closed before a complete response.
	Closed
	// Timeout is a request that reached Config.Timeout.
	Timeout
	// TLSError is a failed TLS handshake or certificate check.
	TLSError
	// Other is any other failure.
	Other
	// Missed is a request never sent: no worker and no backlog room.
	Missed
	// Canceled is a request ended by the run's context.
	Canceled
	// NumOutcomes is the number of outcomes.
	NumOutcomes
)

// String names the outcome.
func (o Outcome) String() string {
	switch o {
	case Success:
		return "success"
	case ClientError:
		return "client_error"
	case ServerError:
		return "server_error"
	case Refused:
		return "refused"
	case Reset:
		return "reset"
	case Closed:
		return "closed"
	case Timeout:
		return "timeout"
	case TLSError:
		return "tls_error"
	case Other:
		return "other"
	case Missed:
		return "missed"
	case Canceled:
		return "canceled"
	case NumOutcomes:
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Response reports whether the request got a response (ClientError and
// ServerError included) rather than a transport failure.
func (o Outcome) Response() bool { return o <= ServerError }

// OutcomeForStatus classifies an HTTP status.
func OutcomeForStatus(status int) Outcome {
	switch {
	case status >= 500:
		return ServerError
	case status >= 400:
		return ClientError
	}
	return Success
}

// Target sends one request. Do runs on a worker goroutine, concurrently
// with other calls; ctx ends at the request's Timeout (cause ErrTimeout)
// or when the run's context ends.
type Target interface {
	Do(ctx context.Context) Response
}

// Response is what a Target reports for one request.
type Response struct {
	// Outcome classifies the request; see the Outcome constants.
	Outcome Outcome
	// Status is the HTTP status; 0 when no response arrived.
	Status int
	// Code is the RZ code of a problem document response; "" otherwise.
	Code string
	// Err is the failure, if any.
	Err error
}

// Config configures a run.
type Config struct {
	// Rate is the fixed arrival rate in requests per second.
	Rate float64
	// Duration is the run's length; without it the run lasts until the
	// end of its context.
	Duration time.Duration
	// MaxInFlight bounds concurrent requests; 0 means DefaultMaxInFlight.
	MaxInFlight int
	// MaxBacklog bounds requests waiting for a free worker; beyond it a
	// request is Missed. 0 means DefaultMaxBacklog; negative means none, so
	// a request that finds every worker busy is Missed at once.
	MaxBacklog int
	// Timeout bounds each request from its send; 0 means DefaultTimeout.
	Timeout time.Duration
	// Clock schedules and times requests; nil means clock.Real().
	Clock clock.Clock
}

func (c *Config) setDefaults() error {
	switch {
	case !(c.Rate > 0) || math.IsInf(c.Rate, 0):
		return fmt.Errorf("loadgen: rate %v is not a positive number", c.Rate)
	case c.Duration < 0, c.Timeout < 0, c.MaxInFlight < 0:
		return errors.New("loadgen: negative duration, timeout or in-flight bound")
	}
	if c.MaxInFlight == 0 {
		c.MaxInFlight = DefaultMaxInFlight
	}
	switch {
	case c.MaxBacklog == 0:
		c.MaxBacklog = DefaultMaxBacklog
	case c.MaxBacklog < 0:
		c.MaxBacklog = 0
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	return nil
}

// Generator is a running load generator.
type Generator struct {
	cfg    Config
	target Target
	clk    clock.Clock
	start  time.Time

	done  chan struct{} // closed when the result is ready
	stats Stats
	err   error

	mu       sync.Mutex // guards inFlight and backlog
	inFlight int
	backlog  int // requests waiting for a free worker, at most cfg.MaxBacklog
	workers  sync.WaitGroup

	rec recorder
}

// Run starts a run and waits for its result; see Start.
func Run(ctx context.Context, t Target, c Config) (Stats, error) {
	g, err := Start(ctx, t, c)
	if err != nil {
		return Stats{}, err
	}
	return g.Wait()
}

// Start begins offering load to t. The run ends when its last request is
// due, or when ctx ends, which also cancels requests in flight; Wait then
// returns the result.
func Start(ctx context.Context, t Target, c Config) (*Generator, error) {
	if t == nil {
		return nil, errors.New("loadgen: nil target")
	}
	if err := c.setDefaults(); err != nil {
		return nil, err
	}
	g := &Generator{
		cfg:    c,
		target: t,
		clk:    c.Clock,
		done:   make(chan struct{}),
	}
	g.start = g.clk.Now()
	go g.run(ctx)
	return g, nil
}

// Wait waits for the run to end and returns its result, with the context's
// error when the context ended the run.
func (g *Generator) Wait() (Stats, error) {
	<-g.done
	return g.stats, g.err
}

// offset returns request seq's intended offset from the start.
func (g *Generator) offset(seq uint64) time.Duration {
	return time.Duration(float64(seq) * 1e9 / g.cfg.Rate)
}

// run owns the run: the schedule, then the wait for every worker. ctx is
// the run's context, which requests in flight derive from.
func (g *Generator) run(ctx context.Context) {
	defer close(g.done)
	err := g.schedule(ctx)
	g.workers.Wait()
	g.stats = g.rec.snapshot()
	g.err = err
}

// schedule dispatches requests at their intended times until the run's
// end, returning the context's error when the context ended it.
func (g *Generator) schedule(ctx context.Context) error {
	timer := g.clk.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for seq := uint64(0); ; {
		if err := ctx.Err(); err != nil {
			// The context ends the run at once; nothing more is sent.
			return err
		}
		off := g.offset(seq)
		if g.cfg.Duration > 0 && off >= g.cfg.Duration {
			return nil
		}
		if now := g.clk.Since(g.start); now < off {
			g.waitDue(ctx, timer, off-now)
			continue
		}
		g.dispatch(ctx)
		seq++
	}
}

// waitDue waits for d or the context's end.
func (g *Generator) waitDue(ctx context.Context, timer clock.Timer, d time.Duration) {
	timer.Reset(d)
	select {
	case <-timer.C():
		return
	case <-ctx.Done():
	}
	timer.Stop()
}

// dispatch hands a due request to a free worker, queues it, or counts it
// Missed.
func (g *Generator) dispatch(ctx context.Context) {
	g.rec.dispatched()
	g.mu.Lock()
	switch {
	case g.inFlight < g.cfg.MaxInFlight:
		g.inFlight++
		g.mu.Unlock()
		g.workers.Add(1)
		go g.worker(ctx)
	case g.backlog < g.cfg.MaxBacklog:
		g.backlog++
		g.mu.Unlock()
	default:
		g.mu.Unlock()
		g.rec.add(Response{Outcome: Missed})
	}
}

// worker sends a request, then backlogged requests, until the backlog is
// empty.
func (g *Generator) worker(ctx context.Context) {
	defer g.workers.Done()
	for {
		g.send(ctx)
		g.mu.Lock()
		if g.backlog == 0 {
			g.inFlight--
			g.mu.Unlock()
			return
		}
		g.backlog--
		g.mu.Unlock()
	}
}

// send runs one request against the target, within Timeout of the run's
// context ctx.
func (g *Generator) send(ctx context.Context) {
	rctx, cancel := context.WithCancelCause(ctx)
	t := g.clk.AfterFunc(g.cfg.Timeout, func() { cancel(ErrTimeout) })
	resp := g.target.Do(rctx)
	t.Stop()
	timedOut := errors.Is(context.Cause(rctx), ErrTimeout)
	cancel(nil)
	resp.Outcome = normalize(resp, timedOut, ctx.Err() != nil)
	g.rec.add(resp)
}

// normalize derives a zero Outcome from Err or Status, then turns a
// transport failure caused by the request's timeout or the run's end into
// Timeout or Canceled.
func normalize(r Response, timedOut, runEnded bool) Outcome {
	o := r.Outcome
	if o == Success {
		switch {
		case r.Err != nil:
			o = Classify(r.Err)
		case r.Status != 0:
			o = OutcomeForStatus(r.Status)
		}
	}
	if o.Response() || o == Missed {
		return o
	}
	switch {
	case timedOut:
		return Timeout
	case runEnded:
		return Canceled
	}
	return o
}
