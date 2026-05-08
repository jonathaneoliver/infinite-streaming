/**
 * v2-compat.js — fetch + EventSource shim that intercepts v1 endpoints
 * and routes them to v2.
 *
 * Designed so the existing v1 dashboard JS at content/dashboard/v2/*
 * keeps its mental model (fat session map, `set/fields/base_revision`
 * PATCH envelope, `/api/sessions/stream` SSE) but the actual on-the-
 * wire requests go to /api/v2/* with Merge Patch + If-Match.
 *
 * Load BEFORE any dashboard scripts:
 *   <script src="/shared/v2-compat.js"></script>
 *
 * What it intercepts:
 *   GET    /api/sessions                  → GET /api/v2/players?include=raw
 *   GET    /api/session/{id}              → GET /api/v2/players/{id}?include=raw
 *   GET    /api/session/{id}/network      → GET /api/v2/players/{id}/network
 *   GET    /api/sessions/stream           → /api/v2/events?include=raw fan-out
 *   PATCH  /api/session/{id}              → v2 Merge Patch + per-rule
 *   DELETE /api/session/{id}              → DELETE /api/v2/players/{id}
 *   DELETE /api/clear-sessions            → DELETE /api/v2/players
 *   POST   /api/session-group/link        → POST /api/v2/player-groups
 *   POST   /api/session-group/unlink      → DELETE /api/v2/player-groups/{id}
 *   GET    /api/session-group/{id}        → GET /api/v2/player-groups/{id}
 *
 * Out of scope (passes through to v1 unchanged):
 *   /api/session/{id}/metrics, /api/external-ips, /api/version,
 *   /api/nftables/*, /api/announce-now, /api/rendezvous, /go-live/*
 */
