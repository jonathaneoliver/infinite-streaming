// Live-SSE entry path for testing.html. Pairs with session-shell.js
// (must be loaded first); reads applySessionsList from
// window.SessionShell. The Session Viewer pages do not load this
// module — the chart engine and replay loader handle their own
// session-list ingestion. Public API: window.SessionLive.start()
// returns true if EventSource attached, false to fall back to polling.
(function () {
    'use strict';
    if (!window.SessionShell) {
        console.error('session-live.js: SessionShell not loaded');
        return;
    }
    const { applySessionsList } = window.SessionShell;

    const sseMissedBadge = document.getElementById('sseMissedBadge');
    let sessionsStream = null;
    let sseLastRevision = 0;
    let sseMissedTotal = 0;
    // Reconnection state — EventSource's built-in auto-retry covers
    // most transient errors, but in some failure modes (notably
    // server redeploy when the container is killed mid-response) the
    // browser sets readyState=CLOSED and gives up. Without an
    // explicit retry the dashboard goes silent until the user
    // hits refresh. Retry with a short fixed backoff; reset on
    // successful (re)connect.
    let sseReconnectTimer = null;
    const SSE_RETRY_MS = 2000;
    // Watchdog: server sends a `heartbeat` event every 5 s. If we
    // don't see ANY event (heartbeat or sessions) within 12 s, the
    // connection is dead in some way the EventSource state machine
    // hasn't surfaced (e.g. browser stuck in CONNECTING with silent
    // retry failures). Force-close + reopen. 12 s allows for one
    // missed heartbeat plus ~2 s of network jitter.
    let sseLastEventAt = Date.now();
    let sseWatchdogTimer = null;
    const SSE_WATCHDOG_MS = 12000;

        function updateSseMissedBadge() {
            if (!sseMissedBadge) return;
            if (sseMissedTotal > 0) {
                sseMissedBadge.textContent = `Missed updates: ${sseMissedTotal}`;
            } else {
                sseMissedBadge.textContent = '';
            }
        }

        // Migration step #5 (issue #441): bypass the v2-compat
        // EventSource shim. Subscribe to /api/v2/events?include=raw
        // directly and maintain the player_id → SessionData map
        // inline. On every player.created/updated/deleted/heartbeat,
        // call applySessionsList() with the same v1-array projection
        // the shim used to dispatch via a synthetic 'sessions' event.
        // sessionsByID survives across reconnects so the dashboard
        // never sees a "no sessions" flash on transient disconnect.
        const sessionsByID = new Map(); // player_id → raw v1 SessionData

        function emitSessions(reason) {
            const arr = Array.from(sessionsByID.values());
            try {
                applySessionsList(arr);
            } catch (err) {
                console.error('applySessionsList failed (' + (reason || '?') + ')', err);
            }
        }

        function startSessionsStream() {
            if (!window.EventSource) return false;
            if (sseReconnectTimer !== null) {
                clearTimeout(sseReconnectTimer);
                sseReconnectTimer = null;
            }
            const source = new EventSource('/api/v2/events?include=raw');
            sessionsStream = source;
            sseLastEventAt = Date.now();
            ensureSseWatchdog();

            // Bootstrap immediately — the v2 events stream's first
            // server-emitted frame can be 15 s out (heartbeat cadence)
            // and the watchdog fires at 12 s. Pull the current snapshot
            // up front so the dashboard renders within hundreds of ms
            // independent of when the SSE connection settles.
            fetch('/api/v2/players?include=raw', { cache: 'no-store' })
                .then(r => r.ok ? r.json() : null)
                .then(body => {
                    if (!body || !Array.isArray(body.items)) return;
                    sessionsByID.clear();
                    for (const p of body.items) {
                        sessionsByID.set(p.id, p.raw_session || {});
                    }
                    emitSessions('bootstrap');
                })
                .catch(err => console.error('SSE bootstrap fetch failed', err));

            source.addEventListener('open', () => {
                // Server is talking again. The v2 stream emits a
                // monotonic event id per frame; we don't track it for
                // gap detection here because the shim never did
                // either — heartbeats re-emit the full snapshot, so
                // any missed delta is repaired within the heartbeat
                // cadence (5 s server-side).
                sseLastRevision = 0;
                sseLastEventAt = Date.now();
            });
            source.addEventListener('heartbeat', () => {
                sseLastEventAt = Date.now();
                // Re-emit the current snapshot so the chart engines
                // (which derive everything from applySessionsList)
                // keep their rolling-window cursors fresh even when
                // no player.updated arrived this cycle.
                emitSessions('heartbeat');
            });

            function handlePlayerEvent(rawEvent, kind) {
                sseLastEventAt = Date.now();
                try {
                    const env = JSON.parse(rawEvent.data);
                    const data = env.data || {};
                    if (kind === 'deleted') {
                        const pid = data.player_id || data.id;
                        if (pid && sessionsByID.delete(pid)) emitSessions('deleted');
                        return;
                    }
                    if (!data.id) return;
                    const sess = data.raw_session || data;
                    sessionsByID.set(data.id, sess);
                    emitSessions(kind);
                } catch (err) {
                    console.error('SSE player event parse failed', err);
                }
            }
            source.addEventListener('player.created', (ev) => handlePlayerEvent(ev, 'created'));
            source.addEventListener('player.updated', (ev) => handlePlayerEvent(ev, 'updated'));
            source.addEventListener('player.deleted', (ev) => handlePlayerEvent(ev, 'deleted'));

            source.addEventListener('error', () => {
                // CONNECTING state means the browser is auto-retrying
                // already — leave it alone. CLOSED means the browser
                // gave up; explicitly retry so a server redeploy
                // doesn't strand the dashboard until manual refresh.
                if (source.readyState !== EventSource.CLOSED) return;
                source.close();
                sessionsStream = null;
                if (sseReconnectTimer !== null) return;
                sseReconnectTimer = setTimeout(() => {
                    sseReconnectTimer = null;
                    startSessionsStream();
                }, SSE_RETRY_MS);
            });
            return true;
        }

    function ensureSseWatchdog() {
        if (sseWatchdogTimer !== null) return;
        // Tick at half the budget so worst-case detection is one
        // tick + the budget itself.
        sseWatchdogTimer = setInterval(() => {
            if (Date.now() - sseLastEventAt < SSE_WATCHDOG_MS) return;
            console.warn(`SSE silent for ${SSE_WATCHDOG_MS}ms — forcing reconnect`);
            if (sessionsStream) {
                try { sessionsStream.close(); } catch (_) { /* ignore */ }
                sessionsStream = null;
            }
            sseLastEventAt = Date.now();
            startSessionsStream();
        }, 2000);
        // Browsers throttle setInterval in background tabs (Chrome
        // clamps to ≥1Hz, can be much slower under load). When the
        // tab becomes visible again, immediately check liveness and
        // force-reconnect if we missed events while throttled.
        document.addEventListener('visibilitychange', () => {
            if (document.visibilityState !== 'visible') return;
            if (Date.now() - sseLastEventAt < SSE_WATCHDOG_MS) return;
            console.warn('SSE silent while tab was hidden — forcing reconnect on visibilitychange');
            if (sessionsStream) {
                try { sessionsStream.close(); } catch (_) { /* ignore */ }
                sessionsStream = null;
            }
            sseLastEventAt = Date.now();
            startSessionsStream();
        });
    }

    function start() {
        return startSessionsStream();
    }

    window.SessionLive = { start };
})();
