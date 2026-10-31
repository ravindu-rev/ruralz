// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

// TestQuickstart runs the image on the quickstart Bundle, unchanged,
// inside the mock Upstream's network namespace: /readyz turns 200 over
// admin TLS, GET / answers the mock's line through the Route's Filter
// Chain, the container runs with the hardening and nothing more, and a
// stop drains the Node to exit 0 inside the 40 s grace period.
func TestQuickstart(t *testing.T) {
	s := requireSuite(t)
	ctx := t.Context()
	p := s.newProject(t, "quickstart", quickstartFile, nil)
	ca := newTestPKI(t)
	sec := newSecrets(t, "admin token")
	nodeFixture(t, p.run, ca, sec.get("admin token"), "127.0.0.1", "localhost")
	quickstartFixture(t, s.root, p.run)

	p.up(ctx, svcUpstream, svcRuralzd)
	admin := p.port(ctx, svcUpstream, portAdmin)
	client := ca.client("")
	waitReady(t, client, admin)

	ex := get(t, client, "http://"+p.port(ctx, svcUpstream, portHTTP)+"/", nil)
	checkQuickstartAnswer(t, ex)

	var log mock.RequestLog
	controlJSON(t, p.port(ctx, svcUpstream, portControl), "/requests", &log)
	proxied := false
	for _, r := range log.Requests {
		proxied = proxied || (r.Method == http.MethodGet && r.Path == "/" && strings.Contains(r.Header.Get("Via"), "ruralz"))
	}
	if !proxied {
		t.Errorf("the mock Upstream logged %+v, want the GET / the Node forwarded (Via: 1.1 ruralz)", log.Requests)
	}

	p.checkHardening(ctx, svcRuralzd)
	p.stop(ctx, svcRuralzd)
	sec.scan(t, "ruralzd logs", []byte(p.logs(ctx, svcRuralzd)))
}
