-- Copyright 2026 Revington
-- SPDX-License-Identifier: Apache-2.0

-- ruralz_v1.lua is the script library of the redis State Store driver,
-- State Store layout version 1
-- (docs/architecture/11-scalability-and-distributed-state.md "State client
-- and RZ-STS error codes"). Nodes load it with SCRIPT LOAD at connect,
-- reconnect and failover, and run it only with EVALSHA. ARGV[1] names the
-- operation; every key it touches is in KEYS and shares one hash slot.
--
--   c  consumptive calls of one slot, in order, stopping after the first
--      deny: g runs GCRA over every limit of one ratelimit Policy with the
--      server's TIME, updating the TATs only if all allow; q reserves one
--      Quota unit in the window the server's TIME picks from the Node's
--      two candidates
--   r  returns one Quota unit, never below 0 and never creating a key
--   s  stores a Response Cache variant under the 1 s fill lease
--   f  refreshes a variant after a 304, under the revalidation lease
--   x  ends a variant's stale window, under the revalidation lease
--   i  bumps a URI's generation to the server time in microseconds
--
-- Numbers cross as text: TATs with %.17g, which reads back exactly,
-- integers with %.0f, so no server's number conversion rounds them.

local op = ARGV[1]
local clock = redis.call('TIME')
local sec = tonumber(clock[1])
local usec = tonumber(clock[2])
local now_ms = sec * 1000 + math.floor(usec / 1000)

-- A variant digest is 64 hex characters; a partition keeps 8 variants and
-- lives at most 25 h.
local DIGEST = 64
local MAX_ORDER = 8 * DIGEST
local MAX_PARTITION_TTL = 90000000

local function int(n)
  return string.format('%.0f', n)
end

local function bad(what)
  return redis.error_reply('ERR ruralz_v1 bad ' .. what)
end

-- drop returns the variant order without variant v.
local function drop(order, v)
  local i = 1
  while i <= string.len(order) do
    if string.sub(order, i, i + DIGEST - 1) == v then
      return string.sub(order, 1, i - 1) .. string.sub(order, i + DIGEST)
    end
    i = i + DIGEST
  end
  return order
end

-- extend returns a partition TTL in milliseconds: the larger of its
-- remaining TTL and ttl, at most 25 h.
local function extend(key, ttl)
  local left = redis.call('PTTL', key)
  if left > ttl then
    ttl = left
  end
  if ttl > MAX_PARTITION_TTL then
    ttl = MAX_PARTITION_TTL
  end
  return ttl
end

if op == 'c' then
  -- GCRA time: microseconds since 2026-01-01T00:00:00Z, exact as a double.
  local now = (sec - 1767225600) * 1000000 + usec
  local out = {clock[1], clock[2]}
  local calls = tonumber(ARGV[2])
  if not calls then
    return bad('calls')
  end
  local a = 3
  local k = 1
  for c = 1, calls do
    local kind = ARGV[a]
    if kind == 'g' then
      local n = tonumber(ARGV[a + 1])
      if not n or n < 0 then
        return bad('limits')
      end
      a = a + 2
      local tats = {}
      local ts = {}
      local taus = {}
      local allowed = true
      for i = 1, n do
        local t = tonumber(ARGV[a])
        local tau = tonumber(ARGV[a + 1])
        if not t or not tau or t <= 0 or tau < 0 then
          return bad('limit')
        end
        a = a + 2
        local tat = now
        local v = redis.call('GET', KEYS[k + i - 1])
        if v then
          local stored = tonumber(v)
          if not stored then
            return bad('tat')
          end
          if stored > now then
            tat = stored
          end
        end
        tats[i] = tat
        ts[i] = t
        taus[i] = tau
        if tat - now > tau then
          allowed = false
        end
      end
      local r = {0}
      if allowed then
        r[1] = 1
      end
      for i = 1, n do
        if allowed then
          local x = tats[i] + ts[i]
          local px = math.ceil((x - now + taus[i]) / 1000)
          if px < 1 then
            px = 1
          end
          redis.call('SET', KEYS[k + i - 1], string.format('%.17g', x), 'PX', int(px))
        end
        r[i + 1] = string.format('%.17g', tats[i])
      end
      k = k + n
      out[#out + 1] = r
      if not allowed then
        return out
      end
    elseif kind == 'q' then
      local limit = tonumber(ARGV[a + 1])
      local w = tonumber(ARGV[a + 2])
      local wa = tonumber(ARGV[a + 3])
      local wb = tonumber(ARGV[a + 4])
      if not limit or not w or not wa or not wb or w < 1 then
        return bad('quota')
      end
      a = a + 5
      local ws = now_ms - now_ms % w
      local key
      if ws == wa then
        key = KEYS[k]
      elseif ws == wb then
        key = KEYS[k + 1]
      else
        return redis.error_reply('RZSKEW ' .. c)
      end
      k = k + 2
      local used = 0
      local v = redis.call('GET', key)
      if v then
        used = tonumber(v)
        if not used then
          return bad('counter')
        end
      end
      if used >= limit then
        out[#out + 1] = {0, used, ws}
        return out
      end
      used = redis.call('INCR', key)
      if used == 1 then
        redis.call('PEXPIREAT', key, int(ws + 2 * w))
      end
      out[#out + 1] = {1, used, ws}
    else
      return bad('kind')
    end
  end
  return out
end

if op == 'r' then
  local v = redis.call('GET', KEYS[1])
  if not v then
    return 0
  end
  local n = tonumber(v)
  if not n then
    return bad('counter')
  end
  if n <= 0 then
    return 0
  end
  redis.call('DECR', KEYS[1])
  return 1
end

if op == 's' then
  local token = ARGV[2]
  local names = ARGV[3]
  local v = ARGV[4]
  local gen = ARGV[5]
  local ttl = tonumber(ARGV[6])
  local entry = ARGV[7]
  if not ttl or ttl < 1 or not entry or string.len(v) ~= DIGEST then
    return bad('store')
  end
  if redis.call('EXISTS', KEYS[2]) == 1 then
    return 0
  end
  redis.call('SET', KEYS[2], token, 'PX', '1000')
  local order = ''
  local held = redis.call('HGET', KEYS[1], 'n')
  if held == names then
    order = redis.call('HGET', KEYS[1], 'o') or ''
    ttl = extend(KEYS[1], ttl)
  elseif held then
    redis.call('DEL', KEYS[1])
  end
  order = drop(order, v) .. v
  while string.len(order) > MAX_ORDER do
    local old = string.sub(order, 1, DIGEST)
    order = string.sub(order, DIGEST + 1)
    redis.call('HDEL', KEYS[1], 'g:' .. old, 't:' .. old, 'e:' .. old)
  end
  redis.call('HSET', KEYS[1], 'n', names, 'o', order, 'g:' .. v, gen, 't:' .. v, int(now_ms), 'e:' .. v, entry)
  redis.call('PEXPIRE', KEYS[1], int(ttl))
  return 1
end

if op == 'f' then
  local v = ARGV[3]
  local entry = ARGV[4]
  local ttl = tonumber(ARGV[5])
  if not ttl or ttl < 0 or not entry then
    return bad('refresh')
  end
  if redis.call('GET', KEYS[2]) ~= ARGV[2] then
    return 0
  end
  if redis.call('HEXISTS', KEYS[1], 'e:' .. v) == 0 then
    return 0
  end
  redis.call('HSET', KEYS[1], 'e:' .. v, entry, 't:' .. v, int(now_ms))
  if ttl > 0 then
    redis.call('PEXPIRE', KEYS[1], int(extend(KEYS[1], ttl)))
  end
  return 1
end

if op == 'x' then
  local v = ARGV[3]
  if redis.call('GET', KEYS[2]) ~= ARGV[2] then
    return 0
  end
  if redis.call('HEXISTS', KEYS[1], 'e:' .. v) == 1 then
    redis.call('HDEL', KEYS[1], 'g:' .. v, 't:' .. v, 'e:' .. v)
    redis.call('HSET', KEYS[1], 'o', drop(redis.call('HGET', KEYS[1], 'o') or '', v))
  end
  return 1
end

if op == 'i' then
  local g = sec * 1000000 + usec
  local v = redis.call('GET', KEYS[1])
  if v then
    local cur = tonumber(v)
    if not cur then
      return bad('generation')
    end
    if cur + 1 > g then
      g = cur + 1
    end
  end
  redis.call('SET', KEYS[1], int(g), 'PX', '93600000')
  return 1
end

return bad('operation')
