// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
)

// CallLogEvery is how often a CallLog drains the counting RESP proxies'
// command logs, often enough that none overflows at a run's rates.
const CallLogEvery = 100 * time.Millisecond

// Call is one State Store command naming a watched key, as a Node's
// counting RESP proxy read it.
type Call struct {
	Node string
	At   time.Time
	Name string
	Key  string
}

// CallLog collects, from every Node's counting RESP proxies, the commands
// whose arguments name a rate limit key of the watched Policies
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing":
// CE-15's GCRA calls cross-checked through the counting RESP proxy).
type CallLog struct {
	r        *Run
	store    string
	policies []string

	mu     sync.Mutex
	calls  []Call
	seqs   map[*respproxy.Proxy]uint64
	missed int64

	stop chan struct{}
	done chan struct{}
}

// StartCallLog starts collecting the calls naming a key of policies on
// the State Store named store, which must run behind counting RESP
// proxies (StateStoreOptions.RESPProxy).
func (r *Run) StartCallLog(ctx context.Context, store string, policies ...string) *CallLog {
	cl := &CallLog{r: r, store: store, policies: policies, seqs: map[*respproxy.Proxy]uint64{}, stop: make(chan struct{}), done: make(chan struct{})}
	for _, n := range r.Top.Nodes() {
		ps, _ := n.RESPProxy(store)
		for _, p := range ps {
			cl.seqs[p] = p.Snapshot().Seq
		}
	}
	go func() {
		defer close(cl.done)
		ticker := time.NewTicker(CallLogEvery)
		defer ticker.Stop()
		for {
			select {
			case <-cl.stop:
				cl.drain()
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				cl.drain()
			}
		}
	}()
	return cl
}

// drain reads every proxy's commands since the last drain.
func (cl *CallLog) drain() {
	for _, n := range cl.r.Top.Nodes() {
		ps, _ := n.RESPProxy(cl.store)
		for _, p := range ps {
			cl.mu.Lock()
			seq := cl.seqs[p]
			cl.mu.Unlock()
			cmds := p.LogSince(seq)
			cl.mu.Lock()
			for _, c := range cmds {
				if c.Seq > seq+1 {
					cl.missed += int64(c.Seq - seq - 1)
				}
				seq = c.Seq
				for _, a := range c.Args {
					if cl.watched(a) {
						cl.calls = append(cl.calls, Call{Node: n.Name(), At: c.At, Name: c.Name, Key: a})
					}
				}
			}
			cl.seqs[p] = seq
			cl.mu.Unlock()
		}
	}
}

// watched reports whether arg is a rate limit key of a watched Policy.
func (cl *CallLog) watched(arg string) bool {
	for _, p := range cl.policies {
		if strings.HasPrefix(arg, keys.PrefixRateLimit+p+":") {
			return true
		}
	}
	return false
}

// Stop stops collecting after a last drain and returns the calls and how
// many commands the proxies' logs dropped before a drain read them.
func (cl *CallLog) Stop() ([]Call, int64) {
	select {
	case <-cl.stop:
	default:
		close(cl.stop)
	}
	<-cl.done
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return append([]Call(nil), cl.calls...), cl.missed
}

// KeyTag returns the hash tag of the rate limit key a Policy keyed by
// value names: the hex SHA-256 of value.
func KeyTag(value string) string {
	d := statestore.DigestOf(value)
	return hex.EncodeToString(d[:])
}

// CallsPerWindow counts the calls naming a key with tag per window of
// the Nodes' clock (span maps a real instant to the Node times it may
// show; a call counts in the window of its earliest Node time) over
// calls read in [from, to), and returns the fullest window's count.
func CallsPerWindow(calls []Call, tag string, span func(time.Time) (lo, hi time.Time), window time.Duration, from, to time.Time) (worst int64, total int64) {
	per := map[int64]int64{}
	for _, c := range calls {
		if c.At.Before(from) || !c.At.Before(to) || keys.HashTag(c.Key) != tag {
			continue
		}
		lo, _ := span(c.At)
		w := lo.UnixNano() / int64(window)
		per[w]++
		total++
	}
	for _, n := range per {
		worst = max(worst, n)
	}
	return worst, total
}
