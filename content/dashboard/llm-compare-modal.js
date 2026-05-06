// Two-session AI compare for sessions.html (epic #412, issue #421).
//
// Adds a checkbox to each picker row alongside the existing star /
// Analyze button. When exactly two rows are selected, a floating
// "Compare 2 sessions" button appears bottom-right; clicking opens
// a modal that POSTs {sessions:[a,b], one_shot:true} to
// /api/session_chat. The forwarder injects "Compare sessions: [a,b]"
// into the system preamble and the prompt's compare-mode hints
// take it from there (Similarities / Differences / Hypotheses).

(function () {
    'use strict';

    if (!window.LLMChat) {
        console.warn('llm-compare-modal: LLMChat not loaded; compare disabled');
        return;
    }

    const STYLE_ID = 'llmchat-compare-styles';
    // Use our own checkbox class so we don't collide with the live
    // testing UI's .session-checkbox (used for failure-mode group ops).
    const CB_CLASS = 'llmchat-compare-cb';
    const selected = new Set();

    function injectStyles() {
        if (document.getElementById(STYLE_ID)) return;
        const css = `
.${CB_CLASS} {
    margin-right: 6px;
    cursor: pointer;
    position: relative;
    z-index: 3;
    vertical-align: middle;
}
.llmchat-compare-fab {
    position: fixed;
    right: 16px;
    bottom: 84px;
    background: #4f46e5;
    color: #fff;
    border: none;
    padding: 10px 16px;
    border-radius: 24px;
    box-shadow: 0 6px 18px rgba(79, 70, 229, 0.35);
    cursor: pointer;
    font-size: 13px;
    font-weight: 600;
    z-index: 9000;
    display: none;
    align-items: center;
    gap: 8px;
}
.llmchat-compare-fab.active { display: inline-flex; }
.llmchat-compare-fab[disabled] { opacity: 0.6; cursor: not-allowed; }
.llmchat-compare-count {
    background: rgba(255, 255, 255, 0.2);
    border-radius: 12px;
    padding: 1px 8px;
    font-size: 11px;
}
        `.trim();
        const style = document.createElement('style');
        style.id = STYLE_ID;
        style.textContent = css;
        document.head.appendChild(style);
    }

    function injectCheckbox(starSpan, fab) {
        const sessionId = starSpan.dataset.sessionId;
        if (!sessionId) return;
        const cell = starSpan.parentElement;
        if (!cell || cell.querySelector(`.${CB_CLASS}`)) return;

        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.className = CB_CLASS;
        cb.dataset.sessionId = sessionId;
        cb.title = 'Select to compare with another session';
        cb.checked = selected.has(sessionId);
        cb.addEventListener('click', (e) => {
            // The row has a stretched-link overlay; stop the click
            // from triggering navigation.
            e.stopPropagation();
        });
        cb.addEventListener('change', () => {
            if (cb.checked) {
                if (selected.size >= 2 && !selected.has(sessionId)) {
                    cb.checked = false;
                    return; // Cap at 2 — third click no-ops.
                }
                selected.add(sessionId);
            } else {
                selected.delete(sessionId);
            }
            // Sync any duplicate checkbox elements (MutationObserver
            // re-renders may have created stale copies during sort).
            document.querySelectorAll(`.${CB_CLASS}`).forEach((other) => {
                if (other.dataset.sessionId === sessionId) other.checked = cb.checked;
            });
            updateFab(fab);
        });
        cell.insertBefore(cb, starSpan);
    }

    function updateFab(fab) {
        const count = selected.size;
        if (count === 0) {
            fab.classList.remove('active');
            return;
        }
        fab.classList.add('active');
        fab.disabled = count !== 2;
        const label = count === 2
            ? 'Compare 2 sessions'
            : (count === 1 ? 'Pick one more to compare' : `${count} selected (cap is 2)`);
        fab.innerHTML = `<span class="llmchat-compare-count">${count}</span>${label}`;
    }

    function openCompareModal() {
        const ids = Array.from(selected);
        if (ids.length !== 2) return;

        const backdrop = document.createElement('div');
        backdrop.className = 'llm-modal-backdrop';
        backdrop.innerHTML = `
            <div class="llm-modal" role="dialog" aria-modal="true">
                <div class="llm-modal-header">
                    <h3>Comparing 2 sessions</h3>
                    <button class="llm-modal-close" type="button" aria-label="Close">×</button>
                </div>
                <div class="llm-modal-body" data-role="body">
                    <div class="llm-modal-status">Sending compare request…</div>
                </div>
                <div class="llm-modal-meta" data-role="meta"></div>
            </div>
        `;
        document.body.appendChild(backdrop);
        const bodyEl = backdrop.querySelector('[data-role=body]');
        const metaEl = backdrop.querySelector('[data-role=meta]');
        const closeBtn = backdrop.querySelector('.llm-modal-close');

        const stream = window.LLMChat.chat({
            profile: localStorage.getItem('llm-chat-profile') || '',
            messages: [{
                role: 'user',
                content: `Compare these two sessions: ${ids[0]} vs ${ids[1]}. What's similar, what's different, what hypotheses explain the divergence?`,
            }],
            sessions: ids,
            oneShot: true,
        });

        let assistantText = '';
        stream.on('tool_call', (data) => {
            if (!assistantText) {
                bodyEl.innerHTML = `<div class="llm-modal-status">Running ${escapeHTML(data?.name || 'tool')}…</div>`;
            }
        });
        stream.on('tool_result', (data) => {
            if (!assistantText) {
                bodyEl.innerHTML = `<div class="llm-modal-status">Got ${data?.rows ?? 0} rows in ${data?.elapsed_ms ?? 0} ms — thinking…</div>`;
            }
        });
        stream.on('assistant_message', (data) => {
            assistantText = data?.content || '';
            bodyEl.innerHTML = window.LLMChat.linkifyTimestamps(assistantText);
        });
        stream.on('usage', (data) => {
            const cost = (data?.cost_usd || 0).toFixed(5);
            const tokens = (data?.input_tokens || 0) + (data?.output_tokens || 0);
            metaEl.textContent = `$${cost} · ${tokens} tokens · ${data?.tool_calls_count || 0} tool calls · stopped: ${data?.stopped_reason || '—'}`;
        });
        stream.on('error', (data) => {
            bodyEl.innerHTML = `<div class="llm-modal-error">${escapeHTML(data?.message || data?.kind || 'error')}</div>`;
        });

        const trigger = document.activeElement;
        const escHandler = (e) => { if (e.key === 'Escape') close(); };
        function close() {
            stream.cancel();
            backdrop.remove();
            document.removeEventListener('keydown', escHandler);
            if (trigger && typeof trigger.focus === 'function') {
                try { trigger.focus(); } catch { /* ignore */ }
            }
        }
        closeBtn.addEventListener('click', close);
        backdrop.addEventListener('click', (e) => { if (e.target === backdrop) close(); });
        document.addEventListener('keydown', escHandler);
    }

    function escapeHTML(s) {
        return String(s).replace(/[&<>"']/g, (c) => ({
            '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
        }[c]));
    }

    function init() {
        injectStyles();
        const fab = document.createElement('button');
        fab.className = 'llmchat-compare-fab';
        fab.type = 'button';
        fab.addEventListener('click', openCompareModal);
        document.body.appendChild(fab);

        function scan() {
            document.querySelectorAll('span[data-star-cell][data-session-id]').forEach((s) => injectCheckbox(s, fab));
        }
        scan();
        const container = document.getElementById('sessionsContainer');
        if (container) {
            const obs = new MutationObserver(() => scan());
            obs.observe(container, { childList: true, subtree: true });
        }
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
