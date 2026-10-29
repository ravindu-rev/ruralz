// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package postcommit

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// classAt returns the class whose turn t is: settlement first and
// invalidations second in every round of three, then stores in four
// rounds of five and Plugin writes in the fifth.
func classAt(t uint64) statestore.WriteClass {
	switch i := t % Turns; {
	case i%3 == 0:
		return statestore.ClassSettle
	case i%3 == 1:
		return statestore.ClassInvalidate
	case i == Turns-1:
		return statestore.ClassPlugin
	default:
		return statestore.ClassStore
	}
}

// next takes the next turn and pops one batch from its class, or from
// the first non-empty class after it; 0 means every class is empty.
func (q *Queue) next(dst []item) (int, statestore.WriteClass) {
	t := q.turn.Add(1) - 1
	for i := range uint64(Turns) {
		cl := classAt(t + i)
		if n := q.classes[cl-1].pop(dst, q.depth()); n > 0 {
			return n, cl
		}
	}
	return 0, 0
}

// run is one writer: it writes batches until ctx ends, waiting for work
// when every class is empty.
func (q *Queue) run(ctx context.Context) {
	items := make([]item, q.lim.Batch)
	writes := make([]*statestore.Write, q.lim.Batch)
	for ctx.Err() == nil {
		n, cl := q.next(items)
		if n == 0 {
			select {
			case <-ctx.Done():
			case <-q.wake:
			}
			continue
		}
		if q.Len() > 0 {
			// More work: wake another writer before this one is busy.
			q.signal()
		}
		q.write(ctx, cl, items[:n], writes)
		clear(items[:n])
	}
}

// write sends one batch of class cl, one Store.Write per Target in the
// order the Targets first appear, then settles every write.
func (q *Queue) write(ctx context.Context, cl statestore.WriteClass, items []item, writes []*statestore.Write) {
	var taken uint64 // bit i: items[i] was sent; len(items) <= 64
	for i := range items {
		if taken&(1<<i) != 0 {
			continue
		}
		b := items[i].b
		group := writes[:0]
		for j := i; j < len(items); j++ {
			if taken&(1<<j) == 0 && items[j].b == b {
				taken |= 1 << j
				group = append(group, items[j].w)
			}
		}
		q.send(ctx, b.t, group)
		clear(group)
		for j := i; j < len(items); j++ {
			if items[j].b == b {
				q.settle(cl, items[j])
			}
		}
	}
}

// send calls t.Write; a panic fails every write the target left without
// an outcome.
func (q *Queue) send(ctx context.Context, t Target, batch []*statestore.Write) {
	for _, w := range batch {
		w.Err, w.Applied = nil, false
	}
	defer func() {
		if v := recover(); v != nil {
			for _, w := range batch {
				if w.Err == nil && !w.Applied {
					w.Err = statestore.ErrFailed
				}
			}
			q.log.ErrorContext(ctx, "post-commit write panicked",
				slog.String(catalog.KeyPanicType, fmt.Sprintf("%T", v)), slog.String(catalog.KeyStack, string(debug.Stack())))
		}
	}()
	t.Write(ctx, batch)
}

// settle ends one written item: an invalidation that failed (RZ-STS-001,
// RZ-STS-002) is queued again once, and dropped when it fails again or
// cannot be queued; a write the store skipped (breaker open, not
// attempted, unsupported) is dropped at once; every other outcome is
// final. A dropped write is counted.
func (q *Queue) settle(cl statestore.WriteClass, it item) {
	if err := it.w.Err; err != nil {
		switch err.Failure() {
		case statestore.FailTimeout, statestore.FailError:
			if cl == statestore.ClassInvalidate {
				if !it.retry {
					it.retry = true
					if q.classes[cl-1].push(it, &q.closed, q.depth()) {
						q.signal()
						return
					}
				}
				q.dropped(it.w.Kind.WriteLabel())
			}
		default:
			q.dropped(it.w.Kind.WriteLabel())
		}
	}
	it.b.t.Release()
	q.done(1)
}
