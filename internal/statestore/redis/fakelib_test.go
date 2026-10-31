// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"math"
	"strconv"
	"strings"
)

// fakeLibrary mirrors ruralz_v1.lua line by line on the fake server's
// keyspace, so the conformance suite run against the fake server checks
// the driver's encoding, decoding and arithmetic against the library's
// semantics. Lua's 1-based ARGV[i] and KEYS[i] are luaArg(argv, i) and
// luaKey(keys, i); Lua numbers are float64.

// luaArg returns Lua's ARGV[i]: ok is false where Lua reads nil.
func luaArg(argv []string, i int) (string, bool) {
	if i < 1 || i > len(argv) {
		return "", false
	}
	return argv[i-1], true
}

// luaKey returns Lua's KEYS[i].
func luaKey(keys []string, i int) (string, bool) { return luaArg(keys, i) }

// tonumber is Lua's tonumber on a string: nil (ok false) when it does not
// read as a number.
func tonumber(s string, present bool) (float64, bool) {
	if !present {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// luaInt is string.format('%.0f', n).
func luaInt(n float64) string { return strconv.FormatFloat(n, 'f', 0, 64) }

// lua17g is string.format('%.17g', x).
func lua17g(x float64) string { return strconv.FormatFloat(x, 'g', 17, 64) }

// badArgs is the error reply of redis.call with a nil argument.
const badArgs = "ERR Lua redis lib command arguments must be strings or integers"

// bad is the library's bad(what).
func bad(what string) string { return errorReply("ERR ruralz_v1 bad " + what) }

// library runs one EVALSHA of the library; the caller holds s.mu.
func (s *fakeServer) library(keys, argv []string) string {
	op, _ := luaArg(argv, 1)
	sec, usec := s.timeNow()
	nowMs := sec*1000 + usec/1000
	switch op {
	case opConsume:
		return s.libConsume(keys, argv, sec, usec, nowMs)
	case opRefund:
		return s.libRefund(keys, argv)
	case opGeneration:
		return s.libGeneration(keys, argv)
	case opStore:
		return s.libStore(keys, argv, nowMs)
	case opRefresh:
		return s.libRefresh(keys, argv, nowMs)
	case opEndStale:
		return s.libEndStale(keys, argv)
	case opInvalidate:
		return s.libInvalidate(keys, argv)
	default:
		return bad("operation")
	}
}

// Library constants.
const (
	libToken         = 32
	libGenerationTTL = 93_600_000
)

// tokenOK is the library's tokenok: 32 lowercase hex characters.
func tokenOK(t string, present bool) bool {
	if !present || len(t) != libToken {
		return false
	}
	for i := range len(t) {
		if c := t[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *fakeServer) libGeneration(keys, argv []string) string {
	key, okKey := luaKey(keys, 1)
	mode, _ := luaArg(argv, 2)
	token, present := luaArg(argv, 3)
	if !tokenOK(token, present) {
		return bad("token")
	}
	if !okKey {
		return errorReply(badArgs)
	}
	if mode == genCreate {
		if _, held := s.get(key); held {
			return integer(0)
		}
		s.set(key, token, libGenerationTTL)
		return integer(1)
	}
	if mode != genHold && mode != genCheck {
		return bad("mode")
	}
	if v, held := s.get(key); !held || v != token {
		return integer(0)
	}
	if mode == genHold {
		s.pexpire(key, libGenerationTTL)
	}
	return integer(1)
}

func (s *fakeServer) libConsume(keys, argv []string, sec, usec, nowMs int64) string {
	now := float64((sec-epoch2026)*1_000_000 + usec)
	out := []string{bulk(strconv.FormatInt(sec, 10)), bulk(strconv.FormatInt(usec, 10))}
	calls, ok := tonumber(luaArg(argv, 2))
	if !ok {
		return bad("calls")
	}
	a, k := 3, 1
	for c := 1; float64(c) <= calls; c++ {
		kind, _ := luaArg(argv, a)
		switch kind {
		case kindGCRA:
			cost, okCost := tonumber(luaArg(argv, a+1))
			if !okCost || cost < 1 || cost != math.Floor(cost) {
				return bad("cost")
			}
			n, ok := tonumber(luaArg(argv, a+2))
			if !ok || n < 0 {
				return bad("limits")
			}
			a += 3
			var tats, ts, taus []float64
			allowed := true
			for i := 1; float64(i) <= n; i++ {
				t, okT := tonumber(luaArg(argv, a))
				tau, okTau := tonumber(luaArg(argv, a+1))
				if !okT || !okTau || t <= 0 || tau < 0 {
					return bad("limit")
				}
				a += 2
				tat := now
				key, ok := luaKey(keys, k+i-1)
				if !ok {
					return errorReply(badArgs)
				}
				if v, held := s.get(key); held {
					stored, ok := tonumber(v, true)
					if !ok {
						return bad("tat")
					}
					if stored > now {
						tat = stored
					}
				}
				tats, ts, taus = append(tats, tat), append(ts, t), append(taus, tau)
				// Lua rounds the product before the subtraction.
				if slack := tau - float64((cost-1)*t); tat-now > slack {
					allowed = false
				}
			}
			flag := int64(0)
			if allowed {
				flag = 1
			}
			r := []string{integer(flag)}
			for i := range tats {
				if allowed {
					x := tats[i] + float64(cost*ts[i])
					px := math.Ceil((x - now + taus[i]) / 1000)
					if px < 1 {
						px = 1
					}
					key, _ := luaKey(keys, k+i)
					p, _ := strconv.ParseInt(luaInt(px), 10, 64)
					s.set(key, lua17g(x), p)
				}
				r = append(r, bulk(lua17g(tats[i])))
			}
			k += len(tats)
			out = append(out, array(r...))
			if !allowed {
				return array(out...)
			}
		case kindQuota:
			limit, ok1 := tonumber(luaArg(argv, a+1))
			cost, ok2 := tonumber(luaArg(argv, a+2))
			sa, ok3 := tonumber(luaArg(argv, a+3))
			ea, ok4 := tonumber(luaArg(argv, a+4))
			sb, ok5 := tonumber(luaArg(argv, a+5))
			eb, ok6 := tonumber(luaArg(argv, a+6))
			if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || limit < 0 ||
				cost < 1 || cost != math.Floor(cost) || ea <= sa || eb <= sb {
				return bad("quota")
			}
			a += 7
			ms := float64(nowMs)
			var key string
			var ws, we float64
			switch {
			case sa <= ms && ms < ea:
				key, ok = luaKey(keys, k)
				ws, we = sa, ea
			case sb <= ms && ms < eb:
				key, ok = luaKey(keys, k+1)
				ws, we = sb, eb
			default:
				return errorReply(skewPrefix + strconv.Itoa(c))
			}
			if !ok {
				return errorReply(badArgs)
			}
			k += 2
			used := 0.0
			v, held := s.get(key)
			if held {
				if used, ok = tonumber(v, true); !ok {
					return bad("counter")
				}
			}
			if used+cost > limit {
				out = append(out, array(integer(0), integer(int64(used)), integer(int64(ws))))
				return array(out...)
			}
			by, _ := strconv.ParseInt(luaInt(cost), 10, 64)
			n, err := s.incrBy(key, by)
			if err != "" {
				return err
			}
			if !held {
				at, _ := strconv.ParseInt(luaInt(we+(we-ws)), 10, 64)
				s.pexpireAt(key, at)
			}
			out = append(out, array(integer(1), integer(n), integer(int64(ws))))
		default:
			return bad("kind")
		}
	}
	return array(out...)
}

// incrBy is INCRBY: an integer value or none, keeping the TTL.
func (s *fakeServer) incrBy(k string, by int64) (int64, string) {
	var n int64
	exp := int64(0)
	if v, ok := s.get(k); ok {
		var err error
		if n, err = strconv.ParseInt(v, 10, 64); err != nil {
			return 0, errorReply("ERR value is not an integer or out of range")
		}
		exp = s.strs[k].exp
	}
	n += by
	s.strs[k] = fstr{v: strconv.FormatInt(n, 10), exp: exp}
	return n, ""
}

func (s *fakeServer) libRefund(keys, argv []string) string {
	units, ok := tonumber(luaArg(argv, 2))
	if !ok || units < 1 || units != math.Floor(units) {
		return bad("units")
	}
	key, ok := luaKey(keys, 1)
	if !ok {
		return errorReply(badArgs)
	}
	v, held := s.get(key)
	if !held {
		return integer(0)
	}
	n, ok := tonumber(v, true)
	if !ok {
		return bad("counter")
	}
	if n <= 0 {
		return integer(0)
	}
	units = min(units, n)
	by, _ := strconv.ParseInt(luaInt(units), 10, 64)
	if _, err := s.incrBy(key, -by); err != "" {
		return err
	}
	return integer(1)
}

// drop is the library's drop: the variant order without v.
func drop(order, v string) string {
	for i := 0; i < len(order); i += 64 {
		if order[i:min(i+64, len(order))] == v {
			return order[:i] + order[min(i+64, len(order)):]
		}
	}
	return order
}

// extend is the library's extend.
func (s *fakeServer) extend(key string, ttl float64) float64 {
	if left := float64(s.pttl(key)); left > ttl {
		ttl = left
	}
	return min(ttl, maxPartitionTTLMs)
}

// del deletes k.
func (s *fakeServer) del(k string) {
	delete(s.strs, k)
	delete(s.hashes, k)
}

// pexpire sets k's TTL to ms milliseconds.
func (s *fakeServer) pexpire(k string, ms float64) {
	n, _ := strconv.ParseInt(luaInt(ms), 10, 64)
	s.pexpireAt(k, s.nowMs()+n)
}

func (s *fakeServer) libStore(keys, argv []string, nowMs int64) string {
	part, ok1 := luaKey(keys, 1)
	lease, ok2 := luaKey(keys, 2)
	token, _ := luaArg(argv, 2)
	names, _ := luaArg(argv, 3)
	v, _ := luaArg(argv, 4)
	ttl, okTTL := tonumber(luaArg(argv, 5))
	delay, okDelay := tonumber(luaArg(argv, 6))
	entry, okEntry := luaArg(argv, 7)
	if !okTTL || ttl < 1 || !okDelay || delay < 0 || !okEntry || len(v) != 64 {
		return bad("store")
	}
	if !ok1 || !ok2 {
		return errorReply(badArgs)
	}
	if s.exists(lease) {
		return integer(0)
	}
	s.set(lease, token, 1000)
	order := ""
	held, ok := s.hash(part).get(keysFieldNames)
	if ok && held == names {
		order, _ = s.hash(part).get(keysFieldOrder)
		ttl = s.extend(part, ttl)
	} else if ok {
		s.del(part)
	}
	order = drop(order, v) + v
	for len(order) > 8*64 {
		old := order[:64]
		order = order[64:]
		s.hdel(part, "t:"+old, "e:"+old)
	}
	s.hset(part, keysFieldNames, names, keysFieldOrder, order, "t:"+v, luaInt(float64(nowMs)-delay), "e:"+v, entry)
	s.pexpire(part, ttl)
	return integer(1)
}

func (s *fakeServer) libRefresh(keys, argv []string, nowMs int64) string {
	part, ok1 := luaKey(keys, 1)
	lease, ok2 := luaKey(keys, 2)
	v, _ := luaArg(argv, 3)
	entry, okEntry := luaArg(argv, 4)
	ttl, okTTL := tonumber(luaArg(argv, 5))
	delay, okDelay := tonumber(luaArg(argv, 6))
	if !okTTL || ttl < 0 || !okDelay || delay < 0 || !okEntry {
		return bad("refresh")
	}
	if !ok1 || !ok2 {
		return errorReply(badArgs)
	}
	token, _ := luaArg(argv, 2)
	if held, ok := s.get(lease); !ok || held != token {
		return integer(0)
	}
	if _, ok := s.hash(part).get("e:" + v); !ok {
		return integer(0)
	}
	s.hset(part, "e:"+v, entry, "t:"+v, luaInt(float64(nowMs)-delay))
	if ttl > 0 {
		s.pexpire(part, s.extend(part, ttl))
	}
	return integer(1)
}

func (s *fakeServer) libEndStale(keys, argv []string) string {
	part, ok1 := luaKey(keys, 1)
	lease, ok2 := luaKey(keys, 2)
	if !ok1 || !ok2 {
		return errorReply(badArgs)
	}
	v, _ := luaArg(argv, 3)
	token, _ := luaArg(argv, 2)
	if held, ok := s.get(lease); !ok || held != token {
		return integer(0)
	}
	if _, ok := s.hash(part).get("e:" + v); ok {
		s.hdel(part, "t:"+v, "e:"+v)
		order, _ := s.hash(part).get(keysFieldOrder)
		s.hset(part, keysFieldOrder, drop(order, v))
	}
	return integer(1)
}

func (s *fakeServer) libInvalidate(keys, argv []string) string {
	token, present := luaArg(argv, 2)
	if !tokenOK(token, present) {
		return bad("token")
	}
	key, ok := luaKey(keys, 1)
	if !ok {
		return errorReply(badArgs)
	}
	s.set(key, token, libGenerationTTL)
	return integer(1)
}

// Partition fields the library writes.
const (
	keysFieldNames = "n"
	keysFieldOrder = "o"
)
