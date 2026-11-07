// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/geoiptest"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// TestNewDatabaseReadsAtStart: NewDatabase reads the file at once, so the
// database serves before Run polls, logs one load naming the database
// and raises nothing.
func TestNewDatabaseReadsAtStart(t *testing.T) {
	fx := newFixture(t, worldBytes(t, geoiptest.Options{DatabaseType: "GeoLite2-Country", BuildEpoch: 1767225600}))
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Errorf("place(%s) = %q, want GB", addrGB, got)
	}
	if got := fx.placed(t, addrJP6); got != "JP" {
		t.Errorf("place(%s) = %q, want JP", addrJP6, got)
	}
	if fx.status.raised() || fx.status.changes != 0 {
		t.Errorf("geoip_database_stale changed %d times for a valid file", fx.status.changes)
	}
	if got := fx.logs.messages(slog.LevelInfo); !slices.Equal(got, []string{msgLoaded}) {
		t.Fatalf("logs = %q, want one %q", got, msgLoaded)
	}
	attrs := map[string]string{}
	fx.logs.recs[0].Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	if attrs["path"] != fx.path || attrs["database_type"] != "GeoLite2-Country" || attrs["build_time"] != "2026-01-01T00:00:00Z" {
		t.Errorf("load log attributes = %v", attrs)
	}
}

// TestUnusableFileAtStart: a file NewDatabase cannot use leaves the Node
// without a database, raises geoip_database_stale and logs the reason
// once; each kind of file a Node refuses is covered: one that cannot be
// read, one that is not a MaxMind DB, and one whose tree, records or
// metadata make it no country database.
func TestUnusableFileAtStart(t *testing.T) {
	valid := worldBytes(t, geoiptest.Options{})
	corruptTree := slices.Clone(valid)
	// Node 0's left record points past the data section.
	corruptTree[0], corruptTree[1], corruptTree[2] = 0xFF, 0xFF, 0xFF
	tests := []struct {
		name string
		data []byte // nil writes no file
		want string // in the logged error
	}{
		{"missing", nil, "GeoLite2-Country.mmdb"},
		{"empty file", []byte{}, "not a MaxMind DB"},
		{"not a MaxMind DB", []byte("GeoLite2-Country.csv,not a database\n"), "not a MaxMind DB"},
		{"truncated", valid[:len(valid)/2], "not a MaxMind DB"},
		{"corrupt search tree", corruptTree, "the search tree is invalid"},
		{"no networks", dbBytes(t, geoiptest.New(geoiptest.Options{})), "no network is placed in a country"},
		{"an ASN database", dbBytes(t, geoiptest.New(geoiptest.Options{DatabaseType: "GeoLite2-ASN"}).
			Insert("81.2.69.0/24", map[string]any{"autonomous_system_number": uint32(20712)})), "no network is placed in a country"},
		{"codes of another form only", dbBytes(t, geoiptest.New(geoiptest.Options{}).
			Insert("81.2.69.0/24", geoiptest.Country("gb")).Insert("89.160.20.0/24", geoiptest.Country("SWE"))), "no network is placed in a country"},
		{"iso_code not a string", dbBytes(t, geoiptest.New(geoiptest.Options{}).
			Insert("81.2.69.0/24", map[string]any{"country": map[string]any{"iso_code": uint32(826)}})), "a record is invalid"},
		{"record not a map", dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", "GB")), "a record is invalid"},
		{"ip_version 5", dbBytes(t, geoiptest.New(geoiptest.Options{Metadata: map[string]any{"ip_version": uint16(5)}}).
			Insert("81.2.69.0/24", geoiptest.Country("GB"))), "ip_version is neither 4 nor 6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, tt.data)
			if fx.db.current() != nil {
				t.Fatal("a database serves from an unusable file")
			}
			if !fx.status.raised() {
				t.Error("geoip_database_stale not raised")
			}
			if got := fx.logs.messages(slog.LevelWarn); !slices.Equal(got, []string{msgRefusedNew}) {
				t.Fatalf("warnings = %q, want one %q", got, msgRefusedNew)
			}
			var errText string
			fx.logs.recs[0].Attrs(func(a slog.Attr) bool {
				if a.Key == "error" {
					errText = a.Value.String()
				}
				return true
			})
			if !strings.Contains(errText, tt.want) {
				t.Errorf("logged error %q, want it to contain %q", errText, tt.want)
			}
			// The same file polled again is not read or reported again.
			fx.poll(t)
			fx.poll(t)
			if n := len(fx.logs.messages(slog.LevelWarn)); n != 1 {
				t.Errorf("%d warnings after two more polls, want 1", n)
			}
		})
	}
}

