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
            return `<label><input type="radio" name="${name}" value="${option.value}" ${checked}>${option.text}</label>`;
        }).join('');
    }

    function renderModeOptions(name, selected) {
        return modeOptions.map(option => {
            const checked = option.value === (selected || 'requests') ? 'checked' : '';
            return `<label><input type="radio" name="${name}" value="${option.value}" ${checked}>${option.text}</label>`;
        }).join('');
    }

    function renderTransportFaultOptions(name, selected) {
        return transportFaultTypes.map(option => {
            const checked = option.value === (selected || 'none') ? 'checked' : '';
            return `<label><input type="radio" name="${name}" value="${option.value}" ${checked}>${option.text}</label>`;
        }).join('');
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

    function renderPatternStepPresetOptions(rate, presets) {
        const numericRate = Number(rate);
        const hasRate = Number.isFinite(numericRate);
        let selectedValue = 'custom';
        const options = presets || [];
        options.forEach((preset) => {
            if (!hasRate) return;
            if (Math.abs(Number(preset.mbps) - numericRate) < 0.001) {
                selectedValue = Number(preset.mbps).toFixed(3);
            }
        });
        const customSelected = selectedValue === 'custom' ? ' selected' : '';
        const optionHtml = options.map((preset) => {
            const value = Number(preset.mbps).toFixed(3);
            const selected = selectedValue === value ? ' selected' : '';
            const riskPrefix = preset.risk ? '⚠ ' : '';
            return `<option value="${value}"${selected}>${riskPrefix}${preset.label}</option>`;
        }).join('');
        return `<option value="custom"${customSelected}>Custom</option>${optionHtml}`;
    }

    function renderPatternStepRowContent(step, presets) {
        const rate = Number.isFinite(Number(step.rate_mbps)) ? Number(step.rate_mbps) : 0;
        const seconds = Number.isFinite(Number(step.duration_seconds)) ? Number(step.duration_seconds) : 1;
        const enabled = step.enabled !== false;
        return `
            <label>Preset</label>
            <select data-field="shaping_step_mbps_preset">
                ${renderPatternStepPresetOptions(rate, presets)}
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

    function renderPatternStepRow(index, step, presets) {
        return `
            <div class="shape-step-row" data-step-index="${index}">
                ${renderPatternStepRowContent(step, presets)}
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
        return `
            <div class="session-card" data-session-id="${sessionId}" data-session-port="${session.x_forwarded_port_external || session.x_forwarded_port || ''}" data-shaping-presets="${encodedPresets}" data-shaping-video-presets="${encodedVideoPresets}" data-shaping-overhead-mbps="${overheadMbps}">
                <div class="session-header">
                    ${hideTitle ? '' : `<div class="session-title">Session ${sessionId}</div>`}
                    <div class="session-meta" title="Port">${portDisplay}</div>
                </div>
                ${inlineHost ? `<div class="session-inline-player" data-inline-host="${sessionId}"></div>` : ''}
                <div class="session-grid">
                    <div class="session-item"><span class="label">User Agent</span><span class="value">${session.user_agent || '—'}</span></div>
                    <div class="session-item"><span class="label">Player IP</span><span class="value">${session.player_ip || '—'}</span></div>
                    ${showPortItem ? `<div class="session-item"><span class="label">Port</span><span class="value">${portDisplay}</span></div>` : ''}
                    <div class="session-item"><span class="label">Last Request</span><span class="value">${formatDate(session.last_request)}</span></div>
                    <div class="session-item"><span class="label">First Request</span><span class="value">${formatDate(session.first_request_time)}</span></div>
                    <div class="session-item"><span class="label">Session Duration</span><span class="value">${formatDuration(session.session_duration)}</span></div>
                    <div class="session-item"><span class="label">Manifest URL</span><span class="value">${session.manifest_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Master Manifest URL</span><span class="value">${session.master_manifest_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Last Request URL</span><span class="value">${session.last_request_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Counts</span><span class="value">Master:${session.master_manifest_requests_count || 0} Manifest:${session.manifest_requests_count || 0} Segment:${session.segments_count || 0}</span></div>
                    <div class="session-item"><span class="label">Measured Mbps</span><span class="value">${session.measured_mbps || '—'}</span></div>
                </div>
                <div class="failure-groups">
                    <div class="failure-group">
                        <div class="failure-title">Segment Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`segment_failure_type_${sessionId}`, session.segment_failure_type, segmentFailureTypes)}</div>
                        <div class="checkbox-group">${renderSegmentOptions(sessionId, manifestVariants, segmentSelected)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`segment_failure_mode_${sessionId}`, session.segment_failure_mode || 'failures_per_seconds')}
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
                    <div class="failure-group">
                        <div class="failure-title">Manifest Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`manifest_failure_type_${sessionId}`, session.manifest_failure_type)}</div>
                        <div class="checkbox-group">${renderManifestOptions(sessionId, manifestVariants, manifestSelected)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`manifest_failure_mode_${sessionId}`, session.manifest_failure_mode || 'failures_per_seconds')}
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
                    <div class="failure-group">
                        <div class="failure-title">Master Manifest Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`master_manifest_failure_type_${sessionId}`, session.master_manifest_failure_type)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`master_manifest_failure_mode_${sessionId}`, session.master_manifest_failure_mode || 'failures_per_seconds')}
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
                    <div class="failure-group">
                        <div class="failure-title">Transport Faults (Port-Wide)</div>
                        <div class="radio-group">${renderTransportFaultOptions(`transport_failure_type_${sessionId}`, transportFaultType)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderTransportModeOptions(`transport_failure_mode_${sessionId}`, transportMode)}
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
                <div class="failure-group" data-net-shaping>
                    <div class="failure-title">Network Shaping</div>
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
                    <div class="shape-pattern-block">
                        <div class="shape-template-row">
                            <label>Pattern</label>
                            <div class="shape-pattern-modes" data-field="shaping_template_mode_group">
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
                        <div class="shape-step-defaults">
                            <label>Step Duration</label>
                            <div class="shape-pattern-modes" data-field="shaping_default_step_seconds_group">
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
                            <span class="shape-default-seconds" data-field="shaping_default_seconds_label">segment ${segmentDurationSeconds}s</span>
                        </div>
                        <div class="shape-template-row">
                            <label>Margin</label>
                            <div class="shape-pattern-modes shape-margin-modes" data-field="shaping_template_margin_group">
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
                        <div class="shape-step-list" data-field="shaping_pattern_rows" style="display:${usePattern ? '' : 'none'};">
                            ${initialSteps.map((step, idx) => renderPatternStepRow(idx, step, shapingPresets)).join('')}
                        </div>
                        <div class="shape-step-actions" style="display:${usePattern ? '' : 'none'};">
                            <button type="button" class="btn btn-secondary btn-mini" data-action="add-shaping-step">Add Step</button>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="clear-shaping-pattern">Clear</button>
                        </div>
                        <div class="shape-apply-pattern" data-field="shaping_apply_pattern_row" style="display:none;">
                            <button type="button" class="btn btn-primary" data-action="apply-pattern">Apply Pattern</button>
                        </div>
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
                    </div>
                    <div class="chart-wrap">
                        <canvas class="bandwidth-chart" width="820" height="220" data-field="bandwidth_chart"></canvas>
                    </div>
                    ${showBufferDepthChart ? `
                    <div class="chart-wrap">
                        <canvas class="buffer-depth-chart" width="820" height="170" data-field="buffer_depth_chart"></canvas>
                    </div>
                    ` : ''}
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
        const getRadioValue = (name) => {
            const selected = card.querySelector(`input[name="${name}"]:checked`);
            return selected ? selected.value : 'none';
        };

        const segmentFailureType = getRadioValue(`segment_failure_type_${sessionId}`);
        const manifestFailureType = getRadioValue(`manifest_failure_type_${sessionId}`);
        const masterManifestFailureType = getRadioValue(`master_manifest_failure_type_${sessionId}`);
        const transportFaultType = getRadioValue(`transport_failure_type_${sessionId}`);

        const segmentFailureUnits = getRadioValue(`segment_failure_units_${sessionId}`) || 'requests';
        const manifestFailureUnits = getRadioValue(`manifest_failure_units_${sessionId}`) || 'requests';
        const masterManifestFailureUnits = getRadioValue(`master_manifest_failure_units_${sessionId}`) || 'requests';
        const segmentMode = getRadioValue(`segment_failure_mode_${sessionId}`) || modeFromUnits(null, null, segmentFailureUnits);
        const manifestMode = getRadioValue(`manifest_failure_mode_${sessionId}`) || modeFromUnits(null, null, manifestFailureUnits);
        const masterManifestMode = getRadioValue(`master_manifest_failure_mode_${sessionId}`) || modeFromUnits(null, null, masterManifestFailureUnits);
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
            segment_failure_units: segmentFailureUnits,
            segment_consecutive_units: segmentUnits.consecutiveUnits,
            segment_frequency_units: segmentUnits.frequencyUnits,
            segment_failure_mode: segmentMode,
            segment_failure_urls: segmentChecks,
            manifest_failure_at: null,
            manifest_failure_recover_at: null,
            manifest_failure_type: manifestFailureType,
            manifest_failure_frequency: getRangeValue('manifest_failure_frequency'),
            manifest_consecutive_failures: getRangeValue('manifest_consecutive_failures'),
            manifest_failure_units: manifestFailureUnits,
            manifest_consecutive_units: manifestUnits.consecutiveUnits,
            manifest_frequency_units: manifestUnits.frequencyUnits,
            manifest_failure_mode: manifestMode,
            manifest_failure_urls: manifestChecks,
            master_manifest_failure_at: null,
            master_manifest_failure_recover_at: null,
            master_manifest_failure_type: masterManifestFailureType,
            master_manifest_failure_frequency: getRangeValue('master_manifest_failure_frequency'),
            master_manifest_consecutive_failures: getRangeValue('master_manifest_consecutive_failures'),
            master_manifest_failure_units: masterManifestFailureUnits,
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
            // Legacy aliases kept for older saved sessions/backends.
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
        const segmentLabel = card.querySelector('[data-field="shaping_default_seconds_label"]');
        const segmentMatch = segmentLabel ? segmentLabel.textContent.match(/segment\s+([0-9.]+)s/i) : null;
        const inferredSegmentSeconds = segmentMatch ? Number(segmentMatch[1]) : 6;
        const segmentDurationSeconds = toPositiveNumber(inferredSegmentSeconds, 6);
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
        const pattern = readShapingPattern(card);
        const label = card.querySelector('[data-field="shaping_default_seconds_label"]');
        if (!label) return;
        label.textContent = `segment ${pattern.segment_duration_seconds}s`;
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

    window.TestingSessionUI = {
        renderSessionCard,
        renderPatternStepRowContent,
        readSessionSettings,
        readShapingPattern,
        updatePatternDefaultLabel,
        updateTransportModeUi,
        formatDate,
        formatDuration
    };
})();
