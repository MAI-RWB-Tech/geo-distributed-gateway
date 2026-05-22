-- score.lua: per-worker zone pinning based on Control Plane recommendations.
-- Filter order in global-envoy.yaml: lua → router. The x-geo header set
-- on request side IS visible to the existing x-geo route-matches (lines 21-35).
--
-- Sentinel x-route-fallback header is a RESPONSE header, set in envoy_on_response
-- by reading dynamicMetadata flag stamped in envoy_on_request. Request-side
-- headers:add() does NOT round-trip back to the client.

local M = {}

-- Per-worker cache: M.cache[service] = { weights = {zone1=N, zone2=N}, exp = epoch_s }
M.cache = {}

-- Seed RNG once per worker on script load.
-- Without this math.random() returns a deterministic sequence per Lua state,
-- causing all workers to make identical zone choices for identical inputs.
math.randomseed(os.time() + (tonumber(tostring({}):sub(8)) or 0))

local TTL_SECONDS = 30
local CALL_TIMEOUT_MS = 1000
local FALLBACK_WEIGHTS = { zone1 = 0.5, zone2 = 0.5 }
local DEFAULT_SERVICE = "service-a"
local METADATA_NAMESPACE = "score.lua"
local METADATA_FALLBACK_KEY = "route_fallback"

local function service_from_path(path)
  local svc = path:match("^/(service%-[a-e])/")
  return svc or DEFAULT_SERVICE
end

-- Synchronous httpCall to Control Plane. Returns (weights, was_fallback).
local function fetch_weights(request_handle, service)
  local headers, body = request_handle:httpCall(
    "control_plane_cluster",
    {
      [":method"] = "GET",
      [":path"] = "/config/weights/" .. service,
      [":authority"] = "control-plane",
    },
    "",
    CALL_TIMEOUT_MS
  )
  if headers == nil or headers[":status"] ~= "200" then
    request_handle:logWarn("score: control-plane unavailable for " .. service ..
      " (status=" .. tostring(headers and headers[":status"] or "nil") .. ") → fallback")
    return FALLBACK_WEIGHTS, true
  end
  local cjson = require("cjson")
  local ok, parsed = pcall(cjson.decode, body)
  if not ok or type(parsed) ~= "table" or not parsed.zone1 or not parsed.zone2 then
    request_handle:logWarn("score: invalid JSON from control-plane for " .. service .. " → fallback")
    return FALLBACK_WEIGHTS, true
  end
  return { zone1 = parsed.zone1, zone2 = parsed.zone2 }, false
end

local function get_weights(request_handle, service)
  local now = os.time()
  local entry = M.cache[service]
  if entry and entry.exp > now then
    return entry.weights, false
  end
  local weights, was_fallback = fetch_weights(request_handle, service)
  -- Cache ONLY successful responses, never fallbacks — otherwise a brief CP
  -- outage pins us to 50/50 for the full TTL even after CP recovers.
  if not was_fallback then
    M.cache[service] = { weights = weights, exp = now + TTL_SECONDS }
  end
  return weights, was_fallback
end

local function pick_zone(weights)
  return (math.random() < weights.zone1) and "zone1" or "zone2"
end

function envoy_on_request(request_handle)
  local headers = request_handle:headers()
  -- Respect explicit client-side zone pinning (used by failure-runner tests).
  local existing = headers:get("x-geo")
  if existing == "zone1" or existing == "zone2" then
    return
  end
  local path = headers:get(":path") or "/"
  local service = service_from_path(path)
  local weights, was_fallback = get_weights(request_handle, service)
  local zone = pick_zone(weights)
  headers:add("x-geo", zone)
end

function envoy_on_response(response_handle)
end
