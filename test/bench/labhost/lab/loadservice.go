// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// LoadStart starts one load of the load service (OpStart): open-loop
// streams started together with one warm-up and measured duration, as a
// chaos run offers its load (test/chaos LoadSpec).
type LoadStart struct {
	Streams []LoadStream `json:"streams"`
	// Warmup precedes each stream's measured phase.
	Warmup time.Duration `json:"warmupNs"`
	// Duration is each stream's measured phase.
	Duration time.Duration `json:"durationNs"`
	// Settle extends each stream's warm-up until its latency settles.
	Settle *SettleSpec `json:"settle,omitempty"`
	// NoRecords keeps no request record, for a load whose results alone
	// are read, as the saturation ladder's steps.
	NoRecords bool `json:"noRecords,omitempty"`
}

// LoadStream is one stream of a load: requests of one shape to one URL at
// a rate (test/chaos StreamSpec).
type LoadStream struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method,omitempty"`
	Body   []byte `json:"body,omitempty"`
	// Protocol is "http1" (the default), "h2c" or "h2".
	Protocol string `json:"protocol,omitempty"`
	// CA and ServerName verify an https URL: the PEM bundle of the CAs
	// and the name its certificate carries.
	CA         string `json:"ca,omitempty"`
	ServerName string `json:"serverName,omitempty"`
	// Connections, RoundRobin and DisableKeepAlives shape the stream's
	// connections (loadgen.HTTPConfig).
	Connections       int  `json:"connections,omitempty"`
	RoundRobin        bool `json:"roundRobin,omitempty"`
	DisableKeepAlives bool `json:"disableKeepAlives,omitempty"`
	// Rate is the offered rate in requests per second.
	Rate float64 `json:"rate"`
	// KeyHeader and Keys set a per-request key header.
	KeyHeader string   `json:"keyHeader,omitempty"`
	Keys      *KeySpec `json:"keys,omitempty"`
}

// SettleSpec is a loadgen.Settle.
type SettleSpec struct {
	Step      time.Duration `json:"stepNs"`
	Max       time.Duration `json:"maxNs"`
	Window    time.Duration `json:"windowNs"`
	Windows   int           `json:"windows"`
	MaxSpread float64       `json:"maxSpread"`
}

// SettleSpecOf returns s as a SettleSpec; nil for nil.
func SettleSpecOf(s *loadgen.Settle) *SettleSpec {
	if s == nil {
		return nil
	}
	return &SettleSpec{Step: s.Step, Max: s.Max, Window: s.Window, Windows: s.Windows, MaxSpread: s.MaxSpread}
}

// settle returns s as a loadgen.Settle; nil for nil.
func (s *SettleSpec) settle() *loadgen.Settle {
	if s == nil {
		return nil
	}
	return &loadgen.Settle{Step: s.Step, Max: s.Max, Window: s.Window, Windows: s.Windows, MaxSpread: s.MaxSpread}
}

// LoadState is a load's progress (OpStart, OpProgress, OpStop), with each
// stream's result once the load ended (OpResult).
type LoadState struct {
	Name string `json:"name"`
	// Running reports a load with a stream whose requests have not all
	// ended.
	Running bool          `json:"running"`
	Streams []StreamState `json:"streams"`
}

// StreamState is one stream's progress (loadgen.Progress) on the load
// host's clock.
type StreamState struct {
	// Start is the intended send time of request 0.
	Start time.Time `json:"start"`
	// MeasureStart is the measured phase's start once the warm-up's end
	// is decided; zero before. Settled reports a warm-up that settled.
	MeasureStart time.Time `json:"measureStart,omitzero"`
	Settled      bool      `json:"settled"`
	// Next is the intended send time of the next request to schedule.
	Next time.Time `json:"next"`
	// Done reports a stream whose every request ended.
	Done bool `json:"done"`
	// Records counts the stream's kept records and Blocks their blocks.
	Records int64 `json:"records"`
	Blocks  int   `json:"blocks"`
	// Error is the end of a stream its context ended, once Done.
	Error string `json:"error,omitempty"`
	// Result is the stream's result (OpResult).
	Result *ResultDoc `json:"result,omitempty"`
}

// RecordBlock is one block of a stream's records (OpRecords), which a
// RecordDecoder reads in block order.
type RecordBlock struct {
	Data []byte `json:"data"`
	// Blocks is the stream's number of blocks.
	Blocks int `json:"blocks"`
}

