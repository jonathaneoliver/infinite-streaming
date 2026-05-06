// Discuss panel for session-viewer.html (epic #412, issue #419).
//
// Side panel that lets the user chat with the LLM about the
// currently-loaded session. Conversation history is persisted in
// localStorage keyed by session_id; closing/reopening a session
// preserves the chat. Multi-turn — each new message reuses the
// full prior history. Cancel button stops a stream mid-flight.

(function () {
    'use strict';

    if (!window.LLMChat) {
        console.warn('llm-discuss-panel: LLMChat not loaded; panel disabled');
        return;
    }

    const STORAGE_PREFIX = 'llm-chat-history:';
    const STYLE_ID = 'llm-discuss-styles';

    function injectStyles() {
        if (document.getElementById(STYLE_ID)) return;
        const css = `
.llm-discuss-toggle {
    position: fixed;
    right: 16px;
    bottom: 16px;
    width: 56px;
    height: 56px;
    border-radius: 50%;
    background: #1e1b4b;
    color: #fff;
    border: none;
    box-shadow: 0 6px 18px rgba(30, 27, 75, 0.35);
    cursor: pointer;
    font-size: 22px;
    z-index: 9000;
    display: flex;
    align-items: center;
    justify-content: center;
    transition: transform 0.15s ease;
}
.llm-discuss-toggle:hover { transform: translateY(-2px); }
.llm-discuss-toggle.active { background: #4f46e5; }
.llm-discuss-toggle[disabled] { opacity: 0.5; cursor: not-allowed; }

.llm-discuss-panel {
    position: fixed;
    right: 0;
    top: 0;
    bottom: 0;
    width: min(440px, 100vw);
    background: #fff;
    border-left: 1px solid #c7d2fe;
    box-shadow: -8px 0 24px rgba(15, 23, 42, 0.12);
    z-index: 9001;
    display: flex;
    flex-direction: column;
    transform: translateX(100%);
    transition: transform 0.18s ease;
}
.llm-discuss-panel.open { transform: translateX(0); }

.llm-discuss-header {
    padding: 14px 16px;
    border-bottom: 1px solid #e5e7eb;
    display: flex;
    align-items: center;
    gap: 10px;
}
.llm-discuss-header h3 { margin: 0; font-size: 15px; font-weight: 600; flex: 1; }
.llm-discuss-close { background: none; border: none; cursor: pointer; font-size: 20px; color: #64748b; padding: 4px 8px; }
.llm-discuss-close:hover { color: #0f172a; }

.llm-discuss-controls {
    padding: 10px 16px;
    border-bottom: 1px solid #f1f5f9;
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    color: #475569;
}
.llm-discuss-controls select {
    flex: 1;
    padding: 4px 8px;
    border: 1px solid #cbd5e1;
    border-radius: 6px;
    font-size: 12px;
}
.llm-discuss-clear { background: none; border: 1px solid #cbd5e1; border-radius: 6px; padding: 4px 8px; cursor: pointer; font-size: 11px; color: #475569; }
.llm-discuss-clear:hover { background: #f1f5f9; }

.llm-discuss-budget {
    padding: 6px 16px;
    font-size: 11px;
    color: #64748b;
    border-bottom: 1px solid #f1f5f9;
    background: #f8fafc;
}
.llm-discuss-budget.over { background: #fef2f2; color: #991b1b; }

.llm-discuss-focus {
    padding: 6px 16px;
    font-size: 11px;
    color: #312e81;
    border-bottom: 1px solid #f1f5f9;
    background: #eef2ff;
    display: none;
}
.llm-discuss-focus.active { display: block; }
.llm-discuss-focus button {
    margin-left: 8px;
    background: none;
    border: 1px solid #c7d2fe;
    border-radius: 4px;
    padding: 2px 6px;
    cursor: pointer;
    font-size: 10px;
    color: #312e81;
}

.llm-discuss-history {
    flex: 1;
    overflow-y: auto;
    padding: 12px 16px;
    background: #fafbff;
}
.llmchat-msg { margin-bottom: 14px; line-height: 1.45; font-size: 13px; }
.llmchat-msg-user { background: #eef2ff; border-radius: 10px; padding: 8px 12px; }
.llmchat-msg-user .llmchat-msg-role { color: #3730a3; font-weight: 600; font-size: 11px; }
.llmchat-msg-assistant { padding: 0 4px; color: #0f172a; }
.llmchat-msg-assistant .llmchat-msg-role { color: #475569; font-weight: 600; font-size: 11px; }
.llmchat-msg-tool { font-size: 11px; color: #64748b; padding: 4px 8px; background: #f1f5f9; border-radius: 6px; font-family: ui-monospace, monospace; }
.llmchat-msg-error { background: #fef2f2; color: #991b1b; padding: 8px 12px; border-radius: 8px; font-size: 12px; }
.llmchat-msg-meta { font-size: 11px; color: #94a3b8; margin-top: 4px; }
.llmchat-ts { color: #4f46e5; text-decoration: underline dotted; cursor: pointer; }
.llmchat-ts:hover { color: #312e81; }

.llm-discuss-input {
    padding: 12px 16px;
    border-top: 1px solid #e5e7eb;
    display: flex;
    flex-direction: column;
    gap: 8px;
}
.llm-discuss-input textarea {
    width: 100%;
    min-height: 60px;
    max-height: 160px;
    padding: 8px 10px;
    border: 1px solid #cbd5e1;
    border-radius: 8px;
    font-family: inherit;
    font-size: 13px;
    resize: vertical;
}
.llm-discuss-actions { display: flex; gap: 8px; align-items: center; }
.llm-discuss-send {
    flex: 1;
    background: #4f46e5;
    color: #fff;
    border: none;
    padding: 8px 14px;
    border-radius: 8px;
    cursor: pointer;
    font-size: 13px;
    font-weight: 600;
}
.llm-discuss-send:hover { background: #4338ca; }
.llm-discuss-send[disabled] { background: #94a3b8; cursor: not-allowed; }
.llm-discuss-cancel {
    background: #fef2f2;
    color: #991b1b;
    border: 1px solid #fecaca;
    padding: 8px 12px;
    border-radius: 8px;
    cursor: pointer;
    font-size: 12px;
}
.llm-discuss-cancel[disabled] { opacity: 0.5; cursor: not-allowed; }
        `.trim();
        const style = document.createElement('style');
        style.id = STYLE_ID;
        style.textContent = css;
        document.head.appendChild(style);
    }

    function buildPanel() {
        const panel = document.createElement('aside');
        panel.className = 'llm-discuss-panel';
        panel.setAttribute('role', 'dialog');
        panel.setAttribute('aria-label', 'AI discussion panel');
        panel.innerHTML = `
            <div class="llm-discuss-header">
                <h3>Discuss this session</h3>
                <button class="llm-discuss-close" type="button" aria-label="Close panel">×</button>
            </div>
            <div class="llm-discuss-controls">
                <label style="font-size:11px;">Model:
                    <select class="llm-discuss-select" data-role="profile" aria-label="LLM profile"></select>
                </label>
                <button class="llm-discuss-clear" type="button">Clear</button>
            </div>
            <div class="llm-discuss-budget" data-role="budget">Loading budget…</div>
            <div class="llm-discuss-focus" data-role="focus"></div>
            <div class="llm-discuss-history" data-role="history" aria-live="polite"></div>
            <div class="llm-discuss-input">
                <textarea data-role="input" placeholder="Ask about this session — e.g. 'what happened around the stall at 0:42?'" aria-label="Message"></textarea>
                <div class="llm-discuss-actions">
                    <button class="llm-discuss-send" data-role="send" type="button" disabled>Loading models…</button>
                    <button class="llm-discuss-cancel" data-role="cancel" type="button" disabled>Cancel</button>
                </div>
            </div>
        `;
        return panel;
    }

    function buildToggle() {
        const btn = document.createElement('button');
        btn.className = 'llm-discuss-toggle';
        btn.type = 'button';
        btn.title = 'Discuss this session with AI';
        btn.textContent = '💬';
        return btn;
    }

    // Persistence: chat history per session_id in localStorage. Keep
    // it small — last 50 messages per session, drop the rest.
    function loadHistory(sessionId) {
        if (!sessionId) return [];
        try {
            const raw = localStorage.getItem(STORAGE_PREFIX + sessionId);
            if (!raw) return [];
            const arr = JSON.parse(raw);
            return Array.isArray(arr) ? arr : [];
        } catch { return []; }
    }
    function saveHistory(sessionId, history) {
        if (!sessionId) return;
        try {
            const trimmed = history.slice(-50);
            localStorage.setItem(STORAGE_PREFIX + sessionId, JSON.stringify(trimmed));
        } catch { /* quota or disabled — ignore */ }
    }
    function clearHistory(sessionId) {
        if (!sessionId) return;
        try { localStorage.removeItem(STORAGE_PREFIX + sessionId); } catch { /* ignore */ }
    }

    function getSessionIdFromURL() {
        const params = new URLSearchParams(window.location.search);
        return params.get('session') || params.get('session_id') || '';
    }

    function renderHistory(historyEl, history) {
        historyEl.innerHTML = '';
        for (const msg of history) {
            historyEl.appendChild(renderMessage(msg));
        }
        historyEl.scrollTop = historyEl.scrollHeight;
    }

    function renderMessage(msg) {
        const div = document.createElement('div');
        div.className = `llmchat-msg llmchat-msg-${msg.role}`;
        if (msg.role === 'user') {
            div.innerHTML = `<div class="llmchat-msg-role">You</div>${escapeHTML(msg.content)}`;
        } else if (msg.role === 'assistant') {
            const meta = msg.meta ? `<div class="llmchat-msg-meta">${escapeHTML(msg.meta)}</div>` : '';
            div.innerHTML = `<div class="llmchat-msg-role">AI</div>${window.LLMChat.linkifyTimestamps(msg.content)}${meta}`;
        } else if (msg.role === 'tool') {
            div.innerHTML = `<div>${escapeHTML(msg.content)}</div>`;
        } else if (msg.role === 'error') {
            div.innerHTML = `<div>${escapeHTML(msg.content)}</div>`;
        }
        return div;
    }

    function escapeHTML(s) {
        return String(s).replace(/[&<>"']/g, (c) => ({
            '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
        }[c]));
    }

    async function populateProfileSelect(selectEl) {
        const data = await window.LLMChat.loadProfiles().catch(() => null);
        selectEl.innerHTML = '';
        if (!data || !data.enabled) {
            const opt = document.createElement('option');
            opt.textContent = '(AI features disabled on this server)';
            opt.disabled = true;
            selectEl.appendChild(opt);
            selectEl.disabled = true;
            return null;
        }
        const profiles = data.profiles || [];
        const stored = localStorage.getItem('llm-chat-profile') || '';
        let picked = '';
        for (const p of profiles) {
            const opt = document.createElement('option');
            opt.value = p.name;
            opt.textContent = `${p.name}${p.available ? '' : ' (no key)'}`;
            opt.disabled = !p.available;
            selectEl.appendChild(opt);
            if (!picked && p.available) picked = p.name;
            if (stored && p.name === stored && p.available) picked = stored;
            if (!stored && p.active && p.available) picked = p.name;
        }
        selectEl.value = picked;
        return picked;
    }

    async function refreshBudget(budgetEl) {
        const b = await window.LLMChat.loadBudget().catch(() => null);
        if (!b) {
            budgetEl.textContent = 'Budget unavailable';
            return;
        }
        const spent = (b.spent_usd || 0).toFixed(4);
        const cap = (b.cap_usd || 0).toFixed(2);
        const calls = b.calls_today || 0;
        const over = (b.spent_usd || 0) >= (b.cap_usd || 0);
        budgetEl.textContent = `$${spent} of $${cap} used today · ${calls} calls`;
        budgetEl.classList.toggle('over', over);
    }

    function init() {
        injectStyles();
        const toggle = buildToggle();
        const panel = buildPanel();
        document.body.appendChild(toggle);
        document.body.appendChild(panel);

        const historyEl = panel.querySelector('[data-role=history]');
        const inputEl = panel.querySelector('[data-role=input]');
        const sendBtn = panel.querySelector('[data-role=send]');
        const cancelBtn = panel.querySelector('[data-role=cancel]');
        const closeBtn = panel.querySelector('.llm-discuss-close');
        const clearBtn = panel.querySelector('.llm-discuss-clear');
        const profileSelect = panel.querySelector('[data-role=profile]');
        const budgetEl = panel.querySelector('[data-role=budget]');
        const focusEl = panel.querySelector('[data-role=focus]');

        let sessionId = getSessionIdFromURL();
        let history = loadHistory(sessionId);
        let activeStream = null;
        let budgetTimer = null;
        let profilesReady = false;
        // Brush range tracked from session-replay.js's
        // 'replay:brush-changed' CustomEvent. null = no brush
        // (overview mode); {fromMs, toMs, sessionStartMs} = forensic
        // mode — the next chat message ships {range:{from,to}} so
        // the LLM scopes queries to the brushed window.
        let brushRange = null;
        let brushOverride = null; // user-suppressed brush focus

        function fmtMs(ms) {
            if (!Number.isFinite(ms) || ms < 0) return '—';
            const totalS = Math.floor(ms / 1000);
            const m = Math.floor(totalS / 60);
            const s = totalS - m * 60;
            return `${m}:${s.toString().padStart(2, '0')}`;
        }
        function effectiveRange() {
            if (brushOverride === 'off') return null;
            return brushRange;
        }
        function refreshFocus() {
            const r = effectiveRange();
            if (!r) {
                focusEl.classList.remove('active');
                focusEl.textContent = '';
                return;
            }
            const startMs = r.sessionStartMs || 0;
            const span = r.toMs - r.fromMs;
            focusEl.classList.add('active');
            focusEl.innerHTML = `Focus: <strong>${escapeHTML(fmtMs(r.fromMs - startMs))}–${escapeHTML(fmtMs(r.toMs - startMs))}</strong> (${escapeHTML(fmtMs(span))} window) <button data-role="focus-clear" type="button">Use full session</button>`;
            focusEl.querySelector('[data-role=focus-clear]').addEventListener('click', () => {
                brushOverride = 'off';
                refreshFocus();
            });
        }

        document.addEventListener('replay:brush-changed', (e) => {
            const detail = e.detail || {};
            if (detail.sessionId && sessionId && detail.sessionId !== sessionId) return;
            brushRange = (detail.fromMs && detail.toMs && detail.toMs > detail.fromMs)
                ? { fromMs: detail.fromMs, toMs: detail.toMs, sessionStartMs: detail.sessionStartMs || 0 }
                : null;
            brushOverride = null; // any new brush activity re-enables focus
            refreshFocus();
        });

        renderHistory(historyEl, history);

        // Document-level Esc handler is bound only while the panel
        // is open; close() unbinds it. Avoids the modal-leak issue
        // the reviewer flagged for the other surface.
        const escHandler = (e) => {
            if (e.key === 'Escape' && !activeStream) close();
        };

        async function open() {
            panel.classList.add('open');
            toggle.classList.add('active');
            document.addEventListener('keydown', escHandler);
            // Refresh budget when panel opens; poll every 30s while open.
            refreshBudget(budgetEl);
            budgetTimer = setInterval(() => refreshBudget(budgetEl), 30000);
            // Re-load profile list in case server config changed.
            // Sequential: must finish before sendMessage can fire so
            // the user doesn't submit with an empty profile.
            await populateProfileSelect(profileSelect);
            profilesReady = true;
            sendBtn.textContent = 'Send';
            if (!activeStream) sendBtn.disabled = false;
            inputEl.focus();
        }
        function close() {
            panel.classList.remove('open');
            toggle.classList.remove('active');
            document.removeEventListener('keydown', escHandler);
            if (budgetTimer) {
                clearInterval(budgetTimer);
                budgetTimer = null;
            }
            if (activeStream) activeStream.cancel();
            // Return focus to the toggle for keyboard users (a11y).
            try { toggle.focus(); } catch { /* ignore */ }
        }

        toggle.addEventListener('click', () => {
            if (panel.classList.contains('open')) close(); else open();
        });
        closeBtn.addEventListener('click', close);
        clearBtn.addEventListener('click', () => {
            clearHistory(sessionId);
            history = [];
            renderHistory(historyEl, history);
        });
        profileSelect.addEventListener('change', () => {
            try { localStorage.setItem('llm-chat-profile', profileSelect.value); } catch { /* ignore */ }
        });

        function setStreaming(on) {
            sendBtn.disabled = on || !profilesReady;
            cancelBtn.disabled = !on;
            // readOnly instead of disabled so the user can still
            // select / copy text from their own pending message.
            inputEl.readOnly = on;
        }

        function sendMessage() {
            if (!profilesReady || !profileSelect.value) return;
            const content = inputEl.value.trim();
            if (!content) return;
            sessionId = sessionId || getSessionIdFromURL();
            const userMsg = { role: 'user', content };
            history.push(userMsg);
            renderHistory(historyEl, history);
            inputEl.value = '';
            setStreaming(true);

            // Build the wire-format message list — only user/assistant/tool roles,
            // never echo a system message back (the forwarder always re-injects).
            const wireMessages = history
                .filter((m) => m.role === 'user' || m.role === 'assistant')
                .map((m) => ({ role: m.role, content: m.content }));

            const partial = { role: 'assistant', content: '', meta: '' };
            history.push(partial);

            const focusRange = effectiveRange();
            const stream = window.LLMChat.chat({
                profile: profileSelect.value,
                messages: wireMessages,
                sessionId,
                range: focusRange ? { from: focusRange.fromMs, to: focusRange.toMs } : null,
            });
            activeStream = stream;

            stream.on('tool_call', (data) => {
                partial.meta = `running ${data?.name || 'tool'}…`;
                renderHistory(historyEl, history);
            });
            stream.on('tool_result', (data) => {
                const rows = data?.rows ?? 0;
                const ms = data?.elapsed_ms ?? 0;
                const trunc = data?.truncated ? ' (truncated)' : '';
                partial.meta = `tool returned ${rows} rows in ${ms} ms${trunc}`;
                renderHistory(historyEl, history);
            });
            stream.on('assistant_message', (data) => {
                partial.content = data?.content || '';
                renderHistory(historyEl, history);
            });
            stream.on('usage', (data) => {
                const cost = (data?.cost_usd || 0).toFixed(5);
                const tokens = (data?.input_tokens || 0) + (data?.output_tokens || 0);
                partial.meta = `$${cost} · ${tokens} tokens · ${data?.tool_calls_count || 0} tool calls · ${data?.iterations || 0} iters`;
                renderHistory(historyEl, history);
            });
            stream.on('error', (data) => {
                history.pop(); // drop the empty assistant placeholder
                history.push({
                    role: 'error',
                    content: data?.kind === 'budget_exceeded'
                        ? `Daily budget exhausted. Resets in ${data.retryAfterSec || 0} s.`
                        : `Error: ${data?.message || data?.kind || 'unknown'}`,
                });
                renderHistory(historyEl, history);
                refreshBudget(budgetEl);
            });
            stream.on('done', () => {
                setStreaming(false);
                activeStream = null;
                saveHistory(sessionId, history);
                refreshBudget(budgetEl);
            });
            stream.on('cancelled', () => {
                partial.meta = 'cancelled';
                renderHistory(historyEl, history);
            });
        }

        sendBtn.addEventListener('click', sendMessage);
        cancelBtn.addEventListener('click', () => {
            if (activeStream) activeStream.cancel();
        });
        inputEl.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                sendMessage();
            }
        });

        // Click-to-seek on cited timestamps. We dispatch a custom
        // event the page-level glue can listen for; pages that don't
        // wire it up just ignore the click (no-op).
        historyEl.addEventListener('click', (e) => {
            const a = e.target.closest('.llmchat-ts');
            if (!a) return;
            e.preventDefault();
            const ts = a.dataset.ts || a.textContent;
            const seconds = window.LLMChat.parseTimestamp(ts);
            if (!Number.isFinite(seconds)) return;
            document.dispatchEvent(new CustomEvent('llm:seek', {
                detail: { sessionId, seconds, ts },
            }));
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
