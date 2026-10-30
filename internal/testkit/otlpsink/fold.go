// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// MaxFoldWindows bounds the windows a Sink folds into at once.
const MaxFoldWindows = 64

// Fold makes a Sink fold one duration attribute of one log event into
// latency histograms as the records arrive, instead of keeping them: the
// benchmark harness's internal view, where the OTLP sink folds each access
// record's gateway_duration into the run's internal histogram
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Measurement points"). A record folds into every window whose
// [Start, End) holds its timestamp (the request start, for access
// records); folded records are never recorded, so a long run keeps a
// histogram per window instead of millions of records. Other log records
// are recorded as usual.
type Fold struct {
	// Event is the event name of the folded records; the access log's is
	// "ruralz.access".
	Event string
	// Attribute is the attribute holding the duration in seconds, a
	// double or an integer; the access log's is "gateway_duration".
	Attribute string
}

// FoldStats counts the records of the folded event.
type FoldStats struct {
	// Records counts the folded event's records received.
	Records int64
	// Missing counts those without a usable Attribute (absent, not a
	// number, or not finite), such as an access record whose
	// gateway_duration a clock anomaly skipped.
	Missing int64
	// Outside counts those whose timestamp lies in no window.
	Outside int64
	// Latest is the latest record timestamp received.
	Latest time.Time
}

// WindowResult is what one window folded.
type WindowResult struct {
	// Start and End bound the window by record timestamp.
	Start, End time.Time
	// Histogram holds the folded durations (a copy).
	Histogram *loadgen.Histogram
	// Count is the records folded into Histogram; Missing those in the
	// window without a usable Attribute.
	Count, Missing int64
}

// errNoFold reports a window operation on a Sink configured without Fold.
var errNoFold = errors.New("otlpsink: the sink folds no attribute (Config.Fold is nil)")

// window is one registered fold window.
type window struct {
	start, end time.Time
	h          *loadgen.Histogram
	count      int64
	missing    int64
}

// folder folds the configured event's records into the windows.
type folder struct {
	cfg Fold

	mu      sync.Mutex
	windows map[string]*window
	stats   FoldStats
	notify  chan struct{} // closed and replaced when Latest moves
}

func newFolder(c Fold) (*folder, error) {
	if c.Event == "" || c.Attribute == "" {
		return nil, errors.New("otlpsink: Fold needs an Event and an Attribute")
	}
	return &folder{cfg: c, windows: map[string]*window{}, notify: make(chan struct{})}, nil
}

// fold folds the matching records of req and returns the request without
// them; nil when nothing else remains.
func (f *folder) fold(req *collogs.ExportLogsServiceRequest) *collogs.ExportLogsServiceRequest {
	var kept []*logspb.ResourceLogs
	folded := false
	f.mu.Lock()
	latest := f.stats.Latest
	for _, rl := range req.GetResourceLogs() {
		var scopes []*logspb.ScopeLogs
		for _, sl := range rl.GetScopeLogs() {
			var records []*logspb.LogRecord
			for _, lr := range sl.GetLogRecords() {
				if lr.GetEventName() != f.cfg.Event {
					records = append(records, lr)
					continue
				}
				folded = true
				f.foldLocked(lr)
			}
			if len(records) > 0 {
				scopes = append(scopes, &logspb.ScopeLogs{Scope: sl.GetScope(), LogRecords: records, SchemaUrl: sl.GetSchemaUrl()})
			}
		}
		if len(scopes) > 0 {
			kept = append(kept, &logspb.ResourceLogs{Resource: rl.GetResource(), ScopeLogs: scopes, SchemaUrl: rl.GetSchemaUrl()})
		}
	}
	if f.stats.Latest.After(latest) {
		close(f.notify)
		f.notify = make(chan struct{})
	}
	f.mu.Unlock()
	if !folded {
		return req
	}
	if len(kept) == 0 {
		return nil
	}
	return &collogs.ExportLogsServiceRequest{ResourceLogs: kept}
}