// ResultDoc is a loadgen.Result, its histograms in the compressed
// HdrHistogram V2 encoding, base64; the failures it lists stay on the
// load host, every request's record holding them.
type ResultDoc struct {
	Rate         float64       `json:"rate"`
	Window       time.Duration `json:"windowNs"`
	Start        time.Time     `json:"start"`
	MeasureStart time.Time     `json:"measureStart"`
	MeasureEnd   time.Time     `json:"measureEnd"`
	Settled      bool          `json:"settled"`
	Warmup       StatsDoc      `json:"warmup"`
	Measured     StatsDoc      `json:"measured"`
	Windows      []WindowDoc   `json:"windows"`
}

// StatsDoc is a loadgen.Stats.
type StatsDoc struct {
	Offered int64 `json:"offered"`
	// Outcomes counts the requests by loadgen.Outcome, in its order.
	Outcomes []int64          `json:"outcomes"`
	Statuses map[int]int64    `json:"statuses,omitempty"`
	Codes    map[string]int64 `json:"codes,omitempty"`
	NewConns int64            `json:"newConns"`
	Bytes    int64            `json:"bytes"`
	Latency  string           `json:"latency"`
	// Service is "" where the stats keep none, as in windows.
	Service string `json:"service,omitempty"`
}

// WindowDoc is a loadgen.Window.
type WindowDoc struct {
	Start time.Time `json:"start"`
	Stats StatsDoc  `json:"stats"`
}

// ResultDocOf returns r as a ResultDoc.
func ResultDocOf(r *loadgen.Result) *ResultDoc {
	d := &ResultDoc{
		Rate: r.Rate, Window: r.Window, Start: r.Start, MeasureStart: r.MeasureStart, MeasureEnd: r.MeasureEnd, Settled: r.Settled,
		Warmup: statsDocOf(&r.Warmup), Measured: statsDocOf(&r.Measured),
	}
	for _, w := range r.Windows {
		d.Windows = append(d.Windows, WindowDoc{Start: w.Start, Stats: statsDocOf(&w.Stats)})
	}
	return d
}

func statsDocOf(s *loadgen.Stats) StatsDoc {
	d := StatsDoc{
		Offered: s.Offered, Outcomes: slices.Clone(s.Outcomes[:]), Statuses: s.Statuses, Codes: s.Codes,
		NewConns: s.NewConns, Bytes: s.Bytes, Latency: EncodeHistogram(s.Latency),
	}
	if s.Service != nil {
		d.Service = EncodeHistogram(s.Service)
	}
	return d
}

// Result returns d as a loadgen.Result, its times moved by shift, such as
// the harness's translation of the load host's clock into its own.
func (d *ResultDoc) Result(shift func(time.Time) time.Time) (*loadgen.Result, error) {
	r := &loadgen.Result{
		Rate: d.Rate, Window: d.Window, Start: shift(d.Start), MeasureStart: shift(d.MeasureStart), MeasureEnd: shift(d.MeasureEnd),
		Settled: d.Settled,
	}
	var err error
	if r.Warmup, err = d.Warmup.stats(); err != nil {
		return nil, err
	}
	if r.Measured, err = d.Measured.stats(); err != nil {
		return nil, err
	}
	for _, w := range d.Windows {
		st, err := w.Stats.stats()
		if err != nil {
			return nil, err
		}
		r.Windows = append(r.Windows, loadgen.Window{Start: shift(w.Start), Stats: st})
	}
	return r, nil
}

func (d StatsDoc) stats() (loadgen.Stats, error) {
	s := loadgen.Stats{Offered: d.Offered, Statuses: d.Statuses, Codes: d.Codes, NewConns: d.NewConns, Bytes: d.Bytes}
	if len(d.Outcomes) > len(s.Outcomes) {
		return s, fmt.Errorf("lab: %d outcomes, want at most %d", len(d.Outcomes), len(s.Outcomes))
	}
	copy(s.Outcomes[:], d.Outcomes)
	var err error
	if s.Latency, err = DecodeHistogram(d.Latency); err != nil {
		return s, err
	}
	if d.Service != "" {
		if s.Service, err = DecodeHistogram(d.Service); err != nil {
			return s, err
		}
	}
	return s, nil
}

// LoadService is the load service of the chaos runs on the load host
// (labhost load; docs/engineering/03-testing-and-quality-strategy.md
// "Scale profiles"): it starts loads on request and answers their
// progress, keeps every request's record of their streams (Record,
// RecordEncoder), and returns the records, block by block, only once no
// load of the session runs, so never during a measured window. Every
// answer carries the service's clock (Response.Now).
type LoadService struct {
	cpu *cpuMeter

	mu    sync.Mutex
	loads map[string]*serviceLoad
}

