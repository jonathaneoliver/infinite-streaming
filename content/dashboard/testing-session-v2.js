/**
 * testing-session-v2.js — bootstrap for the v2-native testing-session
 * page. Builds the page entirely from V2Models / V2Repo / V2Charts and
 * never touches /api/sessions* / /api/session/*.
 *
 * Phase Q5 sub-phases:
 *   Q5b (this file, in-progress) — page chrome, video player engine,
 *     player engine selector, Allow-4K + Auto-Recovery + PiP toggles,
 *     play_id rotation, stats grid, debug log, header / badges /
 *     connection pill.
 *   Q5c-Q5j — fault injection / shape / charts / network log /
 *     groups will hang off the same models. Each section installs its
 *     own subscriber; the bootstrap below just wires the player and
 *     the always-on chrome.
 *
 * Hard rule: no innerHTML for any string sourced from a Player
 * model. Use t() (text node) helper. Static markup may use innerHTML.
 */
(() => {
  "use strict";
  const M = window.V2Models;
  const params = new URLSearchParams(window.location.search);
  const playerIdParam = params.get('player_id') || params.get('session_id') || '';

  // ---- Constants -----------------------------------------------------
  const NATIVE_4K_KEY = 'ismPrefer4kNative';
  const AUTO_RECOVERY_KEY = 'ismAutoRecovery';
  const PLAY_ID_ROTATION_LS_KEY = 'ismPlayIdRotationSeconds';
  const PLAYER_ENGINE_LS_KEY = 'ismPlayerEngine';
  const PLAY_ID_QUIESCENCE_MS = 60_000;

  // ---- Wire-up -------------------------------------------------------
  const client = new window.HarnessV2();
  const repo = new window.V2Repo(client);
  const players = new M.PlayersStore(repo);
  const groups = new M.GroupsStore(repo);
  const toasts = new window.V2Toast();
  toasts.attach(document.body);

  // ---- Element handles ---------------------------------------------
  const els = {
    title: byId('pageTitle'),
    subtitle: byId('playerUrl'),
    badges: byId('playerBadges'),
    video: byId('player'),
    debugLog: byId('debugLog'),
    sessionControls: byId('sessionControls'),
    sessionGroupControls: byId('sessionGroupControls'),
    networkShapingBanner: byId('networkShapingBanner'),
    streamAccessBanner: byId('streamAccessBanner'),
    // Stats grid (one element per stat)
    statState: byId('statState'),
    statTime: byId('statTime'),
    statBufferedEnd: byId('statBufferedEnd'),
    statBufferDepth: byId('statBufferDepth'),
    statLiveOffset: byId('statLiveOffset'),
    statBitrate: byId('statBitrate'),
    statSseMissed: byId('statSseMissed'),
    statDropped: byId('statDropped'),
    statStalls: byId('statStalls'),
    // Action buttons
    retryPlayback: byId('retryPlayback'),
    restartPlayback: byId('restartPlayback'),
    reloadPage: byId('reloadPage'),
    playerSelect: byId('playerSelect'),
    prefer4kToggle: byId('prefer4kToggle'),
    autoRecoveryToggle: byId('autoRecoveryToggle'),
    videoPipToggle: byId('videoPipToggle'),
    playIdRotationGroup: byId('playIdRotationGroup'),
  };
  function byId(id) { return document.getElementById(id); }
  function t(s) { return document.createTextNode(String(s == null ? '' : s)); }

  // ---- Debug log helper --------------------------------------------
  // Bounded ring buffer of the last 200 lines so a long soak doesn't
  // pin a couple MB of DOM nodes.
  const LOG_MAX = 200;
  function log(...args) {
    if (!els.debugLog) return;
    const ts = new Date().toISOString().substring(11, 23);
    const line = ts + '  ' + args.map(a => typeof a === 'string' ? a : JSON.stringify(a)).join(' ');
    const node = document.createElement('div');
    node.appendChild(t(line));
    els.debugLog.appendChild(node);
    while (els.debugLog.childNodes.length > LOG_MAX) {
      els.debugLog.removeChild(els.debugLog.firstChild);
    }
    els.debugLog.scrollTop = els.debugLog.scrollHeight;
  }

  // ---- Connection pill in the badges area --------------------------
  // The header has a #playerBadges container. Install a connection pill
  // inside it that updates from PlayersStore SSE state.
  const connPill = document.createElement('span');
  connPill.className = 'panel-badge';
  connPill.style.cssText = 'background:#fee2e2;color:#7f1d1d;padding:2px 8px;border-radius:999px;font-size:11px;font-weight:600;';
  connPill.appendChild(t('disconnected'));
  if (els.badges) els.badges.appendChild(connPill);

  players.on('connection', (e) => {
    connPill.textContent = e.state;
    const palette = {
      connected:    { bg: '#d1fae5', fg: '#064e3b' },
      connecting:   { bg: '#fef3c7', fg: '#78350f' },
      reconnecting: { bg: '#fef3c7', fg: '#78350f' },
      disconnected: { bg: '#fee2e2', fg: '#7f1d1d' },
    }[e.state] || { bg: '#fee2e2', fg: '#7f1d1d' };
    connPill.style.background = palette.bg;
    connPill.style.color = palette.fg;
  });

  let sseMissed = 0;
  players.on('error', (e) => {
    if (e.operation === 'sse') {
      sseMissed++;
      if (els.statSseMissed) els.statSseMissed.textContent = String(sseMissed);
    }
    toasts.showError(e);
  });

  // ---- Player engine -----------------------------------------------
  // Engine state. Default = HLS.js since it works everywhere.
  // The selector ("Auto / HLS.js / Shaka / Video.js / Native") rebuilds
  // the player on change. Persisted to localStorage so the choice
  // sticks across reloads — same UX as v1.
  let hlsInstance = null;
  let shakaPlayer = null;
  let videojsPlayer = null;
  let currentPlayId = null;
  let playIdMintedAt = 0;
  let playIdLastActivityAt = 0;
  let lastStreamUrl = null;

  function generatePlayId() {
    const r = () => Math.floor(Math.random() * 0x10000).toString(16).padStart(4, '0');
    return [Date.now().toString(16), r(), r(), r(), r() + r() + r()].join('-');
  }
  function markPlayIdActivity() { playIdLastActivityAt = Date.now(); }

  function selectedEngine() {
    return localStorage.getItem(PLAYER_ENGINE_LS_KEY) || 'hlsjs';
  }
  function applyEngineSelector() {
    if (!els.playerSelect) return;
    els.playerSelect.value = selectedEngine();
    els.playerSelect.addEventListener('change', () => {
      localStorage.setItem(PLAYER_ENGINE_LS_KEY, els.playerSelect.value);
      log('ENGINE: switched to', els.playerSelect.value);
      reloadStream();
    });
  }

  function tearDownEngine() {
    if (hlsInstance) { try { hlsInstance.destroy(); } catch (_) {} hlsInstance = null; }
    if (shakaPlayer) { try { shakaPlayer.destroy(); } catch (_) {} shakaPlayer = null; }
    if (videojsPlayer) { try { videojsPlayer.dispose(); } catch (_) {} videojsPlayer = null; }
  }

  async function loadIntoEngine(url) {
    tearDownEngine();
    const engine = selectedEngine();
    const isHls = /\.m3u8(\?|$)/.test(url);
    const isDash = /\.mpd(\?|$)/.test(url);
    log('LOAD:', engine, '→', url);
    try {
      if (engine === 'auto' || engine === 'hlsjs') {
        if (isHls && window.Hls && window.Hls.isSupported()) {
          hlsInstance = new window.Hls({ lowLatencyMode: true });
          hlsInstance.loadSource(url);
          hlsInstance.attachMedia(els.video);
          return;
        }
      }
      if (engine === 'shaka' || (engine === 'auto' && isDash)) {
        if (window.shaka) {
          shakaPlayer = new window.shaka.Player(els.video);
          await shakaPlayer.load(url);
          return;
        }
      }
      if (engine === 'videojs') {
        if (window.videojs) {
          videojsPlayer = window.videojs(els.video, { autoplay: true, controls: true });
          videojsPlayer.src({ src: url, type: isHls ? 'application/x-mpegURL' : 'application/dash+xml' });
          return;
        }
      }
      // Native / fallback: let the browser handle it.
      els.video.src = url;
    } catch (err) {
      log('LOAD ERROR:', err && err.message);
      toasts.show('error', 'Player load failed', err && err.message);
    }
  }

  // streamUrlFor — derive a manifest URL for this player. Honours
  // ?manifest=… override; otherwise picks the first HLS-ready content
  // from /api/content. Per-session-port routing isn't surfaced in v2
  // yet (#TBD), so we hit /go-live/… directly for the scaffold.
  let cachedContent = null;
  async function streamUrlFor(player) {
    // Honour explicit overrides first. Both `?url=…` (v1 alias used by
    // hand-crafted bookmarks and the testing.html picker) and
    // `?manifest=…` (v2 explicit name) point at a fully-formed
    // manifest URL that the user picked. Append play_id only if not
    // already present so the URL stays user-controlled.
    const explicit = params.get('url') || params.get('manifest');
    if (explicit) {
      if (currentPlayId && explicit.indexOf('play_id=') < 0) {
        return explicit + (explicit.indexOf('?') < 0 ? '?' : '&') +
          'play_id=' + encodeURIComponent(currentPlayId);
      }
      return explicit;
    }
    if (!cachedContent) {
      try {
        const r = await fetch('/api/content');
        const list = await r.json();
        const ready = list.find(c => c.has_hls);
        cachedContent = ready ? ready.name : null;
      } catch (_) { /* leave null */ }
    }
    if (!cachedContent) return null;
    const offsetParam = currentPlayId ? '&play_id=' + encodeURIComponent(currentPlayId) : '';
    return '/go-live/' + cachedContent + '/master.m3u8'
      + '?player_id=' + encodeURIComponent(player.id) + offsetParam;
  }

  async function reloadStream() {
    if (!currentPlayer) return;
    if (!currentPlayId) currentPlayId = generatePlayId();
    playIdMintedAt = Date.now();
    const url = await streamUrlFor(currentPlayer);
    if (!url) {
      toasts.show('warn', 'No stream available', 'Upload content via /api/upload first, or pass ?manifest=… on the URL.');
      return;
    }
    lastStreamUrl = url;
    if (els.subtitle) els.subtitle.textContent = url;
    await loadIntoEngine(url);
  }

  function newPlayId(reason) {
    log('PLAY_ID: rotating', reason || '');
    currentPlayId = generatePlayId();
    reloadStream();
  }

  // ---- Player chrome wiring ----------------------------------------
  function wirePlayerControls() {
    applyEngineSelector();

    if (els.retryPlayback) {
      els.retryPlayback.addEventListener('click', () => {
        markPlayIdActivity();
        reloadStream();
      });
    }
    if (els.restartPlayback) {
      els.restartPlayback.addEventListener('click', () => {
        markPlayIdActivity();
        newPlayId('user_restart');
      });
    }
    if (els.reloadPage) {
      els.reloadPage.addEventListener('click', () => window.location.reload());
    }

    // Allow 4K toggle (localStorage; consumed by manifest variant filter
    // when content_allowed_variants is wired in Q5d).
    if (els.prefer4kToggle) {
      els.prefer4kToggle.checked = localStorage.getItem(NATIVE_4K_KEY) === '1';
      els.prefer4kToggle.addEventListener('change', () => {
        localStorage.setItem(NATIVE_4K_KEY, els.prefer4kToggle.checked ? '1' : '0');
        log('PREFER 4K:', els.prefer4kToggle.checked);
      });
    }

    // Auto-recovery (browser-side stall recovery; flips a flag the
    // stats updater consults on every tick).
    if (els.autoRecoveryToggle) {
      els.autoRecoveryToggle.checked = localStorage.getItem(AUTO_RECOVERY_KEY) === '1';
      els.autoRecoveryToggle.addEventListener('change', () => {
        localStorage.setItem(AUTO_RECOVERY_KEY, els.autoRecoveryToggle.checked ? '1' : '0');
        log('AUTO RECOVERY:', els.autoRecoveryToggle.checked);
      });
    }

    // Picture-in-Picture toggle. Uses the standard Document PiP API.
    if (els.videoPipToggle) {
      els.videoPipToggle.addEventListener('change', async () => {
        try {
          if (els.videoPipToggle.checked) {
            await els.video.requestPictureInPicture();
          } else if (document.pictureInPictureElement) {
            await document.exitPictureInPicture();
          }
        } catch (err) {
          els.videoPipToggle.checked = !els.videoPipToggle.checked;
          log('PIP ERROR:', err && err.message);
        }
      });
      els.video.addEventListener('leavepictureinpicture', () => {
        els.videoPipToggle.checked = false;
      });
    }

    // play_id rotation radio group (Off / 5m / 30m / 1h / 6h).
    if (els.playIdRotationGroup) {
      const cur = localStorage.getItem(PLAY_ID_ROTATION_LS_KEY) || '0';
      const radio = els.playIdRotationGroup.querySelector(
        'input[name="playIdRotation"][value="' + cur + '"]');
      if (radio) radio.checked = true;
      els.playIdRotationGroup.addEventListener('change', (ev) => {
        if (ev.target.name === 'playIdRotation') {
          localStorage.setItem(PLAY_ID_ROTATION_LS_KEY, ev.target.value);
          log('PLAY_ID rotation set to', ev.target.value, 's');
        }
      });
    }
  }

  // ---- Stats grid --------------------------------------------------
  // 2Hz tick: reads from the <video> element + Player.metrics. Updates
  // each stats-item value text. Fires the play_id rotation check.
  function statsTick() {
    if (!currentPlayer) return;
    const video = els.video;
    const m = currentPlayer.metrics;
    const sm = currentPlayer.serverMetrics;
    if (els.statState) els.statState.textContent = videoStateLabel(video);
    if (els.statTime) els.statTime.textContent = secondsLabel(video.currentTime);
    if (els.statBufferedEnd) els.statBufferedEnd.textContent = bufferedEnd(video);
    if (els.statBufferDepth) {
      const depth = m && m.bufferDepthS != null ? m.bufferDepthS : bufferDepthFromVideo(video);
      els.statBufferDepth.textContent = depth != null ? depth.toFixed(2) + ' s' : '—';
    }
    if (els.statLiveOffset) {
      const off = m && m.eventTime ? '—' : '—';
      els.statLiveOffset.textContent = off;
    }
    if (els.statBitrate) {
      const mbps = m && m.videoBitrateMbps;
      els.statBitrate.textContent = mbps != null ? mbps.toFixed(2) + ' Mbps' : '—';
    }
    if (els.statDropped) {
      const dropped = video.getVideoPlaybackQuality
        ? (video.getVideoPlaybackQuality().droppedVideoFrames || 0) : 0;
      els.statDropped.textContent = String(dropped);
    }
    if (els.statStalls) {
      els.statStalls.textContent = String(m && m.stalls != null ? m.stalls : 0);
    }
    checkPlayIdRotation();
  }

  function videoStateLabel(v) {
    if (!v) return 'Idle';
    if (v.ended) return 'Ended';
    if (v.paused) return 'Paused';
    if (v.seeking) return 'Seeking';
    if (v.readyState < 3) return 'Buffering';
    return 'Playing';
  }
  function secondsLabel(s) {
    if (s == null || !Number.isFinite(s)) return '--';
    const mm = Math.floor(s / 60);
    const ss = Math.floor(s % 60).toString().padStart(2, '0');
    return mm + ':' + ss;
  }
  function bufferedEnd(v) {
    if (!v || !v.buffered || v.buffered.length === 0) return '--';
    return v.buffered.end(v.buffered.length - 1).toFixed(2);
  }
  function bufferDepthFromVideo(v) {
    if (!v || !v.buffered || v.buffered.length === 0) return null;
    const end = v.buffered.end(v.buffered.length - 1);
    return Math.max(0, end - v.currentTime);
  }
  function checkPlayIdRotation() {
    const raw = localStorage.getItem(PLAY_ID_ROTATION_LS_KEY);
    const setting = raw ? parseInt(raw, 10) : 0;
    if (!setting || !playIdMintedAt || !lastStreamUrl) return;
    const ageMs = Date.now() - playIdMintedAt;
    if (ageMs < setting * 1000) return;
    const sinceActivityMs = playIdLastActivityAt
      ? (Date.now() - playIdLastActivityAt) : Infinity;
    if (sinceActivityMs < PLAY_ID_QUIESCENCE_MS) return;
    newPlayId('age_' + Math.round(ageMs / 1000) + 's');
  }

  // ---- Header / badges ---------------------------------------------
  function refreshHeader(player) {
    if (!player) return;
    document.title = 'Player #' + player.displayId + ' · Testing';
    if (els.title) els.title.textContent = 'Testing Playback #' + player.displayId;
    if (els.subtitle) els.subtitle.textContent = lastStreamUrl || player.id;
    if (!els.badges) return;
    // Rebuild badges (id badge, group badge if present, plus connPill we
    // appended once on boot).
    while (els.badges.firstChild) {
      if (els.badges.firstChild === connPill) break;
      els.badges.removeChild(els.badges.firstChild);
    }
    const idBadge = document.createElement('span');
    idBadge.className = 'panel-badge';
    idBadge.style.cssText = 'background:#e0e7ff;color:#1e3a8a;padding:2px 8px;border-radius:999px;font-size:11px;font-weight:600;margin-right:6px;';
    idBadge.appendChild(t('id ' + player.id.slice(0, 8)));
    els.badges.insertBefore(idBadge, connPill);
  }

  // resolvePlayer — accept exact UUID, prefix-match (the v1 8-char
  // short form like "15780e21" is the prefix of a full UUID v1
  // assigned), or display_id form like "#3".
  function resolvePlayer(idOrFragment) {
    if (!idOrFragment) return null;
    const exact = players.get(idOrFragment);
    if (exact) return exact;
    if (idOrFragment.startsWith('#')) {
      const n = parseInt(idOrFragment.slice(1), 10);
      return players.list().find(p => p.displayId === n) || null;
    }
    // Prefix match — case-insensitive. Lets v1-era short-form ids
    // round-trip to v2's deterministic UUIDv5.
    const prefix = idOrFragment.toLowerCase();
    const matches = players.list().filter(p => p.id.toLowerCase().startsWith(prefix));
    return matches.length === 1 ? matches[0] : null;
  }

  // ---- Resolve player + bootstrap ----------------------------------
  let currentPlayer = null;
  async function bootstrap() {
    wirePlayerControls();

    players.connect();
    await players.hydrate().catch(err => log('HYDRATE ERROR:', err && err.message));
    await groups.hydrate().catch(err => log('GROUPS HYDRATE ERROR:', err && err.message));

    if (playerIdParam) {
      currentPlayer = resolvePlayer(playerIdParam);
    }
    if (!currentPlayer) {
      // No matching player. List has nothing? Mint one. Otherwise
      // pick the freshest one; user came from the picker without an
      // explicit id.
      if (players.list().length === 0) {
        log('No players present — minting synthetic.');
        const r = await repo.createSyntheticPlayer({ synthetic: true });
        if (r.ok) {
          await players.hydrate();
          currentPlayer = players.get(r.body.id);
        }
      } else {
        currentPlayer = players.list()[0];
        log('player_id', playerIdParam || '(none)', 'not found; falling back to', currentPlayer.id);
      }
    }
    if (!currentPlayer) {
      if (els.subtitle) els.subtitle.textContent = 'No player available.';
      return;
    }
    log('PLAYER:', currentPlayer.id, '#' + currentPlayer.displayId);
    refreshHeader(currentPlayer);

    currentPlayer.on('change', () => refreshHeader(currentPlayer));

    // Mount all section render-functions. Each module below installs
    // its own subscriber on currentPlayer and re-renders on `change`.
    if (els.sessionControls && window.TestingSessionSections) {
      window.TestingSessionSections.mountAll(els.sessionControls, currentPlayer, { groups });
    }

    currentPlayId = generatePlayId();
    playIdMintedAt = Date.now();
    await reloadStream();
    setInterval(statsTick, 500);

    // Player video event hooks for activity tracking + log.
    ['play', 'pause', 'waiting', 'playing', 'ended', 'stalled', 'seeking'].forEach(evt => {
      els.video.addEventListener(evt, () => { markPlayIdActivity(); log('VIDEO:', evt); });
    });
    els.video.addEventListener('error', () => {
      const err = els.video.error;
      log('VIDEO ERROR:', err && err.message || 'unknown');
      toasts.show('error', 'Video error', err && err.message || 'unknown');
    });
  }

  bootstrap().catch(err => {
    log('BOOTSTRAP ERROR:', err && (err.stack || err.message));
    toasts.show('error', 'Bootstrap failed', err && err.message);
  });

  // Expose for Q5c-Q5j section modules to attach to.
  window.TestingSessionV2 = {
    repo,
    players,
    groups,
    toasts,
    currentPlayer: () => currentPlayer,
    log,
    streamUrlFor,
    reloadStream,
    newPlayId,
  };
})();
