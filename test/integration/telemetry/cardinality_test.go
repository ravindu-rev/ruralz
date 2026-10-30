// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Cardinality budget run (docs/architecture/10-observability.md
// "Cardinality budget", targets).
const (
	// hotReloads is how many Hot Reloads the run makes, each renaming
	// every Route.
	hotReloads = 1000
	// revisionRoutes is the Routes of each Revision. A Route holds 21
	// series, the five status classes of ruralz_http_requests_total and
	// the 16 of its ruralz_http_request_duration_seconds label set, so
	// the label sets of two old Revisions stay within the retiring
	// ceiling and those of three exceed it.
	revisionRoutes = 450
	// retiringCeiling is the Node-wide ceiling of retiring series.
	retiringCeiling = 25000
	// revisionInfoMax is the most ruralz_config_revision_info series: the
	// active Revision and the Last-Known-Good.
	revisionInfoMax = 2
	// sampleEvery is how many Hot Reloads pass between two scrapes
	// outside the pinning points.
	sampleEvery = 20
	// accumulated is how many requests the first Route of a pinned
	// Revision records before its label sets fold.
	accumulated = 3
	// overflow is the resource label of a folded record.
	overflow = "_overflow"
)

// pinningPoints are the active Revisions at which slow requests pin three
// successive Revisions.
func pinningPoints() map[int]bool { return map[int]bool{150: true, 500: true, 850: true} }

// revisionName is the Route name prefix of Revision i.
func revisionName(i int) string { return fmt.Sprintf("v%04d", i) }

// routeName is Route k of Revision i.
func routeName(i, k int) string { return fmt.Sprintf("%s-%04d", revisionName(i), k) }

// revisionBundle is Revision i: revisionRoutes Routes, all renamed from
// Revision i-1's, to the Upstream at upstream.
func revisionBundle(i int, upstream string) string {
	return gatewayDoc("") + upstreamDoc("u", upstream) + manyRoutes(revisionName(i), revisionRoutes, "")
}

// holdServer is the Upstream of the run: it answers 200 at once, except a
// request whose path ends in /hold, which it holds until the gate armed
// when it arrived is released.
type holdServer struct {
	srv     *http.Server
	addr    string
	arrived chan string
	mu      sync.Mutex
	gate    chan struct{}
}

// startHoldServer starts a holdServer on loopback until the test ends.
func startHoldServer(t *testing.T) *holdServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &holdServer{addr: ln.Addr().String(), arrived: make(chan string, 8), gate: make(chan struct{})}
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = h.srv.Close()
		<-done
	})
	return h
}

// ServeHTTP implements http.Handler.
func (h *holdServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/hold") {
		h.mu.Lock()
		gate := h.gate
		h.mu.Unlock()
		select {
		case h.arrived <- r.URL.Path:
		case <-r.Context().Done():
			return
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// arm makes a new gate for the requests that arrive from now on and
// returns its release, which the test's end also runs.
func (h *holdServer) arm(t *testing.T) (release func()) {
	gate := make(chan struct{})
	h.mu.Lock()
	h.gate = gate
	h.mu.Unlock()
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	return release
}

// heldRequest is a request the hold server holds, which pins the snapshot
// it started on until it ends.
type heldRequest struct {
	path string
	done chan error
}

// startHeld sends GET /<route>/hold and returns once the hold server holds
// it.
func startHeld(t *testing.T, top *topology.Topology, n *topology.Node, h *holdServer, route string) *heldRequest {
	t.Helper()
	r := &heldRequest{path: "/" + route + "/hold", done: make(chan error, 1)}
	client := top.HTTPClient()
	go func() {
		code, err := get(t.Context(), client, n.URL("web")+r.path, nil)
		if err == nil && code != http.StatusOK {
			err = fmt.Errorf("status %d", code)
		}
		r.done <- err
	}()
	select {
	case got := <-h.arrived:
		if got != r.path {
			t.Fatalf("the hold server holds %s, want %s", got, r.path)
		}
	case err := <-r.done:
		t.Fatalf("GET %s ended before the hold server held it: %v", r.path, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("the hold server did not see GET %s", r.path)
	}
	return r
}

// wait waits for the request's 200 once released.
func (r *heldRequest) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("GET %s = %v, want 200", r.path, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("GET %s did not end once released", r.path)
	}
}

// snapshotStates counts the Node's snapshots by state from
// /debug/snapshots.
func snapshotStates(t *testing.T, n *topology.Node) map[string]int {
	t.Helper()
	resp, err := n.Admin(t.Context(), "/debug/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /debug/snapshots = %d: %s", resp.Status, resp.Body)
	}
	var doc adminapi.Snapshots
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("/debug/snapshots: %v", err)
	}
	states := map[string]int{}
	for _, s := range doc.Snapshots {
		states[s.State]++
	}
	return states
}

