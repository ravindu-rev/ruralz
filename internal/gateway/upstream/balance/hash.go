// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math/bits"
	"math/rand/v2"
	"strconv"
)

// FNV-1a 64-bit parameters, as in hash/fnv (OQ-traffic-management-and-
// resilience-15 (a): a standard-library hash). The arithmetic is inlined so
// hashing a key allocates nothing; tests pin it to hash/fnv.
const (
	fnvOffset64 uint64 = 14695981039346656037
	fnvPrime64  uint64 = 1099511628211
)

// fnv1a continues an FNV-1a-64 hash h over s.
func fnv1a(h uint64, s string) uint64 {
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// mix64 is the splitmix64 finalizer. FNV-1a spreads short, similar strings
// poorly over the high bits; the finalizer makes every output bit depend on
// every input bit, so ring positions and keys cover the 64-bit space evenly
// (05 req 14).
func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// HashKey maps a ring-hash key, the string value of hashKey, onto the ring:
// FNV-1a-64 of the key passed through the splitmix64 finalizer, the
// function that places virtual nodes (05 req 14). It allocates nothing.
func HashKey(key string) uint64 { return mix64(fnv1a(fnvOffset64, key)) }

// vnodePrefix is the FNV-1a state after identity + "#".
func vnodePrefix(identity string) uint64 { return fnv1a(fnv1a(fnvOffset64, identity), "#") }

// vnodePosition is the ring position of virtual node j of the Endpoint whose
// vnodePrefix is prefix: FNV-1a-64(identity + "#" + j) through the
// splitmix64 finalizer, j in decimal (05 req 14).
func vnodePosition(prefix uint64, j int) uint64 {
	var buf [20]byte
	h := prefix
	for _, c := range strconv.AppendInt(buf[:0], int64(j), 10) {
		h ^= uint64(c)
		h *= fnvPrime64
	}
	return mix64(h)
}

// runtimeSource draws from math/rand/v2's runtime generator, which is safe
// for concurrent use and takes no lock.
type runtimeSource struct{}

// Uint64 returns a uniform 64-bit value.
func (runtimeSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: Endpoint selection needs a fast uniform draw, not an unpredictable one (05 req 15).
}

// source returns src, or the runtime generator when src is nil.
func source(src rand.Source) rand.Source {
	if src == nil {
		return runtimeSource{}
	}
	return src
}

// uint64n returns a uniform value in [0, n) for n > 0 by Lemire's
// multiply-shift method with rejection, which has no modulo bias.
func uint64n(src rand.Source, n uint64) uint64 {
	if n&(n-1) == 0 {
		return src.Uint64() & (n - 1)
	}
	hi, lo := bits.Mul64(src.Uint64(), n)
	if lo < n {
		thresh := -n % n
		for lo < thresh {
			hi, lo = bits.Mul64(src.Uint64(), n)
		}
	}
	return hi
}