// TestDirectoryIsRefused: a directory at the path is not a regular file.
func TestDirectoryIsRefused(t *testing.T) {
	path := t.TempDir()
	st := &status{}
	db := NewDatabase(DatabaseOptions{Path: path, Clock: clocktest.New(t0), Status: st})
	if db.current() != nil || !st.raised() {
		t.Errorf("a directory: database %v, raised %v; want none and raised", db.current(), st.raised())
	}
}

// TestRecordSizes: databases with 24-, 28- and 32-bit search tree records
// load and place alike.
func TestRecordSizes(t *testing.T) {
	for _, size := range []int{24, 28, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			fx := newFixture(t, worldBytes(t, geoiptest.Options{RecordSize: size}))
			for addr, want := range map[string]string{addrGB: "GB", addrSE: "SE", addrJP6: "JP", addrDE6: "DE", addrPrivate: "", addrNoCode: ""} {
				if got := fx.placed(t, addr); got != want {
					t.Errorf("place(%s) = %q, want %q", addr, got, want)
				}
			}
		})
	}
}

// TestReplacedFileLoadsAtOnce: a new file renamed into place is read by
// the next poll, with no settle, and its database replaces the one in use.
func TestReplacedFileLoadsAtOnce(t *testing.T) {
	fx := newFixture(t, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB"))))
	replace(t, fx.path, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN"))), t0.Add(-30*time.Minute))
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("after one poll place(%s) = %q, want CN from the replaced file", addrGB, got)
	}
	if n := fx.logs.count(msgLoaded); n != 2 {
		t.Errorf("%d loads logged, want 2", n)
	}
}

// TestInPlaceChangeSettles: a file changed in place is read once a poll
// sees no further change, the settle debounce of the file-mode watcher.
func TestInPlaceChangeSettles(t *testing.T) {
	fx := newFixture(t, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB"))))
	cn := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN")))
	rewrite(t, fx.path, cn[:len(cn)/2], t0.Add(-50*time.Minute)) // a write under way
	fx.poll(t)
	rewrite(t, fx.path, cn, t0.Add(-40*time.Minute))
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Fatalf("a file still changing was read: place = %q, want GB", got)
	}
	if fx.status.raised() {
		t.Error("a file still changing raised geoip_database_stale")
	}
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("the settled file was not read: place = %q, want CN", got)
	}
}

// TestUnchangedFileIsNotRead: a poll that sees the file as it was last
// read, outside the racy window, does not read it; content rewritten with
// the same size and modification time is therefore not seen.
func TestUnchangedFileIsNotRead(t *testing.T) {
	gb := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB")))
	cn := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN")))
	if len(gb) != len(cn) {
		t.Fatalf("the two databases differ in size (%d, %d)", len(gb), len(cn))
	}
	fx := newFixture(t, gb)
	rewrite(t, fx.path, cn, t0.Add(-time.Hour)) // same size and time
	for range 3 {
		fx.poll(t)
	}
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Errorf("an unchanged stamp was read again: place = %q, want GB", got)
	}
}

