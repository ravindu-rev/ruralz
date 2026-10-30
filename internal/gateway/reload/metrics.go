// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// nopMetrics returns configuration handles that record nothing.
func nopMetrics() emit.ConfigMetrics {
	return emit.ConfigMetrics{
		Activated:          nopCounter{},
		Rejected:           nopCodes{},
		ActivationDuration: func(string, string) emit.Histogram { return nopHistogram{} },
		RevisionInfo:       func(string, string) {},
	}
}

// fill copies the handles m sets over dst.
func fill(dst *emit.ConfigMetrics, m emit.ConfigMetrics) {
	if m.Activated != nil {
		dst.Activated = m.Activated
	}
	if m.Rejected != nil {
		dst.Rejected = m.Rejected
	}
	if m.ActivationDuration != nil {
		dst.ActivationDuration = m.ActivationDuration
	}
	if m.RevisionInfo != nil {
		dst.RevisionInfo = m.RevisionInfo
	}
}

type (
	nopCounter   struct{}
	nopCodes     struct{}
	nopHistogram struct{}
)

func (nopCounter) Add(emit.Stripe, uint64)                             {}
func (nopCodes) Inc(emit.Stripe, string)                               {}
func (nopHistogram) Record(emit.Stripe, uint64)                        {}
func (nopHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}
