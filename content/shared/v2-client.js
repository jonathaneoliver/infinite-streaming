/**
 * v2-client.js — single-page consumer for the v2 harness API.
 *
 * Lives at /shared/v2-client.js so any dashboard page can `<script
 * src="/shared/v2-client.js"></script>` and grab `window.HarnessV2`.
 *
 * Responsibilities:
 *   - opens & maintains an SSE subscription on /api/v2/events with
 *     automatic reconnect + Last-Event-ID resume + replay.gap signal
 *   - wraps every v2 endpoint as a small async method that returns the
 *     parsed JSON (or RFC 7807 problem on 4xx/5xx)
 *   - tracks the most-recent ETag per player so callers can omit
 *     `If-Match` and the client supplies it automatically
 *
 * No external deps; ES2017+; runs in every modern browser.
 */
(function (global) {
  "use strict";

  const DEFAULT_BASE = "";

  /**
   * subscribeEvents — open an SSE stream and dispatch typed callbacks.
   *
   * Returns a cancel() handle. The stream auto-reconnects with the
   * most-recent `id:` in `Last-Event-ID` so server replay-ring kicks in.
   *
   * Callbacks (all optional):
   *   onPlayerCreated(record)
   *   onPlayerUpdated(record)
   *   onPlayerDeleted({player_id, reason})
   *   onPlayStarted({player_id, play_id, started_at})
   *   onPlayEnded({player_id, play_id, ended_at, reason})
   *   onPlayNetworkEntry({player_id, play_id, entry})
   *   onHeartbeat({ts})
   *   onReplayGap({missed_from, missed_to})
   *   onError(err)
   *   onConnect() / onDisconnect()
   */
  function subscribeEvents(opts) {
    opts = opts || {};
    const base = opts.base || DEFAULT_BASE;
    const cbs = opts.callbacks || {};
    let lastEventID = "";
    let es = null;
    let cancelled = false;

    const dispatch = {
      "player.created": cbs.onPlayerCreated,
      "player.updated": cbs.onPlayerUpdated,
      "player.deleted": cbs.onPlayerDeleted,
      "play.started": cbs.onPlayStarted,
      "play.ended": cbs.onPlayEnded,
      "play.network.entry": cbs.onPlayNetworkEntry,
      "heartbeat": cbs.onHeartbeat,
      "replay.gap": cbs.onReplayGap,
    };

    function open() {
      if (cancelled) return;
      // Browsers send Last-Event-ID automatically based on `id:` lines
      // in the stream. Manual EventSource doesn't expose a header
      // override, so we encode it as a query param the server doesn't
      // currently honour — the browser-native flow handles the common
      // case (transient disconnect mid-stream).
      const url = `${base}/api/v2/events`;
      es = new EventSource(url, { withCredentials: false });
      es.onopen = () => cbs.onConnect && cbs.onConnect();
      es.onerror = (ev) => {
        cbs.onDisconnect && cbs.onDisconnect(ev);
        // Browser auto-reconnect is good enough; nothing to do.
      };
      // The server emits each frame with a typed `event:` line.
      // EventSource fires the matching listener (or the default
      // `message` handler). Register one listener per known type
      // so the dispatch table covers everything.
      const types = Object.keys(dispatch);
      types.forEach((t) => {
        es.addEventListener(t, (ev) => {
          if (ev.lastEventId) lastEventID = ev.lastEventId;
          let data = null;
          try { data = JSON.parse(ev.data || "null"); } catch (e) {
            cbs.onError && cbs.onError(e);
            return;
          }
          const cb = dispatch[t];
          if (cb) cb(data && data.data, data);
        });
      });
    }

    open();

    return {
      cancel() {
        cancelled = true;
        if (es) { es.close(); es = null; }
      },
      lastEventID() { return lastEventID; },
    };
  }

  // ---- HTTP helpers --------------------------------------------------

  async function _fetchJSON(method, url, opts) {
    opts = opts || {};
    const headers = Object.assign({}, opts.headers || {});
    if (opts.body !== undefined && opts.body !== null && !headers["Content-Type"]) {
      headers["Content-Type"] = method === "PATCH"
        ? "application/merge-patch+json"
        : "application/json";
    }
    const init = { method, headers };
    if (opts.body !== undefined && opts.body !== null) {
      init.body = typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body);
    }
    const resp = await fetch(url, init);
    const etag = resp.headers.get("ETag") || "";
    const ct = resp.headers.get("Content-Type") || "";
    let body = null;
    if (ct.includes("application/problem+json") || ct.includes("application/json")) {
      try { body = await resp.json(); } catch (_) { body = null; }
    } else if (ct.includes("text/")) {
      body = await resp.text();
    }
    return {
      ok: resp.ok,
      status: resp.status,
      headers: resp.headers,
      etag,
      body,
    };
  }

  // ---- Client class --------------------------------------------------

  function HarnessV2(base) {
    this.base = base || DEFAULT_BASE;
    this._etags = new Map(); // player_id → most-recent ETag
  }

  HarnessV2.prototype._record = function (id, etag) {
    if (id && etag) this._etags.set(id, etag);
  };
  HarnessV2.prototype.cachedETag = function (id) {
    return this._etags.get(id) || "";
  };

  HarnessV2.prototype.healthz = async function () {
    const r = await fetch(`${this.base}/api/v2/healthz`);
    return { ok: r.ok, status: r.status, body: await r.text() };
  };

  HarnessV2.prototype.info = function () {
    return _fetchJSON("GET", `${this.base}/api/v2/info`);
  };

  HarnessV2.prototype.listPlayers = function (filters) {
    const qs = filters ? "?" + new URLSearchParams(filters).toString() : "";
    return _fetchJSON("GET", `${this.base}/api/v2/players${qs}`);
  };
  HarnessV2.prototype.getPlayer = async function (id) {
    const r = await _fetchJSON("GET", `${this.base}/api/v2/players/${id}`);
    if (r.ok) this._record(id, r.etag);
    return r;
  };
  HarnessV2.prototype.createSyntheticPlayer = function (payload) {
    return _fetchJSON("POST", `${this.base}/api/v2/players`, { body: payload || {} });
  };
  HarnessV2.prototype.patchPlayer = async function (id, patch, opts) {
    opts = opts || {};
    const ifMatch = opts.ifMatch || this.cachedETag(id);
    const r = await _fetchJSON("PATCH", `${this.base}/api/v2/players/${id}`, {
      headers: ifMatch ? { "If-Match": ifMatch } : {},
      body: patch,
    });
    if (r.ok) this._record(id, r.etag);
    return r;
  };
  HarnessV2.prototype.deletePlayer = async function (id) {
    const r = await _fetchJSON("DELETE", `${this.base}/api/v2/players/${id}`);
    this._etags.delete(id);
    return r;
  };
  HarnessV2.prototype.deleteAllPlayers = function () {
    return _fetchJSON("DELETE", `${this.base}/api/v2/players`);
  };

  HarnessV2.prototype.networkLog = function (id, limit) {
    const qs = limit ? `?limit=${limit}` : "";
    return _fetchJSON("GET", `${this.base}/api/v2/players/${id}/network${qs}`);
  };

  HarnessV2.prototype.appendFaultRule = async function (id, rule, opts) {
    opts = opts || {};
    const ifMatch = opts.ifMatch || this.cachedETag(id);
    const r = await _fetchJSON("POST", `${this.base}/api/v2/players/${id}/fault_rules`, {
      headers: ifMatch ? { "If-Match": ifMatch } : {},
      body: rule,
    });
    if (r.ok) this._record(id, r.etag);
    return r;
  };
  HarnessV2.prototype.patchFaultRule = async function (id, ruleId, patch, opts) {
    opts = opts || {};
    const ifMatch = opts.ifMatch || this.cachedETag(id);
    const r = await _fetchJSON("PATCH", `${this.base}/api/v2/players/${id}/fault_rules/${ruleId}`, {
      headers: ifMatch ? { "If-Match": ifMatch } : {},
      body: patch,
    });
    if (r.ok) this._record(id, r.etag);
    return r;
  };
  HarnessV2.prototype.deleteFaultRule = async function (id, ruleId, opts) {
    opts = opts || {};
    const ifMatch = opts.ifMatch || this.cachedETag(id);
    const r = await _fetchJSON("DELETE", `${this.base}/api/v2/players/${id}/fault_rules/${ruleId}`, {
      headers: ifMatch ? { "If-Match": ifMatch } : {},
    });
    if (r.ok) this._record(id, r.etag);
    return r;
  };

  HarnessV2.prototype.listGroups = function () {
    return _fetchJSON("GET", `${this.base}/api/v2/player-groups`);
  };
  HarnessV2.prototype.getGroup = function (id) {
    return _fetchJSON("GET", `${this.base}/api/v2/player-groups/${id}`);
  };
  HarnessV2.prototype.createGroup = function (payload) {
    return _fetchJSON("POST", `${this.base}/api/v2/player-groups`, { body: payload });
  };
  HarnessV2.prototype.patchGroup = function (id, patch, opts) {
    opts = opts || {};
    return _fetchJSON("PATCH", `${this.base}/api/v2/player-groups/${id}`, {
      headers: opts.ifMatch ? { "If-Match": opts.ifMatch } : {},
      body: patch,
    });
  };
  HarnessV2.prototype.deleteGroup = function (id) {
    return _fetchJSON("DELETE", `${this.base}/api/v2/player-groups/${id}`);
  };

  HarnessV2.prototype.getPlay = function (id) {
    return _fetchJSON("GET", `${this.base}/api/v2/plays/${id}`);
  };
  HarnessV2.prototype.patchPlay = function (id, patch, opts) {
    opts = opts || {};
    return _fetchJSON("PATCH", `${this.base}/api/v2/plays/${id}`, {
      headers: opts.ifMatch ? { "If-Match": opts.ifMatch } : {},
      body: patch,
    });
  };
  HarnessV2.prototype.appendPlayFaultRule = function (id, rule, opts) {
    opts = opts || {};
    return _fetchJSON("POST", `${this.base}/api/v2/plays/${id}/fault_rules`, {
      headers: opts.ifMatch ? { "If-Match": opts.ifMatch } : {},
      body: rule,
    });
  };
  HarnessV2.prototype.patchPlayFaultRule = function (id, ruleId, patch, opts) {
    opts = opts || {};
    return _fetchJSON("PATCH", `${this.base}/api/v2/plays/${id}/fault_rules/${ruleId}`, {
      headers: opts.ifMatch ? { "If-Match": opts.ifMatch } : {},
      body: patch,
    });
  };
  HarnessV2.prototype.deletePlayFaultRule = function (id, ruleId, opts) {
    opts = opts || {};
    return _fetchJSON("DELETE", `${this.base}/api/v2/plays/${id}/fault_rules/${ruleId}`, {
      headers: opts.ifMatch ? { "If-Match": opts.ifMatch } : {},
    });
  };

  // ---- Public surface ------------------------------------------------

  global.HarnessV2 = HarnessV2;
  global.HarnessV2.subscribeEvents = subscribeEvents;
})(window);
