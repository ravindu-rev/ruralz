// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// DefaultWarmup is the load generator's warm-up before a run's 60 s
// baseline (target), discarded from the measured phase.
const DefaultWarmup = loadgen.BenchmarkWarmup

// Rate Limit and Quota denial codes: a local token bucket's, which makes
// no State Store call, GCRA's or the over-limit cache's, and an exhausted
// quota's.
const (
	CodeLocalDenied = "RZ-RL-001"
	CodeGCRADenied  = "RZ-RL-002"
	CodeQuotaDenied = "RZ-RL-003"
)

// Decisions are the problem codes of the 429s a limit Route answers by
// design: a run counts them as achieved, never as errors.
func Decisions() []string { return []string{CodeLocalDenied, CodeGCRADenied, CodeQuotaDenied} }

// StreamSpec is one load stream of a run: requests of one shape to one
// listener behind the L4 balancer, at a share of the run's rate.
type StreamSpec struct {
	// Name names the stream in the record's interval logs, such as
	// "plain-http1".
	Name string
	// Path is the request path, such as PathPlain.
	Path string
	// Method is the request method; default GET.
	Method string
	// Body is every request's body.
	Body []byte
	// HTTPS sends through the https listener's balancer (HTTP/2 or
	// HTTP/1.1 over TLS); otherwise through the http one.
	HTTPS bool
	// Protocol is the wire protocol; HTTP1 by default.
	Protocol loadgen.Protocol
	// Connections bounds or sets the stream's connections; 0 keeps the
	// load generator's default.
	Connections int
	// DisableKeepAlives opens a connection per HTTP/1.1 request, so the
	// L4 balancer spreads the stream's requests over every healthy Node,
	// one restarted after the load began included, which keep-alive
	// connections, moved off a Node by its Drain, never return to.
	DisableKeepAlives bool
	// Rate is the stream's offered rate in requests per second.
	Rate float64
	// KeyHeader and Keys set a per-request key header, such as
	// KeyHeader with loadgen.Cycle.
	KeyHeader string
	Keys      loadgen.KeySource
	// APIKeys rotates the Consumers' API keys in APIKeyField, request seq
	// carrying Consumers()[seq % 4]'s key.
	APIKeys bool
	// Counted are the problem codes counted as achieved, such as
	// Decisions() on a limit Route.
	Counted []string
	// Prepare adjusts each request last.
	Prepare func(r *http.Request, seq uint64)
}

// Request is one finished request of a stream, kept for the analyses
// the load generator's aggregates cannot answer: per-key and per-window
// admissions and the attribution of failures.
type Request struct {
	Seq      uint64
	Intended time.Time
	Sent     time.Time
	Done     time.Time
	Outcome  loadgen.Outcome
	Status   int
	Code     string
	// Key is the request's key header value, or the Consumer of its API
	// key.
	Key string
	// Local is the client address of the request's connection, which
	// the L4 balancer attributes to a Node.
	Local string
	// Node is the Node the balancer attributes the connection to, set
	// for every request that did not achieve; "" when unknown.
	Node string
}

// Failed reports whether r did not achieve: a transport failure, a
// missed or canceled request, or an error response with a code the stream
// does not count.
func (s *Stream) Failed(r Request) bool {
	switch r.Outcome {
	case loadgen.Success:
		return false
	case loadgen.ClientError, loadgen.ServerError:
		return !slices.Contains(s.Spec.Counted, r.Code)
	default:
		return true
	}
}

// Stream is one running load stream.
type Stream struct {
	Spec StreamSpec
	gen  *loadgen.Generator
	tgt  *loadgen.HTTPTarget

	mu       sync.Mutex
	requests []Request
	result   *loadgen.Result
	err      error
	consumer map[string]string // API key to Consumer
}

// Result returns the stream's load generator result once Wait returned.
func (s *Stream) Result() *loadgen.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Requests returns the stream's finished requests in completion order.
func (s *Stream) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Generator returns the stream's load generator.
func (s *Stream) Generator() *loadgen.Generator { return s.gen }