// serviceLoad is one load of the service.
type serviceLoad struct {
	streams []*serviceStream
}

// serviceStream is one stream of a load.
type serviceStream struct {
	spec LoadStream
	tgt  *loadgen.HTTPTarget
	gen  *loadgen.Generator
	rec  *RecordEncoder // nil with LoadStart.NoRecords
}

// NewLoadService returns a load service without loads.
func NewLoadService() *LoadService {
	return &LoadService{cpu: newCPUMeter(), loads: map[string]*serviceLoad{}}
}

// Ready describes the service.
func (s *LoadService) Ready() Ready { return ready("load", "") }

// Handle answers stats, reset, start, progress, stop, result, records and
// remove, each with the service's clock.
func (s *LoadService) Handle(ctx context.Context, r Request) Response {
	resp := s.handle(ctx, r)
	resp.Now = time.Now()
	return resp
}

func (s *LoadService) handle(ctx context.Context, r Request) Response {
	switch r.Op {
	case OpStats:
		return Response{OK: true, Process: s.cpu.stats()}
	case OpReset:
		s.cpu.reset()
		return Response{OK: true}
	case OpStart:
		if err := s.start(ctx, r.Name, r.Load); err != nil {
			return failed(err)
		}
		return s.state(r.Name, false)
	case OpProgress:
		return s.state(r.Name, false)
	case OpStop:
		l, err := s.load(r.Name)
		if err != nil {
			return failed(err)
		}
		for _, st := range l.streams {
			st.gen.Stop()
		}
		return s.state(r.Name, false)
	case OpResult:
		return s.state(r.Name, true)
	case OpRecords:
		return s.records(r.Name, r.Stream, r.Block)
	case OpRemove:
		return s.remove(r.Name)
	default:
		return unknown(r)
	}
}

// load returns the load name.
func (s *LoadService) load(name string) (*serviceLoad, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.loads[name]
	if !ok {
		return nil, fmt.Errorf("lab: no load %q", name)
	}
	return l, nil
}

// start starts the load name: a target and a generator per stream.
func (s *LoadService) start(ctx context.Context, name string, ls *LoadStart) error {
	switch {
	case name == "":
		return errors.New("lab: a load needs a name")
	case ls == nil || len(ls.Streams) == 0:
		return fmt.Errorf("lab: load %s: no stream", name)
	case ls.Duration <= 0:
		return fmt.Errorf("lab: load %s: duration %v", name, ls.Duration)
	}
	s.mu.Lock()
	_, dup := s.loads[name]
	s.mu.Unlock()
	if dup {
		return fmt.Errorf("lab: load %q exists", name)
	}
	l := &serviceLoad{}
	closeAll := func() {
		for _, st := range l.streams {
			if st.gen != nil {
				st.gen.Stop()
				_, _ = st.gen.Wait()
			}
			_ = st.tgt.Close()
		}
	}
	for _, spec := range ls.Streams {
		tgt, err := newStreamTarget(spec) //nolint:contextcheck // NewHTTP only checks the URL with a request it never sends; the generator takes the run's context
		if err != nil {
			closeAll()
			return fmt.Errorf("lab: load %s: stream %s: %w", name, spec.Name, err)
		}
		st := &serviceStream{spec: spec, tgt: tgt}
		if !ls.NoRecords {
			st.rec = NewRecordEncoder()
		}
		l.streams = append(l.streams, st)
	}
	for _, st := range l.streams {
		c := loadgen.Config{Rate: st.spec.Rate, Warmup: ls.Warmup, Settle: ls.Settle.settle(), Duration: ls.Duration}
		if rec := st.rec; rec != nil {
			c.OnResult = func(smp loadgen.Sample) { rec.Add(RecordOf(smp)) }
		}
		gen, err := loadgen.Start(ctx, st.tgt, c)
		if err != nil {
			closeAll()
			return fmt.Errorf("lab: load %s: stream %s: %w", name, st.spec.Name, err)
		}
		st.gen = gen
	}
	s.mu.Lock()
	s.loads[name] = l
	s.mu.Unlock()
	return nil
}

