// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// country is an ISO 3166-1 alpha-2 code as an index: noCountry, or 1 plus
// 26 times the first letter's place in the alphabet plus the second's.
type country uint16

// noCountry is an address the database cannot place.
const noCountry country = 0

// numCountries is the number of two-letter codes.
const numCountries = 26 * 26

// parseCountry returns the country of a code of two upper-case ASCII
// letters, the CountryCode pattern; false for anything else.
func parseCountry(code string) (country, bool) {
	if len(code) != 2 {
		return noCountry, false
	}
	a, b := code[0], code[1]
	if a < 'A' || a > 'Z' || b < 'A' || b > 'Z' {
		return noCountry, false
	}
	return country(int(a-'A')*26+int(b-'A')) + 1, true
}

// String returns the code, "" for noCountry.
func (c country) String() string {
	if c == noCountry || c > numCountries {
		return ""
	}
	i := int(c) - 1
	return string([]byte{byte('A' + i/26), byte('A' + i%26)})
}

// countrySet is a set of countries, one bit per two-letter code.
type countrySet [(numCountries + 63) / 64]uint64

// add adds c, which is not noCountry.
func (s *countrySet) add(c country) {
	i := int(c) - 1
	s[i/64] |= 1 << (i % 64)
}

// has reports whether s holds c; never noCountry.
func (s *countrySet) has(c country) bool {
	if c == noCountry || c > numCountries {
		return false
	}
	i := int(c) - 1
	return s[i/64]&(1<<(i%64)) != 0
}

// Reasons a database file is refused; each wraps the cause.
var (
	// errFormat reports a file that is not a MaxMind DB.
	errFormat = errors.New("not a MaxMind DB")
	// errIPVersion reports metadata whose ip_version is neither 4 nor 6.
	errIPVersion = errors.New("ip_version is neither 4 nor 6")
	// errTree reports a search tree that does not walk.
	errTree = errors.New("the search tree is invalid")
	// errRecord reports a record whose country.iso_code is not a string,
	// or that cannot be decoded.
	errRecord = errors.New("a record is invalid")
	// errNoCountry reports a database in which no network has a country:
	// not a country database, such as an ASN one.
	errNoCountry = errors.New("no network is placed in a country (country.iso_code of two upper-case letters)")
)

// walkCheck is how many networks the load walks between checks of its
// context.
const walkCheck = 4096

// countryDB is one loaded database, immutable and safe for concurrent
// use: the reader over the file content it retains, and the country of
// every record its search tree reaches, by data section offset.
type countryDB struct {
	// reader retains buf; neither is ever modified, and the reader is
	// never closed, so a request holding db keeps both alive.
	reader *maxminddb.Reader
	buf    []byte
	// ipv4Only is set for an ip_version 4 database, which places no
	// IPv6 address.
	ipv4Only bool
	// offsets are the data section offsets of the records, ascending;
	// countries[i] is the country of offsets[i].
	offsets   []uintptr
	countries []country
	// typ and built are the metadata database_type and build_epoch.
	typ   string
	built time.Time
}

// openCountryDB parses buf as a MaxMind DB and walks its whole search
// tree, decoding the country of each record it reaches once: the file is
// refused when it is not a MaxMind DB, its ip_version is neither 4 nor 6,
// its tree does not walk, a record's country.iso_code is not a string, or
// no record holds a two-letter upper-case code. An iso_code of any other
// form places nothing. The returned database retains buf.
func openCountryDB(ctx context.Context, buf []byte) (*countryDB, error) {
	r, err := maxminddb.OpenBytes(buf)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFormat, err)
	}
	if v := r.Metadata.IPVersion; v != 4 && v != 6 {
		return nil, fmt.Errorf("%w: %d", errIPVersion, v)
	}
	seen := map[uintptr]country{}
	placed := false
	n := 0
	for res := range r.Networks() {
		if err := res.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", errTree, err)
		}
		if n++; n%walkCheck == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		off := res.Offset()
		if _, ok := seen[off]; ok {
			continue
		}
		c, err := decodeCountry(res)
		if err != nil {
			return nil, err
		}
		seen[off] = c
		placed = placed || c != noCountry
	}
	if !placed {
		return nil, errNoCountry
	}
	db := &countryDB{
		reader: r, buf: buf, ipv4Only: r.Metadata.IPVersion == 4,
		offsets: make([]uintptr, 0, len(seen)), countries: make([]country, len(seen)),
		typ: r.Metadata.DatabaseType, built: r.Metadata.BuildTime().UTC(),
	}
	for off := range seen {
		db.offsets = append(db.offsets, off)
	}
	slices.Sort(db.offsets)
	for i, off := range db.offsets {
		db.countries[i] = seen[off]
	}
	return db, nil
}

// decodeCountry decodes country.iso_code of the record res found: absent,
// or of another form than two upper-case letters, places nothing; a value
// that is not a string, or a record that does not decode, is errRecord.
func decodeCountry(res maxminddb.Result) (country, error) {
	var code *string
	if err := res.DecodePath(&code, "country", "iso_code"); err != nil {
		return noCountry, fmt.Errorf("%w: %w", errRecord, err)
	}
	if code == nil {
		return noCountry, nil
	}
	c, _ := parseCountry(*code)
	return c, nil
}

// place returns the country the database places a in: noCountry for an
// address without a record or with a record without a country (private
// and reserved ranges among them), and for an IPv6 address in an IPv4-only
// database. An IPv4-mapped IPv6 address is looked up as IPv4. A lookup
// that fails is an error. It allocates nothing for a record the load
// walked.
func (db *countryDB) place(a netip.Addr) (country, error) {
	a = a.Unmap()
	if db.ipv4Only && a.Is6() {
		return noCountry, nil
	}
	res := db.reader.Lookup(a)
	if err := res.Err(); err != nil {
		return noCountry, fmt.Errorf("%w: %w", errTree, err)
	}
	if !res.Found() {
		return noCountry, nil
	}
	if i, ok := slices.BinarySearch(db.offsets, res.Offset()); ok {
		return db.countries[i], nil
	}
	// A record the walk did not reach: decode it here.
	return decodeCountry(res)
}
