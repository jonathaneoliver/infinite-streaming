// Per-row Analyze button + modal for sessions.html (epic #412,
// issue #419).
//
// Watches #sessionsContainer for .session-card elements (rendered
// by session-shell.js) and injects an "Analyze" button into each
// card's header. Clicking opens a modal that runs a one-shot chat
// against that session and streams the assistant_message body
// inline.

(function () {
    'use strict';

    if (!window.LLMChat) {
        console.warn('llm-analyze-modal: LLMChat not loaded; modal disabled');
        return;
    }

    const STYLE_ID = 'llm-analyze-styles';

    function injectStyles() {
        if (document.getElementById(STYLE_ID)) return;
        const css = `
.llmchat-analyze-btn {
    background: #4f46e5;
    color: #fff;
    border: none;
    padding: 4px 10px;
    border-radius: 6px;
    cursor: pointer;
    font-size: 11px;
    font-weight: 600;
    margin-left: 6px;
}
.llmchat-analyze-btn:hover { background: #4338ca; }
.llmchat-analyze-btn[disabled] { opacity: 0.5; cursor: not-allowed; }

.llm-modal-backdrop {
    position: fixed;
    inset: 0;
    background: rgba(15, 23, 42, 0.45);
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding-top: 60px;
    z-index: 9100;
}
.llm-modal {
    background: #fff;
    border-radius: 12px;
    width: min(720px, 92vw);
    max-height: 80vh;
    display: flex;
    flex-direction: column;
    box-shadow: 0 20px 60px rgba(15, 23, 42, 0.3);
}
.llm-modal-header {
    padding: 14px 16px;
    border-bottom: 1px solid #e5e7eb;
    display: flex;
    align-items: center;
    gap: 10px;
}
.llm-modal-header h3 { margin: 0; font-size: 15px; flex: 1; }
.llm-modal-close { background: none; border: none; cursor: pointer; font-size: 22px; color: #64748b; }
.llm-modal-body {
    padding: 14px 16px;
    overflow-y: auto;
    flex: 1;
    line-height: 1.55;
    font-size: 13px;
    color: #0f172a;
}
.llm-modal-status {
    color: #64748b;
    font-size: 12px;
    font-style: italic;
}
.llm-modal-meta {
    padding: 8px 16px;
    background: #f8fafc;
    border-top: 1px solid #e5e7eb;
    font-size: 11px;
    color: #64748b;
}
.llm-modal-error { background: #fef2f2; color: #991b1b; padding: 12px; border-radius: 8px; }
        `.trim();
        const style = document.createElement('style');
        style.id = STYLE_ID;
        style.textContent = css;
        document.head.appendChild(style);
    }

    function openModal(sessionId) {
        const backdrop = document.createElement('div');
        backdrop.className = 'llm-modal-backdrop';
        backdrop.innerHTML = `
            <div class="llm-modal" role="dialog" aria-modal="true" aria-labelledby="llm-modal-title">
                <div class="llm-modal-header">
                    <h3 id="llm-modal-title">Analyzing session ${escapeHTML(sessionId.substring(0, 16))}…</h3>
                    <button class="llm-modal-close" type="button" aria-label="Close">×</button>
                </div>
                <div class="llm-modal-body" data-role="body">
                    <div class="llm-modal-status">Sending your request…</div>
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
                content: 'Give me an overview of this session — what happened, anything notable.',
            }],
            sessionId,
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

        // Track the trigger so focus returns there on close (a11y).
        const trigger = document.activeElement;
        const escHandler = (e) => {
            if (e.key === 'Escape') close();
        };
        function close() {
            stream.cancel();
            backdrop.remove();
            document.removeEventListener('keydown', escHandler);
            if (trigger && typeof trigger.focus === 'function') {
                try { trigger.focus(); } catch { /* ignore */ }
            }
        }
        closeBtn.addEventListener('click', close);
        backdrop.addEventListener('click', (e) => {
            if (e.target === backdrop) close();
        });
        document.addEventListener('keydown', escHandler);
    }

    // Picker rows (sessions.html) carry the session_id on a
    // <span data-star-cell> in the first cell rather than on the
    // <tr> directly — see session-replay.js where the table is
    // rendered. Inject the Analyze button into the same cell as
    // the star, with z-index above the row's stretched-link
    // overlay (z-index:0) so clicks reach us, not the row link.
    function injectAnalyzeButton(starSpan) {
        const sessionId = starSpan.dataset.sessionId;
        if (!sessionId) return;
        const cell = starSpan.parentElement;
        if (!cell || cell.querySelector('.llmchat-analyze-btn')) return;
        const btn = document.createElement('button');
        btn.className = 'llmchat-analyze-btn';
        btn.type = 'button';
        btn.textContent = 'Analyze';
        btn.title = 'Run an AI overview of this session';
        btn.style.position = 'relative';
        btn.style.zIndex = '3';
        btn.addEventListener('click', (e) => {
            e.preventDefault();
            e.stopPropagation();
            openModal(sessionId);
        });
        cell.appendChild(btn);
    }

    function scanAndInject() {
        document.querySelectorAll('span[data-star-cell][data-session-id]').forEach(injectAnalyzeButton);
    }

    function escapeHTML(s) {
        return String(s).replace(/[&<>"']/g, (c) => ({
            '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
        }[c]));
    }

    function init() {
        injectStyles();
        // Initial scan + observer for session-shell.js re-renders.
        scanAndInject();
        const container = document.getElementById('sessionsContainer');
        if (!container) return;
        const obs = new MutationObserver(() => scanAndInject());
        obs.observe(container, { childList: true, subtree: true });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