// newStreamTarget returns the HTTP target of a stream.
func newStreamTarget(spec LoadStream) (*loadgen.HTTPTarget, error) {
	if !(spec.Rate > 0) {
		return nil, fmt.Errorf("rate %v", spec.Rate)
	}
	hc := loadgen.HTTPConfig{
		URL: spec.URL, Method: spec.Method, Body: spec.Body, Connections: spec.Connections,
		RoundRobin: spec.RoundRobin, DisableKeepAlives: spec.DisableKeepAlives, KeyHeader: spec.KeyHeader,
	}
	switch spec.Protocol {
	case "http1", "":
		hc.Protocol = loadgen.HTTP1
	case "h2c":
		hc.Protocol = loadgen.H2C
	case "h2":
		hc.Protocol = loadgen.H2
	default:
		return nil, fmt.Errorf("unknown protocol %q", spec.Protocol)
	}
	if spec.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(spec.CA)) {
			return nil, errors.New("the CA bundle holds no certificate")
		}
		hc.TLS = &tls.Config{RootCAs: pool, ServerName: spec.ServerName, MinVersion: tls.VersionTLS12}
	}
	if (spec.Keys == nil) != (spec.KeyHeader == "") {
		return nil, errors.New("a key header and its keys go together")
	}
	if spec.Keys != nil {
		src, err := spec.Keys.Source()
		if err != nil {
			return nil, err
		}
		hc.Keys = src
	}
	return loadgen.NewHTTP(hc)
}

// running reports whether a stream of l has a request that has not ended.
func (l *serviceLoad) running() bool {
	for _, st := range l.streams {
		select {
		case <-st.gen.Done():
		default:
			return true
		}
	}
	return false
}

// state answers the load name's progress, with each stream's result when
// results is set, which needs the load to have ended.
func (s *LoadService) state(name string, results bool) Response {
	l, err := s.load(name)
	if err != nil {
		return failed(err)
	}
	ls := &LoadState{Name: name, Running: l.running()}
	if results && ls.Running {
		return failed(fmt.Errorf("lab: load %s runs", name))
	}
	for _, st := range l.streams {
		p := st.gen.Progress()
		ss := StreamState{Start: st.gen.StartTime(), MeasureStart: p.MeasureStart, Settled: p.Settled, Next: p.Next}
		select {
		case <-st.gen.Done():
			ss.Done = true
			res, err := st.gen.Wait()
			if err != nil {
				ss.Error = err.Error()
			}
			if results && res != nil {
				ss.Result = ResultDocOf(res)
			}
		default:
		}
		if st.rec != nil {
			ss.Records, ss.Blocks = st.rec.Count(), st.rec.Blocks()
		}
		ls.Streams = append(ls.Streams, ss)
	}
	return Response{OK: true, LoadState: ls}
}

// records answers one block of a stream's records, once no load runs.
func (s *LoadService) records(name string, stream, block int) Response {
	s.mu.Lock()
	loads := maps.Clone(s.loads)
	s.mu.Unlock()
	l, ok := loads[name]
	switch {
	case !ok:
		return failed(fmt.Errorf("lab: no load %q", name))
	case stream < 0 || stream >= len(l.streams):
		return failed(fmt.Errorf("lab: load %s has no stream %d", name, stream))
	case l.streams[stream].rec == nil:
		return failed(fmt.Errorf("lab: load %s keeps no records", name))
	}
	for _, k := range slices.Sorted(maps.Keys(loads)) {
		if loads[k].running() {
			return failed(fmt.Errorf("lab: load %s runs, and the records return only once every load has ended", k))
		}
	}
	rec := l.streams[stream].rec
	if rec.Blocks() == 0 && block == 0 {
		// A stream that ended without a request has one empty block.
		return Response{OK: true, Records: &RecordBlock{}}
	}
	data, err := rec.Block(block)
	if err != nil {
		return failed(err)
	}
	return Response{OK: true, Records: &RecordBlock{Data: data, Blocks: rec.Blocks()}}
}

// remove drops an ended load and its records.
func (s *LoadService) remove(name string) Response {
	l, err := s.load(name)
	if err != nil {
		return failed(err)
	}
	if l.running() {
		return failed(fmt.Errorf("lab: load %s runs", name))
	}
	s.mu.Lock()
	delete(s.loads, name)
	s.mu.Unlock()
	for _, st := range l.streams {
		_ = st.tgt.Close()
	}
	return Response{OK: true}
}

// Close stops every load and waits for its streams.
func (s *LoadService) Close() error {
	s.mu.Lock()
	loads := s.loads
	s.loads = map[string]*serviceLoad{}
	s.mu.Unlock()
	for _, l := range loads {
		for _, st := range l.streams {
			st.gen.Stop()
		}
	}
	for _, l := range loads {
		for _, st := range l.streams {
			_, _ = st.gen.Wait()
			_ = st.tgt.Close()
		}
	}
	return nil
}
