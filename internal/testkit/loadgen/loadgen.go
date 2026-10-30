// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package loadgen is the open-loop load generator of the test kit. It
// offers a fixed arrival rate to a Target and measures every latency from
// the request's intended send time, never from when a worker got to it,
// so a stalled system shows up as latency instead of as fewer samples
// (coordinated omission, PBB "Open-loop load and coordinated omission").
// The chaos experiments, end-to-end scenarios and in-process benchmark
// runs drive Nodes and the State client with it (Testing "Chaos testing"
// and "End-to-end tests"); the Latency job's macro runs use oha and
// vegeta instead.
//
// A run schedules request i at start + i/Rate on an injected clock. A free
// worker sends a due request at once; when MaxInFlight requests are in
// flight it waits in a bounded backlog, and when the backlog is full it is
// Missed: counted, never sent, and the run's achieved rate falls short.
// Each request ends with an Outcome (an HTTP status class, a transport
// failure, Timeout, Missed or Canceled), and the RZ code of a problem
// document is counted. Results split into a discarded warm-up and the
// measured phase, plus a time series of Windows by intended send time.
// With Settle, the warm-up extends in steps until the p99 of consecutive
// windows settles (PBB "Warm-up, duration and percentiles"). Valid applies
// PBB's rate rule, and RunLadder and Saturation its rate ladder.
//
// HTTP targets speak HTTP/1.1, h2c or HTTP/2 over TLS, rotate keys and
// bearer tokens, and report the connection each request used, so a test
// can attribute a failure to the Node behind the test kit's L4 balancer.
// With SourceAddrs they send from distinct loopback source addresses
// (127.0.0.x on Linux, LoopbackAddrs), request by request, when a scenario
// needs per-client source.ip behind a balancer speaking PROXY protocol v2.
//
// Goroutines: Start runs one scheduler and at most MaxInFlight workers,
// all owned by the Generator; Wait returns once every one has ended.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
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
	// DefaultTimeout bounds each request from its send (target): above
	// the 25 s Drain deadline (target), so a Drain's RZ-RT-016 reaches the
	// generator before its own timeout.
	DefaultTimeout = 30 * time.Second
	// DefaultWindow is the resolution of Result.Windows (target).
	DefaultWindow = time.Second
	// DefaultMaxFailures bounds the failed Samples kept in
	// Result.Failures (target).
	DefaultMaxFailures = 10000
	// DefaultMaxRateError is PBB's validity rule: the achieved rate stays
	// within 0.5% of the offered rate (target).
	DefaultMaxRateError = 0.005
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
	// Reset is a connection reset or aborted by the peer, or an HTTP/2
	// stream reset.
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

// Request is one scheduled request.
type Request struct {
	// Seq numbers requests from 0 in schedule order, warm-up included.
	Seq uint64
	// Intended is the request's intended send time on the run's clock.
	Intended time.Time
}

// Target sends one request. Do runs on a worker goroutine, concurrently
// with other calls; ctx ends at the request's Timeout (cause ErrTimeout)
// or when the run's context ends.
type Target interface {
	Do(ctx context.Context, r Request) Response
}

// TargetFunc adapts a function to Target.
type TargetFunc func(ctx context.Context, r Request) Response

// Do calls f.
func (f TargetFunc) Do(ctx context.Context, r Request) Response { return f(ctx, r) }

// ConnInfo identifies the connection a request used.
type ConnInfo struct {
	// Local is the client's address: the address an L4 balancer in front
	// of the target saw, which attributes the request to its backend.
	Local string
	// Remote is the address the client connected to.
	Remote string
	// Reused reports a connection that carried an earlier request.
	Reused bool
}

// Response is what a Target reports for one request.
type Response struct {
	// Outcome classifies the request; see the Outcome constants.
	Outcome Outcome
	// Status is the HTTP status; 0 when no response arrived.
	Status int
	// Code is the RZ code of a problem document response; "" otherwise.
	Code string
	// Proto is the response's protocol, such as "HTTP/2.0".
	Proto string
	// Key is the key the request carried, if any.
	Key string
	// Conn is the connection used; zero for targets without one.
	Conn ConnInfo
	// Header is the response header; nil when no response arrived.
	Header http.Header
	// Bytes counts response body bytes read.
	Bytes int64
	// Err is the failure, if any.
	Err error
}

// Sample is one finished request.
type Sample struct {
	Request
	// Sent is when a worker started the request; zero when Missed.
	Sent time.Time
	// Done is when the request ended.
	Done time.Time
	Response
}

