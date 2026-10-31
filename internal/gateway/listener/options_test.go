// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestAsteriskOptionsReachesHandler checks that "OPTIONS *" reaches the
// handler: the servers set DisableGeneralOptionsHandler, so net/http never
// answers it 200 itself (docs/architecture/07-multi-protocol.md "HTTP
// intermediary behavior").
func TestAsteriskOptionsReachesHandler(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	hs := newHarness(t, func(o *Options) { o.Handler = h })
	hs.activate(t, snap(httpListener("api", 0)), nil)
	c := dialRaw(t, hs.addr(t, "api"), nil)
	if _, err := io.WriteString(c, "OPTIONS * HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("OPTIONS * status = %d, want the handler's 404", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "OPTIONS *" {
		t.Errorf("handler saw %q, want [OPTIONS *]", got)
	}
	for _, p := range []v1alpha1.ListenerProtocol{v1alpha1.ListenerProtocolHTTP, v1alpha1.ListenerProtocolHTTPS} {
		if srv := newHTTPServer(p, h, nil); !srv.DisableGeneralOptionsHandler {
			t.Errorf("a %s listener's Server answers OPTIONS * itself", p)
		}
	}
}

// TestConnMetricsAndRequestSequence checks the request handler's view of
// a connection: the listener handles of the snapshot published at accept,
// and a request sequence numbered from 0 on each connection.
func TestConnMetricsAndRequestSequence(t *testing.T) {
	type seen struct {
		seq     uint32
		metrics *emit.ListenerMetrics
	}
	var (
		mu  sync.Mutex
		got []seen
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := ConnFrom(r.Context())
		mu.Lock()
		got = append(got, seen{seq: c.NextRequest(), metrics: c.Metrics()})
		mu.Unlock()
		_, _ = fmt.Fprint(w, "ok")
	})
	hs := newHarness(t, func(o *Options) { o.Handler = h })
	l := httpListener("api", 0)
	fm := newFakeMetrics()
	l.Metrics = fm.m
	hs.activate(t, snap(l), nil)
	for range 2 {
		c := dialRaw(t, hs.addr(t, "api"), nil)
		for range 3 {
			if r := c.mustRoundTrip(t); r.status != http.StatusOK || !strings.HasPrefix(r.body, "ok") {
				t.Fatalf("round trip = %d %q, want 200 ok", r.status, r.body)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []uint32{0, 1, 2, 0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("requests = %d, want %d", len(got), len(want))
	}
	for i, s := range got {
		if s.seq != want[i] {
			t.Errorf("request %d sequence = %d, want %d", i, s.seq, want[i])
		}
		if s.metrics != fm.m {
			t.Errorf("request %d metrics = %p, want the listener's handles %p", i, s.metrics, fm.m)
		}
	}
}
