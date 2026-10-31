// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Clock exchanges and polls of a load session.
const (
	// ClockExchanges is how many exchanges the load host's clock offset
	// takes the quickest of.
	ClockExchanges = 16
	// remotePoll spaces the polls of a load host's progress while a load
	// ends.
	remotePoll = 50 * time.Millisecond
)

// loadSession is a run's load service on the load host (labhost load),
// which starts the run's loads there and keeps every request's record
// until the run's load has ended (docs/engineering/03-testing-and-quality-strategy.md
// "Scale profiles"). Every time it answers is on the load host's clock,
// which the session moves onto the harness's by the offset it measures
// as it starts, so the analyses compare the requests with the injections,
// the samples and the Node clock as at the other profiles.
type loadSession struct {
	t   testing.TB
	svc service
	// offset is the load host's clock minus the harness's; bound is half
	// the round trip of the exchange that measured it, its error bound.
	offset, bound time.Duration

	mu    sync.Mutex
	loads []*remoteLoad
	seq   int
}

// service is a lab service's client: a harness.Proc over ssh, or a
// lab.Client of an in-process service in tests.
type service interface {
	Do(r lab.Request) (lab.Response, error)
}

// procLoad names the load service, its labhost command and the process
// the harness starts it as.
const procLoad = "load"

// startLoadSession starts labhost load on the load host's measured
// cpuset; the session ends with t.
func (pl *Placement) startLoadSession(ctx context.Context, t testing.TB) (*loadSession, error) {
	r := pl.Profile.Runner(harness.RoleLoad)
	p, err := r.Start(context.WithoutCancel(ctx), procLoad, []string{pl.bins[harness.RoleLoad], procLoad}, pl.Profile.Hosts[harness.RoleLoad].CPUs)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { stopService(t, p, procLoad) })
	return newLoadSession(t, p)
}

// newLoadSession returns the session of the load service svc, once it
// measured the load host's clock offset.
func newLoadSession(t testing.TB, svc service) (*loadSession, error) {
	s := &loadSession{t: t, svc: svc}
	var err error
	s.offset, s.bound, err = estimateOffset(func() (time.Time, error) {
		resp, err := svc.Do(lab.Request{Op: lab.OpStats})
		return resp.Now, err
	}, time.Now, ClockExchanges)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// estimateOffset measures a remote clock's offset from now's: of n
// exchanges, each a call of remote between two readings of now, it takes
// the one with the shortest round trip, and the remote reading minus the
// round trip's midpoint; bound, half that round trip, bounds the error.
func estimateOffset(remote func() (time.Time, error), now func() time.Time, n int) (offset, bound time.Duration, err error) {
	best := time.Duration(-1)
	for range n {
		t0 := now()
		at, err := remote()
		if err != nil {
			return 0, 0, fmt.Errorf("chaos: the load host's clock: %w", err)
		}
		rtt := now().Sub(t0)
		if at.IsZero() {
			return 0, 0, errors.New("chaos: the load host answered without its clock")
		}
		if best < 0 || rtt < best {
			best, offset = rtt, at.Sub(t0.Add(rtt/2))
		}
	}
	if best < 0 {
		return 0, 0, errors.New("chaos: no clock exchange")
	}
	return offset, best / 2, nil
}

// local returns at, a time on the load host's clock, on the harness's.
func (s *loadSession) local(at time.Time) time.Time {
	if at.IsZero() {
		return at
	}
	return at.Add(-s.offset)
}

// nextName returns the name of the session's next load.
func (s *loadSession) nextName(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return kind + "-" + strconv.Itoa(s.seq)
}

// remoteLoad is a load of the session.
type remoteLoad struct {
	s    *loadSession
	name string
	load *Load
	attr attribute

	mu       sync.Mutex
	state    *lab.LoadState
	waited   bool
	waitErr  error
	returned bool
}

// remoteStream is stream i of a load on the load host.
type remoteStream struct {
	rl *remoteLoad
	i  int
}

func (rs *remoteStream) stream() lab.StreamState {
	rs.rl.mu.Lock()
	defer rs.rl.mu.Unlock()
	return rs.rl.state.Streams[rs.i]
}

func (rs *remoteStream) progress() loadgen.Progress {
	st := rs.stream()
	return loadgen.Progress{MeasureStart: rs.rl.s.local(st.MeasureStart), Settled: st.Settled, Next: rs.rl.s.local(st.Next)}
}

func (rs *remoteStream) startTime() time.Time { return rs.rl.s.local(rs.stream().Start) }

func (rs *remoteStream) ended() bool { return rs.stream().Done }

// recordsReturned reports whether the load's records returned.
func (rl *remoteLoad) recordsReturned() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.returned
}

