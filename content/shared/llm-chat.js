// Shared SSE consumer + state for the AI session-analysis path
// (epic #412, issue #419).
//
// Used by:
//   - dashboard/llm-discuss-panel.js for the multi-turn Discuss panel
//   - dashboard/llm-analyze-modal.js for the per-row Analyze button
//
// DOM-free by design so each consumer wires its own UI. The module
// surfaces an EventTarget-like API: subscribe to `event:tool_call`,
// `event:tool_result`, `event:assistant_message`, `event:usage`,
// `event:error`, `event:done`. Plus a `cancel()` to abort mid-turn.

(function () {
    'use strict';

    const ANALYTICS_BASE = '/analytics/api';

    function makeEmitter() {
        const map = new Map();
        return {
            on(name, fn) {
                if (!map.has(name)) map.set(name, new Set());
                map.get(name).add(fn);
                return () => map.get(name)?.delete(fn);
            },
            emit(name, payload) {
                map.get(name)?.forEach((fn) => {
                    try { fn(payload); }
                    catch (err) { console.error(`llm-chat: handler for ${name} threw`, err); }
                });
            },
        };
    }

    // parseSSEStream consumes a ReadableStream<Uint8Array> emitting
    // server-sent events. Yields {event, data} objects via callback.
    // Tolerant of split events across chunk boundaries.
    async function parseSSEStream(stream, onEvent, signal) {
        const reader = stream.getReader();
        const decoder = new TextDecoder();
        let buf = '';
        let cur = { event: '', data: '' };
        try {
            while (true) {
                if (signal?.aborted) return;
                const { value, done } = await reader.read();
                if (done) return;
                buf += decoder.decode(value, { stream: true });
                let nl;
                while ((nl = buf.indexOf('\n')) >= 0) {
                    const line = buf.slice(0, nl).replace(/\r$/, '');
                    buf = buf.slice(nl + 1);
                    if (line === '') {
                        // Blank line terminates one event.
                        if (cur.event || cur.data) {
                            let parsed = null;
                            if (cur.data) {
                                try { parsed = JSON.parse(cur.data); }
                                catch { parsed = { raw: cur.data }; }
                            }
                            onEvent(cur.event || 'message', parsed);
                        }
                        cur = { event: '', data: '' };
                    } else if (line.startsWith(':')) {
                        // SSE comment (keepalive). Ignore.
                    } else {
                        // Per W3C EventSource: `field:value` with the
                        // colon optionally followed by a single space.
                        // Tolerate both `event: foo` and `event:foo`.
                        const idx = line.indexOf(':');
                        if (idx <= 0) continue;
                        const field = line.slice(0, idx);
                        let value = line.slice(idx + 1);
                        if (value.startsWith(' ')) value = value.slice(1);
                        if (field === 'event') {
                            cur.event = value;
                        } else if (field === 'data') {
                            cur.data = (cur.data ? cur.data + '\n' : '') + value;
                        }
                        // id: / retry: — ignored for now.
                    }
                }
            }
        } finally {
            try { reader.releaseLock(); } catch { /* already released */ }
        }
    }

    // chat opens an SSE session against /api/session_chat. Returns
    // an emitter + cancel(). Caller subscribes to events before
    // awaiting completion via the `done` event.
    function chat(opts) {
        const {
            profile = '',
            messages,
            sessionId = '',
            sessions = null,
            range = null,
            oneShot = false,
        } = opts || {};
        if (!Array.isArray(messages) || messages.length === 0) {
            throw new Error('llm-chat: messages array required');
        }
        const emitter = makeEmitter();
        const ctrl = new AbortController();
        const body = JSON.stringify({
            profile,
            messages,
            session_id: sessionId,
            sessions,
            range,
            one_shot: oneShot,
        });

        (async () => {
            try {
                const resp = await fetch(`${ANALYTICS_BASE}/session_chat`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body,
                    signal: ctrl.signal,
                });
                if (!resp.ok) {
                    let detail = '';
                    try { detail = await resp.text(); } catch { /* ignore */ }
                    if (resp.status === 429) {
                        emitter.emit('error', {
                            kind: 'budget_exceeded',
                            retryAfterSec: parseInt(resp.headers.get('Retry-After') || '0', 10),
                            message: detail || 'daily budget exhausted',
                        });
                    } else if (resp.status === 413) {
                        emitter.emit('error', { kind: 'input_too_large', message: detail });
                    } else {
                        emitter.emit('error', { kind: 'http', status: resp.status, message: detail });
                    }
                    emitter.emit('done', null);
                    return;
                }
                if (!resp.body) {
                    emitter.emit('error', { kind: 'no_stream', message: 'response had no body' });
                    emitter.emit('done', null);
                    return;
                }
                await parseSSEStream(resp.body, (event, data) => {
                    emitter.emit(event, data);
                }, ctrl.signal);
            } catch (err) {
                if (err && err.name === 'AbortError') {
                    emitter.emit('cancelled', null);
                    emitter.emit('done', null);
                    return;
                }
                emitter.emit('error', { kind: 'fetch', message: String(err) });
                emitter.emit('done', null);
            }
        })();

        return {
            on: emitter.on,
            cancel: () => ctrl.abort(),
        };
    }

    async function loadProfiles() {
        const resp = await fetch(`${ANALYTICS_BASE}/llm_profiles`);
        if (!resp.ok) throw new Error(`llm_profiles: ${resp.status}`);
        return resp.json();
    }

    async function loadBudget() {
        const resp = await fetch(`${ANALYTICS_BASE}/llm_budget`);
        if (!resp.ok) throw new Error(`llm_budget: ${resp.status}`);
        return resp.json();
    }

    // Format mm:ss.ms from ms / s / number — for citation parsing.
    // Used when rendering the assistant_message body so timestamps
    // become click-to-seek anchors.
    function formatTimestamp(s) {
        if (typeof s === 'string') {
            const f = parseFloat(s);
            if (Number.isFinite(f)) s = f;
        }
        if (typeof s !== 'number' || !Number.isFinite(s)) return '';
        const mm = Math.floor(s / 60);
        const ss = (s - mm * 60).toFixed(3).padStart(6, '0');
        return `${mm}:${ss}`;
    }

    // Linkify timestamps in plain text. Returns an HTML string with
    // <a class="llmchat-ts" data-ts="0:42.350">0:42.350</a> spans. The
    // page-level glue listens for clicks on `.llmchat-ts` and routes to
    // the chart-cursor seek.
    function linkifyTimestamps(text) {
        const escaped = text.replace(/[&<>"']/g, (c) => ({
            '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
        }[c]));
        return escaped.replace(
            /\b(\d{1,3}:\d{2}(?:\.\d{1,3})?)\b/g,
            '<a class="llmchat-ts" data-ts="$1" href="javascript:void(0)">$1</a>'
        );
    }

    // Parse mm:ss.ms (possibly "1:23" or "1:23.456") to seconds.
    function parseTimestamp(s) {
        if (typeof s !== 'string') return NaN;
        const m = s.match(/^(\d{1,3}):(\d{2})(?:\.(\d{1,3}))?$/);
        if (!m) return NaN;
        const minutes = parseInt(m[1], 10);
        const seconds = parseInt(m[2], 10);
        const ms = m[3] ? parseInt(m[3].padEnd(3, '0'), 10) : 0;
        return minutes * 60 + seconds + ms / 1000;
    }

    window.LLMChat = {
        chat,
        loadProfiles,
        loadBudget,
        formatTimestamp,
        linkifyTimestamps,
        parseTimestamp,
    };
})();