// TestRacyReadIsCheckedAgain: a read within the racy window of the file's
// modification time is done again by later polls until a read falls
// outside it, so a same-size rewrite within the timestamp granularity is
// still seen; once outside, an unchanged stamp is not read.
func TestRacyReadIsCheckedAgain(t *testing.T) {
	gb := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB")))
	cn := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN")))
	fx := newFixture(t, nil)
	rewrite(t, fx.path, gb, t0)
	fx.poll(t) // the file appears: read at once, within the window
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Fatalf("place = %q, want GB", got)
	}
	rewrite(t, fx.path, cn, t0) // the same stamp
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Fatalf("a racy read was not checked again: place = %q, want CN", got)
	}
	fx.clk.Advance(racyWindow + time.Second)
	fx.poll(t) // read again, now outside the window
	rewrite(t, fx.path, gb, t0)
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("a read outside the window was done again: place = %q, want CN", got)
	}
	if fx.status.raised() {
		t.Error("geoip_database_stale raised")
	}
}

// TestChangeDuringReadIsDiscarded: a read during which the file changed
// is discarded: a file replaced meanwhile is read again at once, one
// changed in place once it settles.
func TestChangeDuringReadIsDiscarded(t *testing.T) {
	gb := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB")))
	cn := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN")))
	se := dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("SE")))
	t.Run("replaced", func(t *testing.T) {
		fx := newFixture(t, gb)
		replace(t, fx.path, cn, t0.Add(-50*time.Minute))
		once := true
		fx.db.afterRead = func() {
			if once {
				once = false
				replace(t, fx.path, se, t0.Add(-40*time.Minute))
			}
		}
		fx.poll(t)
		if got := fx.placed(t, addrGB); got != "SE" {
			t.Errorf("place = %q, want SE, the file replaced during the read, read again at once", got)
		}
		if n := fx.logs.count(msgChanged); n != 1 {
			t.Errorf("%d discarded reads logged, want 1", n)
		}
	})
	t.Run("in place", func(t *testing.T) {
		fx := newFixture(t, gb)
		replace(t, fx.path, cn, t0.Add(-50*time.Minute))
		once := true
		fx.db.afterRead = func() {
			if once {
				once = false
				rewrite(t, fx.path, se, t0.Add(-40*time.Minute))
			}
		}
		fx.poll(t)
		if got := fx.placed(t, addrGB); got != "GB" {
			t.Fatalf("place = %q, want GB: the read the change interrupted is discarded", got)
		}
		fx.poll(t)
		if got := fx.placed(t, addrGB); got != "SE" {
			t.Errorf("place = %q, want SE once the change settled", got)
		}
	})
}

// TestFailedReloadKeepsLastValid: a file that becomes unusable keeps the
// last valid database serving, raises geoip_database_stale and logs once
// per unusable file; a valid file clears the reason.
func TestFailedReloadKeepsLastValid(t *testing.T) {
	fx := newFixture(t, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB"))))
	f := fx.filter(t, codes("GB"), nil)
	replace(t, fx.path, []byte("garbage"), t0.Add(-50*time.Minute))
	fx.poll(t)
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Fatalf("place = %q after a failed reload, want the last valid GB", got)
	}
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, from(addrGB)); r.Outcome != filter.Continue {
		t.Errorf("Handle = %+v on the last valid database, want Continue", r)
	}
	if !fx.status.raised() {
		t.Fatal("geoip_database_stale not raised after a failed reload")
	}
	if got := fx.logs.messages(slog.LevelWarn); !slices.Equal(got, []string{msgRefused}) {
		t.Errorf("warnings = %q, want one %q", got, msgRefused)
	}
	replace(t, fx.path, []byte("other garbage"), t0.Add(-40*time.Minute))
	fx.poll(t)
	if n := len(fx.logs.messages(slog.LevelWarn)); n != 2 {
		t.Errorf("%d warnings after a second unusable file, want 2", n)
	}
	replace(t, fx.path, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN"))), t0.Add(-30*time.Minute))
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("place = %q, want CN from the valid file", got)
	}
	if fx.status.raised() {
		t.Error("geoip_database_stale still raised after a valid file")
	}
	if !slices.Equal(fx.status.sources, []string{degradedSource, degradedSource}) {
		t.Errorf("reason sources = %q, want one raise and one clear by %q", fx.status.sources, degradedSource)
	}
}

