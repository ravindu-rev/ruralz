// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Telemetry receives one plugin Policy's Plugin metrics
// (docs/architecture/05-wasm-plugin-system.md "Observability"): the call
// durations, the instance wait, failures by code, the pool's instance
// counts and the guest's events. Implementations are safe for concurrent
// use and never block.
type Telemetry interface {
	// Call records a Phase call: its whole time and the host time around
	// the guest call (ruralz_plugin_call_duration_seconds and
	// ruralz_plugin_call_overhead_seconds).
	Call(ph phase.Phase, total, overhead time.Duration)
	// Wait records an instance wait (ruralz_plugin_pool_wait_seconds).
	Wait(d time.Duration)
	// Failure counts a failure by RZ code (ruralz_plugin_failures_total).
	Failure(code string)
	// Instances sets the pool's idle and busy instance counts
	// (ruralz_plugin_pool_instances).
	Instances(idle, busy int)
	// GuestEvent adds n to the guest event name bound at index
	// (ruralz_plugin_guest_events_total).
	GuestEvent(index int, name string, n int64)
	// LogTruncated and LogDropped count guest log lines cut at the message
	// cap and dropped past the call or rate cap.
	LogTruncated()
	LogDropped()
}

// nopTelemetry records nothing.
type nopTelemetry struct{}

func (nopTelemetry) Call(phase.Phase, time.Duration, time.Duration) {}
func (nopTelemetry) Wait(time.Duration)                             {}
func (nopTelemetry) Failure(string)                                 {}
func (nopTelemetry) Instances(int, int)                             {}
func (nopTelemetry) GuestEvent(int, string, int64)                  {}
func (nopTelemetry) LogTruncated()                                  {}
func (nopTelemetry) LogDropped()                                    {}
