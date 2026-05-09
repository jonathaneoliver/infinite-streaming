/**
 * v2-repo.js — Repository wrapping HarnessV2.
 *
 * Single point where the v2 wire lives. Models call repo methods;
 * repo translates to /api/v2/* calls with:
 *
 *   - Per-player ETag cache (delegated to HarnessV2's _etags map).
 *   - Per-(playerId, operation) PATCH coalescing in a 50ms window —
 *     slider drags emit one PATCH per window, not per pixel.
 *   - Exponential-backoff retry on transient network errors (max 3
 *     attempts, 100ms / 300ms / 900ms).
 *   - 412 conflicts surfaced as { status: 412 } so callers can roll
 *     back optimistic state and refetch.
 *
 * Swappable for FakeRepo in tests — anything implementing the same
 * method surface works.
 *
 * No external deps. Globals: `window.V2Repo`.
 */
(function (global) {
  "use strict";

  const COALESCE_MS = 50;
  const MAX_ATTEMPTS = 3;

  function V2Repo(client) {
    if (!client) throw new Error('V2Repo: client required');
    this.client = client;
    // Map<string, {timer, latestPatch, resolvers, opFn}>
    // key: `<playerId>:<op>` — coalesces same-op writes inside the window.
    this._coalesce = new Map();
  }

  V2Repo.prototype.cachedETag = function (playerId) {
    return this.client.cachedETag(playerId);
  };

  // ---- Debounce + retry plumbing ------------------------------------

  // _coalescedPATCH(key, patch, fn): schedule fn(mergedPatch) inside
  // the COALESCE_MS window. fn must return a Promise<{ok, status, body}>.
  // Subsequent calls inside the window deep-merge patches into the
  // pending one and return the same eventual promise.
  V2Repo.prototype._coalescedPATCH = function (key, patch, fn) {
    const slot = this._coalesce.get(key);
    if (slot) {
      // Merge into pending. JSON Merge Patch semantics: shallow per-key,
      // deep for objects. Slider drags hit the same leaf repeatedly so
      // last-write-wins is correct.
      slot.latestPatch = mergePatchInto(slot.latestPatch, patch);
      return slot.promise;
    }
    let resolveOuter, rejectOuter;
    const promise = new Promise((res, rej) => { resolveOuter = res; rejectOuter = rej; });
    const newSlot = {
      latestPatch: patch,
      promise,
      timer: setTimeout(() => {
        const merged = newSlot.latestPatch;
        this._coalesce.delete(key);
        this._withRetry(() => fn(merged))
          .then(resolveOuter, rejectOuter);
      }, COALESCE_MS),
    };
    this._coalesce.set(key, newSlot);
    return promise;
  };

  // _withRetry: retry on network failure (TypeError from fetch) or 5xx
  // up to MAX_ATTEMPTS. 4xx (including 412) is final — caller must
  // decide what to do.
  V2Repo.prototype._withRetry = async function (fn) {
    let lastErr;
    for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
      try {
        const r = await fn();
        // 5xx → retry. 4xx → final.
        if (!r.ok && r.status >= 500 && attempt < MAX_ATTEMPTS - 1) {
          await sleep(100 * Math.pow(3, attempt));
          continue;
        }
        return r;
      } catch (err) {
        // Network errors throw — TypeError from fetch.
        lastErr = err;
        if (attempt < MAX_ATTEMPTS - 1) {
          await sleep(100 * Math.pow(3, attempt));
          continue;
        }
      }
    }
    throw lastErr || new Error('retry exhausted');
  };

  function sleep(ms) {
    return new Promise(r => setTimeout(r, ms));
  }

  // mergePatchInto: deep-merge `incoming` into `base` per RFC 7396.
  // null in `incoming` means "delete this key". Returns a new object.
  function mergePatchInto(base, incoming) {
    if (incoming === null || typeof incoming !== 'object' || Array.isArray(incoming)) {
      return incoming;
    }
    const out = (base && typeof base === 'object' && !Array.isArray(base))
      ? Object.assign({}, base)
      : {};
    for (const k of Object.keys(incoming)) {
      const v = incoming[k];
      if (v === null) {
        delete out[k];
      } else if (typeof v === 'object' && !Array.isArray(v)) {
        out[k] = mergePatchInto(out[k], v);
      } else {
        out[k] = v;
      }
    }
    return out;
  }

  // ---- Players ------------------------------------------------------

  V2Repo.prototype.listPlayers = function (filters) {
    return this.client.listPlayers(filters);
  };
  V2Repo.prototype.getPlayer = function (id) {
    return this.client.getPlayer(id);
  };
  V2Repo.prototype.createSyntheticPlayer = function (payload) {
    return this.client.createSyntheticPlayer(payload);
  };
  V2Repo.prototype.deletePlayer = function (id) {
    return this.client.deletePlayer(id);
  };
  V2Repo.prototype.deleteAllPlayers = function () {
    return this.client.deleteAllPlayers();
  };
  V2Repo.prototype.networkLog = function (id, limit) {
    return this.client.networkLog(id, limit);
  };

  // patchPlayer: coalesce + retry. Pass `op` to scope the coalesce key
  // so unrelated mutations don't merge (e.g. label set + shape change
  // in the same window should be two PATCHes, not one).
  V2Repo.prototype.patchPlayer = function (playerId, patch, op) {
    const key = playerId + ':patch:' + (op || 'default');
    return this._coalescedPATCH(key, patch, (merged) =>
      this.client.patchPlayer(playerId, merged));
  };

  // ---- Fault rules --------------------------------------------------

  V2Repo.prototype.appendFaultRule = function (playerId, rule) {
    // Append is not coalesced — each call is a distinct rule.
    return this._withRetry(() => this.client.appendFaultRule(playerId, rule));
  };
  V2Repo.prototype.patchFaultRule = function (playerId, ruleId, patch) {
    const key = playerId + ':rule:' + ruleId;
    return this._coalescedPATCH(key, patch, (merged) =>
      this.client.patchFaultRule(playerId, ruleId, merged));
  };
  V2Repo.prototype.deleteFaultRule = function (playerId, ruleId) {
    return this._withRetry(() => this.client.deleteFaultRule(playerId, ruleId));
  };

  // ---- Groups -------------------------------------------------------

  V2Repo.prototype.listGroups = function () {
    return this.client.listGroups();
  };
  V2Repo.prototype.getGroup = function (id) {
    return this.client.getGroup(id);
  };
  V2Repo.prototype.createGroup = function (payload) {
    return this._withRetry(() => this.client.createGroup(payload));
  };
  V2Repo.prototype.patchGroup = function (id, patch) {
    const key = 'group:' + id;
    return this._coalescedPATCH(key, patch, (merged) =>
      this.client.patchGroup(id, merged));
  };
  V2Repo.prototype.deleteGroup = function (id) {
    return this._withRetry(() => this.client.deleteGroup(id));
  };

  // ---- Plays --------------------------------------------------------

  V2Repo.prototype.getPlay = function (id) {
    return this.client.getPlay(id);
  };
  V2Repo.prototype.patchPlay = function (id, patch) {
    const key = 'play:' + id;
    return this._coalescedPATCH(key, patch, (merged) =>
      this.client.patchPlay(id, merged));
  };
  V2Repo.prototype.appendPlayFaultRule = function (id, rule) {
    return this._withRetry(() => this.client.appendPlayFaultRule(id, rule));
  };
  V2Repo.prototype.patchPlayFaultRule = function (id, ruleId, patch) {
    const key = 'play:' + id + ':rule:' + ruleId;
    return this._coalescedPATCH(key, patch, (merged) =>
      this.client.patchPlayFaultRule(id, ruleId, merged));
  };
  V2Repo.prototype.deletePlayFaultRule = function (id, ruleId) {
    return this._withRetry(() => this.client.deletePlayFaultRule(id, ruleId));
  };

  // ---- SSE passthrough ---------------------------------------------

  V2Repo.prototype.subscribeEvents = function (opts) {
    // HarnessV2.subscribeEvents is a static; client itself wraps it.
    return global.HarnessV2.subscribeEvents(opts);
  };

  // ---- Test surface -------------------------------------------------

  V2Repo._mergePatchInto = mergePatchInto;
  V2Repo._COALESCE_MS = COALESCE_MS;
  V2Repo._MAX_ATTEMPTS = MAX_ATTEMPTS;

  global.V2Repo = V2Repo;
})(window);