// TestRemovedFileKeepsLastValid: removing the file keeps the database in
// use and raises geoip_database_stale; the file back, with the same
// content, clears it without a new load.
func TestRemovedFileKeepsLastValid(t *testing.T) {
	data := worldBytes(t, geoiptest.Options{})
	fx := newFixture(t, data)
	if err := os.Remove(fx.path); err != nil {
		t.Fatal(err)
	}
	fx.poll(t)
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "GB" || !fx.status.raised() {
		t.Fatalf("after removal: place = %q, raised %v; want GB and raised", got, fx.status.raised())
	}
	if n := len(fx.logs.messages(slog.LevelWarn)); n != 1 {
		t.Errorf("%d warnings for one removal, want 1", n)
	}
	replace(t, fx.path, data, t0.Add(-30*time.Minute))
	fx.poll(t)
	if fx.status.raised() {
		t.Error("geoip_database_stale still raised with the file back")
	}
	if n := fx.logs.count(msgLoaded); n != 1 {
		t.Errorf("%d loads logged for the same content, want 1", n)
	}
}

// TestFileAppearsLater: a Node started before its file exists has no
// database until the file appears, which the next poll reads at once.
func TestFileAppearsLater(t *testing.T) {
	fx := newFixture(t, nil)
	f := fx.filter(t, codes("GB"), nil)
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, from(addrGB)); r.Code != "RZ-AUTH-015" {
		t.Errorf("Handle without a database = %+v, want RZ-AUTH-015", r)
	}
	replace(t, fx.path, worldBytes(t, geoiptest.Options{}), t0.Add(-time.Minute))
	fx.poll(t)
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, from(addrGB)); r.Outcome != filter.Continue {
		t.Errorf("Handle once the file appeared = %+v, want Continue", r)
	}
	if fx.status.raised() {
		t.Error("geoip_database_stale still raised")
	}
}

// TestDegradedWhileFilterLiveWithoutDatabase: without the setting nothing
// is degraded until an authz.geoip Filter is live, every live Filter then
// holding geoip_database_stale raised until the last one closes; Close is
// counted once. With a database, a live Filter raises nothing.
func TestDegradedWhileFilterLiveWithoutDatabase(t *testing.T) {
	st := &status{}
	db := NewDatabase(DatabaseOptions{Clock: clocktest.New(t0), Status: st})
	if st.raised() {
		t.Fatal("raised without a live Filter")
	}
	env := func() *Filter {
		return build(t, db, codes("GB"), nil, nil)
	}
	a, b := env(), env()
	if !st.raised() {
		t.Fatal("not raised with live Filters and no database")
	}
	_ = a.Close()
	_ = a.Close()
	if !st.raised() {
		t.Fatal("cleared while a Filter is still live")
	}
	_ = b.Close()
	if st.raised() {
		t.Error("still raised once no Filter is live")
	}
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	fx.filter(t, codes("GB"), nil)
	if fx.status.raised() {
		t.Error("a live Filter over a database raised geoip_database_stale")
	}
}