// Load is the load of one run: one open-loop generator per stream,
// started together with one warm-up and measured duration.
type Load struct {
	Streams []*Stream
	// settle reports a load whose streams extend their warm-up by the
	// load generator's settle rule and run until Wait stops them.
	settle bool
}

// LoadSpec configures a run's load.
type LoadSpec struct {
	// Streams are the streams; each needs a positive Rate.
	Streams []StreamSpec
	// Warmup precedes the measured phase; default DefaultWarmup.
	Warmup time.Duration
	// Settle, when set, extends each stream's warm-up until its latency
	// settles (loadgen.Settle), as a run whose criteria compare latencies
	// with the baseline's needs; Warmup is then the first checkpoint.
	// Each stream lasts Duration past the latest warm-up end Settle
	// allows, and Wait stops it once the run's timeline has ended.
	Settle *loadgen.Settle
	// Duration is the measured phase, every phase of the timeline after
	// the warm-up.
	Duration time.Duration
}

// attribute maps a client address to the Node the balancer that carried
// the connection attributes it to.
type attribute func(https bool, local string) string

// startLoad starts every stream of spec against the balancer addresses.
func startLoad(ctx context.Context, spec LoadSpec, httpAddr, httpsAddr string, clientTLS *tls.Config, apiKeys map[string]string, attr attribute) (*Load, error) {
	if spec.Warmup == 0 {
		spec.Warmup = DefaultWarmup
	}
	if spec.Duration <= 0 || len(spec.Streams) == 0 {
		return nil, errors.New("chaos: load: no stream or no duration")
	}
	l := &Load{settle: spec.Settle != nil}
	duration := spec.Duration
	if spec.Settle != nil {
		// A stream that settles early still covers a timeline that starts
		// at the latest stream's warm-up end.
		duration += max(0, spec.Settle.Max-spec.Warmup)
	}
	keys := make([]string, 0, len(apiKeys))
	byKey := map[string]string{}
	for _, c := range Consumers() {
		k, ok := apiKeys[c]
		if !ok {
			return nil, fmt.Errorf("chaos: load: no API key for Consumer %s", c)
		}
		keys = append(keys, k)
		byKey[k] = c
	}
	for _, ss := range spec.Streams {
		if ss.Rate <= 0 {
			return nil, fmt.Errorf("chaos: load: stream %s has rate %v", ss.Name, ss.Rate)
		}
		base := "http://" + httpAddr
		if ss.HTTPS {
			if httpsAddr == "" {
				return nil, fmt.Errorf("chaos: load: stream %s needs the https balancer", ss.Name)
			}
			base = "https://" + httpsAddr
		}
		// HTTP/1.1 requests spread round-robin over the stream's
		// connections, as HTTP/2 requests do, so the L4 balancer, which
		// balances connections, carries a light load to every Node.
		hc := loadgen.HTTPConfig{
			URL: base + ss.Path, Method: ss.Method, Body: ss.Body, Protocol: ss.Protocol, Connections: ss.Connections,
			RoundRobin: ss.Protocol == loadgen.HTTP1, DisableKeepAlives: ss.DisableKeepAlives,
			KeyHeader: ss.KeyHeader, Keys: ss.Keys, Prepare: ss.Prepare, TLS: clientTLS,
		}
		if ss.APIKeys {
			if ss.KeyHeader != "" {
				return nil, fmt.Errorf("chaos: load: stream %s sets both APIKeys and KeyHeader", ss.Name)
			}
			hc.KeyHeader, hc.Keys = APIKeyField, listKeys(keys)
		}
		tgt, err := loadgen.NewHTTP(hc) //nolint:contextcheck // NewHTTP only checks the URL with a request it never sends; loadgen.Start below takes ctx
		if err != nil {
			l.close()
			return nil, fmt.Errorf("chaos: load: stream %s: %w", ss.Name, err)
		}
		s := &Stream{Spec: ss, tgt: tgt, consumer: byKey}
		l.Streams = append(l.Streams, s)
	}
	for _, s := range l.Streams {
		gen, err := loadgen.Start(ctx, s.tgt, loadgen.Config{
			Rate: s.Spec.Rate, Warmup: spec.Warmup, Settle: spec.Settle, Duration: duration,
			OnResult: func(smp loadgen.Sample) { s.add(smp, attr) },
		})
		if err != nil {
			l.Stop()
			l.close()
			return nil, fmt.Errorf("chaos: load: stream %s: %w", s.Spec.Name, err)
		}
		s.gen = gen
	}
	return l, nil
}

