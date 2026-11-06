// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// KeySource names the key each request carries, such as the
// `x-bench-key` header S2's `ratelimit` Policy reads (PBB "Scenarios").
// Key is a pure function of the request's sequence number, so a run's
// keys never depend on the order workers send requests in.
type KeySource interface {
	// Key returns request seq's key.
	Key(seq uint64) string
	// Cardinality returns the number of distinct keys; 0 when unbounded.
	Cardinality() int
	// Distribution names the key distribution for a result record.
	Distribution() string
}

// Cycle returns n keys, prefix+"0" to prefix+(n−1), each used in turn:
// S2's uniform cycle over 100,000 values (target).
func Cycle(prefix string, n int) (KeySource, error) {
	if n < 1 {
		return nil, fmt.Errorf("loadgen: key cycle of %d keys", n)
	}
	return cycleKeys{prefix: prefix, n: uint64(n)}, nil
}

type cycleKeys struct {
	prefix string
	n      uint64
}

func (c cycleKeys) Key(seq uint64) string {
	return c.prefix + strconv.FormatUint(seq%c.n, 10)
}
func (c cycleKeys) Cardinality() int     { return int(c.n) } //nolint:gosec // G115: built from an int
func (c cycleKeys) Distribution() string { return "uniform" }

// Rotate returns a new key every perKey requests: perKey 1 makes every
// key new, the key-rotation flood of CE-15.
func Rotate(prefix string, perKey uint64) (KeySource, error) {
	if perKey < 1 {
		return nil, errors.New("loadgen: key rotation needs at least one request per key")
	}
	return rotateKeys{prefix: prefix, per: perKey}, nil
}

type rotateKeys struct {
	prefix string
	per    uint64
}

func (r rotateKeys) Key(seq uint64) string {
	return r.prefix + strconv.FormatUint(seq/r.per, 10)
}
func (r rotateKeys) Cardinality() int     { return 0 }
func (r rotateKeys) Distribution() string { return "rotating" }

// List returns the keys given, each used in turn: request seq takes
// keys[seq % len(keys)], as the chaos runs rotate the Consumers' API keys
// and /undeclared's keys.
func List(keys ...string) (KeySource, error) {
	if len(keys) == 0 {
		return nil, errors.New("loadgen: a key list needs a key")
	}
	return listKeys(append([]string(nil), keys...)), nil
}

type listKeys []string

func (l listKeys) Key(seq uint64) string { return l[seq%uint64(len(l))] }
func (l listKeys) Cardinality() int      { return len(l) }
func (l listKeys) Distribution() string  { return "list" }

// Single returns one key for every request: the hot key of CE-12.
func Single(key string) KeySource { return singleKey(key) }

type singleKey string

func (s singleKey) Key(uint64) string    { return string(s) }
func (s singleKey) Cardinality() int     { return 1 }
func (s singleKey) Distribution() string { return "single" }

// ZipfKeys draws n keys, prefix+"0" (the hottest) to prefix+(n−1), with
// rank k chosen with probability proportional to 1/(k+1)^s: PBB's Zipf
// variant of S2. Each request's draw comes from a hash of the seed and its
// sequence number, so the keys are deterministic.
type ZipfKeys struct {
	prefix string
	s      float64
	seed   uint64
	cdf    []float64
}

// Zipf returns n Zipf-distributed keys with exponent s >= 0 (0 is
// uniform).
func Zipf(prefix string, n int, s float64, seed uint64) (*ZipfKeys, error) {
	switch {
	case n < 1:
		return nil, fmt.Errorf("loadgen: Zipf over %d keys", n)
	case !(s >= 0) || math.IsInf(s, 0):
		return nil, fmt.Errorf("loadgen: Zipf exponent %v is not a finite number >= 0", s)
	}
	z := &ZipfKeys{prefix: prefix, s: s, seed: seed, cdf: make([]float64, n)}
	var sum float64
	for k := range n {
		sum += math.Pow(float64(k+1), -s)
		z.cdf[k] = sum
	}
	for k := range z.cdf {
		z.cdf[k] /= sum
	}
	z.cdf[n-1] = 1
	return z, nil
}

// Key returns request seq's key.
func (z *ZipfKeys) Key(seq uint64) string {
	u := unitFloat(z.seed, seq)
	k := sort.Search(len(z.cdf), func(i int) bool { return z.cdf[i] > u })
	return z.prefix + strconv.Itoa(min(k, len(z.cdf)-1))
}

// Share returns the probability of the key of rank k (0 is the hottest),
// so a scenario can keep its hottest key under a rate, as PBB's Zipf
// variant keeps it under 25,000 calls per second (target).
func (z *ZipfKeys) Share(k int) float64 {
	if k < 0 || k >= len(z.cdf) {
		return 0
	}
	if k == 0 {
		return z.cdf[0]
	}
	return z.cdf[k] - z.cdf[k-1]
}

// Cardinality returns the number of keys.
func (z *ZipfKeys) Cardinality() int { return len(z.cdf) }

// Distribution names the distribution with its exponent.
func (z *ZipfKeys) Distribution() string {
	return "zipf(s=" + strconv.FormatFloat(z.s, 'g', -1, 64) + ")"
}

// unitFloat hashes seed and seq to a float in [0, 1) (SplitMix64).
func unitFloat(seed, seq uint64) float64 {
	x := seed + seq*0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	x ^= x >> 31
	return float64(x>>11) / (1 << 53)
}

// maxMixWeight bounds a mix's total weight after dividing out the common
// factor (target); the interleaving table holds one entry per unit.
const maxMixWeight = 1 << 16

// Part is one target of a Mix with its share.
type Part struct {
	// Weight is the part's share of requests, at least 1.
	Weight int
	// Target sends the part's requests.
	Target Target
}

// Mix returns a target that sends each request to one part, in shares
// of the weights, interleaved smoothly and decided by the sequence number
// alone, such as cacheable reads mixed with invalidating writes (CE-16).
func Mix(parts ...Part) (Target, error) {
	if len(parts) == 0 {
		return nil, errors.New("loadgen: empty mix")
	}
	g := 0
	for i, p := range parts {
		if p.Weight < 1 || p.Target == nil {
			return nil, fmt.Errorf("loadgen: mix part %d needs a weight of at least 1 and a target", i)
		}
		g = gcd(g, p.Weight)
	}
	weights := make([]int, len(parts))
	total := 0
	for i, p := range parts {
		weights[i] = p.Weight / g
		total += weights[i]
		if total > maxMixWeight {
			return nil, fmt.Errorf("loadgen: mix weights sum above %d after reduction", maxMixWeight)
		}
	}
	// Smooth weighted round robin over one period fixes the order.
	table := make([]uint32, total)
	current := make([]int, len(parts))
	for t := range table {
		best := 0
		for i, w := range weights {
			current[i] += w
			if current[i] > current[best] {
				best = i
			}
		}
		current[best] -= total
		table[t] = uint32(best)
	}
	return &mix{parts: parts, table: table}, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

type mix struct {
	parts []Part
	table []uint32
}

func (m *mix) Do(ctx context.Context, r Request) Response {
	return m.parts[m.table[r.Seq%uint64(len(m.table))]].Target.Do(ctx, r)
}
