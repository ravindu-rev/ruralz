// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
)

// fixedSource draws lease tokens deterministically.
type fixedSource struct{}

func (fixedSource) Uint64() uint64 { return 7 }

// staleEntry is a revalidate.Entry whose saved request has no validators
// of its own; Refresh and Replace report what the revalidation decided.
type staleEntry struct {
	header http.Header
	result chan string
}

func (e *staleEntry) Request(ctx context.Context) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.example/v1/orders", nil)
	if err != nil {
		return nil, err
	}
	r.Header = http.Header{"Accept": {"application/json"}}
	return r, nil
}

func (e *staleEntry) Header() http.Header    { return e.header }
func (*staleEntry) Authorized() bool         { return false }
func (*staleEntry) Token() statestore.Token  { return statestore.Token{1} }
func (e *staleEntry) report(s string) []byte { e.result <- s; return []byte(s) }
func (e *staleEntry) Refresh(*revalidate.Refreshed) ([]byte, error) {
	return e.report("refreshed"), nil
}

func (e *staleEntry) Replace(*revalidate.Replacement) ([]byte, error) {
	return e.report("replaced"), nil
}

// TestRevalidatorReplays checks the Replayer as the Response Cache's
// revalidation workers use it: the worker replays the saved request with
// the stored validators through the Upstream's leg, the Upstream's 304
// refreshes the entry, and once the worker closed the response nothing is
// pinned or held.
func TestRevalidatorReplays(t *testing.T) {
	f := newFixture(t, 4)
	date := epoch().Format(http.TimeFormat)
	var mu sync.Mutex
	var validator string
	srv := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		validator = r.Header.Get("If-None-Match")
		mu.Unlock()
		w.Header().Set("Date", date)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Etag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	})
	snap := f.publish(handlertest.Spec{Legs: compileUpstreams(t, f, "orders", srv), Routes: []handlertest.Route{plainRoute(0)}})

	store, err := memory.New(t.Context(), statestore.Config{Driver: statestore.DriverMemory},
		statestore.Deps{Clock: f.clk, Logger: slog.New(slog.DiscardHandler)}, memory.Options{Shards: 1, MaxEntries: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	rv := revalidate.New(revalidate.Options{Clock: f.clk, Rand: fixedSource{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rv.Run(ctx) }()

	entry := &staleEntry{
		header: http.Header{"Etag": {`"v1"`}, "Cache-Control": {"max-age=60"}, "Date": {date}},
		result: make(chan string, 1),
	}
	if !rv.Enqueue(revalidate.Job{
		Key: statestore.CacheKey{URI: statestore.DigestOf("uri"), Partition: statestore.DigestOf("p")}, Variant: statestore.DigestOf("v"),
		Store: store, Timeout: 50 * time.Millisecond, Replayer: f.p, Route: "orders", Upstream: "orders", Entry: entry,
	}) {
		t.Fatal("the job was not queued")
	}
	if got := <-entry.result; got != "refreshed" {
		t.Errorf("revalidation = %s, want refreshed by the 304", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if validator != `"v1"` {
		t.Errorf("replayed If-None-Match = %q, want the stored validator", validator)
	}
	if pins(snap) != 0 || f.units.InUse() != 0 {
		t.Errorf("pins %d, units %d after the job, want 0 and 0", pins(snap), f.units.InUse())
	}
}
