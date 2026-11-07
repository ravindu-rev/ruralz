// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package geoiptest writes MaxMind DB files for tests, in format 2.0 of
// the MaxMind DB specification (https://maxmind.github.io/MaxMind-DB/): a
// binary search tree over the networks a test inserts, a data section
// holding their records, and the metadata. No MaxMind DB writer is an
// admitted module (docs/engineering/01-tech-stack-and-libraries.md
// "Library catalog"), so this package uses the standard library alone.
//
// Records are encoded without pointers, and identical records share one
// data section entry, as a MaxMind writer's do. An IPv6 database holds
// IPv4 networks at ::/96, as MaxMind databases do, without the aliases
// (::ffff:0:0/96 and others) a MaxMind writer adds. It is test support:
// no binary links it.
package geoiptest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

// Data section types (MaxMind DB specification, "Output Data Section").
const (
	typeString byte = 2
	typeDouble byte = 3
	typeBytes  byte = 4
	typeUint16 byte = 5
	typeUint32 byte = 6
	typeMap    byte = 7
	typeInt32  byte = 8
	typeUint64 byte = 9
	typeArray  byte = 11
	typeBool   byte = 14
)

// dataSeparator is the 16 zero bytes between the search tree and the data
// section, which data pointers in the tree skip.
const dataSeparator = 16

// metadataMarker starts the metadata section.
const metadataMarker = "\xAB\xCD\xEFMaxMind.com"

// DefaultDatabaseType is the database_type of a Database whose Options
// name none.
const DefaultDatabaseType = "Ruralz-Test-Country"

// Raw is a value already encoded in the data section format, written as
// given, so a test can store a record a MaxMind writer never would.
type Raw []byte

// Options configure a Database. Every field may be left zero.
type Options struct {
	// IPVersion is 4 or 6; 0 means 6.
	IPVersion int
	// RecordSize is the search tree record size in bits, 24, 28 or 32; 0
	// means 24.
	RecordSize int
	// DatabaseType is the metadata database_type; "" means
	// DefaultDatabaseType.
	DatabaseType string
	// BuildEpoch is the metadata build_epoch, in Unix seconds.
	BuildEpoch uint64
	// Metadata replaces or adds metadata entries after the others, so a
	// test can write metadata a MaxMind writer never would.
	Metadata map[string]any
}

// Database is a MaxMind DB under construction; use New. Insert records
// the first error, which Bytes returns.
type Database struct {
	opts    Options
	root    *node
	records [][]byte
	byData  map[string]int
	err     error
}

// node is one search tree node: an inner node with two children, or a
// leaf holding a record index; a nil child is a network without data.
type node struct {
	kids [2]*node
	leaf bool
	rec  int
}

// New returns an empty Database.
func New(o Options) *Database {
	if o.IPVersion == 0 {
		o.IPVersion = 6
	}
	if o.RecordSize == 0 {
		o.RecordSize = 24
	}
	if o.DatabaseType == "" {
		o.DatabaseType = DefaultDatabaseType
	}
	d := &Database{opts: o, root: &node{}, byData: map[string]int{}}
	switch {
	case o.IPVersion != 4 && o.IPVersion != 6:
		d.err = fmt.Errorf("geoiptest: ip version %d is neither 4 nor 6", o.IPVersion)
	case o.RecordSize != 24 && o.RecordSize != 28 && o.RecordSize != 32:
		d.err = fmt.Errorf("geoiptest: record size %d is not 24, 28 or 32", o.RecordSize)
	}
	return d
}

// Country returns the record a GeoIP2 or GeoLite2 Country database holds
// for a network placed in the country whose ISO 3166-1 alpha-2 code is
// iso: country.iso_code, beside the names and identifiers such a record
// also carries.
func Country(iso string) map[string]any {
	return map[string]any{
		"continent": map[string]any{"code": "XX", "geoname_id": uint32(1)},
		"country": map[string]any{
			"geoname_id": uint32(2),
			"iso_code":   iso,
			"names":      map[string]any{"en": "Country " + iso},
		},
		"registered_country": map[string]any{"iso_code": iso},
	}
}