// TestRunPollsEveryInterval: Run polls once per interval on the clock,
// reads a replaced file then, and returns nil when its context ends;
// without a path it arms no timer and waits for the context.
func TestRunPollsEveryInterval(t *testing.T) {
	fx := newFixture(t, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB"))))
	polled := make(chan struct{}, 1)
	fx.db.polled = func() { polled <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- fx.db.Run(ctx) }()
	if err := fx.clk.BlockUntil(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	replace(t, fx.path, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN"))), t0.Add(-time.Minute))
	fx.clk.Advance(PollInterval)
	<-polled
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("place = %q after one interval, want CN", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}

	clk := clocktest.New(t0)
	none := NewDatabase(DatabaseOptions{Clock: clk})
	ctx, cancel = context.WithCancel(t.Context())
	go func() { done <- none.Run(ctx) }()
	cancel()
	if err := <-done; err != nil || clk.Pending() != 0 {
		t.Errorf("Run without a path = %v with %d timers, want nil and none", err, clk.Pending())
	}
	if none.Name() != ComponentName {
		t.Errorf("Name = %q", none.Name())
	}
}

// TestLoadStopsOnEndedContext: the walk of a large database stops when
// the poll's context ends during it, and the unfinished load is neither a
// failure nor a new database; the next poll loads the file.
func TestLoadStopsOnEndedContext(t *testing.T) {
	big := geoiptest.New(geoiptest.Options{})
	for i := range 2 * walkCheck {
		big.Insert(fmt.Sprintf("11.%d.%d.0/24", i/256, i%256), geoiptest.Country("GB"))
	}
	data := dbBytes(t, big)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openCountryDB(ended, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("openCountryDB = %v, want context.Canceled", err)
	}
	fx := newFixture(t, worldBytes(t, geoiptest.Options{}))
	replace(t, fx.path, data, t0.Add(-time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	fx.db.afterRead = cancel // the context ends once the file is read, during the walk
	fx.db.poll(ctx)
	if got := fx.placed(t, "11.0.1.1"); got != "" {
		t.Errorf("a canceled load replaced the database: place = %q", got)
	}
	if fx.status.raised() || len(fx.logs.messages(slog.LevelWarn)) != 0 {
		t.Errorf("a canceled load counted as a failure: raised %v, warnings %q", fx.status.raised(), fx.logs.messages(slog.LevelWarn))
	}
	fx.db.afterRead = nil
	fx.poll(t)
	if got := fx.placed(t, "11.0.1.1"); got != "GB" {
		t.Errorf("place = %q after the next poll, want GB", got)
	}
}

// TestPlaceDecodesUnwalkedRecord: a record the load did not walk to is
// decoded at lookup, so a lookup is right whatever the walk reached.
func TestPlaceDecodesUnwalkedRecord(t *testing.T) {
	db, err := openCountryDB(t.Context(), worldBytes(t, geoiptest.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	bare := &countryDB{reader: db.reader, buf: db.buf}
	for addr, want := range map[string]string{addrGB: "GB", addrJP6: "JP", addrNoCode: "", addrPrivate: ""} {
		got, err := bare.place(netip.MustParseAddr(addr))
		if err != nil || got.String() != want {
			t.Errorf("place(%s) without a table = %q, %v; want %q", addr, got.String(), err, want)
		}
	}
	if len(db.offsets) != 7 {
		t.Errorf("the walk found %d records, want 7, one per network", len(db.offsets))
	}
}

// TestCountry checks the code index: two upper-case ASCII letters and
// nothing else, round-tripping through String, and set membership.
func TestCountry(t *testing.T) {
	for _, code := range []string{"AA", "DE", "GB", "ZZ", "XK"} {
		c, ok := parseCountry(code)
		if !ok || c == noCountry || c.String() != code {
			t.Errorf("parseCountry(%q) = %d, %v (String %q)", code, c, ok, c.String())
		}
	}
	for _, code := range []string{"", "D", "de", "Gb", "DEU", "1A", "A1", "@A", "A[", "Ä"} {
		if c, ok := parseCountry(code); ok || c != noCountry {
			t.Errorf("parseCountry(%q) = %d, %v; want refused", code, c, ok)
		}
	}
	var s countrySet
	de, _ := parseCountry("DE")
	zz, _ := parseCountry("ZZ")
	aa, _ := parseCountry("AA")
	s.add(de)
	s.add(zz)
	if !s.has(de) || !s.has(zz) || s.has(aa) || s.has(noCountry) || s.has(numCountries+1) {
		t.Errorf("set membership wrong: %v", s)
	}
	if noCountry.String() != "" || country(numCountries+1).String() != "" {
		t.Error("an out-of-range country has a code")
	}
}
