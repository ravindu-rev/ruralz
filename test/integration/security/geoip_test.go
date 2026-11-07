// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/geoiptest"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// authz.geoip on a running Node (docs/architecture/08-security-and-identity.md
// "IP filtering and GeoIP"): the Node reads RURALZ_GEOIP_DATABASE_FILE at
// start and places source.ip, the client address a trusted proxy
// forwards, never the proxy itself; a file renamed into place loads at the
// next poll of the Node's clock; an unusable file keeps the last valid
// database serving and raises geoip_database_stale until a valid file
// loads; and a Node without the setting fails the Policy closed with 403
// RZ-AUTH-015, raising the reason while the Policy is active.

// Client addresses the test databases place.
const (
	geoClientA = "198.51.100.7"
	geoClientB = "203.0.113.7"
	geoUnknown = "192.0.2.1"
)

// geoBundle serves /geo through an authz.geoip Policy allowing GB, behind
// loopback as the trusted proxy.
func geoBundle(addr string) []byte {
	return bundle(
		gatewayDoc(webListener+"  trustedProxies: [\"127.0.0.0/8\", \"::1/128\"]\n"),
		upstreamDoc("backend", addr, ""),
		policyDoc("geo", "authz.geoip", "    allow: [GB]\n"),
		routeDoc("geo", "/geo", "backend", "geo"),
	)
}

// writeGeoDB renames into place at path a database placing geoClientA's
// network in a and geoClientB's in b.
func writeGeoDB(t *testing.T, path, a, b string) {
	t.Helper()
	tmp := path + ".tmp"
	geoiptest.Write(t, tmp, geoiptest.New(geoiptest.Options{DatabaseType: "GeoLite2-Country"}).
		Insert("198.51.100.0/24", geoiptest.Country(a)).Insert("203.0.113.0/24", geoiptest.Country(b)))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestGeoIP(t *testing.T) {
	top := newTopology(t, topology.Config{})
	mock := startMock(t, top, mockup.Config{LogSize: 256})
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	writeGeoDB(t, path, "GB", "US")
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle: geoBundle(mock.Addr()), Env: []string{setting.EnvGeoIPDatabaseFile + "=" + path},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, c := nodes[0], top.HTTPClient()
	from := func(addr string) reply {
		t.Helper()
		return get(t, c, n.URL("web")+"/geo", "X-Forwarded-For", addr)
	}

	t.Run("read at start", func(t *testing.T) {
		wantOK(t, "a GB client", from(geoClientA))
		wantCode(t, "a US client", from(geoClientB), http.StatusForbidden, "RZ-AUTH-014")
		wantCode(t, "a client the database cannot place", from(geoUnknown), http.StatusForbidden, "RZ-AUTH-014")
		wantCode(t, "the trusted proxy's own request, placed as loopback", get(t, c, n.URL("web")+"/geo"), http.StatusForbidden, "RZ-AUTH-014")
		if degraded(n, "geoip_database_stale") {
			t.Error("geoip_database_stale raised for a valid file")
		}
	})

	t.Run("replaced file", func(t *testing.T) {
		writeGeoDB(t, path, "US", "GB")
		advance(t, top, geoip.PollInterval)
		waitFor(t, "the replaced database", func() (bool, string) {
			r := from(geoClientB)
			return r.status == http.StatusOK && r.code == "", r.String()
		})
		wantCode(t, "a client now placed in US", from(geoClientA), http.StatusForbidden, "RZ-AUTH-014")
	})

	t.Run("unusable file keeps the last valid", func(t *testing.T) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte("not a MaxMind DB"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		advance(t, top, geoip.PollInterval)
		waitDegraded(t, n, "geoip_database_stale", true)
		wantOK(t, "the last valid database still serves", from(geoClientB))
		writeGeoDB(t, path, "GB", "GB")
		advance(t, top, geoip.PollInterval)
		waitDegraded(t, n, "geoip_database_stale", false)
		wantOK(t, "the new valid database", from(geoClientA))
	})

	t.Run("no setting", func(t *testing.T) {
		bare, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: geoBundle(mock.Addr())})
		if err != nil {
			t.Fatal(err)
		}
		r := get(t, c, bare[0].URL("web")+"/geo", "X-Forwarded-For", geoClientA)
		wantCode(t, "a Node without a database", r, http.StatusForbidden, "RZ-AUTH-015")
		waitDegraded(t, bare[0], "geoip_database_stale", true)
	})
}
