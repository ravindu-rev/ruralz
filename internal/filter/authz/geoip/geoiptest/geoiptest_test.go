// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoiptest

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

// open writes d and opens it with the MaxMind DB reader.
func open(t *testing.T, d *Database) *maxminddb.Reader {
	t.Helper()
	b, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	r, err := maxminddb.OpenBytes(b)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	if err := r.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return r
}

// lookup returns the record of addr, or nil when it has none.
func lookup(t *testing.T, r *maxminddb.Reader, addr string) map[string]any {
	t.Helper()
	res := r.Lookup(netip.MustParseAddr(addr))
	if !res.Found() {
		if err := res.Err(); err != nil {
			t.Fatalf("Lookup(%s): %v", addr, err)
		}
		return nil
	}
	var rec map[string]any
	if err := res.Decode(&rec); err != nil {
		t.Fatalf("Decode(%s): %v", addr, err)
	}
	return rec
}

// TestDatabaseReadsBack: every record size, both IP versions, the
// metadata and every value type read back through the reader as written;
// a more specific network splits a shorter one, and an address outside
// every network has no record.
func TestDatabaseReadsBack(t *testing.T) {
	rich := map[string]any{
		"s": "text", "b": []byte{1, 2}, "t": true, "f": false, "d": 2.5,
		"u16": uint16(7), "u32": uint32(1 << 20), "u64": uint64(1 << 40), "zero": uint64(0), "i32": int32(-3),
		"a": []any{"x", uint32(1)}, "m": map[string]any{"k": "v"}, "long": strings.Repeat("x", 300),
	}
	for _, size := range []int{24, 28, 32} {
		for _, v := range []int{4, 6} {
			t.Run(fmt.Sprintf("record %d, IPv%d", size, v), func(t *testing.T) {
				d := New(Options{IPVersion: v, RecordSize: size, DatabaseType: "Test-Type", BuildEpoch: 1700000000}).
					Insert("10.0.0.0/8", Country("GB")).
					Insert("10.1.0.0/16", Country("FR")).
					Insert("192.0.2.1", rich)
				if v == 6 {
					d.Insert("2001:db8::/32", Country("DE"))
				}
				r := open(t, d)
				m := r.Metadata
				if m.IPVersion != uint(v) || m.RecordSize != uint(size) || m.DatabaseType != "Test-Type" || m.BuildEpoch != 1700000000 ||
					m.BinaryFormatMajorVersion != 2 || m.Description["en"] == "" {
					t.Errorf("metadata = %+v", m)
				}
				for addr, want := range map[string]string{"10.200.0.1": "GB", "10.1.2.3": "FR", "10.0.255.255": "GB"} {
					rec := lookup(t, r, addr)
					if got := rec["country"].(map[string]any)["iso_code"]; got != want {
						t.Errorf("Lookup(%s) country = %v, want %s", addr, got, want)
					}
				}
				if rec := lookup(t, r, "192.0.2.2"); rec != nil {
					t.Errorf("Lookup(192.0.2.2) = %v, want no record", rec)
				}
				got := lookup(t, r, "192.0.2.1")
				want := map[string]any{
					"s": "text", "b": []byte{1, 2}, "t": true, "f": false, "d": 2.5,
					"u16": uint64(7), "u32": uint64(1 << 20), "u64": uint64(1 << 40), "zero": uint64(0), "i32": int32(-3),
					"a": []any{"x", uint64(1)}, "m": map[string]any{"k": "v"}, "long": strings.Repeat("x", 300),
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("rich record = %#v\nwant %#v", got, want)
				}
				if v == 6 {
					if rec := lookup(t, r, "2001:db8::1"); rec["country"].(map[string]any)["iso_code"] != "DE" {
						t.Errorf("IPv6 lookup = %v", rec)
					}
				}
			})
		}
	}
}

