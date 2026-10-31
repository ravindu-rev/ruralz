// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// FuzzAdmissionBound checks the global quota accuracy bound with a
// healthy State Store (docs/architecture/11-scalability-and-distributed-state.md
// "Consistency and accuracy bounds": 0% over-admission). Each pair of
// bytes is one step: a request of cost 1 to 8, served or refunded; two
// quota Policies of one request on one counter; or a clock advance of up
// to 255 s. In every 1-minute window the admitted units, less those
// refunded, never exceed the limit, every admission reports exactly the
// units left, a script denial happens only when the cost exceeds them,
// and a request charges its counter once.
func FuzzAdmissionBound(f *testing.F) {
	f.Add([]byte{0, 3, 0, 5, 0, 2, 1, 4, 0, 4, 2, 30, 0, 7, 3, 0, 0, 1})
	f.Add([]byte{0, 7, 0, 7, 1, 7, 0, 7, 2, 60, 0, 7, 0, 7})
	f.Add([]byte{3, 1, 3, 1, 3, 1, 3, 1, 3, 1, 3, 1, 2, 59, 3, 1})
	f.Add([]byte{1, 0, 1, 1, 1, 2, 1, 3, 0, 9, 0, 9, 0, 9, 2, 200, 0, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		const limit = 10
		fx := newFixture(t)
		cost := `int(request.headers["x-cost"])`
		gw, _ := fx.build(t, "quota-gateway", config("daily", cost))
		rt, _ := fx.build(t, "quota-route", config("daily", cost))
		acme := consumer("acme", windowQuota("daily", limit, time.Minute))
		used := map[time.Time]int64{}
		check := func(step int, o outcome, units int64) {
			ws := fx.clk.Now().Truncate(time.Minute)
			switch {
			case o.r.Outcome == filter.Continue:
				used[ws] += units
				if used[ws] > limit {
					t.Fatalf("step %d: %d units admitted in the window of %v, limit %d", step, used[ws], ws, limit)
				}
				if fs := o.fields(); len(fs) != 1 || fs[0].R != limit-used[ws] {
					t.Fatalf("step %d: fields %+v, want r=%d", step, fs, limit-used[ws])
				}
			case o.status() == http.StatusTooManyRequests:
				if o.called && units <= limit-used[ws] {
					t.Fatalf("step %d: the script denied cost %d with %d units left", step, units, limit-used[ws])
				}
			default:
				t.Fatalf("step %d: outcome %+v", step, o.r)
			}
		}
		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i]%4, ops[i+1]
			units := int64(arg%8) + 1
			v := vars(acme, "GET", strconv.FormatInt(units, 10))
			switch op {
			case 0, 1:
				final := served()
				if op == 1 {
					final = filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-001"}
				}
				o := fx.request(t, gw, v, final)
				check(i, o, units)
				for _, w := range fx.queue.writes {
					fx.mem.Write(context.Background(), []*statestore.Write{w})
					if w.Applied {
						used[w.Refund.WindowStart] -= w.Refund.Units
					}
				}
				fx.queue.writes, fx.queue.class = nil, nil
			case 2:
				a, b := exchange("quota-gateway", v), exchange("quota-route", v)
				a.Charged = &filtertest.Charges{}
				b.Charged = a.Charged
				before := fx.store.roundTrips()
				o := fx.decide(t, gw, a)
				check(i, o, units)
				if r := fx.decide(t, rt, b); o.r.Outcome == filter.Continue && (r.called || r.r.Outcome != filter.Continue) {
					t.Fatalf("step %d: the second Policy %+v, called %v; want the first's admission", i, r.r, r.called)
				}
				if n := fx.store.roundTrips() - before; n > 1 {
					t.Fatalf("step %d: one request made %d round trips", i, n)
				}
				gw.Finish(context.Background(), a)
				rt.Finish(context.Background(), b)
			default:
				fx.clk.Advance(time.Duration(arg) * time.Second)
			}
		}
	})
}
