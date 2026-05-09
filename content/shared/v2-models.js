/**
 * v2-models.js — domain models for the v2 dashboard.
 *
 * Layered between view and repository. Holds canonical state, exposes
 * narrow setters, emits typed events, and coordinates optimistic
 * updates with rollback on 412 conflicts.
 *
 * Model graph (see plan Phase Q):
 *
 *   PlayersStore      Map<id, Player>, hydrates from GET /players,
 *                     mutates from SSE player.created/updated/deleted.
 *
 *   Player            id, displayId, controlRevision, labels, shape,
 *                     faultRules, currentPlay, metrics, serverMetrics,
 *                     faultCounters, originationIp, firstSeenAt,
 *                     lastSeenAt.
 *
 *   ShapeProfile      rateMbps, delayMs, lossPct, transportFault,
 *                     pattern.
 *
 *   FaultRuleSet      array-like, append/update/remove.
 *   FaultRule         id, type, frequency, consecutive, mode, filter.
 *
 *   GroupsStore       Map<id, Group>, hydrates from GET /player-groups.
 *   Group             id, label, labels, memberPlayerIds.
 *
 *   PlayerMetrics     read-only view (player-reported telemetry).
 *   ServerMetrics     read-only view (server-observed telemetry).
 *   FaultCounters     read-only view (per-device fault hit counters).
 *   NetworkLogEntry   read-only HAR-shaped record.
 *
 * Every model class extends EventTargetLike, emitting:
 *   change   { field?, oldValue?, newValue?, source: 'local'|'remote' }
 *   pending  { operation, targetField? }
 *   error    { operation, response, willRetry }
 *
 * Optimistic updates: setters apply locally first, snapshot the prior
 * value, fire the PATCH through repo, and on 412 roll back + refetch.
 *
 * Globals: window.V2Models = { Player, ShapeProfile, ..., PlayersStore,
 *                              GroupsStore }.
 */
