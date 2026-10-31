// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// streamChunk is how many consecutive resources make one unit of decode
// work: large enough that handing one over costs little next to checking
// it, small enough that workers start while the parser is still early in a
// large Revision.
const streamChunk = 32

// errPanicked stops the parse once a check panicked: the decode raises
// that panic once its goroutines have returned.
var errPanicked = errors.New("canonical: a check panicked")

// chunk is a run of consecutive elements of the top-level resources array
// and the results of their checks. The parser goroutine fills items and
// spans before handing the chunk over; the checker that receives it owns
// the results until stream returns, and drops items and spans once it has
// checked them. Findings, and a context error a checker meets, go to the
// decode's run.
type chunk struct {
	// lo is the index of the first element in the array.
	lo    int
	items []*tree.Node
	spans [][2]int
	// entries holds one entry per element whose envelope tree could be
	// built, in order, while the decode can still succeed (run.fails).
	entries []entry
	// done is set once the checker is done with the chunk, after which
	// only the parser goroutine reads it (sweep).
	done atomic.Bool
}

// stream parses the content and checks each element of its top-level
// resources array as soon as the parser has completed it: the envelope
// rules, the newer walk and the re-encoding. A decode runs on d.workers
// goroutines at most: the calling goroutine parses and, each time a chunk
// fills, hands it to the goroutines stream owns through a channel of
// capacity d.workers-1, starting one more goroutine with each chunk it
// hands over until there are d.workers-1, so a decode never runs more
// goroutines than it has chunks. When the channel is full the parser
// checks the chunk itself instead of queueing more or waiting. With one
// worker, or content too small to fill a chunk while parsing, the checks
// run on the calling goroutine, as do those of the last chunk. Once the
// parse ends, successfully or not, stream checks the elements the parser
// completed that no chunk holds yet, closes the channel and waits for
// every goroutine before it returns, on every path out of it: success,
// failure, cancellation and a panic.
//
// The parser stops early once the newer findings of r passed the cap
// (errStop), at a format other than ruralz.canonical.v1 (errOther), at a
// limit of the decoder (CodeLimit) or at invalid JSON (CodeMismatch); end
// is that reason, or nil when it read the whole content. A worker skips
// the rest of its chunks once ctx is done, and the parser checks ctx
// between resources; err is ctx's error, which wins over every other
// outcome. A panic in a check, on any goroutine, stops the parse and the
// remaining checks and is raised again once every goroutine has returned
// (workPanic).
//
// y is the calling goroutine's yield timer, which the parser and the
// checks made on that goroutine share; each goroutine stream starts has
// its own. r counts the goroutines started and the chunks handed over and
// in flight (completed by the parser and not yet checked), which the
// tests of these bounds read.
func (d *Decoder) stream(ctx context.Context, in *input, r *run, y *yielder) (doc *tree.Node, chunks []*chunk, end, err error) {
	var (
		cur      *chunk
		work     chan *chunk
		wg       sync.WaitGroup
		ps       panics
		own      = d.newChecker(in, r, y)
		canceled error
	)
	// join closes the channel and waits for the workers, once, whichever
	// way stream returns.
	joined := false
	join := func() {
		if work != nil && !joined {
			joined = true
			close(work)
			wg.Wait()
		}
	}
	defer join()
	check := func(c *checker, ch *chunk) {
		if !ps.stop.Load() {
			ps.run(func() { c.run(ctx, ch) })
		}
	}
	// flush hands the current chunk to a worker, or checks it on the
	// calling goroutine when there is one worker, when the channel is full
	// (the workers are behind, so waiting for them would only idle the
	// parser) or when the parse has ended.
	flush := func(inline bool) {
		if cur == nil {
			return
		}
		ch := cur
		cur = nil
		r.busy(1)
		// A chunk is kept for its entries only while the decode can still
		// succeed; once it is known to fail, none of its elements gets
		// one, and keeping it would cost memory linear in the content.
		// The chunks checked before a finding was known are dropped too.
		chunks = sweep(r, chunks)
		if !r.fails(ch.lo) {
			chunks = append(chunks, ch)
		}
		if !inline && d.workers > 1 {
			if work == nil {
				work = make(chan *chunk, d.workers-1)
			}
			select {
			case work <- ch:
				r.handed++
				if r.started < d.workers-1 {
					r.started++
					wg.Go(func() {
						wy := d.newYielder()
						defer d.done(wy)
						c := d.newChecker(in, r, wy)
						for ch := range work {
							check(c, ch)
							r.busy(-1)
						}
					})
				}
				return
			default:
			}
		}
		check(own, ch)
		r.busy(-1)
	}
	each := func(i int, rn *tree.Node, lo, hi int) error {
		if err := ctx.Err(); err != nil {
			canceled = err
			return err
		}
		if ps.stop.Load() {
			return errPanicked
		}
		if r.newer.full.Load() {
			// The findings kept all lie before this element.
			return errStop
		}
		y.check(siteElement)
		if cur == nil {
			cur = &chunk{lo: i, items: make([]*tree.Node, 0, streamChunk), spans: make([][2]int, 0, streamChunk)}
		}
		cur.items = append(cur.items, rn)
		cur.spans = append(cur.spans, [2]int{lo, hi})
		if len(cur.items) == streamChunk {
			flush(false)
		}
		return nil
	}
	doc, end = d.parse(in, r, y, each)
	if canceled == nil {
		// The parser is done, so its goroutine checks the elements it
		// completed last while the workers finish their chunks: after
		// invalid JSON, a limit or another format too, so the newer
		// findings before that point are all known.
		flush(true)
	}
	join()
	ps.raise()
	if canceled == nil {
		canceled = r.canceledBy()
	}
	if canceled != nil {
		return nil, nil, nil, canceled
	}
	return doc, chunks, end, nil
}

