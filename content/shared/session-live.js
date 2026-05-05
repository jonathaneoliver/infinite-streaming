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
            source.addEventListener('open', () => {
                // Server is talking again. Server-side
                // sessionsBroadcastSeq resets across container
                // restarts, so the previous revision number is
                // meaningless on the new connection — clear it so
                // we don't compute a bogus gap on the first
                // post-reconnect event.
                sseLastRevision = 0;
            });
            source.addEventListener('sessions', (event) => {
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

    function start() {
        return startSessionsStream();
    }

    window.SessionLive = { start };
})();
