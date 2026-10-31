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
--      server's TIME, charging config.cost emission intervals and
--      updating the TATs only if all allow; q reserves config.cost
--      Quota units, all or none, in the window the server's TIME picks
--      from the Node's two candidates, each an epoch-aligned window or a
--      UTC calendar month given as its start and end
--   r  returns a reservation's units, never below 0 and never creating a
--      key
--   n  the generation step of a Response Cache store, refresh or
--      end-stale, on the URI's generation key: c creates it holding the
--      Node's token unless a key exists (SET NX), h checks that it holds
--      the token and renews its 26 h TTL, v only checks; a store or lease
--      write runs its partition step only after this step applied, since
--      the two keys hash to different cluster slots
--   s  stores a Response Cache variant under the 1 s fill lease, its
--      store time the server time less the write's receipt delay
--   f  rewrites a variant with an entry a 304 freshened or a full
--      response replaced, under the revalidation lease, its store time
--      the server time less the write's receipt delay
--   x  ends a variant's stale window, under the revalidation lease
--   i  bumps a URI's generation key to the Node's token
--
-- Numbers cross as text: TATs with %.17g, which reads back exactly,
-- integers with %.0f, so no server's number conversion rounds them.
-- Generation tokens are 32 lowercase hex characters.

local op = ARGV[1]
local clock = redis.call('TIME')
local sec = tonumber(clock[1])
local usec = tonumber(clock[2])
local now_ms = sec * 1000 + math.floor(usec / 1000)

-- A variant digest is 64 hex characters; a partition keeps 8 variants and
-- lives at most 25 h; a generation key holds a 32-character token for
-- 26 h after its creation, a bump or a store under it.
local DIGEST = 64
local MAX_ORDER = 8 * DIGEST
local MAX_PARTITION_TTL = 90000000
local TOKEN = 32
local GENERATION_TTL = '93600000'

local function int(n)
  return string.format('%.0f', n)
end

local function bad(what)
  return redis.error_reply('ERR ruralz_v1 bad ' .. what)
end

-- tokenok reports a generation token: 32 lowercase hex characters.
local function tokenok(t)
  return t ~= nil and string.len(t) == TOKEN and not string.find(t, '[^0-9a-f]')
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
      -- cost units conform when TAT - now is at most tau - (cost - 1) * T;
      -- an allow advances each TAT by cost * T.
      local cost = tonumber(ARGV[a + 1])
      if not cost or cost < 1 or cost ~= math.floor(cost) then
        return bad('cost')
      end
      local n = tonumber(ARGV[a + 2])
      if not n or n < 0 then
        return bad('limits')
      end
      a = a + 3
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
        local slack = tau - (cost - 1) * t
        if tat - now > slack then
          allowed = false
        end
      end
      local r = {0}
      if allowed then
        r[1] = 1
      end
      for i = 1, n do
        if allowed then
          local x = tats[i] + cost * ts[i]
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
      -- The candidate [start, end) holding the server's now is charged:
      -- cost units if the count plus cost stays within the limit, else
      -- nothing. A new counter expires a window after its window closes.
      local limit = tonumber(ARGV[a + 1])
      local cost = tonumber(ARGV[a + 2])
      local sa = tonumber(ARGV[a + 3])
      local ea = tonumber(ARGV[a + 4])
      local sb = tonumber(ARGV[a + 5])
      local eb = tonumber(ARGV[a + 6])
      if not limit or not cost or not sa or not ea or not sb or not eb or limit < 0 or
          cost < 1 or cost ~= math.floor(cost) or ea <= sa or eb <= sb then
        return bad('quota')
      end
      a = a + 7
      local key, ws, we
      if sa <= now_ms and now_ms < ea then
        key, ws, we = KEYS[k], sa, ea
      elseif sb <= now_ms and now_ms < eb then
        key, ws, we = KEYS[k + 1], sb, eb
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
      if used + cost > limit then
        out[#out + 1] = {0, used, ws}
        return out
      end
      used = redis.call('INCRBY', key, int(cost))
      if not v then
        redis.call('PEXPIREAT', key, int(we + (we - ws)))
      end
      out[#out + 1] = {1, used, ws}
    else
      return bad('kind')
    end
  end
  return out
end

if op == 'r' then
  local units = tonumber(ARGV[2])
  if not units or units < 1 or units ~= math.floor(units) then
    return bad('units')
  end
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
  if units > n then
    units = n
  end
  redis.call('DECRBY', KEYS[1], int(units))
  return 1
end

if op == 'n' then
  local mode = ARGV[2]
  local token = ARGV[3]
  if not tokenok(token) then
    return bad('token')
  end
  if mode == 'c' then
    if redis.call('SET', KEYS[1], token, 'NX', 'PX', GENERATION_TTL) then
      return 1
    end
    return 0
  end
  if mode ~= 'h' and mode ~= 'v' then
    return bad('mode')
  end
  if redis.call('GET', KEYS[1]) ~= token then
    return 0
  end
  if mode == 'h' then
    redis.call('PEXPIRE', KEYS[1], GENERATION_TTL)
  end
  return 1
end

if op == 's' then
  local token = ARGV[2]
  local names = ARGV[3]
  local v = ARGV[4]
  local ttl = tonumber(ARGV[5])
  local delay = tonumber(ARGV[6])
  local entry = ARGV[7]
  if not ttl or ttl < 1 or not delay or delay < 0 or not entry or string.len(v) ~= DIGEST then
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
    redis.call('HDEL', KEYS[1], 't:' .. old, 'e:' .. old)
  end
  redis.call('HSET', KEYS[1], 'n', names, 'o', order, 't:' .. v, int(now_ms - delay), 'e:' .. v, entry)
  redis.call('PEXPIRE', KEYS[1], int(ttl))
  return 1
end

if op == 'f' then
  local v = ARGV[3]
  local entry = ARGV[4]
  local ttl = tonumber(ARGV[5])
  local delay = tonumber(ARGV[6])
  if not ttl or ttl < 0 or not delay or delay < 0 or not entry then
    return bad('refresh')
  end
  if redis.call('GET', KEYS[2]) ~= ARGV[2] then
    return 0
  end
  if redis.call('HEXISTS', KEYS[1], 'e:' .. v) == 0 then
    return 0
  end
  redis.call('HSET', KEYS[1], 'e:' .. v, entry, 't:' .. v, int(now_ms - delay))
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
    redis.call('HDEL', KEYS[1], 't:' .. v, 'e:' .. v)
    redis.call('HSET', KEYS[1], 'o', drop(redis.call('HGET', KEYS[1], 'o') or '', v))
  end
  return 1
end

if op == 'i' then
  local token = ARGV[2]
  if not tokenok(token) then
    return bad('token')
  end
  redis.call('SET', KEYS[1], token, 'PX', GENERATION_TTL)
  return 1
end

return bad('operation')