// add keeps one finished request, attributing a failed one to its Node
// while the balancer still knows its connection.
func (s *Stream) add(smp loadgen.Sample, attr attribute) {
	r := Request{
		Seq: smp.Seq, Intended: smp.Intended, Sent: smp.Sent, Done: smp.Done,
		Outcome: smp.Outcome, Status: smp.Status, Code: smp.Code, Key: smp.Key, Local: smp.Conn.Local,
	}
	if c, ok := s.consumer[r.Key]; ok && s.Spec.APIKeys {
		r.Key = c
	}
	if s.Failed(r) && r.Local != "" && attr != nil {
		r.Node = attr(s.Spec.HTTPS, r.Local)
	}
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
}

// MeasureStart returns when every stream's measured phase has begun: the
// latest stream's warm-up end; ok is false while a stream's warm-up end
// is undecided, as while a settling stream has not settled.
func (l *Load) MeasureStart() (t time.Time, ok bool) {
	for _, s := range l.Streams {
		st := s.gen.Progress().MeasureStart
		if st.IsZero() {
			return time.Time{}, false
		}
		if st.After(t) {
			t = st
		}
	}
	return t, true
}

// Settled reports whether every stream's warm-up settled; a load without
// Settle always has.
func (l *Load) Settled() bool {
	for _, s := range l.Streams {
		if !s.gen.Progress().Settled {
			return false
		}
	}
	return true
}

// Warmup returns how long the warm-up lasted: from the first stream's
// start to MeasureStart, zero while undecided.
func (l *Load) Warmup() time.Duration {
	t, ok := l.MeasureStart()
	if !ok {
		return 0
	}
	first := t
	for _, s := range l.Streams {
		if st := s.gen.StartTime(); st.Before(first) {
			first = st
		}
	}
	return t.Sub(first)
}

// WaitMeasuring waits until every stream's warm-up end is decided, or ctx
// ends, and returns MeasureStart.
func (l *Load) WaitMeasuring(ctx context.Context) (time.Time, error) {
	for {
		if t, ok := l.MeasureStart(); ok {
			return t, nil
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// WaitUntil waits until every stream has scheduled its requests due by
// t, so a run's phases follow the load's schedule, or ctx ends.
func (l *Load) WaitUntil(ctx context.Context, t time.Time) error {
	for {
		done := true
		for _, s := range l.Streams {
			select {
			case <-s.gen.Done():
				continue
			default:
			}
			if s.gen.Progress().Next.Before(t) {
				done = false
			}
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Stop ends every stream's schedule.
func (l *Load) Stop() {
	for _, s := range l.Streams {
		if s.gen != nil {
			s.gen.Stop()
		}
	}
}

// Wait waits for every stream and keeps its result. A settling load runs
// past its timeline (LoadSpec.Settle), so Wait stops it first; call it
// once the run's last phase has ended.
func (l *Load) Wait() error {
	if l.settle {
		l.Stop()
	}
	var errs []error
	for _, s := range l.Streams {
		if s.gen == nil {
			continue
		}
		res, err := s.gen.Wait()
		s.mu.Lock()
		s.result, s.err = res, err
		s.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("stream %s: %w", s.Spec.Name, err))
		}
	}
	l.close()
	return errors.Join(errs...)
}

func (l *Load) close() {
	for _, s := range l.Streams {
		if s.tgt != nil {
			_ = s.tgt.Close()
		}
	}
}

// listKeys is a fixed list of keys, used in turn.
type listKeys []string

func (l listKeys) Key(seq uint64) string { return l[seq%uint64(len(l))] }
func (l listKeys) Cardinality() int      { return len(l) }
func (l listKeys) Distribution() string  { return "cycle" }
