// Session Viewer / replay engine. Pairs with session-shell.js (must be
// loaded first); reads chart-history Maps + applySessionsList /
// pushBandwidthSample from window.SessionShell. Exposes startMode (full
// replay loader for ?session=<id>) and startPicker (filterable session
// list shown when ?replay=1 has no &session=) on window.SessionReplay.
(function () {
    'use strict';
    if (!window.SessionShell) {
        console.error('session-replay.js: SessionShell not loaded');
        return;
    }
    const {
        sessionsById,
        applySessionsList,
        pushBandwidthSample,
        bandwidthHistory,
        bandwidthEventHistory,
        bandwidthCharts,
        bufferDepthCharts,
        videoFpsCharts,
        eventsCharts,
        bandwidthVisibility,
        bitrateAxisMaxBySession,
        chartViewportBySession,
        chartPausedBySession,
        lastRecordedPlayerEventBySession,
        lastRecordedLoopCountBySession,
        lastRecordedServerLoopCountBySession,
        lastRecordedLoopEventStampBySession,
        lastRecordedServerLoopEventStampBySession,
        lastRecordedControlsBySession,
        lastRecordedPlayIdBySession,
        chartRenderSuppressedBySession,
        chartWindowMsBySession,
        playBoundsBySession,
        brushDraggingBySession
    } = window.SessionShell;

        // shared-nav.js wraps page content into <div class="ism-app">…<div id="ism-content">.
        // Banners must mount inside the content area, otherwise they end up as a sibling of
        // the entire app shell and stretch across the viewport above the sidebar.
        function mountPoint() {
            return document.getElementById('ism-content') || document.body;
        }

        function makeReplayBanner(initialText, sessionId, playId) {
            // Clean dashboard panel — no historical-stripe chrome. The
            // "back to sessions" link and the scrubber row live here.
            const banner = document.createElement('div');
            banner.id = 'replay-banner';
            banner.className = 'panel';
            banner.style.cssText = 'position:sticky;top:0;z-index:1000;font:13px system-ui;color:var(--text-primary);';
            const topRow = document.createElement('div');
            topRow.style.cssText = 'display:flex;align-items:center;justify-content:space-between;gap:16px;flex-wrap:wrap;';
            const left = document.createElement('div');
            left.style.cssText = 'display:flex;align-items:center;gap:10px;flex-wrap:wrap;';
            const text = document.createElement('span');
            text.id = 'replay-banner-text';
            text.style.cssText = 'color:var(--text-secondary);';
            text.textContent = initialText;
            left.append(text);
            const right = document.createElement('div');
            right.style.cssText = 'display:flex;align-items:center;gap:8px;';
            // Session bundle download — full snapshots + HAR + events
            // archived as a ZIP. Server builds it; browser just clicks
            // <a download>. Live during the replay session, no need to
            // wait for snapshots to finish loading.
            if (sessionId) {
                const bundleHref = '/analytics/api/session_bundle?session=' + encodeURIComponent(sessionId)
                    + (playId ? '&play_id=' + encodeURIComponent(playId) : '');
                const bundleBtn = document.createElement('a');
                bundleBtn.href = bundleHref;
                bundleBtn.setAttribute('download', '');
                bundleBtn.textContent = '📥 Download bundle';
                bundleBtn.title = 'Download session as .zip (snapshots, HAR, events)';
                bundleBtn.className = 'btn btn-secondary';
                right.appendChild(bundleBtn);
            }
            const exit = document.createElement('a');
            exit.textContent = '← Back to sessions';
            exit.href = '/dashboard/sessions.html';
            exit.className = 'btn btn-secondary';
            right.appendChild(exit);
            topRow.append(left, right);
            const scrubRow = document.createElement('div');
            scrubRow.id = 'replay-scrub-row';
            scrubRow.style.cssText = 'display:none;align-items:center;gap:10px;margin-top:10px;font:500 12px system-ui;color:var(--text-secondary);';
            banner.append(topRow, scrubRow);
            // Mount inside .ism-content-wide *after* the page-header so it
            // sits in the document flow with the page title above it.
            const wide = document.querySelector('.ism-content-wide');
            if (wide) {
                const header = wide.querySelector('.page-header');
                if (header && header.nextSibling) wide.insertBefore(banner, header.nextSibling);
                else if (header) wide.appendChild(banner);
                else wide.insertBefore(banner, wide.firstChild);
            } else {
                const mp = mountPoint();
                mp.insertBefore(banner, mp.firstChild);
            }
            document.body.classList.add('replay-mode');
            return { banner, text, scrubRow };
        }

        // Reset the per-session state Maps that pushBandwidthSample populates,
        // so we can re-feed a different time window of snapshots without
        // ghost data from the previous window. We *don't* destroy the
        // Chart.js / vis-timeline instances here — that triggers ~150-
        // 400ms of teardown + reconstruction per brush move, which is
        // why scrubbing felt slow. Instead we wipe the underlying
        // history Maps; the next pushBandwidthSample run rebuilds them
        // and the existing chart instances pick up the new data via
        // chart.update('none') (cheap).
        function clearReplaySessionState(sessionId) {
            const key = String(sessionId);
            bandwidthHistory.delete(key);
            bandwidthEventHistory.delete(key);
            bandwidthVisibility.delete(key);
            bitrateAxisMaxBySession.delete(key);
            chartViewportBySession.delete(key);
            chartPausedBySession.delete(key);
            lastRecordedPlayerEventBySession.delete(key);
            lastRecordedLoopCountBySession.delete(key);
            lastRecordedServerLoopCountBySession.delete(key);
            lastRecordedLoopEventStampBySession.delete(key);
            lastRecordedServerLoopEventStampBySession.delete(key);
            lastRecordedControlsBySession.delete(key);
            lastRecordedPlayIdBySession.delete(key);
        }

        // Render-without-fetch: feed a windowed slice of the cached
        // snapshot array through the renderer. Date.now is patched so the
        // chart accumulators key off recorded ts. Performance: rather than
        // call applySessionsList → renderSessions per snapshot (which
        // would redraw Chart.js + vis-timeline every iteration), we update
        // the history Maps directly via pushBandwidthSample for all but
        // the last snapshot, then call applySessionsList once at the end
        // to trigger a single full render. Yields to the main thread
        // every YIELD_EVERY iterations to keep the page responsive.
        async function replayWindow(sessionId, snapshots, endMs, windowMs, progressFn) {
            const startMs = endMs - windowMs;
            // Tell pushBandwidthSample / renderBandwidthChart to use the
            // brush window as the chart's rolling cap (instead of the
            // hard-coded 10 minutes). This lets expanding the focus
            // window in the session viewer actually grow the bitrate /
            // buffer / FPS / events chart x-axis. Cleared when the user
            // exits replay; live mode never sets it.
            chartWindowMsBySession.set(String(sessionId), windowMs);
            // Filter then sort — `snapshots` is in receive order (whatever
            // direction the stream arrived), but pushBandwidthSample needs
            // chronological feeding. Subset is bounded by window size, so
            // an O(W log W) sort here is much cheaper than maintaining a
            // sorted invariant on every line.
            const subset = snapshots.filter(s => s.tsMs >= startMs && s.tsMs <= endMs);
            subset.sort((a, b) => a.tsMs - b.tsMs);
            clearReplaySessionState(sessionId);
            const realDateNow = Date.now.bind(Date);
            let mockNow = realDateNow();
            Date.now = () => mockNow;
            // Suppress per-snapshot chart re-renders during the bulk
            // pushBandwidthSample loop — otherwise we'd pay for a full
            // chart.update() per snapshot (N updates for an N-snapshot
            // window). The history Maps still get populated; we then do
            // ONE render at the end via applySessionsList. Refcount-
            // based so overlapping replayWindow calls (rapid brush
            // dragging) don't permanently strand the suppression.
            const sidStr = String(sessionId);
            chartRenderSuppressedBySession.enter(sidStr);
            let exited = false;
            const YIELD_EVERY = 250;
            try {
                for (let i = 0; i < subset.length - 1; i++) {
                    const s = subset[i];
                    if (Number.isFinite(s.tsMs)) mockNow = s.tsMs;
                    sessionsById.set(sidStr, s.snap);
                    try { pushBandwidthSample(s.snap); } catch (_e) {}
                    if ((i + 1) % YIELD_EVERY === 0) {
                        if (progressFn) progressFn(i + 1, subset.length);
                        await new Promise(r => setTimeout(r, 0));
                    }
                }
                // Last snapshot: drop our refcount and do the full render
                // path. If no other replayWindow is also suppressing,
                // depth hits 0 and the render fires inside the
                // applySessionsList → pushBandwidthSamplesForAllSessions
                // chain. If another is overlapping, that one's exit will
                // be the one to actually render.
                if (subset.length > 0) {
                    const last = subset[subset.length - 1];
                    if (Number.isFinite(last.tsMs)) mockNow = last.tsMs;
                    chartRenderSuppressedBySession.exit(sidStr);
                    exited = true;
                    // {force:true} bypasses the live-mode version-gate
                    // — replay deliberately re-applies older snapshots
                    // when the user scrubs back through history.
                    try { applySessionsList([last.snap], { force: true }); } catch (err) { console.warn('replay render error', err); }
                }
                if (progressFn) progressFn(subset.length, subset.length);
            } finally {
                Date.now = realDateNow;
                if (!exited) chartRenderSuppressedBySession.exit(sidStr);
            }
            return subset.length;
        }

        function fmtIsoLocal(ms) {
            if (!Number.isFinite(ms)) return '—';
            const d = new Date(ms);
            const pad = n => String(n).padStart(2, '0');
            return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
        }

        function fmtDuration(ms) {
            if (!Number.isFinite(ms) || ms <= 0) return '0s';
            const s = Math.round(ms / 1000);
            const h = Math.floor(s / 3600);
            const m = Math.floor((s % 3600) / 60);
            const ss = s % 60;
            if (h) return `${h}h${String(m).padStart(2,'0')}m`;
            if (m) return `${m}m${String(ss).padStart(2,'0')}s`;
            return `${ss}s`;
        }

        async function startReplayMode(sessionId, fromIso, toIso, playId) {
            const { banner, text, scrubRow } = makeReplayBanner(`session ${sessionId} — loading snapshots…`, sessionId, playId);

            const fail = (msg) => {
                banner.style.background = '#fef2f2';
                banner.style.borderColor = '#fecaca';
                text.style.color = '#991b1b';
                text.textContent = msg;
            };

            // Pre-flight: fetch count + bounds so we can pick an adaptive
            // stride. Hour-long sessions with 100k+ snapshots get bucketed
            // server-side; short sessions stream raw at full fidelity.
            let expectedTotal = null;
            let preflightFirstTs = '';
            let preflightLastTs = '';
            const countParams = new URLSearchParams({ session: sessionId });
            if (playId) countParams.set('play_id', playId);
            if (fromIso) countParams.set('from', fromIso);
            if (toIso) countParams.set('to', toIso);
            try {
                const cr = await fetch(`/analytics/api/snapshot_count?${countParams.toString()}`);
                if (cr.ok) {
                    const ct = await cr.text();
                    const line = (ct || '').split('\n').find(l => l);
                    if (line) {
                        const meta = JSON.parse(line);
                        const n = Number(meta.count);
                        if (Number.isFinite(n) && n > 0) expectedTotal = n;
                        preflightFirstTs = meta.first_ts || '';
                        preflightLastTs  = meta.last_ts  || '';
                    }
                }
            } catch (err) { console.warn('snapshot_count failed:', err); }

            // Adaptive stride: target ~5000 sampled snapshots so the
            // browser stays responsive on long sessions. Anything below
            // that streams raw.
            const TARGET_PTS = 5000;
            let strideMs = 0;
            if (expectedTotal && expectedTotal > TARGET_PTS && preflightFirstTs && preflightLastTs) {
                const startMs = Date.parse(preflightFirstTs.replace(' ', 'T') + 'Z');
                const endMs   = Date.parse(preflightLastTs.replace(' ', 'T') + 'Z');
                const span    = endMs - startMs;
                if (Number.isFinite(span) && span > 0) {
                    strideMs = Math.max(100, Math.ceil(span / TARGET_PTS));
                }
            }

            // Stream snapshots from ClickHouse in reverse-chronological
            // order so the end-of-session window paints first.
            const params = new URLSearchParams({
                session: sessionId,
                limit: '200000',
                order: 'desc'
            });
            if (playId) params.set('play_id', playId);
            if (fromIso) params.set('from', fromIso);
            if (toIso) params.set('to', toIso);
            if (strideMs > 0) params.set('stride_ms', String(strideMs));
            let resp;
            try {
                resp = await fetch(`/analytics/api/snapshots?${params.toString()}`);
            } catch (err) {
                fail(`Replay failed: ${err.message}`); return;
            }
            if (!resp.ok) { fail(`Replay failed: HTTP ${resp.status} (analytics forwarder reachable?)`); return; }
            if (!resp.body || !resp.body.getReader) { fail('Replay failed: streaming reader not supported by this browser'); return; }

            // Snapshots accumulate in receive order (no sort during ingest)
            // and only get sorted in replayWindow's small filtered subset
            // before feeding pushBandwidthSample. Bounds are tracked inline
            // for O(1) per-line cost.
            const snapshots = [];
            let observedStartMs = Infinity;
            let observedEndMs = -Infinity;
            const BATCH_SIZE = 250;

            // Progress UI: a thin progress bar + text under the main banner
            // line. Updated as bytes/lines stream in. Total count comes from
            // a separate /api/snapshot_count fetch fired in parallel.
            const progressWrap = document.createElement('div');
            progressWrap.style.cssText = 'display:flex;align-items:center;gap:10px;margin-top:8px;font:500 11px ui-monospace,Menlo,monospace;color:var(--text-secondary);';
            const progressBarOuter = document.createElement('div');
            progressBarOuter.style.cssText = 'flex:1;min-width:160px;height:6px;background:var(--bg-secondary,#e5e7eb);border-radius:3px;overflow:hidden;position:relative;';
            const progressBarFill = document.createElement('div');
            progressBarFill.style.cssText = 'position:absolute;left:0;top:0;bottom:0;width:0%;background:linear-gradient(90deg,#3b82f6,#1d4ed8);transition:width 120ms linear;';
            const progressBarShimmer = document.createElement('div');
            progressBarShimmer.style.cssText = 'position:absolute;left:0;top:0;bottom:0;width:100%;background:repeating-linear-gradient(45deg,transparent 0 8px,rgba(255,255,255,0.3) 8px 16px);animation:replay-shimmer 1s linear infinite;opacity:0.5;';
            progressBarOuter.append(progressBarFill, progressBarShimmer);
            const progressText = document.createElement('span');
            progressText.style.cssText = 'flex-shrink:0;white-space:nowrap;';
            progressText.textContent = 'streaming…';
            progressWrap.append(progressBarOuter, progressText);
            banner.appendChild(progressWrap);
            // Inject keyframes once for the shimmer animation.
            if (!document.getElementById('replay-shimmer-keyframes')) {
                const style = document.createElement('style');
                style.id = 'replay-shimmer-keyframes';
                style.textContent = '@keyframes replay-shimmer { from { background-position: 0 0; } to { background-position: 16px 0; } }';
                document.head.appendChild(style);
            }

            // expectedTotal already set in the pre-flight above; the
            // progress UI also accounts for stride downsampling: the
            // server returns ceil(span/stride) rows, not `count`.
            const downsampledTotal = strideMs > 0 && preflightFirstTs && preflightLastTs
                ? Math.max(1, Math.ceil(
                    (Date.parse(preflightLastTs.replace(' ', 'T') + 'Z') -
                     Date.parse(preflightFirstTs.replace(' ', 'T') + 'Z')) / strideMs))
                : 0;
            if (downsampledTotal > 0) expectedTotal = downsampledTotal;

            const streamStartedAt = performance.now();
            const updateProgress = () => {
                const elapsedSec = (performance.now() - streamStartedAt) / 1000;
                const rate = elapsedSec > 0 ? totalLines / elapsedSec : 0;
                const kb = (bytesIn / 1024);
                const kbRate = elapsedSec > 0 ? kb / elapsedSec : 0;
                if (expectedTotal != null && expectedTotal > 0) {
                    const pct = Math.min(100, (totalLines / expectedTotal) * 100);
                    progressBarFill.style.width = pct.toFixed(1) + '%';
                    progressBarShimmer.style.display = pct >= 100 ? 'none' : 'block';
                    const remaining = Math.max(0, expectedTotal - totalLines);
                    const etaSec = rate > 0 ? remaining / rate : null;
                    const etaStr = etaSec == null
                        ? '—'
                        : (etaSec < 1 ? '<1s' : etaSec < 60 ? `${etaSec.toFixed(0)}s` : `${(etaSec/60).toFixed(1)}m`);
                    progressText.textContent = `${totalLines.toLocaleString()}/${expectedTotal.toLocaleString()} · ${pct.toFixed(0)}% · ${kb.toFixed(0)} KB · ${kbRate.toFixed(0)} KB/s · ${rate.toFixed(0)}/s · ETA ${etaStr}`;
                } else {
                    // No total yet — show indeterminate progress.
                    progressBarFill.style.width = '0%';
                    progressText.textContent = `${totalLines.toLocaleString()} streamed · ${kb.toFixed(0)} KB · ${kbRate.toFixed(0)} KB/s · ${rate.toFixed(0)}/s`;
                }
            };
            const finishProgress = (label) => {
                progressBarFill.style.width = '100%';
                progressBarFill.style.background = '#16a34a';
                progressBarShimmer.style.display = 'none';
                const elapsedSec = (performance.now() - streamStartedAt) / 1000;
                progressText.textContent = `${label} · ${totalLines.toLocaleString()} snapshots in ${elapsedSec.toFixed(1)}s`;
                // Once the stream is done the progress bar is just visual
                // weight. Fade it down to a small caption after a moment.
                setTimeout(() => {
                    progressBarOuter.style.display = 'none';
                    progressWrap.style.fontSize = '10px';
                    progressWrap.style.opacity = '0.55';
                }, 1500);
            };
            const failProgress = (label) => {
                progressBarFill.style.background = '#dc2626';
                progressBarShimmer.style.display = 'none';
                progressText.textContent = label;
            };

            // First-line gate: we don't know session bounds until at least
            // one row arrives. Initialise the UI on first batch.
            let initialized = false;
            let sessionStartMs = 0, sessionEndMs = 0, sessionDurationMs = 0;
            const WINDOW_MS = 10 * 60 * 1000;
            const MIN_WINDOW_MS = 5000;
            let brushStart = 0, brushEnd = 0;
            let userMovedBrush = false;

            // Brush UI is built once on first batch (when we know session
            // bounds). Subsequent batches just refresh the ticks and
            // bounds without rebuilding the DOM scaffolding.
            let overviewEl, ticksEl, markersEl, brushEl, windowText;
            let leftHandle, rightHandle;

            const setHeader = (extra) => {
                // session_id is just a small integer with no meaning to humans
                // beyond "the proxy port slot", so drop it. play_id is the
                // useful one (it identifies the playback episode).
                const idChip = (label, value) => `<code style="background:rgba(0,0,0,0.06);padding:1px 5px;border-radius:3px;font:600 11px ui-monospace,Menlo,monospace;" title="${label}">${value}</code>`;
                const parts = [];
                if (playId) parts.push(`play ${idChip('play_id', playId)}`);
                parts.push(`duration <strong>${fmtDuration(sessionDurationMs)}</strong>`);
                parts.push(`${snapshots.length} snapshots`);
                text.innerHTML = parts.join(' · ') + (extra ? ' · ' + extra : '');
            };

            const updateBrushVisuals = () => {
                if (!brushEl) return;
                const span = Math.max(1, sessionDurationMs);
                const leftPct = Math.max(0, Math.min(100, ((brushStart - sessionStartMs) / span) * 100));
                const widthPct = Math.max(0.5, Math.min(100 - leftPct, ((brushEnd - brushStart) / span) * 100));
                brushEl.style.left = `${leftPct}%`;
                brushEl.style.width = `${widthPct}%`;
                // Position the floating windowText so it sits under the
                // brush midpoint, clamped so it never spills off the rail.
                if (windowText) {
                    const midPct = Math.max(8, Math.min(92, leftPct + widthPct / 2));
                    windowText.style.left = midPct.toFixed(2) + '%';
                }
                // Update start/end rail labels — only on first paint or
                // when bounds change (cheap DOM writes).
                if (scrubRow && scrubRow._startLabel && scrubRow._endLabel) {
                    scrubRow._startLabel.textContent = fmtIsoLocal(sessionStartMs);
                    scrubRow._endLabel.textContent = fmtIsoLocal(sessionEndMs);
                }
            };
            const updateWindowText = (status) => {
                if (!windowText) return;
                const winSpan = brushEnd - brushStart;
                const fromEnd = sessionEndMs - brushEnd;
                const place = fromEnd < 1500 ? 'at end' : `${fmtDuration(fromEnd)} from end`;
                // Compact form for the brush-anchored label — full
                // ISO range is overkill once start/end labels show it.
                const tail = status ? ` · ${status}` : '';
                windowText.textContent = `${fmtDuration(winSpan)} · ${place}${tail}`;
            };

            // Health-heatmap data (filled by fetchHeatmap) and notable
            // session events (filled by fetchEvents). Both share the
            // brush overview rail: heatmap paints cell backgrounds,
            // events paint vertical markers + populate the jump-list.
            let heatmapBuckets = null;
            let sessionEvents = null;

            // Lazy-create a singleton tooltip pinned to the body so
            // it can overflow any container. Used by rail-marker
            // hover; positioned via fixed coords from mouse events.
            let railTooltip = null;
            const ensureRailTooltip = () => {
                if (railTooltip) return railTooltip;
                railTooltip = document.createElement('div');
                railTooltip.style.cssText =
                    'position:fixed;z-index:9999;pointer-events:none;' +
                    'background:rgba(17,24,39,0.95);color:#f9fafb;' +
                    'padding:6px 8px;border-radius:4px;' +
                    'font:12px system-ui,-apple-system,sans-serif;' +
                    'box-shadow:0 4px 10px rgba(0,0,0,0.25);' +
                    'max-width:320px;display:none;line-height:1.35;' +
                    'border-left:3px solid #0891b2;';
                document.body.appendChild(railTooltip);
                return railTooltip;
            };
            const showRailTooltip = (ev, mouseX, mouseY) => {
                const tip = ensureRailTooltip();
                const typeSty = EVENT_TYPE_STYLE[ev.type] || { label: ev.type, icon: '•' };
                const prSty   = PRIORITY_STYLE[ev.priority] || PRIORITY_STYLE[4];
                const offsetMs = Math.max(0, ev.tsMs - sessionStartMs);
                const kindTag = ev.kind === 'cause'
                    ? '<span style="opacity:0.7;font-size:10px;">cause</span>'
                    : '<span style="opacity:0.7;font-size:10px;">effect</span>';
                tip.innerHTML =
                    `<div style="font-weight:600;margin-bottom:2px;">` +
                        `${typeSty.icon} ${typeSty.label}` +
                    `</div>` +
                    `<div style="font-size:11px;opacity:0.85;">` +
                        `T+${fmtDuration(offsetMs)} · ` +
                        `<span style="color:${prSty.color};font-weight:600;">${prSty.label}</span> · ` +
                        `${kindTag}` +
                    `</div>` +
                    (ev.label && ev.label !== `${typeSty.label} (T+${fmtDuration(offsetMs)})`
                        ? `<div style="font-size:11px;opacity:0.85;margin-top:3px;">${ev.label}</div>`
                        : '') +
                    `<div style="font-size:10px;opacity:0.6;margin-top:4px;">click to jump</div>`;
                tip.style.display = 'block';
                // Position above the cursor, flipping below if too close to top.
                const TIP_PAD = 12;
                const rect = tip.getBoundingClientRect();
                let left = mouseX + TIP_PAD;
                let top  = mouseY - rect.height - TIP_PAD;
                if (left + rect.width > window.innerWidth) left = mouseX - rect.width - TIP_PAD;
                if (top < 4) top = mouseY + TIP_PAD;
                tip.style.left = `${Math.max(4, left)}px`;
                tip.style.top  = `${top}px`;
            };
            const hideRailTooltip = () => {
                if (railTooltip) railTooltip.style.display = 'none';
            };

            // Centre the brush on an event's timestamp, fire the marker
            // dispatch (so charts below draw their guide lines), and
            // sync the dropdown selection. Single source of truth for
            // both the dropdown change handler and rail-marker clicks.
            const selectEvent = (idx) => {
                if (!Number.isFinite(idx) || !sessionEvents || !sessionEvents[idx]) return;
                const ev = sessionEvents[idx];
                const span = brushEnd - brushStart;
                const half = span / 2;
                let newStart = ev.tsMs - half;
                let newEnd = ev.tsMs + half;
                if (newStart < sessionStartMs) { newStart = sessionStartMs; newEnd = newStart + span; }
                if (newEnd > sessionEndMs)     { newEnd = sessionEndMs; newStart = newEnd - span; }
                brushStart = newStart; brushEnd = newEnd;
                userMovedBrush = true;
                updateBrushVisuals();
                triggerRender();
                const typeSty = EVENT_TYPE_STYLE[ev.type] || { label: ev.type, icon: '•' };
                document.dispatchEvent(new CustomEvent('replay:event-marker', {
                    detail: {
                        sessionId,
                        tsMs: ev.tsMs,
                        label: `${typeSty.icon} ${typeSty.label}`,
                        type: ev.type
                    }
                }));
                // Mirror the pick into the dropdown so it always
                // reflects the most recently selected event regardless
                // of whether the user came in via dropdown or rail.
                if (eventsSelect) {
                    const opt = Array.from(eventsSelect.options).find(o => Number(o.value) === idx);
                    if (opt) eventsSelect.value = String(idx);
                }
            };

            // Compute a severity score per heatmap bucket so we can pick a
            // single color cell. Errors and faults dominate; stalls and
            // downshifts are moderate; drops are bucketed per 100 frames.
            const bucketSeverity = (b) => {
                const n = (k) => Number(b[k]) || 0;
                const score = n('error_rows') * 10
                            + n('fault_rows') * 3
                            + n('stalls') * 1
                            + n('downshifts') * 1
                            + Math.ceil(n('drops') / 100);
                return score;
            };
            // Green for nominal so a clean session reads as "all clear"
            // rather than "broken viz". Amber/red still flag trouble.
            const severityColor = (s) => {
                if (s === 0)  return '#a7f3d0'; // nominal: light green
                if (s <= 2)   return '#fde68a'; // minor: light amber
                if (s <= 9)   return '#f59e0b'; // moderate: amber
                return '#dc2626';               // severe: red
            };

            // Re-build the overview rail. With heatmap data: colored cells.
            // Without it (still loading): fall back to per-snapshot ticks.
            const SAMPLE_LIMIT = 1500;
            // Heatmap cell background colors keyed by bucket-max priority.
            // Light enough to read as background; the foreground markers
            // sit on top. A bucket with no filtered events is light green
            // (clean) so a session with no problems reads positively.
            const HEATMAP_CELL_BG = {
                1: '#fecaca', // P1 Critical — light red
                2: '#fde68a', // P2 High — light amber
                3: '#bfdbfe', // P3 Medium — light blue
                4: '#e5e7eb', // P4 Low — light gray
                0: '#dcfce7'  // none — light green (clean)
            };

            const rebuildTicks = () => {
                if (!ticksEl) return;
                ticksEl.replaceChildren();
                if (markersEl) markersEl.replaceChildren();
                const span = Math.max(1, sessionDurationMs);

                if (sessionEvents) {
                    // Bucket events by time and color each cell by the
                    // max-priority event that fell into it (P1 wins over
                    // P2, etc.). Filters from the chip toggles apply
                    // here too — toggling Critical off recolors all
                    // P1-only buckets to whatever's next-highest, or
                    // green if no filtered events landed in the bucket.
                    // 120 cells gives ~1 cell per 30s for a 1-hour
                    // session — enough resolution for at-a-glance scan.
                    const NUM_BUCKETS = 120;
                    const bucketSize  = Math.max(1, span / NUM_BUCKETS);
                    const bucketMaxPriority = new Array(NUM_BUCKETS).fill(0);
                    const bucketCount = new Array(NUM_BUCKETS).fill(0);
                    for (const e of sessionEvents) {
                        if (!priorityEnabled[e.priority]) continue;
                        if (!kindEnabled[e.kind]) continue;
                        if (!isEventTypeEnabled(e.type)) continue;
                        if (!uncategorizedEnabled && isUncategorizedType(e.type)) continue;
                        const offset = e.tsMs - sessionStartMs;
                        if (offset < 0 || offset >= span) continue;
                        const idx = Math.min(NUM_BUCKETS - 1, Math.floor(offset / bucketSize));
                        // Lower number = higher priority. 0 means "none yet."
                        if (bucketMaxPriority[idx] === 0 || e.priority < bucketMaxPriority[idx]) {
                            bucketMaxPriority[idx] = e.priority;
                        }
                        bucketCount[idx]++;
                    }
                    const cellWidth = 100 / NUM_BUCKETS;
                    for (let i = 0; i < NUM_BUCKETS; i++) {
                        const left = i * cellWidth;
                        const cell = document.createElement('div');
                        const p = bucketMaxPriority[i];
                        cell.style.cssText = `position:absolute;top:0;bottom:0;left:${left.toFixed(4)}%;width:${cellWidth.toFixed(4)}%;background:${HEATMAP_CELL_BG[p]};`;
                        if (bucketCount[i] > 0) {
                            cell.title = `${bucketCount[i]} event${bucketCount[i] === 1 ? '' : 's'} (max: ${PRIORITY_STYLE[p].label})`;
                        }
                        ticksEl.appendChild(cell);
                    }

                    // Overlay event markers on top of the cells so the
                    // user sees both bucket-density and discrete moments.
                    // Each marker is a transparent ~10 px wide hit-zone
                    // wrapping a thin coloured visible bar — gives a
                    // clickable target without making every marker
                    // visually fat. Hover paints a styled tooltip
                    // (richer than the native title) and click jumps
                    // the brush + fires the marker dispatch.
                    sessionEvents.forEach((e, idx) => {
                        if (!priorityEnabled[e.priority]) return;
                        if (!kindEnabled[e.kind]) return;
                        if (!isEventTypeEnabled(e.type)) return;
                        if (!uncategorizedEnabled && isUncategorizedType(e.type)) return;
                        const offset = (e.tsMs - sessionStartMs) / span;
                        if (offset < 0 || offset > 1) return;
                        const w = e.priority === 1 ? 3 : (e.priority === 2 ? 2 : 1);
                        const op = e.kind === 'cause' ? 0.5 : 1;
                        const HIT_W = 10;
                        const m = document.createElement('div');
                        m.style.cssText =
                            `position:absolute;top:0;bottom:0;` +
                            `left:calc(${(offset*100).toFixed(4)}% - ${HIT_W/2}px);` +
                            `width:${HIT_W}px;cursor:pointer;` +
                            `display:flex;justify-content:center;align-items:stretch;` +
                            `pointer-events:auto;`;
                        m.dataset.eventIdx = String(idx);
                        const bar = document.createElement('div');
                        bar.style.cssText =
                            `width:${w}px;background:${e.color};opacity:${op};` +
                            `pointer-events:none;` +
                            `transition:width 60ms ease, box-shadow 60ms ease;`;
                        m.appendChild(bar);
                        m.addEventListener('mouseenter', (mev) => {
                            // Highlight the visual bar on hover so the
                            // user sees which marker the tooltip belongs
                            // to even when several are clustered nearby.
                            bar.style.width = `${Math.max(w, 4)}px`;
                            bar.style.boxShadow = '0 0 0 1px #0891b2';
                            showRailTooltip(e, mev.clientX, mev.clientY);
                        });
                        m.addEventListener('mousemove', (mev) => {
                            showRailTooltip(e, mev.clientX, mev.clientY);
                        });
                        m.addEventListener('mouseleave', () => {
                            bar.style.width = `${w}px`;
                            bar.style.boxShadow = '';
                            hideRailTooltip();
                        });
                        m.addEventListener('click', (mev) => {
                            mev.stopPropagation();
                            hideRailTooltip();
                            selectEvent(idx);
                        });
                        (markersEl || ticksEl).appendChild(m);
                    });
                    return;
                }

                // Pre-events fallback: per-snapshot tick lines until the
                // events fetch lands.
                const stride = Math.max(1, Math.ceil(snapshots.length / SAMPLE_LIMIT));
                for (let i = 0; i < snapshots.length; i += stride) {
                    const s = snapshots[i];
                    const offset = (s.tsMs - sessionStartMs) / span;
                    const tick = document.createElement('div');
                    tick.style.cssText = `position:absolute;top:0;bottom:0;left:${(offset*100).toFixed(4)}%;width:1px;`;
                    const fault = Number(s.snap?.transport_fault_active) === 1
                        || (s.snap?.manifest_failure_type && s.snap.manifest_failure_type !== 'none')
                        || (s.snap?.segment_failure_type && s.snap.segment_failure_type !== 'none')
                        || (s.snap?.all_failure_type && s.snap.all_failure_type !== 'none');
                    const stalled = String(s.snap?.player_metrics_state || '').toLowerCase() === 'stalled';
                    tick.style.background = fault ? '#dc2626' : (stalled ? '#f59e0b' : '#9ca3af');
                    ticksEl.appendChild(tick);
                }
            };

            // Fetch the per-bucket health summary once we know session
            // bounds. 120 buckets gives ~1 cell per 30s for a 1-hour
            // session, which paints clearly without overflowing the rail.
            async function fetchHeatmap() {
                const p = new URLSearchParams({ session: sessionId, buckets: '120' });
                if (playId) p.set('play_id', playId);
                try {
                    const r = await fetch(`/analytics/api/session_heatmap?${p.toString()}`);
                    if (!r.ok) return;
                    const t = await r.text();
                    heatmapBuckets = t.split('\n').filter(l => l).map(l => {
                        try { return JSON.parse(l); } catch { return null; }
                    }).filter(Boolean);
                    rebuildTicks();
                } catch (err) {
                    console.warn('session_heatmap failed:', err);
                }
            }

            // Per-event-type display: label + icon. Color comes from the
            // event's priority (set by the forwarder) so we can re-tier
            // events server-side without rebuilding the client palette.
            const EVENT_TYPE_STYLE = {
                error:                   { label: 'Error',          icon: '⚠' },
                user_marked:             { label: '911 / User flag',icon: '🚨' },
                master_manifest_failure: { label: 'Master fail',    icon: '◉' },
                all_failure:             { label: 'All-fault',      icon: '◉' },
                stall:                   { label: 'Stall',          icon: '⏸' },
                buffering:               { label: 'Buffering',      icon: '◐' },
                playback_start:          { label: 'Playback start', icon: '▶' },
                restart:                 { label: 'Restart',        icon: '↻' },
                timejump:                { label: 'Time jump',      icon: '⤺' },
                manifest_failure:        { label: 'Manifest fail',  icon: '✕' },
                segment_failure:         { label: 'Segment fail',   icon: '✕' },
                transport_failure:       { label: 'Transport fail', icon: '✕' },
                transfer_active_timeout: { label: 'Active timeout', icon: '⏱' },
                transfer_idle_timeout:   { label: 'Idle timeout',   icon: '⏱' },
                downshift:               { label: 'Downshift',      icon: '⤓' },
                fault_on:                { label: 'Fault on',       icon: '⚡' },
                upshift:                 { label: 'Upshift',        icon: '⤒' },
                fault_off:               { label: 'Fault off',      icon: '✓' },
                loop_server:             { label: 'Server loop',    icon: '↻' },
                // HAR-derived events (network_requests). All classified
                // as 'cause' kind by the forwarder.
                http_5xx:                { label: 'HTTP 5xx',        icon: '⛔' },
                http_4xx:                { label: 'HTTP 4xx',        icon: '⚠' },
                request_timeout:         { label: 'Request timeout', icon: '⏱' },
                request_incomplete:      { label: 'Incomplete req',  icon: '✂' },
                request_faulted:         { label: 'Faulted request', icon: '✕' },
                slow_request:            { label: 'Slow request',    icon: '🐢' },
                slow_segment:            { label: 'Slow segment',    icon: '🐢' },
                request_retry:           { label: 'Retry',           icon: '↺' }
            };
            const PRIORITY_STYLE = {
                1: { label: 'Critical', color: '#dc2626', bg: '#fee2e2', border: '#fca5a5' },
                2: { label: 'High',     color: '#b45309', bg: '#fef3c7', border: '#fcd34d' },
                3: { label: 'Medium',   color: '#1d4ed8', bg: '#dbeafe', border: '#93c5fd' },
                4: { label: 'Low',      color: '#4b5563', bg: '#e5e7eb', border: '#9ca3af' }
            };
            // Default chip state — P1+P2+P3 visible, P4 hidden. Persisted
            // across reloads so a power user's preference sticks.
            const PRIORITY_FILTER_KEY = 'ismReplayPriorityFilter';
            const priorityEnabled = (() => {
                try {
                    const v = JSON.parse(localStorage.getItem(PRIORITY_FILTER_KEY) || 'null');
                    if (v && typeof v === 'object') return Object.assign({1:true,2:true,3:true,4:false}, v);
                } catch {}
                return {1:true, 2:true, 3:true, 4:false};
            })();
            const savePriorityFilter = () => {
                try { localStorage.setItem(PRIORITY_FILTER_KEY, JSON.stringify(priorityEnabled)); } catch {}
            };

            // Helper: an event type is "categorized" if it has an entry
            // in EVENT_TYPE_STYLE; otherwise it came from the forwarder's
            // catch-all UNION ALL — a player-emitted last_event value
            // we don't have a label / icon / priority opinion for. The
            // Uncategorized chip toggles all of those as a group.
            const isUncategorizedType = (type) => !Object.prototype.hasOwnProperty.call(EVENT_TYPE_STYLE, type);

            // Per-event-type filter (default all-true). Toggled via the
            // right-click popover on each priority chip — fine-grained
            // override of the priority-level toggle. Persisted across
            // reloads so a user's "I only care about stalls and errors"
            // preference sticks.
            const TYPE_FILTER_KEY = 'ismReplayEventTypeFilter';
            const eventTypeEnabled = (() => {
                try {
                    const v = JSON.parse(localStorage.getItem(TYPE_FILTER_KEY) || 'null');
                    if (v && typeof v === 'object') return v;
                } catch {}
                return {};
            })();
            const isEventTypeEnabled = (type) => eventTypeEnabled[type] !== false;
            const saveTypeFilter = () => {
                try { localStorage.setItem(TYPE_FILTER_KEY, JSON.stringify(eventTypeEnabled)); } catch {}
            };

            // Bulk on/off for the Uncategorized class. Persisted via
            // the same localStorage backing as per-type filter — toggling
            // the chip flips every uncategorized type's flag in one go.
            const UNCATEGORIZED_FILTER_KEY = 'ismReplayUncategorizedFilter';
            let uncategorizedEnabled = (() => {
                try {
                    const v = JSON.parse(localStorage.getItem(UNCATEGORIZED_FILTER_KEY) || 'null');
                    return v !== false;
                } catch { return true; }
            })();
            const saveUncategorizedFilter = () => {
                try { localStorage.setItem(UNCATEGORIZED_FILTER_KEY, JSON.stringify(uncategorizedEnabled)); } catch {}
            };

            // Kind filter: effects (what the user saw) vs causes (proxy /
            // system actions that *might* produce an effect — fault firings,
            // server-side failure counters). Effects on by default; causes
            // off so triage isn't drowned in operator-injected noise.
            const KIND_FILTER_KEY = 'ismReplayKindFilter';
            const kindEnabled = (() => {
                try {
                    const v = JSON.parse(localStorage.getItem(KIND_FILTER_KEY) || 'null');
                    if (v && typeof v === 'object') return Object.assign({effect:true, cause:false}, v);
                } catch {}
                return {effect: true, cause: false};
            })();
            const saveKindFilter = () => {
                try { localStorage.setItem(KIND_FILTER_KEY, JSON.stringify(kindEnabled)); } catch {}
            };

            // Build the events jump-list dropdown. Each option, when
            // selected, centers the brush on the event's timestamp.
            let eventsSelect = null;
            async function fetchEvents() {
                // Default LIMIT was 500; for a chatty play (lots of
                // ABR shifts) the latest events were silently truncated.
                // Forwarder caps at 5000 so use that here. Replay mode
                // wants the whole picture; the chip / type filters handle
                // visual density downstream.
                const p = new URLSearchParams({ session: sessionId, limit: '5000' });
                if (playId) p.set('play_id', playId);
                try {
                    const r = await fetch(`/analytics/api/session_events?${p.toString()}`);
                    if (!r.ok) return;
                    const t = await r.text();
                    const rows = t.split('\n').filter(l => l).map(l => {
                        try { return JSON.parse(l); } catch { return null; }
                    }).filter(Boolean);
                    sessionEvents = rows.map(e => {
                        const tsMs = Date.parse((e.ts || '').replace(' ', 'T') + 'Z');
                        const typeSty  = EVENT_TYPE_STYLE[e.type] || { label: e.type, icon: '•' };
                        const priority = Number(e.priority) || 4;
                        const kind     = e.kind === 'cause' ? 'cause' : 'effect';
                        const prSty = PRIORITY_STYLE[priority] || PRIORITY_STYLE[4];
                        const labelInfo = e.info ? ` ${e.info}` : '';
                        const offsetMs = Number.isFinite(tsMs) && Number.isFinite(sessionStartMs)
                            ? Math.max(0, tsMs - sessionStartMs) : 0;
                        const offsetStr = fmtDuration(offsetMs);
                        // Cause events get a hollow dot prefix in the dropdown
                        // so the user can distinguish "system did this" from
                        // "user saw this" at a glance. The dropdown's job is
                        // "jump to the next event of this type" — drop the
                        // per-instance info (5.2→1.8 Mbps, durations, error
                        // messages) so all downshifts read as one scannable
                        // group. The detail text still shows on the rail-
                        // marker hover tooltip via the `label` field.
                        const kindMark = kind === 'cause' ? '◇ ' : '';
                        return {
                            tsMs,
                            type: e.type,
                            priority,
                            kind,
                            color: prSty.color,
                            label: `${typeSty.label}${labelInfo} (T+${offsetStr})`,
                            optionLabel: `${kindMark}${typeSty.icon} ${typeSty.label} (T+${offsetStr})`
                        };
                    }).filter(e => Number.isFinite(e.tsMs));
                    populateEventsDropdown();
                    rebuildPriorityChips();
                    rebuildTicks();
                } catch (err) {
                    console.warn('session_events failed:', err);
                }
            }
            function populateEventsDropdown() {
                if (!eventsSelect || !sessionEvents) return;
                eventsSelect.replaceChildren();

                // Filter by enabled priorities AND enabled kinds; group by
                // priority via <optgroup> so the dropdown is scannable.
                const visible = sessionEvents
                    .map((e, i) => ({ e, i }))
                    .filter(({ e }) => priorityEnabled[e.priority] && kindEnabled[e.kind] && isEventTypeEnabled(e.type) && (uncategorizedEnabled || !isUncategorizedType(e.type)));
                const placeholder = document.createElement('option');
                placeholder.value = '';
                placeholder.textContent = sessionEvents.length === 0
                    ? '✓ Clean playback — no notable events'
                    : visible.length === 0
                        ? `${sessionEvents.length} events — all hidden by priority filter`
                        : `Jump to event (${visible.length} of ${sessionEvents.length})…`;
                eventsSelect.appendChild(placeholder);

                if (visible.length === 0) {
                    eventsSelect.disabled = sessionEvents.length === 0;
                    return;
                }

                // Group visible events by priority for the optgroup labels.
                const byPriority = new Map();
                for (const v of visible) {
                    if (!byPriority.has(v.e.priority)) byPriority.set(v.e.priority, []);
                    byPriority.get(v.e.priority).push(v);
                }
                const order = [1, 2, 3, 4];
                for (const p of order) {
                    const group = byPriority.get(p);
                    if (!group || group.length === 0) continue;
                    const og = document.createElement('optgroup');
                    og.label = `${PRIORITY_STYLE[p].label} (${group.length})`;
                    for (const { e, i } of group) {
                        const opt = document.createElement('option');
                        opt.value = String(i);
                        opt.textContent = e.optionLabel;
                        og.appendChild(opt);
                    }
                    eventsSelect.appendChild(og);
                }
                eventsSelect.disabled = false;
            }

            // Priority chips above the events dropdown — toggleable filter
            // with per-priority counts. Click toggles visibility of that
            // priority class everywhere (dropdown + rail markers).
            let priorityChipsHost = null;
            let kindChipsHost = null;
            function rebuildKindChips() {
                if (!kindChipsHost) return;
                const counts = { effect: 0, cause: 0 };
                for (const e of (sessionEvents || [])) {
                    if (counts[e.kind] !== undefined) counts[e.kind]++;
                }
                const KIND_STYLE = {
                    effect: { label: 'Effects', color: '#1d4ed8', bg: '#dbeafe', border: '#93c5fd', tip: 'What the player or user saw — stalls, errors, bitrate changes' },
                    cause:  { label: 'Causes',  color: '#7c2d12', bg: '#fed7aa', border: '#fdba74', tip: 'Proxy / system actions that may or may not have produced a user-visible effect' }
                };
                kindChipsHost.replaceChildren();
                const heading = document.createElement('span');
                heading.style.cssText = 'font-size:11px;color:var(--text-secondary);font-weight:600;margin-right:4px;';
                heading.textContent = 'Show:';
                kindChipsHost.appendChild(heading);
                for (const k of ['effect', 'cause']) {
                    const sty = KIND_STYLE[k];
                    const enabled = !!kindEnabled[k];
                    const chip = document.createElement('button');
                    chip.type = 'button';
                    chip.title = sty.tip;
                    chip.style.cssText = [
                        'display:inline-flex','align-items:center','gap:6px',
                        'padding:3px 10px','border-radius:999px',
                        'font:600 11px system-ui',
                        `border:1px solid ${sty.border}`,
                        `background:${enabled ? sty.bg : 'transparent'}`,
                        `color:${enabled ? sty.color : 'var(--text-secondary)'}`,
                        `opacity:${counts[k] === 0 ? '0.45' : '1'}`,
                        `cursor:${counts[k] === 0 ? 'default' : 'pointer'}`,
                        'user-select:none'
                    ].join(';');
                    const dot = document.createElement('span');
                    dot.style.cssText = `display:inline-block;width:8px;height:8px;border-radius:50%;background:${sty.color};${enabled ? '' : 'opacity:0.3;'}`;
                    chip.appendChild(dot);
                    chip.appendChild(document.createTextNode(` ${counts[k]} ${sty.label}`));
                    if (counts[k] > 0) {
                        chip.addEventListener('click', () => {
                            kindEnabled[k] = !kindEnabled[k];
                            // Don't let both go off at once — leave one on.
                            if (!kindEnabled.effect && !kindEnabled.cause) kindEnabled[k === 'effect' ? 'cause' : 'effect'] = true;
                            saveKindFilter();
                            rebuildPriorityChips();
                            populateEventsDropdown();
                            rebuildTicks();
                        });
                    }
                    kindChipsHost.appendChild(chip);
                }
            }
            function rebuildPriorityChips() {
                rebuildKindChips();
                if (!priorityChipsHost) return;
                // Counts respect the active kind filter — so when "causes"
                // is off, the chip counts reflect only effects, matching
                // what's in the dropdown.
                const counts = {1:0, 2:0, 3:0, 4:0};
                let uncategorizedCount = 0;
                for (const e of (sessionEvents || [])) {
                    if (!kindEnabled[e.kind]) continue;
                    if (counts[e.priority] !== undefined) counts[e.priority]++;
                    if (isUncategorizedType(e.type)) uncategorizedCount++;
                }
                priorityChipsHost.replaceChildren();
                for (const p of [1, 2, 3, 4]) {
                    const sty = PRIORITY_STYLE[p];
                    const enabled = !!priorityEnabled[p];
                    const chip = document.createElement('button');
                    chip.type = 'button';
                    chip.style.cssText = [
                        'display:inline-flex', 'align-items:center', 'gap:6px',
                        'padding:3px 10px', 'border-radius:999px',
                        'font:600 11px system-ui',
                        `border:1px solid ${sty.border}`,
                        `background:${enabled ? sty.bg : 'transparent'}`,
                        `color:${enabled ? sty.color : 'var(--text-secondary)'}`,
                        `opacity:${counts[p] === 0 ? '0.45' : '1'}`,
                        `cursor:${counts[p] === 0 ? 'default' : 'pointer'}`,
                        'user-select:none'
                    ].join(';');
                    chip.title = `${sty.label} (priority ${p}) — click to ${enabled ? 'hide' : 'show'}`;
                    const dot = document.createElement('span');
                    dot.style.cssText = `display:inline-block;width:8px;height:8px;border-radius:50%;background:${sty.color};${enabled ? '' : 'opacity:0.3;'}`;
                    chip.appendChild(dot);
                    chip.appendChild(document.createTextNode(` ${counts[p]} ${sty.label}`));
                    if (counts[p] > 0) {
                        chip.addEventListener('click', () => {
                            priorityEnabled[p] = !priorityEnabled[p];
                            savePriorityFilter();
                            rebuildPriorityChips();
                            populateEventsDropdown();
                            rebuildTicks();
                        });
                        // Right-click → per-event-type popover so the
                        // user can disable individual types (e.g. hide
                        // upshifts but keep downshifts) without losing
                        // the whole priority class.
                        chip.addEventListener('contextmenu', (e) => {
                            e.preventDefault();
                            openTypeFilterPopover(chip, p);
                        });
                        chip.title += ' · right-click to toggle individual event types';
                    }
                    priorityChipsHost.appendChild(chip);
                }

                // Uncategorized chip — events whose `type` isn't in our
                // EVENT_TYPE_STYLE table. Comes from the forwarder's
                // catch-all UNION ALL on last_event values we haven't
                // explicitly mapped. Toggle hides them everywhere.
                const uncStyle = { color:'#6b7280', bg:'#f3f4f6', border:'#d1d5db' };
                const uncEnabled = !!uncategorizedEnabled;
                const uncChip = document.createElement('button');
                uncChip.type = 'button';
                uncChip.style.cssText = [
                    'display:inline-flex','align-items:center','gap:6px',
                    'padding:3px 10px','border-radius:999px',
                    'font:600 11px system-ui',
                    `border:1px dashed ${uncStyle.border}`,
                    `background:${uncEnabled ? uncStyle.bg : 'transparent'}`,
                    `color:${uncEnabled ? uncStyle.color : 'var(--text-secondary)'}`,
                    `opacity:${uncategorizedCount === 0 ? '0.45' : '1'}`,
                    `cursor:${uncategorizedCount === 0 ? 'default' : 'pointer'}`,
                    'user-select:none'
                ].join(';');
                uncChip.title = `Uncategorized event types (player_emitted last_event values not in our taxonomy) — click to ${uncEnabled ? 'hide' : 'show'}`;
                const uncDot = document.createElement('span');
                uncDot.style.cssText = `display:inline-block;width:8px;height:8px;border-radius:50%;background:${uncStyle.color};${uncEnabled ? '' : 'opacity:0.3;'}`;
                uncChip.appendChild(uncDot);
                uncChip.appendChild(document.createTextNode(` ${uncategorizedCount} Uncategorized`));
                if (uncategorizedCount > 0) {
                    uncChip.addEventListener('click', () => {
                        uncategorizedEnabled = !uncategorizedEnabled;
                        saveUncategorizedFilter();
                        rebuildPriorityChips();
                        populateEventsDropdown();
                        rebuildTicks();
                    });
                    uncChip.addEventListener('contextmenu', (e) => {
                        // Right-click: open a popover listing the actual
                        // unrecognised type names so the user can toggle
                        // each one individually (e.g. ignore one new
                        // event type but keep watching the others).
                        e.preventDefault();
                        openUncategorizedPopover(uncChip);
                    });
                }
                priorityChipsHost.appendChild(uncChip);
            }

            // Right-click popover for the Uncategorized chip — same shape
            // as the priority chip popover but lists every unrecognised
            // type observed in this session.
            function openUncategorizedPopover(anchor) {
                closeTypeFilterPopover();
                const counts = new Map();
                for (const e of (sessionEvents || [])) {
                    if (!isUncategorizedType(e.type)) continue;
                    counts.set(e.type, (counts.get(e.type) || 0) + 1);
                }
                if (counts.size === 0) return;

                const pop = document.createElement('div');
                pop.style.cssText = [
                    'position:absolute','z-index:5000',
                    'background:var(--bg-primary,#fff)',
                    'border:1px dashed #d1d5db','border-radius:6px',
                    'box-shadow:0 10px 24px rgba(0,0,0,0.12), 0 2px 6px rgba(0,0,0,0.08)',
                    'padding:6px 0','min-width:240px',
                    'max-height:60vh','overflow:auto',
                    'font:13px system-ui','color:var(--text-primary)'
                ].join(';');

                const header = document.createElement('div');
                header.style.cssText = `padding:6px 12px;font:600 11px system-ui;letter-spacing:0.04em;color:#6b7280;border-bottom:1px solid var(--border-color,#e5e7eb);text-transform:uppercase;`;
                header.textContent = `Uncategorized · ${counts.size} type${counts.size === 1 ? '' : 's'}`;
                pop.appendChild(header);

                const setAll = (val) => {
                    for (const [t] of counts) eventTypeEnabled[t] = val;
                    saveTypeFilter();
                    rebuildPriorityChips();
                    populateEventsDropdown();
                    rebuildTicks();
                    refreshRows();
                };
                const allRow = document.createElement('div');
                allRow.style.cssText = 'display:flex;gap:6px;padding:6px 12px;border-bottom:1px solid var(--border-color,#f3f4f6);';
                for (const [label, val] of [['All on', true], ['All off', false]]) {
                    const b = document.createElement('button');
                    b.type = 'button'; b.textContent = label; b.className = 'btn btn-secondary';
                    b.style.cssText = 'padding:2px 8px;font:600 11px system-ui;';
                    b.addEventListener('click', () => setAll(val));
                    allRow.appendChild(b);
                }
                pop.appendChild(allRow);

                const rowsHost = document.createElement('div');
                pop.appendChild(rowsHost);
                const refreshRows = () => {
                    rowsHost.replaceChildren();
                    const sorted = Array.from(counts.entries()).sort((a, b) => b[1] - a[1]);
                    for (const [type, n] of sorted) {
                        const enabled = isEventTypeEnabled(type);
                        const row = document.createElement('label');
                        row.style.cssText = `display:flex;align-items:center;gap:8px;padding:5px 12px;cursor:pointer;${enabled ? '' : 'opacity:0.55;'}`;
                        row.addEventListener('mouseenter', () => row.style.background = 'var(--bg-hover,#f9fafb)');
                        row.addEventListener('mouseleave', () => row.style.background = '');
                        const cb = document.createElement('input');
                        cb.type = 'checkbox'; cb.checked = enabled;
                        cb.addEventListener('change', () => {
                            eventTypeEnabled[type] = cb.checked;
                            saveTypeFilter();
                            rebuildPriorityChips();
                            populateEventsDropdown();
                            rebuildTicks();
                            refreshRows();
                        });
                        const icon = document.createElement('span');
                        icon.textContent = '•';
                        icon.style.cssText = 'min-width:16px;text-align:center;color:#6b7280;';
                        const label = document.createElement('span');
                        label.textContent = type;
                        label.style.cssText = 'flex:1;font:500 12px ui-monospace,Menlo,monospace;';
                        const count = document.createElement('span');
                        count.textContent = String(n);
                        count.style.cssText = 'color:var(--text-secondary);font:600 11px ui-monospace,Menlo,monospace;min-width:32px;text-align:right;';
                        row.append(cb, icon, label, count);
                        rowsHost.appendChild(row);
                    }
                };
                refreshRows();

                document.body.appendChild(pop);
                const r = anchor.getBoundingClientRect();
                const pr = pop.getBoundingClientRect();
                let top = window.scrollY + r.bottom + 6;
                let left = window.scrollX + r.left;
                if (top + pr.height > window.scrollY + window.innerHeight - 8) {
                    top = window.scrollY + r.top - pr.height - 6;
                }
                if (left + pr.width > window.scrollX + window.innerWidth - 8) {
                    left = window.scrollX + window.innerWidth - pr.width - 8;
                }
                pop.style.top = top + 'px';
                pop.style.left = Math.max(8, left) + 'px';
                openPopover = pop;
                setTimeout(() => {
                    document.addEventListener('click', closeOnOutsideClick, true);
                    document.addEventListener('keydown', closeOnEsc, true);
                }, 0);
            }

            // The popover is a small floating menu anchored to the chip.
            // It lists every event TYPE observed at this priority in
            // the current session, with a checkbox per type. Toggling
            // any updates eventTypeEnabled and re-renders the dropdown
            // / rail / chip counts.
            let openPopover = null;
            function closeTypeFilterPopover() {
                if (openPopover && openPopover.parentNode) {
                    openPopover.parentNode.removeChild(openPopover);
                }
                openPopover = null;
                document.removeEventListener('click', closeOnOutsideClick, true);
                document.removeEventListener('keydown', closeOnEsc, true);
            }
            function closeOnOutsideClick(ev) {
                if (openPopover && !openPopover.contains(ev.target)) closeTypeFilterPopover();
            }
            function closeOnEsc(ev) {
                if (ev.key === 'Escape') closeTypeFilterPopover();
            }
            function openTypeFilterPopover(anchor, priority) {
                closeTypeFilterPopover();
                // Distinct event types observed at this priority,
                // grouped with their counts so the user sees what
                // they're toggling.
                const counts = new Map();
                for (const e of (sessionEvents || [])) {
                    if (e.priority !== priority) continue;
                    counts.set(e.type, (counts.get(e.type) || 0) + 1);
                }
                if (counts.size === 0) return;

                const sty = PRIORITY_STYLE[priority];
                const pop = document.createElement('div');
                pop.style.cssText = [
                    'position:absolute', 'z-index:5000',
                    'background:var(--bg-primary,#fff)',
                    `border:1px solid ${sty.border}`,
                    'border-radius:6px',
                    'box-shadow:0 10px 24px rgba(0,0,0,0.12), 0 2px 6px rgba(0,0,0,0.08)',
                    'padding:6px 0',
                    'min-width:220px',
                    'max-height:60vh', 'overflow:auto',
                    'font:13px system-ui',
                    'color:var(--text-primary)'
                ].join(';');

                const header = document.createElement('div');
                header.style.cssText = `padding:6px 12px 6px 12px;font:600 11px system-ui;letter-spacing:0.04em;color:${sty.color};border-bottom:1px solid var(--border-color,#e5e7eb);text-transform:uppercase;`;
                header.textContent = `${sty.label} · types`;
                pop.appendChild(header);

                // "All" / "None" quick-set row.
                const setAll = (val) => {
                    for (const [t] of counts) eventTypeEnabled[t] = val;
                    saveTypeFilter();
                    rebuildPriorityChips();
                    populateEventsDropdown();
                    rebuildTicks();
                    refreshRows();
                };
                const allRow = document.createElement('div');
                allRow.style.cssText = 'display:flex;gap:6px;padding:6px 12px;border-bottom:1px solid var(--border-color,#f3f4f6);';
                for (const [label, val] of [['All on', true], ['All off', false]]) {
                    const b = document.createElement('button');
                    b.type = 'button';
                    b.textContent = label;
                    b.className = 'btn btn-secondary';
                    b.style.cssText = 'padding:2px 8px;font:600 11px system-ui;';
                    b.addEventListener('click', () => setAll(val));
                    allRow.appendChild(b);
                }
                pop.appendChild(allRow);

                const refreshRows = () => {
                    // Re-render the row container in place rather than
                    // closing/reopening the popover.
                    rowsHost.replaceChildren();
                    const sorted = Array.from(counts.entries()).sort((a, b) => b[1] - a[1]);
                    for (const [type, n] of sorted) {
                        const enabled = isEventTypeEnabled(type);
                        const typeSty = EVENT_TYPE_STYLE[type] || { label: type, icon: '•' };
                        const row = document.createElement('label');
                        row.style.cssText = `display:flex;align-items:center;gap:8px;padding:5px 12px;cursor:pointer;${enabled ? '' : 'opacity:0.55;'}`;
                        row.addEventListener('mouseenter', () => row.style.background = 'var(--bg-hover,#f9fafb)');
                        row.addEventListener('mouseleave', () => row.style.background = '');
                        const cb = document.createElement('input');
                        cb.type = 'checkbox';
                        cb.checked = enabled;
                        cb.style.cssText = 'cursor:pointer;';
                        cb.addEventListener('change', () => {
                            eventTypeEnabled[type] = cb.checked;
                            saveTypeFilter();
                            rebuildPriorityChips();
                            populateEventsDropdown();
                            rebuildTicks();
                            refreshRows();
                        });
                        const icon = document.createElement('span');
                        icon.textContent = typeSty.icon;
                        icon.style.cssText = 'min-width:16px;text-align:center;color:' + sty.color + ';';
                        const label = document.createElement('span');
                        label.textContent = typeSty.label;
                        label.style.cssText = 'flex:1;';
                        const count = document.createElement('span');
                        count.textContent = String(n);
                        count.style.cssText = 'color:var(--text-secondary);font:600 11px ui-monospace,Menlo,monospace;min-width:32px;text-align:right;';
                        row.append(cb, icon, label, count);
                        rowsHost.appendChild(row);
                    }
                };
                const rowsHost = document.createElement('div');
                pop.appendChild(rowsHost);
                refreshRows();

                // Position next to the chip (below by default; if it
                // would clip the viewport bottom, flip above).
                document.body.appendChild(pop);
                const r = anchor.getBoundingClientRect();
                const pr = pop.getBoundingClientRect();
                let top = window.scrollY + r.bottom + 6;
                let left = window.scrollX + r.left;
                if (top + pr.height > window.scrollY + window.innerHeight - 8) {
                    top = window.scrollY + r.top - pr.height - 6;
                }
                if (left + pr.width > window.scrollX + window.innerWidth - 8) {
                    left = window.scrollX + window.innerWidth - pr.width - 8;
                }
                pop.style.top = top + 'px';
                pop.style.left = Math.max(8, left) + 'px';
                openPopover = pop;
                // Defer outside-click registration to next tick so this
                // contextmenu's bubbling doesn't immediately close it.
                setTimeout(() => {
                    document.addEventListener('click', closeOnOutsideClick, true);
                    document.addEventListener('keydown', closeOnEsc, true);
                }, 0);
            }

            // Push the current brush window down to the network log so its
            // waterfall fold filters to the same time range. The waterfall
            // re-renders only if its fold is open; otherwise the brush
            // state is stashed and the next open picks it up.
            const syncNetworkLogToBrush = () => {
                const TUI = window.TestingSessionUI;
                if (!TUI || typeof TUI.setNetworkLogTimeRange !== 'function') return;
                if (!Number.isFinite(brushStart) || !Number.isFinite(brushEnd)) return;
                if (brushEnd <= brushStart) return;
                TUI.setNetworkLogTimeRange(sessionId, brushStart, brushEnd);
            };

            let renderToken = 0;
            const triggerRender = async () => {
                const myToken = ++renderToken;
                const winMs = brushEnd - brushStart;
                updateWindowText('rendering…');
                syncNetworkLogToBrush();
                const done = await replayWindow(sessionId, snapshots, brushEnd, winMs, (i, n) => {
                    if (myToken !== renderToken) return;
                    updateWindowText(`${i}/${n}`);
                });
                if (myToken !== renderToken) return;
                updateWindowText(`${done} rendered`);
            };
            let renderDebounce;
            const scheduleRender = () => {
                clearTimeout(renderDebounce);
                renderDebounce = setTimeout(triggerRender, 180);
            };

            const buildBrushUI = () => {
                scrubRow.innerHTML = '';
                scrubRow.style.cssText = 'display:flex;flex-direction:column;gap:8px;margin-top:10px;font:500 12px system-ui;color:var(--text-secondary);';

                const topControls = document.createElement('div');
                topControls.style.cssText = 'display:flex;align-items:center;gap:10px;';

                const jumpStart = document.createElement('button');
                jumpStart.type = 'button';
                jumpStart.textContent = '⏮';
                jumpStart.title = 'Jump to start of session';
                const btnCss = 'background:var(--bg-primary);color:var(--text-primary);border:1px solid var(--border-color, #d1d5db);padding:4px 10px;border-radius:4px;font:600 12px system-ui;cursor:pointer;flex-shrink:0;';
                jumpStart.style.cssText = btnCss;
                const jumpEnd = document.createElement('button');
                jumpEnd.type = 'button';
                jumpEnd.textContent = '⏭';
                jumpEnd.title = 'Jump to end of session';
                jumpEnd.style.cssText = btnCss;

                const overviewWrap = document.createElement('div');
                overviewWrap.style.cssText = 'flex:1;min-width:300px;position:relative;';
                overviewEl = document.createElement('div');
                overviewEl.className = 'netwf-overview';
                overviewEl.style.cssText = 'background:var(--bg-secondary,#f3f4f6);height:44px;border:1px solid var(--border-color,#e5e7eb);border-radius:6px;position:relative;cursor:crosshair;user-select:none;overflow:hidden;';
                ticksEl = document.createElement('div');
                ticksEl.className = 'netwf-overview-bars';
                ticksEl.style.cssText = 'position:absolute;inset:3px 0;pointer-events:none;';
                // Markers live on a separate layer that paints on top
                // of the brush so hover/click on event markers always
                // wins over the brush's drag handler. Layer itself is
                // pointer-events:none; only individual marker elements
                // opt back in via pointer-events:auto, leaving the
                // brush draggable everywhere except the ~10 px hit
                // zones around each marker.
                markersEl = document.createElement('div');
                markersEl.className = 'netwf-overview-markers';
                markersEl.style.cssText = 'position:absolute;inset:3px 0;pointer-events:none;z-index:5;';
                // Brush is the visible selection box — beefed up so it
                // unambiguously tells the user "this is the time window
                // you're looking at." Outer ring (box-shadow) + thicker
                // borders + slightly higher fill alpha + a thin glow
                // halo ensure it pops over heatmap colors.
                brushEl = document.createElement('div');
                brushEl.className = 'netwf-brush';
                brushEl.style.cssText = [
                    'position:absolute',
                    'top:-2px',
                    'bottom:-2px',
                    'background:rgba(37,99,235,0.20)',
                    'border:2px solid #1d4ed8',
                    'border-radius:4px',
                    'box-shadow:0 0 0 1px rgba(255,255,255,0.6), 0 4px 12px rgba(29,78,216,0.35)',
                    'cursor:grab',
                    'box-sizing:border-box',
                    'z-index:2',
                ].join(';');
                leftHandle = document.createElement('div');
                leftHandle.className = 'netwf-brush-handle left';
                leftHandle.style.cssText = 'position:absolute;top:0;bottom:0;left:-5px;width:10px;background:#1d4ed8;border-radius:2px;cursor:ew-resize;box-shadow:0 0 0 1px rgba(255,255,255,0.7);';
                rightHandle = document.createElement('div');
                rightHandle.className = 'netwf-brush-handle right';
                rightHandle.style.cssText = 'position:absolute;top:0;bottom:0;right:-5px;width:10px;background:#1d4ed8;border-radius:2px;cursor:ew-resize;box-shadow:0 0 0 1px rgba(255,255,255,0.7);';
                brushEl.append(leftHandle, rightHandle);
                overviewEl.append(ticksEl, brushEl, markersEl);
                overviewWrap.append(overviewEl);

                // Tick labels under the rail anchor the user in absolute
                // time. Three labels: session start, brush midpoint
                // (windowText), session end. The midpoint is positioned
                // by left% so it tracks the brush and the rail width
                // never reflows due to text length changes.
                const railLabels = document.createElement('div');
                railLabels.style.cssText = 'position:relative;height:18px;margin-top:4px;font:500 11px ui-monospace,Menlo,monospace;color:var(--text-secondary);user-select:none;';
                const startLabel = document.createElement('span');
                startLabel.style.cssText = 'position:absolute;left:0;top:0;white-space:nowrap;';
                const endLabel = document.createElement('span');
                endLabel.style.cssText = 'position:absolute;right:0;top:0;white-space:nowrap;';
                windowText = document.createElement('span');
                windowText.style.cssText = 'position:absolute;top:0;transform:translateX(-50%);white-space:nowrap;color:var(--text-primary);font-weight:600;background:var(--bg-primary);padding:0 6px;border-radius:3px;';
                railLabels.append(startLabel, windowText, endLabel);
                overviewWrap.appendChild(railLabels);

                topControls.append(jumpStart, overviewWrap, jumpEnd);
                scrubRow.appendChild(topControls);

                // Stash the rail label refs so updateWindowText can position
                // the brush-anchored label without re-finding them.
                scrubRow._startLabel = startLabel;
                scrubRow._endLabel   = endLabel;

                // Kind toggle (Effects / Causes) — what the user saw vs
                // what the proxy/system did. Effects on by default; causes
                // off so triage isn't drowned in operator-injected noise.
                kindChipsHost = document.createElement('div');
                kindChipsHost.style.cssText = 'display:flex;flex-wrap:wrap;gap:6px;align-items:center;';
                scrubRow.appendChild(kindChipsHost);

                // Priority chip row sits above the dropdown. Toggling a
                // chip filters both the dropdown and the rail markers.
                priorityChipsHost = document.createElement('div');
                priorityChipsHost.style.cssText = 'display:flex;flex-wrap:wrap;gap:6px;align-items:center;';
                scrubRow.appendChild(priorityChipsHost);
                rebuildPriorityChips();

                // Event jump-list: dropdown of notable events (stalls,
                // errors, fault toggles, ABR downshifts). Selecting one
                // centers the brush window on that event ±half-window.
                const eventsRow = document.createElement('div');
                eventsRow.style.cssText = 'display:flex;align-items:center;gap:10px;';
                const eventsLbl = document.createElement('span');
                eventsLbl.textContent = 'Events:';
                eventsLbl.style.cssText = 'font-size:12px;color:var(--text-secondary);';
                eventsSelect = document.createElement('select');
                eventsSelect.style.cssText = 'padding:4px 8px;font:13px system-ui;background:var(--bg-primary);color:var(--text-primary);border:1px solid var(--border-color,#d1d5db);border-radius:4px;flex:1;min-width:200px;';
                const placeholder = document.createElement('option');
                placeholder.value = ''; placeholder.textContent = 'Loading events…';
                eventsSelect.appendChild(placeholder);
                eventsSelect.addEventListener('change', () => {
                    const idx = Number(eventsSelect.value);
                    if (!Number.isFinite(idx)) return;
                    selectEvent(idx);
                });
                eventsRow.append(eventsLbl, eventsSelect);
                scrubRow.appendChild(eventsRow);
                if (sessionEvents) populateEventsDropdown();

                let drag = null;
                const pxPerMs = () => overviewEl.clientWidth / Math.max(1, sessionDurationMs);
                const startDrag = (e, mode) => {
                    drag = { mode, startX: e.clientX, startStart: brushStart, startEnd: brushEnd };
                    brushEl.classList.add('dragging');
                    // Tell ambient timers to back off mid-drag — the
                    // network-log auto-refresh otherwise flickers the
                    // waterfall every 1.5s while the user is actively
                    // moving the brush.
                    brushDraggingBySession.add(String(sessionId));
                    e.preventDefault(); e.stopPropagation();
                };
                const onMove = (e) => {
                    if (!drag) return;
                    const dx = e.clientX - drag.startX;
                    const dms = dx / pxPerMs();
                    const span = drag.startEnd - drag.startStart;
                    if (drag.mode === 'pan') {
                        let newStart = drag.startStart + dms;
                        let newEnd = newStart + span;
                        if (newStart < sessionStartMs) { newStart = sessionStartMs; newEnd = newStart + span; }
                        if (newEnd > sessionEndMs)     { newEnd = sessionEndMs; newStart = newEnd - span; }
                        brushStart = newStart; brushEnd = newEnd;
                    } else if (drag.mode === 'resize-left') {
                        let newStart = drag.startStart + dms;
                        if (newStart < sessionStartMs) newStart = sessionStartMs;
                        if (newStart > drag.startEnd - MIN_WINDOW_MS) newStart = drag.startEnd - MIN_WINDOW_MS;
                        brushStart = newStart;
                    } else if (drag.mode === 'resize-right') {
                        let newEnd = drag.startEnd + dms;
                        if (newEnd > sessionEndMs) newEnd = sessionEndMs;
                        if (newEnd < drag.startStart + MIN_WINDOW_MS) newEnd = drag.startStart + MIN_WINDOW_MS;
                        brushEnd = newEnd;
                    }
                    userMovedBrush = true;
                    updateBrushVisuals();
                    updateWindowText('…');
                    // Don't render mid-drag — just update the visible
                    // brush position. Charts re-render on mouseup so
                    // the user gets one snappy update at release time
                    // instead of debounced flicker through the gesture.
                };
                const onUp = () => {
                    if (!drag) return;
                    drag = null;
                    brushEl.classList.remove('dragging');
                    brushDraggingBySession.delete(String(sessionId));
                    // Single render at release time. No `schedule` —
                    // we want the chart to update immediately, not in
                    // 180ms.
                    triggerRender();
                };

                brushEl.addEventListener('mousedown', (e) => {
                    if (e.target.classList.contains('netwf-brush-handle')) return;
                    startDrag(e, 'pan');
                });
                leftHandle.addEventListener('mousedown', (e) => startDrag(e, 'resize-left'));
                rightHandle.addEventListener('mousedown', (e) => startDrag(e, 'resize-right'));
                overviewEl.addEventListener('mousedown', (e) => {
                    if (e.target !== overviewEl && e.target !== ticksEl) return;
                    const rect = overviewEl.getBoundingClientRect();
                    if (rect.width <= 0) return;
                    const fracX = (e.clientX - rect.left) / rect.width;
                    const targetMs = sessionStartMs + fracX * sessionDurationMs;
                    const span = brushEnd - brushStart;
                    let newStart = targetMs - span / 2;
                    let newEnd = newStart + span;
                    if (newStart < sessionStartMs) { newStart = sessionStartMs; newEnd = newStart + span; }
                    if (newEnd > sessionEndMs)     { newEnd = sessionEndMs; newStart = newEnd - span; }
                    brushStart = newStart; brushEnd = newEnd;
                    userMovedBrush = true;
                    updateBrushVisuals();
                    updateWindowText('…');
                    scheduleRender();
                });
                document.addEventListener('mousemove', onMove);
                document.addEventListener('mouseup', onUp);

                // Reverse-sync: when the network-log fold's own brush
                // moves (drag, resize, click-to-recenter), reflect the
                // change back onto the main session-viewer brush so
                // both rails always stay in lockstep. live:true =
                // mid-drag (visual update only); live:false = release
                // (full chart re-render). The main → network log
                // direction is already handled in syncNetworkLogToBrush.
                document.addEventListener('replay:brush-range-change', (ev) => {
                    if (!ev || !ev.detail) return;
                    if (String(ev.detail.sessionId) !== String(sessionId)) return;
                    if (ev.detail.source !== 'network-log') return;
                    const startMs = Number(ev.detail.startMs);
                    const endMs   = Number(ev.detail.endMs);
                    if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) return;
                    brushStart = startMs;
                    brushEnd   = endMs;
                    userMovedBrush = true;
                    updateBrushVisuals();
                    updateWindowText(ev.detail.live ? '…' : 'rendering…');
                    if (!ev.detail.live) {
                        triggerRender();
                    }
                });

                // Alt/Option + wheel = zoom the selection window around
                // the cursor position, mirroring the bitrate chart's
                // chartjs-plugin-zoom behaviour. passive:false because we
                // preventDefault so the page doesn't scroll while zooming.
                const ZOOM_STEP = 1.18;
                overviewEl.addEventListener('wheel', (e) => {
                    if (!e.altKey) return;
                    e.preventDefault();
                    const rect = overviewEl.getBoundingClientRect();
                    if (rect.width <= 0) return;
                    const fracX = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
                    const cursorMs = sessionStartMs + fracX * sessionDurationMs;
                    const span = Math.max(1, brushEnd - brushStart);
                    // Where in the current window does the cursor sit?
                    // Preserve that fraction so the cursor stays parked
                    // at the same on-screen pixel through the zoom.
                    const cursorWindowFrac = Math.max(0, Math.min(1, (cursorMs - brushStart) / span));
                    const zoomIn = e.deltaY < 0;
                    let newSpan = zoomIn ? span / ZOOM_STEP : span * ZOOM_STEP;
                    if (newSpan < MIN_WINDOW_MS) newSpan = MIN_WINDOW_MS;
                    if (newSpan > sessionDurationMs) newSpan = sessionDurationMs;
                    let newStart = cursorMs - cursorWindowFrac * newSpan;
                    let newEnd = newStart + newSpan;
                    if (newStart < sessionStartMs) { newStart = sessionStartMs; newEnd = newStart + newSpan; }
                    if (newEnd > sessionEndMs)     { newEnd = sessionEndMs; newStart = newEnd - newSpan; }
                    brushStart = newStart; brushEnd = newEnd;
                    userMovedBrush = true;
                    updateBrushVisuals();
                    updateWindowText('…');
                    scheduleRender();
                }, { passive: false });

                jumpStart.addEventListener('click', () => {
                    const span = brushEnd - brushStart;
                    brushStart = sessionStartMs;
                    brushEnd = Math.min(sessionEndMs, brushStart + span);
                    userMovedBrush = true;
                    updateBrushVisuals();
                    triggerRender();
                });
                jumpEnd.addEventListener('click', () => {
                    const span = brushEnd - brushStart;
                    brushEnd = sessionEndMs;
                    brushStart = Math.max(sessionStartMs, brushEnd - span);
                    userMovedBrush = true;
                    updateBrushVisuals();
                    triggerRender();
                });
            };

            // Stream the NDJSON response. Each line is a JSON object with
            // {ts, session_json}. Most-recent first (order=desc), so the
            // FIRST batch holds the end-of-session window; we render that
            // immediately. Older batches arrive next, extend the brush
            // overview and (only) re-render if the user is parked on the
            // earliest part of the session.
            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let leftover = '';
            let totalLines = 0;
            let bytesIn = 0;
            let firstBatchSeen = false;
            text.textContent = `session ${sessionId} — streaming…`;

            const ingestLine = (line) => {
                let row;
                try { row = JSON.parse(line); } catch { return; }
                let snap;
                try { snap = JSON.parse(row.session_json || '{}'); } catch { return; }
                if (!snap || typeof snap !== 'object') return;
                const tsMs = Date.parse((row.ts || '').replace(' ', 'T') + 'Z');
                if (!Number.isFinite(tsMs)) return;
                // O(1) push regardless of stream direction. The array is
                // intentionally NOT kept sorted — that was the O(N²) tax
                // of reverse streaming. Nothing in the brush/rail/header
                // path needs ordering; replayWindow sorts its small
                // filtered subset before feeding the chart.
                snapshots.push({ tsMs, snap });
                if (tsMs < observedStartMs) observedStartMs = tsMs;
                if (tsMs > observedEndMs)   observedEndMs   = tsMs;
                totalLines++;
            };

            // Bounds are maintained inline in ingestLine (O(1) per line).
            const onBatchBoundary = async () => {
                if (snapshots.length === 0) return;
                sessionStartMs = observedStartMs;
                sessionEndMs   = observedEndMs;
                sessionDurationMs = sessionEndMs - sessionStartMs;
                // Publish play bounds for other modules (network log)
                // so they can scope their fetch to this play instead
                // of pulling all rows for the session_id.
                playBoundsBySession.set(String(sessionId), {
                    startMs: sessionStartMs,
                    endMs:   sessionEndMs
                });
                if (!firstBatchSeen) {
                    firstBatchSeen = true;
                    buildBrushUI();
                    brushEnd = sessionEndMs;
                    brushStart = Math.max(sessionStartMs, brushEnd - Math.min(WINDOW_MS, sessionDurationMs || WINDOW_MS));
                    updateBrushVisuals();
                    updateWindowText('first paint…');
                    setHeader(`streaming…`);
                    rebuildTicks();
                    // Kick off the events query in parallel with the rest
                    // of the snapshot stream. The heatmap cells used to be
                    // a separate /api/session_heatmap call but are now
                    // derived directly from sessionEvents, so toggling
                    // chip filters recolors the rail in real time.
                    fetchEvents();
                    await triggerRender();
                } else {
                    rebuildTicks();
                    setHeader(`streaming…`);
                    // If user hasn't moved the brush, keep it pinned to
                    // the new end-of-session window. If they have, leave
                    // it alone.
                    if (!userMovedBrush) {
                        brushEnd = sessionEndMs;
                        brushStart = Math.max(sessionStartMs, brushEnd - Math.min(WINDOW_MS, sessionDurationMs || WINDOW_MS));
                        updateBrushVisuals();
                    }
                    updateWindowText('streaming…');
                    // Re-render charts as more (older) snapshots arrive so
                    // the brush window fills in from right→left rather than
                    // freezing on the first batch's slice. Debounced so a
                    // dense stream doesn't thrash Chart.js.
                    scheduleRender();
                }
                updateProgress();
            };

            // Periodic ticker so the progress text stays fresh between
            // batch boundaries (rate / KB / ETA).
            const progressTicker = setInterval(updateProgress, 250);

            try {
                while (true) {
                    const { done, value } = await reader.read();
                    if (done) break;
                    bytesIn += value.byteLength;
                    leftover += decoder.decode(value, { stream: true });
                    let nl;
                    while ((nl = leftover.indexOf('\n')) >= 0) {
                        const line = leftover.slice(0, nl);
                        leftover = leftover.slice(nl + 1);
                        if (line.length > 0) ingestLine(line);
                        if (totalLines > 0 && totalLines % BATCH_SIZE === 0) {
                            await onBatchBoundary();
                        }
                    }
                }
                if (leftover.length > 0) ingestLine(leftover);
                await onBatchBoundary();
            } catch (err) {
                clearInterval(progressTicker);
                failProgress(`stream error: ${err.message}`);
                fail(`Replay stream error: ${err.message}`);
                return;
            }

            clearInterval(progressTicker);
            if (snapshots.length === 0) { failProgress('no data'); fail(`No snapshots archived for session ${sessionId}`); return; }
            setHeader(`${snapshots.length} snapshots cached`);
            updateWindowText('done');
            finishProgress('complete');
            // Final render: any straggler snapshots since the last batch
            // boundary may not have triggered a render yet, and rebuildTicks
            // needs the heatmap (which arrives async) to overlay correctly.
            rebuildTicks();
            scheduleRender();

            // Live-tail mode: if the session is still being broadcast on
            // the SSE stream, new snapshots will keep arriving. Poll for
            // them and append; auto-advance the brush *only* if it was
            // anchored at the end of the session before the new data
            // arrived (so a user who's parked the brush back in time
            // doesn't get yanked forward).
            startLiveTail();

            function startLiveTail() {
                // The live-tail badge is mounted *into the banner* (next to
                // the back-to-sessions row) so it's discoverable from the
                // moment the page loads. It starts in "polling…" state and
                // graduates to "LIVE" the first time the loop sees fresh
                // data. After ~3 empty polls (~12s of silence) it switches
                // to "ended" state and stops polling.
                const liveBadge = document.createElement('span');
                liveBadge.id = 'replay-live-badge';
                liveBadge.style.cssText = 'display:inline-flex;align-items:center;gap:6px;padding:2px 8px;border-radius:10px;background:#e5e7eb;color:#4b5563;font:600 11px system-ui;letter-spacing:0.04em;margin-left:8px;';
                const setBadge = (state, label) => {
                    const colors = {
                        pending: ['#e5e7eb', '#4b5563', '#9ca3af', false],
                        live:    ['#dcfce7', '#166534', '#16a34a', true],
                        ended:   ['#fee2e2', '#991b1b', '#dc2626', false]
                    }[state] || ['#e5e7eb', '#4b5563', '#9ca3af', false];
                    const [bg, fg, dot, pulse] = colors;
                    liveBadge.style.background = bg;
                    liveBadge.style.color = fg;
                    liveBadge.innerHTML = `<span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:${dot};${pulse ? 'animation:replay-live-pulse 1.2s ease-in-out infinite;' : ''}"></span> ${label}`;
                };
                setBadge('pending', 'polling…');
                // Mount as the LAST element in the banner's left cluster so
                // it sits next to the session header text.
                if (text && text.parentElement) {
                    text.parentElement.appendChild(liveBadge);
                } else {
                    banner.appendChild(liveBadge);
                }
                console.debug('[replay] live-tail starting; cursor=', new Date(sessionEndMs).toISOString());
                if (!document.getElementById('replay-live-pulse-keyframes')) {
                    const style = document.createElement('style');
                    style.id = 'replay-live-pulse-keyframes';
                    style.textContent = '@keyframes replay-live-pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.35; } }';
                    document.head.appendChild(style);
                }

                const POLL_INTERVAL_MS = 3000;
                const HEATMAP_REFRESH_EVERY = 6;        // ~18s
                const END_TOLERANCE_MS = 1500;          // brush "at end" if within this many ms of sessionEnd
                const QUIESCE_AFTER_EMPTY_POLLS = 4;    // ~12s of silence ⇒ stop polling

                let cursorMs = sessionEndMs;
                let pollIdx = 0;
                let emptyStreak = 0;
                let stopped = false;
                let activeFetch = null;

                async function tick() {
                    if (stopped) return;
                    pollIdx++;
                    const sinceIso = new Date(cursorMs + 1).toISOString();
                    const params = new URLSearchParams({
                        session: sessionId,
                        order: 'asc',
                        limit: '5000',
                        from: sinceIso
                    });
                    if (playId) params.set('play_id', playId);
                    const url = '/analytics/api/snapshots?' + params.toString();
                    let resp;
                    try {
                        activeFetch = fetch(url);
                        resp = await activeFetch;
                    } catch (err) {
                        console.warn('[replay] live-tail fetch failed:', err);
                        scheduleNext();
                        return;
                    } finally {
                        activeFetch = null;
                    }
                    if (!resp.ok) {
                        console.warn('[replay] live-tail HTTP', resp.status, 'for', url);
                        scheduleNext();
                        return;
                    }
                    const body = await resp.text();
                    const lines = body.split('\n').filter(l => l);
                    console.debug('[replay] live-tail tick', pollIdx, 'got', lines.length, 'rows since', sinceIso);
                    if (lines.length === 0) {
                        emptyStreak++;
                        if (emptyStreak >= QUIESCE_AFTER_EMPTY_POLLS) {
                            // Session has plausibly ended — switch the badge
                            // to "ended" and stop polling. Resume requires
                            // page reload.
                            setBadge('ended', 'session ended');
                            console.debug('[replay] live-tail quiescing after', emptyStreak, 'empty polls');
                            stopped = true;
                            return;
                        }
                        scheduleNext();
                        return;
                    }
                    emptyStreak = 0;

                    // Was the brush parked at the end before this batch?
                    // Compute *before* we mutate sessionEndMs so we know
                    // whether to keep tailing or stay parked.
                    const wasAtEnd = (sessionEndMs - brushEnd) <= END_TOLERANCE_MS;
                    const prevWindowSpan = brushEnd - brushStart;

                    // Parse new rows, push to the snapshot array, advance
                    // cursor + bounds. Collect into newRows for the
                    // incremental render below.
                    const newRows = [];
                    for (const line of lines) {
                        let row;
                        try { row = JSON.parse(line); } catch { continue; }
                        let snap;
                        try { snap = JSON.parse(row.session_json || '{}'); } catch { continue; }
                        if (!snap || typeof snap !== 'object') continue;
                        const tsMs = Date.parse((row.ts || '').replace(' ', 'T') + 'Z');
                        if (!Number.isFinite(tsMs)) continue;
                        if (tsMs <= cursorMs) continue; // dedup against the floor
                        snapshots.push({ tsMs, snap });
                        if (tsMs > observedEndMs) observedEndMs = tsMs;
                        if (tsMs > cursorMs) cursorMs = tsMs;
                        totalLines++;
                        newRows.push({ tsMs, snap });
                    }
                    sessionEndMs = observedEndMs;
                    sessionDurationMs = sessionEndMs - sessionStartMs;
                    setBadge('live', `LIVE · ${snapshots.length} events`);

                    if (wasAtEnd) {
                        brushEnd = sessionEndMs;
                        brushStart = Math.max(sessionStartMs, brushEnd - prevWindowSpan);
                    }

                    setHeader('streaming live');
                    updateBrushVisuals();
                    rebuildTicks();

                    // Smooth incremental render: instead of replayWindow
                    // (which clears all chart state and re-feeds the whole
                    // 10-min window through pushBandwidthSample on every
                    // tick — expensive), feed only the NEW snapshots and
                    // let Chart.js update its datasets in place. This is
                    // exactly what testing.html does on each SSE pulse.
                    //
                    // Only do this when the brush was at the end. If the
                    // user has dragged back, the chart shows a different
                    // window and they'd see the new data flicker in/out;
                    // skip the chart push (data still records into the
                    // shell's history Maps). When they pan the brush back
                    // to the end, the existing brush handler triggers a
                    // full replayWindow that picks up everything.
                    if (wasAtEnd && newRows.length > 0) {
                        const sidStr = String(sessionId);
                        for (const r of newRows) {
                            sessionsById.set(sidStr, r.snap);
                            try {
                                pushBandwidthSample(r.snap);
                            } catch (err) {
                                console.warn('[replay] pushBandwidthSample failed:', err);
                            }
                        }
                        // One DOM update for session-details fields,
                        // events-timeline, etc. — applySessionsList does
                        // the heavy DOM render path; calling it once per
                        // tick (with the latest snap) is what testing.html
                        // also does per SSE event.
                        try {
                            applySessionsList([newRows[newRows.length - 1].snap]);
                        } catch (err) {
                            console.warn('[replay] applySessionsList failed:', err);
                        }
                    }

                    // Refresh events occasionally so the rail markers and
                    // bucket colors track real time. (Heatmap cells are
                    // now derived from sessionEvents — no separate fetch.)
                    if (pollIdx % HEATMAP_REFRESH_EVERY === 0) {
                        fetchEvents();
                    }
                    scheduleNext();
                }

                function scheduleNext() {
                    if (stopped) return;
                    setTimeout(tick, POLL_INTERVAL_MS);
                }

                // First tick fires immediately so a still-active session
                // shows the LIVE badge fast.
                tick();
            }
        }

        // Replay session picker: shown when ?replay=1 has no &session=<id>.
        // Lists recent archived sessions from the analytics forwarder so the
        // user can pick one without copy-pasting an id from Grafana.
        async function startReplayPicker() {
            // Render the picker as a standard dashboard panel so it matches
            // the rest of the app shell instead of using the amber "replay"
            // chrome (which only makes sense on the single-session viewer).
            const banner = document.createElement('div');
            banner.className = 'panel session-picker-panel';
            banner.style.cssText = 'font:13px system-ui;color:var(--text-primary);';
            banner.innerHTML = '<strong>Loading sessions…</strong>';
            const wide = document.querySelector('.ism-content-wide');
            if (wide) {
                const header = wide.querySelector('.page-header');
                if (header && header.nextSibling) wide.insertBefore(banner, header.nextSibling);
                else if (header) wide.appendChild(banner);
                else wide.insertBefore(banner, wide.firstChild);
            } else {
                const mp = mountPoint();
                mp.insertBefore(banner, mp.firstChild);
            }
            // The selector page is not actually a "replay" view — it's a
            // session index. Don't add replay-mode (which dims controls
            // intended for live testing) and don't prefix the page title.

            // Inject the live-pulse keyframes once if not already present
            // (they're also added by the session-viewer; either page may
            // load first depending on user navigation).
            if (!document.getElementById('replay-live-pulse-keyframes')) {
                const style = document.createElement('style');
                style.id = 'replay-live-pulse-keyframes';
                style.textContent = '@keyframes replay-live-pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.35; } }';
                document.head.appendChild(style);
            }

            // Splunk-style time-range presets. Stored in localStorage so the
            // selection persists across reloads. "Custom" exposes datetime-local
            // inputs and an Apply button.
            const RANGES = [
                { id: '15m',  label: 'Last 15 minutes', ms: 15 * 60 * 1000 },
                { id: '1h',   label: 'Last hour',       ms: 60 * 60 * 1000 },
                { id: '4h',   label: 'Last 4 hours',    ms: 4 * 60 * 60 * 1000 },
                { id: '24h',  label: 'Last 24 hours',   ms: 24 * 60 * 60 * 1000 },
                { id: '7d',   label: 'Last 7 days',     ms: 7 * 24 * 60 * 60 * 1000 },
                { id: '30d',  label: 'Last 30 days',    ms: 30 * 24 * 60 * 60 * 1000 },
                { id: 'all',  label: 'All time',        ms: 0 },
                { id: 'custom', label: 'Custom range…', ms: -1 }
            ];
            const RANGE_KEY = 'ismSessionsRange';
            const RANGE_CUSTOM_KEY = 'ismSessionsRangeCustom';
            let activeRangeId = localStorage.getItem(RANGE_KEY) || '24h';
            if (!RANGES.find(r => r.id === activeRangeId)) activeRangeId = '24h';
            const customStored = (() => {
                try { return JSON.parse(localStorage.getItem(RANGE_CUSTOM_KEY) || 'null'); }
                catch { return null; }
            })();
            let customFrom = customStored?.from || '';
            let customTo   = customStored?.to   || '';
            const computeRange = () => {
                if (activeRangeId === 'custom') {
                    return { since: customFrom || '', until: customTo || '', label: 'Custom range' };
                }
                const meta = RANGES.find(r => r.id === activeRangeId);
                if (!meta || meta.ms === 0) return { since: '1970-01-01T00:00:00Z', until: '', label: meta?.label || 'All time' };
                const since = new Date(Date.now() - meta.ms).toISOString();
                return { since, until: '', label: meta.label };
            };

            // Mutable rows array — replaced in place on time-range change so all
            // closures (refreshSelects, rebuildTable) see the new data.
            const rows = [];
            let rowsLoaded = false;

            async function loadRows() {
                const { since, until } = computeRange();
                const params = new URLSearchParams();
                if (since) params.set('since', since);
                if (until) params.set('until', until);
                const url = '/analytics/api/sessions' + (params.toString() ? '?' + params : '');
                let resp;
                try { resp = await fetch(url); }
                catch (err) {
                    banner.style.background = '#fee2e2'; banner.style.color = '#991b1b';
                    banner.textContent = `Picker failed: ${err.message}`;
                    return false;
                }
                if (!resp.ok) {
                    banner.style.background = '#fee2e2'; banner.style.color = '#991b1b';
                    banner.textContent = `Picker failed: HTTP ${resp.status} (analytics forwarder reachable?)`;
                    return false;
                }
                const body = await resp.text();
                const fresh = body.split('\n').filter(l => l).map(l => {
                    try { return JSON.parse(l); } catch { return null; }
                }).filter(Boolean);
                for (const r of fresh) {
                    const t0 = Date.parse(r.started);
                    const t1 = Date.parse(r.last_seen);
                    r.duration_ms = (Number.isFinite(t0) && Number.isFinite(t1) && t1 >= t0) ? (t1 - t0) : 0;
                    deriveHealth(r);
                }
                rows.splice(0, rows.length, ...fresh);
                rowsLoaded = true;
                return true;
            }

            // Derive synthetic per-session metrics shown as columns A/B/C in
            // the picker. The forwarder query returns raw counters; we roll
            // them up here so we can iterate on weights without redeploying
            // the forwarder.
            function deriveHealth(r) {
                const n = (k) => Number(r[k]) || 0;
                const stalls    = n('stalls');
                const drops     = n('dropped_frames');
                const downshifts = n('downshifts');
                const upshifts   = n('upshifts');
                const resChanges = n('resolution_changes');
                const errors    = (r.last_player_error && r.last_player_error.length > 0) ? 1 : 0;
                const faults    = n('master_manifest_failures') + n('manifest_failures') + n('segment_failures')
                                + n('all_failures') + n('transport_failures')
                                + n('active_timeouts') + n('idle_timeouts');
                // Drops are noisy — scale to "drop blocks" of 100 frames
                // so a session with 543 drops counts as 6, not 543.
                const dropBlocks = Math.ceil(drops / 100);

                r.errors_count = errors;
                r.faults_count = faults;
                r.downshifts_count = downshifts;
                r.upshifts_count   = upshifts;

                // Per-event "really bad things" surfacing — directly
                // from the forwarder's per-(session,play) aggregations.
                // Critical = anything that should make a triager stop
                // and look. user_marked / frozen / hard error are the
                // top signals; segment_stall / restart are second-tier
                // (recovery attempts that might not have succeeded).
                r.user_marked_count   = n('user_marked_count');
                r.frozen_count        = n('frozen_count');
                r.segment_stall_count = n('segment_stall_count');
                r.restart_count       = n('restart_count');
                r.error_event_count   = n('error_event_count');
                r.is_critical = (r.user_marked_count > 0)
                            || (r.frozen_count > 0)
                            || (r.error_event_count > 0)
                            || (errors > 0)
                            || (n('master_manifest_failures') > 0)
                            || (n('all_failures') > 0);

                // (A) Issues badge — single weighted total. Weights chosen so
                // a clean session is 0 and a session with one fatal error is
                // already in the red bucket.
                r.issues_count = stalls + errors * 5 + faults + downshifts + dropBlocks;
                r.issues_breakdown = {
                    stalls, errors, faults, downshifts, drops, dropBlocks,
                    resolution_changes: resChanges,
                    upshifts, player_error: r.last_player_error || ''
                };

                // (C) Health score — 100 minus weighted deductions. Faults
                // and drops are capped because they're either intentional
                // (faults are injected) or noisy (drops are per-frame).
                const deductStalls = stalls * 2;
                const deductErrors = errors * 25;
                const deductFaults = Math.min(faults * 1, 10);
                const deductDrops  = Math.min(dropBlocks, 20);
                const deductShifts = downshifts * 1;
                const total = deductStalls + deductErrors + deductFaults + deductDrops + deductShifts;
                r.health_score = Math.max(0, 100 - total);
                r.health_breakdown = {
                    stalls: deductStalls, errors: deductErrors,
                    faults: deductFaults, drops: deductDrops, downshifts: deductShifts
                };
            }

            const escapeHtml = (s) => String(s == null ? '' : s)
                .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
                .replace(/"/g, '&quot;').replace(/'/g, '&#39;');

            // (A) Color-coded badge cell for Issues count.
            const fmtIssuesBadge = (count, r) => {
                const c = Number(count) || 0;
                let bg, fg;
                if (c === 0)      { bg = '#d1fae5'; fg = '#065f46'; }
                else if (c <= 4)  { bg = '#fef3c7'; fg = '#92400e'; }
                else              { bg = '#fee2e2'; fg = '#991b1b'; }
                const b = r.issues_breakdown || {};
                const tipParts = [];
                if (b.stalls)      tipParts.push(`${b.stalls} stall${b.stalls === 1 ? '' : 's'}`);
                if (b.errors)      tipParts.push(`player error: ${b.player_error || 'yes'}`);
                if (b.faults)      tipParts.push(`${b.faults} injected fault${b.faults === 1 ? '' : 's'}`);
                if (b.downshifts)  tipParts.push(`${b.downshifts} ABR downshift${b.downshifts === 1 ? '' : 's'}`);
                if (b.drops)       tipParts.push(`${b.drops} dropped frames (~${b.dropBlocks} blocks)`);
                if (b.resolution_changes) tipParts.push(`${b.resolution_changes} resolution changes`);
                const tip = tipParts.length ? tipParts.join(' · ') : 'no noteworthy events';
                return `<span title="${escapeHtml(tip)}" style="display:inline-block;min-width:28px;text-align:center;padding:2px 8px;border-radius:10px;background:${bg};color:${fg};font-weight:700;font-family:system-ui;">${c}</span>`;
            };

            // (B) Per-category numeric cell with simple thresholding.
            const fmtCount = (thresholds /* [warn, bad] */) => (v) => {
                const n = Number(v) || 0;
                let color = '#065f46';
                if (n >= thresholds[1]) color = '#991b1b';
                else if (n >= thresholds[0]) color = '#92400e';
                else if (n === 0)      color = '#9ca3af';
                return `<span style="color:${color};font-weight:${n === 0 ? '400' : '600'};">${n}</span>`;
            };

            // (D) Flags column — visible chip per "really bad" event
            // type so a row with a 911 / freeze / hard error stands
            // out without the user having to read every counter. Each
            // chip carries a count + tooltip; only chips with > 0 are
            // rendered. Empty cell when the session is clean.
            const FLAG_DEFS = [
                // [key, icon, label, color, severity]
                { key: 'user_marked_count',   icon: '🚨', label: '911 / user flag',     color: '#dc2626', severity: 1 },
                { key: 'frozen_count',        icon: '❄️', label: 'frozen',              color: '#7c3aed', severity: 1 },
                { key: 'error_event_count',   icon: '⛔', label: 'error event',         color: '#b91c1c', severity: 1 },
                { key: 'segment_stall_count', icon: '⏸',  label: 'segment stall',       color: '#c2410c', severity: 2 },
                { key: 'restart_count',       icon: '🔄', label: 'restart',             color: '#b45309', severity: 2 }
            ];
            const fmtFlags = (_, r) => {
                const chips = [];
                for (const f of FLAG_DEFS) {
                    const c = Number(r[f.key]) || 0;
                    if (c <= 0) continue;
                    const tip = `${c} ${f.label}${c === 1 ? '' : 's'}`;
                    chips.push(
                        `<span title="${escapeHtml(tip)}" style="display:inline-block;` +
                        `padding:1px 6px;margin:0 2px 0 0;border-radius:10px;` +
                        `background:${f.color};color:#fff;` +
                        `font:600 11px system-ui;line-height:1.4;">${f.icon} ${c}</span>`
                    );
                }
                return chips.join('') || '<span style="color:#9ca3af;">—</span>';
            };

            // (C) Health score 0-100 with green/amber/red.
            const fmtHealth = (score, r) => {
                const s = Number(score);
                let bg, fg;
                if (s >= 90)      { bg = '#d1fae5'; fg = '#065f46'; }
                else if (s >= 70) { bg = '#fef3c7'; fg = '#92400e'; }
                else              { bg = '#fee2e2'; fg = '#991b1b'; }
                const b = r.health_breakdown || {};
                const tip = `100 − stalls:${b.stalls||0} − errors:${b.errors||0} − faults:${b.faults||0} − drops:${b.drops||0} − downshifts:${b.downshifts||0}`;
                return `<span title="${escapeHtml(tip)}" style="display:inline-block;min-width:36px;text-align:center;padding:2px 8px;border-radius:4px;background:${bg};color:${fg};font-weight:700;">${s}</span>`;
            };

            const fmtPct = (v) => {
                const n = Number(v);
                if (!Number.isFinite(n) || n === 0) return '<span style="color:#9ca3af;">—</span>';
                let color = '#065f46';
                if (n < 60)      color = '#991b1b';
                else if (n < 85) color = '#92400e';
                return `<span style="color:${color};">${n.toFixed(1)}%</span>`;
            };

            if (!await loadRows()) return;

            // Cascading filter UI: Player → Group → Content → Play (id) →
            // Session row. Each select narrows the next; the table at the
            // bottom shows the rows that match all active filters.
            const distinct = (key) => Array.from(new Set(rows.map(r => r[key] || '').filter(v => v))).sort();
            const filters = { player_id: '', group_id: '', content_id: '', play_id: '' };
            const matches = (r) =>
                (!filters.player_id  || r.player_id  === filters.player_id) &&
                (!filters.group_id   || r.group_id   === filters.group_id) &&
                (!filters.content_id || r.content_id === filters.content_id) &&
                (!filters.play_id    || r.play_id    === filters.play_id);

            const wrap = document.createElement('div');
            wrap.style.cssText = 'display:flex;flex-direction:column;gap:10px;';
            const heading = document.createElement('div');
            heading.style.cssText = 'font-size:13px;color:var(--text-secondary);';
            const updateHeading = () => {
                const { label } = computeRange();
                heading.textContent = `${rows.length} playback episodes archived (${label.toLowerCase()}). Filter then pick one to replay.`;
            };
            updateHeading();

            // Time-range row: Splunk-style preset dropdown, plus custom-range
            // datetime inputs that show only when "Custom" is selected.
            const ctrlInputCss = 'padding:4px 8px;font:13px system-ui;background:var(--bg-primary);color:var(--text-primary);border:1px solid var(--border-color, #d1d5db);border-radius:4px;';
            const labelCss = 'display:flex;align-items:center;gap:6px;font-size:12px;color:var(--text-secondary);font-weight:500;';
            const rangeRow = document.createElement('div');
            rangeRow.style.cssText = 'display:flex;flex-wrap:wrap;gap:10px;align-items:center;';
            const rangeLbl = document.createElement('label');
            rangeLbl.style.cssText = labelCss;
            const rangeSel = document.createElement('select');
            rangeSel.style.cssText = ctrlInputCss;
            for (const r of RANGES) {
                const opt = document.createElement('option');
                opt.value = r.id; opt.textContent = r.label;
                if (r.id === activeRangeId) opt.selected = true;
                rangeSel.appendChild(opt);
            }
            rangeLbl.append('Time range:', rangeSel);
            rangeRow.appendChild(rangeLbl);

            const customWrap = document.createElement('span');
            customWrap.style.cssText = 'display:none;align-items:center;gap:6px;font-size:12px;color:var(--text-secondary);';
            const fromInput = document.createElement('input');
            fromInput.type = 'datetime-local';
            fromInput.style.cssText = ctrlInputCss;
            const toInput = document.createElement('input');
            toInput.type = 'datetime-local';
            toInput.style.cssText = ctrlInputCss;
            const applyBtn = document.createElement('button');
            applyBtn.type = 'button';
            applyBtn.className = 'btn btn-secondary';
            applyBtn.textContent = 'Apply';
            // datetime-local is local-naive; convert to UTC ISO so the user's
            // "2pm" means 2pm in their timezone, not 2pm UTC.
            const localToISO = (v) => {
                if (!v) return '';
                const d = new Date(v);
                return Number.isFinite(d.getTime()) ? d.toISOString() : '';
            };
            const isoToLocal = (iso) => {
                if (!iso) return '';
                const d = new Date(iso);
                if (!Number.isFinite(d.getTime())) return '';
                const pad = (n) => String(n).padStart(2, '0');
                return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
            };
            fromInput.value = isoToLocal(customFrom);
            toInput.value   = isoToLocal(customTo);
            customWrap.append('from', fromInput, 'to', toInput, applyBtn);
            rangeRow.appendChild(customWrap);

            const reloadingTag = document.createElement('span');
            reloadingTag.style.cssText = 'font-size:12px;color:var(--text-secondary);font-style:italic;display:none;';
            reloadingTag.textContent = 'Reloading…';
            rangeRow.appendChild(reloadingTag);

            const filterRow = document.createElement('div');
            filterRow.style.cssText = 'display:flex;flex-wrap:wrap;gap:10px;align-items:center;';

            const buildSelect = (key, label) => {
                const lbl = document.createElement('label');
                lbl.style.cssText = labelCss;
                const sel = document.createElement('select');
                sel.dataset.key = key;
                sel.style.cssText = ctrlInputCss;
                lbl.append(label + ':', sel);
                filterRow.appendChild(lbl);
                return sel;
            };
            const playerSel  = buildSelect('player_id',  'Player');
            const groupSel   = buildSelect('group_id',   'Group');
            const contentSel = buildSelect('content_id', 'Content');
            const playSel    = buildSelect('play_id',    'Play');
            const clearBtn = document.createElement('button');
            clearBtn.type = 'button';
            clearBtn.className = 'btn btn-secondary';
            clearBtn.textContent = 'Clear filters';
            filterRow.appendChild(clearBtn);

            const matchCount = document.createElement('span');
            matchCount.style.cssText = 'margin-left:auto;font-size:12px;color:var(--text-secondary);';
            filterRow.appendChild(matchCount);

            // Sessions list as a scrollable table.
            const tableWrap = document.createElement('div');
            tableWrap.style.cssText = 'max-height:60vh;overflow:auto;background:var(--bg-primary);border:1px solid var(--border-color, #e5e7eb);border-radius:6px;';
            const table = document.createElement('table');
            table.style.cssText = 'width:100%;border-collapse:collapse;font:12px ui-monospace,Menlo,monospace;';
            tableWrap.appendChild(table);
            wrap.append(heading, rangeRow, filterRow, tableWrap);
            banner.replaceChildren(wrap);

            const showCustomInputs = () => {
                customWrap.style.display = activeRangeId === 'custom' ? 'inline-flex' : 'none';
            };
            showCustomInputs();

            // Re-fetch from the forwarder and rebuild the table. `silent`
            // suppresses the "Reloading…" tag for ambient auto-refresh so
            // it doesn't flash every tick. A concurrent-reload guard
            // prevents overlapping fetches if the forwarder is slow.
            let reloadInFlight = false;
            async function reloadForRange(opts = {}) {
                if (reloadInFlight) return;
                reloadInFlight = true;
                if (!opts.silent) reloadingTag.style.display = 'inline';
                try {
                    const ok = await loadRows();
                    if (!ok) return;
                    updateHeading();
                    refreshSelects();
                } finally {
                    if (!opts.silent) reloadingTag.style.display = 'none';
                    reloadInFlight = false;
                }
            }

            rangeSel.addEventListener('change', () => {
                activeRangeId = rangeSel.value;
                localStorage.setItem(RANGE_KEY, activeRangeId);
                showCustomInputs();
                if (activeRangeId !== 'custom') reloadForRange();
            });
            applyBtn.addEventListener('click', () => {
                customFrom = localToISO(fromInput.value);
                customTo   = localToISO(toInput.value);
                localStorage.setItem(RANGE_CUSTOM_KEY, JSON.stringify({ from: customFrom, to: customTo }));
                reloadForRange();
            });

            const refreshSelects = () => {
                const visible = rows.filter(matches);
                const fillSelect = (sel, key) => {
                    const candidates = (key === 'player_id'  ? rows :
                                        key === 'group_id'   ? rows.filter(r => !filters.player_id || r.player_id === filters.player_id) :
                                        key === 'content_id' ? rows.filter(r =>
                                            (!filters.player_id || r.player_id === filters.player_id) &&
                                            (!filters.group_id  || r.group_id  === filters.group_id)) :
                                        rows.filter(r =>
                                            (!filters.player_id  || r.player_id  === filters.player_id) &&
                                            (!filters.group_id   || r.group_id   === filters.group_id) &&
                                            (!filters.content_id || r.content_id === filters.content_id))
                                       );
                    const values = Array.from(new Set(candidates.map(r => r[key] || '').filter(Boolean))).sort();
                    sel.replaceChildren();
                    const all = document.createElement('option');
                    all.value = ''; all.textContent = `all (${values.length})`;
                    sel.appendChild(all);
                    for (const v of values) {
                        const opt = document.createElement('option');
                        opt.value = v; opt.textContent = v;
                        sel.appendChild(opt);
                    }
                    if (values.includes(filters[key])) sel.value = filters[key];
                    else { sel.value = ''; filters[key] = ''; }
                };
                fillSelect(playerSel,  'player_id');
                fillSelect(groupSel,   'group_id');
                fillSelect(contentSel, 'content_id');
                fillSelect(playSel,    'play_id');
                matchCount.textContent = `${visible.length} matching`;
                rebuildTable(visible);
            };

            // Column metadata: display label, source field on the row,
            // and how to coerce for sort (string vs number). Click a
            // header to sort; click again to flip direction. Default
            // sort is "started DESC" (newest first). duration_ms is
            // computed in loadRows() from started/last_seen.
            const fmtDur = (ms) => {
                if (!ms) return '—';
                const s = Math.round(ms / 1000);
                const h = Math.floor(s / 3600);
                const m = Math.floor((s % 3600) / 60);
                const sec = s % 60;
                if (h) return `${h}h ${m}m ${sec}s`;
                if (m) return `${m}m ${sec}s`;
                return `${sec}s`;
            };

            // Columns are grouped: identification, then (A) Issues badge,
            // then (C) Health score, then (B) per-category counts. Hover any
            // colored cell for tooltip breakdown.
            // Relative-time formatter for "Last updated". Includes a tiny
            // pulse dot when the session was updated very recently so a
            // user scanning the list can spot active sessions at a glance.
            const fmtRelTime = (iso) => {
                if (!iso) return '<span style="color:#9ca3af;">—</span>';
                const t = Date.parse(String(iso).replace(' ', 'T') + 'Z');
                if (!Number.isFinite(t)) return '<span style="color:#9ca3af;">—</span>';
                const ageSec = Math.max(0, (Date.now() - t) / 1000);
                let label;
                if (ageSec < 60)        label = `${Math.round(ageSec)}s ago`;
                else if (ageSec < 3600) label = `${Math.round(ageSec / 60)}m ago`;
                else if (ageSec < 86400) label = `${Math.round(ageSec / 3600)}h ago`;
                else                    label = `${Math.round(ageSec / 86400)}d ago`;
                const tip = String(iso);
                if (ageSec < 30) {
                    return `<span title="${escapeHtml(tip)}" style="color:#16a34a;font-weight:600;display:inline-flex;align-items:center;gap:6px;"><span style="display:inline-block;width:6px;height:6px;border-radius:50%;background:#16a34a;animation:replay-live-pulse 1.2s ease-in-out infinite;"></span>${label}</span>`;
                }
                const color = ageSec < 300 ? 'var(--text-primary)' : 'var(--text-secondary)';
                return `<span title="${escapeHtml(tip)}" style="color:${color};">${label}</span>`;
            };

            const columns = [
                { label: 'Started',     key: 'started',          type: 'string' },
                { label: 'Last updated', key: 'last_seen',       type: 'string', html: true, format: fmtRelTime },
                { label: 'Duration',    key: 'duration_ms',      type: 'number', format: fmtDur },
                { label: 'Player',      key: 'player_id',        type: 'string' },
                { label: 'Content',     key: 'content_id',       type: 'string' },
                { label: 'Play ID',     key: 'play_id',          type: 'string', html: true,
                  format: (v, r) => {
                      // Wrap the play_id value in its own <a> so the
                      // browser's native right-click menu on the cell
                      // offers "Open Link in New Tab" / "Copy Link
                      // Address" instead of the text-selection menu.
                      // The row also has a stretched <a> covering the
                      // whole row at z-index:0; this anchor sits at
                      // z-index:2 so it wins focus, hover and the
                      // context menu on this specific cell.
                      const text = (v == null) ? '' : String(v);
                      if (!text || text === '—') return escapeHtml(text);
                      const params = new URLSearchParams({ replay: '1', session: r.session_id });
                      params.set('play_id', text);
                      const href = '/dashboard/session-viewer.html?' + params.toString();
                      return `<a href="${href}" `
                          + `style="color:#1d4ed8;text-decoration:none;font-weight:600;`
                          + `position:relative;z-index:2;">${escapeHtml(text)}</a>`;
                  } },
                { label: 'State',       key: 'last_state',       type: 'string' },
                { label: 'Issues',      key: 'issues_count',     type: 'number', html: true, format: fmtIssuesBadge },
                { label: 'Flags',       key: '__flags',          type: 'string', html: true, format: fmtFlags },
                { label: 'Health',      key: 'health_score',     type: 'number', html: true, format: fmtHealth },
                { label: 'Stalls',      key: 'stalls',           type: 'number', html: true, format: fmtCount([1, 5]) },
                { label: 'Errors',      key: 'errors_count',     type: 'number', html: true, format: fmtCount([1, 1]) },
                { label: 'Faults',      key: 'faults_count',     type: 'number', html: true, format: fmtCount([1, 10]) },
                { label: 'Downshifts',  key: 'downshifts_count', type: 'number', html: true, format: fmtCount([1, 5]) },
                { label: 'Drops',       key: 'dropped_frames',   type: 'number', html: true, format: fmtCount([100, 1000]) },
                { label: 'Avg Q%',      key: 'avg_quality_pct',  type: 'number', html: true, format: fmtPct },
                { label: 'Metrics',     key: 'metric_events',    type: 'number' },
                { label: 'HAR',         key: 'net_events',       type: 'number' },
                { label: '',            key: '__bundle',         type: 'string', html: true,
                  format: (_, r) => {
                      const pid = (r.play_id && r.play_id !== '—') ? r.play_id : '';
                      const url = '/analytics/api/session_bundle?'
                          + 'session=' + encodeURIComponent(r.session_id)
                          + (pid ? '&play_id=' + encodeURIComponent(pid) : '');
                      // data-bundle-link signals to the row's click handler
                      // that this is a bundle download — we intercept and
                      // prevent the page-level navigation that would
                      // otherwise replace the picker view with the viewer.
                      return `<a href="${url}" download data-bundle-link
                          title="Download session bundle (.zip)"
                          style="display:inline-block;padding:2px 8px;border-radius:4px;
                                 background:var(--bg-secondary,#f3f4f6);
                                 color:var(--text-primary,#111827);text-decoration:none;
                                 font-size:13px;line-height:1.2;
                                 position:relative;z-index:2;">📥</a>`;
                  } }
            ];
            let sortKey = 'started';
            let sortDir = 'desc';
            let lastVisible = [];

            const cmp = (a, b, key, type) => {
                const av = a[key];
                const bv = b[key];
                if (type === 'number') {
                    const an = Number(av) || 0;
                    const bn = Number(bv) || 0;
                    return an - bn;
                }
                const as = String(av || '');
                const bs = String(bv || '');
                return as < bs ? -1 : as > bs ? 1 : 0;
            };

            const rebuildTable = (visible) => {
                lastVisible = visible.slice();
                const colMeta = columns.find(c => c.key === sortKey) || columns[0];
                const sorted = lastVisible.sort((a, b) => {
                    const c = cmp(a, b, sortKey, colMeta.type);
                    return sortDir === 'asc' ? c : -c;
                });

                const head = document.createElement('thead');
                const headTr = document.createElement('tr');
                headTr.style.cssText = 'background:var(--bg-secondary, #f5f5f5);position:sticky;top:0;text-align:left;';
                for (const col of columns) {
                    const th = document.createElement('th');
                    th.style.cssText = 'padding:8px 10px;border-bottom:1px solid var(--border-color, #e5e7eb);font-weight:600;color:var(--text-primary);cursor:pointer;user-select:none;white-space:nowrap;';
                    th.title = `Sort by ${col.label}`;
                    const arrow = sortKey === col.key
                        ? (sortDir === 'asc' ? ' ▲' : ' ▼')
                        : ' <span style="color:#9ca3af;">⇅</span>';
                    th.innerHTML = col.label + arrow;
                    th.addEventListener('click', () => {
                        if (sortKey === col.key) {
                            sortDir = sortDir === 'asc' ? 'desc' : 'asc';
                        } else {
                            sortKey = col.key;
                            sortDir = col.type === 'number' ? 'desc' : 'asc';
                        }
                        rebuildTable(lastVisible);
                    });
                    headTr.appendChild(th);
                }
                head.appendChild(headTr);

                const body = document.createElement('tbody');
                if (sorted.length === 0) {
                    body.innerHTML = `<tr><td colspan="${columns.length}" style="padding:16px;text-align:center;color:var(--text-secondary);">No sessions match the current filters.</td></tr>`;
                } else {
                    for (const r of sorted) {
                        const tr = document.createElement('tr');
                        // Critical rows get a red left bar so the eye
                        // catches them while scrolling — applies to any
                        // session with a 911, frozen, or error event.
                        const baseBorder = r.is_critical
                            ? 'border-left:4px solid #dc2626;'
                            : 'border-left:4px solid transparent;';
                        // position:relative so the stretched <a> below
                        // resolves against the row, not the table.
                        tr.style.cssText = `cursor:pointer;border-bottom:1px solid var(--border-color, #f3f4f6);position:relative;${baseBorder}`;
                        const hoverBg = r.is_critical ? '#fef2f2' : 'var(--bg-hover, #f9fafb)';
                        tr.addEventListener('mouseenter', () => tr.style.background = hoverBg);
                        tr.addEventListener('mouseleave', () => tr.style.background = '');

                        const params = new URLSearchParams({ replay: '1', session: r.session_id });
                        if (r.play_id && r.play_id !== '—') params.set('play_id', r.play_id);
                        const href = '/dashboard/session-viewer.html?' + params.toString();

                        const cells = columns.map(col => {
                            const v = r[col.key];
                            if (col.format) {
                                const out = col.format(v, r);
                                return col.html ? out : escapeHtml(out);
                            }
                            if (col.type === 'number') return escapeHtml(String(v || 0));
                            return escapeHtml(v || '');
                        });
                        tr.innerHTML = cells.map((c, i) => {
                            const colKey = columns[i].key;
                            const styles = colKey === 'play_id'
                                ? 'padding:5px 8px;font-weight:600;color:#1d4ed8;'
                                : 'padding:5px 8px;';
                            return `<td style="${styles}">${c}</td>`;
                        }).join('');

                        // Add a hidden full-row link so middle-click / Cmd-
                        // click / right-click → "Open in New Tab" all work
                        // natively. The link is added to the LAST cell with
                        // position:absolute against the relatively-positioned
                        // <tr>, so it covers the whole row. We also keep a JS
                        // click handler for plain left-click so per-cell
                        // tooltips (Issues / Health breakdowns) keep working
                        // — left-click bubbles past the link only when JS
                        // isn't disabled, but the link is the right-click /
                        // middle-click / Cmd-click target.
                        const lastTd = tr.lastElementChild;
                        if (lastTd) {
                            const a = document.createElement('a');
                            a.href = href;
                            a.setAttribute('aria-label', `Open session ${r.session_id}${r.play_id ? ' / ' + r.play_id : ''}`);
                            // Tagged so the row's click handler can tell
                            // this row-fallback anchor apart from cell-
                            // level anchors (e.g. the play_id link).
                            a.dataset.rowFallback = '1';
                            a.tabIndex = -1;
                            a.style.cssText = 'position:absolute;inset:0;z-index:0;text-decoration:none;';
                            // Suppress the link's own left-click — we let
                            // the JS handler below decide. Cmd/Ctrl/middle/
                            // right-click bypass click handlers and use the
                            // browser's native link affordances.
                            a.addEventListener('click', (e) => {
                                if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                                e.preventDefault();
                            });
                            lastTd.appendChild(a);
                        }
                        // Cells need position:relative so the link's
                        // inset:0 (which resolves against the <tr>) doesn't
                        // visually clip them. Cells also need z-index:1 so
                        // their interactive content (badge tooltips) sits
                        // above the link for hover purposes.
                        for (const td of tr.children) {
                            td.style.position = 'relative';
                            if (td !== lastTd) td.style.zIndex = '1';
                        }

                        tr.addEventListener('click', (e) => {
                            // Bundle download link — let the browser
                            // start the .zip download, don't navigate.
                            if (e.target.closest && e.target.closest('[data-bundle-link]')) return;
                            // Cell-level explicit anchors (e.g. the
                            // play_id link) — let the browser handle
                            // the default navigation. Otherwise we'd
                            // double-fire (the anchor's default plus
                            // the row's window.location.href below).
                            // The row's stretched <a> at z-index:0 has
                            // its own click handler that preventDefaults
                            // on plain left-click — we still need to
                            // navigate via the row handler in that case.
                            const explicitAnchor = e.target.closest && e.target.closest('a:not([data-row-fallback])');
                            if (explicitAnchor) return;
                            // If the click was on the link, the link's own
                            // handler already chose to either preventDefault
                            // (plain left-click → fall through to here) or
                            // let the browser navigate (modifier click).
                            // For modifier clicks we just bail.
                            if (e.metaKey || e.ctrlKey || e.shiftKey) return;
                            window.location.href = href;
                        });
                        body.appendChild(tr);
                    }
                }
                table.replaceChildren(head, body);
            };

            const onFilterChange = (e) => {
                filters[e.target.dataset.key] = e.target.value || '';
                refreshSelects();
            };
            playerSel.addEventListener('change', onFilterChange);
            groupSel.addEventListener('change', onFilterChange);
            contentSel.addEventListener('change', onFilterChange);
            playSel.addEventListener('change', onFilterChange);
            clearBtn.addEventListener('click', () => {
                filters.player_id = filters.group_id = filters.content_id = filters.play_id = '';
                refreshSelects();
            });

            refreshSelects();

            // Auto-refresh: every 5s re-fetch from the forwarder so
            // last_seen advances and new sessions appear. Silent (no
            // "Reloading…" flash). Tightening past 5s mostly buys nothing
            // — the upstream pipeline (proxy SSE debounce 250ms +
            // forwarder batch flush 1s) caps how fresh ClickHouse can
            // ever be. 5s × concurrent-fetch guard ≈ steady-state load.
            const autoRefreshTicker = setInterval(() => {
                reloadForRange({ silent: true });
            }, 5000);
            window.addEventListener('beforeunload', () => clearInterval(autoRefreshTicker), { once: true });
        }

    window.SessionReplay = {
        startMode: startReplayMode,
        startPicker: startReplayPicker
    };
})();