// setState keeps the load's latest progress.
func (rl *remoteLoad) setState(st *lab.LoadState) error {
	if st == nil || len(st.Streams) != len(rl.load.Streams) {
		return fmt.Errorf("chaos: load %s: the load host answered %+v", rl.name, st)
	}
	rl.mu.Lock()
	rl.state = st
	rl.mu.Unlock()
	return nil
}

// refresh reads the load's progress.
func (rl *remoteLoad) refresh() error {
	resp, err := rl.s.svc.Do(lab.Request{Op: lab.OpProgress, Name: rl.name})
	if err != nil {
		return fmt.Errorf("chaos: load %s: %w", rl.name, err)
	}
	return rl.setState(resp.LoadState)
}

// stop ends the load's schedules.
func (rl *remoteLoad) stop() {
	if resp, err := rl.s.svc.Do(lab.Request{Op: lab.OpStop, Name: rl.name}); err == nil {
		_ = rl.setState(resp.LoadState)
	}
}

// running reports whether the load has a stream with a request that has
// not ended, as last read.
func (rl *remoteLoad) running() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.state.Running
}

// wait waits until the load ended and keeps each stream's result; with
// records, the records of every load of the session return once each of
// them ended and was waited for, the run's load having ended then.
func (rl *remoteLoad) wait(records bool) error {
	rl.mu.Lock()
	waited, werr := rl.waited, rl.waitErr
	rl.mu.Unlock()
	if !waited {
		werr = rl.waitEnd()
		rl.mu.Lock()
		rl.waited, rl.waitErr = true, werr
		rl.mu.Unlock()
	}
	if !records {
		return werr
	}
	return errors.Join(werr, rl.s.returnRecords())
}

