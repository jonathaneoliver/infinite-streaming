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
    const networkWaterfallTimelines = new Map();
    const networkWaterfallViewBySession = new Map();

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
        const allChecked = selectedSet.size === 0 || selectedSet.has('All');
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
        const allChecked = selectedSet.size === 0 || selectedSet.has('All');
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
        const manifestSelected = session.manifest_failure_urls || [];
        const segmentSelected = session.segment_failure_urls || [];
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
        const chartMaxMode = ['auto', '5', '10', '20', '40'].includes(chartMaxRaw) ? chartMaxRaw : 'auto';
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

        // Calculate summary counts for badge
        const masterCount = session.master_manifest_requests_count || 0;
        const manifestCount = session.manifest_requests_count || 0;
        const segmentCount = session.segments_count || 0;

        const sessionDetailsOpen = resolveSectionDefault(options, 'session-details', false);
        const faultInjectionOpen = resolveSectionDefault(options, 'fault-injection', true);
        const networkShapingOpen = resolveSectionDefault(options, 'network-shaping', true);
        const bitrateChartOpen = resolveSectionDefault(options, 'bitrate-chart', false);

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
                            <div class="session-item"><span class="label">User Agent</span><span class="value" data-field="session_user_agent">${session.user_agent || '—'}</span></div>
                            <div class="session-item"><span class="label">Player IP</span><span class="value" data-field="session_player_ip">${session.player_ip || '—'}</span></div>
                            ${showPortItem ? `<div class="session-item"><span class="label">Port</span><span class="value" data-field="session_port_display">${portDisplay}</span></div>` : ''}
                            <div class="session-item"><span class="label">Last Request</span><span class="value" data-field="session_last_request">${formatDate(session.last_request)}</span></div>
                            <div class="session-item"><span class="label">First Request</span><span class="value" data-field="session_first_request">${formatDate(session.first_request_time)}</span></div>
                            <div class="session-item"><span class="label">Session Duration</span><span class="value" data-field="session_duration">${formatDuration(session.session_duration)}</span></div>
                            <div class="session-item"><span class="label">Manifest URL</span><span class="value" data-field="session_manifest_url">${session.manifest_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Master Manifest URL</span><span class="value" data-field="session_master_manifest_url">${session.master_manifest_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Last Request URL</span><span class="value" data-field="session_last_request_url">${session.last_request_url || '—'}</span></div>
                            <div class="session-item"><span class="label">Measured Mbps</span><span class="value" data-field="session_measured_mbps">${session.measured_mbps || '—'}</span></div>
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
                                </div>
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
                                    <input type="range" min="0" max="30" step="0.1" data-field="shaping_throughput_mbps" value="${session.nftables_bandwidth_mbps || 0}" ${usePattern ? 'disabled' : ''}>
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
                                    <input type="radio" name="bitrate_chart_max_mbps_${sessionId}" value="40" data-field="bitrate_chart_max_mbps" ${chartMaxMode === '40' ? 'checked' : ''}>
                                    <span>40 Mbps</span>
                                </label>
                            </div>
                        </div>
                        <div class="chart-wrap">
                            <canvas class="bandwidth-chart" data-field="bandwidth_chart"></canvas>
                        </div>
                        ${showBufferDepthChart ? `
                        <div class="chart-wrap">
                            <canvas class="buffer-depth-chart" data-field="buffer_depth_chart"></canvas>
                        </div>
                        ` : ''}
                    </div>
                </div>

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
                                <label class="network-log-filter">
                                    <input type="checkbox" data-filter="show-faulted" checked>
                                    Show Faults
                                </label>
                                <label class="network-log-filter">
                                    <input type="checkbox" data-filter="show-successful" checked>
                                    Show Successful
                                </label>
                            </div>
                            <div class="network-log-waterfall-wrap">
                                <div class="network-log-waterfall" data-field="network_log_waterfall"></div>
                                <div class="network-log-waterfall-empty" data-field="network_log_waterfall_empty" style="display:none;">No requests to plot yet.</div>
                            </div>
                            <div class="network-log-table-wrap">
                                <table class="network-log-table">
                                    <thead>
                                        <tr>
                                            <th class="net-col-method">Method</th>
                                            <th class="net-col-path">Path</th>
                                            <th class="net-col-type">Type</th>
                                            <th class="net-col-status">Status</th>
                                            <th class="net-col-size">Size</th>
                                            <th class="net-col-timing">Timing</th>
                                        </tr>
                                    </thead>
                                    <tbody data-field="network_log_body">
                                        <tr class="network-log-empty">
                                            <td colspan="6">No network requests yet. Requests will appear here once playback starts.</td>
                                        </tr>
                                    </tbody>
                                </table>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="session-actions">
                    <button class="btn btn-secondary" data-action="save-session">Save Settings</button>
                    <button class="btn btn-danger" data-action="delete-session">Delete Session</button>
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
            transport_fault_off_seconds: getRangeValue('transport_failure_frequency')
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
        });
    }

    // Initialize collapsible sections and tabs
    function initializeUI() {
        document.addEventListener('click', (e) => {
            // Handle collapsible toggles
            const toggle = e.target.closest('[data-toggle]');
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
                            window.TestingSessionUI.updateNetworkLog(sessionId);
                        }
                    }
                }
            }

            // Handle tab switches
            const tabButton = e.target.closest('.tab-button');
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
        window.addEventListener('resize', () => {
            networkWaterfallTimelines.forEach((state) => {
                if (state && state.timeline && typeof state.timeline.redraw === 'function') {
                    state.timeline.redraw();
                }
            });
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

    function getStatusClass(status) {
        if (status >= 200 && status < 300) return 'net-status-2xx';
        if (status >= 300 && status < 400) return 'net-status-3xx';
        if (status >= 400 && status < 500) return 'net-status-4xx';
        if (status >= 500) return 'net-status-5xx';
        return '';
    }

    function renderNetworkTimingBar(entry) {
        if (entry.faulted && !entry.dns_ms && !entry.connect_ms && !entry.ttfb_ms) {
            return `<span class="net-fault-injected">Injected by proxy</span>`;
        }

        const total = entry.total_ms || 0;
        if (total === 0) return '<span class="net-timing-total">—</span>';

        const dns = entry.dns_ms || 0;
        const connect = entry.connect_ms || 0;
        const tls = entry.tls_ms || 0;
        const ttfb = entry.ttfb_ms || 0;
        const transfer = entry.transfer_ms || 0;

        const dnsWidth = total > 0 ? (dns / total * 100) : 0;
        const connectWidth = total > 0 ? (connect / total * 100) : 0;
        const tlsWidth = total > 0 ? (tls / total * 100) : 0;
        const ttfbWidth = total > 0 ? (ttfb / total * 100) : 0;
        const transferWidth = total > 0 ? (transfer / total * 100) : 0;

        let html = '<div class="net-timing-bar">';
        if (dnsWidth > 0) {
            html += `<div class="net-timing-segment net-timing-dns" style="width: ${dnsWidth}%;" title="DNS: ${formatMilliseconds(dns)}"></div>`;
        }
        if (connectWidth > 0) {
            html += `<div class="net-timing-segment net-timing-connect" style="width: ${connectWidth}%;" title="Connect: ${formatMilliseconds(connect)}"></div>`;
        }
        if (tlsWidth > 0) {
            html += `<div class="net-timing-segment net-timing-tls" style="width: ${tlsWidth}%;" title="TLS: ${formatMilliseconds(tls)}"></div>`;
        }
        if (ttfbWidth > 0) {
            html += `<div class="net-timing-segment net-timing-ttfb" style="width: ${ttfbWidth}%;" title="TTFB: ${formatMilliseconds(ttfb)}"></div>`;
        }
        if (transferWidth > 0) {
            html += `<div class="net-timing-segment net-timing-transfer" style="width: ${transferWidth}%;" title="Transfer: ${formatMilliseconds(transfer)}"></div>`;
        }
        html += '</div>';
        html += `<span class="net-timing-total">${formatMilliseconds(total)}</span>`;
        return html;
    }

    function renderNetworkLogRow(entry) {
        const pathParts = (entry.path || entry.url || '').split('/');
        const filename = pathParts[pathParts.length - 1] || entry.path || entry.url || '—';
        const statusClass = getStatusClass(entry.status);
        const faultBadge = entry.faulted ? `<span class="net-fault-badge">${entry.fault_category || 'fault'}</span>` : '';
        const rowClass = entry.faulted ? 'faulted' : '';

        return `
            <tr class="${rowClass}" data-faulted="${entry.faulted || false}">
                <td class="net-col-method">${entry.method || 'GET'}</td>
                <td class="net-col-path">
                    <div class="net-path" title="${entry.url || ''}">${filename}</div>
                </td>
                <td class="net-col-type">
                    <span class="net-type">${entry.request_kind || '—'}</span>
                </td>
                <td class="net-col-status">
                    <span class="net-status ${statusClass}">${entry.status || '—'}</span>
                    ${faultBadge}
                </td>
                <td class="net-col-size net-size">${formatBytes(entry.bytes_out || 0)}</td>
                <td class="net-col-timing">${renderNetworkTimingBar(entry)}</td>
            </tr>
        `;
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
        const showFaulted = card.querySelector('[data-filter="show-faulted"]')?.checked ?? true;
        const showSuccessful = card.querySelector('[data-filter="show-successful"]')?.checked ?? true;
        return entries.filter((entry) => {
            const faulted = !!entry.faulted;
            return (faulted && showFaulted) || (!faulted && showSuccessful);
        });
    }

    function buildWaterfallRows(entries) {
        const rows = entries.slice().slice(-120).map((entry, index) => {
            const timestamp = Date.parse(entry.timestamp || '') || 0;
            const dns = Number(entry.dns_ms || 0);
            const connect = Number(entry.connect_ms || 0);
            const tls = Number(entry.tls_ms || 0);
            const ttfb = Number(entry.ttfb_ms || 0);
            const total = Number(entry.total_ms || 0);
            const transferRaw = Number(entry.transfer_ms || 0);
            const handshake = dns + connect + tls;
            const wait = Math.max(0, ttfb - handshake);
            const transfer = transferRaw > 0 ? transferRaw : Math.max(0, total - ttfb);
            const duration = Math.max(0, dns + connect + tls + wait + transfer);
            const pathParts = (entry.path || entry.url || '').split('/');
            const filename = pathParts[pathParts.length - 1] || entry.path || 'request';
            const prefix = entry.faulted ? '!' : '';
            return {
                index,
                entry,
                timestamp,
                filename,
                label: `${prefix}${entry.method || 'GET'} ${filename}`,
                dns,
                connect,
                tls,
                wait,
                transfer,
                duration
            };
        });
        return rows.filter((row) => row.timestamp > 0).sort((a, b) => a.timestamp - b.timestamp);
    }

    function ensureWaterfallTimeline(card, sessionId) {
        const field = card.querySelector('[data-field="network_log_waterfall"]');
        if (!field || !hasVisTimeline()) return null;
        const key = String(sessionId);
        let state = networkWaterfallTimelines.get(key) || null;

        if (state && state.host !== field) {
            if (state.timeline && typeof state.timeline.destroy === 'function') {
                state.timeline.destroy();
            }
            state = null;
        }

        if (!state) {
            const groups = new window.vis.DataSet();
            const items = new window.vis.DataSet();
            const options = {
                stack: false,
                zoomable: true,
                moveable: true,
                horizontalScroll: true,
                verticalScroll: true,
                showCurrentTime: false,
                selectable: false,
                showMajorLabels: false,
                showMinorLabels: true,
                orientation: { axis: 'top', item: 'bottom' },
                groupHeightMode: 'fixed',
                margin: {
                    axis: 0,
                    item: {
                        horizontal: 0,
                        vertical: 0
                    }
                },
                minHeight: '240px',
                maxHeight: '360px',
                tooltip: {
                    followMouse: true,
                    overflowMethod: 'flip'
                },
                format: {
                    minorLabels: {
                        millisecond: 'SSS[ms]',
                        second: 's.SSS[s]'
                    },
                    majorLabels: {
                        millisecond: '',
                        second: 's'
                    }
                }
            };
            const timeline = new window.vis.Timeline(field, items, groups, options);
            timeline.on('rangechanged', (props) => {
                if (!props || !props.start || !props.end || props.byUser !== true) return;
                const startMs = new Date(props.start).getTime();
                const endMs = new Date(props.end).getTime();
                if (Number.isFinite(startMs) && Number.isFinite(endMs) && endMs > startMs) {
                    networkWaterfallViewBySession.set(key, { startMs, endMs });
                }
            });
            state = { host: field, timeline, groups, items };
            networkWaterfallTimelines.set(key, state);
        }
        return state;
    }

    function updateNetworkWaterfall(card, sessionId) {
        const key = String(sessionId);
        const chartHost = card.querySelector('[data-field="network_log_waterfall"]');
        const emptyHost = card.querySelector('[data-field="network_log_waterfall_empty"]');
        if (!chartHost || !emptyHost) return;

        if (!hasVisTimeline()) {
            emptyHost.textContent = 'vis-timeline not loaded; waterfall unavailable.';
            emptyHost.style.display = 'block';
            return;
        }

        const rows = buildWaterfallRows(getFilteredNetworkEntries(card, key));
        if (!rows.length) {
            emptyHost.textContent = 'No requests to plot yet.';
            emptyHost.style.display = 'block';
            const state = networkWaterfallTimelines.get(key);
            if (state && state.items && state.groups) {
                state.items.clear();
                state.groups.clear();
                if (state.timeline && typeof state.timeline.redraw === 'function') {
                    state.timeline.redraw();
                }
            }
            return;
        }
        emptyHost.style.display = 'none';

        const state = ensureWaterfallTimeline(card, key);
        if (!state) return;

        const minStart = Math.min(...rows.map((row) => row.timestamp));
        const toTime = (offsetMs) => new Date(Math.max(0, Math.round(offsetMs)));
        const groups = [];
        const items = [];

        rows.forEach((row, idx) => {
            const groupId = idx + 1;
            const method = row.entry.method || 'GET';
            const status = row.entry.status || '—';
            const kind = row.entry.request_kind || 'request';
            const labelClass = row.entry.faulted ? 'waterfall-label is-faulted' : 'waterfall-label';
            const relativeStart = row.timestamp - minStart;
            const bytesOut = Number(row.entry.bytes_out || 0);
            const sizeLabel = bytesOut > 0 ? formatBytes(bytesOut) : '—';

            groups.push({
                id: groupId,
                content: `<div class="${labelClass}"><span class="waterfall-method">${method}</span><span class="waterfall-path" title="${row.entry.url || row.entry.path || ''}">${row.filename}</span><span class="waterfall-meta">${status} · ${kind} · ${sizeLabel}</span></div>`
            });

            const phases = [
                { key: 'dns', value: row.dns, label: 'DNS' },
                { key: 'connect', value: row.connect, label: 'Connect' },
                { key: 'tls', value: row.tls, label: 'TLS' },
                { key: 'wait', value: row.wait, label: 'Wait' },
                { key: 'transfer', value: row.transfer, label: 'Receive' }
            ];

            let cursor = relativeStart;
            phases.forEach((phase) => {
                if (phase.value <= 0) return;
                const phaseStart = cursor;
                const phaseEnd = cursor + phase.value;
                items.push({
                    id: `${groupId}-${phase.key}`,
                    group: groupId,
                    start: toTime(phaseStart),
                    end: toTime(phaseEnd),
                    className: `waterfall-phase waterfall-${phase.key}`,
                    title: [
                        `${method} ${row.filename}`,
                        `Status: ${status}`,
                        `${phase.label}: ${formatMilliseconds(phase.value)}`,
                        `Total: ${formatMilliseconds(row.duration)}`,
                        row.entry.url || row.entry.path || ''
                    ].join('\n')
                });
                cursor = phaseEnd;
            });
        });

        state.groups.clear();
        state.items.clear();
        state.groups.add(groups);
        state.items.add(items);

        const maxEnd = Math.max(...rows.map((row) => (row.timestamp - minStart) + row.duration));
        const storedView = networkWaterfallViewBySession.get(key);
        if (storedView && Number.isFinite(storedView.startMs) && Number.isFinite(storedView.endMs)) {
            const clampedStart = Math.max(0, Math.min(maxEnd, storedView.startMs));
            const clampedEnd = Math.max(clampedStart + 20, Math.min(maxEnd + 200, storedView.endMs));
            state.timeline.setWindow(toTime(clampedStart), toTime(clampedEnd), { animation: false });
        } else {
            state.timeline.setWindow(toTime(0), toTime(maxEnd + 200), { animation: false });
        }
        state.timeline.redraw();
    }

    function updateNetworkLog(sessionId) {
        const card = document.querySelector(`.session-card[data-session-id="${sessionId}"]`);
        if (!card) return;

        const tbody = card.querySelector('[data-field="network_log_body"]');
        const countBadge = card.querySelector('[data-field="network_log_count"]');
        if (!tbody) return;

        fetch(`/api/session/${sessionId}/network`)
            .then(response => response.json())
            .then(data => {
                const entries = data.entries || [];
                const count = entries.length;
                networkLogEntriesBySession.set(String(sessionId), entries);

                // Update count badge
                if (countBadge) {
                    countBadge.textContent = `${count} request${count !== 1 ? 's' : ''}`;
                }

                if (count === 0) {
                    tbody.innerHTML = `
                        <tr class="network-log-empty">
                            <td colspan="6">No network requests yet. Requests will appear here once playback starts.</td>
                        </tr>
                    `;
                    updateNetworkWaterfall(card, sessionId);
                    return;
                }

                // Render rows (most recent first)
                const rows = entries.slice().reverse().map(entry => renderNetworkLogRow(entry)).join('');
                tbody.innerHTML = rows;

                // Apply filters
                applyNetworkLogFilters(card);
            })
            .catch(error => {
                console.error('Failed to fetch network log:', error);
                networkLogEntriesBySession.delete(String(sessionId));
                if (tbody) {
                    tbody.innerHTML = `
                        <tr class="network-log-empty">
                            <td colspan="6" style="color: #dc2626;">Failed to load network log</td>
                        </tr>
                    `;
                }
                updateNetworkWaterfall(card, sessionId);
            });
    }

    function applyNetworkLogFilters(card) {
        const sessionId = String(card?.dataset?.sessionId || '');
        const showFaulted = card.querySelector('[data-filter="show-faulted"]')?.checked ?? true;
        const showSuccessful = card.querySelector('[data-filter="show-successful"]')?.checked ?? true;
        const tbody = card.querySelector('[data-field="network_log_body"]');
        if (!tbody) return;

        const rows = tbody.querySelectorAll('tr:not(.network-log-empty)');
        let visibleCount = 0;

        rows.forEach(row => {
            const isFaulted = row.dataset.faulted === 'true';
            const shouldShow = (isFaulted && showFaulted) || (!isFaulted && showSuccessful);
            row.style.display = shouldShow ? '' : 'none';
            if (shouldShow) visibleCount++;
        });

        // Show empty message if no rows match filters
        if (visibleCount === 0 && rows.length > 0) {
            const emptyRow = tbody.querySelector('.network-log-empty');
            if (!emptyRow) {
                tbody.insertAdjacentHTML('beforeend', `
                    <tr class="network-log-empty">
                        <td colspan="6">No requests match the current filters</td>
                    </tr>
                `);
            }
        } else {
            const emptyRow = tbody.querySelector('.network-log-empty');
            if (emptyRow) emptyRow.remove();
        }
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
