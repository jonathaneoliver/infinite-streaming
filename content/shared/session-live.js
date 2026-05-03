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
            const source = new EventSource('/api/sessions/stream');
            sessionsStream = source;
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
                if (source.readyState === EventSource.CLOSED) {
                    source.close();
                    sessionsStream = null;
                    startSessionsPolling();
                }
            });
            return true;
        }

    function start() {
        return startSessionsStream();
    }

    window.SessionLive = { start };
})();
