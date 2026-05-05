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

        function parseSessionsStreamPayload(event) {
            const data = JSON.parse(event.data);
            if (Array.isArray(data)) {
                return { sessions: data, revision: Number(event.lastEventId || 0), dropped: 0, active_sessions: null };
            }
            const sessions = Array.isArray(data.sessions) ? data.sessions : [];
            const revision = Number(data.revision || event.lastEventId || 0);
            const dropped = Number(data.dropped || 0);
            const active_sessions = Array.isArray(data.active_sessions) ? data.active_sessions : null;
            return { sessions, revision, dropped, active_sessions };
        }


        function startSessionsStream() {
            if (!window.EventSource) return false;
            if (sseReconnectTimer !== null) {
                clearTimeout(sseReconnectTimer);
                sseReconnectTimer = null;
            }
            const source = new EventSource('/api/sessions/stream');
            sessionsStream = source;
            sseLastEventAt = Date.now();
            ensureSseWatchdog();
            source.addEventListener('open', () => {
                // Server is talking again. Server-side
                // sessionsBroadcastSeq resets across container
                // restarts, so the previous revision number is
                // meaningless on the new connection — clear it so
                // we don't compute a bogus gap on the first
                // post-reconnect event.
                sseLastRevision = 0;
                sseLastEventAt = Date.now();
            });
            source.addEventListener('heartbeat', () => {
                sseLastEventAt = Date.now();
            });
            source.addEventListener('sessions', (event) => {
                sseLastEventAt = Date.now();
                try {
                    const payload = parseSessionsStreamPayload(event);
                    const sessions = payload.sessions;
                    const revision = Number(payload.revision || 0);
                    const dropped = Number(payload.dropped || 0);
                    let missed = 0;
                    if (Number.isFinite(revision) && revision > 0 && sseLastRevision > 0) {
                        const gap = revision - sseLastRevision - 1;
                        if (gap > 0) {
                            missed += gap;
                        }
                    }
                    if (Number.isFinite(dropped) && dropped > 0) {
                        missed += dropped;
                    }
                    if (missed > 0) {
                        sseMissedTotal += missed;
                        updateSseMissedBadge();
                        console.warn(`SSE missed ${missed} update${missed === 1 ? '' : 's'}`);
                    }
                    if (Number.isFinite(revision) && revision > 0) {
                        sseLastRevision = revision;
                    }
                    applySessionsList(sessions);
                } catch (err) {
                    console.error('Error parsing sessions stream payload', err);
                }
            });
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