// TestIdenticalRecordsShareData: two networks with equal records point at
// one data section entry, as a MaxMind writer's do.
func TestIdenticalRecordsShareData(t *testing.T) {
	r := open(t, New(Options{}).Insert("10.0.0.0/8", Country("GB")).Insert("192.0.2.0/24", Country("GB")).Insert("198.51.100.0/24", Country("FR")))
	a, b, c := r.Lookup(netip.MustParseAddr("10.0.0.1")), r.Lookup(netip.MustParseAddr("192.0.2.1")), r.Lookup(netip.MustParseAddr("198.51.100.1"))
	if a.Offset() != b.Offset() || a.Offset() == c.Offset() {
		t.Errorf("offsets %d, %d, %d; want the first two equal and the third apart", a.Offset(), b.Offset(), c.Offset())
	}
}

// TestLargeSizes: sizes past 28 take one, two or three extra size bytes.
func TestLargeSizes(t *testing.T) {
	for _, n := range []int{28, 29, 284, 285, 65820, 65821, 70000} {
		r := open(t, New(Options{}).Insert("10.0.0.0/8", map[string]any{"v": strings.Repeat("y", n)}))
		if got := lookup(t, r, "10.0.0.1")["v"]; got != strings.Repeat("y", n) {
			t.Errorf("a %d-byte string reads back as %d bytes", n, len(fmt.Sprint(got)))
		}
	}
}

// TestRawAndMetadata: Raw is written as given, and Options.Metadata
// replaces metadata entries.
func TestRawAndMetadata(t *testing.T) {
	d := New(Options{Metadata: map[string]any{"database_type": "Override", "extra": "x"}}).
		Insert("10.0.0.0/8", Raw{0x42, 'h', 'i'}) // a 2-byte UTF-8 string
	r := open(t, d)
	if r.Metadata.DatabaseType != "Override" {
		t.Errorf("database_type = %q, want Override", r.Metadata.DatabaseType)
	}
	var s string
	if err := r.Lookup(netip.MustParseAddr("10.0.0.1")).Decode(&s); err != nil || s != "hi" {
		t.Errorf("Raw record = %q, %v; want hi", s, err)
	}
}

// TestErrors: options, networks and values the writer cannot write are
// errors of Bytes.
func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		d    *Database
		want string
	}{
		{"ip version", New(Options{IPVersion: 5}), "ip version 5"},
		{"record size", New(Options{RecordSize: 20}), "record size 20"},
		{"bad network", New(Options{}).Insert("10.0.0.0/33", Country("GB")), "geoiptest:"},
		{"bad address", New(Options{}).Insert("not-an-address", Country("GB")), "geoiptest:"},
		{"IPv6 in IPv4", New(Options{IPVersion: 4}).Insert("2001:db8::/32", Country("DE")), "IPv6 network"},
		{"value type", New(Options{}).Insert("10.0.0.0/8", map[string]any{"x": 1}), "unsupported value type int"},
		{"element type", New(Options{}).Insert("10.0.0.0/8", []any{struct{}{}}), "element 0"},
		{"first error kept", New(Options{}).Insert("bad", Country("GB")).Insert("10.0.0.0/8", Country("GB")), "geoiptest:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.d.Bytes()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Bytes error = %v, want one containing %q", err, tt.want)
			}
		})
	}
	if !errors.Is(func() error { _, err := encode(nil, math.Pi+0i); return err }(), errType) {
		t.Error("an unsupported value is not errType")
	}
}

// TestWholeSpaceAndWrite: a /0 network covers every address, and Write
// writes a file the reader opens.
func TestWholeSpaceAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.mmdb")
	Write(t, path, New(Options{IPVersion: 4}).Insert("0.0.0.0/0", Country("AQ")))
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, addr := range []string{"0.0.0.1", "203.0.113.9", "255.255.255.255"} {
		if rec := lookup(t, r, addr); rec["country"].(map[string]any)["iso_code"] != "AQ" {
			t.Errorf("Lookup(%s) = %v, want AQ", addr, rec)
		}
	}
}