// checker is the scratch of one goroutine's resource checks: the writer
// of the re-encoding, its buffer, the findings and the identity of the
// current resource, the newer walk, and the goroutine's yield timer.
type checker struct {
	d     *Decoder
	in    *input
	r     *run
	y     *yielder
	w     writer
	buf   []byte
	found found
	who   ident
	nw    newerWalk
}

func (d *Decoder) newChecker(in *input, r *run, y *yielder) *checker {
	return &checker{d: d, in: in, r: r, y: y, w: writer{y: y}, found: found{limit: d.maxDiags + 1}, nw: newerWalk{d: d, in: in, y: y}}
}

// run checks the elements of ch: the envelope rules and the newer walk,
// offering their findings to the run, and, for each element with an
// entry, the re-encoding while the decode can still succeed. It stops at
// the first element whose findings all come after the newer findings
// kept: none of them can be kept, and with a newer finding known neither
// form findings nor the re-encoding matter. Form findings are not built
// once a newer finding is known or none of the element's can be kept.
// An element's entry, and with it its tree, is kept only while the decode
// can still succeed (run.fails): once it is known to fail, the trees the
// checks are done with are unlinked and garbage, so a failing decode
// holds no more than the elements in flight. The re-encoding is wasted
// when a later part of the content has a finding; its result is then not
// read.
func (c *checker) run(ctx context.Context, ch *chunk) {
	j := 0
	defer func() {
		// The elements left unchecked are dropped too.
		for ; j < len(ch.items); j++ {
			unlink(ch.items[j])
		}
		ch.items, ch.spans = nil, nil
		ch.done.Store(true)
	}()
	content := c.in.content()
	r := c.r
	for ; j < len(ch.items); j++ {
		rn := ch.items[j]
		if err := ctx.Err(); err != nil {
			r.cancel(err)
			return
		}
		i := ch.lo + j
		if r.newer.after(i) {
			return
		}
		c.y.check(siteCheck)
		r.checked.Add(1)
		c.found.reset(!r.newer.any() && !r.form.after(i))
		e := c.resource(rn, i)
		r.newer.addResource(i, c.found.newer, c.y)
		if !r.newer.any() {
			r.form.addResource(i, c.found.form, c.y)
		}
		if e.node == nil || r.fails(i) {
			unlink(rn)
			continue
		}
		e.start, e.end = ch.spans[j][0], ch.spans[j][1]
		c.buf = c.d.form(&c.w, c.buf[:0], &e, content, c.in)
		if e.form != nil || e.diff >= 0 {
			r.reject(i)
		}
		if ch.entries == nil {
			ch.entries = make([]entry, 0, len(ch.items)-j)
		}
		ch.entries = append(ch.entries, e)
	}
}

// sweep returns chunks without those whose entries the decode can no
// longer read: once a finding is known, the decode fails whatever the
// content holds, so the entries of every chunk whose check is done are
// unlinked and dropped, those a check made before the finding was known
// included (with several workers, those checked while another goroutine
// was still checking an earlier element that had a finding). A chunk
// still in check stays for a later sweep. Before any finding there is
// nothing to drop and sweep does no work; after one, chunks holds only
// the chunks in flight, since no further chunk is kept, so each sweep
// costs at most 2(Workers-1)+1 steps. A rejected re-encoding alone
// sweeps nothing: the entries up to the rejected resource still decide
// the outcome (sequence), and those after it that were checked before
// the rejection was known are kept until the decode returns, as a decode
// that succeeds keeps its own. Only the parser goroutine calls sweep.
func sweep(r *run, chunks []*chunk) []*chunk {
	if !r.newer.any() && !r.form.any() {
		return chunks
	}
	keep := chunks[:0]
	for _, ch := range chunks {
		if !ch.done.Load() {
			keep = append(keep, ch)
			continue
		}
		for i := range ch.entries {
			unlink(ch.entries[i].node)
		}
		ch.entries = nil
	}
	clear(chunks[len(keep):])
	return keep
}

// unlink clears the member and item links of the tree under n, an element
// the decode no longer needs. Its nodes, members and items come from
// slabs it shares with the elements before and after it, which hold
// links to each other's nodes, so a dropped element whose links stayed
// would keep its neighbors' slabs alive, and through them, from the
// parser's current slabs, every element before it. The checker that
// received the element owns it: the parser never writes to it again.
func unlink(n *tree.Node) {
	for i := range n.Members {
		unlink(n.Members[i].Value)
	}
	for _, it := range n.Items {
		unlink(it)
	}
	n.Members, n.Items = nil, nil
}
