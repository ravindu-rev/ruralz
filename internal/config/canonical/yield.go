// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// DefaultYieldAfter is the work a goroutine of a call does before it yields
// the processor when Options.YieldAfter or DecoderOptions.YieldAfter is
// zero or negative (100 µs).
const DefaultYieldAfter = 100 * time.Microsecond

// yieldCheck is the number of work units between two reads of the yield
// timer inside a work unit (checked every 256 nodes).
const yieldCheck = 256

// piece is the most bytes the package hashes or copies in one step, so a
// goroutine reads its yield timer at least once per piece of a large
// Revision.
const piece = 64 << 10

// yieldAfter returns the yield interval of the option v.
func yieldAfter(v time.Duration) time.Duration {
	if v <= 0 {
		return DefaultYieldAfter
	}
	return v
}

// site names one place of the package that reads the yield timer: a loop
// whose length depends on the content, or a step between two units of
// work. The tests count each site's units, so each has its own test that
// fails when the site stops counting.
type site uint8

// Sites, one per loop or boundary.
const (
	siteItem       site = iota // Encode, EncodeResources: a resource collected
	siteKind                   // order: a resource's kind checked
	siteSorted                 // order: a comparison of the sortedness check
	siteSort                   // order: a comparison of the sort
	siteDuplicate              // order: a resource compared with the one before
	siteResource               // encode: between two resources written
	siteValue                  // writer.value: a value written
	siteJoin                   // encode: between two chunks joined
	siteHash                   // sum: after a piece hashed
	siteCopy                   // parse: after a piece of the content copied
	siteNode                   // parser: a value built
	siteToken                  // skip: a token of a value scanned, not built
	siteElement                // stream: between two elements of resources
	siteCheck                  // checker.run: between two resources checked
	siteMember                 // checker.resource: a member judged
	siteWalkMember             // newerWalk: a member judged
	siteWalkItem               // newerWalk: a list item visited
	siteOffer                  // keeper: a finding offered
	siteKeepSort               // keeper: a comparison sorting the findings
	siteList                   // keeper: a finding listed
	siteDocument               // envelope: a document member read
	siteChunk                  // decode: a chunk's entries counted or gathered
	siteSequence               // sequence: an entry checked
	siteEnvelope               // envelopes: an envelope built
	siteDump                   // scanDump: a member of the dump document
	siteCount
)

// yielder is the yield timer of one goroutine of a call: the goroutine
// reads the clock between two units of work (check) and once every
// yieldCheck units of work inside one (charge), and calls runtime.Gosched
// once after has passed since its last yield, so a large Revision shares
// the processor with live traffic. A yielder belongs to one goroutine;
// the calling goroutine of a call has one, and so has each goroutine the
// call starts. Its first read starts the interval, so the clock is read
// only at the sites, inside the work whose panics the call recovers on
// the goroutines it starts. A nil yielder, or one without a clock, counts
// nothing and reads no clock.
type yielder struct {
	// now is the clock (Options.Now, DecoderOptions.Now); nil never
	// yields.
	now func() time.Time
	// after is the work between two yields.
	after time.Duration
	// started reports that last was read.
	started bool
	// last is when the goroutine last yielded, or first read the clock.
	last time.Time
	// units counts the work units since the timer was last read.
	units int
	// yields, checks and sites count the yields, the reads between units
	// and the units of each site, for tests.
	yields, checks int
	sites          [siteCount]int
}

// newYielder returns the yield timer of a goroutine; it reads no clock.
func newYielder(now func() time.Time, after time.Duration) *yielder {
	return &yielder{now: now, after: after}
}

// on reports whether y reads a clock.
func (y *yielder) on() bool { return y != nil && y.now != nil }

// charge counts k work units at site s and, each time they reach
// yieldCheck, reads the timer.
func (y *yielder) charge(s site, k int) {
	if !y.on() {
		return
	}
	y.sites[s] += k
	if y.units += k; y.units >= yieldCheck {
		y.units = 0
		y.tick()
	}
}

// check reads the timer between two units of work at site s.
func (y *yielder) check(s site) {
	if !y.on() {
		return
	}
	y.sites[s]++
	y.checks++
	y.units = 0
	y.tick()
}

// tick yields once y.after has passed since the last yield; the first
// tick starts the interval.
func (y *yielder) tick() {
	if !y.started {
		y.started, y.last = true, y.now()
		return
	}
	if y.now().Sub(y.last) >= y.after {
		runtime.Gosched()
		y.yields++
		y.last = y.now()
	}
}

// sum returns the digest of content, revision.Sum's, hashing a piece at a
// time with a read of y's timer after each, so a large Revision is hashed
// in steps of bounded work. Without a clock it is revision.Sum.
func sum(content []byte, y *yielder) revision.Digest {
	if !y.on() {
		return revision.Sum(content)
	}
	h := sha256.New()
	for rest := content; ; {
		k := min(len(rest), piece)
		_, _ = h.Write(rest[:k])
		rest = rest[k:]
		y.check(siteHash)
		if len(rest) == 0 {
			break
		}
	}
	// The text of a SHA-256 sum always parses.
	d, _ := revision.Parse("sha256:" + hex.EncodeToString(h.Sum(nil)))
	return d
}

// copyString returns a string copy of content, made a piece at a time
// with a read of y's timer after each. Without a clock it is one copy.
func copyString(content []byte, y *yielder) string {
	if !y.on() {
		return string(content)
	}
	var b strings.Builder
	b.Grow(len(content))
	for rest := content; ; {
		k := min(len(rest), piece)
		_, _ = b.Write(rest[:k])
		rest = rest[k:]
		y.check(siteCopy)
		if len(rest) == 0 {
			break
		}
	}
	return b.String()
}
