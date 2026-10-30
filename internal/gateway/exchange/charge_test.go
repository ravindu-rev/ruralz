// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestChargeRecord covers the charged-counter record
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas"):
// the first Policy charging a counter makes the record, named after it;
// a later Policy charging the same counter gets that record and is not
// first; a counter differing in quota, window, month or key is another
// record; each record keeps its address while more are made.
func TestChargeRecord(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	gateway := testPolicy("quota-gateway", 0, phase.ClassAdmission, phase.ScopeGateway, 0)
	route := testPolicy("quota-route", 1, phase.ClassAdmission, phase.ScopeRoute, 0)
	monthly := filter.Counter{Quota: "monthly", Month: true, Key: statestore.DigestOf("acme")}

	x.Enter(gateway, phase.OnRequestHeaders, nil)
	rec, first := x.Exchange().Charge(monthly)
	if rec == nil || !first || rec.Policy != "quota-gateway" || rec.Counter != monthly || rec.Decided {
		t.Fatalf("first charge = %+v, %v; want a new record of quota-gateway", rec, first)
	}
	rec.Decided, rec.Result = true, 3

	x.Enter(route, phase.OnRequestHeaders, nil)
	again, first := x.Exchange().Charge(monthly)
	if again != rec || first || again.Policy != "quota-gateway" || !again.Decided || again.Result != 3 {
		t.Fatalf("second charge = %+v, %v; want quota-gateway's record and decision", again, first)
	}
	others := []filter.Counter{
		{Quota: "daily", Month: true, Key: monthly.Key},
		{Quota: "monthly", Window: 24 * time.Hour, Key: monthly.Key},
		{Quota: "monthly", Month: true, Key: statestore.DigestOf("beta")},
	}
	for i := range 20 {
		c := others[i%len(others)]
		c.Window += time.Duration(i/len(others)) * time.Second
		if r, first := x.Exchange().Charge(c); !first || r == rec || r.Policy != "quota-route" {
			t.Fatalf("counter %+v = %+v, %v; want a new record of quota-route", c, r, first)
		}
	}
	if r, first := x.Exchange().Charge(monthly); r != rec || first {
		t.Fatal("a record moved while more were made")
	}
}

// TestChargeRecordReset covers reuse: a request after Put starts with no
// record, and records reuse the earlier request's memory, so a warm
// Request charges without allocating.
func TestChargeRecordReset(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	p := testPolicy("quota", 0, phase.ClassAdmission, phase.ScopeRoute, 0)
	c := filter.Counter{Quota: "monthly", Month: true, Key: statestore.DigestOf("acme")}
	x.Enter(p, phase.OnRequestHeaders, nil)
	if _, first := x.Exchange().Charge(c); !first {
		t.Fatal("first charge is not first")
	}
	f.pool.Put(x)
	x = f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	x.Enter(p, phase.OnRequestHeaders, nil)
	rec, first := x.Exchange().Charge(c)
	if !first || rec.Decided {
		t.Fatalf("a reused Request kept its record: %+v, %v", rec, first)
	}
	allocs := testing.AllocsPerRun(100, func() {
		x.charges.reset()
		x.Exchange().Charge(c)
	})
	if allocs != 0 {
		t.Errorf("a warm charge allocates %v times", allocs)
	}
}

// TestChargeOnLeg covers a leg view: it keeps no record, so a Filter
// there charges as the first, and the request's record is untouched.
func TestChargeOnLeg(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("orders", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	l := x.Leg("orders", "")
	defer l.Release()
	rec, first := l.Exchange().Charge(filter.Counter{Quota: "q"})
	if rec != nil || !first || len(x.charges.list) != 0 {
		t.Fatalf("leg Charge = %+v, %v with %d request records; want nil, true and none", rec, first, len(x.charges.list))
	}
}

// TestFinalUpstreamReached covers SetFinal carrying whether an attempt
// reached an Upstream, which the quota refund reads.
func TestFinalUpstreamReached(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	x.SetFinal(filter.Final{Status: http.StatusOK, UpstreamReached: true}, 0)
	if !x.Exchange().Final().UpstreamReached {
		t.Error("Final() lost UpstreamReached")
	}
	x.SetFinal(filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-001"}, 0)
	if x.Exchange().Final().UpstreamReached {
		t.Error("Final() kept an earlier UpstreamReached")
	}
}
