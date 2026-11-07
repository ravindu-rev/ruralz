// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// componentRaft is the process-log component of hashicorp/raft's records.
const componentRaft = "controlstore.raft"

// newRaftLogger returns the hclog.Logger hashicorp/raft logs through: an
// intercepting logger whose own output is off and whose sink forwards
// every record to slog (ADR-0006 "Telemetry"), keeping hclog's caller-side
// level checks, so nothing reaches stdout outside the process log.
func newRaftLogger(l *slog.Logger) hclog.Logger {
	il := hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: componentRaft, Level: hclog.Off, Output: io.Discard})
	il.RegisterSink(&slogSink{l: l})
	return il
}

// slogSink is the hclog.SinkAdapter that forwards records to slog.
type slogSink struct{ l *slog.Logger }

// Accept forwards one record; hclog's trace level becomes slog debug.
func (s *slogSink) Accept(name string, level hclog.Level, msg string, args ...any) {
	lvl := slogLevel(level)
	ctx := context.Background()
	if !s.l.Enabled(ctx, lvl) {
		return
	}
	attrs := make([]slog.Attr, 0, len(args)/2+1)
	attrs = append(attrs, slog.String(catalog.KeyComponent, name))
	for i := 0; i+1 < len(args); i += 2 {
		attrs = append(attrs, slog.Any(logKey(fmt.Sprint(args[i])), logValue(args[i+1])))
	}
	if len(args)%2 == 1 {
		attrs = append(attrs, slog.Any("extra", logValue(args[len(args)-1])))
	}
	s.l.LogAttrs(ctx, lvl, msg, attrs...) //nolint:sloglint // forwards hashicorp/raft's own message verbatim (ADR-0006 "Telemetry").
}

// slogLevel maps an hclog level.
func slogLevel(l hclog.Level) slog.Level {
	switch l {
	case hclog.Trace, hclog.Debug:
		return slog.LevelDebug
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	case hclog.NoLevel, hclog.Info, hclog.Off:
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// logKey turns a hashicorp key such as "last-index" into snake_case, the
// process log's key style. A key the record itself writes (time, level,
// msg, source) or the forwarder sets (component) takes a raft_ prefix, so
// no record carries a key twice.
func logKey(k string) string {
	k = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, strings.ToLower(k))
	switch k {
	case slog.TimeKey, slog.LevelKey, slog.MessageKey, slog.SourceKey, catalog.KeyComponent:
		return "raft_" + k
	}
	return k
}

// logValue renders errors and values that format themselves.
func logValue(v any) any {
	switch x := v.(type) {
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	default:
		return v
	}
}

// MetricSink is the go-metrics sink hashicorp/raft reports through: it
// forwards raft's commit time to ruralz_control_raft_commit_duration_seconds
// and drops every other raft metric, so no go-metrics sink of its own,
// Prometheus or Circonus, is imported (ADR-0006 "Telemetry").
type MetricSink struct {
	commit emit.Histogram
}

// Compile-time check of the contract.
var _ metrics.MetricSink = (*MetricSink)(nil)

// NewMetricSink returns a sink recording into commit, the
// ruralz_control_raft_commit_duration_seconds handle (nanoseconds).
func NewMetricSink(commit emit.Histogram) *MetricSink { return &MetricSink{commit: commit} }

// raftCommitKey is the go-metrics key raft measures a commit under.
func raftCommitKey() []string { return []string{"raft", "commitTime"} }

// AddSample records a commit time, a float of milliseconds.
func (m *MetricSink) AddSample(key []string, val float32) {
	if m.commit == nil || !slices.Equal(key, raftCommitKey()) || val < 0 || math.IsNaN(float64(val)) {
		return
	}
	m.commit.Record(0, uint64(float64(val)*float64(time.Millisecond)))
}

// AddSampleWithLabels records like AddSample; raft's commit time has no
// labels.
func (m *MetricSink) AddSampleWithLabels(key []string, val float32, _ []metrics.Label) {
	m.AddSample(key, val)
}

// SetGauge drops raft's gauges.
func (*MetricSink) SetGauge([]string, float32) {}

// SetGaugeWithLabels drops raft's gauges.
func (*MetricSink) SetGaugeWithLabels([]string, float32, []metrics.Label) {}

// EmitKey drops raft's keys.
func (*MetricSink) EmitKey([]string, float32) {}

// IncrCounter drops raft's counters.
func (*MetricSink) IncrCounter([]string, float32) {}

// IncrCounterWithLabels drops raft's counters.
func (*MetricSink) IncrCounterWithLabels([]string, float32, []metrics.Label) {}

// InstallMetricSink makes sink the process-wide go-metrics sink raft
// reports to, with no service or host prefix, millisecond timers and no
// runtime collector. It changes a library global, so a binary calls it
// once at start, as it runs one replica.
func InstallMetricSink(sink *MetricSink) error {
	conf := metrics.DefaultConfig("")
	conf.EnableHostname = false
	conf.EnableHostnameLabel = false
	conf.EnableRuntimeMetrics = false
	conf.EnableTypePrefix = false
	conf.TimerGranularity = time.Millisecond
	if _, err := metrics.NewGlobal(conf, sink); err != nil {
		return fmt.Errorf("controlstore: install the raft metric sink: %w", err)
	}
	return nil
}
