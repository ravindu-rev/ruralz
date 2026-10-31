// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fixedSource draws 2 every time: every weighted pick is the first
// candidate and every backoff zero, so the test needs no randomness.
type fixedSource struct{}

func (fixedSource) Uint64() uint64 { return 2 }

// seen is what a loopback Upstream received.
type seen struct {
	method, path, query, body string
}

// recorder is a loopback Upstream recording each request and answering
// with a JSON body.
type recorder struct {
	mu   sync.Mutex
	reqs []seen
	srv  *httptest.Server
}

// newRecorder starts a loopback server answering body.
func newRecorder(t *testing.T, body string) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, seen{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, string(b)})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=60")
		// The fake clock's time, so the response's age never reads the
		// wall clock.
		w.Header().Set("Date", epoch().Format(http.TimeFormat))
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// last returns the last request the server saw.
func (r *recorder) last() seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return seen{}
	}
	return r.reqs[len(r.reqs)-1]
}

// TestOverUpstreamLayer runs an aggregate over the real Upstream layer and
// loopback Upstreams: each step's request line carries its method, its
// computed path unchanged (an escaped slash kept) and its query or the
// client's, only a step with the incoming method sends the client body,
// and the bodies merge.
func TestOverUpstreamLayer(t *testing.T) {
	orders := newRecorder(t, `{"id":"a/b","total":3}`)
	audit := newRecorder(t, `{"logged":true}`)
	f := newFixture(t)
	var d net.Dialer
	m := upstream.NewManager(upstream.Options{Clock: f.clk, Rand: fixedSource{}, Dial: d.DialContext})
	ups := func(name, addr string) upstream.Config {
		return upstream.Config{Upstream: &v1alpha1.Upstream{
			Metadata: v1alpha1.ObjectMeta{Name: name},
			Spec:     v1alpha1.UpstreamSpec{Protocol: v1alpha1.UpstreamProtocolHTTP, Endpoints: []v1alpha1.Endpoint{{Address: addr}}},
		}}
	}
	set, err := m.Compile(t.Context(), upstream.Input{Upstreams: []upstream.Config{
		ups("orders", orders.srv.Listener.Addr().String()),
		ups("audit", audit.srv.Listener.Addr().String()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	set.Activate()
	order := v1alpha1.CompositionStep{Name: "order", Upstream: "orders", PathExpression: `"/orders/" + request.rawPathParams.id`, Group: "order"}
	log := v1alpha1.CompositionStep{Name: "audit", Upstream: "audit", Method: http.MethodGet, Path: "/audit?from=gateway"}
	comp, err := f.e.Compile(Input{
		Route:       "r",
		Composition: composition(v1alpha1.CompositionModeAggregate, order, log),
		Upstream: func(name string) (Leg, bool) {
			u, ok := set.Upstream(name)
			if !ok {
				return nil, false
			}
			return u, true
		},
		Builder: celtest.Compiler(t).NewBuilder(nil, expr.InputCaps{}),
		Limits:  f.limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	x := exchange(http.MethodPost, "/v1/orders/a%2Fb", "tag=1", expr.Param{Name: "id", Value: "a/b", Raw: "a%2Fb"})
	resp, err := comp.Forward(t.Context(), f.outbound(x, `{"note":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"order":{"id":"a/b","total":3},"logged":true}` {
		t.Errorf("merged body = %s", got)
	}
	if got, want := orders.last(), (seen{http.MethodPost, "/orders/a%2Fb", "tag=1", `{"note":"hi"}`}); got != want {
		t.Errorf("orders saw %+v, want %+v", got, want)
	}
	if got, want := audit.last(), (seen{http.MethodGet, "/audit", "from=gateway", ""}); got != want {
		t.Errorf("audit saw %+v, want %+v", got, want)
	}
	if got := resp.Header.Get("Cache-Control"); got != "max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
}
