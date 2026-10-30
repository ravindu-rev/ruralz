// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// LagSampleEvery is how often CE-5 samples the replica's offset lag
// before the kill (hypothesis).
const LagSampleEvery = 100 * time.Millisecond

// OffsetSample is one INFO replication reading of a primary: its
// master_repl_offset and its replica's acknowledged offset.
type OffsetSample struct {
	At              time.Time
	Master, Replica int64
}

// LagTracker samples a primary's replication offsets on a period.
type LagTracker struct {
	mu      sync.Mutex
	samples []OffsetSample
	errs    int
	stop    chan struct{}
	done    chan struct{}
}

// TrackLag samples primary every LagSampleEvery until Stop or the end of
// ctx.
func TrackLag(ctx context.Context, primary *redisserver.Server) *LagTracker {
	lt := &LagTracker{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(lt.done)
		ticker := time.NewTicker(LagSampleEvery)
		defer ticker.Stop()
		for {
			sctx, cancel := context.WithTimeout(ctx, LagSampleEvery)
			m, r, err := ReplicationOffsets(sctx, primary)
			cancel()
			lt.mu.Lock()
			if err != nil {
				lt.errs++
			} else {
				lt.samples = append(lt.samples, OffsetSample{At: time.Now(), Master: m, Replica: r})
			}
			lt.mu.Unlock()
			select {
			case <-lt.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return lt
}

// Stop stops sampling and returns the samples, oldest first.
func (lt *LagTracker) Stop() []OffsetSample {
	select {
	case <-lt.stop:
	default:
		close(lt.stop)
	}
	<-lt.done
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return append([]OffsetSample(nil), lt.samples...)
}

// ReplicaLag returns L, the replica's lag in time at the last sample: how
// long before it the primary had reached the offset the replica had
// acknowledged by then, at the samples' resolution; 0 for a replica in
// step. ok is false without samples, or when the replica trails every
// sample, which then bounds L from below.
func ReplicaLag(samples []OffsetSample) (time.Duration, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	last := samples[len(samples)-1]
	if last.Replica >= last.Master {
		return 0, true
	}
	for i, s := range samples {
		if s.Master >= last.Replica {
			// Past the replica at the first sample: the primary got there
			// earlier still, so the lag is only bounded from below.
			return last.At.Sub(s.At), i > 0 || s.Master == last.Replica
		}
	}
	return last.At.Sub(samples[0].At), false
}

// ShardOf returns the index among primaries of the shard owning slot,
// read from the first primary's CLUSTER SLOTS; 0 for a deployment that is
// not a cluster.
func ShardOf(ctx context.Context, d *redisserver.Deployment, slot uint16) (int, error) {
	if d.Topology() != statestore.TopologyCluster {
		return 0, nil
	}
	primaries := d.Nodes()
	v, err := primaries[0].Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		return 0, fmt.Errorf("chaos: CLUSTER SLOTS: %w", err)
	}
	for _, r := range v.Elems {
		if len(r.Elems) < 3 || len(r.Elems[2].Elems) < 2 {
			continue
		}
		if int64(slot) < r.Elems[0].Int || int64(slot) > r.Elems[1].Int {
			continue
		}
		addr := net.JoinHostPort(r.Elems[2].Elems[0].Str, strconv.FormatInt(r.Elems[2].Elems[1].Int, 10))
		for i, p := range primaries {
			if p.ClientAddr() == addr || p.Addr() == addr {
				return i, nil
			}
		}
		return 0, fmt.Errorf("chaos: slot %d is on %s, no primary", slot, addr)
	}
	return 0, fmt.Errorf("chaos: slot %d has no owner", slot)
}

// LimitedKeySlot returns the slot of a Consumer's /limited and
// /limited-closed rate limit keys, whose hash tag is the digest of
// "rl:" + consumer.name.
func LimitedKeySlot(consumer string) uint16 {
	return keys.DigestSlot(statestore.DigestOf("rl:" + consumer))
}