// Insert adds network, a CIDR or a bare address, with rec, a value of the
// types encode takes. A network inserted inside an earlier, shorter one
// splits it; one inserted over earlier, longer ones replaces them. An IPv4
// network of an IPv6 database goes to ::/96. It returns d.
func (d *Database) Insert(network string, rec any) *Database {
	if d.err != nil {
		return d
	}
	p, err := parseNetwork(network)
	if err != nil {
		d.err = err
		return d
	}
	bits, n, err := d.treeBits(p)
	if err != nil {
		d.err = err
		return d
	}
	data, err := encode(nil, rec)
	if err != nil {
		d.err = fmt.Errorf("geoiptest: network %s: %w", network, err)
		return d
	}
	idx, ok := d.byData[string(data)]
	if !ok {
		idx = len(d.records)
		d.records = append(d.records, data)
		d.byData[string(data)] = idx
	}
	d.insert(bits, n, idx)
	return d
}

// parseNetwork parses a CIDR or a bare address as a masked prefix.
func parseNetwork(network string) (netip.Prefix, error) {
	if strings.Contains(network, "/") {
		p, err := netip.ParsePrefix(network)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("geoiptest: %w", err)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(network)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("geoiptest: %w", err)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// treeBits returns the search tree path of p: its address bits and
// prefix length in the database's address space.
func (d *Database) treeBits(p netip.Prefix) ([16]byte, int, error) {
	var bits [16]byte
	a := p.Addr()
	switch {
	case d.opts.IPVersion == 4 && !a.Is4():
		return bits, 0, fmt.Errorf("geoiptest: IPv6 network %s in an IPv4 database", p)
	case d.opts.IPVersion == 4:
		a4 := a.As4()
		copy(bits[:4], a4[:])
		return bits, p.Bits(), nil
	case a.Is4():
		a4 := a.As4()
		copy(bits[12:], a4[:])
		return bits, 96 + p.Bits(), nil
	default:
		return a.As16(), p.Bits(), nil
	}
}

// insert places record idx at the n-bit path bits.
func (d *Database) insert(bits [16]byte, n, idx int) {
	if n == 0 {
		d.root.kids = [2]*node{{leaf: true, rec: idx}, {leaf: true, rec: idx}}
		return
	}
	cur := d.root
	for i := range n {
		b := (bits[i/8] >> (7 - i%8)) & 1
		if i == n-1 {
			cur.kids[b] = &node{leaf: true, rec: idx}
			return
		}
		next := cur.kids[b]
		switch {
		case next == nil:
			next = &node{}
			cur.kids[b] = next
		case next.leaf:
			next = &node{kids: [2]*node{{leaf: true, rec: next.rec}, {leaf: true, rec: next.rec}}}
			cur.kids[b] = next
		}
		cur = next
	}
}

// Bytes returns the database file: the search tree, the data section
// separator, the data section and the metadata.
func (d *Database) Bytes() ([]byte, error) {
	if d.err != nil {
		return nil, d.err
	}
	order := []*node{d.root}
	index := map[*node]uint64{d.root: 0}
	for i := 0; i < len(order); i++ {
		for _, k := range order[i].kids {
			if k != nil && !k.leaf {
				index[k] = uint64(len(order))
				order = append(order, k)
			}
		}
	}
	count := uint64(len(order))
	var data []byte
	offsets := make([]uint64, len(d.records))
	for i, r := range d.records {
		offsets[i] = uint64(len(data))
		data = append(data, r...)
	}
	value := func(k *node) uint64 {
		switch {
		case k == nil:
			return count
		case k.leaf:
			return count + dataSeparator + offsets[k.rec]
		default:
			return index[k]
		}
	}
	size := d.opts.RecordSize
	limit := uint64(1)<<size - 1
	out := make([]byte, 0, len(order)*size/4+dataSeparator+len(data)+256)
	for _, n := range order {
		l, r := value(n.kids[0]), value(n.kids[1])
		if l > limit || r > limit {
			return nil, fmt.Errorf("geoiptest: a record does not fit %d bits", size)
		}
		out = appendNode(out, size, l, r)
	}
	out = append(out, make([]byte, dataSeparator)...)
	out = append(out, data...)
	out = append(out, metadataMarker...)
	meta := map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 d.opts.BuildEpoch,
		"database_type":               d.opts.DatabaseType,
		"description":                 map[string]any{"en": "Ruralz test database"},
		"ip_version":                  uint16(d.opts.IPVersion), //nolint:gosec // G115: New allows 4 or 6 only.
		"languages":                   []any{"en"},
		"node_count":                  uint32(count), //nolint:gosec // G115: a test database holds few nodes.
		"record_size":                 uint16(size),  //nolint:gosec // G115: New allows 24, 28 or 32 only.
	}
	maps.Copy(meta, d.opts.Metadata)
	metaBytes, err := encode(nil, meta)
	if err != nil {
		return nil, err
	}
	return append(out, metaBytes...), nil
}

// appendNode appends one node of two records l and r of size bits, which
// Bytes checked they fit: 24-bit records as three bytes each, 28-bit ones
// as three bytes each around a byte holding the high nibble of l and then
// of r, 32-bit ones as four bytes each.
func appendNode(out []byte, size int, l, r uint64) []byte {
	var lb, rb [8]byte
	binary.BigEndian.PutUint64(lb[:], l)
	binary.BigEndian.PutUint64(rb[:], r)
	switch size {
	case 24:
		return append(append(out, lb[5:]...), rb[5:]...)
	case 28:
		out = append(out, lb[5:]...)
		out = append(out, (lb[4]&0x0F)<<4|rb[4]&0x0F)
		return append(out, rb[5:]...)
	default:
		return append(append(out, lb[4:]...), rb[4:]...)
	}
}

// errType reports a value encode cannot write.
var errType = errors.New("unsupported value type")

// encode appends v in the data section format: string, []byte, bool,
// float64, uint16, uint32, uint64, int32, map[string]any (keys in
// ascending order), []any and Raw.
func encode(out []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case Raw:
		return append(out, v...), nil
	case string:
		return append(control(out, typeString, len(v)), v...), nil
	case []byte:
		return append(control(out, typeBytes, len(v)), v...), nil
	case bool:
		n := 0
		if v {
			n = 1
		}
		return control(out, typeBool, n), nil
	case float64:
		return binary.BigEndian.AppendUint64(control(out, typeDouble, 8), math.Float64bits(v)), nil
	case uint16:
		return appendUint(out, typeUint16, uint64(v)), nil
	case uint32:
		return appendUint(out, typeUint32, uint64(v)), nil
	case uint64:
		return appendUint(out, typeUint64, v), nil
	case int32:
		return binary.BigEndian.AppendUint32(control(out, typeInt32, 4), uint32(v)), nil //nolint:gosec // G115: the int32 encoding is the value's two's complement bits.
	case map[string]any:
		out = control(out, typeMap, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var err error
		for _, k := range keys {
			out = append(control(out, typeString, len(k)), k...)
			if out, err = encode(out, v[k]); err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
		}
		return out, nil
	case []any:
		out = control(out, typeArray, len(v))
		var err error
		for i, e := range v {
			if out, err = encode(out, e); err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w %T", errType, v)
	}
}

// appendUint appends an unsigned integer of type typ in its fewest
// big-endian bytes, none for 0.
func appendUint(out []byte, typ byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	payload := bytes.TrimLeft(b[:], "\x00")
	return append(control(out, typ, len(payload)), payload...)
}

// control appends the control byte of a value of type typ and size n:
// the type in the top three bits, or 0 there and the type minus 7 in the
// next byte for an extended type, then the size in the low five bits, with
// one to three more bytes past 28.
func control(out []byte, typ byte, n int) []byte {
	u := uint32(n) //nolint:gosec // G115: n is the length of a test value, far below 2^32.
	var first byte
	if typ <= 7 {
		first = typ << 5
	}
	var b [4]byte
	var extra []byte
	switch {
	case u < 29:
		binary.BigEndian.PutUint32(b[:], u)
		first |= b[3]
	case u < 29+256:
		first |= 29
		binary.BigEndian.PutUint32(b[:], u-29)
		extra = b[3:]
	case u < 285+65536:
		first |= 30
		binary.BigEndian.PutUint32(b[:], u-285)
		extra = b[2:]
	default:
		first |= 31
		binary.BigEndian.PutUint32(b[:], u-65821)
		extra = b[1:]
	}
	out = append(out, first)
	if typ > 7 {
		out = append(out, typ-7)
	}
	return append(out, extra...)
}

// Write writes d to path with mode 0o600, failing tb on any error.
func Write(tb testing.TB, path string, d *Database) {
	tb.Helper()
	b, err := d.Bytes()
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		tb.Fatal(err)
	}
}
