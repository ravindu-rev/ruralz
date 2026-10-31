// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// newUpstreamServer starts a loopback Upstream running h until the test
// ends.
func newUpstreamServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// compileUpstreams compiles and activates the Upstream layer of one http
// Upstream named name whose static Endpoint is srv, on the fixture's clock
// and tracer, dialing loopback directly.
func compileUpstreams(t *testing.T, f *fixture, name string, srv *httptest.Server) *upstream.Set {
	t.Helper()
	var d net.Dialer
	m := upstream.NewManager(upstream.Options{Clock: f.clk, Dial: d.DialContext, Tracer: f.tracer})
	u := &v1alpha1.Upstream{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.UpstreamSpec{
			Protocol:  v1alpha1.UpstreamProtocolHTTP,
			Endpoints: []v1alpha1.Endpoint{{Address: srv.Listener.Addr().String()}},
		},
	}
	set, err := m.Compile(t.Context(), upstream.Input{Upstreams: []upstream.Config{{Upstream: u}}})
	if err != nil {
		t.Fatalf("compile the Upstream layer: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	set.Activate()
	return set
}