// waitFreed waits until the active snapshot is the Node's only one, every
// older one freed.
func waitFreed(t *testing.T, n *topology.Node) {
	t.Helper()
	eventually(t, 30*time.Second, "every old snapshot freed", func() bool {
		s := snapshotStates(t, n)
		return len(s) == 1 && s[adminapi.StateActive] == 1
	})
}

// cardinality is what a scrape says about the series.
type cardinality struct {
	// live and retiring are ruralz_telemetry_series by state.
	live, retiring float64
	// folded is ruralz_telemetry_folded_label_sets over every instrument.
	folded float64
	// revisionInfo is the number of ruralz_config_revision_info series.
	revisionInfo int
}

func cardinalityOf(t *testing.T, set promtext.Set) cardinality {
	t.Helper()
	return cardinality{
		live:         value(t, set, catalog.TelemetrySeries, "state", "live"),
		retiring:     value(t, set, catalog.TelemetrySeries, "state", "retiring"),
		folded:       set.Sum(catalog.TelemetryFoldedLabelSets, nil),
		revisionInfo: len(set.Samples(catalog.ConfigRevisionInfo, nil)),
	}
}

// revisionSeries is the live series less the ruralz_config_revision_info
// ones, which count the active Revision and the Last-Known-Good, not label
// sets: the single-Revision count a Node returns to.
func (c cardinality) revisionSeries() float64 { return c.live - float64(c.revisionInfo) }

// check applies the bounds every scrape of the run must meet.
func (c cardinality) check(t *testing.T, at string) {
	t.Helper()
	if c.retiring > retiringCeiling {
		t.Errorf("%s: %v retiring series, above the %d ceiling", at, c.retiring, retiringCeiling)
	}
	if c.revisionInfo > revisionInfoMax {
		t.Errorf("%s: %d %s series, want at most %d", at, c.revisionInfo, catalog.ConfigRevisionInfo, revisionInfoMax)
	}
}

// routeSets counts the ruralz_http_request_duration_seconds label sets of
// Revision i's Routes a scrape exports.
func routeSets(set promtext.Set, i int) int {
	prefix := revisionName(i) + "-"
	count := 0
	for _, s := range set.Samples(catalog.HTTPRequestDurationSeconds+"_count", nil) {
		if strings.HasPrefix(s.Labels["route"], prefix) {
			count++
		}
	}
	return count
}

// overflowRecords returns the _overflow 2xx count of
// ruralz_http_requests_total and the _overflow count of
// ruralz_http_request_duration_seconds.
func overflowRecords(t *testing.T, set promtext.Set) (requests, durations float64) {
	t.Helper()
	h, err := set.Histogram(catalog.HTTPRequestDurationSeconds, promtext.L("route", overflow))
	if err != nil {
		t.Fatal(err)
	}
	return value(t, set, catalog.HTTPRequestsTotal, "route", overflow, "status_class", "2xx"), h.Count
}