// waitEnd polls the load's progress until it ended, then reads each
// stream's result.
func (rl *remoteLoad) waitEnd() error {
	for {
		if err := rl.refresh(); err != nil {
			return err
		}
		if !rl.running() {
			break
		}
		time.Sleep(remotePoll)
	}
	resp, err := rl.s.svc.Do(lab.Request{Op: lab.OpResult, Name: rl.name})
	if err != nil {
		return fmt.Errorf("chaos: load %s: %w", rl.name, err)
	}
	if err := rl.setState(resp.LoadState); err != nil {
		return err
	}
	var errs []error
	for i, st := range resp.LoadState.Streams {
		s := rl.load.Streams[i]
		var res *loadgen.Result
		if st.Result != nil {
			if res, err = st.Result.Result(rl.s.local); err != nil {
				errs = append(errs, fmt.Errorf("stream %s: %w", s.Spec.Name, err))
			}
		}
		var serr error
		if st.Error != "" {
			serr = errors.New(st.Error)
			errs = append(errs, fmt.Errorf("stream %s: %s", s.Spec.Name, st.Error))
		}
		s.mu.Lock()
		s.result, s.err = res, serr
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

// returnRecords returns the records of every load of the session once
// each load ended and was waited for: then the run's load has ended, and
// no measured window remains. A load still waited for keeps every record
// where it is.
func (s *loadSession) returnRecords() error {
	s.mu.Lock()
	loads := append([]*remoteLoad(nil), s.loads...)
	s.mu.Unlock()
	for _, rl := range loads {
		rl.mu.Lock()
		waited := rl.waited
		rl.mu.Unlock()
		if !waited {
			return nil
		}
	}
	var errs []error
	for _, rl := range loads {
		if !rl.recordsReturned() {
			errs = append(errs, rl.fetch())
		}
	}
	return errors.Join(errs...)
}

// fetch returns the load's records, block by block, into its streams,
// moved onto the harness's clock, each key derived from its sequence
// number with the stream's key source and each failed request attributed
// to its Node, then drops them from the load host.
func (rl *remoteLoad) fetch() error {
	rl.mu.Lock()
	state := rl.state
	rl.mu.Unlock()
	for i, st := range rl.load.Streams {
		// The load host counted the records: about 60 million for one
		// CE-12 phase (hypothesis), which a growing slice would hold twice
		// while it copies.
		st.mu.Lock()
		st.requests = slices.Grow(st.requests, int(state.Streams[i].Records))
		st.mu.Unlock()
		var d lab.RecordDecoder
		for b := 0; ; b++ {
			resp, err := rl.s.svc.Do(lab.Request{Op: lab.OpRecords, Name: rl.name, Stream: i, Block: b})
			if err != nil {
				return fmt.Errorf("chaos: load %s: %w", rl.name, err)
			}
			if resp.Records == nil {
				return fmt.Errorf("chaos: load %s: no records in the answer", rl.name)
			}
			if err := d.Decode(resp.Records.Data, func(r lab.Record) { st.keep(rl.request(st, r), rl.attr) }); err != nil {
				return fmt.Errorf("chaos: load %s: stream %s: %w", rl.name, st.Spec.Name, err)
			}
			if b+1 >= resp.Records.Blocks {
				break
			}
		}
	}
	rl.mu.Lock()
	rl.returned = true
	rl.mu.Unlock()
	if _, err := rl.s.svc.Do(lab.Request{Op: lab.OpRemove, Name: rl.name}); err != nil {
		return fmt.Errorf("chaos: load %s: %w", rl.name, err)
	}
	return nil
}

// request returns the Request of a returned record of stream st.
func (rl *remoteLoad) request(st *Stream, r lab.Record) Request {
	q := Request{
		Seq: r.Seq, Intended: rl.s.local(r.Intended), Sent: rl.s.local(r.Sent), Done: rl.s.local(r.Done),
		Outcome: r.Outcome, Status: r.Status, Code: r.Code, Local: r.Local,
	}
	if r.Keyed && st.keys != nil {
		q.Key = st.keys.Key(r.Seq)
	}
	return q
}

// loadTarget is what a load on the load host sends its requests to: the
// balancers' addresses and the CA of the https listener's certificate,
// which names the loopback hosts.
type loadTarget struct {
	http, https string
	ca          []byte
}

// planRemoteLoad returns the load service's start of spec against target,
// with the Consumers' API keys keys, and the streams that keep its
// returned records. Prepare, which runs on the harness's host, cannot run
// there.
func planRemoteLoad(spec LoadSpec, target loadTarget, keys []string, byKey map[string]string) (*lab.LoadStart, []*Stream, error) {
	if spec.Warmup == 0 {
		spec.Warmup = DefaultWarmup
	}
	if spec.Duration <= 0 || len(spec.Streams) == 0 {
		return nil, nil, errors.New("chaos: load: no stream or no duration")
	}
	ls := &lab.LoadStart{Warmup: spec.Warmup, Duration: spec.streamDuration(), Settle: lab.SettleSpecOf(spec.Settle)}
	var streams []*Stream
	for _, ss := range spec.Streams {
		if ss.Prepare != nil {
			return nil, nil, fmt.Errorf("chaos: load: stream %s prepares its requests on the harness's host, which the load host cannot", ss.Name)
		}
		header, ks, src, err := streamKeys(ss, keys)
		if err != nil {
			return nil, nil, err
		}
		// HTTP/1.1 requests spread round-robin over the stream's
		// connections, as on the harness's host.
		stream := lab.LoadStream{
			Name: ss.Name, URL: "http://" + target.http + ss.Path, Method: ss.Method, Body: ss.Body, Protocol: protocolName(ss.Protocol),
			Connections: ss.Connections, RoundRobin: ss.Protocol == loadgen.HTTP1, DisableKeepAlives: ss.DisableKeepAlives,
			Rate: ss.Rate, KeyHeader: header, Keys: ks,
		}
		if ss.HTTPS {
			if target.https == "" {
				return nil, nil, fmt.Errorf("chaos: load: stream %s needs the https balancer", ss.Name)
			}
			stream.URL, stream.CA, stream.ServerName = "https://"+target.https+ss.Path, string(target.ca), "localhost"
		}
		ls.Streams = append(ls.Streams, stream)
		streams = append(streams, &Stream{Spec: ss, keys: src, consumer: byKey})
	}
	return ls, streams, nil
}

// startLoad starts spec's streams as one load of the session, against
// target, its failed requests attributed by attr once they return.
func (s *loadSession) startLoad(spec LoadSpec, target loadTarget, apiKeyMap map[string]string, attr attribute) (*Load, error) {
	keys, byKey, err := apiKeys(apiKeyMap)
	if err != nil {
		return nil, err
	}
	ls, streams, err := planRemoteLoad(spec, target, keys, byKey)
	if err != nil {
		return nil, err
	}
	l := &Load{Streams: streams, settle: spec.Settle != nil}
	rl := &remoteLoad{s: s, name: s.nextName("load"), load: l, attr: attr}
	l.remote = rl
	for i, st := range l.Streams {
		st.remote = &remoteStream{rl: rl, i: i}
	}
	resp, err := s.svc.Do(lab.Request{Op: lab.OpStart, Name: rl.name, Load: ls})
	if err != nil {
		return nil, fmt.Errorf("chaos: load %s: %w", rl.name, err)
	}
	if err := rl.setState(resp.LoadState); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.loads = append(s.loads, rl)
	s.mu.Unlock()
	return l, nil
}

// startRemoteLoad starts spec's streams as one load on the load host,
// against the balancers on the gateway host.
func (r *Run) startRemoteLoad(spec LoadSpec) (*Load, error) {
	s, err := r.loadSession()
	if err != nil {
		return nil, err
	}
	target := loadTarget{http: r.HTTP.Addr(), ca: r.Top.CA().CertPEM}
	if r.HTTPS != nil {
		target.https = r.HTTPS.Addr()
	}
	return s.startLoad(spec, target, r.APIKeys, r.attribute)
}

// protocolName returns the load service's name of p.
func protocolName(p loadgen.Protocol) string {
	switch p {
	case loadgen.H2C:
		return "h2c"
	case loadgen.H2:
		return "h2"
	default:
		return "http1"
	}
}

// loadSession returns the run's load session, starting it at the first
// load.
func (r *Run) loadSession() (*loadSession, error) {
	r.mu.Lock()
	s := r.session
	r.mu.Unlock()
	if s != nil {
		return s, nil
	}
	s, err := r.H.place.startLoadSession(r.ctx, r.T)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.session = s
	r.mu.Unlock()
	r.Breakdown("load_clock_offset_us", float64(s.offset)/float64(time.Microsecond))
	r.Breakdown("load_clock_bound_us", float64(s.bound)/float64(time.Microsecond))
	return s, nil
}

// runStep runs one step of the saturation ladder on the load host: one
// stream, keeping no records, whose result it returns once it ended.
func (s *loadSession) runStep(stream lab.LoadStream, warmup, duration time.Duration) (*loadgen.Result, error) {
	name := s.nextName("step")
	if _, err := s.svc.Do(lab.Request{Op: lab.OpStart, Name: name, Load: &lab.LoadStart{
		Streams: []lab.LoadStream{stream}, Warmup: warmup, Duration: duration, NoRecords: true,
	}}); err != nil {
		return nil, fmt.Errorf("chaos: %s: %w", name, err)
	}
	for {
		resp, err := s.svc.Do(lab.Request{Op: lab.OpProgress, Name: name})
		if err != nil {
			return nil, fmt.Errorf("chaos: %s: %w", name, err)
		}
		if !resp.LoadState.Running {
			break
		}
		time.Sleep(remotePoll)
	}
	resp, err := s.svc.Do(lab.Request{Op: lab.OpResult, Name: name})
	if err != nil {
		return nil, fmt.Errorf("chaos: %s: %w", name, err)
	}
	st := resp.LoadState.Streams[0]
	if st.Error != "" {
		return nil, fmt.Errorf("chaos: %s: %s", name, st.Error)
	}
	res, err := st.Result.Result(s.local)
	if err != nil {
		return nil, fmt.Errorf("chaos: %s: %w", name, err)
	}
	if _, err := s.svc.Do(lab.Request{Op: lab.OpRemove, Name: name}); err != nil {
		return nil, fmt.Errorf("chaos: %s: %w", name, err)
	}
	return res, nil
}
