(function () {
    'use strict';

    const baseFailureTypes = [
        { value: 'none', text: 'None' },
        { value: '404', text: '404' },
        { value: '500', text: '500' },
        { value: '403', text: '403' },
        { value: 'timeout', text: 'Timeout' },
        { value: 'connection_refused', text: 'Conn Refused' },
        { value: 'dns_failure', text: 'DNS Failure' },
        { value: 'rate_limiting', text: 'Rate Limit' },
        { value: 'request_connect_reset', text: 'Request Connect Reset' },
        { value: 'request_connect_delayed', text: 'Request Connect Delay' },
        { value: 'request_connect_hang', text: 'Request Connect Hang' },
        { value: 'request_first_byte_reset', text: 'Request Header Reset' },
        { value: 'request_first_byte_delayed', text: 'Request Header Delay' },
        { value: 'request_first_byte_hang', text: 'Request Header Hang' },
        { value: 'request_body_reset', text: 'Request Body Reset' },
        { value: 'request_body_delayed', text: 'Request Body Delay' },
        { value: 'request_body_hang', text: 'Request Body Hang' }
    ];

    const segmentFailureTypes = [
        ...baseFailureTypes,
        { value: 'corrupted', text: 'Corrupted' }
    ];

    const modeOptions = [
        { value: 'requests', text: 'Requests' },
        { value: 'seconds', text: 'Seconds' },
        { value: 'failures_per_seconds', text: 'Failures / Seconds' }
    ];

    const transportFaultTypes = [
        { value: 'none', text: 'None' },
        { value: 'drop', text: 'Drop (Blackhole)' },
        { value: 'reject', text: 'Reject (RST)' }
    ];
    const transportModeOptions = [
        { value: 'failures_per_seconds', text: 'Seconds' },
        { value: 'failures_per_packets', text: 'Packets / Seconds' }
    ];
    const networkLogEntriesBySession = new Map();
    const networkWaterfallFollowModeBySession = new Map();
    const networkWaterfallRenderSignatureBySession = new Map();
    const networkWaterfallRowSignaturesBySession = new Map();
    const networkWaterfallRowsBySession = new Map();
    // Per-session brush state: { startMs, endMs, follow }. `follow=true`
    // means the brush sticks to the right edge as new entries arrive
    // (Following Latest). Drag-pan flips it to false.
    const networkWaterfallBrushBySession = new Map();
    const networkWaterfallRollingWindowMs = 10 * 60 * 1000;
    // Default and minimum brush span. Tighter than this and the time
    // axis gets too granular to be useful; this is also the default
    // initial span when a session first opens.
    const networkWaterfallMinBrushMs = 1 * 60 * 1000;
    const networkLogAutoRefreshTimers = new Map();
    const networkLogFetchInFlight = new Set();
    const networkLogAutoRefreshMs = 1500;
    // Network log used to be developer-only; now enabled for everyone.
    // The constant stays as `true` so the runtime gates still compile.
    const networkLogDeveloperEnabled = true;

    function formatDate(value) {
        if (!value) return '—';
        const date = new Date(value);
        return date.toLocaleString();
    }

    function formatDuration(seconds) {
        if (!seconds && seconds !== 0) return '—';
        const hrs = Math.floor(seconds / 3600).toString().padStart(2, '0');
        const mins = Math.floor((seconds % 3600) / 60).toString().padStart(2, '0');
        const secs = Math.floor(seconds % 60).toString().padStart(2, '0');
        return `${hrs}:${mins}:${secs}`;
    }

    function formatPercent(value) {
        if (value === null || value === undefined || value === '') return '—';
        const numeric = Number(value);
        if (!Number.isFinite(numeric)) return String(value);
        const rounded = Math.round(numeric * 100) / 100;
        return `${rounded}%`;
    }

    function formatSeconds(value) {
        if (value === null || value === undefined || value === '') return '—';
        const numeric = Number(value);
        if (!Number.isFinite(numeric)) return String(value);
        return `${numeric.toFixed(3)}s`;
    }

    function sortedPlaylists(playlists) {
        if (!Array.isArray(playlists)) return [];
        return playlists.slice().sort((a, b) => b.bandwidth - a.bandwidth);
    }

    function renderFailureTypeOptions(name, selected, options) {
        const list = options || baseFailureTypes;
        return list.map(option => {
            const checked = option.value === (selected || 'none') ? 'checked' : '';
            return `<label><input type="radio" name="${name}" value="${option.value}" data-field="${name.replace(/_\d+$/, '')}" ${checked}>${option.text}</label>`;
        }).join('');
    }

    function renderModeDropdown(name, selected) {
        const optionsHtml = modeOptions.map(option => {
            const isSelected = option.value === (selected || 'requests') ? 'selected' : '';
            return `<option value="${option.value}" ${isSelected}>${option.text}</option>`;
        }).join('');
        return `<select name="${name}" data-field="${name.replace(/_\d+$/, '')}">${optionsHtml}</select>`;
    }

    function renderTransportFaultOptions(name, selected) {
        return renderFailureTypeOptions(name, selected, transportFaultTypes);
    }

    function renderTransportModeOptions(name, selected) {
        return transportModeOptions.map(option => {
            const checked = option.value === (selected || 'failures_per_seconds') ? 'checked' : '';
            return `<label><input type="radio" name="${name}" value="${option.value}" data-field="transport_failure_mode" ${checked}>${option.text}</label>`;
        }).join('');
    }

    function modeFromUnits(consecutiveUnits, frequencyUnits, fallbackUnits) {
        const cons = consecutiveUnits || fallbackUnits || 'requests';
        const freq = frequencyUnits || fallbackUnits || 'requests';
        if (cons === 'requests' && freq === 'seconds') {
            return 'failures_per_seconds';
        }
        if (cons === 'seconds' && freq === 'seconds') {
            return 'seconds';
        }
        return 'requests';
    }

    function unitsFromMode(mode) {
        if (mode === 'seconds') {
            return { consecutiveUnits: 'seconds', frequencyUnits: 'seconds' };
        }
        if (mode === 'failures_per_seconds') {
            return { consecutiveUnits: 'requests', frequencyUnits: 'seconds' };
        }
        return { consecutiveUnits: 'requests', frequencyUnits: 'requests' };
    }

    function normalizeTransportMode(raw) {
        const value = String(raw || '').trim().toLowerCase();
        if (value === 'failures_per_packets' || value === 'failures_per_packet') {
            return 'failures_per_packets';
        }
        return 'failures_per_seconds';
    }

    function transportUnitsFromMode(mode) {
        if (normalizeTransportMode(mode) === 'failures_per_packets') {
            return { consecutiveUnits: 'packets', frequencyUnits: 'seconds' };
        }
        return { consecutiveUnits: 'seconds', frequencyUnits: 'seconds' };
    }

    function transportModeFromSession(session) {
        const mode = normalizeTransportMode(session.transport_failure_mode);
        if (mode === 'failures_per_packets') return mode;
        const unitsRaw = String(session.transport_consecutive_units || session.transport_failure_units || '').trim().toLowerCase();
        if (unitsRaw === 'packets' || unitsRaw === 'packet' || unitsRaw === 'pkts' || unitsRaw === 'pkt') {
            return 'failures_per_packets';
        }
        return 'failures_per_seconds';
    }

    function transportConsecutiveRangeForMode(mode) {
        if (normalizeTransportMode(mode) === 'failures_per_packets') {
            return { min: 0, max: 500, step: 1, label: 'Consecutive (pkts)' };
        }
        return { min: 0, max: 30, step: 1, label: 'Consecutive (secs)' };
    }

    function normalizeTransportConsecutiveValue(raw, mode) {
        const numeric = toNonNegativeNumber(raw, 0);
        if (normalizeTransportMode(mode) === 'failures_per_packets') {
            return Math.round(numeric);
        }
        return Math.round(numeric * 10) / 10;
    }

    function renderManifestOptions(sessionId, variants, selected) {
        const selectedSet = new Set(selected || []);
        const list = sortedPlaylists(variants);
        const allChecked = selected == null ? true : selectedSet.has('All');
        const checkbox = (value, label) => {
            const checked = allChecked || selectedSet.has(value) ? 'checked' : '';
            return `<label><input type="checkbox" data-field="manifest_failure_urls" value="${value}" ${checked}>${label}</label>`;
        };
        const items = [checkbox('All', 'All'), checkbox('audio', 'Audio')];
        list.forEach(variant => {
            const resolution = variant.resolution || 'unknown';
            const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
            const heightLabel = height === 'unknown' ? 'unknown' : `${height}p`;
            const label = `${heightLabel}/${Math.round(variant.bandwidth / 1000)}kbps`;
            items.push(checkbox(variant.url, label));
        });
        return items.join('');
    }

    function variantFromManifestUrl(url) {
        if (!url) return '';
        const parts = url.split('/');
        if (parts.length > 1) {
            return parts[0] || '';
        }
        let base = url.replace(/\?.*$/, '').replace(/\.m3u8.*$/i, '');
        if (base.includes('_')) {
            base = base.split('_').pop();
        }
        return base;
    }

    function renderSegmentOptions(sessionId, playlists, selected) {
        const selectedSet = new Set(selected || []);
        const list = sortedPlaylists(playlists);
        const allChecked = selected == null ? true : selectedSet.has('All');
        const checkbox = (value, label) => {
            const checked = allChecked || selectedSet.has(value) ? 'checked' : '';
            return `<label><input type="checkbox" data-field="segment_failure_urls" value="${value}" ${checked}>${label}</label>`;
        };
        const variants = new Map();
        list.forEach(playlist => {
            const value = variantFromManifestUrl(playlist.url);
            if (value && !variants.has(value)) {
                const resolution = playlist.resolution || 'unknown';
                const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
                const heightLabel = height === 'unknown' ? 'unknown' : `${height}p`;
                const label = `${heightLabel}/${Math.round(playlist.bandwidth / 1000)}kbps`;
                variants.set(value, label);
            }
        });
        const items = [checkbox('All', 'All'), checkbox('audio', 'Audio')];
        Array.from(variants.entries()).forEach(([value, label]) => {
            items.push(checkbox(value, label));
        });
        return items.join('');
    }

    function renderContentVariantOptions(sessionId, variants, allowedVariants) {
        const list = sortedPlaylists(variants);
        const allowedSet = new Set(allowedVariants || []);
        const allChecked = allowedSet.size === 0;
        
        const checkbox = (value, label) => {
            const checked = allChecked || allowedSet.has(value) ? 'checked' : '';
            return `<label><input type="checkbox" data-field="content_allowed_variants" value="${value}" ${checked}>${label}</label>`;
        };
        
        const items = [];
        list.forEach(variant => {
            const resolution = variant.resolution || 'unknown';
            const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
            const heightLabel = height === 'unknown' ? 'unknown' : `${height}p`;
            const label = `${heightLabel} / ${Math.round(variant.bandwidth / 1000)} kbps`;
            items.push(checkbox(variant.url, label));
        });
        
        if (items.length === 0) {
            return '<span class="no-variants-message">Play content once to populate variant list</span>';
        }
        
        return items.join('');
    }

    function collectShapingBandwidthPresets(playlists) {
        const list = sortedPlaylists(playlists);
        const presets = [];
        const seen = new Set();
        list.forEach((playlist) => {
            const bandwidth = Number(playlist.bandwidth || 0);
            if (!Number.isFinite(bandwidth) || bandwidth <= 0) return;
            const mbps = Math.round((bandwidth / 1000000) * 1000) / 1000;
            const key = mbps.toFixed(3);
            if (seen.has(key)) return;
            seen.add(key);
            const resolution = playlist.resolution || 'unknown';
            const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
            const heightLabel = playlist.url && playlist.url.includes('audio')
                ? 'audio'
                : (height === 'unknown' ? 'unknown' : `${height}p`);
            const kbps = Math.round(bandwidth / 1000);
            presets.push({
                mbps,
                label: `${heightLabel}/${kbps}kbps`
            });
        });
        return presets;
    }

    function getBool(obj, key) {
        const val = obj[key];
        return val === true || val === 'true' || val === 1 || val === '1';
    }

    function getStringSlice(obj, key) {
        const val = obj[key];
        if (Array.isArray(val)) return val;
        return [];
    }

    function collectVideoShapingPresets(playlists) {
        const list = sortedPlaylists(playlists).filter((playlist) => !(playlist.url || '').includes('audio'));
        const presets = [];
        const seen = new Set();
        list.forEach((playlist) => {
            const bandwidth = Number(playlist.bandwidth || 0);
            if (!Number.isFinite(bandwidth) || bandwidth <= 0) return;
            const mbps = Math.round((bandwidth / 1000000) * 1000) / 1000;
            const key = mbps.toFixed(3);
            if (seen.has(key)) return;
            seen.add(key);
            const resolution = playlist.resolution || 'unknown';
            const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
            const heightLabel = height === 'unknown' ? 'unknown' : `${height}p`;
            const kbps = Math.round(bandwidth / 1000);
            presets.push({
                mbps,
                label: `${heightLabel}/${kbps}kbps`
            });
        });
        return presets.sort((a, b) => a.mbps - b.mbps);
    }

    function estimateAudioOverheadMbps(playlists) {
        const list = sortedPlaylists(playlists);
        let audioMbps = 0;
        list.forEach((playlist) => {
            if (!(playlist.url || '').includes('audio')) return;
            const bandwidth = Number(playlist.bandwidth || 0);
            if (!Number.isFinite(bandwidth) || bandwidth <= 0) return;
            audioMbps = Math.max(audioMbps, bandwidth / 1000000);
        });
        const playlistOverheadMbps = 0.05;
        return Math.round((audioMbps + playlistOverheadMbps) * 1000) / 1000;
    }

    function computeStallRiskThreshold(videoPresets) {
        if (!Array.isArray(videoPresets) || !videoPresets.length) return null;
        const minVideo = Math.min(...videoPresets
            .map((preset) => Number(preset.mbps))
            .filter((value) => Number.isFinite(value) && value > 0));
        if (!Number.isFinite(minVideo) || minVideo <= 0) return null;
        return Math.round(minVideo * 1.1 * 1000) / 1000;
    }

    function renderPatternStepPresetOptions(rate, presets, matchOptions = {}) {
        const numericRate = Number(rate);
        const hasRate = Number.isFinite(numericRate);
        let selectedValue = 'custom';
        const options = presets || [];
        const overheadMbps = Number(matchOptions.overheadMbps || 0);
        const marginPct = Number(matchOptions.marginPct || 0);
        const adjust = (Number.isFinite(overheadMbps) && overheadMbps !== 0)
            || (Number.isFinite(marginPct) && marginPct !== 0);
        const adjustedValue = (base) => {
            const adjusted = (base * (1 + (marginPct / 100))) + overheadMbps;
            return Math.round(adjusted * 1000) / 1000;
        };
        options.forEach((preset) => {
            if (!hasRate) return;
            const base = Number(preset.mbps);
            const candidate = adjust ? adjustedValue(base) : base;
            if (Math.abs(candidate - numericRate) < 0.001) {
                selectedValue = Number(candidate).toFixed(3);
            }
        });
        const customSelected = selectedValue === 'custom' ? ' selected' : '';
        const optionHtml = options.map((preset) => {
            const base = Number(preset.mbps);
            const candidate = adjust ? adjustedValue(base) : base;
            const value = Number(candidate).toFixed(3);
            const selected = selectedValue === value ? ' selected' : '';
            const riskPrefix = preset.risk ? '⚠ ' : '';
            return `<option value="${value}"${selected}>${riskPrefix}${preset.label}</option>`;
        }).join('');
        return `<option value="custom"${customSelected}>Custom</option>${optionHtml}`;
    }

    function renderPatternStepRowContent(step, presets, matchOptions = {}) {
        const rate = Number.isFinite(Number(step.rate_mbps)) ? Number(step.rate_mbps) : 0;
        const seconds = Number.isFinite(Number(step.duration_seconds)) ? Number(step.duration_seconds) : 1;
        const enabled = step.enabled !== false;
        return `
            <label>Preset</label>
            <select data-field="shaping_step_mbps_preset">
                ${renderPatternStepPresetOptions(rate, presets, matchOptions)}
            </select>
            <label>Mbps</label>
            <input type="number" min="0" step="0.1" data-field="shaping_step_mbps" value="${rate}">
            <label>Time (s)</label>
            <input type="number" min="0.5" step="0.1" data-field="shaping_step_seconds" value="${seconds}">
            <label class="shape-step-enabled"><input type="checkbox" data-field="shaping_step_enabled" ${enabled ? 'checked' : ''}>Enabled</label>
        `;
    }

    function toPositiveNumber(value, fallback) {
        const n = Number(value);
        if (!Number.isFinite(n) || n <= 0) return fallback;
        return n;
    }

    function toNonNegativeNumber(value, fallback) {
        const n = Number(value);
        if (!Number.isFinite(n) || n < 0) return fallback;
        return n;
    }

    function inferSegmentDurationSeconds(session) {
        const explicit = Number(session.nftables_pattern_segment_duration_seconds || 0);
        if (Number.isFinite(explicit) && explicit > 0) {
            return explicit;
        }
        const candidates = [
            session.manifest_url || '',
            session.master_manifest_url || '',
            session.last_request_url || ''
        ];
        for (const value of candidates) {
            const match = value.match(/(?:_|\/)(\d+)s(?:[._/?]|$)/i);
            if (match) {
                const parsed = Number(match[1]);
                if (Number.isFinite(parsed) && parsed > 0) {
                    return parsed;
                }
            }
        }
        return 1;
    }

    function parsePatternSteps(raw) {
        if (!Array.isArray(raw)) return [];
        return raw
            .map(step => {
                if (!step || typeof step !== 'object') return null;
                const rate = Number(step.rate_mbps);
                const seconds = Number(step.duration_seconds);
                const enabled = step.enabled !== false;
                if (!Number.isFinite(rate) || !Number.isFinite(seconds) || seconds <= 0) {
                    return null;
                }
                return { rate_mbps: rate, duration_seconds: seconds, enabled };
            })
            .filter(Boolean);
    }

    function renderPatternStepRow(index, step, presets, matchOptions = {}) {
        return `
            <div class="shape-step-row" data-step-index="${index}">
                ${renderPatternStepRowContent(step, presets, matchOptions)}
            </div>
        `;
    }

    function closestStepDuration(seconds) {
        const options = [6, 12, 18, 24];
        const numeric = Number(seconds);
        if (!Number.isFinite(numeric) || numeric <= 0) return 12;
        let best = options[0];
        let bestDiff = Math.abs(numeric - best);
        options.forEach((value) => {
            const diff = Math.abs(numeric - value);
            if (diff < bestDiff) {
                best = value;
                bestDiff = diff;
            }
        });
        return best;
    }

    function resolveSectionDefault(options, key, fallback) {
        if (!options || !options.sectionDefaults) return fallback;
        if (!Object.prototype.hasOwnProperty.call(options.sectionDefaults, key)) return fallback;
        return !!options.sectionDefaults[key];
    }

    function renderSessionCard(session, options = {}) {
        const sessionId = session.session_id;
        const manifestVariants = session.manifest_variants || [];
        const manifestSelected = session.manifest_failure_urls;
        const segmentSelected = session.segment_failure_urls;
        const inlineHost = options.inlineHost || false;
        const hideTitle = options.hideTitle || false;
        const showPortItem = options.showPortItem || false;
        const showBufferDepthChart = options.showBufferDepthChart || false;
        const segmentDurationSeconds = inferSegmentDurationSeconds(session);
        const defaultSegments = toPositiveNumber(session.nftables_pattern_default_segments, 2);
        const storedDefaultStepSeconds = toPositiveNumber(session.nftables_pattern_default_step_seconds, 0);
        const defaultStepSeconds = storedDefaultStepSeconds > 0
            ? Math.round(storedDefaultStepSeconds * 10) / 10
            : Math.round(segmentDurationSeconds * defaultSegments * 10) / 10;
        const selectedStepSeconds = closestStepDuration(defaultStepSeconds);
        const videoPresets = collectVideoShapingPresets(manifestVariants);
        const stallRiskThreshold = computeStallRiskThreshold(videoPresets);
        const shapingPresets = collectShapingBandwidthPresets(manifestVariants).map((preset) => ({
            ...preset,
            risk: Number.isFinite(stallRiskThreshold) && Number(preset.mbps) < stallRiskThreshold
        }));
        const overheadMbps = estimateAudioOverheadMbps(manifestVariants);
        const patternSteps = parsePatternSteps(session.nftables_pattern_steps);
        const initialSteps = patternSteps.length
            ? patternSteps
            : [{ rate_mbps: Number(session.nftables_bandwidth_mbps || 0), duration_seconds: defaultStepSeconds }];
        const encodedPresets = encodeURIComponent(JSON.stringify(shapingPresets));
        const encodedVideoPresets = encodeURIComponent(JSON.stringify(videoPresets));
        const templateModeRaw = String(session.nftables_pattern_template_mode || '').toLowerCase();
        const templateMode = ['sliders', 'square_wave', 'ramp_up', 'ramp_down', 'pyramid'].includes(templateModeRaw)
            ? templateModeRaw
            : 'sliders';
        const usePattern = templateMode !== 'sliders';
        const marginRaw = Number(session.nftables_pattern_margin_pct);
        const marginPct = [0, 10, 25, 50].includes(marginRaw) ? marginRaw : 0;
        const chartMaxRaw = String(session.ui_bitrate_axis_max || '').toLowerCase();
        const chartMaxMode = ['auto', '5', '10', '20', '30', '40', '50'].includes(chartMaxRaw) ? chartMaxRaw : 'auto';
        const transportFaultRaw = String(session.transport_failure_type || session.transport_fault_type || 'none').toLowerCase();
        const transportFaultType = ['none', 'drop', 'reject'].includes(transportFaultRaw) ? transportFaultRaw : 'none';
        const transportMode = transportModeFromSession(session);
        const transportConsecutiveRange = transportConsecutiveRangeForMode(transportMode);
        const transportConsecutive = normalizeTransportConsecutiveValue(
            toNonNegativeNumber(session.transport_consecutive_failures, toNonNegativeNumber(session.transport_consecutive_seconds, toNonNegativeNumber(session.transport_fault_on_seconds, 0))),
            transportMode
        );
        const transportOffSeconds = Math.round(
            toNonNegativeNumber(session.transport_failure_frequency, toNonNegativeNumber(session.transport_frequency_seconds, toNonNegativeNumber(session.transport_fault_off_seconds, 0))) * 10
        ) / 10;
        const transportActive = !!session.transport_fault_active;
        const transportDropPackets = Number(session.transport_fault_drop_packets || 0);
        const transportRejectPackets = Number(session.transport_fault_reject_packets || 0);
        const portDisplay = session.x_forwarded_port_external || session.x_forwarded_port || '—';
        const developerMode = typeof options.developerMode === 'boolean'
            ? options.developerMode
            : (new URLSearchParams(window.location.search).get('developer') === '1');

        // Calculate summary counts for badge
        const masterCount = session.master_manifest_requests_count || 0;
        const manifestCount = session.manifest_requests_count || 0;
        const segmentCount = session.segments_count || 0;

        const sessionDetailsOpen = resolveSectionDefault(options, 'session-details', false);
        const faultInjectionOpen = resolveSectionDefault(options, 'fault-injection', true);
        const serverTimeoutsOpen = resolveSectionDefault(options, 'server-timeouts', false);
        const networkShapingOpen = resolveSectionDefault(options, 'network-shaping', true);
        const bitrateChartOpen = resolveSectionDefault(options, 'bitrate-chart', false);
        const playerMetricsOpen = resolveSectionDefault(options, 'player-metrics', false);

        return `
            <div class="session-card" data-session-id="${sessionId}" data-session-port="${session.x_forwarded_port_external || session.x_forwarded_port || ''}" data-segment-duration-seconds="${segmentDurationSeconds}" data-shaping-presets="${encodedPresets}" data-shaping-video-presets="${encodedVideoPresets}" data-shaping-overhead-mbps="${overheadMbps}">
                <div class="session-header">
                    ${hideTitle ? '' : `<div class="session-title">Session ${sessionId}</div>`}
                    <div class="session-meta" title="Port">${portDisplay}</div>
                </div>
                ${inlineHost ? `<div class="session-inline-player" data-inline-host="${sessionId}"></div>` : ''}

                <!-- Collapsible Session Details -->
                <div class="collapsible-section" data-section="session-details" data-default-open="${sessionDetailsOpen}">
                    <div class="collapsible-header" data-toggle="session-details">
                        <span class="collapsible-icon">${sessionDetailsOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Session Details</span>
                        <span class="collapsible-badge" data-field="session_detail_counts">M:${masterCount} / Man:${manifestCount} / Seg:${segmentCount}</span>
                    </div>
                    <div class="collapsible-content" data-content="session-details" style="display: ${sessionDetailsOpen ? 'block' : 'none'};">
                        <div class="session-grid">
                            <div class="session-item"><span class="label">Session ID</span><span class="value" data-field="session_session_id">${session.session_id || '—'}</span></div>
                            <div class="session-item"><span class="label">Player ID</span><span class="value" data-field="session_player_id">${session.player_id || '—'}</span></div>
                            <div class="session-item"><span class="label">User Agent</span><span class="value" data-field="session_user_agent">${session.user_agent || '—'}</span></div>
                            <div class="session-item"><span class="label">Player IP</span><span class="value" data-field="session_player_ip">${session.player_ip || '—'}</span></div>
                            <div class="session-item"><span class="label">Origination IP</span><span class="value" data-field="session_origination_ip">${session.origination_ip || '—'}${session.is_external_ip ? ' <span class="badge external-badge">External</span>' : ''}</span></div>
                            <div class="session-item"><span class="label">Origination Time</span><span class="value" data-field="session_origination_time">${formatDate(session.origination_time)}</span></div>
                            ${showPortItem ? `<div class="session-item"><span class="label">Port</span><span class="value" data-field="session_port_display">${portDisplay}</span></div>` : ''}
                            <div class="session-item"><span class="label">Last Request</span><span class="value" data-field="session_last_request">${formatDate(session.last_request)}</span></div>
                            <div class="session-item"><span class="label">First Request</span><span class="value" data-field="session_first_request">${formatDate(session.first_request_time)}</span></div>
                            <div class="session-item"><span class="label">Session Duration</span><span class="value" data-field="session_duration">${formatDuration(session.session_duration)}</span></div>
                            <div class="session-item"><span class="label">Manifest URL</span><span class="value" data-field="session_manifest_url">${session.manifest_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Master Manifest URL</span><span class="value" data-field="session_master_manifest_url">${session.master_manifest_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Last Request URL</span><span class="value" data-field="session_last_request_url">${session.last_request_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Loop Count</span><span class="value" data-field="session_loop_count_server">${session.loop_count_server ?? '0'}</span></div>
                            <div class="session-item"><span class="label">Shaper Avg</span><span class="value" data-field="session_mbps_shaper_avg">${session.mbps_shaper_avg ?? '—'}</span></div>
                            ${developerMode ? `
                            <div class="session-item"><span class="label">mbps_shaper_rate</span><span class="value" data-field="session_mbps_shaper_rate">${session.mbps_shaper_rate ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_transfer_rate</span><span class="value" data-field="session_mbps_transfer_rate">${session.mbps_transfer_rate ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_transfer_complete</span><span class="value" data-field="session_mbps_transfer_complete">${session.mbps_transfer_complete ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_in</span><span class="value" data-field="session_mbps_in">${session.mbps_in ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_out</span><span class="value" data-field="session_mbps_out">${session.mbps_out ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_in_avg</span><span class="value" data-field="session_mbps_in_avg">${session.mbps_in_avg ?? '—'}</span></div>
                            <div class="session-item"><span class="label">mbps_in_active</span><span class="value" data-field="session_mbps_in_active">${session.mbps_in_active ?? '—'}</span></div>
                            <div class="session-item"><span class="label">bytes_in_total</span><span class="value" data-field="session_bytes_in_total">${session.bytes_in_total ?? '—'}</span></div>
                            <div class="session-item"><span class="label">bytes_out_total</span><span class="value" data-field="session_bytes_out_total">${session.bytes_out_total ?? '—'}</span></div>
                            <div class="session-item"><span class="label">bytes_in_last</span><span class="value" data-field="session_bytes_in_last">${session.bytes_in_last ?? '—'}</span></div>
                            <div class="session-item"><span class="label">bytes_out_last</span><span class="value" data-field="session_bytes_out_last">${session.bytes_out_last ?? '—'}</span></div>
                            <div class="session-item"><span class="label">measured_mbps</span><span class="value" data-field="session_measured_mbps">${session.measured_mbps ?? '—'}</span></div>
                            <div class="session-item"><span class="label">measurement_window_io</span><span class="value" data-field="session_measurement_window_io">${session.measurement_window_io ?? '—'}</span></div>
                            <div class="session-item"><span class="label">measurement_window_active</span><span class="value" data-field="session_measurement_window_active">${session.measurement_window_active ?? '—'}</span></div>
                            ` : ''}
                        </div>
                    </div>
                </div>

                <!-- Collapsible Player Metrics -->
                <div class="collapsible-section" data-section="player-metrics" data-default-open="${playerMetricsOpen}">
                    <div class="collapsible-header" data-toggle="player-metrics">
                        <span class="collapsible-icon">${playerMetricsOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Player Metrics</span>
                    </div>
                    <div class="collapsible-content" data-content="player-metrics" style="display: ${playerMetricsOpen ? 'block' : 'none'};">
                        <div class="session-grid">
                            <div class="session-item"><span class="label">Last Event</span><span class="value" data-field="player_metrics_last_event">${session.player_metrics_last_event || '—'}</span></div>
                            <div class="session-item"><span class="label">trigger_type</span><span class="value" data-field="player_metrics_trigger_type">${session.player_metrics_trigger_type || '—'}</span></div>
                            <div class="session-item"><span class="label">Last Event At</span><span class="value" data-field="player_metrics_last_event_at">${formatDate(session.player_metrics_last_event_at) || '—'}</span></div>
                            <div class="session-item"><span class="label">Event Time</span><span class="value" data-field="player_metrics_event_time">${formatDate(session.player_metrics_event_time) || '—'}</span></div>
                            <div class="session-item"><span class="label">State</span><span class="value" data-field="player_metrics_state">${session.player_metrics_state || '—'}</span></div>
                            <div class="session-item"><span class="label">Position</span><span class="value" data-field="player_metrics_position_s">${session.player_metrics_position_s ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Playback Rate</span><span class="value" data-field="player_metrics_playback_rate">${session.player_metrics_playback_rate ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Buffer Depth</span><span class="value" data-field="player_metrics_buffer_depth_s">${formatSeconds(session.player_metrics_buffer_depth_s)}</span></div>
                            <div class="session-item"><span class="label">Buffer End</span><span class="value" data-field="player_metrics_buffer_end_s">${formatSeconds(session.player_metrics_buffer_end_s)}</span></div>
                            <div class="session-item"><span class="label">Seekable End</span><span class="value" data-field="player_metrics_seekable_end_s">${formatSeconds(session.player_metrics_seekable_end_s)}</span></div>
                            <div class="session-item"><span class="label">Live Edge</span><span class="value" data-field="player_metrics_live_edge_s">${formatSeconds(session.player_metrics_live_edge_s)}</span></div>
                            <div class="session-item"><span class="label">Live Offset</span><span class="value" data-field="player_metrics_live_offset_s">${formatSeconds(session.player_metrics_live_offset_s)}</span></div>
                            <div class="session-item"><span class="label">Wall-Clock Offset</span><span class="value" data-field="player_metrics_true_offset_s">${formatSeconds(session.player_metrics_true_offset_s)}</span></div>
                            <div class="session-item"><span class="label">Display Resolution</span><span class="value" data-field="player_metrics_display_resolution">${session.player_metrics_display_resolution ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Video Resolution</span><span class="value" data-field="player_metrics_video_resolution">${session.player_metrics_video_resolution ?? '—'}</span></div>
                            <div class="session-item"><span class="label">First Frame Time</span><span class="value" data-field="player_metrics_video_first_frame_time_s">${formatSeconds(session.player_metrics_video_first_frame_time_s)}</span></div>
                            <div class="session-item"><span class="label">Video Start Time</span><span class="value" data-field="player_metrics_video_start_time_s">${formatSeconds(session.player_metrics_video_start_time_s)}</span></div>
                            <div class="session-item"><span class="label">Video Bitrate Mbps</span><span class="value" data-field="player_metrics_video_bitrate_mbps">${session.player_metrics_video_bitrate_mbps ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Server Rendition</span><span class="value" data-field="server_video_rendition">${session.server_video_rendition || '—'}</span></div>
                            <div class="session-item"><span class="label">Server Rendition Mbps</span><span class="value" data-field="server_video_rendition_mbps">${session.server_video_rendition_mbps ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Video Quality</span><span class="value" data-field="player_metrics_video_quality_pct">${formatPercent(session.player_metrics_video_quality_pct)}</span></div>
                            <div class="session-item"><span class="label">avgNetworkBitrate Mbps</span><span class="value" data-field="player_metrics_avg_network_bitrate_mbps">${session.player_metrics_avg_network_bitrate_mbps ?? '—'}</span></div>
                            <div class="session-item"><span class="label">networkBitrate Mbps</span><span class="value" data-field="player_metrics_network_bitrate_mbps">${session.player_metrics_network_bitrate_mbps ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Loop Count</span><span class="value" data-field="player_metrics_loop_count">${session.player_metrics_loop_count_player ?? '0'}</span></div>
                            <div class="session-item"><span class="label">Profile Shifts</span><span class="value" data-field="player_metrics_profile_shift_count">${session.player_metrics_profile_shift_count ?? '0'}</span></div>
                            <div class="session-item"><span class="label">Frames Displayed</span><span class="value" data-field="player_metrics_frames_displayed">${session.player_metrics_frames_displayed ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Dropped Frames</span><span class="value" data-field="player_metrics_dropped_frames">${session.player_metrics_dropped_frames ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Stalls</span><span class="value" data-field="player_metrics_stall_count">${session.player_metrics_stall_count ?? '—'}</span></div>
                            <div class="session-item"><span class="label">Player Restarts</span><span class="value" data-field="player_restarts">${session.player_restarts ?? '0'}</span></div>
                            <div class="session-item"><span class="label">Stall Time</span><span class="value" data-field="player_metrics_stall_time_s">${formatSeconds(session.player_metrics_stall_time_s)}</span></div>
                            <div class="session-item"><span class="label">Last Stall Time</span><span class="value" data-field="player_metrics_last_stall_time_s">${formatSeconds(session.player_metrics_last_stall_time_s)}</span></div>
                            <div class="session-item"><span class="label">Last Error</span><span class="value" data-field="player_metrics_error">${session.player_metrics_error || '—'}</span></div>
                            <div class="session-item"><span class="label">Source</span><span class="value" data-field="player_metrics_source">${session.player_metrics_source || '—'}</span></div>
                        </div>
                    </div>
                </div>

                <!-- Collapsible Fault Injection -->
                <div class="collapsible-section" data-section="fault-injection" data-default-open="${faultInjectionOpen}">
                    <div class="collapsible-header" data-toggle="fault-injection">
                        <span class="collapsible-icon">${faultInjectionOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Fault Injection</span>
                    </div>
                    <div class="collapsible-content" data-content="fault-injection" style="display: ${faultInjectionOpen ? 'block' : 'none'};">
                        <div class="fault-injection-section">
                            <div class="tabs-container">
                                <div class="tabs-header">
                                    <button class="tab-button active" data-tab="segment-failures">Segment</button>
                                    <button class="tab-button" data-tab="manifest-failures">Manifest</button>
                                    <button class="tab-button" data-tab="master-failures">Master</button>
                                    <button class="tab-button" data-tab="transport-faults">Transport</button>
                                    <button class="tab-button" data-tab="content-manipulation">Content</button>
                                </div>
                                <div class="tabs-content">
                                    <!-- Segment Tab -->
                                    <div class="tab-panel active" data-panel="segment-failures">
                                        <div class="fault-control-row">
                                            <label>Failure Type</label>
                                            <div class="radio-group">
                                                ${renderFailureTypeOptions(`segment_failure_type_${sessionId}`, session.segment_failure_type, segmentFailureTypes)}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Scope</label>
                                            <div class="checkbox-group">${renderSegmentOptions(sessionId, manifestVariants, segmentSelected)}</div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Mode</label>
                                            ${renderModeDropdown(`segment_failure_mode_${sessionId}`, session.segment_failure_mode || 'failures_per_seconds')}
                                        </div>
                                        <div class="range-row">
                                            <label>Consecutive</label>
                                            <input type="range" min="0" max="10" step="1" data-field="segment_consecutive_failures" value="${Number.isFinite(Number(session.segment_consecutive_failures)) && Number(session.segment_consecutive_failures) >= 0 ? Number(session.segment_consecutive_failures) : 1}">
                                            <span class="range-value">${Number.isFinite(Number(session.segment_consecutive_failures)) && Number(session.segment_consecutive_failures) >= 0 ? Number(session.segment_consecutive_failures) : 1}</span>
                                        </div>
                                        <div class="range-row">
                                            <label>Frequency</label>
                                            <input type="range" min="0" max="10" step="1" data-field="segment_failure_frequency" value="${Number.isFinite(Number(session.segment_failure_frequency)) && Number(session.segment_failure_frequency) >= 0 ? Number(session.segment_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.segment_failure_frequency)) && Number(session.segment_failure_frequency) >= 0 ? Number(session.segment_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Manifest Tab -->
                                    <div class="tab-panel" data-panel="manifest-failures">
                                        <div class="fault-control-row">
                                            <label>Failure Type</label>
                                            <div class="radio-group">
                                                ${renderFailureTypeOptions(`manifest_failure_type_${sessionId}`, session.manifest_failure_type)}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Scope</label>
                                            <div class="checkbox-group">${renderManifestOptions(sessionId, manifestVariants, manifestSelected)}</div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Mode</label>
                                            ${renderModeDropdown(`manifest_failure_mode_${sessionId}`, session.manifest_failure_mode || 'failures_per_seconds')}
                                        </div>
                                        <div class="range-row">
                                            <label>Consecutive</label>
                                            <input type="range" min="0" max="10" step="1" data-field="manifest_consecutive_failures" value="${Number.isFinite(Number(session.manifest_consecutive_failures)) && Number(session.manifest_consecutive_failures) >= 0 ? Number(session.manifest_consecutive_failures) : 1}">
                                            <span class="range-value">${Number.isFinite(Number(session.manifest_consecutive_failures)) && Number(session.manifest_consecutive_failures) >= 0 ? Number(session.manifest_consecutive_failures) : 1}</span>
                                        </div>
                                        <div class="range-row">
                                            <label>Frequency</label>
                                            <input type="range" min="0" max="10" step="1" data-field="manifest_failure_frequency" value="${Number.isFinite(Number(session.manifest_failure_frequency)) && Number(session.manifest_failure_frequency) >= 0 ? Number(session.manifest_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.manifest_failure_frequency)) && Number(session.manifest_failure_frequency) >= 0 ? Number(session.manifest_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Master Manifest Tab -->
                                    <div class="tab-panel" data-panel="master-failures">
                                        <div class="fault-control-row">
                                            <label>Failure Type</label>
                                            <div class="radio-group">
                                                ${renderFailureTypeOptions(`master_manifest_failure_type_${sessionId}`, session.master_manifest_failure_type)}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Mode</label>
                                            ${renderModeDropdown(`master_manifest_failure_mode_${sessionId}`, session.master_manifest_failure_mode || 'failures_per_seconds')}
                                        </div>
                                        <div class="range-row">
                                            <label>Consecutive</label>
                                            <input type="range" min="0" max="10" step="1" data-field="master_manifest_consecutive_failures" value="${Number.isFinite(Number(session.master_manifest_consecutive_failures)) && Number(session.master_manifest_consecutive_failures) >= 0 ? Number(session.master_manifest_consecutive_failures) : 1}">
                                            <span class="range-value">${Number.isFinite(Number(session.master_manifest_consecutive_failures)) && Number(session.master_manifest_consecutive_failures) >= 0 ? Number(session.master_manifest_consecutive_failures) : 1}</span>
                                        </div>
                                        <div class="range-row">
                                            <label>Frequency</label>
                                            <input type="range" min="0" max="10" step="1" data-field="master_manifest_failure_frequency" value="${Number.isFinite(Number(session.master_manifest_failure_frequency)) && Number(session.master_manifest_failure_frequency) >= 0 ? Number(session.master_manifest_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.master_manifest_failure_frequency)) && Number(session.master_manifest_failure_frequency) >= 0 ? Number(session.master_manifest_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Transport Faults Tab -->
                                    <div class="tab-panel" data-panel="transport-faults">
                                        <div class="fault-control-row">
                                            <label>Fault Type</label>
                                            <div class="radio-group">
                                                ${renderTransportFaultOptions(`transport_failure_type_${sessionId}`, transportFaultType)}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Mode</label>
                                            <div class="radio-group">
                                                ${renderTransportModeOptions(`transport_failure_mode_${sessionId}`, transportMode)}
                                            </div>
                                        </div>
                                        <div class="range-row">
                                            <label data-field="transport_consecutive_label">${transportConsecutiveRange.label}</label>
                                            <input
                                                type="range"
                                                min="${transportConsecutiveRange.min}"
                                                max="${transportConsecutiveRange.max}"
                                                step="${transportConsecutiveRange.step}"
                                                data-field="transport_consecutive_failures"
                                                value="${transportConsecutive}">
                                            <span class="range-value">${transportConsecutive}</span>
                                        </div>
                                        <div class="range-row">
                                            <label>Frequency (secs)</label>
                                            <input type="range" min="0" max="60" step="1" data-field="transport_failure_frequency" value="${transportOffSeconds}">
                                            <span class="range-value">${transportOffSeconds}</span>
                                        </div>
                                        <div class="session-item">
                                            <span class="label">State</span>
                                            <span class="value" data-field="transport_fault_state">${transportActive ? 'Active' : 'Idle'}</span>
                                        </div>
                                        <div class="session-item">
                                            <span class="label">Fault Counters</span>
                                            <span class="value" data-field="transport_fault_counters">Drop ${transportDropPackets} pkts · Reject ${transportRejectPackets} pkts</span>
                                        </div>
                                    </div>

                                    <!-- Content Manipulation Tab -->
                                    <div class="tab-panel" data-panel="content-manipulation">
                                        <div class="fault-control-row">
                                            <label>Strip CODEC Information</label>
                                            <div class="checkbox-group">
                                                <label>
                                                    <input type="checkbox" data-field="content_strip_codecs" ${getBool(session, 'content_strip_codecs') ? 'checked' : ''}>
                                                    Remove CODEC attributes from master playlist
                                                </label>
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Strip AVERAGE-BANDWIDTH</label>
                                            <div class="checkbox-group">
                                                <label>
                                                    <input type="checkbox" data-field="content_strip_average_bandwidth" ${getBool(session, 'content_strip_average_bandwidth') ? 'checked' : ''}>
                                                    Remove AVERAGE-BANDWIDTH from master playlist
                                                </label>
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Overstate Bandwidth</label>
                                            <div class="checkbox-group">
                                                <label>
                                                    <input type="checkbox" data-field="content_overstate_bandwidth" ${getBool(session, 'content_overstate_bandwidth') ? 'checked' : ''}>
                                                    Increase BANDWIDTH and AVERAGE-BANDWIDTH by 10%
                                                </label>
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Allowed Variants</label>
                                            <div class="checkbox-group">
                                                ${renderContentVariantOptions(sessionId, manifestVariants, getStringSlice(session, 'content_allowed_variants'))}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Live Offset</label>
                                            <div class="radio-group">
                                                <label><input type="radio" name="content_live_offset_${sessionId}" data-field="content_live_offset" value="none" ${!session.content_live_offset || session.content_live_offset === 'none' ? 'checked' : ''}> None</label>
                                                <label><input type="radio" name="content_live_offset_${sessionId}" data-field="content_live_offset" value="6" ${String(session.content_live_offset) === '6' ? 'checked' : ''}> 6s</label>
                                                <label><input type="radio" name="content_live_offset_${sessionId}" data-field="content_live_offset" value="18" ${String(session.content_live_offset) === '18' ? 'checked' : ''}> 18s</label>
                                                <label><input type="radio" name="content_live_offset_${sessionId}" data-field="content_live_offset" value="24" ${String(session.content_live_offset) === '24' ? 'checked' : ''}> 24s</label>
                                            </div>
                                            <div class="content-tab-note" style="margin-top:4px">
                                                Drives both the player's <strong>start time</strong> (<code>EXT-X-START:TIME-OFFSET</code>, master and variant) and its steady-state <strong>HOLD-BACK</strong> on 2s/6s variants. <code>PART-HOLD-BACK</code> on the LL variant is left untouched since it's a sub-second LL timing parameter, not a window-scale offset. Values below the spec minimum (3× target duration) may be rejected by AVPlayer.
                                            </div>
                                        </div>
                                        <div class="content-tab-note">
                                            <strong>Note:</strong> Content modifications apply to master playlist requests.
                                            For HLS, play content once to populate variant list, configure settings, then replay to apply changes.
                                        </div>
                                    </div>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <!-- Collapsible Server Timeouts -->
                <div class="collapsible-section" data-section="server-timeouts" data-default-open="${serverTimeoutsOpen}">
                    <div class="collapsible-header" data-toggle="server-timeouts">
                        <span class="collapsible-icon">${serverTimeoutsOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Server Timeouts</span>
                    </div>
                    <div class="collapsible-content" data-content="server-timeouts" style="display: ${serverTimeoutsOpen ? 'block' : 'none'};">
                        <div class="fault-injection-section">
                            <div class="fault-control-row">
                                <label>Apply To</label>
                                <div class="checkbox-group">
                                    <label><input type="checkbox" data-field="transfer_timeout_applies_segments" ${getBool(session, 'transfer_timeout_applies_segments') ? 'checked' : ''}>Segments</label>
                                    <label><input type="checkbox" data-field="transfer_timeout_applies_manifests" ${getBool(session, 'transfer_timeout_applies_manifests') ? 'checked' : ''}>Media manifests</label>
                                    <label><input type="checkbox" data-field="transfer_timeout_applies_master" ${getBool(session, 'transfer_timeout_applies_master') ? 'checked' : ''}>Master manifest</label>
                                </div>
                            </div>
                            <div class="range-row">
                                <label>Active timeout (s)</label>
                                <input type="range" min="0" max="30" step="1" data-field="transfer_active_timeout_seconds" value="${Number.isFinite(Number(session.transfer_active_timeout_seconds)) && Number(session.transfer_active_timeout_seconds) >= 0 ? Number(session.transfer_active_timeout_seconds) : 0}">
                                <span class="range-value">${Number.isFinite(Number(session.transfer_active_timeout_seconds)) && Number(session.transfer_active_timeout_seconds) >= 0 ? Number(session.transfer_active_timeout_seconds) : 0}</span>
                            </div>
                            <div class="range-row">
                                <label>Idle timeout (s)</label>
                                <input type="range" min="0" max="30" step="1" data-field="transfer_idle_timeout_seconds" value="${Number.isFinite(Number(session.transfer_idle_timeout_seconds)) && Number(session.transfer_idle_timeout_seconds) >= 0 ? Number(session.transfer_idle_timeout_seconds) : 0}">
                                <span class="range-value">${Number.isFinite(Number(session.transfer_idle_timeout_seconds)) && Number(session.transfer_idle_timeout_seconds) >= 0 ? Number(session.transfer_idle_timeout_seconds) : 0}</span>
                            </div>
                            <div class="session-item">
                                <span class="label">Fault Counters</span>
                                <span class="value" data-field="transfer_timeout_counters">Active ${Number(session.fault_count_transfer_active_timeout) || 0} · Idle ${Number(session.fault_count_transfer_idle_timeout) || 0}</span>
                            </div>
                        </div>
                    </div>
                </div>

                <!-- Collapsible Network Shaping Section -->
                <div class="collapsible-section" data-section="network-shaping" data-default-open="${networkShapingOpen}" data-net-shaping>
                    <div class="collapsible-header" data-toggle="network-shaping">
                        <span class="collapsible-icon">${networkShapingOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Network Shaping</span>
                    </div>
                    <div class="collapsible-content" data-content="network-shaping" style="display: ${networkShapingOpen ? 'block' : 'none'};">
                        <div class="network-shaping-section">
                            <!-- Basic Controls -->
                            <div class="shaping-basic-controls">
                                <div class="range-row">
                                    <label>Delay (ms)</label>
                                    <input type="range" min="0" max="250" step="5" data-field="shaping_delay_ms" value="${session.nftables_delay_ms || 0}">
                                    <span class="range-value">${session.nftables_delay_ms || 0}</span>
                                </div>
                                <div class="range-row">
                                    <label>Loss (%)</label>
                                    <input type="range" min="0" max="10" step="0.5" data-field="shaping_loss_pct" value="${session.nftables_packet_loss || 0}">
                                    <span class="range-value">${session.nftables_packet_loss || 0}</span>
                                </div>
                                <div class="range-row${usePattern ? ' range-row-disabled' : ''}" data-field="shaping_throughput_row">
                                    <label>Throughput (Mbps)</label>
                                    <input type="range" min="0" max="50" step="0.1" data-field="shaping_throughput_mbps" value="${session.nftables_bandwidth_mbps || 0}" ${usePattern ? 'disabled' : ''}>
                                    <span class="range-value">${session.nftables_bandwidth_mbps || 0}</span>
                                </div>
                            </div>

                            <!-- Pattern Controls Group -->
                            <div class="shaping-pattern-group">
                                <div class="shape-template-row shape-template-compact">
                                    <div class="shape-template-block">
                                        <label>Pattern</label>
                                        <div class="shape-pattern-modes compact" data-field="shaping_template_mode_group">
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_mode_${sessionId}" value="sliders" data-field="shaping_template_mode" ${templateMode === 'sliders' ? 'checked' : ''}>
                                                <span title="Use slider value">🎚 Sliders</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_mode_${sessionId}" value="square_wave" data-field="shaping_template_mode" ${templateMode === 'square_wave' ? 'checked' : ''}>
                                                <span title="Alternate max/min bitrate">▁▔ Square</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_mode_${sessionId}" value="ramp_up" data-field="shaping_template_mode" ${templateMode === 'ramp_up' ? 'checked' : ''}>
                                                <span title="Step low to high">↗ Ramp Up</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_mode_${sessionId}" value="ramp_down" data-field="shaping_template_mode" ${templateMode === 'ramp_down' ? 'checked' : ''}>
                                                <span title="Step high to low">↘ Ramp Down</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_mode_${sessionId}" value="pyramid" data-field="shaping_template_mode" ${templateMode === 'pyramid' ? 'checked' : ''}>
                                                <span title="Up then down">⛰ Pyramid</span>
                                            </label>
                                        </div>
                                    </div>
                                    <div class="shape-template-block">
                                        <label>Step Duration</label>
                                        <div class="shape-pattern-modes compact" data-field="shaping_default_step_seconds_group">
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_default_step_seconds_${sessionId}" value="6" data-field="shaping_default_step_seconds" ${selectedStepSeconds === 6 ? 'checked' : ''}>
                                                <span>6s</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_default_step_seconds_${sessionId}" value="12" data-field="shaping_default_step_seconds" ${selectedStepSeconds === 12 ? 'checked' : ''}>
                                                <span>12s</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_default_step_seconds_${sessionId}" value="18" data-field="shaping_default_step_seconds" ${selectedStepSeconds === 18 ? 'checked' : ''}>
                                                <span>18s</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_default_step_seconds_${sessionId}" value="24" data-field="shaping_default_step_seconds" ${selectedStepSeconds === 24 ? 'checked' : ''}>
                                                <span>24s</span>
                                            </label>
                                        </div>
                                    </div>
                                    <div class="shape-template-block">
                                        <label>Margin</label>
                                        <div class="shape-pattern-modes shape-margin-modes compact" data-field="shaping_template_margin_group">
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_margin_${sessionId}" value="0" data-field="shaping_template_margin_pct" ${marginPct === 0 ? 'checked' : ''}>
                                                <span>Exact</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_margin_${sessionId}" value="10" data-field="shaping_template_margin_pct" ${marginPct === 10 ? 'checked' : ''}>
                                                <span>+10%</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_margin_${sessionId}" value="25" data-field="shaping_template_margin_pct" ${marginPct === 25 ? 'checked' : ''}>
                                                <span>+25%</span>
                                            </label>
                                            <label class="shape-pattern-mode">
                                                <input type="radio" name="shaping_template_margin_${sessionId}" value="50" data-field="shaping_template_margin_pct" ${marginPct === 50 ? 'checked' : ''}>
                                                <span>+50%</span>
                                            </label>
                                        </div>
                                    </div>
                                </div>

                                <div class="shape-step-list" data-field="shaping_pattern_rows" style="display:${usePattern ? '' : 'none'};">
                                    ${initialSteps.map((step, idx) => renderPatternStepRow(idx, step, shapingPresets, { overheadMbps, marginPct })).join('')}
                                </div>
                                <div class="shape-step-actions" style="display:${usePattern ? '' : 'none'};">
                                    <button type="button" class="btn btn-secondary btn-mini" data-action="add-shaping-step">Add Step</button>
                                    <button type="button" class="btn btn-secondary btn-mini" data-action="clear-shaping-pattern">Clear</button>
                                </div>
                                <div class="shape-apply-pattern" data-field="shaping_apply_pattern_row" style="display:none;">
                                    <button type="button" class="btn btn-primary" data-action="apply-pattern">Apply Pattern</button>
                                    <button type="button" class="btn btn-secondary btn-mini" data-action="edit-pattern" style="display:none;">Edit Pattern</button>
                                </div>

                            </div>
                        </div>
                    </div>
                </div>

                <!-- Collapsible Bitrate Chart -->
                <div class="collapsible-section" data-section="bitrate-chart" data-default-open="${bitrateChartOpen}">
                    <div class="collapsible-header" data-toggle="bitrate-chart">
                        <span class="collapsible-icon">${bitrateChartOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Bitrate Chart</span>
                    </div>
                    <div class="collapsible-content" data-content="bitrate-chart" style="display: ${bitrateChartOpen ? 'block' : 'none'};">
                        <div class="chart-axis-row">
                            <label>Bitrate Y Max</label>
                            <div class="shape-pattern-modes" data-field="bitrate_chart_max_mbps_group">
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="auto" data-field="bitrate_chart_max_mbps" ${chartMaxMode === 'auto' ? 'checked' : ''}>
                                    <span>Auto</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="5" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '5' ? 'checked' : ''}>
                                    <span>5 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="10" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '10' ? 'checked' : ''}>
                                    <span>10 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="20" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '20' ? 'checked' : ''}>
                                    <span>20 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="30" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '30' ? 'checked' : ''}>
                                    <span>30 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="40" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '40' ? 'checked' : ''}>
                                    <span>40 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="50" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '50' ? 'checked' : ''}>
                                    <span>50 Mbps</span>
                                </label>
                                <label class="shape-pattern-mode">
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="100" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '100' ? 'checked' : ''}>
                                    <span>100 Mbps</span>
                                </label>
                            </div>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="reset-bitrate-zoom">Reset Zoom</button>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="pause-bitrate-chart">⏸ Pause</button>
                            <span class="chart-hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">Alt/⌥+scroll/drag to zoom · right-drag to pan</span>
                        </div>
                        <div class="chart-wrap events-timeline-wrap">
                            <div class="events-timeline-legend" data-field="events_timeline_legend"></div>
                            <div class="events-timeline" data-field="events_timeline"></div>
                        </div>
                        <div class="chart-wrap">
                            <canvas class="bandwidth-chart" data-field="bandwidth_chart"></canvas>
                        </div>
                        ${showBufferDepthChart ? `
                        <div class="chart-wrap">
                            <canvas class="buffer-depth-chart" data-field="buffer_depth_chart"></canvas>
                        </div>
                        <div class="chart-wrap">
                            <canvas class="video-fps-chart" data-field="video_fps_chart"></canvas>
                        </div>
                        ` : ''}
                    </div>
                </div>

                ${developerMode ? `
                <div class="collapsible-section" data-section="player-characterization" data-default-open="false">
                    <div class="collapsible-header" data-toggle="player-characterization">
                        <span class="collapsible-icon">▶</span>
                        <span class="collapsible-title">Player Characterization</span>
                    </div>
                    <div class="collapsible-content" data-content="player-characterization" style="display: none;">
                        <div data-field="player_characterization_host"></div>
                    </div>
                </div>
                ` : ''}

                <!-- Collapsible Network Log -->
                <div class="collapsible-section" data-section="network-log" data-default-open="false">
                    <div class="collapsible-header" data-toggle="network-log">
                        <span class="collapsible-icon">▶</span>
                        <span class="collapsible-title">Network Log</span>
                        <span class="collapsible-badge" data-field="network_log_count">0 requests</span>
                    </div>
                    <div class="collapsible-content" data-content="network-log" style="display: none;">
                        <div class="network-log-section">
                            <div class="network-log-controls">
                                <button type="button" class="btn btn-mini btn-secondary" data-action="refresh-network-log">Refresh</button>
                                <button type="button" class="btn btn-mini btn-secondary" data-action="save-har-snapshot" title="Save the current network timeline as a HAR file: downloads to your machine and adds it to the Incidents list">Download HAR</button>
                                <a href="/dashboard/incidents.html" target="_blank" rel="noopener" class="btn btn-mini btn-secondary" title="Browse saved HAR snapshots">Incidents</a>
                                <button type="button" class="btn btn-mini btn-secondary" data-action="network-log-follow">Following Latest</button>
                                <label class="network-log-filter" title="When checked, only faulted / non-success entries appear in the list.">
                                    <input type="checkbox" data-filter="hide-successful">
                                    Hide Successful
                                </label>
                            </div>
                            <div class="network-log-warning">
                                Transfer timings and derived Mbps are approximate, and measured <strong>downstream</strong> — from when go-proxy starts writing the response back to the client device until the last byte is flushed (proxy → player). They do <strong>not</strong> include the upstream fetch from go-proxy to go-live. The numbers are most reliable when the network is slow and transfers are large (especially video segments); short responses transfer in &lt;1 ms and round to noise.
                            </div>
                            <div class="network-log-waterfall-wrap">
                                <div class="netwf-summary" data-field="netwf_summary"></div>
                                <div class="netwf-overview-axis" data-field="netwf_overview_axis"></div>
                                <div class="netwf-overview" data-field="netwf_overview">
                                    <div class="netwf-overview-bars" data-field="netwf_overview_bars"></div>
                                    <div class="netwf-brush" data-field="netwf_brush" style="left:0%;width:100%;">
                                        <div class="netwf-brush-handle left" data-field="netwf_brush_handle_left"></div>
                                        <div class="netwf-brush-handle right" data-field="netwf_brush_handle_right"></div>
                                    </div>
                                </div>
                                <div class="network-log-waterfall-scroll" data-field="network_log_waterfall_scroll">
                                    <div class="network-log-waterfall" data-field="network_log_waterfall"></div>
                                </div>
                                <div class="network-log-waterfall-empty" data-field="network_log_waterfall_empty" style="display:none;">No requests to plot yet.</div>
                            </div>
                        </div>
                    </div>
                </div>

            </div>
        `;
    }

    function readSessionSettings(card) {
        const sessionId = card.dataset.sessionId;

        const getSelectValue = (name) => {
            const select = card.querySelector(`select[name="${name}"]`);
            return select ? select.value : 'none';
        };

        const getRadioValue = (name) => {
            const selected = card.querySelector(`input[name="${name}"]:checked`);
            return selected ? selected.value : 'none';
        };

        const segmentFailureType = getRadioValue(`segment_failure_type_${sessionId}`);
        const manifestFailureType = getRadioValue(`manifest_failure_type_${sessionId}`);
        const masterManifestFailureType = getRadioValue(`master_manifest_failure_type_${sessionId}`);
        const transportFaultType = getRadioValue(`transport_failure_type_${sessionId}`);

        const segmentMode = getSelectValue(`segment_failure_mode_${sessionId}`) || 'requests';
        const manifestMode = getSelectValue(`manifest_failure_mode_${sessionId}`) || 'requests';
        const masterManifestMode = getSelectValue(`master_manifest_failure_mode_${sessionId}`) || 'requests';
        const transportMode = normalizeTransportMode(getRadioValue(`transport_failure_mode_${sessionId}`));

        const segmentUnits = unitsFromMode(segmentMode);
        const manifestUnits = unitsFromMode(manifestMode);
        const masterManifestUnits = unitsFromMode(masterManifestMode);
        const transportUnits = transportUnitsFromMode(transportMode);

        const getRangeValue = (field) => {
            const input = card.querySelector(`input[data-field="${field}"]`);
            return input ? Number(input.value) : 0;
        };

        const manifestChecks = Array.from(card.querySelectorAll('input[data-field="manifest_failure_urls"]:checked'))
            .map(input => input.value);
        const segmentChecks = Array.from(card.querySelectorAll('input[data-field="segment_failure_urls"]:checked'))
            .map(input => input.value);
        
        // Transfer timeout settings
        const transferActiveTimeoutSeconds = getRangeValue('transfer_active_timeout_seconds');
        const transferIdleTimeoutSeconds = getRangeValue('transfer_idle_timeout_seconds');
        const transferAppliesSegments = !!card.querySelector('input[data-field="transfer_timeout_applies_segments"]')?.checked;
        const transferAppliesManifests = !!card.querySelector('input[data-field="transfer_timeout_applies_manifests"]')?.checked;
        const transferAppliesMaster = !!card.querySelector('input[data-field="transfer_timeout_applies_master"]')?.checked;

        // Content manipulation settings
        const contentStripCodecs = !!card.querySelector('input[data-field="content_strip_codecs"]')?.checked;
        const contentStripAvgBandwidth = !!card.querySelector('input[data-field="content_strip_average_bandwidth"]')?.checked;
        const contentOverstateBandwidth = !!card.querySelector('input[data-field="content_overstate_bandwidth"]')?.checked;
        const contentAllowedVariants = Array.from(card.querySelectorAll('input[data-field="content_allowed_variants"]:checked'))
            .map(input => input.value);
        const liveOffsetRaw = card.querySelector('input[data-field="content_live_offset"]:checked')?.value || 'none';
        const contentLiveOffset = liveOffsetRaw === 'none' ? 0 : Number(liveOffsetRaw) || 0;

        return {
            session_id: sessionId,
            segment_failure_at: null,
            segment_failure_recover_at: null,
            segment_failure_type: segmentFailureType,
            segment_failure_frequency: getRangeValue('segment_failure_frequency'),
            segment_consecutive_failures: getRangeValue('segment_consecutive_failures'),
            segment_failure_units: segmentUnits.consecutiveUnits,
            segment_consecutive_units: segmentUnits.consecutiveUnits,
            segment_frequency_units: segmentUnits.frequencyUnits,
            segment_failure_mode: segmentMode,
            segment_failure_urls: segmentChecks,
            manifest_failure_at: null,
            manifest_failure_recover_at: null,
            manifest_failure_type: manifestFailureType,
            manifest_failure_frequency: getRangeValue('manifest_failure_frequency'),
            manifest_consecutive_failures: getRangeValue('manifest_consecutive_failures'),
            manifest_failure_units: manifestUnits.consecutiveUnits,
            manifest_consecutive_units: manifestUnits.consecutiveUnits,
            manifest_frequency_units: manifestUnits.frequencyUnits,
            manifest_failure_mode: manifestMode,
            manifest_failure_urls: manifestChecks,
            master_manifest_failure_at: null,
            master_manifest_failure_recover_at: null,
            master_manifest_failure_type: masterManifestFailureType,
            master_manifest_failure_frequency: getRangeValue('master_manifest_failure_frequency'),
            master_manifest_consecutive_failures: getRangeValue('master_manifest_consecutive_failures'),
            master_manifest_failure_units: masterManifestUnits.consecutiveUnits,
            master_manifest_consecutive_units: masterManifestUnits.consecutiveUnits,
            master_manifest_frequency_units: masterManifestUnits.frequencyUnits,
            master_manifest_failure_mode: masterManifestMode,
            transport_failure_type: transportFaultType,
            transport_failure_frequency: getRangeValue('transport_failure_frequency'),
            transport_consecutive_failures: getRangeValue('transport_consecutive_failures'),
            transport_failure_units: transportUnits.consecutiveUnits,
            transport_consecutive_units: transportUnits.consecutiveUnits,
            transport_frequency_units: transportUnits.frequencyUnits,
            transport_failure_mode: transportMode,
            // Legacy aliases
            transport_fault_type: transportFaultType,
            transport_consecutive_seconds: getRangeValue('transport_consecutive_failures'),
            transport_frequency_seconds: getRangeValue('transport_failure_frequency'),
            transport_fault_on_seconds: getRangeValue('transport_consecutive_failures'),
            transport_fault_off_seconds: getRangeValue('transport_failure_frequency'),
            // Transfer timeouts
            transfer_active_timeout_seconds: transferActiveTimeoutSeconds,
            transfer_idle_timeout_seconds: transferIdleTimeoutSeconds,
            transfer_timeout_applies_segments: transferAppliesSegments,
            transfer_timeout_applies_manifests: transferAppliesManifests,
            transfer_timeout_applies_master: transferAppliesMaster,
            // Content manipulation
            content_strip_codecs: contentStripCodecs,
            content_strip_average_bandwidth: contentStripAvgBandwidth,
            content_overstate_bandwidth: contentOverstateBandwidth,
            content_live_offset: contentLiveOffset,
            content_allowed_variants: contentAllowedVariants.length > 0 ? contentAllowedVariants : []
        };
    }

    function readShapingPattern(card) {
        const getNumber = (selector, fallback) => {
            const input = card.querySelector(selector);
            if (!input) return fallback;
            const value = Number(input.value);
            if (!Number.isFinite(value)) return fallback;
            return value;
        };
        const segmentDurationSeconds = toPositiveNumber(card?.dataset?.segmentDurationSeconds, 6);
        const defaultStepSeconds = toPositiveNumber(
            Number(card.querySelector('input[data-field="shaping_default_step_seconds"]:checked')?.value || 12),
            12
        );
        const defaultSegments = Math.max(0.5, Math.round((defaultStepSeconds / segmentDurationSeconds) * 10) / 10);
        const selectedMode = card.querySelector('input[data-field="shaping_template_mode"]:checked')?.value || 'sliders';
        const selectedMarginPct = Number(card.querySelector('input[data-field="shaping_template_margin_pct"]:checked')?.value || 0);
        const rows = Array.from(card.querySelectorAll('.shape-step-row'));
        const rowSteps = rows.map(row => {
            const rate = Number(row.querySelector('input[data-field="shaping_step_mbps"]')?.value ?? 0);
            const secondsRaw = Number(row.querySelector('input[data-field="shaping_step_seconds"]')?.value ?? defaultStepSeconds);
            const seconds = toPositiveNumber(secondsRaw, defaultStepSeconds);
            const enabled = !!row.querySelector('input[data-field="shaping_step_enabled"]')?.checked;
            if (!Number.isFinite(rate) || rate < 0) {
                return null;
            }
            return {
                rate_mbps: Math.round(rate * 1000) / 1000,
                duration_seconds: Math.round(seconds * 10) / 10,
                enabled
            };
        }).filter(Boolean);
        const sliderRate = Number(card.querySelector('input[data-field="shaping_throughput_mbps"]')?.value ?? 0);
        const sliderStep = {
            rate_mbps: Number.isFinite(sliderRate) && sliderRate >= 0 ? Math.round(sliderRate * 1000) / 1000 : 0,
            duration_seconds: Math.round(defaultStepSeconds * 10) / 10,
            enabled: true
        };
        const steps = selectedMode === 'sliders' ? [sliderStep] : rowSteps;
        return {
            pattern_enabled: true,
            segment_duration_seconds: segmentDurationSeconds,
            default_segments: defaultSegments,
            default_step_seconds: defaultStepSeconds,
            template_mode: selectedMode,
            template_margin_pct: selectedMarginPct,
            steps
        };
    }

    function updatePatternDefaultLabel(card) {
        if (!card) return;
        const pattern = readShapingPattern(card);
        if (pattern && Number.isFinite(pattern.segment_duration_seconds)) {
            card.dataset.segmentDurationSeconds = String(pattern.segment_duration_seconds);
        }
    }

    function updateTransportModeUi(card) {
        if (!card) return;
        const sessionId = String(card.dataset.sessionId || '');
        if (!sessionId) return;
        const selected = card.querySelector(`input[name="transport_failure_mode_${sessionId}"]:checked`);
        const mode = normalizeTransportMode(selected ? selected.value : 'failures_per_seconds');
        const range = transportConsecutiveRangeForMode(mode);
        const label = card.querySelector('[data-field="transport_consecutive_label"]');
        const slider = card.querySelector('input[data-field="transport_consecutive_failures"]');
        const valueEl = slider ? slider.parentElement.querySelector('.range-value') : null;
        if (label) label.textContent = range.label;
        if (!slider) return;
        slider.min = String(range.min);
        slider.max = String(range.max);
        slider.step = String(range.step);
        let value = Number(slider.value);
        if (!Number.isFinite(value)) value = range.min;
        value = Math.max(range.min, Math.min(range.max, value));
        if (mode === 'failures_per_packets') {
            value = Math.round(value);
        } else {
            value = Math.round(value * 10) / 10;
        }
        slider.value = String(value);
        if (valueEl) valueEl.textContent = String(value);
    }

    function getCollapsibleState(section, fallback) {
        const store = window.TestingSessionUICollapseState;
        if (store && typeof store.get === 'function') {
            const value = store.get(section);
            if (typeof value === 'boolean') return value;
        }
        if (store && Object.prototype.hasOwnProperty.call(store, section)) {
            return !!store[section];
        }
        return fallback;
    }

    function setCollapsibleState(section, isOpen) {
        const store = window.TestingSessionUICollapseState;
        if (store && typeof store.set === 'function') {
            store.set(section, isOpen);
            return;
        }
        if (store) {
            store[section] = isOpen;
        }
    }

    function applyCollapsibleState(root) {
        const host = root || document;
        host.querySelectorAll('.collapsible-section').forEach(section => {
            const key = section.dataset.section;
            const content = section.querySelector('.collapsible-content');
            const icon = section.querySelector('.collapsible-icon');
            if (!key || !content) return;
            const fallback = section.dataset.defaultOpen === 'true';
            const isOpen = getCollapsibleState(key, fallback);
            content.style.display = isOpen ? 'block' : 'none';
            if (icon) icon.textContent = isOpen ? '▼' : '▶';
            if (key === 'network-log') {
                if (!networkLogDeveloperEnabled) {
                    return;
                }
                const card = section.closest('.session-card');
                const sessionId = card ? String(card.dataset.sessionId || '') : '';
                if (!sessionId) return;
                if (isOpen) {
                    updateNetworkLogFollowButton(card, sessionId);
                    startNetworkLogAutoRefresh(sessionId, card);
                } else {
                    stopNetworkLogAutoRefresh(sessionId);
                }
            }
        });
    }

    // Initialize collapsible sections and tabs
    function initializeUI() {
        // "All" checkbox toggles all sibling scope checkboxes
        document.addEventListener('change', (e) => {
            const cb = e.target;
            if (!cb || cb.type !== 'checkbox' || !cb.dataset.field) return;
            const field = cb.dataset.field;
            if (field !== 'segment_failure_urls' && field !== 'manifest_failure_urls') return;
            const group = cb.closest('.checkbox-group');
            if (!group) return;
            const allCheckboxes = group.querySelectorAll(`input[data-field="${field}"]`);
            const allCb = group.querySelector(`input[data-field="${field}"][value="All"]`);
            if (cb.value === 'All') {
                allCheckboxes.forEach(c => { c.checked = cb.checked; });
            } else if (allCb) {
                const others = Array.from(allCheckboxes).filter(c => c.value !== 'All');
                allCb.checked = others.every(c => c.checked);
            }
        });

        document.addEventListener('click', (e) => {
            const eventTarget = e && e.target;
            const targetElement = eventTarget instanceof Element
                ? eventTarget
                : (eventTarget && eventTarget.parentElement ? eventTarget.parentElement : null);
            if (!targetElement) {
                return;
            }

            const actionButton = targetElement.closest('[data-action]');
            if (actionButton) {
                const action = actionButton.dataset.action;
                if (action === 'network-log-follow') {
                    const card = actionButton.closest('.session-card');
                    const sessionId = card ? String(card.dataset.sessionId || '') : '';
                    if (sessionId) {
                        const nextFollow = !isNetworkLogFollowMode(sessionId);
                        setNetworkLogFollowMode(sessionId, nextFollow);
                        updateNetworkLogFollowButton(card, sessionId);
                        if (nextFollow) {
                            // Snap to the live point immediately —
                            // refresh the entry list, re-render so the
                            // brush hits the right edge, then make sure
                            // the row list is scrolled to the bottom
                            // even after layout settles.
                            updateNetworkLog(sessionId);
                            // Section has no internal scroll — the page
                            // scrolls instead. Bring the last row into
                            // view across two rAFs so the new entries
                            // (which may not be in the DOM yet) are
                            // covered.
                            const chartHost = card.querySelector('[data-field="network_log_waterfall"]');
                            const snapToLast = () => {
                                const rows = chartHost ? chartHost.children : null;
                                const last = rows && rows[rows.length - 1];
                                if (last) last.scrollIntoView({ block: 'end', behavior: 'auto' });
                            };
                            window.requestAnimationFrame(() => {
                                snapToLast();
                                window.requestAnimationFrame(snapToLast);
                            });
                        } else if (card) {
                            applyNetworkLogFilters(card);
                        }
                    }
                    return;
                }
                if (action === 'save-har-snapshot') {
                    const card = actionButton.closest('.session-card');
                    const sessionId = card ? String(card.dataset.sessionId || '') : '';
                    if (!sessionId) return;
                    actionButton.disabled = true;
                    fetch(`/api/session/${encodeURIComponent(sessionId)}/har/snapshot`, {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ reason: 'manual', source: 'dashboard' })
                    })
                        .then(r => r.json())
                        .then(async (data) => {
                            if (data && data.incident && data.incident.path) {
                                // Fetch the just-saved file as a blob and
                                // trigger the download from a blob: URL.
                                // Linking to /api/incidents/... directly
                                // triggers Chrome's "insecure download"
                                // block on plain-HTTP origins (mixed-
                                // content download). A blob URL is
                                // local-origin, so it isn't blocked.
                                const fileResp = await fetch(`/api/incidents/${data.incident.path}`);
                                if (!fileResp.ok) {
                                    throw new Error(`HAR fetch ${fileResp.status}`);
                                }
                                const blob = await fileResp.blob();
                                const url = URL.createObjectURL(blob);
                                const a = document.createElement('a');
                                a.href = url;
                                a.download = data.incident.filename || 'incident.har';
                                a.style.display = 'none';
                                document.body.appendChild(a);
                                a.click();
                                document.body.removeChild(a);
                                // Defer revoke so the click has time to start.
                                setTimeout(() => URL.revokeObjectURL(url), 1000);
                            } else if (data && data.error) {
                                window.alert(`HAR save failed: ${data.error}`);
                            }
                        })
                        .catch(err => window.alert(`HAR save failed: ${err}`))
                        .finally(() => { actionButton.disabled = false; });
                    return;
                }
            }

            // Handle collapsible toggles
            const toggle = targetElement.closest('[data-toggle]');
            if (toggle) {
                const section = toggle.dataset.toggle;
                const sectionEl = toggle.closest('.collapsible-section');
                const content = sectionEl
                    ? sectionEl.querySelector('.collapsible-content')
                    : document.querySelector(`[data-content="${section}"]`);
                const icon = toggle.querySelector('.collapsible-icon');
                if (content && icon) {
                    const isOpen = content.style.display !== 'none';
                    const nextOpen = !isOpen;
                    content.style.display = nextOpen ? 'block' : 'none';
                    icon.textContent = nextOpen ? '▼' : '▶';
                    if (section) {
                        setCollapsibleState(section, nextOpen);
                    }
                    if (section === 'bitrate-chart' && nextOpen) {
                        const card = sectionEl ? sectionEl.closest('.session-card') : null;
                        const sessionId = card ? card.dataset.sessionId : null;
                        if (sessionId) {
                            const event = new CustomEvent('testing-session:charts-resize', {
                                detail: { sessionId }
                            });
                            document.dispatchEvent(event);
                        }
                    }
                    if (section === 'network-shaping' && nextOpen) {
                        const card = sectionEl ? sectionEl.closest('.session-card') : null;
                        const scope = card || document;
                        const chartContent = scope.querySelector('[data-content="bitrate-chart"]');
                        const chartIcon = scope.querySelector('[data-toggle="bitrate-chart"] .collapsible-icon');
                        if (chartContent && chartIcon) {
                            chartContent.style.display = 'block';
                            chartIcon.textContent = '▼';
                            setCollapsibleState('bitrate-chart', true);
                            const sessionId = card ? card.dataset.sessionId : null;
                            if (sessionId) {
                                const event = new CustomEvent('testing-session:charts-resize', {
                                    detail: { sessionId }
                                });
                                document.dispatchEvent(event);
                            }
                        }
                    }
                    if (section === 'network-log' && nextOpen) {
                        const card = sectionEl ? sectionEl.closest('.session-card') : null;
                        const sessionId = card ? card.dataset.sessionId : null;
                        if (sessionId && window.TestingSessionUI) {
                            updateNetworkLogFollowButton(card, sessionId);
                            startNetworkLogAutoRefresh(sessionId, card);
                        }
                    }
                    if (section === 'network-log' && !nextOpen) {
                        const card = sectionEl ? sectionEl.closest('.session-card') : null;
                        const sessionId = card ? String(card.dataset.sessionId || '') : '';
                        if (sessionId) {
                            stopNetworkLogAutoRefresh(sessionId);
                        }
                    }
                }
            }

            // Handle tab switches
            const tabButton = targetElement.closest('.tab-button');
            if (tabButton) {
                const tabName = tabButton.dataset.tab;
                const container = tabButton.closest('.tabs-container');

                // Update buttons
                container.querySelectorAll('.tab-button').forEach(btn => btn.classList.remove('active'));
                tabButton.classList.add('active');

                // Update panels
                container.querySelectorAll('.tab-panel').forEach(panel => panel.classList.remove('active'));
                const targetPanel = container.querySelector(`[data-panel="${tabName}"]`);
                if (targetPanel) targetPanel.classList.add('active');
            }
        });

        applyCollapsibleState(document);
        if (!networkLogDeveloperEnabled) {
            stopAllNetworkLogAutoRefresh();
            return;
        }
        resumeNetworkLogAutoRefreshForVisiblePanels();
        // Native waterfall layouts re-flow on the next refresh tick;
        // no explicit redraw needed on window resize.
        document.addEventListener('visibilitychange', () => {
            if (document.hidden) {
                stopAllNetworkLogAutoRefresh();
                return;
            }
            resumeNetworkLogAutoRefreshForVisiblePanels();
        });
    }

    function stopNetworkLogAutoRefresh(sessionId) {
        const key = String(sessionId || '');
        if (!key) return;
        const timer = networkLogAutoRefreshTimers.get(key);
        if (timer) {
            clearInterval(timer);
            networkLogAutoRefreshTimers.delete(key);
        }
    }

    function stopAllNetworkLogAutoRefresh() {
        Array.from(networkLogAutoRefreshTimers.keys()).forEach((sessionId) => {
            stopNetworkLogAutoRefresh(sessionId);
        });
    }

    function startNetworkLogAutoRefresh(sessionId, card) {
        if (!networkLogDeveloperEnabled) return;
        const key = String(sessionId || '');
        if (!key || document.hidden) return;
        const hostCard = card || document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!hostCard) return;
        if (networkLogAutoRefreshTimers.has(key)) return;
        updateNetworkLog(key, { skipIfInFlight: true });
        const timer = setInterval(() => {
            updateNetworkLog(key, { skipIfInFlight: true });
        }, networkLogAutoRefreshMs);
        networkLogAutoRefreshTimers.set(key, timer);
    }

    function isNetworkLogFollowMode(sessionId) {
        const key = String(sessionId || '');
        // Default ON — with the brushable overview, the user can pan
        // away whenever they want; the live tail is the more useful
        // initial state.
        return networkWaterfallFollowModeBySession.get(key) !== false;
    }

    function setNetworkLogFollowMode(sessionId, enabled) {
        const key = String(sessionId || '');
        if (!key) return;
        networkWaterfallFollowModeBySession.set(key, !!enabled);
    }

    function updateNetworkLogFollowButton(card, sessionId) {
        const hostCard = card || document.querySelector(`.session-card[data-session-id="${sessionId}"]`);
        if (!hostCard) return;
        const button = hostCard.querySelector('[data-action="network-log-follow"]');
        if (!button) return;
        const following = isNetworkLogFollowMode(sessionId);
        button.textContent = 'Following Latest';
        button.setAttribute('aria-pressed', following ? 'true' : 'false');
        button.classList.toggle('btn-primary', following);
        button.classList.toggle('btn-secondary', !following);
    }

    function scrollWaterfallToLatestRow(state) {
        if (!state || !state.host) return;
        const applyScroll = () => {
            if (!state.host) return;
            const leftContent = state.host.querySelector('.vis-panel.vis-left .vis-content');
            const centerContent = state.host.querySelector('.vis-panel.vis-center .vis-content');
            if (!leftContent && !centerContent) return;

            const leftMax = leftContent ? Math.max(0, leftContent.scrollHeight - leftContent.clientHeight) : 0;
            const centerMax = centerContent ? Math.max(0, centerContent.scrollHeight - centerContent.clientHeight) : 0;
            const target = Math.max(leftMax, centerMax);

            if (leftContent) leftContent.scrollTop = target;
            if (centerContent) centerContent.scrollTop = target;

            const leftAtBottom = !leftContent || Math.abs(leftMax - leftContent.scrollTop) <= 1;
            const centerAtBottom = !centerContent || Math.abs(centerMax - centerContent.scrollTop) <= 1;
            return leftAtBottom && centerAtBottom;
        };

        if (typeof window !== 'undefined' && typeof window.requestAnimationFrame === 'function') {
            let attempts = 0;
            const tick = () => {
                attempts += 1;
                const done = applyScroll();
                if (!done && attempts < 8) {
                    window.requestAnimationFrame(tick);
                }
            };
            window.requestAnimationFrame(() => {
                window.requestAnimationFrame(tick);
            });
            return;
        }
        let tries = 0;
        const retry = () => {
            tries += 1;
            const done = applyScroll();
            if (!done && tries < 5) {
                setTimeout(retry, 16);
            }
        };
        setTimeout(retry, 0);
    }

    function resumeNetworkLogAutoRefreshForVisiblePanels() {
        if (!networkLogDeveloperEnabled) {
            stopAllNetworkLogAutoRefresh();
            return;
        }
        document.querySelectorAll('.session-card').forEach((card) => {
            const sessionId = String(card.dataset.sessionId || '');
            if (!sessionId) return;
            const content = card.querySelector('[data-content="network-log"]');
            if (!content || content.style.display === 'none') {
                stopNetworkLogAutoRefresh(sessionId);
                return;
            }
            updateNetworkLogFollowButton(card, sessionId);
            startNetworkLogAutoRefresh(sessionId, card);
        });
    }

    // Network Log Functions
    function formatBytes(bytes) {
        if (!bytes || bytes === 0) return '—';
        if (bytes < 1024) return bytes + ' B';
        if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
        return (bytes / (1024 * 1024)).toFixed(2) + ' MB';
    }

    function formatMilliseconds(ms) {
        if (!ms && ms !== 0) return '—';
        if (ms < 1) return '<1ms';
        if (ms < 1000) return Math.round(ms) + 'ms';
        return (ms / 1000).toFixed(2) + 's';
    }

    function formatMbpsFromBytesAndMs(bytes, transferMs) {
        const bytesNum = Number(bytes || 0);
        const msNum = Number(transferMs || 0);
        if (!Number.isFinite(bytesNum) || !Number.isFinite(msNum) || bytesNum <= 0 || msNum <= 0) {
            return '—';
        }
        const mbps = (bytesNum * 8) / (msNum * 1000);
        if (!Number.isFinite(mbps) || mbps <= 0) {
            return '—';
        }
        return `${mbps.toFixed(2)} Mbps`;
    }

    // Fixed-precision formatters for the row table. Decimal count is
    // constant across rows so a column scans cleanly (e.g. "5.00",
    // "12.30", "145.00" rather than "5", "12.3", "145").
    function formatKBNumber(bytes) {
        const n = Number(bytes || 0);
        if (!Number.isFinite(n) || n <= 0) return '—';
        return (n / 1024).toFixed(1);
    }

    function formatMbpsNumber(bytes, transferMs) {
        const bytesNum = Number(bytes || 0);
        const msNum = Number(transferMs || 0);
        if (!Number.isFinite(bytesNum) || !Number.isFinite(msNum) || bytesNum <= 0 || msNum <= 0) {
            return '—';
        }
        const mbps = (bytesNum * 8) / (msNum * 1000);
        if (!Number.isFinite(mbps) || mbps <= 0) return '—';
        return mbps.toFixed(2);
    }

    function escapeHtml(value) {
        return String(value || '')
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function formatColumn(value, width, alignRight = false) {
        const text = String(value ?? '—');
        let clipped = text;
        if (clipped.length > width) {
            clipped = width >= 2 ? `${clipped.slice(0, width - 1)}…` : clipped.slice(0, width);
        }
        return alignRight ? clipped.padStart(width, ' ') : clipped.padEnd(width, ' ');
    }

    function hasVisTimeline() {
        return (
            typeof window !== 'undefined' &&
            window.vis &&
            typeof window.vis.DataSet === 'function' &&
            typeof window.vis.Timeline === 'function'
        );
    }

    function getFilteredNetworkEntries(card, sessionId) {
        const entries = networkLogEntriesBySession.get(String(sessionId)) || [];
        // Faults are always shown. "Hide Successful" defaults off, so
        // by default we show everything; tick it to focus on problems.
        const hideSuccessful = card.querySelector('[data-filter="hide-successful"]')?.checked ?? false;
        return entries.filter((entry) => entry.faulted || !hideSuccessful);
    }

    // Derive segment duration from the URL conventions used by go-live:
    //   /go-live/<content>/2s/...    -> 2s segments
    //   /go-live/<content>/6s/...    -> 6s segments
    //   /go-live/<content>/ll/...    -> LL partials (~200ms part target;
    //                                    use 1s as a conservative
    //                                    "normal cadence")
    //   playlist_<N>s_<variant>.m3u8 -> N seconds (also matches DASH-side)
    // Anything we can't classify falls back to the 6s default.
    function waterfallSegmentDurationMsFor(row) {
        const url = String(row.entry.url || row.entry.path || row.pathDisplay || '').toLowerCase();
        if (!url) return 6000;
        const lowLatency = /(^|[/_])ll([/_.]|$)/.test(url);
        if (lowLatency) return 1000;
        const m = url.match(/(?:[/_])(\d{1,2})s(?:[/_])/);
        if (m) {
            const secs = parseInt(m[1], 10);
            if (secs > 0 && secs <= 30) return secs * 1000;
        }
        const m2 = url.match(/playlist_(\d{1,2})s[_-]/);
        if (m2) {
            const secs = parseInt(m2[1], 10);
            if (secs > 0 && secs <= 30) return secs * 1000;
        }
        return 6000;
    }

    function buildWaterfallRows(entries) {
        const decodeSegment = (value) => {
            if (!value) return '';
            try {
                return decodeURIComponent(value);
            } catch (err) {
                return value;
            }
        };
        const parsePathAndVariant = (entry) => {
            let pathname = String(entry.path || '');
            if (!pathname && entry.url) {
                try {
                    pathname = new URL(entry.url).pathname || '';
                } catch (err) {
                    pathname = String(entry.url || '');
                }
            }
            const rawSegments = pathname.split('/').filter(Boolean);
            const segments = rawSegments.map((segment) => decodeSegment(segment));
            const filename = segments[segments.length - 1] || decodeSegment(pathname) || 'request';

            let pathSegments = segments.slice();
            if (pathSegments.length >= 3 && (pathSegments[0] === 'go-live' || pathSegments[0] === 'go-proxy')) {
                // Hide route and content title segment: /go-live/<content>/<rest...> -> /<rest...>
                pathSegments = pathSegments.slice(2);
            }
            const pathDisplay = pathSegments.join('/') || filename;

            let variantName = '';
            let variantKbps = '';

            const playlistMatch = filename.match(/playlist_\d+s_(audio|\d{3,4}p)(?:[_-](\d{3,6})kbps)?\.m3u8/i);
            if (playlistMatch) {
                variantName = String(playlistMatch[1] || '');
                variantKbps = String(playlistMatch[2] || '');
            }

            if (!variantName) {
                for (let i = pathSegments.length - 1; i >= 0; i -= 1) {
                    const seg = String(pathSegments[i] || '');
                    if (/^\d{3,4}p$/i.test(seg) || /^audio$/i.test(seg)) {
                        variantName = seg;
                        break;
                    }
                }
            }

            if (!variantKbps) {
                const kbpsMatch = pathname.match(/(\d{3,6})kbps/i);
                if (kbpsMatch) {
                    variantKbps = String(kbpsMatch[1] || '');
                }
            }

            let variantLabel = '';
            if (variantName) {
                variantLabel = variantName.toLowerCase() === 'audio' ? 'audio' : variantName;
                if (variantKbps) {
                    variantLabel += `/${variantKbps}kbps`;
                }
            }

            return { filename, pathDisplay, variantLabel };
        };

        const rows = entries.slice().map((entry, index) => {
            const timestamp = Date.parse(entry.timestamp || '') || 0;
            // Upstream-perspective phases (proxy → origin) — surfaced in the
            // tooltip for forensics but not part of the player-perceived bar.
            const dns = Number(entry.dns_ms || 0);
            const connect = Number(entry.connect_ms || 0);
            const tls = Number(entry.tls_ms || 0);
            const ttfb = Number(entry.ttfb_ms || 0);
            const total = Number(entry.total_ms || 0);
            const transferRaw = Number(entry.transfer_ms || 0);
            // Player-perceived wait — request received → first response byte
            // sent. Falls back to the legacy upstream-derived value for
            // entries written before the downstream-timings change (#272).
            const clientWaitRaw = Number(entry.client_wait_ms || 0);
            const handshake = dns + connect + tls;
            const wait = clientWaitRaw > 0
                ? clientWaitRaw
                : Math.max(0, ttfb - handshake);
            const transfer = transferRaw > 0 ? transferRaw : Math.max(0, total - ttfb);
            // The bar represents the player's view: just wait + transfer.
            // The upstream handshake phases are out-of-band (proxy ↔ origin)
            // and don't count toward what the player saw.
            const duration = clientWaitRaw > 0
                ? Math.max(0, wait + transfer)
                : Math.max(0, dns + connect + tls + wait + transfer);
            const parsed = parsePathAndVariant(entry);
            const prefix = entry.faulted ? '!' : '';
            return {
                index,
                entry,
                timestamp,
                filename: parsed.filename,
                pathDisplay: parsed.pathDisplay,
                variantLabel: parsed.variantLabel,
                label: `${prefix}${entry.method || 'GET'} ${parsed.pathDisplay}`,
                dns,
                connect,
                tls,
                wait,
                transfer,
                duration
            };
        });
        const ordered = rows.filter((row) => row.timestamp > 0).sort((a, b) => a.timestamp - b.timestamp);
        if (!ordered.length) return ordered;
        // Tag re-requests: same URL+method seen earlier in the session.
        // `attempt` / `totalAttempts` give chronological context; the
        // narrower `is_retry` flag fires only when the gap from the
        // previous fetch is suspiciously short — faster than a normal
        // HLS playlist refresh / segment refetch cadence. The cutoff
        // is *half the segment duration* parsed from the URL path
        // (.../2s/..., .../6s/..., .../ll/..., or playlist_2s_...).
        // Routine periodic refetches won't get flagged; rapid retries
        // after a failure will.
        const attemptByKey = new Map();
        const lastTimeByKey = new Map();
        for (const row of ordered) {
            const k = `${row.entry.method || 'GET'} ${row.entry.url || row.entry.path || ''}`;
            const next = (attemptByKey.get(k) || 0) + 1;
            attemptByKey.set(k, next);
            row.attempt = next;
            const prevTs = lastTimeByKey.get(k);
            row.segment_duration_ms = waterfallSegmentDurationMsFor(row);
            const threshold = row.segment_duration_ms / 2;
            row.retry_threshold_ms = threshold;
            row.is_retry = next > 1 && prevTs !== undefined && (row.timestamp - prevTs) < threshold;
            row.gap_from_prev_ms = prevTs !== undefined ? row.timestamp - prevTs : null;
            lastTimeByKey.set(k, row.timestamp);
            row.totalAttempts = 0; // filled in below
        }
        for (const row of ordered) {
            const k = `${row.entry.method || 'GET'} ${row.entry.url || row.entry.path || ''}`;
            row.totalAttempts = attemptByKey.get(k) || 1;
        }
        const newestTimestamp = ordered[ordered.length - 1].timestamp;
        const cutoff = newestTimestamp - networkWaterfallRollingWindowMs;
        return ordered.filter((row) => (row.timestamp + row.duration) >= cutoff);
    }


    // Global-axis waterfall with a brushable overview pane (issue
    // #291). The overview shows the full session at small scale; the
    // user drags the brush to pick a time window, and the bars in the
    // main view position themselves within that window. Chrome
    // DevTools' Network panel pattern.
    function updateNetworkWaterfall(card, sessionId) {
        const key = String(sessionId);
        const chartHost = card.querySelector('[data-field="network_log_waterfall"]');
        const scrollHost = card.querySelector('[data-field="network_log_waterfall_scroll"]');
        const emptyHost = card.querySelector('[data-field="network_log_waterfall_empty"]');
        const overviewEl = card.querySelector('[data-field="netwf_overview"]');
        const overviewBars = card.querySelector('[data-field="netwf_overview_bars"]');
        const brushEl = card.querySelector('[data-field="netwf_brush"]');
        if (!chartHost || !emptyHost) return;
        applyPersistedWaterfallColumns(chartHost);

        const rows = buildWaterfallRows(getFilteredNetworkEntries(card, key));
        if (!rows.length) {
            emptyHost.textContent = 'No requests to plot yet.';
            emptyHost.style.display = 'block';
            chartHost.replaceChildren();
            if (overviewBars) overviewBars.replaceChildren();
            networkWaterfallRenderSignatureBySession.delete(key);
            networkWaterfallRowsBySession.delete(key);
            return;
        }
        emptyHost.style.display = 'none';

        // Full session range (the overview always shows this).
        const dataStartMs = rows[0].timestamp;
        const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
        const fullSpan = Math.max(50, dataEndMs - dataStartMs);

        // Brush state — Following Latest = stick to the right edge.
        // Default: zoom to the most recent 2 minutes (or full session
        // if shorter). 2 min is also the floor we enforce on user
        // resize so bars stay scannable.
        let brush = networkWaterfallBrushBySession.get(key);
        if (!brush) {
            const initialSpan = Math.min(fullSpan, networkWaterfallMinBrushMs);
            brush = { startMs: dataEndMs - initialSpan, endMs: dataEndMs, follow: true };
            networkWaterfallBrushBySession.set(key, brush);
        }
        // Sync brush.follow with the Follow Latest button. Clicking
        // the button re-engages right-edge stickiness even if the user
        // had previously dragged the brush.
        brush.follow = isNetworkLogFollowMode(key);
        if (brush.follow) {
            const span = brush.endMs - brush.startMs;
            brush.endMs = dataEndMs;
            brush.startMs = Math.max(dataStartMs, brush.endMs - span);
        }
        // Clamp to data range and enforce the minimum brush width.
        if (brush.endMs > dataEndMs) brush.endMs = dataEndMs;
        if (brush.startMs < dataStartMs) brush.startMs = dataStartMs;
        if (brush.endMs - brush.startMs < networkWaterfallMinBrushMs) {
            // Try to expand left first; if there's not enough history,
            // accept whatever we can fit (full session shorter than
            // the minimum is fine — the floor only applies when the
            // user could have a wider view).
            brush.startMs = Math.max(dataStartMs, brush.endMs - networkWaterfallMinBrushMs);
        }

        // Summary row: total + categorical counts. Read at a glance
        // before the user starts panning the brush.
        const summaryEl = card.querySelector('[data-field="netwf_summary"]');
        if (summaryEl) {
            renderWaterfallSummary(summaryEl, rows, dataStartMs, dataEndMs);
        }

        // Time labels above the overview pane — full session range,
        // independent of the brush. So the user sees both the
        // big-picture wall clock (this row) and the zoomed-in
        // wall clock (the row above the bars below).
        const overviewAxis = card.querySelector('[data-field="netwf_overview_axis"]');
        if (overviewAxis) {
            renderWaterfallAxisTicks(overviewAxis, dataStartMs, dataEndMs);
        }

        // Render overview ticks (one per row, positioned by absolute
        // time). Re-render is cheap; we redraw on every refresh.
        // Apply the same status/fault classes the main rows use so
        // bad requests stand out on the strip too.
        if (overviewBars) {
            overviewBars.replaceChildren();
            for (const row of rows) {
                const left = ((row.timestamp - dataStartMs) / fullSpan) * 100;
                const width = Math.max(0.05, (Math.max(50, row.duration) / fullSpan) * 100);
                const tick = document.createElement('div');
                tick.className = 'netwf-overview-tick' + waterfallRowStatusClasses(row);
                tick.style.left = `${left.toFixed(3)}%`;
                tick.style.width = `${width.toFixed(3)}%`;
                overviewBars.appendChild(tick);
            }
        }

        // Position the brush to match its current state.
        if (brushEl) {
            const left = ((brush.startMs - dataStartMs) / fullSpan) * 100;
            const width = ((brush.endMs - brush.startMs) / fullSpan) * 100;
            brushEl.style.left = `${Math.max(0, left).toFixed(3)}%`;
            brushEl.style.width = `${Math.max(0.5, width).toFixed(3)}%`;
        }

        // Lazy-attach brush drag handlers (one set per overview).
        if (overviewEl && !overviewEl.dataset.netwfBound) {
            overviewEl.dataset.netwfBound = '1';
            attachBrushHandlers(card, overviewEl, brushEl);
        }

        // Filter the row list to requests that overlap the brush
        // window. The overview above keeps showing the full session;
        // the list below shows only what the user has selected.
        const visibleRows = rows.filter((row) => {
            const reqEnd = row.timestamp + Math.max(50, row.duration);
            return reqEnd >= brush.startMs && row.timestamp <= brush.endMs;
        });

        // Sticky axis row at the top of the list — column headers (with
        // drag-to-resize handles) on the left, tick marks across the
        // brush window on the right. One axis row in DOM, rebuilt only
        // when its content changes.
        let axisEl = chartHost.firstElementChild;
        if (!axisEl || !axisEl.classList.contains('netwf-axis')) {
            axisEl = buildWaterfallAxisRow(chartHost);
            if (chartHost.firstElementChild) {
                chartHost.insertBefore(axisEl, chartHost.firstElementChild);
            } else {
                chartHost.appendChild(axisEl);
            }
        }
        // Update the time-cell header text with current count.
        const timeHdr = axisEl.querySelector('.netwf-cell.time');
        if (timeHdr) timeHdr.textContent = `${visibleRows.length}/${rows.length}`;
        const axisScale = axisEl.querySelector('[data-field="netwf_axis_scale"]');
        if (axisScale) {
            renderWaterfallAxisTicks(axisScale, brush.startMs, brush.endMs);
        }

        // Diff-based DOM updates for the row list. Native scroll keeps
        // its position when only existing rows update / new ones
        // append. Note: the axis row sits at index 0, so data rows
        // start at child index 1.
        const sigs = visibleRows.map(buildRowSignature);
        const oldSigs = networkWaterfallRowSignaturesBySession.get(key) || [];
        const winKey = `${Math.round(brush.startMs)}-${Math.round(brush.endMs)}`;
        const winChanged = chartHost.dataset.netwfWin !== winKey;

        // Trim removed rows (filter shrunk OR ring buffer evicted).
        while (chartHost.children.length - 1 > visibleRows.length) {
            chartHost.removeChild(chartHost.lastElementChild);
        }
        visibleRows.forEach((row, idx) => {
            const domIdx = idx + 1; // axis is at index 0
            const existing = chartHost.children[domIdx];
            if (existing && !winChanged && oldSigs[idx] === sigs[idx]) return;
            const rowEl = buildWaterfallRowEl(row, idx, brush.startMs, brush.endMs);
            if (existing) {
                chartHost.replaceChild(rowEl, existing);
            } else {
                chartHost.appendChild(rowEl);
            }
        });
        chartHost.dataset.netwfWin = winKey;
        networkWaterfallRowSignaturesBySession.set(key, sigs);
        // Keep the unfiltered rows on the session so the brush handler
        // knows the full data range.
        networkWaterfallRowsBySession.set(key, rows);
        networkWaterfallRenderSignatureBySession.set(key, sigs.length + ':' + (sigs[sigs.length - 1] || ''));

        // Following Latest: bring the last row into view via the page
        // scroll. The waterfall has no internal scroll cap — the
        // section grows with the data — so we use the bottom row's
        // own scrollIntoView instead of `scrollTop = scrollHeight`.
        if (isNetworkLogFollowMode(key)) {
            const rowEls = chartHost.children;
            const lastRow = rowEls[rowEls.length - 1];
            if (lastRow) {
                lastRow.scrollIntoView({ block: 'end', behavior: 'auto' });
            }
        }
        updateNetworkLogFollowButton(card, key);

        // Lazy-attach an IntersectionObserver on the last-row sentinel
        // so when the user scrolls the page away from the live tail
        // (the bottom of the row list leaves the viewport) we
        // auto-disable Following Latest. One observer per chart host.
        if (chartHost && !chartHost.dataset.netwfFollowObserved) {
            chartHost.dataset.netwfFollowObserved = '1';
            attachWaterfallFollowObserver(card, chartHost);
        }

        // Lazy-attach hover tooltip handlers on the chart host.
        if (chartHost && !chartHost.dataset.netwfTipBound) {
            chartHost.dataset.netwfTipBound = '1';
            attachWaterfallHoverTooltip(chartHost);
        }
    }

    function attachWaterfallFollowObserver(card, chartHost) {
        if (typeof window.IntersectionObserver !== 'function') return;
        const sessionId = String(card.dataset.sessionId || '');
        if (!sessionId) return;
        const scrollHost = chartHost.parentElement;
        if (!scrollHost) return;
        // A 1px sentinel placed *after* the chart host (sibling, not
        // child) so the row-update trim loop in updateNetworkWaterfall
        // doesn't remove it. When the sentinel is off-screen, the
        // user has scrolled away from the live tail.
        const sentinel = document.createElement('div');
        sentinel.style.cssText = 'height:1px;width:1px;pointer-events:none;';
        sentinel.dataset.field = 'netwf_follow_sentinel';
        scrollHost.appendChild(sentinel);
        const io = new IntersectionObserver((entries) => {
            for (const entry of entries) {
                if (!entry.isIntersecting && isNetworkLogFollowMode(sessionId)) {
                    setNetworkLogFollowMode(sessionId, false);
                    updateNetworkLogFollowButton(card, sessionId);
                }
            }
        }, { threshold: 0 });
        io.observe(sentinel);
    }

    let netwfTooltipEl = null;
    function ensureNetwfTooltip() {
        if (netwfTooltipEl && document.body.contains(netwfTooltipEl)) return netwfTooltipEl;
        netwfTooltipEl = document.createElement('div');
        netwfTooltipEl.className = 'netwf-tooltip';
        document.body.appendChild(netwfTooltipEl);
        return netwfTooltipEl;
    }

    function attachWaterfallHoverTooltip(chartHost) {
        let activeRowEl = null;
        const onMove = (event) => {
            const rowEl = event.target.closest('.netwf-row');
            if (!rowEl || !chartHost.contains(rowEl) || !rowEl.__netwfRow) {
                hideTip();
                return;
            }
            if (rowEl !== activeRowEl) {
                activeRowEl = rowEl;
                renderTipFor(rowEl.__netwfRow);
            }
            positionTip(event.clientX, event.clientY);
        };
        const onLeave = () => hideTip();
        chartHost.addEventListener('mousemove', onMove);
        chartHost.addEventListener('mouseleave', onLeave);
    }

    function renderTipFor(row) {
        const tip = ensureNetwfTooltip();
        const entry = row.entry || {};
        const status = entry.status ? String(entry.status) : '—';
        const method = entry.method || 'GET';
        const bytesOut = Number(entry.bytes_out || 0);
        const bytesLabel = bytesOut > 0 ? formatBytes(bytesOut) : '—';
        const mbpsLabel = formatMbpsFromBytesAndMs(bytesOut, row.transfer);
        const startedAt = new Date(row.timestamp);
        const startedLabel = `${String(startedAt.getHours()).padStart(2,'0')}:${String(startedAt.getMinutes()).padStart(2,'0')}:${String(startedAt.getSeconds()).padStart(2,'0')}.${String(startedAt.getMilliseconds()).padStart(3,'0')}`;

        const phases = [
            { key: 'dns', name: 'DNS', value: row.dns },
            { key: 'connect', name: 'Connect', value: row.connect },
            { key: 'tls', name: 'TLS', value: row.tls },
            { key: 'wait', name: 'Wait (TTFB)', value: row.wait },
            { key: 'transfer', name: 'Receive', value: row.transfer }
        ].filter((p) => p.value > 0);

        const phaseRows = phases.map((p) =>
            `<div class="netwf-tooltip-phase-swatch ${p.key}"></div>`
            + `<div class="netwf-tooltip-phase-name">${escapeHtml(p.name)}</div>`
            + `<div class="netwf-tooltip-phase-value">${escapeHtml(formatMilliseconds(p.value))}</div>`
        ).join('');

        const variant = row.variantLabel ? row.variantLabel : '—';
        const filename = row.filename || '—';
        const faultBlock = entry.faulted
            ? `<div class="netwf-tooltip-fault"><strong>FAULT</strong>: ${escapeHtml(entry.fault_type || 'unknown')}${entry.fault_action ? ` · ${escapeHtml(entry.fault_action)}` : ''}</div>`
            : '';
        const url = entry.url || entry.path || '';

        const attemptStr = row.totalAttempts > 1
            ? `${row.attempt} of ${row.totalAttempts}`
                + (row.gap_from_prev_ms !== null && row.gap_from_prev_ms !== undefined
                    ? ` · ${formatMilliseconds(row.gap_from_prev_ms)} since previous`
                    : '')
                + (row.is_retry
                    ? ` (quick retry · cutoff ${formatMilliseconds(row.retry_threshold_ms || 0)})`
                    : '')
            : '1';
        const reqRange = entry.request_range || '';
        const respRange = entry.response_content_range || '';
        const rangeStr = reqRange || respRange
            ? `${reqRange ? `req: ${reqRange}` : ''}${reqRange && respRange ? ' · ' : ''}${respRange ? `resp: ${respRange}` : ''}`
            : '';

        tip.innerHTML = `
            <div class="netwf-tooltip-head">${escapeHtml(method)} ${escapeHtml(filename)}</div>
            <dl class="netwf-tooltip-meta">
                <dt>Status</dt><dd>${escapeHtml(status)}${status === '206' ? ' (Partial Content)' : ''}</dd>
                <dt>Total</dt><dd>${escapeHtml(formatMilliseconds(row.duration))}</dd>
                <dt>Bytes</dt><dd>${escapeHtml(bytesLabel)} @ ${escapeHtml(mbpsLabel)}</dd>
                <dt>Variant</dt><dd>${escapeHtml(variant)}</dd>
                <dt>Started</dt><dd>${escapeHtml(startedLabel)}</dd>
                <dt>Attempt</dt><dd>${escapeHtml(attemptStr)}</dd>
                ${rangeStr ? `<dt>Range</dt><dd>${escapeHtml(rangeStr)}</dd>` : ''}
            </dl>
            <div class="netwf-tooltip-phases">${phaseRows || '<div></div><div class="netwf-tooltip-phase-name">no phase data</div><div></div>'}</div>
            ${faultBlock}
            ${url ? `<div class="netwf-tooltip-url">${escapeHtml(url)}</div>` : ''}
        `;
        tip.classList.add('visible');
    }

    function positionTip(clientX, clientY) {
        const tip = ensureNetwfTooltip();
        if (!tip.classList.contains('visible')) return;
        const margin = 12;
        // Read after innerHTML has been set so dimensions reflect content.
        const rect = tip.getBoundingClientRect();
        let left = clientX + 14;
        let top = clientY + 14;
        if (left + rect.width + margin > window.innerWidth) {
            left = clientX - rect.width - 14;
        }
        if (top + rect.height + margin > window.innerHeight) {
            top = clientY - rect.height - 14;
        }
        if (left < margin) left = margin;
        if (top < margin) top = margin;
        tip.style.left = `${left}px`;
        tip.style.top = `${top}px`;
    }

    function hideTip() {
        if (netwfTooltipEl) netwfTooltipEl.classList.remove('visible');
    }

    function buildRowSignature(row) {
        return `${row.timestamp}|${Math.round(row.duration)}|${row.entry.status || ''}|${row.entry.bytes_out || 0}|${row.label}|${row.entry.faulted ? 'F' : ''}|${row.entry.fault_type || ''}|${row.attempt || 1}|${row.is_retry ? 'R' : ''}|${row.entry.request_range || ''}`;
    }

    function waterfallRowStatusClasses(row) {
        const cls = [];
        const status = Number(row.entry.status) || 0;
        if (status === 206) cls.push(' status-206');
        else if (status >= 200 && status < 300) cls.push(' status-2xx');
        else if (status >= 300 && status < 400) cls.push(' status-3xx');
        else if (status >= 400 && status < 500) cls.push(' status-4xx');
        else if (status >= 500) cls.push(' status-5xx');

        const ft = String(row.entry.fault_type || '').toLowerCase();
        if (ft.includes('timeout')) cls.push(' fault-timeout');
        else if (ft.includes('corrupt') || ft.includes('partial') || isWaterfallRowIncomplete(row)) cls.push(' fault-incomplete');
        else if (row.entry.faulted) cls.push(' is-faulted');

        if (row.is_retry) cls.push(' is-retry');
        return cls.join('');
    }

    function isWaterfallRowIncomplete(row) {
        const ft = String(row.entry.fault_type || '').toLowerCase();
        if (ft.includes('timeout') || ft.includes('corrupt') || ft.includes('partial') || ft.includes('abandon')) return true;
        // Heuristic: faulted with bytes-out smaller than expected. We
        // don't always know "expected", but a faulted entry with a
        // 2xx status implies the proxy injected a partial/incomplete
        // response.
        const status = Number(row.entry.status) || 0;
        if (row.entry.faulted && status >= 200 && status < 300) return true;
        return false;
    }

    function buildWaterfallRowEl(row, idx, winStart, winEnd) {
        const el = document.createElement('div');
        el.className = 'netwf-row' + waterfallRowStatusClasses(row);
        el.dataset.rowIdx = String(idx);
        // Stash the row data on the DOM node so the hover tooltip can
        // build a rich expansion without re-querying the session map.
        el.__netwfRow = row;

        // Column cells. Widths come from CSS vars on the chart host —
        // resizing a header reflows every row in sync.
        const ts = new Date(row.timestamp);
        const tsLabel = `${String(ts.getHours()).padStart(2,'0')}:${String(ts.getMinutes()).padStart(2,'0')}:${String(ts.getSeconds()).padStart(2,'0')}.${String(ts.getMilliseconds()).padStart(3,'0')}`;
        const method = row.entry.method || 'GET';
        const bytesOut = Number(row.entry.bytes_out || 0);
        const bytesLabel = formatKBNumber(bytesOut);
        const mbpsLabel = formatMbpsNumber(bytesOut, row.transfer);
        const path = row.pathDisplay || row.filename || '';
        const flags = (row.is_retry ? '↻' : '') + (row.entry.faulted ? '!' : '');
        const statusCode = Number(row.entry.status) || 0;

        const cells = [
            { col: 'time', text: tsLabel },
            { col: 'flags', text: flags, color: row.is_retry ? '#be185d' : (row.entry.faulted ? '#7f1d1d' : '') },
            { col: 'method', text: method },
            { col: 'path', text: path, title: row.entry.url || row.entry.path || '' },
            { col: 'bytes', text: bytesLabel },
            { col: 'mbps', text: mbpsLabel },
            { col: 'status', text: statusCode > 0 ? String(statusCode) : '—' }
        ];
        for (const c of cells) {
            const cellEl = document.createElement('div');
            cellEl.className = `netwf-cell ${c.col}`;
            cellEl.textContent = c.text;
            if (c.title) cellEl.title = c.title;
            if (c.color) cellEl.style.color = c.color;
            el.appendChild(cellEl);
        }

        const trackEl = document.createElement('div');
        trackEl.className = 'netwf-row-track';

        const winSpan = Math.max(50, winEnd - winStart);
        const reqStart = row.timestamp;
        const reqEnd = row.timestamp + Math.max(50, row.duration);
        // Off-screen rows (entirely outside the brush): render an
        // empty track so vertical alignment with the URL list stays
        // correct.
        if (reqEnd >= winStart && reqStart <= winEnd) {
            const left = ((reqStart - winStart) / winSpan) * 100;
            const width = ((reqEnd - reqStart) / winSpan) * 100;
            const barEl = document.createElement('div');
            barEl.className = 'netwf-row-bar' + (isWaterfallRowIncomplete(row) ? ' is-incomplete' : '');
            barEl.style.left = `${Math.max(0, left).toFixed(3)}%`;
            barEl.style.width = `${Math.max(0.2, width).toFixed(3)}%`;
            // No native title — custom hover tooltip handles details.
            const phases = [
                { key: 'dns', value: row.dns },
                { key: 'connect', value: row.connect },
                { key: 'tls', value: row.tls },
                { key: 'wait', value: row.wait },
                { key: 'transfer', value: row.transfer }
            ];
            const total = phases.reduce((sum, p) => sum + Math.max(0, p.value), 0);
            for (const phase of phases) {
                if (phase.value <= 0) continue;
                const seg = document.createElement('div');
                seg.className = `netwf-row-phase ${phase.key}`;
                seg.style.flex = `${phase.value / total} 0 0`;
                barEl.appendChild(seg);
            }
            trackEl.appendChild(barEl);

            // Duration text just past the right edge of the bar. If
            // the bar is too far right to fit text after it, anchor
            // the text just before the bar's right edge instead.
            const right = Math.max(0, left) + Math.max(0.2, width);
            const durEl = document.createElement('div');
            durEl.className = 'netwf-row-duration';
            durEl.textContent = formatMilliseconds(row.duration);
            if (right < 90) {
                durEl.style.left = `calc(${right.toFixed(3)}% + 6px)`;
            } else {
                durEl.style.right = `${(100 - right + 0.5).toFixed(3)}%`;
            }
            trackEl.appendChild(durEl);
        }

        el.appendChild(trackEl);
        return el;
    }

    // Header column definitions — `key` matches CSS variable name
    // (`--netwf-col-${key}`) and the cell class. `min` is the smallest
    // pixel width we'll let the user drag the column to.
    const NETWF_COLUMNS = [
        { key: 'time',   label: 'Time',     min: 60 },
        { key: 'flags',  label: '',         min: 16 },
        { key: 'method', label: 'Method',   min: 30 },
        { key: 'path',   label: 'Path',     min: 60 },
        // Single-unit columns: header carries the unit, cells carry
        // tabular-aligned numbers. Bytes always rendered in KB,
        // throughput always in Mbps — easier to scan than mixed units.
        { key: 'bytes',  label: 'KB',       min: 50 },
        { key: 'mbps',   label: 'Mbps',     min: 50 },
        { key: 'status', label: 'Status',   min: 40 }
    ];

    function buildWaterfallAxisRow(chartHost) {
        const axisEl = document.createElement('div');
        axisEl.className = 'netwf-axis';
        for (const col of NETWF_COLUMNS) {
            const cell = document.createElement('div');
            cell.className = `netwf-cell ${col.key}`;
            cell.textContent = col.label;
            const resizer = document.createElement('div');
            resizer.className = 'netwf-cell-resizer';
            resizer.dataset.col = col.key;
            resizer.dataset.minPx = String(col.min);
            cell.appendChild(resizer);
            attachWaterfallColumnResizer(chartHost, resizer);
            axisEl.appendChild(cell);
        }
        const scale = document.createElement('div');
        scale.className = 'netwf-axis-scale';
        scale.dataset.field = 'netwf_axis_scale';
        axisEl.appendChild(scale);
        return axisEl;
    }

    function attachWaterfallColumnResizer(chartHost, resizer) {
        // Document-level mousemove/mouseup listeners are scoped to a
        // single drag — added on mousedown, removed on mouseup. This
        // avoids the "every axis rebuild adds another pair to document"
        // leak the older keep-alive form would create.
        resizer.addEventListener('mousedown', (e) => {
            const col = resizer.dataset.col;
            if (!col) return;
            const cell = resizer.parentElement;
            const startPx = cell.getBoundingClientRect().width;
            const drag = {
                col,
                startX: e.clientX,
                startPx,
                minPx: parseInt(resizer.dataset.minPx, 10) || 30,
                cssVar: `--netwf-col-${col}`
            };
            const onMove = (ev) => {
                const next = Math.max(drag.minPx, drag.startPx + (ev.clientX - drag.startX));
                chartHost.style.setProperty(drag.cssVar, `${Math.round(next)}px`);
            };
            const onUp = () => {
                document.removeEventListener('mousemove', onMove);
                document.removeEventListener('mouseup', onUp);
                resizer.classList.remove('dragging');
                document.body.style.cursor = '';
                // Persist the new width so it survives session-card
                // re-renders and page reloads.
                try {
                    const stored = JSON.parse(localStorage.getItem('netwf-cols-v2') || '{}');
                    stored[drag.col] = chartHost.style.getPropertyValue(`--netwf-col-${drag.col}`);
                    localStorage.setItem('netwf-cols-v2', JSON.stringify(stored));
                } catch (_) { /* no-op */ }
            };
            resizer.classList.add('dragging');
            document.body.style.cursor = 'col-resize';
            document.addEventListener('mousemove', onMove);
            document.addEventListener('mouseup', onUp);
            e.preventDefault();
            e.stopPropagation();
        });
    }

    // Re-apply persisted column widths to a chart host on first
    // render so user preferences survive page reloads.
    function applyPersistedWaterfallColumns(chartHost) {
        if (!chartHost || chartHost.dataset.netwfColsApplied) return;
        chartHost.dataset.netwfColsApplied = '1';
        try {
            const stored = JSON.parse(localStorage.getItem('netwf-cols-v2') || '{}');
            for (const col of NETWF_COLUMNS) {
                const v = stored[col.key];
                if (v) chartHost.style.setProperty(`--netwf-col-${col.key}`, v);
            }
        } catch (_) { /* no-op */ }
    }

    function renderWaterfallAxisTicks(host, winStart, winEnd) {
        host.replaceChildren();
        const span = winEnd - winStart;
        if (span <= 0) return;
        const targetTickCount = 6;
        const niceMs = pickNiceTickIntervalMs(span / targetTickCount);
        const firstTick = Math.ceil(winStart / niceMs) * niceMs;
        for (let t = firstTick; t <= winEnd; t += niceMs) {
            const left = ((t - winStart) / span) * 100;
            if (left < 0 || left > 100) continue;
            const tick = document.createElement('div');
            tick.className = 'netwf-axis-tick';
            tick.style.left = `${left.toFixed(3)}%`;
            host.appendChild(tick);
            const label = document.createElement('div');
            label.className = 'netwf-axis-tick-label';
            label.style.left = `${left.toFixed(3)}%`;
            label.textContent = formatAxisTickLabel(t);
            host.appendChild(label);
        }
        // Right-aligned summary: brush span (zoom level).
        const summary = document.createElement('div');
        summary.className = 'netwf-axis-summary';
        summary.textContent = `Δ ${formatMilliseconds(span)}`;
        host.appendChild(summary);
    }

    function renderWaterfallSummary(host, rows, dataStartMs, dataEndMs) {
        const counts = {
            total: rows.length,
            success: 0,
            partial: 0,
            client: 0,
            server: 0,
            faulted: 0,
            timeout: 0,
            disconnect: 0,
            retry: 0
        };
        for (const row of rows) {
            const status = Number(row.entry.status) || 0;
            if (status === 206) counts.partial += 1;
            else if (status >= 200 && status < 300) counts.success += 1;
            else if (status >= 400 && status < 500) counts.client += 1;
            else if (status >= 500) counts.server += 1;

            const ft = String(row.entry.fault_type || '').toLowerCase();
            if (ft.includes('timeout')) counts.timeout += 1;
            else if (ft.includes('client_disconnect') || ft === 'client_disconnect') counts.disconnect += 1;
            else if (row.entry.faulted) counts.faulted += 1;

            if (row.is_retry) counts.retry += 1;
        }
        const span = Math.max(0, dataEndMs - dataStartMs);
        const pill = (cls, label, count) => {
            const zero = count === 0 ? ' zero' : '';
            return `<span class="netwf-summary-pill ${cls}${zero}"><span class="count">${count}</span> ${escapeHtml(label)}</span>`;
        };
        host.innerHTML = [
            `<span class="netwf-summary-pill total"><span class="count">${counts.total}</span> requests</span>`,
            `<span class="netwf-summary-pill span">${escapeHtml(formatMilliseconds(span))} span</span>`,
            pill('success', '2xx', counts.success),
            pill('partial', '206', counts.partial),
            pill('client-error', '4xx', counts.client),
            pill('server-error', '5xx', counts.server),
            pill('faulted', 'faulted', counts.faulted),
            pill('timeout', 'timeouts', counts.timeout),
            pill('disconnect', 'disconnects', counts.disconnect),
            pill('retry', 'retries', counts.retry)
        ].join('');
    }

    function pickNiceTickIntervalMs(roughMs) {
        const candidates = [10, 25, 50, 100, 200, 250, 500, 1000, 2000, 5000, 10000, 15000, 30000, 60000, 120000, 300000];
        for (const c of candidates) if (c >= roughMs) return c;
        return Math.ceil(roughMs / 60000) * 60000;
    }

    function formatAxisTickLabel(absMs) {
        const d = new Date(absMs);
        const hh = String(d.getHours()).padStart(2, '0');
        const mm = String(d.getMinutes()).padStart(2, '0');
        const ss = String(d.getSeconds()).padStart(2, '0');
        return `${hh}:${mm}:${ss}`;
    }

    // Brush drag/resize handlers. The brush is positioned in % of the
    // overview's width, so we convert px deltas to % via the
    // overview's bounding rect. Drag-pan or drag-resize disables
    // Following Latest so the user's selection stays put.
    function attachBrushHandlers(card, overviewEl, brushEl) {
        if (!overviewEl) return;
        const sessionId = String(card.dataset.sessionId || '');
        if (!sessionId) return;

        let drag = null;

        const startDrag = (event, mode) => {
            const rect = overviewEl.getBoundingClientRect();
            if (rect.width <= 0) return;
            const rows = networkWaterfallRowsBySession.get(sessionId) || [];
            if (!rows.length) return;
            const dataStartMs = rows[0].timestamp;
            const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
            const fullSpan = Math.max(50, dataEndMs - dataStartMs);
            const brush = networkWaterfallBrushBySession.get(sessionId);
            if (!brush) return;
            drag = {
                mode,
                rect,
                fullSpan,
                dataStartMs,
                dataEndMs,
                startBrushStart: brush.startMs,
                startBrushEnd: brush.endMs,
                pointerStartX: event.clientX
            };
            brushEl?.classList.add('dragging');
            event.preventDefault();
            event.stopPropagation();
        };

        const onPointerMove = (event) => {
            if (!drag) return;
            const dxPx = event.clientX - drag.pointerStartX;
            const dxMs = (dxPx / drag.rect.width) * drag.fullSpan;
            const brush = networkWaterfallBrushBySession.get(sessionId);
            if (!brush) return;
            if (drag.mode === 'pan') {
                let newStart = drag.startBrushStart + dxMs;
                let newEnd = drag.startBrushEnd + dxMs;
                const span = newEnd - newStart;
                if (newStart < drag.dataStartMs) { newStart = drag.dataStartMs; newEnd = newStart + span; }
                if (newEnd > drag.dataEndMs) { newEnd = drag.dataEndMs; newStart = newEnd - span; }
                brush.startMs = newStart;
                brush.endMs = newEnd;
            } else if (drag.mode === 'resize-left') {
                let newStart = drag.startBrushStart + dxMs;
                if (newStart < drag.dataStartMs) newStart = drag.dataStartMs;
                // Enforce the minimum brush width — never let the user
                // drag the left handle within networkWaterfallMinBrushMs
                // of the right edge.
                if (newStart > brush.endMs - networkWaterfallMinBrushMs) {
                    newStart = brush.endMs - networkWaterfallMinBrushMs;
                }
                brush.startMs = newStart;
            } else if (drag.mode === 'resize-right') {
                let newEnd = drag.startBrushEnd + dxMs;
                if (newEnd > drag.dataEndMs) newEnd = drag.dataEndMs;
                if (newEnd < brush.startMs + networkWaterfallMinBrushMs) {
                    newEnd = brush.startMs + networkWaterfallMinBrushMs;
                }
                brush.endMs = newEnd;
            }
            brush.follow = false;
            // Disable Following Latest so the user's pick sticks.
            if (isNetworkLogFollowMode(sessionId)) {
                setNetworkLogFollowMode(sessionId, false);
                updateNetworkLogFollowButton(card, sessionId);
            }
            updateNetworkWaterfall(card, sessionId);
        };

        const onPointerUp = () => {
            if (!drag) return;
            drag = null;
            brushEl?.classList.remove('dragging');
        };

        // Brush body = pan. Handles = resize. Click on bare overview
        // jumps the brush centre to the click point.
        if (brushEl) {
            brushEl.addEventListener('mousedown', (e) => {
                if (e.target.classList.contains('netwf-brush-handle')) return;
                startDrag(e, 'pan');
            });
            const leftHandle = brushEl.querySelector('.netwf-brush-handle.left');
            const rightHandle = brushEl.querySelector('.netwf-brush-handle.right');
            if (leftHandle) leftHandle.addEventListener('mousedown', (e) => startDrag(e, 'resize-left'));
            if (rightHandle) rightHandle.addEventListener('mousedown', (e) => startDrag(e, 'resize-right'));
        }
        overviewEl.addEventListener('mousedown', (e) => {
            if (e.target !== overviewEl && !e.target.classList.contains('netwf-overview-bars')) return;
            const rect = overviewEl.getBoundingClientRect();
            const rows = networkWaterfallRowsBySession.get(sessionId) || [];
            if (!rows.length || rect.width <= 0) return;
            const dataStartMs = rows[0].timestamp;
            const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
            const fullSpan = Math.max(50, dataEndMs - dataStartMs);
            const fracX = (e.clientX - rect.left) / rect.width;
            const targetMs = dataStartMs + fracX * fullSpan;
            const brush = networkWaterfallBrushBySession.get(sessionId);
            if (!brush) return;
            const span = brush.endMs - brush.startMs;
            brush.startMs = Math.max(dataStartMs, targetMs - span / 2);
            brush.endMs = Math.min(dataEndMs, brush.startMs + span);
            brush.startMs = brush.endMs - span;
            brush.follow = false;
            if (isNetworkLogFollowMode(sessionId)) {
                setNetworkLogFollowMode(sessionId, false);
                updateNetworkLogFollowButton(card, sessionId);
            }
            updateNetworkWaterfall(card, sessionId);
        });

        document.addEventListener('mousemove', onPointerMove);
        document.addEventListener('mouseup', onPointerUp);
    }


    function updateNetworkLog(sessionId, options = {}) {
        if (!networkLogDeveloperEnabled) return;
        const key = String(sessionId || '');
        if (!key) return;
        if (options.skipIfInFlight && networkLogFetchInFlight.has(key)) return;

        const card = document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!card) {
            stopNetworkLogAutoRefresh(key);
            return;
        }

        const countBadge = card.querySelector('[data-field="network_log_count"]');
        networkLogFetchInFlight.add(key);

        fetch(`/api/session/${key}/network`)
            .then(response => response.json())
            .then(data => {
                const entries = data.entries || [];
                const count = entries.length;
                networkLogEntriesBySession.set(key, entries);

                // Update count badge
                if (countBadge) {
                    countBadge.textContent = `${count} request${count !== 1 ? 's' : ''}`;
                }
                applyNetworkLogFilters(card);
            })
            .catch(error => {
                console.error('Failed to fetch network log:', error);
                networkLogEntriesBySession.delete(key);
                if (countBadge) {
                    countBadge.textContent = '0 requests';
                }
                updateNetworkWaterfall(card, key);
            })
            .finally(() => {
                networkLogFetchInFlight.delete(key);
            });
    }

    function applyNetworkLogFilters(card) {
        const sessionId = String(card?.dataset?.sessionId || '');
        if (sessionId) {
            updateNetworkWaterfall(card, sessionId);
        }
    }

    // Initialize on load
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', initializeUI);
    } else {
        initializeUI();
    }

    window.TestingSessionUI = {
        renderSessionCard,
        renderPatternStepRowContent,
        readSessionSettings,
        readShapingPattern,
        updatePatternDefaultLabel,
        updateTransportModeUi,
        formatDate,
        formatDuration,
        applyCollapsibleState,
        updateNetworkLog,
        applyNetworkLogFilters,
        updateNetworkWaterfall
    };
})();
