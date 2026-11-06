// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"maps"
	"sync"
)

// Stats aggregates the requests of a run.
type Stats struct {
	// Offered counts requests whose intended time came, Missed ones
	// included.
	Offered int64
	// Outcomes counts requests by Outcome.
	Outcomes [NumOutcomes]int64
	// Statuses counts responses by HTTP status.
	Statuses map[int]int64
	// Codes counts error responses by the RZ code of their problem
	// document.
	Codes map[string]int64
}

func (s *Stats) add(r Response) {
	s.Outcomes[r.Outcome]++
	if r.Status != 0 {
		if s.Statuses == nil {
			s.Statuses = map[int]int64{}
		}
		s.Statuses[r.Status]++
	}
	if r.Code != "" && r.Outcome != Success {
		if s.Codes == nil {
			s.Codes = map[string]int64{}
		}
		s.Codes[r.Code]++
	}
}

// recorder aggregates the requests of a run as they are dispatched and
// end; its methods are safe for concurrent use.
type recorder struct {
	mu    sync.Mutex
	stats Stats
}

// dispatched counts a request whose intended time came.
func (r *recorder) dispatched() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Offered++
}

// add records a finished request.
func (r *recorder) add(resp Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.add(resp)
}

// snapshot returns a copy of the counts.
func (r *recorder) snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Statuses = maps.Clone(s.Statuses)
	s.Codes = maps.Clone(s.Codes)
	return s
}
