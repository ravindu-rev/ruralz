// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

// TestAirGappedStart boots the image on the quickstart Bundle on a network
// created internal: from a probe client container on that network,
// /readyz turns 200 and the proxied request answers 200, while the DNS
// sink, the Node's only resolver, records no lookup of a name outside the
// network.
func TestAirGappedStart(t *testing.T) {
	s := requireSuite(t)
	ctx := t.Context()
	p := s.newProject(t, "airgap", airgapFile, nil)
	ca := newTestPKI(t)
	sec := newSecrets(t, "admin token")
	nodeFixture(t, p.run, ca, sec.get("admin token"), svcUpstream)
	quickstartFixture(t, s.root, p.run)
	writeShared(t, filepath.Join(p.run, "probe", "ca.crt"), ca.ca.CertPEM)
	writeShared(t, filepath.Join(p.run, "resolv.conf"), []byte(resolvConf))

	p.up(ctx, svcUpstream, svcRuralzd)
	// -T: no pseudo-TTY, so the output is the probe's report alone.
	out, err := p.compose(ctx, "--profile", svcProbe, "run", "--rm", "--no-deps", "-T", svcProbe)
	if err != nil {
		t.Fatalf("the probe: %v\n%s", err, out)
	}
	var rep mock.ProbeReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("the probe's report %q: %v", out, err)
	}

	internal, err := s.dockerCmd(ctx, nil, "network", "inspect", "--format", "{{.Internal}}", p.name+"_airgap")
	if err != nil || strings.TrimSpace(string(internal)) != "true" {
		t.Errorf("network %s_airgap internal = %q (%v), want true", p.name, internal, err)
	}
	if rep.Ready == nil || rep.Ready.Status != http.StatusOK {
		t.Fatalf("/readyz from the probe = %+v, want 200", rep.Ready)
	}
	if rep.Request == nil {
		t.Fatal("the probe sent no request")
	}
	checkQuickstartAnswer(t, exchange{status: rep.Request.Status, header: rep.Request.Header, body: []byte(rep.Request.Body)})
	if rep.SinkCheck == nil || !rep.SinkCheck.NotFound {
		t.Fatalf("the probe's lookup through the sink = %+v, want an answered not-found", rep.SinkCheck)
	}
	if rep.DNS == nil {
		t.Fatalf("the probe read no DNS log: %s", rep.DNSError)
	}
	for _, problem := range dnsLogProblems(*rep.DNS, []string{svcUpstream, svcRuralzd, svcProbe}) {
		t.Error(problem)
	}

	p.checkHardening(ctx, svcRuralzd)
	p.stop(ctx, svcRuralzd)
	sec.scan(t, "ruralzd logs", []byte(p.logs(ctx, svcRuralzd)))
}