(function (global) {
  "use strict";

  // ---- Tiny EventTarget shim ----------------------------------------
  // We don't need DOM EventTarget's quirks — just on/off/emit. Keeps
  // the model usable in test harnesses without a DOM.

  function EventTargetLike() {
    this._listeners = Object.create(null);
  }
  EventTargetLike.prototype.on = function (type, cb) {
    (this._listeners[type] = this._listeners[type] || []).push(cb);
    return () => this.off(type, cb);
  };
  EventTargetLike.prototype.off = function (type, cb) {
    const list = this._listeners[type];
    if (!list) return;
    const i = list.indexOf(cb);
    if (i >= 0) list.splice(i, 1);
  };
  EventTargetLike.prototype.emit = function (type, payload) {
    const list = this._listeners[type];
    if (!list) return;
    // Iterate copy in case a listener mutates the list mid-emit.
    for (const cb of list.slice()) {
      try { cb(payload, this); } catch (err) {
        // A listener throwing should not abort other listeners or the
        // model itself. Surface to console rather than blackholing.
        if (global.console) global.console.error('[v2-models] listener threw', err);
      }
    }
  };

  // ---- Optimistic update helper -------------------------------------

  // applyOptimistic: snapshot field, set new value, emit change+pending,
  // run patchFn, on success clear pending+emit change(remote), on 412
  // roll back + refetch, on other error emit error.
  //
  //   model.applyOptimistic('rateMbps', n, 'patchShape', () => repo.patchPlayer(...))
  function applyOptimistic(model, field, newValue, operation, patchFn, opts) {
    opts = opts || {};
    const oldValue = readField(model, field);
    writeField(model, field, newValue);
    model.emit('change', { field, oldValue, newValue, source: 'local' });
    model.emit('pending', { operation, targetField: field });

    return patchFn().then(
      (resp) => {
        model.emit('pending', { operation, targetField: field, done: true });
        if (resp && resp.ok) {
          // Server may have echoed a richer payload — let caller absorb.
          if (opts.absorb && resp.body) opts.absorb(resp.body);
          return resp;
        }
        if (resp && resp.status === 412) {
          // ETag conflict — roll back, surface error, refetch from
          // server so the local model matches authoritative state.
          writeField(model, field, oldValue);
          model.emit('change', { field, oldValue: newValue, newValue: oldValue, source: 'rollback' });
          model.emit('error', { operation, response: resp, willRetry: false });
          if (opts.refetch) opts.refetch();
          return resp;
        }
        // 4xx/5xx — roll back, surface error.
        writeField(model, field, oldValue);
        model.emit('change', { field, oldValue: newValue, newValue: oldValue, source: 'rollback' });
        model.emit('error', { operation, response: resp, willRetry: false });
        return resp;
      },
      (err) => {
        // Network failure after retry exhaustion.
        model.emit('pending', { operation, targetField: field, done: true });
        writeField(model, field, oldValue);
        model.emit('change', { field, oldValue: newValue, newValue: oldValue, source: 'rollback' });
        model.emit('error', { operation, message: err && err.message, willRetry: false });
        throw err;
      }
    );
  }

  function readField(obj, path) {
    const parts = path.split('.');
    let cur = obj;
    for (const p of parts) cur = cur && cur[p];
    return cur;
  }
  function writeField(obj, path, value) {
    const parts = path.split('.');
    let cur = obj;
    for (let i = 0; i < parts.length - 1; i++) cur = cur[parts[i]];
    cur[parts[parts.length - 1]] = value;
  }

  // ---- TransferTimeouts ---------------------------------------------

  function TransferTimeouts(parent, raw) {
    EventTargetLike.call(this);
    this._parent = parent;
    this._absorb(raw || {});
  }
  TransferTimeouts.prototype = Object.create(EventTargetLike.prototype);
  TransferTimeouts.prototype.constructor = TransferTimeouts;

  TransferTimeouts.prototype._absorb = function (raw) {
    this.appliesSegments = raw.applies_segments !== false;
    this.appliesManifests = !!raw.applies_manifests;
    this.appliesMaster = !!raw.applies_master;
    this.activeTimeoutSeconds = numericOrNull(raw.active_timeout_seconds) || 0;
    this.idleTimeoutSeconds = numericOrNull(raw.idle_timeout_seconds) || 0;
  };

  TransferTimeouts.prototype._patchField = function (field, newValue, partialPatch) {
    const player = this._parent;
    return applyOptimistic(this, field, newValue, 'patchTransferTimeouts',
      () => player._repo.patchPlayer(player.id, { transfer_timeouts: partialPatch }, 'transfer_timeouts'),
      { refetch: () => player._refetch() });
  };
  TransferTimeouts.prototype.setActive = function (seconds) {
    return this._patchField('activeTimeoutSeconds', seconds, { active_timeout_seconds: seconds });
  };
  TransferTimeouts.prototype.setIdle = function (seconds) {
    return this._patchField('idleTimeoutSeconds', seconds, { idle_timeout_seconds: seconds });
  };
  TransferTimeouts.prototype.setAppliesSegments = function (b) {
    return this._patchField('appliesSegments', !!b, { applies_segments: !!b });
  };
  TransferTimeouts.prototype.setAppliesManifests = function (b) {
    return this._patchField('appliesManifests', !!b, { applies_manifests: !!b });
  };
  TransferTimeouts.prototype.setAppliesMaster = function (b) {
    return this._patchField('appliesMaster', !!b, { applies_master: !!b });
  };

  // ---- ContentManipulation ------------------------------------------

  function ContentManipulation(parent, raw) {
    EventTargetLike.call(this);
    this._parent = parent;
    this._absorb(raw || {});
  }
  ContentManipulation.prototype = Object.create(EventTargetLike.prototype);
  ContentManipulation.prototype.constructor = ContentManipulation;

  ContentManipulation.prototype._absorb = function (raw) {
    this.stripCodecs = !!raw.strip_codecs;
    this.stripAverageBandwidth = !!raw.strip_average_bandwidth;
    this.overstateBandwidth = !!raw.overstate_bandwidth;
    this.allowedVariants = (raw.allowed_variants || []).slice();
    this.liveOffset = numericOrNull(raw.live_offset) || 0;
  };

  ContentManipulation.prototype._patchField = function (field, newValue, partialPatch) {
    const player = this._parent;
    return applyOptimistic(this, field, newValue, 'patchContent',
      () => player._repo.patchPlayer(player.id, { content: partialPatch }, 'content'),
      { refetch: () => player._refetch() });
  };
  ContentManipulation.prototype.setStripCodecs = function (b) {
    return this._patchField('stripCodecs', !!b, { strip_codecs: !!b });
  };
  ContentManipulation.prototype.setStripAverageBandwidth = function (b) {
    return this._patchField('stripAverageBandwidth', !!b, { strip_average_bandwidth: !!b });
  };
  ContentManipulation.prototype.setOverstateBandwidth = function (b) {
    return this._patchField('overstateBandwidth', !!b, { overstate_bandwidth: !!b });
  };
  ContentManipulation.prototype.setLiveOffset = function (n) {
    return this._patchField('liveOffset', n, { live_offset: n });
  };
  ContentManipulation.prototype.setAllowedVariants = function (arr) {
    const next = Array.isArray(arr) ? arr.slice() : [];
    return this._patchField('allowedVariants', next, { allowed_variants: next });
  };

  // ---- ShapeProfile -------------------------------------------------

  function ShapeProfile(parent, raw) {
    EventTargetLike.call(this);
    this._parent = parent; // Player
    this._absorb(raw || {});
  }
  ShapeProfile.prototype = Object.create(EventTargetLike.prototype);
  ShapeProfile.prototype.constructor = ShapeProfile;

  ShapeProfile.prototype._absorb = function (raw) {
    this.rateMbps = numericOrNull(raw.rate_mbps);
    this.delayMs = numericOrNull(raw.delay_ms);
    this.lossPct = numericOrNull(raw.loss_pct);
    this.transportFault = raw.transport_fault || null;
    this.pattern = raw.pattern || null;
  };

  ShapeProfile.prototype.toMergePatch = function () {
    // Only include defined fields. Caller wraps under {shape: ...}.
    const out = {};
    if (this.rateMbps != null) out.rate_mbps = this.rateMbps;
    if (this.delayMs != null) out.delay_ms = this.delayMs;
    if (this.lossPct != null) out.loss_pct = this.lossPct;
    if (this.transportFault) out.transport_fault = this.transportFault;
    if (this.pattern) out.pattern = this.pattern;
    return out;
  };

  ShapeProfile.prototype.setRate = function (mbps) {
    return this._patchField('rateMbps', mbps, { rate_mbps: mbps });
  };
  ShapeProfile.prototype.setDelay = function (ms) {
    return this._patchField('delayMs', ms, { delay_ms: ms });
  };
  ShapeProfile.prototype.setLoss = function (pct) {
    return this._patchField('lossPct', pct, { loss_pct: pct });
  };
  ShapeProfile.prototype.setTransportFault = function (tf) {
    return this._patchField('transportFault', tf, { transport_fault: tf });
  };
  ShapeProfile.prototype.setPattern = function (pattern) {
    return this._patchField('pattern', pattern, { pattern: pattern });
  };
  ShapeProfile.prototype.clearPattern = function () {
    return this._patchField('pattern', null, { pattern: null });
  };

  ShapeProfile.prototype._patchField = function (field, newValue, shapePatch) {
    const player = this._parent;
    return applyOptimistic(this, field, newValue, 'patchShape',
      () => player._repo.patchPlayer(player.id, { shape: shapePatch }, 'shape'),
      { refetch: () => player._refetch() });
  };

  // ---- FaultRuleSet -------------------------------------------------

  function FaultRuleSet(parent, raw) {
    EventTargetLike.call(this);
    this._parent = parent;
    this._absorb(raw || []);
  }
  FaultRuleSet.prototype = Object.create(EventTargetLike.prototype);
  FaultRuleSet.prototype.constructor = FaultRuleSet;

  FaultRuleSet.prototype._absorb = function (raw) {
    this.rules = (raw || []).map(r => new FaultRule(this, r));
  };

  FaultRuleSet.prototype.append = function (rule) {
    const player = this._parent;
    this.emit('pending', { operation: 'appendFaultRule' });
    return player._repo.appendFaultRule(player.id, rule).then((resp) => {
      this.emit('pending', { operation: 'appendFaultRule', done: true });
      if (resp && resp.ok && resp.body) {
        this.rules.push(new FaultRule(this, resp.body));
        this.emit('change', { field: 'rules', source: 'local' });
      } else {
        this.emit('error', { operation: 'appendFaultRule', response: resp });
      }
      return resp;
    });
  };

  FaultRuleSet.prototype.update = function (ruleId, patch) {
    const player = this._parent;
    const idx = this.rules.findIndex(r => r.id === ruleId);
    if (idx < 0) return Promise.resolve({ ok: false, status: 404 });
    const rule = this.rules[idx];
    const snapshot = rule.toJSON();
    rule._absorb(Object.assign({}, snapshot, patch));
    this.emit('change', { field: 'rules', source: 'local' });
    this.emit('pending', { operation: 'patchFaultRule', targetField: ruleId });
    return player._repo.patchFaultRule(player.id, ruleId, patch).then((resp) => {
      this.emit('pending', { operation: 'patchFaultRule', targetField: ruleId, done: true });
      if (!resp.ok) {
        rule._absorb(snapshot);
        this.emit('change', { field: 'rules', source: 'rollback' });
        this.emit('error', { operation: 'patchFaultRule', response: resp });
      }
      return resp;
    });
  };

  FaultRuleSet.prototype.remove = function (ruleId) {
    const player = this._parent;
    const idx = this.rules.findIndex(r => r.id === ruleId);
    if (idx < 0) return Promise.resolve({ ok: false, status: 404 });
    const removed = this.rules.splice(idx, 1)[0];
    this.emit('change', { field: 'rules', source: 'local' });
    this.emit('pending', { operation: 'deleteFaultRule', targetField: ruleId });
    return player._repo.deleteFaultRule(player.id, ruleId).then((resp) => {
      this.emit('pending', { operation: 'deleteFaultRule', targetField: ruleId, done: true });
      if (!resp.ok) {
        // Rollback: restore at the original index.
        this.rules.splice(idx, 0, removed);
        this.emit('change', { field: 'rules', source: 'rollback' });
        this.emit('error', { operation: 'deleteFaultRule', response: resp });
      }
      return resp;
    });
  };

  // ---- FaultRule ----------------------------------------------------

  function FaultRule(parent, raw) {
    this._parent = parent;
    this._absorb(raw || {});
  }
  FaultRule.prototype._absorb = function (raw) {
    this.id = raw.id || null;
    this.type = raw.type || 'none';
    this.frequency = raw.frequency != null ? raw.frequency : null;
    this.consecutive = raw.consecutive != null ? raw.consecutive : null;
    this.mode = raw.mode || null;
    this.filter = raw.filter || null;
  };
  FaultRule.prototype.toJSON = function () {
    const out = { type: this.type };
    if (this.id != null) out.id = this.id;
    if (this.frequency != null) out.frequency = this.frequency;
    if (this.consecutive != null) out.consecutive = this.consecutive;
    if (this.mode != null) out.mode = this.mode;
    if (this.filter != null) out.filter = this.filter;
    return out;
  };

  // ---- Read-only views (metrics + counters) -------------------------

  function PlayerMetrics(raw) { this._absorb(raw || {}); }
  PlayerMetrics.prototype._absorb = function (raw) {
    this.videoResolution = raw.video_resolution || null;
    this.displayResolution = raw.display_resolution || null;
    this.videoBitrateMbps = numericOrNull(raw.video_bitrate_mbps);
    this.videoQualityPct = numericOrNull(raw.video_quality_pct);
    this.avgNetworkBitrateMbps = numericOrNull(raw.avg_network_bitrate_mbps);
    this.networkBitrateMbps = numericOrNull(raw.network_bitrate_mbps);
    this.bufferDepthS = numericOrNull(raw.buffer_depth_s);
    this.bufferEndS = numericOrNull(raw.buffer_end_s);
    this.seekableEndS = numericOrNull(raw.seekable_end_s);
    this.liveEdgeS = numericOrNull(raw.live_edge_s);
    this.liveOffsetS = numericOrNull(raw.live_offset_s);
    this.trueOffsetS = numericOrNull(raw.true_offset_s);
    this.positionS = numericOrNull(raw.position_s);
    this.playbackRate = numericOrNull(raw.playback_rate);
    this.firstFrameTimeS = numericOrNull(raw.first_frame_time_s);
    this.videoStartTimeS = numericOrNull(raw.video_start_time_s);
    this.stalls = numericOrNull(raw.stalls);
    this.stallTimeS = numericOrNull(raw.stall_time_s);
    this.lastStallTimeS = numericOrNull(raw.last_stall_time_s);
    this.framesDisplayed = numericOrNull(raw.frames_displayed);
    this.droppedFrames = numericOrNull(raw.dropped_frames);
    this.playerRestarts = numericOrNull(raw.player_restarts);
    this.loopCountPlayer = numericOrNull(raw.loop_count_player);
    this.loopCountIncrement = numericOrNull(raw.loop_count_increment);
    this.profileShiftCount = numericOrNull(raw.profile_shift_count);
    this.lastEvent = raw.last_event || null;
    this.triggerType = raw.trigger_type || null;
    this.state = raw.state || null;
    this.error = raw.error || null;
    this.source = raw.source || null;
    this.eventTime = raw.event_time || null;
  };

  function ServerMetrics(raw) { this._absorb(raw || {}); }
  ServerMetrics.prototype._absorb = function (raw) {
    this.renditionUrl = raw.rendition_url || null;
    this.renditionMbps = numericOrNull(raw.rendition_mbps);
    this.serverRendition = raw.server_rendition || null;
    this.rttMs = numericOrNull(raw.rtt_ms);
    this.rttMinMs = numericOrNull(raw.rtt_min_ms);
    this.rttMaxMs = numericOrNull(raw.rtt_max_ms);
    this.rttMinLifetimeMs = numericOrNull(raw.rtt_min_lifetime_ms);
    this.rttVarMs = numericOrNull(raw.rtt_var_ms);
    this.rtoMs = numericOrNull(raw.rto_ms);
    this.pathPingRttMs = numericOrNull(raw.path_ping_rtt_ms);
    this.rttStale = raw.rtt_stale != null ? !!raw.rtt_stale : null;
    this.bytesInTotal = numericOrNull(raw.bytes_in_total);
    this.bytesOutTotal = numericOrNull(raw.bytes_out_total);
    this.bytesInLast = numericOrNull(raw.bytes_in_last);
    this.bytesOutLast = numericOrNull(raw.bytes_out_last);
    this.bytesLastTs = numericOrNull(raw.bytes_last_ts);
    this.mbpsShaperAvg = numericOrNull(raw.mbps_shaper_avg);
    this.mbpsShaperRate = numericOrNull(raw.mbps_shaper_rate);
    this.mbpsTransferRate = numericOrNull(raw.mbps_transfer_rate);
    this.mbpsTransferComplete = numericOrNull(raw.mbps_transfer_complete);
    this.mbpsIn = numericOrNull(raw.mbps_in);
    this.mbpsOut = numericOrNull(raw.mbps_out);
    this.mbpsInAvg = numericOrNull(raw.mbps_in_avg);
    this.mbpsInActive = numericOrNull(raw.mbps_in_active);
    this.measuredMbps = numericOrNull(raw.measured_mbps);
    this.measurementWindowIo = numericOrNull(raw.measurement_window_io);
    this.measurementWindowActive = numericOrNull(raw.measurement_window_active);
  };

  function FaultCounters(raw) { this._absorb(raw || {}); }
  FaultCounters.prototype._absorb = function (raw) {
    this.byKind = Object.assign({}, raw);
  };
  FaultCounters.prototype.total = function () {
    if (this.byKind.total != null) return this.byKind.total;
    let n = 0;
    for (const k of Object.keys(this.byKind)) n += this.byKind[k] || 0;
    return n;
  };

  function NetworkLogEntry(raw) {
    this.timestamp = raw.timestamp || null;
    this.method = raw.method || null;
    this.url = raw.url || null;
    this.upstreamUrl = raw.upstream_url || null;
    this.path = raw.path || null;
    this.requestKind = raw.request_kind || null;
    this.status = numericOrNull(raw.status);
    this.bytesIn = numericOrNull(raw.bytes_in);
    this.bytesOut = numericOrNull(raw.bytes_out);
    this.contentType = raw.content_type || null;
    this.playId = raw.play_id || null;
    this.ttfbMs = numericOrNull(raw.ttfb_ms);
    this.totalMs = numericOrNull(raw.total_ms);
    this.dnsMs = numericOrNull(raw.dns_ms);
    this.connectMs = numericOrNull(raw.connect_ms);
    this.tlsMs = numericOrNull(raw.tls_ms);
    this.transferMs = numericOrNull(raw.transfer_ms);
    this.clientWaitMs = numericOrNull(raw.client_wait_ms);
    this.faulted = !!raw.faulted;
    this.faultType = raw.fault_type || null;
    this.faultAction = raw.fault_action || null;
    this.faultCategory = raw.fault_category || null;
  }

  // ---- Player -------------------------------------------------------

  function Player(repo, raw) {
    EventTargetLike.call(this);
    this._repo = repo;
    this._absorb(raw || {});
  }
  Player.prototype = Object.create(EventTargetLike.prototype);
  Player.prototype.constructor = Player;

  Player.prototype._absorb = function (raw) {
    this.id = raw.id;
    this.displayId = raw.display_id;
    this.controlRevision = raw.control_revision;
    this.labels = Object.assign({}, raw.labels || {});
    this.originationIp = raw.origination_ip || null;
    this.playerIp = raw.player_ip || null;
    this.userAgent = raw.user_agent || null;
    this.loopCountServer = raw.loop_count_server != null ? raw.loop_count_server : null;
    this.firstSeenAt = raw.first_seen_at || null;
    this.lastSeenAt = raw.last_seen_at || null;
    if (this.shape) this.shape._absorb(raw.shape || {});
    else this.shape = new ShapeProfile(this, raw.shape || {});
    if (this.faultRules) this.faultRules._absorb(raw.fault_rules || []);
    else this.faultRules = new FaultRuleSet(this, raw.fault_rules || []);
    this.currentPlay = raw.current_play || null; // Phase Q5 wraps in Play model
    this.metrics = raw.player_metrics ? new PlayerMetrics(raw.player_metrics) : null;
    this.serverMetrics = raw.server_metrics ? new ServerMetrics(raw.server_metrics) : null;
    this.faultCounters = raw.fault_counters ? new FaultCounters(raw.fault_counters) : null;
    if (this.transferTimeouts) this.transferTimeouts._absorb(raw.transfer_timeouts || {});
    else this.transferTimeouts = new TransferTimeouts(this, raw.transfer_timeouts || {});
    if (this.content) this.content._absorb(raw.content || {});
    else this.content = new ContentManipulation(this, raw.content || {});
  };

  Player.prototype.setLabel = function (key, value) {
    const oldLabels = Object.assign({}, this.labels);
    const newLabels = Object.assign({}, this.labels, { [key]: value });
    return applyOptimistic(this, 'labels', newLabels, 'patchLabels',
      () => this._repo.patchPlayer(this.id, { labels: { [key]: value } }, 'labels'),
      { refetch: () => this._refetch() });
  };

  Player.prototype.clearLabel = function (key) {
    if (!(key in this.labels)) return Promise.resolve({ ok: true });
    const oldLabels = Object.assign({}, this.labels);
    const newLabels = Object.assign({}, this.labels);
    delete newLabels[key];
    return applyOptimistic(this, 'labels', newLabels, 'patchLabels',
      () => this._repo.patchPlayer(this.id, { labels: { [key]: null } }, 'labels'),
      { refetch: () => this._refetch() });
  };

  Player.prototype.delete = function () {
    this.emit('pending', { operation: 'deletePlayer' });
    return this._repo.deletePlayer(this.id).then((resp) => {
      this.emit('pending', { operation: 'deletePlayer', done: true });
      if (!resp.ok) this.emit('error', { operation: 'deletePlayer', response: resp });
      return resp;
    });
  };

  Player.prototype._refetch = function () {
    return this._repo.getPlayer(this.id).then((resp) => {
      if (resp.ok && resp.body) {
        this._absorb(resp.body);
        this.controlRevision = resp.body.control_revision;
        this.emit('change', { source: 'remote' });
      }
      return resp;
    });
  };

  // Called by PlayersStore when an SSE player.updated frame lands.
  Player.prototype._absorbFromRemote = function (raw) {
    this._absorb(raw);
    this.emit('change', { source: 'remote' });
  };

  // ---- PlayersStore -------------------------------------------------

  function PlayersStore(repo) {
    EventTargetLike.call(this);
    this._repo = repo;
    this.players = new Map(); // id → Player
    this.connectionState = 'disconnected';
    this._sub = null;
  }
  PlayersStore.prototype = Object.create(EventTargetLike.prototype);
  PlayersStore.prototype.constructor = PlayersStore;

  PlayersStore.prototype.hydrate = function () {
    return this._repo.listPlayers({ include: 'raw' }).then((resp) => {
      if (!resp.ok || !resp.body) return resp;
      const items = (resp.body.items || []);
      const seen = new Set();
      for (const raw of items) {
        seen.add(raw.id);
        const existing = this.players.get(raw.id);
        if (existing) existing._absorbFromRemote(raw);
        else this._addFromRemote(raw);
      }
      // Drop players the server no longer reports.
      for (const id of Array.from(this.players.keys())) {
        if (!seen.has(id)) {
          this.players.delete(id);
          this.emit('player.removed', { id });
        }
      }
      this.emit('change', { source: 'remote', reason: 'hydrate' });
      return resp;
    });
  };

  PlayersStore.prototype.connect = function () {
    if (this._sub) return;
    this._setState('connecting');
    this._sub = this._repo.subscribeEvents({
      callbacks: {
        onConnect: () => { this._setState('connected'); this.hydrate(); },
        onDisconnect: () => { this._setState('reconnecting'); },
        onError: (err) => { this.emit('error', { operation: 'sse', message: err && err.message }); },
        onPlayerCreated: (raw) => this._addFromRemote(raw),
        onPlayerUpdated: (raw) => {
          const p = this.players.get(raw.id);
          if (p) p._absorbFromRemote(raw);
          else this._addFromRemote(raw);
        },
        onPlayerDeleted: (data) => {
          const id = data && (data.player_id || data.id);
          if (id && this.players.delete(id)) {
            this.emit('player.removed', { id });
            this.emit('change', { source: 'remote', reason: 'deleted' });
          }
        },
        onReplayGap: () => { this.hydrate(); },
        onHeartbeat: () => {},
      },
    });
  };

  PlayersStore.prototype.disconnect = function () {
    if (this._sub) { this._sub.cancel(); this._sub = null; }
    this._setState('disconnected');
  };

  PlayersStore.prototype._setState = function (s) {
    if (this.connectionState === s) return;
    this.connectionState = s;
    this.emit('connection', { state: s });
  };

  PlayersStore.prototype._addFromRemote = function (raw) {
    const p = new Player(this._repo, raw);
    this.players.set(raw.id, p);
    this.emit('player.added', { id: raw.id, player: p });
    this.emit('change', { source: 'remote', reason: 'added' });
  };

  PlayersStore.prototype.get = function (id) { return this.players.get(id); };
  PlayersStore.prototype.list = function () { return Array.from(this.players.values()); };

  // ---- Group / GroupsStore -----------------------------------------

  function Group(repo, raw) {
    EventTargetLike.call(this);
    this._repo = repo;
    this._absorb(raw || {});
  }
  Group.prototype = Object.create(EventTargetLike.prototype);
  Group.prototype.constructor = Group;

  Group.prototype._absorb = function (raw) {
    this.id = raw.id;
    this.label = raw.label || null;
    this.labels = Object.assign({}, raw.labels || {});
    this.memberPlayerIds = (raw.member_player_ids || raw.members || []).slice();
    this.controlRevision = raw.control_revision || null;
  };

  Group.prototype.addMember = function (playerId) {
    const next = this.memberPlayerIds.slice();
    if (next.indexOf(playerId) < 0) next.push(playerId);
    return applyOptimistic(this, 'memberPlayerIds', next, 'patchGroup',
      () => this._repo.patchGroup(this.id, { member_player_ids: next }));
  };
  Group.prototype.removeMember = function (playerId) {
    const next = this.memberPlayerIds.filter(id => id !== playerId);
    return applyOptimistic(this, 'memberPlayerIds', next, 'patchGroup',
      () => this._repo.patchGroup(this.id, { member_player_ids: next }));
  };
  Group.prototype.setLabel = function (key, value) {
    const next = Object.assign({}, this.labels, { [key]: value });
    return applyOptimistic(this, 'labels', next, 'patchGroup',
      () => this._repo.patchGroup(this.id, { labels: { [key]: value } }));
  };
  Group.prototype.disband = function () {
    this.emit('pending', { operation: 'deleteGroup' });
    return this._repo.deleteGroup(this.id).then((resp) => {
      this.emit('pending', { operation: 'deleteGroup', done: true });
      if (!resp.ok) this.emit('error', { operation: 'deleteGroup', response: resp });
      return resp;
    });
  };

  function GroupsStore(repo) {
    EventTargetLike.call(this);
    this._repo = repo;
    this.groups = new Map();
  }
  GroupsStore.prototype = Object.create(EventTargetLike.prototype);
  GroupsStore.prototype.constructor = GroupsStore;

  GroupsStore.prototype.hydrate = function () {
    return this._repo.listGroups().then((resp) => {
      if (!resp.ok || !resp.body) return resp;
      const items = resp.body.items || [];
      const seen = new Set();
      for (const raw of items) {
        seen.add(raw.id);
        const existing = this.groups.get(raw.id);
        if (existing) existing._absorb(raw);
        else this.groups.set(raw.id, new Group(this._repo, raw));
      }
      for (const id of Array.from(this.groups.keys())) {
        if (!seen.has(id)) this.groups.delete(id);
      }
      this.emit('change', { source: 'remote', reason: 'hydrate' });
      return resp;
    });
  };

  GroupsStore.prototype.create = function (payload) {
    return this._repo.createGroup(payload).then((resp) => {
      if (resp.ok && resp.body) {
        const g = new Group(this._repo, resp.body);
        this.groups.set(g.id, g);
        this.emit('change', { source: 'local', reason: 'created' });
      }
      return resp;
    });
  };

  GroupsStore.prototype.get = function (id) { return this.groups.get(id); };
  GroupsStore.prototype.list = function () { return Array.from(this.groups.values()); };

  // ---- Helpers ------------------------------------------------------

  function numericOrNull(v) {
    if (v == null) return null;
    const n = typeof v === 'number' ? v : Number(v);
    return Number.isFinite(n) ? n : null;
  }

  // ---- Public surface -----------------------------------------------

  global.V2Models = {
    EventTargetLike,
    Player,
    PlayersStore,
    ShapeProfile,
    FaultRuleSet,
    FaultRule,
    PlayerMetrics,
    ServerMetrics,
    FaultCounters,
    NetworkLogEntry,
    Group,
    GroupsStore,
    TransferTimeouts,
    ContentManipulation,
    // For tests:
    _applyOptimistic: applyOptimistic,
  };
})(window);
