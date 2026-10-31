// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: Redis names scripts by SHA-1 (EVALSHA); not a security use
	_ "embed"
	"encoding/hex"
	"log/slog"
	"strings"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// script is the ruralz_v1.lua library, State Store layout version 1.
//
//go:embed ruralz_v1.lua
var script string

// Operations of the library (ARGV[1]) and of its consumptive calls.
const (
	opConsume    = "c"
	opRefund     = "r"
	opGeneration = "n"
	opStore      = "s"
	opRefresh    = "f"
	opEndStale   = "x"
	opInvalidate = "i"
	kindGCRA     = "g"
	kindQuota    = "q"
)

// Modes of the generation step (opGeneration, ARGV[2]).
const (
	// genCreate creates the generation key holding the token unless a key
	// exists: a store whose lookup found none.
	genCreate = "c"
	// genHold checks that the key holds the token and renews its 26 h TTL:
	// a store, refresh or replacement under it.
	genHold = "h"
	// genCheck only checks that the key holds the token: an end-stale.
	genCheck = "v"
)

// skewPrefix starts the error reply of a Quota call whose window neither
// Node candidate matches, followed by the call's 1-based index.
const skewPrefix = "RZSKEW "

// scriptSHA returns the hex SHA-1 of the library, its EVALSHA name.
func scriptSHA() string {
	sum := sha1.Sum([]byte(script)) //nolint:gosec // G401: the EVALSHA name, not a security use
	return hex.EncodeToString(sum[:])
}

// scriptsLoaded records a successful SCRIPT LOAD on sh.
func (d *Driver) scriptsLoaded(ctx context.Context, sh *shard) {
	sh.pending.Store(false)
	if sh.noScripts.Swap(false) {
		d.log.InfoContext(ctx, "state store scripts loaded", slog.String(catalog.KeyShard, sh.id))
	}
}

// scriptsRefused records a SCRIPT LOAD answered as an unknown command or
// subcommand, or refused by an ACL: the shard lacks the script command
// set, its script calls fail with RZ-STS-005, and the maintenance
// goroutine retries the load at every memory reading.
func (d *Driver) scriptsRefused(ctx context.Context, sh *shard, msg string) {
	sh.pending.Store(true)
	if !sh.noScripts.Swap(true) {
		w, _, _ := strings.Cut(msg, " ")
		d.log.ErrorContext(ctx, "state store refuses scripts; script calls fail with RZ-STS-005",
			slog.String(catalog.KeyShard, sh.id), slog.String(catalog.KeyErrorType, w))
	}
}

// requestScripts schedules SCRIPT LOAD on sh, single-flight: a request
// while a load runs makes it run once more. NOSCRIPT replies and pending
// loads come here; new connections load in the dialer.
func (d *Driver) requestScripts(ctx context.Context, sh *shard) {
	sh.pending.Store(true)
	if !sh.loading.CompareAndSwap(false, true) {
		sh.again.Store(true)
		return
	}
	if !d.goStart() {
		sh.loading.Store(false)
		return
	}
	go d.loadScripts(context.WithoutCancel(ctx), sh)
}

// loadScripts runs SCRIPT LOAD on sh until no request is left.
func (d *Driver) loadScripts(ctx context.Context, sh *shard) {
	defer d.goEnd()
	for {
		sh.again.Store(false)
		d.loadScript(ctx, sh)
		if !sh.again.Load() || d.closed.Load() {
			break
		}
	}
	sh.loading.Store(false)
	if sh.again.Load() && !d.closed.Load() {
		d.requestScripts(ctx, sh)
	}
}

// loadScript sends SCRIPT LOAD to sh through the State client once it
// exists. Any failure other than a missing command set leaves the load
// pending for the next memory reading.
func (d *Driver) loadScript(ctx context.Context, sh *shard) {
	c := d.awaitClient()
	if c == nil {
		return
	}
	node := d.nodeClient(c, sh.addr)
	if node == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, d.opts.AdminTimeout)
	defer cancel()
	sha, err := node.Do(ctx, node.B().ScriptLoad().Script(script).Build()).ToString()
	re, isReply := rueidis.IsRedisErr(err)
	switch {
	case err == nil && sha == d.sha:
		d.scriptsLoaded(ctx, sh)
	case err == nil:
		if sh.scriptLog.allow(d.clk.Now()) {
			d.log.ErrorContext(ctx, "state store script load returned another SHA-1", slog.String(catalog.KeyShard, sh.id))
		}
	case isReply && lacksScriptsMsg(re.Error()):
		d.scriptsRefused(ctx, sh, re.Error())
	default:
		if sh.scriptLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, "state store script load failed", slog.String(catalog.KeyShard, sh.id), slog.String(catalog.KeyErrorType, errType(err)))
		}
	}
}

// lacksScriptsMsg reports a SCRIPT error reply meaning the deployment does
// not offer scripts to this client: an unknown command or subcommand, or
// an ACL refusal. rueidis drops a leading "ERR ".
func lacksScriptsMsg(msg string) bool {
	m := strings.ToLower(strings.TrimPrefix(msg, "ERR "))
	return strings.HasPrefix(m, "noperm") || strings.Contains(m, "unknown command") || strings.Contains(m, "unknown subcommand")
}

// isNoScript reports a NOSCRIPT error reply.
func isNoScript(err error) bool {
	re, ok := rueidis.IsRedisErr(err)
	return ok && re.IsNoScript()
}

// isReadOnly reports a READONLY error reply: the server is a replica,
// such as a primary a failover demoted.
func isReadOnly(err error) bool {
	re, ok := rueidis.IsRedisErr(err)
	return ok && strings.HasPrefix(re.Error(), "READONLY")
}
