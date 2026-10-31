// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// FillPrefix names the filler keys a run writes to fill a State Store.
const FillPrefix = "chaos:fill:"

// fillValue is one filler key's value size.
const fillValue = 32 << 10

// MemoryUse returns a server's used_memory and maxmemory from INFO
// memory.
func MemoryUse(ctx context.Context, s *redisserver.Server) (used, limit int64, err error) {
	info, err := s.Info(ctx, "memory")
	if err != nil {
		return 0, 0, err
	}
	if used, err = strconv.ParseInt(info["used_memory"], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("chaos: INFO memory used_memory = %q", info["used_memory"])
	}
	if limit, err = strconv.ParseInt(info["maxmemory"], 10, 64); err != nil || limit <= 0 {
		return 0, 0, fmt.Errorf("chaos: INFO memory maxmemory = %q", info["maxmemory"])
	}
	return used, limit, nil
}

// Fill writes filler keys to s, each with ttl when positive, until its
// used memory reaches share of maxmemory, a write is refused for memory
// (a full noeviction deployment), or it wrote twice maxmemory (a full
// deployment that evicts); it returns the keys written and whether a
// write was refused.
func Fill(ctx context.Context, s *redisserver.Server, share float64, ttl time.Duration, from int) (int, bool, error) {
	val := strings.Repeat("x", fillValue)
	n := from
	for {
		used, limit, err := MemoryUse(ctx, s)
		if err != nil {
			return n - from, false, err
		}
		if float64(used) >= share*float64(limit) || int64(n-from)*fillValue >= 2*limit {
			return n - from, false, nil
		}
		for range 32 {
			args := []string{"SET", FillPrefix + strconv.Itoa(n), val}
			if ttl > 0 {
				args = append(args, "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
			}
			refused, err := refusedForMemory(s.Do(ctx, args...))
			switch {
			case err != nil:
				return n - from, false, err
			case refused:
				return n - from, true, nil
			}
			n++
		}
	}
}

// pinMargin is how far under its used memory Pin sets a full
// deployment's maxmemory.
const pinMargin = 1 << 20

// Pin holds a full noeviction deployment full for CE-6's fault phase
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing"):
// it sets maxmemory pinMargin under the memory s uses, so the short-lived
// keys the Nodes write and expire, such as GCRA keys, never bring it back
// under maxmemory and every write that could grow it is refused until
// Unpin. It returns the maxmemory to restore.
func Pin(ctx context.Context, s *redisserver.Server) (int64, error) {
	used, limit, err := MemoryUse(ctx, s)
	if err != nil {
		return 0, err
	}
	pinned := max(1, used-pinMargin)
	if _, err := s.Do(ctx, "CONFIG", "SET", "maxmemory", strconv.FormatInt(pinned, 10)); err != nil {
		return 0, fmt.Errorf("chaos: pin maxmemory at %d: %w", pinned, err)
	}
	return limit, nil
}

// Unpin restores the maxmemory Pin lowered.
func Unpin(ctx context.Context, s *redisserver.Server, limit int64) error {
	if _, err := s.Do(ctx, "CONFIG", "SET", "maxmemory", strconv.FormatInt(limit, 10)); err != nil {
		return fmt.Errorf("chaos: restore maxmemory %d: %w", limit, err)
	}
	return nil
}

// refusedForMemory reads the outcome of a filler write: an OOM error
// reply, which redisserver returns as a *redisserver.ReplyError beside
// the reply, is a write a full noeviction deployment refused; any other
// error fails the fill.
func refusedForMemory(_ redisserver.Value, err error) (bool, error) {
	var re *redisserver.ReplyError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &re) && re.Code() == "OOM":
		return true, nil
	case errors.As(err, &re):
		return false, fmt.Errorf("chaos: SET filler: %s", re.Msg)
	default:
		return false, err
	}
}

// Unfill deletes the filler keys of s.
func Unfill(ctx context.Context, s *redisserver.Server) error {
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := s.Do(ctx, append([]string{"UNLINK"}, batch...)...)
		batch = batch[:0]
		return err
	}
	err := scanKeys(ctx, []*redisserver.Server{s}, FillPrefix+"*", func(_ *redisserver.Server, k string) error {
		batch = append(batch, k)
		if len(batch) == 256 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// EvictedKeys returns a server's evicted_keys from INFO stats.
func EvictedKeys(ctx context.Context, s *redisserver.Server) (int64, error) {
	info, err := s.Info(ctx, "stats")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(info["evicted_keys"], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chaos: INFO stats evicted_keys = %q", info["evicted_keys"])
	}
	return n, nil
}