// foldLocked folds one record; f.mu is held.
func (f *folder) foldLocked(lr *logspb.LogRecord) {
	f.stats.Records++
	ts := unixNano(lr.GetTimeUnixNano())
	if ts.After(f.stats.Latest) {
		f.stats.Latest = ts
	}
	d, ok := durationAttr(lr, f.cfg.Attribute)
	if !ok {
		f.stats.Missing++
	}
	in := false
	for _, w := range f.windows {
		if ts.Before(w.start) || !ts.Before(w.end) {
			continue
		}
		in = true
		if !ok {
			w.missing++
			continue
		}
		w.h.Record(d)
		w.count++
	}
	if !in {
		f.stats.Outside++
	}
}

// durationAttr reads a duration in seconds from attribute key.
func durationAttr(lr *logspb.LogRecord, key string) (time.Duration, bool) {
	v, ok := Attr(lr.GetAttributes(), key)
	if !ok {
		return 0, false
	}
	var sec float64
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		sec = x.DoubleValue
	case *commonpb.AnyValue_IntValue:
		sec = float64(x.IntValue)
	default:
		return 0, false
	}
	if math.IsNaN(sec) || math.IsInf(sec, 0) {
		return 0, false
	}
	return time.Duration(math.Round(sec * 1e9)), true
}

// AddWindow registers a window folding the records whose timestamp lies in
// [start, end). It fails without Config.Fold, for a name in use, an empty
// window or more than MaxFoldWindows windows.
func (s *Sink) AddWindow(name string, start, end time.Time) error {
	if s.fold == nil {
		return errNoFold
	}
	if !end.After(start) {
		return fmt.Errorf("otlpsink: window %q ends at %v, not after its start %v", name, end, start)
	}
	h, err := loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits)
	if err != nil {
		return err
	}
	f := s.fold
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.windows[name]; dup {
		return fmt.Errorf("otlpsink: window %q exists", name)
	}
	if len(f.windows) >= MaxFoldWindows {
		return fmt.Errorf("otlpsink: at most %d fold windows", MaxFoldWindows)
	}
	f.windows[name] = &window{start: start, end: end, h: h}
	return nil
}

// Window returns what window name folded so far.
func (s *Sink) Window(name string) (WindowResult, error) {
	if s.fold == nil {
		return WindowResult{}, errNoFold
	}
	f := s.fold
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.windows[name]
	if !ok {
		return WindowResult{}, fmt.Errorf("otlpsink: no window %q", name)
	}
	return WindowResult{Start: w.start, End: w.end, Histogram: w.h.Clone(), Count: w.count, Missing: w.missing}, nil
}

// RemoveWindow stops folding into window name; removing an unknown name
// does nothing.
func (s *Sink) RemoveWindow(name string) {
	if s.fold == nil {
		return
	}
	s.fold.mu.Lock()
	delete(s.fold.windows, name)
	s.fold.mu.Unlock()
}

// FoldStats returns the folded event's counters; zero without Config.Fold.
func (s *Sink) FoldStats() FoldStats {
	if s.fold == nil {
		return FoldStats{}
	}
	s.fold.mu.Lock()
	defer s.fold.mu.Unlock()
	return s.fold.stats
}

// WaitFolded waits until a folded record with a timestamp at or after t
// arrived, so a window ending at t holds every record exported in
// timestamp order, or until ctx ends.
func (s *Sink) WaitFolded(ctx context.Context, t time.Time) error {
	if s.fold == nil {
		return errNoFold
	}
	for {
		s.fold.mu.Lock()
		latest, ch := s.fold.stats.Latest, s.fold.notify
		s.fold.mu.Unlock()
		if !latest.Before(t) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("otlpsink: no folded record at or after %v (latest %v): %w", t, latest, ctx.Err())
		case <-ch:
		case <-s.closed:
			return ErrClosed
		}
	}
}
