// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Count returns the counter of one Quota window, for the Model's tests.
func (m *Model) Count(name string, window time.Duration, month bool, start time.Time, d statestore.Digest) int64 {
	return m.counts[modelWindow(name, window, month, start.UnixMilli(), d)]
}
