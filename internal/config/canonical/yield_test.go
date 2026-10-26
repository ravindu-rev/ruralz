// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// stepClock is a fake clock that advances by step on every read; it is
// safe for concurrent use and reads no wall clock.
type stepClock struct {
	step  time.Duration
	reads atomic.Int64
}

func (c *stepClock) now() time.Time {
	return time.Unix(0, 0).Add(time.Duration(c.reads.Add(1)) * c.step)
}

// observed collects the yield timers of the goroutines of the calls of one
// Encoder or Decoder, each handed over once its goroutine is done with it.
type observed struct {
	mu sync.Mutex
	ys []*yielder
}

func (o *observed) add(y *yielder) {
	o.mu.Lock()
	o.ys = append(o.ys, y)
	o.mu.Unlock()
}

// take returns the timers collected and forgets them.
func (o *observed) take() []*yielder {
	o.mu.Lock()
	defer o.mu.Unlock()
	ys := o.ys
	o.ys = nil
	return ys
}

// total sums the units of every site, the reads between units and the
// yields of ys.
func total(ys []*yielder) (sites [siteCount]int, checks, yields int) {
	for _, y := range ys {
		for s := range sites {
			sites[s] += y.sites[s]
		}
		checks += y.checks
		yields += y.yields
	}
	return sites, checks, yields
}

// inner returns the reads of the timers ys made inside units of work, on
// a clock where every read but a timer's first yields: their yields,
// plus the first read of each timer that started, minus the reads
// between units.
func inner(ys []*yielder) int {
	_, checks, yields := total(ys)
	for _, y := range ys {
		if y.started {
			yields++
		}
	}
	return yields - checks
}

// checkYields fails unless every timer of ys yielded at every boundary
// but its first read, which starts its interval, and the timers yielded
// at least once per 256 of the values of the largest unit of work.
func checkYields(t *testing.T, what string, ys []*yielder, unitValues int) {
	t.Helper()
	for i, y := range ys {
		if y.yields < y.checks-1 {
			t.Errorf("%s: timer %d yielded %d times at %d boundaries", what, i, y.yields, y.checks)
		}
	}
	if got := inner(ys); got < unitValues/yieldCheck {
		t.Errorf("%s: %d reads inside units, want at least %d for the unit of %d values", what, got, unitValues/yieldCheck, unitValues)
	}
}

// siteNames names the sites for messages.
func siteNames() [siteCount]string {
	return [siteCount]string{
		"item", "kind", "sorted", "sort", "duplicate", "resource", "value", "join", "hash", "copy", "node", "token",
		"element", "check", "member", "walk member", "walk item", "offer", "keep sort", "list", "document", "chunk",
		"sequence", "envelope", "dump",
	}
}

// due is the yield option under which every read of a stepClock finds
// YieldAfter passed, so every read of the timer yields.
func due() (func() time.Time, time.Duration) {
	c := &stepClock{step: time.Microsecond}
	return c.now, time.Microsecond
}