// Latency returns the time from the intended send time to the end: the
// latency the generator records.
func (s Sample) Latency() time.Duration { return s.Done.Sub(s.Intended) }

// Service returns the time from the actual send to the end; 0 when the
// request was never sent.
func (s Sample) Service() time.Duration {
	if s.Sent.IsZero() {
		return 0
	}
	return s.Done.Sub(s.Sent)
}

// Settle extends the warm-up until latency settles (PBB "Warm-up, duration
// and percentiles"): at Config.Warmup and then every Step until Max, the
// warm-up ends when the p99 of the last Windows windows of Window length
// spread by at most MaxSpread (Settled); a warm-up still unsettled at Max
// ends there and the run is invalid.
type Settle struct {
	// Step extends the warm-up; PBB: 30 s (target).
	Step time.Duration
	// Max is the longest warm-up; PBB: 180 s (target).
	Max time.Duration
	// Window is the length of a compared window, a multiple of
	// Config.Window; PBB: 10 s (target).
	Window time.Duration
	// Windows is the number of consecutive windows compared; PBB: 3.
	Windows int
	// MaxSpread bounds (max − min) / min of their p99 values; PBB: 5%
	// (target).
	MaxSpread float64
}

// BenchmarkSettle returns PBB's warm-up rule; use it with a 60 s (target)
// Config.Warmup.
func BenchmarkSettle() *Settle {
	return &Settle{Step: 30 * time.Second, Max: 180 * time.Second, Window: 10 * time.Second, Windows: 3, MaxSpread: 0.05}
}

// BenchmarkWarmup is PBB's initial warm-up: 60 s (target).
const BenchmarkWarmup = 60 * time.Second

// Settled reports whether p99 values spread by at most maxSpread, measured
// as (max − min) / min (target): every value lies within maxSpread of
// the smallest. Values that are all zero are settled.
func Settled(p99s []time.Duration, maxSpread float64) bool {
	if len(p99s) == 0 {
		return false
	}
	lo, hi := slices.Min(p99s), slices.Max(p99s)
	if hi == lo {
		return true
	}
	if lo <= 0 {
		return false
	}
	return float64(hi-lo)/float64(lo) <= maxSpread
}

// Config configures a run.
type Config struct {
	// Rate is the fixed arrival rate in requests per second.
	Rate float64
	// Duration is the measured phase's length; Requests its number of
	// requests. Set at most one; with neither the run lasts until Stop or
	// the end of its context.
	Duration time.Duration
	Requests uint64
	// Warmup is offered before the measured phase and discarded from
	// Result.Measured.
	Warmup time.Duration
	// Settle, when set, extends Warmup until latency settles.
	Settle *Settle
	// MaxInFlight bounds concurrent requests; 0 means DefaultMaxInFlight.
	MaxInFlight int
	// MaxBacklog bounds requests waiting for a free worker; beyond it a
	// request is Missed. 0 means DefaultMaxBacklog; negative means none, so
	// a request that finds every worker busy is Missed at once.
	MaxBacklog int
	// Timeout bounds each request from its send; 0 means DefaultTimeout.
	Timeout time.Duration
	// Window is the resolution of Result.Windows; 0 means DefaultWindow.
	Window time.Duration
	// Clock schedules and times requests; nil means clock.Real().
	Clock clock.Clock
	// OnResult, when set, sees every request once it ends, Missed ones
	// included, on the worker goroutine (the scheduler's for Missed); it
	// must be safe for concurrent use and must not block.
	OnResult func(Sample)
	// MaxFailures bounds the Samples kept in Result.Failures; 0 means
	// DefaultMaxFailures and negative keeps none.
	MaxFailures int
}