(function () {
  "use strict";

  const origFetch = window.fetch.bind(window);
  const OrigEventSource = window.EventSource;

  // ---- Path matchers ------------------------------------------------

  const re = {
    sessions: /^\/api\/sessions(?:\?(.*))?$/,
    sessionsStream: /^\/api\/sessions\/stream(?:\?(.*))?$/,
    sessionByID: /^\/api\/session\/([^\/?]+)(?:\?(.*))?$/,
    sessionByIDPatch: /^\/api\/session\/([^\/?]+)$/,
    sessionNetwork: /^\/api\/session\/([^\/?]+)\/network(?:\?(.*))?$/,
    sessionMetrics: /^\/api\/session\/([^\/?]+)\/metrics(?:\?(.*))?$/,
    clearSessions: /^\/api\/clear-sessions$/,
    groupLink: /^\/api\/session-group\/link$/,
    groupUnlink: /^\/api\/session-group\/unlink$/,
    groupGet: /^\/api\/session-group\/([^\/?]+)(?:\?(.*))?$/,
  };

  function pathOf(url) {
    if (typeof url !== "string") return "";
    if (url.startsWith("http")) {
      try { return new URL(url).pathname + (new URL(url).search || ""); }
      catch (_) { return url; }
    }
    return url;
  }

  // ---- v1 ↔ v2 SessionData translation ------------------------------

  // v2 list → v1 bare-array (the shape GET /api/sessions returns).
  // v1's SSE stream uses a different envelope (revision, dropped,
  // sessions:[]) which the EventSource shim handles separately.
  function v2ListToV1Array(v2Body) {
    const items = (v2Body && v2Body.items) || [];
    return items.map((p) => p.raw_session || {});
  }

  // SSE-stream envelope shape that session-live.js consumes:
  //   {revision, dropped, sessions: [SessionData]}
  function sessionsEnvelope(arr, revision) {
    return { revision: revision || Date.now(), dropped: 0, sessions: arr };
  }

  // v2 GET /players/{id} → v1 GET /api/session/{id} (returns SessionData directly)
  function v2RecordToV1Session(rec) {
    if (!rec) return null;
    return rec.raw_session || rec;
  }

  // ---- v1 PATCH envelope → v2 Merge Patch ---------------------------
  //
  // v1 envelope: { set: {<v1 key>: value, ...}, fields: [...], base_revision: "..." }
  // v2 PATCH    : Merge Patch body + If-Match header + per-rule sub-resource calls
  //
  // Per-key translation:
  //   nftables_bandwidth_mbps   → shape.rate_mbps
  //   nftables_delay_ms         → shape.delay_ms
  //   nftables_packet_loss      → shape.loss_pct
  //   nftables_pattern_enabled  → shape.pattern (null) | (built from steps)
  //   nftables_pattern_steps    → shape.pattern.steps
  //   transport_failure_type    → shape.transport_fault.type
  //   transport_failure_frequency → shape.transport_fault.frequency
  //   transport_consecutive_failures → shape.transport_fault.consecutive
  //   transport_failure_mode    → shape.transport_fault.mode
  //   group_id                  → group operation (skip in PATCH; handled separately)
  //   <surface>_failure_type/frequency/consecutive/mode → fault_rules[<surface>]
  //
  // Surfaces: segment, manifest, master_manifest, all
  // Per-surface rule id: "v1-<surface>"
  function buildV2PatchFromV1Set(setMap) {
    const patch = {};
    const setShape = (k, v) => { patch.shape = patch.shape || {}; patch.shape[k] = v; };
    const setTF = (k, v) => {
      patch.shape = patch.shape || {};
      patch.shape.transport_fault = patch.shape.transport_fault || {};
      patch.shape.transport_fault[k] = v;
    };

    if ("nftables_bandwidth_mbps" in setMap) setShape("rate_mbps", Number(setMap.nftables_bandwidth_mbps));
    if ("nftables_delay_ms" in setMap) setShape("delay_ms", Number(setMap.nftables_delay_ms));
    if ("nftables_packet_loss" in setMap) setShape("loss_pct", Number(setMap.nftables_packet_loss));

    if ("nftables_pattern_enabled" in setMap || "nftables_pattern_steps" in setMap) {
      const enabled = setMap.nftables_pattern_enabled !== false;
      const steps = setMap.nftables_pattern_steps;
      if (!enabled || (Array.isArray(steps) && steps.length === 0)) {
        setShape("pattern", null);
      } else if (Array.isArray(steps)) {
        setShape("pattern", { steps: steps.map((s) => ({
          duration_seconds: Number(s.duration_seconds),
          rate_mbps: Number(s.rate_mbps),
          enabled: s.enabled !== false,
        })) });
      }
    }

    if ("transport_failure_type" in setMap) setTF("type", String(setMap.transport_failure_type || "none"));
    if ("transport_failure_frequency" in setMap) setTF("frequency", Number(setMap.transport_failure_frequency));
    if ("transport_consecutive_failures" in setMap) setTF("consecutive", Number(setMap.transport_consecutive_failures));
    if ("transport_failure_mode" in setMap) setTF("mode", String(setMap.transport_failure_mode));

    return patch;
  }

  // perSurfaceRulePatches: returns a list of {ruleId, fields} describing
  // what per-rule fault sub-resource calls need to fire.
  function perSurfaceRulePatches(setMap) {
    const surfaces = ["segment", "manifest", "master_manifest", "all"];
    const out = [];
    for (const sfx of surfaces) {
      const typeKey = sfx + "_failure_type";
      const freqKey = sfx + "_failure_frequency";
      const consecKey = sfx + "_consecutive_failures";
      const modeKey = sfx + "_failure_mode";
      const touched = [typeKey, freqKey, consecKey, modeKey].some((k) => k in setMap);
      if (!touched) continue;
      const fields = {};
      if (typeKey in setMap) fields.type = String(setMap[typeKey]);
      if (freqKey in setMap) fields.frequency = Number(setMap[freqKey]);
      if (consecKey in setMap) fields.consecutive = Number(setMap[consecKey]);
      if (modeKey in setMap) fields.mode = String(setMap[modeKey]);
      if (sfx !== "all") fields.filter = { request_kind: [sfx] };
      out.push({ ruleId: "v1-" + sfx, fields });
    }
    return out;
  }

  // ---- Response synthesis ------------------------------------------

  function jsonResponse(status, body, etag) {
    const headers = { "Content-Type": "application/json" };
    if (etag) headers["ETag"] = etag;
    return new Response(JSON.stringify(body), { status, headers });
  }

  // Cache the most-recent ETag per player so PATCH calls can supply
  // If-Match without the dashboard knowing about RFC 7232.
  const etagCache = new Map(); // playerId → ETag string

  async function ensureETag(playerId) {
    if (etagCache.has(playerId)) return etagCache.get(playerId);
    const r = await origFetch("/api/v2/players/" + playerId);
    if (r.ok) {
      const e = r.headers.get("ETag");
      if (e) etagCache.set(playerId, e);
      return e;
    }
    return null;
  }

  // ---- fetch shim --------------------------------------------------

  window.fetch = async function (url, init) {
    init = init || {};
    const path = pathOf(url);
    const method = (init.method || "GET").toUpperCase();

    // GET /api/sessions — v1 returns the bare array (NOT the SSE envelope)
    if (method === "GET" && re.sessions.test(path)) {
      const v2 = await origFetch("/api/v2/players?include=raw");
      if (!v2.ok) return v2;
      const body = await v2.json();
      return jsonResponse(200, v2ListToV1Array(body));
    }

    // GET /api/session/{id}/network
    let m = re.sessionNetwork.exec(path);
    if (m && method === "GET") {
      const id = await playerIDForSessionLookup(m[1]);
      const v2 = await origFetch("/api/v2/players/" + id + "/network" + (m[2] ? "?" + m[2] : ""));
      if (!v2.ok) return v2;
      const body = await v2.json();
      // v1 dashboard reads the array directly. v2 returns {items: []}.
      return jsonResponse(200, body.items || []);
    }

    // GET / DELETE / PATCH /api/session/{id}
    m = re.sessionByID.exec(path);
    if (m) {
      const sessionID = m[1];
      const playerID = await playerIDForSessionLookup(sessionID);
      if (method === "GET") {
        const v2 = await origFetch("/api/v2/players/" + playerID + "?include=raw");
        if (!v2.ok) return v2;
        const body = await v2.json();
        const e = v2.headers.get("ETag");
        if (e) etagCache.set(playerID, e);
        return jsonResponse(200, v2RecordToV1Session(body), e);
      }
      if (method === "DELETE") {
        return await origFetch("/api/v2/players/" + playerID, { method: "DELETE" });
      }
      if (method === "PATCH") {
        return await translateAndApplyV1Patch(playerID, init);
      }
    }

    // GET /api/session/{id}/metrics — POSTs are PLAYER → server metrics,
    // out of v2 scope. Pass through to v1.
    if (re.sessionMetrics.test(path)) {
      return origFetch(url, init);
    }

    // POST /api/clear-sessions OR /api/clear-sessions DELETE
    if (re.clearSessions.test(path)) {
      return await origFetch("/api/v2/players", { method: "DELETE" });
    }

    // POST /api/session-group/link
    if (re.groupLink.test(path) && method === "POST") {
      let payload = {};
      try { payload = JSON.parse(init.body || "{}"); } catch (_) {}
      const memberIDs = await Promise.all(
        (payload.session_ids || []).map((sid) => playerIDForSessionLookup(sid))
      );
      const v2Body = {
        member_player_ids: memberIDs.filter(Boolean),
        label: payload.group_id || undefined,
      };
      const r = await origFetch("/api/v2/player-groups", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(v2Body),
      });
      if (!r.ok) return r;
      const created = await r.json();
      // v1 response shape: { message, group_id, linked_count }
      return jsonResponse(200, {
        message: "Sessions linked successfully",
        group_id: created.id,
        linked_count: (created.member_player_ids || []).length,
      });
    }

    // POST /api/session-group/unlink
    if (re.groupUnlink.test(path) && method === "POST") {
      let payload = {};
      try { payload = JSON.parse(init.body || "{}"); } catch (_) {}
      if (payload.unlink_group && payload.group_id) {
        const r = await origFetch("/api/v2/player-groups/" + payload.group_id, { method: "DELETE" });
        return jsonResponse(r.ok ? 200 : r.status, { unlinked: r.ok });
      }
      // Single-member unlink: re-PATCH the group's member_player_ids
      // minus this one. We don't have the current member list cheaply,
      // so we fall back to v1 path passthrough for this edge case.
      return origFetch(url, init);
    }

    // GET /api/session-group/{id}
    m = re.groupGet.exec(path);
    if (m && method === "GET") {
      const r = await origFetch("/api/v2/player-groups/" + m[1]);
      if (!r.ok) return r;
      const g = await r.json();
      // v1 shape: { group_id, members: [...], ... }
      return jsonResponse(200, {
        group_id: g.id,
        members: g.member_player_ids || [],
        label: g.label,
      });
    }

    // Default: pass through.
    return origFetch(url, init);
  };

  // ---- session_id → player_id resolution ----------------------------

  // The v1 dashboard refers to players by `session_id` (a small
  // integer 1..8) but v2 uses `player_id` (UUID). To translate, we
  // look up the active player list and match `session_id`. Cache the
  // result; refresh on miss.
  const sidCache = new Map(); // session_id → player_id

  async function playerIDForSessionLookup(sid) {
    if (!sid) return "";
    // If sid already looks like a UUID, treat as player_id.
    if (/^[0-9a-f]{8}-/i.test(sid)) return sid;
    if (sidCache.has(sid)) return sidCache.get(sid);
    await refreshSidCache();
    return sidCache.get(sid) || sid;
  }

  async function refreshSidCache() {
    try {
      const r = await origFetch("/api/v2/players?include=raw");
      if (!r.ok) return;
      const body = await r.json();
      sidCache.clear();
      for (const p of body.items || []) {
        const raw = p.raw_session || {};
        const sid = String(raw.session_id || raw.session_number || "");
        if (sid) sidCache.set(sid, p.id);
      }
    } catch (_) {}
  }

  // ---- PATCH translator --------------------------------------------

  async function translateAndApplyV1Patch(playerID, init) {
    let envelope = {};
    try { envelope = JSON.parse(init.body || "{}"); } catch (_) {}
    const setMap = envelope.set || {};

    // 1. Top-level Merge Patch (shape, transport_fault, etc.)
    const v2Patch = buildV2PatchFromV1Set(setMap);
    const ifMatch = await ensureETag(playerID);
    let lastResp = null;

    if (Object.keys(v2Patch).length > 0) {
      const r = await origFetch("/api/v2/players/" + playerID, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/merge-patch+json",
          ...(ifMatch ? { "If-Match": ifMatch } : {}),
        },
        body: JSON.stringify(v2Patch),
      });
      const newE = r.headers.get("ETag");
      if (newE) etagCache.set(playerID, newE);
      lastResp = r;
      if (!r.ok && r.status !== 412) return v1ShapedPatchResp(r, playerID);
    }

    // 2. Per-rule fault sub-resources (one PATCH per surface touched).
    const ruleOps = perSurfaceRulePatches(setMap);
    for (const op of ruleOps) {
      let cur = etagCache.get(playerID) || (await ensureETag(playerID));
      // Try PATCH first; on 404, POST a new one.
      let rr = await origFetch("/api/v2/players/" + playerID + "/fault_rules/" + op.ruleId, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/merge-patch+json",
          ...(cur ? { "If-Match": cur } : {}),
        },
        body: JSON.stringify(op.fields),
      });
      if (rr.status === 404) {
        const body = { id: op.ruleId, ...op.fields };
        rr = await origFetch("/api/v2/players/" + playerID + "/fault_rules", {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            ...(cur ? { "If-Match": cur } : {}),
          },
          body: JSON.stringify(body),
        });
      }
      const newE = rr.headers.get("ETag");
      if (newE) etagCache.set(playerID, newE);
      lastResp = rr;
    }

    // 3. URL-filter fields (segment_failure_urls, etc.) — v1's per-
    // surface URL substring filter has no clean v2 equivalent yet;
    // forward to v1 path so these still work during the migration.
    const urlKeys = Object.keys(setMap).filter((k) => k.endsWith("_failure_urls"));
    if (urlKeys.length > 0) {
      const subset = {};
      for (const k of urlKeys) subset[k] = setMap[k];
      const passThru = await origFetch("/api/session/" + playerID, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          set: subset,
          fields: urlKeys,
          base_revision: envelope.base_revision || "",
        }),
      });
      lastResp = passThru;
    }

    if (lastResp && !lastResp.ok && lastResp.status !== 412) {
      return v1ShapedPatchResp(lastResp, playerID);
    }

    // 4. Synthesize a v1 PATCH response: { session: <fresh SessionData> }
    const fresh = await origFetch("/api/v2/players/" + playerID + "?include=raw");
    if (!fresh.ok) return jsonResponse(500, { error: "translation failed" });
    const body = await fresh.json();
    return jsonResponse(200, { session: v2RecordToV1Session(body) });
  }

  function v1ShapedPatchResp(v2Resp, playerID) {
    // For 412 conflict, v1 expects { session: <current> }. Fetch fresh.
    if (v2Resp.status === 412) {
      return origFetch("/api/v2/players/" + playerID + "?include=raw")
        .then((r) => r.json())
        .then((body) => jsonResponse(409, { session: v2RecordToV1Session(body) }));
    }
    return v2Resp;
  }

  // ---- EventSource shim --------------------------------------------
  //
  // The v1 dashboard subscribes to /api/sessions/stream and expects
  // each `data` line to carry the FULL session list:
  //   {revision, dropped, sessions: [...]}
  //
  // The v2 stream emits typed deltas. We translate by:
  //   - Subscribing to /api/v2/events?include=raw
  //   - Maintaining a player_id → session map client-side
  //   - On any player.* event, dispatching a synthetic 'sessions' event
  //     with the full map values.

  function PatchedEventSource(url, opts) {
    const path = pathOf(url);
    const m = re.sessionsStream.exec(path);
    if (!m) {
      return new OrigEventSource(url, opts);
    }
    return new V1StreamFromV2(url, opts);
  }
  PatchedEventSource.CONNECTING = 0;
  PatchedEventSource.OPEN = 1;
  PatchedEventSource.CLOSED = 2;

  function V1StreamFromV2(url, opts) {
    // Parse the v1 player_id filter (if any) out of the URL.
    const params = new URLSearchParams((url.split("?")[1] || ""));
    const filterPlayerID = params.get("player_id") || "";

    // v2's events endpoint requires a strict UUID for the player_id
    // filter (oapigen rejects 8-char short hex with 400). v1 accepted
    // any string and filtered server-side. To stay compatible, only
    // pass the filter through when it parses as a full UUID; otherwise
    // we filter client-side below.
    const isUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(filterPlayerID);
    const filterClientSide = filterPlayerID && !isUUID;

    // Build the v2 URL: server-side filter only when we have a valid UUID.
    const v2URL = "/api/v2/events?include=raw" +
      (isUUID ? "&player_id=" + encodeURIComponent(filterPlayerID) : "");
    const sub = new OrigEventSource(v2URL, opts);

    const target = new EventTarget();
    const sessionsByID = new Map(); // player_id → SessionData
    let revision = 0;

    function emitSessions() {
      revision++;
      const ev = new MessageEvent("sessions", {
        data: JSON.stringify(sessionsEnvelope(Array.from(sessionsByID.values()), revision)),
      });
      // Fire on both the named-event and the default `message` channel.
      target.dispatchEvent(ev);
    }

    // v1 used substring matches over `player_id` (e.g. matches when
    // sess.player_id starts with the supplied prefix). Mirror that
    // when we have to filter client-side. UUID-shaped filters are
    // already handled server-side and arrive here pre-narrowed.
    function passesFilter(rawSess) {
      if (!filterClientSide) return true;
      const candidates = [
        rawSess.player_id,
        rawSess.session_id,
        rawSess.session_number,
        rawSess.headers_player_id,
        rawSess["headers_player-ID"],
        rawSess.x_playback_session_id,
      ];
      for (const c of candidates) {
        if (typeof c === "string" && c && c.indexOf(filterPlayerID) >= 0) {
          return true;
        }
      }
      return false;
    }

    function handleEnvelope(ev) {
      try {
        const body = JSON.parse(ev.data);
        const data = body.data || {};
        switch (body.type) {
          case "player.created":
          case "player.updated": {
            if (!data.id) break;
            const raw = data.raw_session || data;
            if (!passesFilter(raw)) break;
            sessionsByID.set(data.id, raw);
            emitSessions();
            break;
          }
          case "player.deleted":
            if (data.player_id && sessionsByID.has(data.player_id)) {
              sessionsByID.delete(data.player_id);
              emitSessions();
            }
            break;
        }
      } catch (_) {}
    }

    sub.addEventListener("player.created", handleEnvelope);
    sub.addEventListener("player.updated", handleEnvelope);
    sub.addEventListener("player.deleted", handleEnvelope);
    // Heartbeat → re-emit the current session list so session-live.js's
    // 12s silence watchdog doesn't force a reconnect every iteration.
    // Also dispatch a `heartbeat` event so consumers that listen on
    // that channel directly (session-live.js does) get the same
    // signal v1 used to send.
    sub.addEventListener("heartbeat", () => {
      target.dispatchEvent(new MessageEvent("heartbeat", { data: "{}" }));
      emitSessions();
    });
    sub.onopen = () => target.dispatchEvent(new Event("open"));
    sub.onerror = () => target.dispatchEvent(new Event("error"));

    // Bootstrap immediately — don't wait for sub.onopen. With an idle
    // proxy the v2 events stream's first server-emitted frame is a
    // 15s heartbeat, but the watchdog fires at 12s; doing the
    // initial /api/v2/players fetch + emit on construction means the
    // dashboard sees a sessions event within hundreds of ms regardless
    // of when the underlying SSE connection settles.
    origFetch("/api/v2/players?include=raw")
      .then((r) => r.json())
      .then((body) => {
        for (const p of body.items || []) {
          const raw = p.raw_session || {};
          if (passesFilter(raw)) sessionsByID.set(p.id, raw);
        }
        emitSessions();
      })
      .catch(() => emitSessions());

    // Public EventSource-shaped surface
    const proxy = {
      url: url,
      readyState: PatchedEventSource.CONNECTING,
      withCredentials: false,
      onmessage: null,
      onopen: null,
      onerror: null,
      addEventListener: (type, handler) => target.addEventListener(type, handler),
      removeEventListener: (type, handler) => target.removeEventListener(type, handler),
      close: () => { sub.close(); proxy.readyState = PatchedEventSource.CLOSED; },
    };
    target.addEventListener("open", () => {
      proxy.readyState = PatchedEventSource.OPEN;
      if (proxy.onopen) proxy.onopen({});
    });
    target.addEventListener("error", (e) => { if (proxy.onerror) proxy.onerror(e); });
    target.addEventListener("sessions", (e) => {
      if (proxy.onmessage) proxy.onmessage(e);
      // Also fire as `message` for default-event consumers.
      const msg = new MessageEvent("message", { data: e.data });
      target.dispatchEvent(msg);
    });
    return proxy;
  }

  window.EventSource = PatchedEventSource;
})();
