// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Slots is the number of Redis Cluster hash slots.
const Slots = 16384

// crcTable is the read-only CRC16-XMODEM table (polynomial 0x1021): entry
// i is the CRC of the single byte i. TestCRCTable recomputes it.
var crcTable = [256]uint16{
	0x0000, 0x1021, 0x2042, 0x3063, 0x4084, 0x50a5, 0x60c6, 0x70e7,
	0x8108, 0x9129, 0xa14a, 0xb16b, 0xc18c, 0xd1ad, 0xe1ce, 0xf1ef,
	0x1231, 0x0210, 0x3273, 0x2252, 0x52b5, 0x4294, 0x72f7, 0x62d6,
	0x9339, 0x8318, 0xb37b, 0xa35a, 0xd3bd, 0xc39c, 0xf3ff, 0xe3de,
	0x2462, 0x3443, 0x0420, 0x1401, 0x64e6, 0x74c7, 0x44a4, 0x5485,
	0xa56a, 0xb54b, 0x8528, 0x9509, 0xe5ee, 0xf5cf, 0xc5ac, 0xd58d,
	0x3653, 0x2672, 0x1611, 0x0630, 0x76d7, 0x66f6, 0x5695, 0x46b4,
	0xb75b, 0xa77a, 0x9719, 0x8738, 0xf7df, 0xe7fe, 0xd79d, 0xc7bc,
	0x48c4, 0x58e5, 0x6886, 0x78a7, 0x0840, 0x1861, 0x2802, 0x3823,
	0xc9cc, 0xd9ed, 0xe98e, 0xf9af, 0x8948, 0x9969, 0xa90a, 0xb92b,
	0x5af5, 0x4ad4, 0x7ab7, 0x6a96, 0x1a71, 0x0a50, 0x3a33, 0x2a12,
	0xdbfd, 0xcbdc, 0xfbbf, 0xeb9e, 0x9b79, 0x8b58, 0xbb3b, 0xab1a,
	0x6ca6, 0x7c87, 0x4ce4, 0x5cc5, 0x2c22, 0x3c03, 0x0c60, 0x1c41,
	0xedae, 0xfd8f, 0xcdec, 0xddcd, 0xad2a, 0xbd0b, 0x8d68, 0x9d49,
	0x7e97, 0x6eb6, 0x5ed5, 0x4ef4, 0x3e13, 0x2e32, 0x1e51, 0x0e70,
	0xff9f, 0xefbe, 0xdfdd, 0xcffc, 0xbf1b, 0xaf3a, 0x9f59, 0x8f78,
	0x9188, 0x81a9, 0xb1ca, 0xa1eb, 0xd10c, 0xc12d, 0xf14e, 0xe16f,
	0x1080, 0x00a1, 0x30c2, 0x20e3, 0x5004, 0x4025, 0x7046, 0x6067,
	0x83b9, 0x9398, 0xa3fb, 0xb3da, 0xc33d, 0xd31c, 0xe37f, 0xf35e,
	0x02b1, 0x1290, 0x22f3, 0x32d2, 0x4235, 0x5214, 0x6277, 0x7256,
	0xb5ea, 0xa5cb, 0x95a8, 0x8589, 0xf56e, 0xe54f, 0xd52c, 0xc50d,
	0x34e2, 0x24c3, 0x14a0, 0x0481, 0x7466, 0x6447, 0x5424, 0x4405,
	0xa7db, 0xb7fa, 0x8799, 0x97b8, 0xe75f, 0xf77e, 0xc71d, 0xd73c,
	0x26d3, 0x36f2, 0x0691, 0x16b0, 0x6657, 0x7676, 0x4615, 0x5634,
	0xd94c, 0xc96d, 0xf90e, 0xe92f, 0x99c8, 0x89e9, 0xb98a, 0xa9ab,
	0x5844, 0x4865, 0x7806, 0x6827, 0x18c0, 0x08e1, 0x3882, 0x28a3,
	0xcb7d, 0xdb5c, 0xeb3f, 0xfb1e, 0x8bf9, 0x9bd8, 0xabbb, 0xbb9a,
	0x4a75, 0x5a54, 0x6a37, 0x7a16, 0x0af1, 0x1ad0, 0x2ab3, 0x3a92,
	0xfd2e, 0xed0f, 0xdd6c, 0xcd4d, 0xbdaa, 0xad8b, 0x9de8, 0x8dc9,
	0x7c26, 0x6c07, 0x5c64, 0x4c45, 0x3ca2, 0x2c83, 0x1ce0, 0x0cc1,
	0xef1f, 0xff3e, 0xcf5d, 0xdf7c, 0xaf9b, 0xbfba, 0x8fd9, 0x9ff8,
	0x6e17, 0x7e36, 0x4e55, 0x5e74, 0x2e93, 0x3eb2, 0x0ed1, 0x1ef0,
}

