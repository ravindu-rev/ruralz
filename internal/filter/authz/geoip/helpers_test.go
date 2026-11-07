// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/geoiptest"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the fake time of every test; files the tests write carry
// modification times before it, outside the racy window, unless a test
// says otherwise.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// Client addresses of the test database (worldDB).
const (
	addrGB       = "81.2.69.142"
	addrSE       = "89.160.20.112"
	addrCN       = "175.16.199.1"
	addrUS       = "216.160.83.56"
	addrJP6      = "2001:218::1"
	addrDE6      = "2a02:ff80::1"
	addrNoCode   = "202.196.224.1" // a record without a country
	addrUnknown  = "8.8.8.8"       // no record
	addrPrivate  = "10.0.0.1"
	addrPrivate6 = "fd00::1"
	addrLoopback = "::1"
)

// world returns the test database: a few networks in GB, SE, CN, US, JP
// and DE, one whose record has a continent but no country, and nothing
// for private, loopback or other addresses.
func world(o geoiptest.Options) *geoiptest.Database {
	return geoiptest.New(o).
		Insert("81.2.69.0/24", geoiptest.Country("GB")).
		Insert("89.160.20.0/24", geoiptest.Country("SE")).
		Insert("175.16.199.0/24", geoiptest.Country("CN")).
		Insert("216.160.83.0/24", geoiptest.Country("US")).
		Insert("2001:218::/32", geoiptest.Country("JP")).
		Insert("2a02:ff80::/29", geoiptest.Country("DE")).
		Insert("202.196.224.0/20", map[string]any{"continent": map[string]any{"code": "AS"}})
}

// worldBytes returns world(o) as a file.
func worldBytes(t *testing.T, o geoiptest.Options) []byte {
	t.Helper()
	b, err := world(o).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dbBytes returns d as a file.
func dbBytes(t *testing.T, d *geoiptest.Database) []byte {
	t.Helper()
	b, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewrite writes data to path in place, keeping its identity, with
// modification time mtime.
func rewrite(t *testing.T, path string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// replace writes data beside path and renames it into place, a new file,
// with modification time mtime.
func replace(t *testing.T, path string, data []byte, mtime time.Time) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	rewrite(t, tmp, data, mtime)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// status records geoip_database_stale as the Node's status does: the
// reason is raised while its source holds it.
type status struct {
	mu      sync.Mutex
	on      bool
	sources []string
	changes int
}

func (s *status) SetDegraded(r catalog.Reason, source string, on bool) {
	if r != catalog.ReasonGeoIPDatabaseStale {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.on = on
	s.sources = append(s.sources, source)
	s.changes++
}

func (*status) SetCleartextHops(catalog.Hop, int) {}

// raised reports whether geoip_database_stale is raised.
func (s *status) raised() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

var _ emit.NodeStatus = (*status)(nil)

// logs records the messages of a logger at warn and above, and at info.
type logs struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logs) Enabled(context.Context, slog.Level) bool { return true }

func (l *logs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}

func (l *logs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logs) WithGroup(string) slog.Handler      { return l }

// messages returns the messages logged at level or above, in order.
func (l *logs) messages(level slog.Level) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if r.Level >= level {
			out = append(out, r.Message)
		}
	}
	return out
}

// count returns how often msg was logged.
func (l *logs) count(msg string) int {
	return len(slices.DeleteFunc(l.messages(slog.LevelDebug), func(m string) bool { return m != msg }))
}

// fixture is a Database on a fake clock over a file in a temporary
// directory, with its status and logs recorded.
type fixture struct {
	path   string
	clk    *clocktest.Fake
	status *status
	logs   *logs
	db     *Database
}

// newFixture writes data to the database file, unless data is nil, and
// opens the Database over it, which reads it at once.
func newFixture(t *testing.T, data []byte) *fixture {
	t.Helper()
	f := &fixture{
		path: filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb"), clk: clocktest.New(t0),
		status: &status{}, logs: &logs{},
	}
	if data != nil {
		rewrite(t, f.path, data, t0.Add(-time.Hour))
	}
	f.db = NewDatabase(DatabaseOptions{Path: f.path, Clock: f.clk, Logger: slog.New(f.logs), Status: f.status})
	return f
}

// poll runs one poll of the Database.
func (f *fixture) poll(t *testing.T) { f.db.poll(t.Context()) }

// filter builds an authz.geoip Filter over the fixture's database.
func (f *fixture) filter(t *testing.T, allow, deny []v1alpha1.CountryCode) *Filter {
	t.Helper()
	return build(t, f.db, allow, deny, nil)
}

// build compiles the lists over db, with decisions recorded in rec (nil
// records nothing); the Filter is closed when the test ends.
func build(t *testing.T, db *Database, allow, deny []v1alpha1.CountryCode, rec *decisions) *Filter {
	t.Helper()
	env := filter.BuildEnv{Config: &v1alpha1.AuthzGeoIPConfig{Allow: allow, Deny: deny}}
	if rec != nil {
		env.Metrics = &emit.PolicyMetrics{Auth: rec}
	}
	f, err := Factory{Database: db}.Build(t.Context(), env)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = f.(*Filter).Close() })
	return f.(*Filter)
}

// codes returns the codes as a list.
func codes(cs ...string) []v1alpha1.CountryCode {
	out := make([]v1alpha1.CountryCode, len(cs))
	for i, c := range cs {
		out[i] = v1alpha1.CountryCode(c)
	}
	return out
}

// from returns an Exchange whose client address is addr.
func from(addr string) *filtertest.Exchange {
	return &filtertest.Exchange{SourceV: filter.Source{IP: netip.MustParseAddr(addr)}}
}

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu     sync.Mutex
	allows int
	denies []string
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allows++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denies = append(d.denies, code)
}

// placed returns the country the fixture's database in use places addr
// in, failing the test without a database.
func (f *fixture) placed(t *testing.T, addr string) string {
	t.Helper()
	db := f.db.current()
	if db == nil {
		t.Fatal("no database in use")
	}
	c, err := db.place(netip.MustParseAddr(addr))
	if err != nil {
		t.Fatalf("place(%s): %v", addr, err)
	}
	return c.String()
}
