// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// metricsOf returns the listener's metric handles, or handles that record
// nothing for a listener compiled without any.
func metricsOf(l *snapshot.Listener) *emit.ListenerMetrics {
	if l.Metrics != nil {
		return l.Metrics
	}
	m := &emit.ListenerMetrics{
		Requests:        nopRequests{},
		GatewayDuration: nopHistogram{},
		RequestBody:     nopHistogram{},
		ResponseBody:    nopHistogram{},
		Active:          nopGauge{},
		TLSHandshake:    nopHistogram{},
	}
	for p := range m.OpenConns {
		m.OpenConns[p] = nopGauge{}
		for r := range m.Conns[p] {
			m.Conns[p][r] = nopCounter{}
		}
	}
	return m
}

type (
	nopRequests  struct{}
	nopHistogram struct{}
	nopGauge     struct{}
	nopCounter   struct{}
)

func (nopRequests) Inc(emit.Stripe, int, int, int)                     {}
func (nopHistogram) Record(emit.Stripe, uint64)                        {}
func (nopHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}
func (nopGauge) Add(emit.Stripe, int64)                                {}
func (nopGauge) Set(int64)                                             {}
func (nopCounter) Add(emit.Stripe, uint64)                             {}