// TestYielderTimer covers the yield timer on one goroutine: no clock
// read to build it, the first read starting the interval, no read below
// 256 units, one read when they reach it, also in one charge, a yield
// once YieldAfter has passed since the last one and none before, a
// second read after the yield to restart the interval, a read at every
// boundary between units, which also restarts the count, and nothing at
// all without a clock or for a nil timer; zero and negative YieldAfter
// mean DefaultYieldAfter.
func TestYielderTimer(t *testing.T) {
	var at time.Duration
	reads := 0
	now := func() time.Time {
		reads++
		return time.Unix(0, 0).Add(at)
	}
	y := newYielder(now, 100*time.Microsecond)
	if reads != 0 {
		t.Fatalf("%d reads to build the timer, want none", reads)
	}
	at = time.Hour
	y.check(siteElement)
	if reads != 1 || y.yields != 0 || y.last != time.Unix(0, 0).Add(at) {
		t.Fatalf("first read: %d reads, %d yields, last %v; want the interval started", reads, y.yields, y.last)
	}
	at = 0
	y.last = time.Unix(0, 0)
	y.charge(siteNode, yieldCheck-1)
	if reads != 1 {
		t.Errorf("%d clock reads below %d units", reads-1, yieldCheck)
	}
	at = 99 * time.Microsecond
	y.charge(siteNode, 1)
	if reads != 2 || y.yields != 0 {
		t.Errorf("at 99 µs: %d reads, %d yields; want 2 reads, no yield", reads, y.yields)
	}
	at = 100 * time.Microsecond
	y.charge(siteValue, 3*yieldCheck)
	if reads != 4 || y.yields != 1 || y.last != time.Unix(0, 0).Add(at) {
		t.Errorf("at 100 µs: %d reads, %d yields, last %v; want 4 reads (check and restart), 1 yield", reads, y.yields, y.last)
	}
	y.charge(siteNode, yieldCheck-1)
	y.check(siteElement)
	if reads != 5 || y.yields != 1 || y.checks != 2 || y.units != 0 {
		t.Errorf("a boundary right after a yield: %d reads, %d yields, %d checks, %d units", reads, y.yields, y.checks, y.units)
	}
	y.charge(siteNode, yieldCheck-1)
	if reads != 5 {
		t.Errorf("the boundary did not restart the count: %d reads", reads)
	}
	if y.sites[siteNode] != 3*yieldCheck-2 || y.sites[siteValue] != 3*yieldCheck || y.sites[siteElement] != 2 {
		t.Errorf("sites %v", y.sites)
	}
	for _, idle := range []*yielder{newYielder(nil, time.Microsecond), nil} {
		idle.charge(siteNode, 10*yieldCheck)
		idle.check(siteElement)
		if idle != nil && (idle.yields != 0 || idle.checks != 0 || idle.sites != [siteCount]int{}) {
			t.Errorf("timer without a clock: %+v", idle)
		}
	}
	for _, tc := range []struct{ in, want time.Duration }{{0, DefaultYieldAfter}, {-time.Second, DefaultYieldAfter}, {time.Nanosecond, time.Nanosecond}, {time.Second, time.Second}} {
		if got := yieldAfter(tc.in); got != tc.want {
			t.Errorf("yieldAfter(%v) = %v, want %v", tc.in, got, tc.want)
		}
		if got := NewDecoder(nil, DecoderOptions{YieldAfter: tc.in}).yieldAfter; got != tc.want {
			t.Errorf("DecoderOptions{YieldAfter: %v}: %v, want %v", tc.in, got, tc.want)
		}
		if got := NewEncoder(nil).newYielder(Options{Now: now, YieldAfter: tc.in}).after; got != tc.want {
			t.Errorf("Options{YieldAfter: %v}: %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestYieldAfterHonored: on a clock that advances 1 µs per read, a timer
// with YieldAfter 50 µs read at every boundary yields once per 51 reads
// (50 boundaries without a yield, then the yielding read and the restart
// read).
func TestYieldAfterHonored(t *testing.T) {
	c := &stepClock{step: time.Microsecond}
	y := newYielder(c.now, 50*time.Microsecond)
	for range 5100 {
		y.check(siteElement)
	}
	if y.yields < 95 || y.yields > 105 {
		t.Errorf("%d yields over 5,100 boundaries, want about 100", y.yields)
	}
}

// valueCount returns the values of a tree: one per node.
func valueCount(n *tree.Node) int {
	if n == nil {
		return 0
	}
	c := 1
	for _, m := range n.Members {
		c += valueCount(m.Value)
	}
	for _, it := range n.Items {
		c += valueCount(it)
	}
	return c
}

// tokenCounts returns the values and the tokens of one JSON text.
func tokenCounts(t *testing.T, data []byte) (values, tokens int) {
	t.Helper()
	s := jsonval.NewScanner(data, jsonval.ScanOptions{MaxDepth: jsonval.MaxDepthLimit})
	for {
		tok, err := s.Next()
		if errors.Is(err, io.EOF) {
			return values, tokens
		}
		if err != nil {
			t.Fatal(err)
		}
		tokens++
		switch tok.Kind {
		case jsonval.KindName, jsonval.KindObjectEnd, jsonval.KindArrayEnd:
		default:
			values++
		}
	}
}

// pieces returns the pieces a copy or hash of n bytes takes.
func pieces(n int) int { return max(1, (n+piece-1)/piece) }

// yieldBundle is a Bundle of 121 small resources and one Plugin of more
// than 2,000 values, so a unit of work holds more than 256 values.
func yieldBundle(t *testing.T) (*hub.Bundle, int) {
	t.Helper()
	big := resourceOf(t, jsonTree(t, largeResource(300)))
	meta, _ := big.Tree.Get("metadata")
	spec, _ := big.Tree.Get("spec")
	return hub.NewBundle(append(slices.Clone(largeBundle(t, 40).Resources()), big)), valueCount(meta) + valueCount(spec)
}

// TestYieldEncode: every goroutine of an Encode or EncodeResources reads
// its yield timer at every site, with one worker and with several, and
// yields when YieldAfter has passed: between resources, chunks and pieces
// of the hash, and every 256 values inside one resource. On a clock where
// YieldAfter passes at every read, each site counts its exact units, every
// timer yields at each boundary, and the resource of more than 2,000 values
// yields inside itself; each pooled goroutine has a timer of its own.
func TestYieldEncode(t *testing.T) {
	b, bigValues := yieldBundle(t)
	n := len(b.Resources())
	values := 0
	for _, r := range b.Resources() {
		meta, _ := r.Tree.Get("metadata")
		spec, _ := r.Tree.Get("spec")
		values += valueCount(meta) + valueCount(spec)
	}
	enc := NewEncoder(hubIndex(t))
	var o observed
	enc.observe = o.add
	envelopes := make([]tree.Resource, n)
	for i, r := range b.Resources() {
		envelopes[n-1-i] = tree.Resource{ID: r.ID, Root: r.Tree}
	}
	names := siteNames()
	for _, w := range []int{1, 4} {
		for _, mode := range []string{"Encode", "EncodeResources"} {
			now, after := due()
			opt := Options{Workers: w, Now: now, YieldAfter: after}
			var rev revision.Revision
			var err error
			if mode == "Encode" {
				rev, err = enc.Encode(context.Background(), b, opt)
			} else {
				rev, err = enc.EncodeResources(context.Background(), slices.Clone(envelopes), opt)
			}
			if err != nil {
				t.Fatal(err)
			}
			ys := o.take()
			sites, _, _ := total(ys)
			what := mode + ", workers " + strconv.Itoa(w)
			chunks := chunkCount(n, w)
			want := map[site]int{
				siteItem: n, siteKind: n, siteDuplicate: n - 1, siteResource: n, siteValue: values,
				siteHash: pieces(len(rev.Content)),
			}
			if mode == "Encode" {
				// The Bundle is in canonical order: one comparison per pair.
				want[siteSorted] = n - 1
			}
			if chunks > 1 {
				want[siteJoin] = chunks
			}
			for s, name := range names {
				switch got, exact := sites[s], want[site(s)]; {
				case site(s) == siteSorted && mode == "EncodeResources":
					// Reversed input: the sortedness check stops at once.
					if got < 1 {
						t.Errorf("%s: site %s counted %d", what, name, got)
					}
				case site(s) == siteSort && mode == "EncodeResources":
					if got < n {
						t.Errorf("%s: site %s counted %d comparisons for %d resources", what, name, got, n)
					}
				case got != exact:
					t.Errorf("%s: site %s counted %d, want %d", what, name, got, exact)
				}
			}
			if wantYs := 1 + map[bool]int{true: min(w, chunks), false: 0}[w > 1 && chunks > 1]; len(ys) != wantYs {
				t.Errorf("%s: %d goroutines with a timer, want %d", what, len(ys), wantYs)
			}
			checkYields(t, what, ys, bigValues)
		}
	}
	// Without a clock nothing is counted and nothing yields.
	if _, err := enc.Encode(context.Background(), b, Options{Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if sites, checks, yields := total(o.take()); sites != [siteCount]int{} || checks != 0 || yields != 0 {
		t.Errorf("no clock: sites %v, %d checks, %d yields", sites, checks, yields)
	}
}

// decodeSites returns the units each decode site counts for content, a
// valid Revision of n resources in chunks that the parser completes.
func decodeSites(t *testing.T, content []byte, n int) map[site]int {
	t.Helper()
	values, _ := tokenCounts(t, content)
	chunks := (n + streamChunk - 1) / streamChunk
	return map[site]int{
		siteCopy: pieces(len(content)), siteNode: values, siteElement: n, siteCheck: n, siteMember: 3 * n,
		// The document, format and resources, then each resource's root
		// and kind, are not re-encoded through the writer.
		siteValue: values - 3 - 2*n, siteDocument: 2, siteChunk: 2 * chunks, siteSequence: n, siteEnvelope: n,
	}
}

// TestYieldDecode: every goroutine of a Decode, Verify or ReadDump reads
// its yield timer at every site, with one worker and with several, and
// yields when YieldAfter has passed: between elements parsed and checked,
// entries sequenced and built, and pieces copied and hashed, and every 256
// units inside one: values built, walked and re-encoded, tokens of a dump
// scanned. On a clock where YieldAfter passes at every read each site
// counts its exact units, every timer yields at each boundary, the resource
// of more than 2,000 values yields inside itself, and each goroutine the
// decode starts has a timer of its own.
func TestYieldDecode(t *testing.T) {
	b, bigValues := yieldBundle(t)
	content := mustEncodeBundle(t, b)
	n := len(b.Resources())
	rev := revision.Revision{Content: content, Digest: revision.Sum(content)}
	body := dumpOf(t, nil, rev, rev.Digest)
	_, contentTokens := tokenCounts(t, content)
	names := siteNames()
	for _, w := range []int{1, 4} {
		for _, mode := range []string{"Decode", "Verify", "ReadDump"} {
			now, after := due()
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, Now: now, YieldAfter: after})
			var o observed
			dec.observe = o.add
			var err error
			switch mode {
			case "Decode":
				_, err = dec.Decode(context.Background(), content, Source{})
			case "Verify":
				_, err = dec.Verify(context.Background(), content, rev.Digest, Source{})
			default:
				_, _, err = dec.ReadDump(context.Background(), bytes.NewReader(body), 0, Source{})
			}
			if err != nil {
				t.Fatal(err)
			}
			ys := o.take()
			sites, _, _ := total(ys)
			what := mode + ", workers " + strconv.Itoa(w)
			want := decodeSites(t, content, n)
			if mode != "Decode" {
				want[siteHash] = pieces(len(content))
			}
			if mode == "ReadDump" {
				// content, digest, lastKnownGood and revision, and the
				// content's tokens after its first, skipped by the scan.
				want[siteDump] = 4
				want[siteToken] = contentTokens - 1
			}
			for s, name := range names {
				got := sites[s]
				switch site(s) {
				case siteWalkMember, siteWalkItem:
					if got < n {
						t.Errorf("%s: site %s counted %d", what, name, got)
					}
				default:
					if got != want[site(s)] {
						t.Errorf("%s: site %s counted %d, want %d", what, name, got, want[site(s)])
					}
				}
			}
			if w > 1 && len(ys) < 2 {
				t.Errorf("%s: %d goroutines with a timer, want the caller's and at least one worker's", what, len(ys))
			}
			checkYields(t, what, ys, bigValues)
		}
	}
}

// TestYieldDecodeFindings: the sites only content with findings or with
// values the parser scans without building reaches: a finding offered, the
// comparisons sorting the kept ones and the findings listed, and the
// tokens of a document member that only counts as newer.
func TestYieldDecodeFindings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		sites   []site
	}{
		{"newer findings", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"y":1,"z":1}}`), []site{siteOffer, siteKeepSort, siteList}},
		{"a newer document member", `{"format":"ruralz.canonical.v1","resources":[],"x":[1,2,3]}`, []site{siteToken, siteOffer, siteList}},
		{"form findings", doc(`1`, `2`), []site{siteOffer, siteKeepSort, siteList}},
	} {
		for _, w := range []int{1, 4} {
			now, after := due()
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, Now: now, YieldAfter: after})
			var o observed
			dec.observe = o.add
			if _, err := dec.Decode(context.Background(), []byte(tc.content), Source{}); err == nil {
				t.Fatalf("%s: no error", tc.name)
			}
			sites, _, _ := total(o.take())
			for _, s := range tc.sites {
				if sites[s] == 0 {
					t.Errorf("%s, workers %d: site %s counted nothing", tc.name, w, siteNames()[s])
				}
			}
		}
	}
}

// TestYieldGivesUpProcessor: a yield gives the processor to another
// goroutine (runtime.Gosched). With one P, a goroutine that counts its
// turns runs about once per yield of a decode and an encode on a clock
// where YieldAfter passes at every read; without the yields it runs only
// when async preemption or the end of the call lets it (0 to 3 times).
func TestYieldGivesUpProcessor(t *testing.T) {
	b, _ := yieldBundle(t)
	content := mustEncodeBundle(t, b)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, mode := range []string{"Decode", "Encode"} {
		now, after := due()
		var o observed
		var turns atomic.Int64
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				turns.Add(1)
				runtime.Gosched()
			}
		}()
		runtime.Gosched()
		turns.Store(0)
		var err error
		if mode == "Decode" {
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1, Now: now, YieldAfter: after})
			dec.observe = o.add
			_, err = dec.Decode(context.Background(), content, Source{})
		} else {
			enc := NewEncoder(hubIndex(t))
			enc.observe = o.add
			_, err = enc.Encode(context.Background(), b, Options{Workers: 1, Now: now, YieldAfter: after})
		}
		got := turns.Load()
		stop.Store(true)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		_, _, yields := total(o.take())
		t.Logf("%s: the other goroutine ran %d times during %d yields", mode, got, yields)
		if got < int64(yields/4) {
			t.Errorf("%s: the other goroutine ran %d times during %d yields", mode, got, yields)
		}
	}
}

// panickingNow is a clock that panics with errInjected at its at-th read
// made at a site whose function name holds site (the first function on
// the stack that is not the timer's), from the goroutine mine when onMine
// and from any other when not; every other read advances a step clock on
// which YieldAfter passes at every read.
type panickingNow struct {
	site   string
	mine   string
	onMine bool
	at     int64
	calls  atomic.Int64
	fired  atomic.Bool
	clock  stepClock
}

func (c *panickingNow) now() time.Time {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if !strings.Contains(f.Function, "(*yielder).") {
			if strings.Contains(f.Function, c.site) && (goroutineID() == c.mine) == c.onMine && c.calls.Add(1) == c.at {
				c.fired.Store(true)
				panic(errInjected)
			}
			break
		}
		if !more {
			break
		}
	}
	return c.clock.now()
}

// TestNowPanicSurfaces: a panic in Options.Now or DecoderOptions.Now
// surfaces as a panic in the work that read the clock does (package
// documentation, Concurrency): read while resources are checked or
// encoded, on a goroutine the call started or on the calling one, it is
// raised again as a *workPanic once every goroutine has returned; read by
// the parser, the sort or the hash, it is raised as it is, after the
// goroutines the call started were stopped and waited for. No goroutine is
// left behind, and the decoder and encoder work after.
func TestNowPanicSurfaces(t *testing.T) {
	// The Plugin of more than 2,000 values makes the parser read the
	// clock inside a unit.
	yb, _ := yieldBundle(t)
	content := mustEncodeBundle(t, yb)
	n := len(yb.Resources())
	b := largeBundle(t, 50)
	enc := NewEncoder(hubIndex(t))
	want, err := enc.Encode(context.Background(), b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		panicCase
		call string
	}{
		{panicCase{"a check on a worker", "(*checker).run", false, 3, []int{4}, true}, "Decode"},
		{panicCase{"a check on the calling goroutine", "(*checker).run", true, 2, []int{1, 4}, true}, "Decode"},
		{panicCase{"the parser", "(*parser).node", true, 2, []int{1, 4}, false}, "Decode"},
		{panicCase{"the parser between elements", "(*Decoder).stream", true, 100, []int{1, 4}, false}, "Decode"},
		{panicCase{"the hash of Verify", "canonical.sum", true, 1, []int{1, 4}, false}, "Verify"},
		{panicCase{"an encode worker", "(*Encoder).encode.func", false, 3, []int{4}, true}, "Encode"},
		{panicCase{"an encode on the calling goroutine", "(*Encoder).encode.func", true, 3, []int{1}, true}, "Encode"},
		{panicCase{"the order", "(*Encoder).order", true, 1, []int{1, 4}, false}, "Encode"},
		{panicCase{"the hash of Encode", "canonical.sum", true, 1, []int{1, 4}, false}, "Encode"},
	} {
		for _, w := range tc.workers {
			base := runtime.NumGoroutine()
			c := &panickingNow{site: tc.site, mine: goroutineID(), onMine: tc.onMine, at: tc.at, clock: stepClock{step: time.Microsecond}}
			v := recovered(func() {
				switch tc.call {
				case "Decode":
					_, _ = NewDecoder(hubIndex(t), DecoderOptions{Workers: w, Now: c.now, YieldAfter: time.Microsecond}).Decode(context.Background(), content, Source{})
				case "Verify":
					_, _ = NewDecoder(hubIndex(t), DecoderOptions{Workers: w, Now: c.now, YieldAfter: time.Microsecond}).Verify(context.Background(), content, revision.Sum(content), Source{})
				default:
					_, _ = enc.Encode(context.Background(), b, Options{Workers: w, Now: c.now, YieldAfter: time.Microsecond})
				}
			})
			if !c.fired.Load() {
				t.Fatalf("%s, workers %d: the panic was not injected", tc.name, w)
			}
			checkSurfaced(t, tc.panicCase, w, v)
			settles(t, base)
			if tc.call == "Encode" {
				got, err := enc.Encode(context.Background(), b, Options{Workers: w})
				if err != nil {
					t.Fatal(err)
				}
				sameBytes(t, tc.name+" then encode", got.Content, want.Content)
			} else if rs, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w}).Decode(context.Background(), content, Source{}); err != nil || len(rs) != n {
				t.Errorf("%s, workers %d: the decode after: %d resources, %v", tc.name, w, len(rs), err)
			}
		}
	}
}
