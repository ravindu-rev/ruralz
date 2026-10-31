// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import "github.com/ravindu-rev/ruralz/internal/filter"

// charges is a request's charged-counter record (filter.Exchange.Charge,
// docs/architecture/09-traffic-management-and-resilience.md "Quotas": a
// request charges each Quota counter once, a later quota Policy takes the
// charging Policy's decision without a call, and only the charging Policy
// refunds). Each Charge lives at a fixed address until the request ends,
// so a Filter may keep the pointer from onRequestHeaders to Finish; reset
// keeps them for the next request, so a warm Request records without
// allocating.
type charges struct {
	// list holds the request's Charges in charge order; free the ones a
	// previous request used.
	list []*filter.Charge
	free []*filter.Charge
}

// charge returns the record of counter c and whether it is new, made
// for policy, the calling Policy's name.
func (r *charges) charge(c filter.Counter, policy string) (*filter.Charge, bool) {
	for _, ch := range r.list {
		if ch.Counter == c {
			return ch, false
		}
	}
	var ch *filter.Charge
	if n := len(r.free); n > 0 {
		ch, r.free = r.free[n-1], r.free[:n-1]
	} else {
		ch = new(filter.Charge)
	}
	*ch = filter.Charge{Counter: c, Policy: policy}
	r.list = append(r.list, ch)
	return ch, true
}

// reset clears the record for the next request, keeping its Charges.
func (r *charges) reset() {
	for _, ch := range r.list {
		*ch = filter.Charge{}
		r.free = append(r.free, ch)
	}
	clear(r.list)
	r.list = r.list[:0]
}

// charge records counter c for the calling Policy
// (filter.Exchange.Charge).
func (x *Request) charge(c filter.Counter) (*filter.Charge, bool) {
	name := ""
	if x.cur != nil {
		name = x.cur.Name
	}
	return x.charges.charge(c, name)
}