func (c *Config) setDefaults() error {
	switch {
	case !(c.Rate > 0) || math.IsInf(c.Rate, 0):
		return fmt.Errorf("loadgen: rate %v is not a positive number", c.Rate)
	case c.Duration < 0, c.Warmup < 0, c.Timeout < 0, c.Window < 0, c.MaxInFlight < 0:
		return errors.New("loadgen: negative duration, warm-up, timeout, window or in-flight bound")
	case c.Duration > 0 && c.Requests > 0:
		return errors.New("loadgen: set Duration or Requests, not both")
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
	if c.Window == 0 {
		c.Window = DefaultWindow
	}
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	switch {
	case c.MaxFailures == 0:
		c.MaxFailures = DefaultMaxFailures
	case c.MaxFailures < 0:
		c.MaxFailures = 0
	}
	if s := c.Settle; s != nil {
		switch {
		case s.Step <= 0 || s.Windows < 1 || s.Window <= 0:
			return errors.New("loadgen: settle needs a positive step, window and window count")
		case s.Window%c.Window != 0:
			return fmt.Errorf("loadgen: settle window %v is not a multiple of the window %v", s.Window, c.Window)
		case time.Duration(s.Windows)*s.Window > c.Warmup:
			return fmt.Errorf("loadgen: warm-up %v is shorter than %d settle windows of %v", c.Warmup, s.Windows, s.Window)
		case s.Max < c.Warmup:
			return fmt.Errorf("loadgen: settle maximum %v below the warm-up %v", s.Max, c.Warmup)
		case !(s.MaxSpread >= 0):
			return errors.New("loadgen: negative settle spread")
		}
	}
	return nil
}

// Progress is a running generator's counters.
type Progress struct {
	// Scheduled counts requests whose intended time came, Missed ones
	// included; Done those that ended.
	Scheduled, Done int64
	// Missed counts requests never sent.
	Missed int64
	// InFlight counts requests being sent; Backlog those waiting.
	InFlight, Backlog int64
	// Measuring reports whether the measured phase has begun.
	Measuring bool
	// Next is the intended send time of the next request to schedule;
	// once it lies after the clock's time, every due request has been
	// dispatched.
	Next time.Time
}

// Generator is a running load generator.
type Generator struct {
	cfg    Config
	target Target
	clk    clock.Clock
	start  time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wake     chan struct{} // capacity 1: a window finished or Stop
	done     chan struct{} // closed when the result is ready
	result   *Result
	err      error

	mu       sync.Mutex // guards inFlight and backlog
	inFlight int
	backlog  []queued // FIFO, at most cfg.MaxBacklog
	workers  sync.WaitGroup

	rec *recorder
	ph  *phases // scheduler goroutine only, except the atomics it publishes

	scheduled, finished, missed atomic.Int64
	next                        atomic.Int64 // offset of the next request to schedule
	measuring                   atomic.Bool
	// pending is the offset of the checkpoint awaiting a settle decision,
	// or -1; workers finishing a request before it wake the scheduler.
	pending atomic.Int64
}

type queued struct {
	r   Request
	off time.Duration
}

// Run starts a run and waits for its result; see Start.
func Run(ctx context.Context, t Target, c Config) (*Result, error) {
	g, err := Start(ctx, t, c)
	if err != nil {
		return nil, err
	}
	return g.Wait()
}

// Start begins offering load to t. The run ends when its measured phase
// is complete, at Stop, or when ctx ends (which also cancels requests in
// flight); Wait then returns the result.
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
		stopCh: make(chan struct{}),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	g.start = g.clk.Now()
	g.ph = newPhases(&g.cfg)
	g.rec = newRecorder(&g.cfg, g.ph.segmentStarts())
	g.pending.Store(-1)
	go g.run(ctx)
	return g, nil
}

// Stop ends the schedule: requests already due are still sent, those in
// flight finish, and the measured phase ends at the stop instant. It is
// idempotent.
func (g *Generator) Stop() {
	g.stopOnce.Do(func() {
		close(g.stopCh)
		g.poke()
	})
}

// Done is closed when the result is ready.
func (g *Generator) Done() <-chan struct{} { return g.done }

// Wait waits for the run to end and returns its result, with the context's
// error when the context ended the run.
func (g *Generator) Wait() (*Result, error) {
	<-g.done
	return g.result, g.err
}

// Progress returns the current counters.
func (g *Generator) Progress() Progress {
	g.mu.Lock()
	inFlight, backlog := g.inFlight, len(g.backlog)
	g.mu.Unlock()
	return Progress{
		Scheduled: g.scheduled.Load(),
		Done:      g.finished.Load(),
		Missed:    g.missed.Load(),
		InFlight:  int64(inFlight),
		Backlog:   int64(backlog),
		Measuring: g.measuring.Load(),
		Next:      g.start.Add(time.Duration(g.next.Load())),
	}
}

// StartTime returns the time the schedule began: the intended send time
// of request 0.
func (g *Generator) StartTime() time.Time { return g.start }

func (g *Generator) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// offset returns request seq's intended offset from the start.
func (g *Generator) offset(seq uint64) time.Duration {
	return time.Duration(float64(seq) * 1e9 / g.cfg.Rate)
}

// run owns the run: the schedule, then the wait for every worker. ctx is
// the run's context, which requests in flight derive from.
func (g *Generator) run(ctx context.Context) {
	defer close(g.done)
	endOff, err := g.schedule(ctx)
	g.workers.Wait()
	g.result = g.rec.result(g.start, g.ph, endOff, g.cfg.Rate)
	g.err = err
}

