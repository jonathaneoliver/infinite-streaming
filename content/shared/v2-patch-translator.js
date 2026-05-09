/**
 * v2-patch-translator.js — v1 PATCH envelope → v2 Merge Patch + per-rule
 * fault sub-resource calls. The dashboard's v1 PATCH builders (in
 * session-shell.js) emit the legacy `{set:{}, fields:[], base_revision}`
 * shape; this module translates that to the v2 wire calls and returns a
 * Response-shaped value the v1 .then chain expects.
 *
 * Replaces the patch path in the deleted v2-compat.js. The fetch +
 * EventSource overrides v2-compat.js used to install are no longer
 * needed — every other call site in the dashboard now talks v2 directly.
 *
 * Public surface:
 *   window.V2Compat.translateV1Patch(playerID, init) → Promise<Response>
 *
 * Note: name kept as `V2Compat` so existing call sites in
 * session-shell.js (sendSessionPatch, sendInlinePatch) continue to work
 * verbatim.
 */
(function () {
  "use strict";

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

  // ---- v1 SessionData projection ------------------------------------

  function v2RecordToV1Session(rec) {
    if (!rec) return null;
    return rec.raw_session || rec;
  }

  // ---- Response synthesis -------------------------------------------

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
    const r = await fetch("/api/v2/players/" + playerId);
    if (r.ok) {
      const e = r.headers.get("ETag");
      if (e) etagCache.set(playerId, e);
      return e;
    }
    return null;
  }

  // ---- PATCH translator --------------------------------------------

  async function translateV1Patch(playerID, init) {
    let envelope = {};
    try { envelope = JSON.parse(init.body || "{}"); } catch (_) {}
    const setMap = envelope.set || {};

    // 1. Top-level Merge Patch (shape, transport_fault, etc.)
    const v2Patch = buildV2PatchFromV1Set(setMap);
    const ifMatch = await ensureETag(playerID);
    let lastResp = null;

    if (Object.keys(v2Patch).length > 0) {
      const r = await fetch("/api/v2/players/" + playerID, {
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
      let rr = await fetch("/api/v2/players/" + playerID + "/fault_rules/" + op.ruleId, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/merge-patch+json",
          ...(cur ? { "If-Match": cur } : {}),
        },
        body: JSON.stringify(op.fields),
      });
      if (rr.status === 404) {
        const body = { id: op.ruleId, ...op.fields };
        rr = await fetch("/api/v2/players/" + playerID + "/fault_rules", {
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
    // forward to the v1 path so these still work during the migration.
    // No other v1 endpoint is called from the dashboard; this remains
    // the single legacy-path holdout.
    const urlKeys = Object.keys(setMap).filter((k) => k.endsWith("_failure_urls"));
    if (urlKeys.length > 0) {
      const subset = {};
      for (const k of urlKeys) subset[k] = setMap[k];
      const passThru = await fetch("/api/session/" + playerID, {
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
    const fresh = await fetch("/api/v2/players/" + playerID + "?include=raw");
    if (!fresh.ok) return jsonResponse(500, { error: "translation failed" });
    const body = await fresh.json();
    return jsonResponse(200, { session: v2RecordToV1Session(body) });
  }

  function v1ShapedPatchResp(v2Resp, playerID) {
    // For 412 conflict, v1 expects { session: <current> }. Fetch fresh.
    if (v2Resp.status === 412) {
      return fetch("/api/v2/players/" + playerID + "?include=raw")
        .then((r) => r.json())
        .then((body) => jsonResponse(409, { session: v2RecordToV1Session(body) }));
    }
    return v2Resp;
  }

  window.V2Compat = window.V2Compat || {};
  window.V2Compat.translateV1Patch = translateV1Patch;
})();
