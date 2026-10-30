// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// QuotaCounters returns each Consumer's chaos-requests units charged in
// the State Store: the sum of its counters over every window
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas":
// rz:qt:<consumerQuota>:<window>:<start>:{<SHA-256 of the key>}, the key
// being consumer.name).
func QuotaCounters(ctx context.Context, servers []*redisserver.Server) (map[string]int64, error) {
	byTag := map[string]string{}
	for _, c := range Consumers() {
		d := statestore.DigestOf(c)
		byTag[hex.EncodeToString(d[:])] = c
	}
	out := map[string]int64{}
	err := scanKeys(ctx, servers, keys.PrefixQuota+Quota+":*", func(srv *redisserver.Server, key string) error {
		tag := keys.HashTag(key)
		c, ok := byTag[tag]
		if !ok {
			return nil
		}
		v, err := srv.Do(ctx, "GET", key)
		if err != nil {
			return err
		}
		if v.Null {
			return nil // expired since the scan
		}
		n, err := strconv.ParseInt(v.Str, 10, 64)
		if err != nil {
			return fmt.Errorf("chaos: quota counter %s holds %q", key, v.Str)
		}
		out[c] += n
		return nil
	})
	return out, err
}

// scanKeys calls fn for every key matching pattern on every server.
func scanKeys(ctx context.Context, servers []*redisserver.Server, pattern string, fn func(*redisserver.Server, string) error) error {
	for _, srv := range servers {
		cursor := "0"
		for {
			v, err := srv.Do(ctx, "SCAN", cursor, "MATCH", pattern, "COUNT", "1000")
			if err != nil {
				return fmt.Errorf("chaos: SCAN %s: %w", srv.Name(), err)
			}
			if len(v.Elems) != 2 {
				return fmt.Errorf("chaos: SCAN %s answered %v", srv.Name(), v)
			}
			for _, k := range v.Elems[1].Elems {
				if err := fn(srv, k.Str); err != nil {
					return err
				}
			}
			if cursor = v.Elems[0].Str; cursor == "0" {
				break
			}
		}
	}
	return nil
}

// CountKeys returns the number of keys matching pattern on the servers.
func CountKeys(ctx context.Context, servers []*redisserver.Server, pattern string) (int64, error) {
	var n int64
	err := scanKeys(ctx, servers, pattern, func(*redisserver.Server, string) error { n++; return nil })
	return n, err
}

// CommandStat is one command's INFO commandstats line.
type CommandStat struct {
	Calls int64
	Usec  int64
}

// CommandStats returns a server's INFO commandstats by command name, as
// "evalsha".
func CommandStats(ctx context.Context, s *redisserver.Server) (map[string]CommandStat, error) {
	info, err := s.Info(ctx, "commandstats")
	if err != nil {
		return nil, err
	}
	out := map[string]CommandStat{}
	for k, v := range info {
		name, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok {
			continue
		}
		var st CommandStat
		for kv := range strings.SplitSeq(v, ",") {
			field, val, _ := strings.Cut(kv, "=")
			switch field {
			case "calls":
				st.Calls, _ = strconv.ParseInt(val, 10, 64)
			case "usec":
				st.Usec, _ = strconv.ParseInt(val, 10, 64)
			}
		}
		out[name] = st
	}
	return out, nil
}

// MainThreadCPU returns a server's main-thread CPU time from INFO cpu:
// used_cpu_sys_main_thread plus used_cpu_user_main_thread, the shard
// script CPU of CE-12.
func MainThreadCPU(ctx context.Context, s *redisserver.Server) (time.Duration, error) {
	info, err := s.Info(ctx, "cpu")
	if err != nil {
		return 0, err
	}
	var total float64
	for _, k := range []string{"used_cpu_sys_main_thread", "used_cpu_user_main_thread"} {
		f, err := strconv.ParseFloat(info[k], 64)
		if err != nil {
			return 0, fmt.Errorf("chaos: INFO cpu %s = %q", k, info[k])
		}
		total += f
	}
	return time.Duration(total * float64(time.Second)), nil
}

// ReplicationOffsets returns a primary's master_repl_offset and its first
// replica's acknowledged offset from INFO replication.
func ReplicationOffsets(ctx context.Context, primary *redisserver.Server) (master, replica int64, err error) {
	info, err := primary.Info(ctx, "replication")
	if err != nil {
		return 0, 0, err
	}
	master, err = strconv.ParseInt(info["master_repl_offset"], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("chaos: INFO replication master_repl_offset = %q", info["master_repl_offset"])
	}
	for kv := range strings.SplitSeq(info["slave0"], ",") {
		if v, ok := strings.CutPrefix(kv, "offset="); ok {
			replica, err = strconv.ParseInt(v, 10, 64)
			return master, replica, err
		}
	}
	return master, 0, fmt.Errorf("chaos: %s lists no replica", primary.Name())
}