// schedule dispatches requests at their intended times until the run's
// end and returns the offset at which the schedule ended, with the
// context's error when the context ended it.
func (g *Generator) schedule(ctx context.Context) (time.Duration, error) {
	timer := g.clk.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var seq uint64
	stopAt := time.Duration(-1)
	for {
		if err := ctx.Err(); err != nil {
			// The context ends the run at once; nothing more is sent.
			end := g.clk.Since(g.start)
			g.ph.stop(end)
			return end, err
		}
		if stopAt < 0 && g.stopped() {
			stopAt = g.clk.Since(g.start)
		}
		off := g.offset(seq)
		g.ph.decide(g, off)
		if stopAt >= 0 && off >= stopAt {
			// Requests due before the stop instant were still sent.
			g.ph.stop(stopAt)
			return stopAt, nil
		}
		switch g.ph.admit(seq, off) {
		case admitEnd:
			return g.ph.end, nil
		case admitPause:
			// The settle decision for the pending checkpoint has not come
			// yet; requests past the end it would set wait for it, unless
			// the run is stopping.
			if stopAt >= 0 {
				g.ph.stop(off)
				return off, nil
			}
			g.waitWake(ctx, timer, -1)
			continue
		case admitGo:
		}
		if now := g.clk.Since(g.start); now < off {
			g.waitWake(ctx, timer, off-now)
			continue
		}
		g.dispatch(ctx, seq, off)
		seq++
		g.next.Store(int64(g.offset(seq)))
	}
}

// waitWake waits for d (forever when negative), a wake-up, Stop or the
// context's end.
func (g *Generator) waitWake(ctx context.Context, timer clock.Timer, d time.Duration) {
	var fire <-chan time.Time
	if d >= 0 {
		timer.Reset(d)
		fire = timer.C()
	}
	select {
	case <-fire:
		return
	case <-g.wake:
	case <-g.stopCh:
	case <-ctx.Done():
	}
	timer.Stop()
}

func (g *Generator) stopped() bool {
	select {
	case <-g.stopCh:
		return true
	default:
		return false
	}
}

// dispatch hands request seq to a free worker, queues it, or counts it
// Missed.
func (g *Generator) dispatch(ctx context.Context, seq uint64, off time.Duration) {
	r := Request{Seq: seq, Intended: g.start.Add(off)}
	g.rec.dispatched(off)
	g.scheduled.Add(1)
	if g.ph.measuringAt(off) {
		g.measuring.Store(true)
	}
	g.mu.Lock()
	switch {
	case g.inFlight < g.cfg.MaxInFlight:
		g.inFlight++
		g.mu.Unlock()
		g.workers.Add(1)
		go g.worker(ctx, queued{r, off})
	case len(g.backlog) < g.cfg.MaxBacklog:
		g.backlog = append(g.backlog, queued{r, off})
		g.mu.Unlock()
	default:
		g.mu.Unlock()
		g.missed.Add(1)
		g.finish(Sample{Request: r, Done: g.clk.Now(), Response: Response{Outcome: Missed}}, off)
	}
}

// worker sends q, then backlogged requests, until the backlog is empty.
func (g *Generator) worker(ctx context.Context, q queued) {
	defer g.workers.Done()
	for {
		g.send(ctx, q)
		g.mu.Lock()
		if len(g.backlog) == 0 {
			g.inFlight--
			g.backlog = nil
			g.mu.Unlock()
			return
		}
		q = g.backlog[0]
		g.backlog[0] = queued{}
		g.backlog = g.backlog[1:]
		g.mu.Unlock()
	}
}

// send runs one request against the target, within Timeout of the run's
// context ctx.
func (g *Generator) send(ctx context.Context, q queued) {
	sent := g.clk.Now()
	rctx, cancel := context.WithCancelCause(ctx)
	t := g.clk.AfterFunc(g.cfg.Timeout, func() { cancel(ErrTimeout) })
	resp := g.target.Do(rctx, q.r)
	t.Stop()
	timedOut := errors.Is(context.Cause(rctx), ErrTimeout)
	cancel(nil)
	resp.Outcome = normalize(resp, timedOut, ctx.Err() != nil)
	g.finish(Sample{Request: q.r, Sent: sent, Done: g.clk.Now(), Response: resp}, q.off)
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

// finish records a finished request and runs the hook.
func (g *Generator) finish(s Sample, off time.Duration) {
	g.rec.add(s, off)
	g.finished.Add(1)
	if p := g.pending.Load(); p >= 0 && int64(off) < p {
		g.poke()
	}
	if g.cfg.OnResult != nil {
		g.cfg.OnResult(s)
	}
}