// crcStep folds one byte into a running CRC16-XMODEM.
func crcStep(crc uint16, c byte) uint16 { return crc<<8 ^ crcTable[byte(crc>>8)^c] }

// CRC16 is CRC16-XMODEM (polynomial 0x1021, initial value 0, no
// reflection), the Redis Cluster key hash: CRC16("123456789") = 0x31C3.
func CRC16(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc = crcStep(crc, c)
	}
	return crc
}

// crc16String is CRC16 over a string.
func crc16String(s string) uint16 {
	var crc uint16
	for i := range len(s) {
		crc = crcStep(crc, s[i])
	}
	return crc
}

// hashTagBounds returns the hash tag of key per the Redis Cluster
// specification: the bytes between the first '{' and the next '}' when
// non-empty, else the whole key.
func hashTagBounds(key string) (lo, hi int) {
	i := strings.IndexByte(key, '{')
	if i < 0 {
		return 0, len(key)
	}
	j := strings.IndexByte(key[i+1:], '}')
	if j <= 0 {
		return 0, len(key)
	}
	return i + 1, i + 1 + j
}

// HashTag returns the part of key its slot is computed from (spec 08 req
// 49): foo{bar}{zap} hashes bar, foo{}{bar} and a key without braces hash
// the whole key, foo{{bar}}zap hashes {bar.
func HashTag(key string) string {
	lo, hi := hashTagBounds(key)
	return key[lo:hi]
}

// Slot is the Redis Cluster hash slot of key: CRC16 of its hash tag,
// modulo 16384 (spec 08 req 49).
func Slot(key []byte) uint16 {
	if i := bytes.IndexByte(key, '{'); i >= 0 {
		if j := bytes.IndexByte(key[i+1:], '}'); j > 0 {
			key = key[i+1 : i+1+j]
		}
	}
	return CRC16(key) % Slots
}

// SlotString is Slot for a string key.
func SlotString(key string) uint16 {
	lo, hi := hashTagBounds(key)
	return crc16String(key[lo:hi]) % Slots
}

// crcDigest folds the lowercase hex encoding of d into crc.
func crcDigest(crc uint16, d *statestore.Digest) uint16 {
	for _, b := range d {
		crc = crcStep(crc, hexDigits[b>>4])
		crc = crcStep(crc, hexDigits[b&0x0f])
	}
	return crc
}

// DigestSlot is the slot of every key tagged {<hex d>}: the GCRA and
// Quota keys of one config.key digest, and the generation key of a URI
// digest. Consumptive calls with equal digests therefore share one slot
// and merge into one script (spec 08 reqs 29 and 49).
func DigestSlot(d statestore.Digest) uint16 { return crcDigest(0, &d) % Slots }

// PartitionSlot is the slot of a Response Cache partition hash and its
// lease, both tagged {<hex U>:<hex P>}.
func PartitionSlot(k statestore.CacheKey) uint16 {
	crc := crcDigest(0, &k.URI)
	crc = crcStep(crc, ':')
	return crcDigest(crc, &k.Partition) % Slots
}

// SlotOf returns the slot of a consumptive call's keys (GCRA and Quota:
// the config.key digest's tag), or -1 for a call without one slot (a
// Response Cache lookup reads the generation and the partition, two tags).
func SlotOf(c *statestore.Call) int {
	switch c.Kind {
	case statestore.OpGCRA:
		return int(DigestSlot(c.GCRA.Digest))
	case statestore.OpQuota:
		return int(DigestSlot(c.Quota.Digest))
	default:
		return -1
	}
}

// WriteSlot returns the slot of a post-commit write's keys, or -1 for an
// unknown kind: a refund's counter, a store's or lease's partition, an
// invalidation's generation key.
func WriteSlot(w *statestore.Write) int {
	switch w.Kind {
	case statestore.OpRefund:
		return int(DigestSlot(w.Refund.Digest))
	case statestore.OpCacheSet:
		if w.Lease.Mode != 0 {
			return int(PartitionSlot(w.Lease.Key))
		}
		return int(PartitionSlot(w.Cache.Key))
	case statestore.OpCacheInvalidate:
		return int(DigestSlot(w.Invalidate))
	default:
		return -1
	}
}