// pinningPoint drives one pinning point from active Revision a: the first
// Route of a records requests, then slow requests pin a, a+1 and a+2 while
// Hot Reloads activate a+1 to a+3, so two retired Revisions and a closing
// one hold label sets at once and the oldest retiring sets fold. It checks
// the ceiling and the fold, ends the slow requests and checks the release.
func pinningPoint(t *testing.T, top *topology.Topology, n *topology.Node, h *holdServer, a int, single float64) {
	t.Helper()
	folding := routeName(a, 0)
	requests(t, top, n, "/"+folding, accumulated)
	before := scrape(t, top, n)
	if v := value(t, before, catalog.HTTPRequestsTotal, "route", folding, "status_class", "2xx"); v != accumulated {
		t.Fatalf("Revision %d: %s 2xx = %v, want %d", a, folding, v, accumulated)
	}
	overflowRequests, overflowDurations := overflowRecords(t, before)

	release := h.arm(t)
	held := []*heldRequest{startHeld(t, top, n, h, folding)}
	for i := a + 1; i <= a+3; i++ {
		reload(t, n, revisionBundle(i, h.addr))
		if i < a+3 {
			held = append(held, startHeld(t, top, n, h, routeName(i, 0)))
		}
	}
	at := fmt.Sprintf("Revisions %d to %d pinned", a, a+2)
	if s := snapshotStates(t, n); s[adminapi.StateActive] != 1 || s[adminapi.StateRetired] != 2 || s[adminapi.StateClosing] != 1 || len(s) != 3 {
		t.Fatalf("%s: snapshots %v, want one active, two retired and one closing", at, s)
	}
	peak := scrape(t, top, n)
	c := cardinalityOf(t, peak)
	c.check(t, at)
	t.Logf("%s: %v live and %v retiring series, %v label sets folded", at, c.live, c.retiring, c.folded)
	for _, name := range []string{catalog.HTTPRequestsTotal, catalog.HTTPRequestDurationSeconds} {
		if v := value(t, peak, catalog.TelemetryFoldedLabelSets, "instrument", name); v <= 0 {
			t.Errorf("%s: %s folded label sets = %v, want the oldest retiring ones folded", at, name, v)
		}
	}
	// Only the oldest retiring sets fold: part of Revision a, none of the
	// retired Revisions a+1 and a+2, none of the live a+3.
	if got := routeSets(peak, a); got == 0 || got >= revisionRoutes {
		t.Errorf("%s: Revision %d exports %d of its %d Route label sets, want its oldest folded and the rest kept", at, a, got, revisionRoutes)
	}
	for i := a + 1; i <= a+3; i++ {
		if got := routeSets(peak, i); got != revisionRoutes {
			t.Errorf("%s: Revision %d exports %d of its %d Route label sets, want all", at, i, got, revisionRoutes)
		}
	}
	if peak.Has(catalog.HTTPRequestsTotal, promtext.L("route", folding)) {
		t.Errorf("%s: the folded %s still exports its series", at, folding)
	}
	// Folding carries no accumulated value: _overflow stays where it was.
	if r, d := overflowRecords(t, peak); r != overflowRequests || d != overflowDurations {
		t.Errorf("%s: _overflow records %v requests and %v durations, want %v and %v: the fold carried %s's accumulated value",
			at, r, d, overflowRequests, overflowDurations, folding)
	}
	// A live Route keeps recording on its own label set.
	live := routeName(a+3, revisionRoutes-1)
	requests(t, top, n, "/"+live, 1)

	release()
	for _, r := range held {
		r.wait(t)
	}
	waitFreed(t, n)
	after := scrape(t, top, n)
	at = fmt.Sprintf("Revisions %d to %d released", a, a+2)
	c = cardinalityOf(t, after)
	c.check(t, at)
	if c.retiring != 0 || c.folded != 0 || c.revisionSeries() != single {
		t.Errorf("%s: %v retiring series, %v folded label sets and %v Revision series, want 0, 0 and the single-Revision %v",
			at, c.retiring, c.folded, c.revisionSeries(), single)
	}
	// Only the slow request on the folded Route, which ended after the
	// fold, reached _overflow.
	if r, d := overflowRecords(t, after); r != overflowRequests+1 || d != overflowDurations+1 {
		t.Errorf("%s: _overflow records %v requests and %v durations, want %v and %v",
			at, r, d, overflowRequests+1, overflowDurations+1)
	}
	if v := value(t, after, catalog.HTTPRequestsTotal, "route", live, "status_class", "2xx"); v != 1 {
		t.Errorf("%s: the live %s 2xx = %v, want 1", at, live, v)
	}
}

// TestCardinalityBudget makes 1,000 Hot Reloads, each renaming every
// Route, and checks the retiring ceiling, folding and release of the
// Cardinality budget: retiring series stay within 25,000, only retiring
// sets fold, no _overflow series grows by a folded set's accumulated
// value, the series return to the single-Revision count once old
// snapshots are freed, and at most two ruralz_config_revision_info series
// show. Each activation is awaited before the next Revision is written,
// since the loader keeps only the latest pending one.
func TestCardinalityBudget(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	h := startHoldServer(t)
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(revisionBundle(0, h.addr))})
	first := cardinalityOf(t, scrape(t, top, n))
	first.check(t, "Revision 0")
	single := first.revisionSeries()
	if first.retiring != 0 || first.folded != 0 {
		t.Fatalf("Revision 0: %v retiring series and %v folded label sets, want none", first.retiring, first.folded)
	}
	pins := pinningPoints()
	start := time.Now()
	for active := 0; active < hotReloads; {
		if pins[active] && active+3 <= hotReloads {
			pinningPoint(t, top, n, h, active, single)
			active += 3
			continue
		}
		active++
		reload(t, n, revisionBundle(active, h.addr))
		if active%sampleEvery == 0 {
			c := cardinalityOf(t, scrape(t, top, n))
			at := fmt.Sprintf("Revision %d", active)
			c.check(t, at)
			// Unpinned old snapshots free at once: nothing folds.
			if c.folded != 0 {
				t.Errorf("%s: %v folded label sets with no Revision pinned, want none", at, c.folded)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	waitFreed(t, n)
	last := cardinalityOf(t, scrape(t, top, n))
	at := fmt.Sprintf("Revision %d", hotReloads)
	last.check(t, at)
	if last.retiring != 0 || last.folded != 0 || last.revisionSeries() != single {
		t.Errorf("%s: %v retiring series, %v folded label sets and %v Revision series, want 0, 0 and the single-Revision %v",
			at, last.retiring, last.folded, last.revisionSeries(), single)
	}
	t.Logf("%d Hot Reloads in %v", hotReloads, time.Since(start).Round(time.Second))
}
