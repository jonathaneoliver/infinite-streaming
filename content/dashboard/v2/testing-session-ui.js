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
    // Per-session event-marker state — { tsMs, label } picked from the
    // session-replay events dropdown / rail. Painted as a cyan vertical
    // guide on both the overview rail and the row-list time scale so
    // the user can see exactly where the picked event lands among the
    // network requests.
    const networkLogEventMarkerBySession = new Map();
    // Session ids whose next paint should auto-scroll the row list to
    // the row closest to the marker's timestamp. We only scroll on a
    // fresh pick (not on every brush drag / re-render) so the user
    // can scroll away to read nearby rows without being yanked back.
    const networkLogPendingScrollBySession = new Set();
    const NETLOG_EVENT_MARKER_COLOR = '#0891b2';
    // Per-session column sort state: { col, dir } where dir is 'asc' or
    // 'desc'. col=null (or absent) means default chronological order.
    const networkWaterfallSortBySession = new Map();
    const networkWaterfallRollingWindowMs = 10 * 60 * 1000;
    // Default and minimum brush span. Tighter than this and the time
    // axis gets too granular to be useful; this is also the default
    // initial span when a session first opens.
    const networkWaterfallMinBrushMs = 30 * 1000;
    // In replay mode the network log brush should match the main page
    // brush span (10 min) by default rather than the live-mode 30s tail.
    // Using the same span makes the two scrub bars feel coordinated even
    // before the user touches either one. Live testing.html keeps the
    // small default — there entries arrive in real time and a tight
    // tail is more useful.
    const networkWaterfallReplayDefaultMs = 10 * 60 * 1000;
    const networkLogAutoRefreshTimers = new Map();
    const networkLogFetchInFlight = new Set();
    const networkLogAutoRefreshMs = 1500;
    // True when the user explicitly paused fetching for this session.
    // While paused, the auto-refresh timer is stopped — entries don't
    // grow, the brush stays where the user left it, and a "PAUSED"
    // overlay sits on top of the waterfall. Click Live to resume.
    const networkLogPausedBySession = new Map();
    // Set of session_ids whose own network-log brush is being dragged.
    // Used to gate the 1.5s auto-refresh poll so the row table doesn't
    // rebuild mid-gesture. Distinct from SessionShell's
    // brushDraggingBySession (the main session-viewer brush).
    const networkLogBrushDragging = new Set();
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

    function renderSegmentOptions(sessionId, playlists, selected, fieldName) {
        const field = fieldName || 'segment_failure_urls';
        const selectedSet = new Set(selected || []);
        const list = sortedPlaylists(playlists);
        const allChecked = selected == null ? true : selectedSet.has('All');
        const checkbox = (value, label) => {
            const checked = allChecked || selectedSet.has(value) ? 'checked' : '';
            return `<label><input type="checkbox" data-field="${field}" value="${value}" ${checked}>${label}</label>`;
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
        const allSelected = session.all_failure_urls;
        const allOverrideActive = session.all_failure_type && session.all_failure_type !== 'none';
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
        const playerStateOpen = resolveSectionDefault(options, 'player-state', false);
        const playerMetricsOpen = resolveSectionDefault(options, 'player-metrics', false);

        const hideHeader = options.hideHeader || false;
        return `
            <div class="session-card" data-session-id="${sessionId}" data-session-port="${session.x_forwarded_port_external || session.x_forwarded_port || ''}" data-segment-duration-seconds="${segmentDurationSeconds}" data-shaping-presets="${encodedPresets}" data-shaping-video-presets="${encodedVideoPresets}" data-shaping-overhead-mbps="${overheadMbps}">
                ${hideHeader ? '' : `<div class="session-header">
                    ${hideTitle ? '' : `<div class="session-title">Session ${sessionId}</div>`}
                    <div class="session-meta" title="Port">${portDisplay}</div>
                </div>`}
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
                            <div class="session-item"><span class="label">Play ID</span><span class="value" data-field="session_play_id" title="Fresh UUID minted by the player at every loadStream() boundary (issue #280). Each fresh playback episode gets its own play_id; the analytics pipeline partitions on (session_id, play_id).">${session.play_id || '—'}</span></div>
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
                                    <button class="tab-button active" data-tab="all-failures">All</button>
                                    <button class="tab-button" data-tab="segment-failures">Segment</button>
                                    <button class="tab-button" data-tab="manifest-failures">Manifest</button>
                                    <button class="tab-button" data-tab="master-failures">Master</button>
                                    <button class="tab-button" data-tab="transport-faults">Transport</button>
                                    <button class="tab-button" data-tab="content-manipulation">Content</button>
                                </div>
                                <div class="tabs-content">
                                    <!-- All Tab (override) -->
                                    <div class="tab-panel active" data-panel="all-failures">
                                        <div class="content-tab-note" style="margin-bottom:8px">
                                            <strong>Override:</strong> when active, this rule applies to <em>every</em> HTTP request (segments, media manifests, master). The Segment / Manifest / Master tabs are bypassed while All is active.
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Failure Type</label>
                                            <div class="radio-group">
                                                ${renderFailureTypeOptions(`all_failure_type_${sessionId}`, session.all_failure_type)}
                                            </div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Scope</label>
                                            <div class="checkbox-group">${renderSegmentOptions(sessionId, manifestVariants, allSelected, 'all_failure_urls')}</div>
                                        </div>
                                        <div class="fault-control-row">
                                            <label>Mode</label>
                                            ${renderModeDropdown(`all_failure_mode_${sessionId}`, session.all_failure_mode || 'failures_per_seconds')}
                                        </div>
                                        <div class="range-row">
                                            <label>Consecutive</label>
                                            <input type="range" min="0" max="10" step="1" data-field="all_consecutive_failures" value="${Number.isFinite(Number(session.all_consecutive_failures)) && Number(session.all_consecutive_failures) >= 0 ? Number(session.all_consecutive_failures) : 1}">
                                            <span class="range-value">${Number.isFinite(Number(session.all_consecutive_failures)) && Number(session.all_consecutive_failures) >= 0 ? Number(session.all_consecutive_failures) : 1}</span>
                                        </div>
                                        <div class="range-row">
                                            <label>Frequency</label>
                                            <input type="range" min="0" max="30" step="1" data-field="all_failure_frequency" value="${Number.isFinite(Number(session.all_failure_frequency)) && Number(session.all_failure_frequency) >= 0 ? Number(session.all_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.all_failure_frequency)) && Number(session.all_failure_frequency) >= 0 ? Number(session.all_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Segment Tab -->
                                    <div class="tab-panel" data-panel="segment-failures">
                                        ${allOverrideActive ? '<div class="content-tab-note" style="margin-bottom:8px;background:#fffbe6;border-color:#f0c97b"><strong>All override active</strong> — this tab is ignored. Set <em>All → Failure Type</em> to <code>none</code> to re-enable per-kind tabs.</div>' : ''}
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
                                            <input type="range" min="0" max="30" step="1" data-field="segment_failure_frequency" value="${Number.isFinite(Number(session.segment_failure_frequency)) && Number(session.segment_failure_frequency) >= 0 ? Number(session.segment_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.segment_failure_frequency)) && Number(session.segment_failure_frequency) >= 0 ? Number(session.segment_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Manifest Tab -->
                                    <div class="tab-panel" data-panel="manifest-failures">
                                        ${allOverrideActive ? '<div class="content-tab-note" style="margin-bottom:8px;background:#fffbe6;border-color:#f0c97b"><strong>All override active</strong> — this tab is ignored. Set <em>All → Failure Type</em> to <code>none</code> to re-enable per-kind tabs.</div>' : ''}
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
                                            <input type="range" min="0" max="30" step="1" data-field="manifest_failure_frequency" value="${Number.isFinite(Number(session.manifest_failure_frequency)) && Number(session.manifest_failure_frequency) >= 0 ? Number(session.manifest_failure_frequency) : 6}">
                                            <span class="range-value">${Number.isFinite(Number(session.manifest_failure_frequency)) && Number(session.manifest_failure_frequency) >= 0 ? Number(session.manifest_failure_frequency) : 6}</span>
                                        </div>
                                    </div>

                                    <!-- Master Manifest Tab -->
                                    <div class="tab-panel" data-panel="master-failures">
                                        ${allOverrideActive ? '<div class="content-tab-note" style="margin-bottom:8px;background:#fffbe6;border-color:#f0c97b"><strong>All override active</strong> — this tab is ignored. Set <em>All → Failure Type</em> to <code>none</code> to re-enable per-kind tabs.</div>' : ''}
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
                                            <input type="range" min="0" max="30" step="1" data-field="master_manifest_failure_frequency" value="${Number.isFinite(Number(session.master_manifest_failure_frequency)) && Number(session.master_manifest_failure_frequency) >= 0 ? Number(session.master_manifest_failure_frequency) : 6}">
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

                <!-- Collapsible Player State -->
                <div class="collapsible-section" data-section="player-state" data-default-open="${playerStateOpen}">
                    <div class="collapsible-header" data-toggle="player-state">
                        <span class="collapsible-icon">${playerStateOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Player State</span>
                    </div>
                    <div class="collapsible-content" data-content="player-state" style="display: ${playerStateOpen ? 'block' : 'none'};">
                        <div class="chart-axis-row">
                            <button type="button" class="btn btn-secondary btn-mini" data-action="reset-bitrate-zoom">Reset Zoom</button>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="pause-bitrate-chart">⏸ Pause</button>
                            <span class="chart-hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">Alt/⌥+scroll/drag to zoom · right-drag to pan</span>
                        </div>
                        <div class="chart-wrap events-timeline-wrap">
                            <div class="events-timeline-legend" data-field="events_timeline_legend"></div>
                            <div class="events-timeline" data-field="events_timeline"></div>
                        </div>
                    </div>
                </div>

                <!-- Collapsible Bitrate Chart -->
                <div class="collapsible-section" data-section="bitrate-chart" data-default-open="${bitrateChartOpen}">
                    <div class="collapsible-header" data-toggle="bitrate-chart">
                        <span class="collapsible-icon">${bitrateChartOpen ? '▼' : '▶'}</span>
                        <span class="collapsible-title">Bitrate Chart etc</span>
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
                            <div class="chart-axis-row-break"></div>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="reset-bitrate-zoom">Reset Zoom</button>
                            <button type="button" class="btn btn-secondary btn-mini" data-action="pause-bitrate-chart">⏸ Pause</button>
                            <span class="chart-hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">Alt/⌥+scroll/drag to zoom · right-drag to pan</span>
                        </div>
                        <div class="chart-wrap">
                            <button type="button" class="chart-expand-btn" data-action="toggle-chart-expanded" title="Toggle expanded chart height" aria-label="Toggle expanded chart height"><span class="chart-expand-icon">⤢</span><span class="chart-expand-label">Expand</span></button>
                            <canvas class="bandwidth-chart" data-field="bandwidth_chart"></canvas>
                        </div>
                        <div class="chart-wrap">
                            <button type="button" class="chart-expand-btn" data-action="toggle-chart-expanded" title="Toggle expanded chart height" aria-label="Toggle expanded chart height"><span class="chart-expand-icon">⤢</span><span class="chart-expand-label">Expand</span></button>
                            <canvas class="rtt-chart" data-field="rtt_chart"></canvas>
                        </div>
                        ${showBufferDepthChart ? `
                        <div class="chart-wrap">
                            <button type="button" class="chart-expand-btn" data-action="toggle-chart-expanded" title="Toggle expanded chart height" aria-label="Toggle expanded chart height"><span class="chart-expand-icon">⤢</span><span class="chart-expand-label">Expand</span></button>
                            <canvas class="buffer-depth-chart" data-field="buffer_depth_chart"></canvas>
                        </div>
                        <div class="chart-wrap">
                            <button type="button" class="chart-expand-btn" data-action="toggle-chart-expanded" title="Toggle expanded chart height" aria-label="Toggle expanded chart height"><span class="chart-expand-icon">⤢</span><span class="chart-expand-label">Expand</span></button>
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
                                <button type="button" class="btn btn-mini btn-secondary" data-action="pause-network-log" title="Stop fetching new entries — freeze the current view until you click Live">⏸ Pause</button>
                                <label class="network-log-filter" title="When checked, the row list snaps to the latest entry on every refresh.">
                                    <input type="checkbox" data-field="follow-latest" checked>
                                    Follow Latest
                                </label>
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
        const allFailureType = getRadioValue(`all_failure_type_${sessionId}`);
        const transportFaultType = getRadioValue(`transport_failure_type_${sessionId}`);

        const segmentMode = getSelectValue(`segment_failure_mode_${sessionId}`) || 'requests';
        const manifestMode = getSelectValue(`manifest_failure_mode_${sessionId}`) || 'requests';
        const masterManifestMode = getSelectValue(`master_manifest_failure_mode_${sessionId}`) || 'requests';
        const allMode = getSelectValue(`all_failure_mode_${sessionId}`) || 'requests';
        const transportMode = normalizeTransportMode(getRadioValue(`transport_failure_mode_${sessionId}`));

        const segmentUnits = unitsFromMode(segmentMode);
        const manifestUnits = unitsFromMode(manifestMode);
        const masterManifestUnits = unitsFromMode(masterManifestMode);
        const allUnits = unitsFromMode(allMode);
        const transportUnits = transportUnitsFromMode(transportMode);

        const getRangeValue = (field) => {
            const input = card.querySelector(`input[data-field="${field}"]`);
            return input ? Number(input.value) : 0;
        };

        const manifestChecks = Array.from(card.querySelectorAll('input[data-field="manifest_failure_urls"]:checked'))
            .map(input => input.value);
        const segmentChecks = Array.from(card.querySelectorAll('input[data-field="segment_failure_urls"]:checked'))
            .map(input => input.value);
        const allChecks = Array.from(card.querySelectorAll('input[data-field="all_failure_urls"]:checked'))
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
            all_failure_at: null,
            all_failure_recover_at: null,
            all_failure_type: allFailureType,
            all_failure_frequency: getRangeValue('all_failure_frequency'),
            all_consecutive_failures: getRangeValue('all_consecutive_failures'),
            all_failure_units: allUnits.consecutiveUnits,
            all_consecutive_units: allUnits.consecutiveUnits,
            all_frequency_units: allUnits.frequencyUnits,
            all_failure_mode: allMode,
            all_failure_urls: allChecks,
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
        // Click on empty space inside the network-log waterfall toggles
        // pause/live, mirroring the chart canvas behaviour. Skip clicks
        // on rows, the brush, controls, and links — those have their
        // own behaviour. Drag-detection uses a 5-pixel threshold so a
        // brush-drag that ends inside the wrap doesn't accidentally
        // toggle pause.
        let netwfMouseDownX = 0, netwfMouseDownY = 0;
        document.addEventListener('mousedown', (e) => {
            if (e.button === 0 && e.target.closest('.network-log-waterfall-wrap')) {
                netwfMouseDownX = e.clientX;
                netwfMouseDownY = e.clientY;
            }
        });
        document.addEventListener('click', (e) => {
            const wrap = e.target.closest('.network-log-waterfall-wrap');
            if (!wrap) return;
            // Was this a real click, not the end of a drag?
            if (Math.abs(e.clientX - netwfMouseDownX) > 5 || Math.abs(e.clientY - netwfMouseDownY) > 5) return;
            // Skip clicks on interactive sub-elements that have their
            // own meaning (rows, the brush, controls, links).
            const skipSelectors = '.netwf-row, .netwf-brush, .netwf-brush-handle, .netwf-overview-bars, .netwf-overview-tick, button, input, label, select, textarea, a';
            if (e.target.closest(skipSelectors)) return;
            const card = wrap.closest('.session-card');
            const sessionId = card ? String(card.dataset.sessionId || '') : '';
            if (!sessionId) return;
            toggleNetworkLogPaused(sessionId);
        });

        // "All" checkbox toggles all sibling scope checkboxes
        document.addEventListener('change', (e) => {
            const cb = e.target;
            if (!cb || cb.type !== 'checkbox' || !cb.dataset.field) return;
            const field = cb.dataset.field;
            if (field === 'follow-latest') {
                const card = cb.closest('.session-card');
                const sessionId = card ? String(card.dataset.sessionId || '') : '';
                if (!sessionId) return;
                setNetworkLogFollowMode(sessionId, !!cb.checked);
                if (cb.checked) {
                    // Refetch + snap the INNER scroll to the bottom.
                    // Two rAFs so the new entries (still being added
                    // to the DOM after the fetch) are covered.
                    updateNetworkLog(sessionId);
                    const scrollHost = card.querySelector('[data-field="network_log_waterfall_scroll"]');
                    const snapToLast = () => {
                        if (scrollHost) scrollHost.scrollTop = scrollHost.scrollHeight;
                    };
                    window.requestAnimationFrame(() => {
                        snapToLast();
                        window.requestAnimationFrame(snapToLast);
                    });
                } else if (card) {
                    applyNetworkLogFilters(card);
                }
                return;
            }
            if (field !== 'segment_failure_urls' && field !== 'manifest_failure_urls' && field !== 'all_failure_urls') return;
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
                if (action === 'toggle-chart-expanded') {
                    e.preventDefault();
                    e.stopPropagation();
                    const wrap = actionButton.closest('.chart-wrap');
                    if (!wrap) return;
                    const card = wrap.closest('.session-card');
                    const sessionId = card ? String(card.dataset.sessionId || '') : '';
                    wrap.classList.toggle('chart-expanded');
                    if (sessionId) {
                        requestAnimationFrame(() => {
                            document.dispatchEvent(new CustomEvent('testing-session:charts-resize', {
                                detail: { sessionId }
                            }));
                        });
                    }
                    return;
                }
                if (action === 'pause-network-log') {
                    const card = actionButton.closest('.session-card');
                    const sessionId = card ? String(card.dataset.sessionId || '') : '';
                    if (!sessionId) return;
                    toggleNetworkLogPaused(sessionId);
                    return;
                }
                if (action === 'refresh-network-log') {
                    const card = actionButton.closest('.session-card');
                    const sessionId = card ? String(card.dataset.sessionId || '') : '';
                    if (!sessionId) return;
                    // One-shot refresh ignores the pause state — the user
                    // explicitly asked for fresh data right now.
                    updateNetworkLog(sessionId);
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
                    if ((section === 'bitrate-chart' || section === 'player-state') && nextOpen) {
                        const card = sectionEl ? sectionEl.closest('.session-card') : null;
                        const sessionId = card ? card.dataset.sessionId : null;
                        if (sessionId) {
                            // When player-state opens from closed, force
                            // the events-timeline to be rebuilt — vis.Timeline
                            // created in a display:none container has broken
                            // internal layout state that redraw() can't fix.
                            const rebuildEventsTimeline = (section === 'player-state');
                            const event = new CustomEvent('testing-session:charts-resize', {
                                detail: { sessionId, rebuildEventsTimeline }
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
        // Honour the user's Pause toggle — even when the fold opens or
        // the page becomes visible again, don't resume polling unless
        // the user has explicitly clicked Live.
        if (isNetworkLogPaused(key)) return;
        const hostCard = card || document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!hostCard) return;
        if (networkLogAutoRefreshTimers.has(key)) return;
        updateNetworkLog(key, { skipIfInFlight: true });
        const timer = setInterval(() => {
            updateNetworkLog(key, { skipIfInFlight: true });
        }, networkLogAutoRefreshMs);
        networkLogAutoRefreshTimers.set(key, timer);
    }

    function isNetworkLogPaused(sessionId) {
        return networkLogPausedBySession.get(String(sessionId || '')) === true;
    }

    // Sync the visible button label + PAUSED badge to the persisted
    // pause state. Called from setNetworkLogPaused (after toggle) and
    // from updateNetworkWaterfall (after every refresh) so SSE-driven
    // session-card re-renders don't reset the visual state.
    function applyNetworkLogPauseUI(sessionId, card) {
        const key = String(sessionId || '');
        if (!key) return;
        const hostCard = card || document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!hostCard) return;
        const paused = isNetworkLogPaused(key);
        for (const btn of hostCard.querySelectorAll('button[data-action="pause-network-log"]')) {
            if (paused) {
                btn.textContent = '▶ Live';
                btn.classList.add('netwf-live');
                btn.title = 'Resume fetching new entries';
            } else {
                btn.textContent = '⏸ Pause';
                btn.classList.remove('netwf-live');
                btn.title = 'Stop fetching new entries — freeze the current view until you click Live';
            }
        }
        const wrap = hostCard.querySelector('.network-log-waterfall-wrap');
        if (wrap) {
            let badge = wrap.querySelector('.network-log-paused-badge');
            if (!badge) {
                badge = document.createElement('div');
                badge.className = 'network-log-paused-badge';
                badge.textContent = 'PAUSED';
                badge.style.cssText = 'position:absolute;top:8px;right:8px;padding:2px 10px;border-radius:10px;background:rgba(220,38,38,0.85);color:#fff;font:600 11px system-ui;letter-spacing:0.06em;pointer-events:none;display:none;z-index:5;';
                if (getComputedStyle(wrap).position === 'static') wrap.style.position = 'relative';
                wrap.appendChild(badge);
            }
            badge.style.display = paused ? 'block' : 'none';
        }
    }

    function setNetworkLogPaused(sessionId, paused) {
        const key = String(sessionId || '');
        if (!key) return;
        networkLogPausedBySession.set(key, !!paused);
        const card = document.querySelector(`.session-card[data-session-id="${key}"]`);
        applyNetworkLogPauseUI(key, card);
        if (paused) {
            stopNetworkLogAutoRefresh(key);
        } else {
            // Resume: kick off a fresh poll immediately.
            startNetworkLogAutoRefresh(key, card);
        }
    }

    function toggleNetworkLogPaused(sessionId) {
        setNetworkLogPaused(sessionId, !isNetworkLogPaused(sessionId));
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
        const checkbox = hostCard.querySelector('input[data-field="follow-latest"]');
        if (!checkbox) return;
        const following = isNetworkLogFollowMode(sessionId);
        if (checkbox.checked !== following) {
            checkbox.checked = following;
        }
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
            // Two on-the-wire formats:
            //   live (go-proxy):       "2026-05-02T23:18:20.608Z"  (RFC3339, UTC)
            //   archived (ClickHouse): "2026-05-02 23:18:20.608"   (no TZ marker)
            // Date.parse defaults the second form to LOCAL time, which shifts
            // every archived entry by the user's UTC offset and makes the
            // network log times disagree with the chart/heatmap times.
            // Normalise the unspaced/no-TZ form to ISO+Z before parsing.
            const tsRaw = String(entry.timestamp || '');
            let timestamp;
            if (!tsRaw) {
                timestamp = 0;
            } else if (tsRaw.includes('T') && (tsRaw.endsWith('Z') || /[+-]\d\d:?\d\d$/.test(tsRaw))) {
                timestamp = Date.parse(tsRaw) || 0;
            } else {
                timestamp = Date.parse(tsRaw.replace(' ', 'T') + 'Z') || 0;
            }
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
        // Live testing.html uses a 10-min rolling cutoff so the table
        // doesn't grow forever as new requests stream in. In replay mode
        // we WANT the whole session — the user is investigating history,
        // not tailing live traffic — so don't trim. Otherwise a 75-min
        // session would silently lose 65 min of HAR rows before the rail
        // / brush even sees them.
        if (document.body.classList.contains('replay-mode')) {
            return ordered;
        }
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
        // Re-establish the pause-state UI on every render — SSE-driven
        // session-card re-renders on testing.html / testing-session.html
        // would otherwise drop the "▶ Live" label and PAUSED badge even
        // though the persisted pause state still suppresses polling.
        applyNetworkLogPauseUI(key, card);
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

        // The overview rail used to span just the HAR data extent. In
        // session-viewer the user can expand the main brush past the
        // available data — we extend the rail to the union of data
        // range and brush range so the rail always covers what the
        // user is looking at, with empty area for "no requests here."
        const dataStartMs = rows[0].timestamp;
        const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
        // Span of the actual HAR data — used for default brush sizing.
        // Distinct from `fullSpan` (= rail span = max(data, brush))
        // computed once the brush is settled, below.
        const dataSpan = Math.max(50, dataEndMs - dataStartMs);

        // Brush state — Following Latest = stick to the right edge.
        // Default: zoom to the most recent 2 minutes (or full session
        // if shorter). 2 min is also the floor we enforce on user
        // resize so bars stay scannable.
        const isReplay = document.body.classList.contains('replay-mode');
        const defaultSpanMs = isReplay ? networkWaterfallReplayDefaultMs : networkWaterfallMinBrushMs;
        let brush = networkWaterfallBrushBySession.get(key);
        if (!brush) {
            const initialSpan = Math.min(dataSpan, defaultSpanMs);
            brush = { startMs: dataEndMs - initialSpan, endMs: dataEndMs, follow: true };
            networkWaterfallBrushBySession.set(key, brush);
        }
        // External callers (e.g. the bitrate chart's pan/zoom or the
        // session-viewer scrubber) may have set a brush range that's
        // entirely outside the actual HAR data — for example, a session
        // viewer scrubbed to "last 30s" but the proxy buffer only holds
        // entries from earlier. If the requested range has no overlap
        // with the data we have, fall back to a sensible default
        // (follow-latest) so the user sees *something*. The next user
        // gesture re-anchors the brush.
        const noOverlap = (brush.endMs < dataStartMs) || (brush.startMs > dataEndMs);
        if (noOverlap) {
            const initialSpan = Math.min(dataSpan, networkWaterfallMinBrushMs);
            brush.startMs = Math.max(dataStartMs, dataEndMs - initialSpan);
            brush.endMs = dataEndMs;
            brush.follow = true;
        }
        // Sync brush.follow with the Follow Latest button. Clicking
        // the button re-engages right-edge stickiness even if the user
        // had previously dragged the brush.
        brush.follow = brush.follow || isNetworkLogFollowMode(key);
        if (brush.follow) {
            const span = brush.endMs - brush.startMs;
            brush.endMs = dataEndMs;
            brush.startMs = Math.max(dataStartMs, brush.endMs - span);
        }
        // Clamp to data range and enforce the minimum brush width.
        // Defensive: end can still be <= start if the requested range
        // was zero or otherwise pathological. Fall back to a sensible
        // default brush at the data tail.
        if (brush.endMs <= brush.startMs) {
            brush.endMs = dataEndMs;
            const initialSpan = Math.min(dataEndMs - dataStartMs, defaultSpanMs);
            brush.startMs = Math.max(dataStartMs, dataEndMs - initialSpan);
        }
        // Min-brush-width enforcement only kicks in when the brush would
        // otherwise be uselessly small. Don't clamp to data range here —
        // the rail extends to encompass the brush below.
        if (brush.endMs - brush.startMs < networkWaterfallMinBrushMs) {
            brush.startMs = brush.endMs - networkWaterfallMinBrushMs;
        }

        // Rail coordinate system = union of data range and brush range.
        // When the brush extends past the data (typical when expanding
        // the session-viewer focus window past available HAR archival),
        // the rail grows to include the brush — empty space rendered
        // for "no requests in this part" rather than clipping the brush.
        const railStartMs = Math.min(dataStartMs, brush.startMs);
        const railEndMs   = Math.max(dataEndMs,   brush.endMs);
        const fullSpan = Math.max(50, railEndMs - railStartMs);

        // Summary row: total + categorical counts. Read at a glance
        // before the user starts panning the brush.
        const summaryEl = card.querySelector('[data-field="netwf_summary"]');
        if (summaryEl) {
            renderWaterfallSummary(summaryEl, rows, dataStartMs, dataEndMs);
        }

        // Time labels above the overview pane — span the rail (which
        // covers data + brush) so axis ticks line up with the rail
        // beneath them.
        const overviewAxis = card.querySelector('[data-field="netwf_overview_axis"]');
        if (overviewAxis) {
            renderWaterfallAxisTicks(overviewAxis, railStartMs, railEndMs);
        }

        // Render overview ticks (one per row, positioned by absolute
        // time within the rail). Rail bounds in use; data ticks fall
        // wherever they are, with empty rail beyond if the brush
        // extends past data range.
        if (overviewBars) {
            overviewBars.replaceChildren();
            for (const row of rows) {
                const left = ((row.timestamp - railStartMs) / fullSpan) * 100;
                const width = Math.max(0.05, (Math.max(50, row.duration) / fullSpan) * 100);
                const tick = document.createElement('div');
                tick.className = 'netwf-overview-tick' + waterfallRowStatusClasses(row);
                tick.style.left = `${left.toFixed(3)}%`;
                tick.style.width = `${width.toFixed(3)}%`;
                overviewBars.appendChild(tick);
            }
        }

        // Position the brush to match its current state, in rail coords.
        if (brushEl) {
            const left = ((brush.startMs - railStartMs) / fullSpan) * 100;
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
        const visibleRowsRaw = rows.filter((row) => {
            const reqEnd = row.timestamp + Math.max(50, row.duration);
            return reqEnd >= brush.startMs && row.timestamp <= brush.endMs;
        });
        const sort = getNetworkWaterfallSort(key);
        const visibleRows = applyNetworkWaterfallSort(visibleRowsRaw, sort);

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
        const timeHdr = axisEl.querySelector('.netwf-cell.time .netwf-cell-label');
        if (timeHdr) timeHdr.textContent = `${visibleRows.length}/${rows.length}`;
        paintWaterfallSortIndicators(axisEl, sort);
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
        // Repaint the event-marker guide (if any) using the freshly
        // rendered axis scale and brush window.
        paintNetworkLogEventMarker(card, key);

        // Following Latest: snap the *inner* scroll to the bottom.
        // The list now has its own scroll surface (max-height +
        // overflow-y:auto), so chasing the live tail never moves the
        // page itself. No "near-bottom" guard needed.
        if (isNetworkLogFollowMode(key) && scrollHost) {
            scrollHost.scrollTop = scrollHost.scrollHeight;
        }
        // Lazy-attach the alt-wheel handler that lets normal mouse
        // wheel scroll the page, and Alt/Option+wheel scroll the
        // inner list. One handler per scrollHost.
        if (scrollHost && !scrollHost.dataset.netwfWheelBound) {
            scrollHost.dataset.netwfWheelBound = '1';
            attachWaterfallAltWheel(scrollHost);
        }
        updateNetworkLogFollowButton(card, key);

        // (Auto-disable-on-scroll-away removed — too many false
        // positives from layout reflow / DOM refresh briefly hiding
        // the sentinel. Following Latest now persists until the user
        // explicitly clicks the button.)

        // Lazy-attach hover tooltip handlers on the chart host.
        if (chartHost && !chartHost.dataset.netwfTipBound) {
            chartHost.dataset.netwfTipBound = '1';
            attachWaterfallHoverTooltip(chartHost);
        }
    }

    // Wheel handler: by default the page scrolls (the list's own
    // overflow scroll is bypassed). Hold Alt/Option to scroll the
    // list internally instead. This avoids the dashboard becoming a
    // mouse-wheel maze where every region traps your scroll until
    // it bottoms out, while still giving the user a path to scrub
    // through the row history when they want to.
    function attachWaterfallAltWheel(scrollHost) {
        scrollHost.addEventListener('wheel', (event) => {
            if (event.altKey) {
                // Native inner scroll — let the browser do its thing.
                return;
            }
            // Otherwise: cancel the native wheel target, hand the
            // scroll delta off to the page.
            event.preventDefault();
            window.scrollBy({ top: event.deltaY, left: event.deltaX, behavior: 'auto' });
        }, { passive: false });
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
        return `${row.timestamp}|${Math.round(row.duration)}|${row.entry.status || ''}|${row.entry.bytes_out || 0}|${row.label}|${row.entry.faulted ? 'F' : ''}|${row.entry.fault_type || ''}|${row.entry.fault_category || ''}|${row.attempt || 1}|${row.is_retry ? 'R' : ''}|${row.entry.request_range || ''}|${isSlowSegmentTransfer(row) ? 'S' : ''}`;
    }

    // HLS default target duration is 6s. A media-segment transfer that
    // takes longer than that to download means the player will start to
    // stall — flag it so it stands out in the row list. Only the actual
    // transfer phase counts: dns/connect/tls/wait don't reduce segment
    // availability.
    const SLOW_SEGMENT_TRANSFER_MS = 6000;
    const MEDIA_SEGMENT_PATH_RE = /\.(m4s|ts|mp4|m4a|m4v|aac|webm|mp3)(\?|$)/i;
    function isSlowSegmentTransfer(row) {
        const url = String((row && row.entry && (row.entry.url || row.entry.path)) || '');
        if (!MEDIA_SEGMENT_PATH_RE.test(url)) return false;
        return Number(row && row.transfer || 0) > SLOW_SEGMENT_TRANSFER_MS;
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
        if (isSlowSegmentTransfer(row)) cls.push(' slow-transfer');
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
        // Distinguish the three "looked like 200, ended badly" cases
        // since the status column alone can't tell them apart:
        //   ✂ socket fault inject — server deliberately cut the body
        //                           (request_body_*/connect_*/first_byte_*).
        //                           Status is forced to 503 by the proxy.
        //   ⏱ transfer timeout    — server gave up on slow upstream or
        //                           idle player drain (transfer_active_/
        //                           idle_timeout). Status is upstream's
        //                           original (typ. 200) for mid-body, or
        //                           504 if it tripped pre-headers.
        //   ↩ client disconnect  — the player aborted mid-transfer.
        //                           Status is upstream's original.
        // Everything else (HTTP 4xx/5xx, transport drop/reject, corruption)
        // keeps the plain "!" — the status code already tells the story.
        const faultCategory = String(row.entry.fault_category || '').toLowerCase();
        let faultGlyph = '';
        if (row.entry.faulted) {
            if (faultCategory === 'socket') faultGlyph = '!✂';
            else if (faultCategory === 'transfer_timeout') faultGlyph = '!⏱';
            else if (faultCategory === 'client_disconnect') faultGlyph = '!↩';
            else faultGlyph = '!';
        }
        // ⏰ flags a media-segment transfer that took longer than the
        // default HLS target duration (6 s). Player stalls if this
        // happens regularly.
        const slowGlyph = isSlowSegmentTransfer(row) ? '⏰' : '';
        const flags = (row.is_retry ? '↻' : '') + faultGlyph + slowGlyph;
        const statusCode = Number(row.entry.status) || 0;
        const flagColor = row.is_retry
            ? '#be185d'
            : (row.entry.faulted ? '#7f1d1d' : (isSlowSegmentTransfer(row) ? '#92400e' : ''));

        const cells = [
            { col: 'time', text: tsLabel },
            { col: 'flags', text: flags, color: flagColor },
            { col: 'method', text: method },
            { col: 'path', text: path, title: row.entry.url || row.entry.path || '' },
            { col: 'bytes', text: bytesLabel },
            { col: 'mbps', text: mbpsLabel },
            { col: 'duration', text: formatMilliseconds(row.duration) },
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
        { key: 'time',   label: 'Time',     min: 60, sortable: true },
        { key: 'flags',  label: '',         min: 16 },
        { key: 'method', label: 'Method',   min: 30, sortable: true },
        { key: 'path',   label: 'Path',     min: 60, sortable: true },
        // Single-unit columns: header carries the unit, cells carry
        // tabular-aligned numbers. Bytes always rendered in KB,
        // throughput always in Mbps — easier to scan than mixed units.
        { key: 'bytes',  label: 'KB',       min: 50, sortable: true },
        { key: 'mbps',   label: 'Mbps',     min: 50, sortable: true },
        // Duration is the request's full elapsed wall-clock from
        // first byte sent to last byte received (dns + connect + tls
        // + wait + transfer). Sortable so the slowest requests are
        // one click away.
        { key: 'duration', label: 'Dur',   min: 60, sortable: true },
        { key: 'status', label: 'Status',   min: 40, sortable: true }
    ];

    function netwfRowSortValue(row, col) {
        switch (col) {
            case 'time':     return Number(row.timestamp) || 0;
            case 'method':   return String(row.entry.method || '');
            case 'path':     return String(row.pathDisplay || row.filename || row.entry.path || '');
            case 'bytes':    return Number(row.entry.bytes_out || 0);
            case 'mbps':     return (Number(row.transfer || 0) > 0)
                                 ? (Number(row.entry.bytes_out || 0) * 8) / (Number(row.transfer) * 1000)
                                 : 0;
            case 'duration': return Number(row.duration || 0);
            case 'status':   return Number(row.entry.status || 0);
            default:         return 0;
        }
    }

    function getNetworkWaterfallSort(key) {
        return networkWaterfallSortBySession.get(String(key || '')) || { col: null, dir: 'desc' };
    }

    function cycleNetworkWaterfallSort(key, col) {
        const k = String(key || '');
        const cur = getNetworkWaterfallSort(k);
        let next;
        if (cur.col !== col) next = { col, dir: 'desc' };
        else if (cur.dir === 'desc') next = { col, dir: 'asc' };
        else next = { col: null, dir: 'desc' };
        networkWaterfallSortBySession.set(k, next);
    }

    function applyNetworkWaterfallSort(rows, sort) {
        if (!sort || !sort.col) return rows;
        const sign = sort.dir === 'asc' ? 1 : -1;
        const sorted = rows.slice();
        sorted.sort((a, b) => {
            const av = netwfRowSortValue(a, sort.col);
            const bv = netwfRowSortValue(b, sort.col);
            if (typeof av === 'string' || typeof bv === 'string') {
                return String(av).localeCompare(String(bv)) * sign;
            }
            return (av - bv) * sign;
        });
        return sorted;
    }

    function buildWaterfallAxisRow(chartHost) {
        const axisEl = document.createElement('div');
        axisEl.className = 'netwf-axis';
        for (const col of NETWF_COLUMNS) {
            const cell = document.createElement('div');
            cell.className = `netwf-cell ${col.key}`;
            cell.dataset.col = col.key;
            const labelEl = document.createElement('span');
            labelEl.className = 'netwf-cell-label';
            labelEl.textContent = col.label;
            cell.appendChild(labelEl);
            if (col.sortable) {
                cell.classList.add('sortable');
                const arrowEl = document.createElement('span');
                arrowEl.className = 'netwf-cell-sort-arrow';
                cell.appendChild(arrowEl);
                cell.addEventListener('click', (event) => {
                    if (event.target.closest('.netwf-cell-resizer')) return;
                    const card = chartHost.closest('.session-card');
                    const sessionId = card ? card.dataset.sessionId : null;
                    if (!sessionId) return;
                    cycleNetworkWaterfallSort(sessionId, col.key);
                    updateNetworkWaterfall(card, sessionId);
                });
            }
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

    function paintWaterfallSortIndicators(axisEl, sort) {
        if (!axisEl) return;
        const activeCol = sort && sort.col;
        const dir = sort && sort.dir;
        for (const cell of axisEl.querySelectorAll('.netwf-cell.sortable')) {
            const isActive = cell.dataset.col === activeCol;
            cell.classList.toggle('sort-active', isActive);
            cell.classList.toggle('sort-asc', isActive && dir === 'asc');
            cell.classList.toggle('sort-desc', isActive && dir === 'desc');
            const arrow = cell.querySelector('.netwf-cell-sort-arrow');
            if (arrow) {
                arrow.textContent = isActive ? (dir === 'asc' ? ' ▲' : ' ▼') : '';
            }
        }
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

    // Cheap mid-drag brush reposition — updates the brush element's
    // left/width CSS only, without rebuilding the row table or
    // recomputing summaries. Used during drag so the waterfall
    // doesn't re-render on every mousemove. Mirrors the rail bounds
    // logic from updateNetworkWaterfall (rail = data ∪ brush).
    function repositionBrushOnly(card, sessionId) {
        const brush = networkWaterfallBrushBySession.get(String(sessionId));
        if (!brush) return;
        const brushEl = card.querySelector('[data-field="netwf_brush"]');
        if (!brushEl) return;
        const rows = networkWaterfallRowsBySession.get(String(sessionId)) || [];
        if (!rows.length) return;
        const dataStartMs = rows[0].timestamp;
        const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
        const railStartMs = Math.min(dataStartMs, brush.startMs);
        const railEndMs   = Math.max(dataEndMs,   brush.endMs);
        const fullSpan = Math.max(50, railEndMs - railStartMs);
        const left = ((brush.startMs - railStartMs) / fullSpan) * 100;
        const width = ((brush.endMs - brush.startMs) / fullSpan) * 100;
        brushEl.style.left = `${Math.max(0, left).toFixed(3)}%`;
        brushEl.style.width = `${Math.max(0.5, width).toFixed(3)}%`;
        // Mid-drag reposition of the event-marker guide too — the row
        // list isn't re-rendered until release, but the rail line and
        // the row-list line should track the brush in real time.
        paintNetworkLogEventMarker(card, sessionId);
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
            networkLogBrushDragging.add(sessionId);
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
            // Mid-drag: cheap reposition only (brush CSS left/width).
            // Full waterfall re-render fires on mouseup.
            repositionBrushOnly(card, sessionId);
            // Live-sync the main session-viewer brush so both rails
            // move together. live:true means "no chart re-render yet"
            // — the chart-side listener just repositions its brush.
            document.dispatchEvent(new CustomEvent('replay:brush-range-change', {
                detail: {
                    sessionId,
                    startMs: brush.startMs,
                    endMs: brush.endMs,
                    source: 'network-log',
                    live: true
                }
            }));
        };

        const onPointerUp = () => {
            if (!drag) return;
            drag = null;
            brushEl?.classList.remove('dragging');
            networkLogBrushDragging.delete(sessionId);
            // Final full render on release — picks up the brush range
            // and rebuilds the row table to match.
            updateNetworkWaterfall(card, sessionId);
            // Tell the main session-viewer brush to settle on this
            // range and trigger its full chart re-render.
            const brush = networkWaterfallBrushBySession.get(sessionId);
            if (brush) {
                document.dispatchEvent(new CustomEvent('replay:brush-range-change', {
                    detail: {
                        sessionId,
                        startMs: brush.startMs,
                        endMs: brush.endMs,
                        source: 'network-log',
                        live: false
                    }
                }));
            }
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
            // Sync the main brush — discrete click is a settle event,
            // so live:false to trigger the full chart re-render.
            document.dispatchEvent(new CustomEvent('replay:brush-range-change', {
                detail: {
                    sessionId,
                    startMs: brush.startMs,
                    endMs: brush.endMs,
                    source: 'network-log',
                    live: false
                }
            }));
        });

        document.addEventListener('mousemove', onPointerMove);
        document.addEventListener('mouseup', onPointerUp);
    }


    function updateNetworkLog(sessionId, options = {}) {
        if (!networkLogDeveloperEnabled) return;
        const key = String(sessionId || '');
        if (!key) return;
        if (options.skipIfInFlight && networkLogFetchInFlight.has(key)) return;
        // Skip while the user is actively dragging the main session-
        // viewer brush. Ambient 1.5s polling otherwise flickers the
        // waterfall mid-gesture; one render fires on release via
        // syncNetworkLogToBrush. Manual user actions (clicking
        // Refresh) bypass this check by passing skipIfDragging=false.
        const skipIfDragging = options.skipIfDragging !== false;
        if (skipIfDragging) {
            // Two drag sources to consider:
            //   1) the session-viewer's main brush (SessionShell flag)
            //   2) the network-log fold's own brush (local flag)
            // Either one being active means we should skip the
            // ambient refresh — the rebuild would flicker the row
            // table mid-gesture. Manual user actions (clicking
            // Refresh) bypass via skipIfDragging:false.
            if (window.SessionShell
                && window.SessionShell.brushDraggingBySession
                && window.SessionShell.brushDraggingBySession.has(key)) {
                return;
            }
            if (networkLogBrushDragging.has(key)) {
                return;
            }
        }

        const card = document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!card) {
            stopNetworkLogAutoRefresh(key);
            return;
        }

        const countBadge = card.querySelector('[data-field="network_log_count"]');
        networkLogFetchInFlight.add(key);

        // In replay mode, the live go-proxy session is gone — go straight
        // to the analytics archive. In live mode we hit go-proxy first
        // (cheaper, freshest); the archive is queried only as a fallback
        // for sessions whose proxy buffer has aged out.
        //
        // We deliberately do NOT filter by play_id on the archive query.
        // iOS HLS doesn't preserve the master-manifest's `?play_id=…`
        // query string on variant / segment URLs, so many requests land
        // with empty play_id even when the session is bound to a single
        // playback episode. Filtering by play_id would hide them. We
        // scope by TIME RANGE instead, using the play bounds that
        // session-replay.js publishes to SessionShell after first batch.
        // Without this scope the rail extends back to *the earliest
        // entry across all plays of this session_id* — easily hours
        // earlier than the play we're actually viewing.
        const isReplay = document.body.classList.contains('replay-mode');
        const archiveParams = new URLSearchParams({ session: key });
        if (isReplay && window.SessionShell && window.SessionShell.playBoundsBySession) {
            const bounds = window.SessionShell.playBoundsBySession.get(key);
            if (bounds && Number.isFinite(bounds.startMs) && Number.isFinite(bounds.endMs)) {
                archiveParams.set('from', new Date(bounds.startMs).toISOString());
                archiveParams.set('to',   new Date(bounds.endMs).toISOString());
            }
        }
        const archiveURL = `/analytics/api/network_requests?${archiveParams.toString()}`;
        const liveURL = `/api/session/${key}/network`;

        const apply = (entries) => {
            const count = entries.length;
            networkLogEntriesBySession.set(key, entries);
            if (countBadge) {
                countBadge.textContent = `${count} request${count !== 1 ? 's' : ''}`;
            }
            applyNetworkLogFilters(card);
        };

        const fetchFrom = (u) => fetch(u).then(r => {
            if (!r.ok) throw new Error(`HTTP ${r.status}`);
            return r.json();
        });

        const primary = isReplay ? archiveURL : liveURL;
        const fallback = isReplay ? null : archiveURL;
        fetchFrom(primary)
            .then(data => {
                const entries = data.entries || [];
                if (entries.length === 0 && fallback) {
                    return fetchFrom(fallback).then(d => apply(d.entries || []));
                }
                apply(entries);
            })
            .catch(error => {
                if (fallback) {
                    return fetchFrom(fallback)
                        .then(d => apply(d.entries || []))
                        .catch(err2 => {
                            console.error('Network log fetch (both sources) failed:', error, err2);
                            networkLogEntriesBySession.delete(key);
                            if (countBadge) countBadge.textContent = '0 requests';
                            updateNetworkWaterfall(card, key);
                        });
                }
                console.error('Network log fetch failed:', error);
                networkLogEntriesBySession.delete(key);
                if (countBadge) countBadge.textContent = '0 requests';
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

    // Sync the network-log brush to a given wall-clock range, so other
    // controls (e.g. the bitrate chart's pan/zoom) can drive the
    // waterfall's visible time window. Disengages Follow-Latest because
    // the caller is asserting a specific range.
    function setNetworkLogTimeRange(sessionId, fromMs, toMs) {
        const key = String(sessionId || '');
        if (!key) return;
        if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) return;
        networkWaterfallBrushBySession.set(key, { startMs: fromMs, endMs: toMs, follow: false });
        setNetworkLogFollowMode(key, false);
        const card = document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!card) return;
        // Only re-render if the network log fold is open — collapsed
        // sections will pick up the new range when the user opens them.
        const content = card.querySelector('[data-content="network-log"]');
        if (!content || content.style.display === 'none') return;
        updateNetworkWaterfall(card, key);
    }

    // Listen for replay event-marker picks. If the picked event's
    // timestamp falls outside the current network-log brush window,
    // shift the window (preserving its span) so the event lands at
    // the centre. Ignores in-window picks so the user's existing
    // zoom level is preserved when scanning nearby events.
    // Guarded against duplicate registration if this module's IIFE
    // runs more than once (live-reload, future SPA navigation).
    if (!window.__networkLogEventMarkerListenerBound) {
    window.__networkLogEventMarkerListenerBound = true;
    document.addEventListener('replay:event-marker', (ev) => {
        if (!ev || !ev.detail) return;
        const sessionId = String(ev.detail.sessionId || '');
        const tsMs = Number(ev.detail.tsMs);
        if (!sessionId) return;
        const card = document.querySelector(`.session-card[data-session-id="${sessionId}"]`);
        if (!Number.isFinite(tsMs)) {
            networkLogEventMarkerBySession.delete(sessionId);
            if (card) paintNetworkLogEventMarker(card, sessionId);
            return;
        }
        // Stash the marker so the next updateNetworkWaterfall (or
        // brush-only reposition) can paint the cyan guide line.
        networkLogEventMarkerBySession.set(sessionId, {
            tsMs,
            label: String(ev.detail.label || '')
        });
        // Fresh pick → ask the next paint to scroll the row list so
        // the closest-to-event row is in view. One-shot; cleared by
        // paintNetworkLogEventMarker once the scroll is performed.
        networkLogPendingScrollBySession.add(sessionId);
        const brush = networkWaterfallBrushBySession.get(sessionId);
        // No brush yet (network log fold not opened) — bail; the
        // marker we just stashed will be painted on first render.
        if (!brush) {
            if (card) paintNetworkLogEventMarker(card, sessionId);
            return;
        }
        const span = brush.endMs - brush.startMs;
        if (span <= 0) {
            if (card) paintNetworkLogEventMarker(card, sessionId);
            return;
        }
        // 5 % padding inside the brush so the marker doesn't sit
        // exactly on the edge.
        const pad = span * 0.05;
        if (tsMs >= brush.startMs + pad && tsMs <= brush.endMs - pad) {
            // Already in view — just repaint the line at the current
            // position; no scroll needed.
            if (card) paintNetworkLogEventMarker(card, sessionId);
            return;
        }
        const half = span / 2;
        let newStart = tsMs - half;
        let newEnd = tsMs + half;
        // Clamp against the data range so we never scroll past the
        // first/last archived row.
        const rows = networkWaterfallRowsBySession.get(sessionId) || [];
        if (rows.length) {
            const dataStartMs = rows[0].timestamp;
            const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
            if (newStart < dataStartMs) { newStart = dataStartMs; newEnd = newStart + span; }
            if (newEnd > dataEndMs)     { newEnd = dataEndMs;     newStart = newEnd - span; }
        }
        setNetworkLogTimeRange(sessionId, newStart, newEnd);
        // setNetworkLogTimeRange runs updateNetworkWaterfall which
        // will pick up the marker via paintNetworkLogEventMarker
        // below.
    });
    }

    // Paint (or remove) the cyan event-marker guide on the network
    // log's overview rail and on the row-list's time scale. Called
    // at the end of updateNetworkWaterfall and also directly from
    // the replay:event-marker listener for in-window picks.
    function paintNetworkLogEventMarker(card, sessionId) {
        const key = String(sessionId);
        const marker = networkLogEventMarkerBySession.get(key);
        const overviewEl = card.querySelector('[data-field="netwf_overview"]');
        const chartHost = card.querySelector('[data-field="network_log_waterfall"]');
        const RAIL_ID = 'netwf_event_marker_rail';
        const ROW_ID  = 'netwf_event_marker_row';

        const removeIf = (host, id) => {
            if (!host) return;
            const el = host.querySelector(`[data-field="${id}"]`);
            if (el) el.remove();
        };
        if (!marker) {
            removeIf(overviewEl, RAIL_ID);
            removeIf(chartHost, ROW_ID);
            return;
        }

        // Overview rail line: rail coords = union(data, brush).
        if (overviewEl) {
            const rows = networkWaterfallRowsBySession.get(key) || [];
            const brush = networkWaterfallBrushBySession.get(key);
            if (rows.length && brush) {
                const dataStartMs = rows[0].timestamp;
                const dataEndMs = Math.max(...rows.map((r) => r.timestamp + Math.max(50, r.duration)));
                const railStartMs = Math.min(dataStartMs, brush.startMs);
                const railEndMs   = Math.max(dataEndMs,   brush.endMs);
                const fullSpan = Math.max(50, railEndMs - railStartMs);
                let line = overviewEl.querySelector(`[data-field="${RAIL_ID}"]`);
                if (marker.tsMs < railStartMs || marker.tsMs > railEndMs) {
                    if (line) line.remove();
                } else {
                    if (!line) {
                        line = document.createElement('div');
                        line.dataset.field = RAIL_ID;
                        line.style.cssText =
                            'position:absolute;top:-2px;bottom:-2px;width:2px;' +
                            `background:${NETLOG_EVENT_MARKER_COLOR};` +
                            'pointer-events:none;z-index:6;';
                        overviewEl.appendChild(line);
                    }
                    const leftPct = ((marker.tsMs - railStartMs) / fullSpan) * 100;
                    line.style.left = `calc(${leftPct.toFixed(3)}% - 1px)`;
                }
            }
        }

        // Row-list line: spans the entire chartHost vertically and is
        // positioned within the time-scale region (right of the resizable
        // columns). We anchor x to the axis's `netwf-axis-scale` element
        // because that's the live width of the time area as columns
        // resize. Time coords = brush window only.
        if (chartHost) {
            const brush = networkWaterfallBrushBySession.get(key);
            const axisScale = chartHost.querySelector('[data-field="netwf_axis_scale"]');
            if (!brush || !axisScale) {
                removeIf(chartHost, ROW_ID);
                return;
            }
            const span = brush.endMs - brush.startMs;
            if (span <= 0 || marker.tsMs < brush.startMs || marker.tsMs > brush.endMs) {
                removeIf(chartHost, ROW_ID);
                return;
            }
            // chartHost must be position:relative for absolute children
            // to anchor to it. Set on first paint; harmless to re-set.
            const cs = window.getComputedStyle(chartHost).position;
            if (cs === 'static') chartHost.style.position = 'relative';
            const hostRect = chartHost.getBoundingClientRect();
            const scaleRect = axisScale.getBoundingClientRect();
            const scaleLeftPx = scaleRect.left - hostRect.left;
            const scaleWidthPx = scaleRect.width;
            const fracInBrush = (marker.tsMs - brush.startMs) / span;
            const linePx = scaleLeftPx + fracInBrush * scaleWidthPx;
            let line = chartHost.querySelector(`[data-field="${ROW_ID}"]`);
            if (!line) {
                line = document.createElement('div');
                line.dataset.field = ROW_ID;
                line.style.cssText =
                    'position:absolute;top:0;bottom:0;width:0;' +
                    `border-left:1.5px dashed ${NETLOG_EVENT_MARKER_COLOR};` +
                    'pointer-events:none;z-index:6;';
                const label = document.createElement('div');
                label.dataset.field = ROW_ID + '_label';
                label.style.cssText =
                    'position:absolute;top:2px;left:4px;' +
                    `background:${NETLOG_EVENT_MARKER_COLOR};color:#fff;` +
                    'font:10px system-ui,-apple-system,sans-serif;' +
                    'padding:2px 4px;border-radius:2px;white-space:nowrap;';
                line.appendChild(label);
                chartHost.appendChild(line);
            }
            line.style.left = `${linePx.toFixed(2)}px`;
            line.style.height = `${chartHost.scrollHeight}px`;
            const labelEl = line.querySelector(`[data-field="${ROW_ID}_label"]`);
            if (labelEl) labelEl.textContent = marker.label || '';

            // Auto-scroll the row list vertically so the row whose
            // timestamp is closest to the event lands roughly at the
            // centre of the visible scroll area. One-shot per pick —
            // scrolling on every brush drag would yank the user
            // around while they're reading nearby rows.
            if (networkLogPendingScrollBySession.has(key)) {
                networkLogPendingScrollBySession.delete(key);
                const scrollHost = card.querySelector('[data-field="network_log_waterfall_scroll"]');
                if (scrollHost) {
                    // Disable Following Latest — its bottom-snap would
                    // immediately overwrite our targeted scroll.
                    if (isNetworkLogFollowMode(key)) {
                        setNetworkLogFollowMode(key, false);
                        updateNetworkLogFollowButton(card, key);
                    }
                    // chartHost children: [axis, row, row, ...]. Walk
                    // them and pick the row whose data-ts is closest
                    // to (and ≤, when possible) the marker timestamp.
                    let bestEl = null;
                    let bestDiff = Infinity;
                    for (let i = 1; i < chartHost.children.length; i++) {
                        const child = chartHost.children[i];
                        const rowData = child.__netwfRow;
                        const ts = rowData ? Number(rowData.timestamp) : Number(child.dataset.ts);
                        if (!Number.isFinite(ts)) continue;
                        const diff = Math.abs(ts - marker.tsMs);
                        if (diff < bestDiff) {
                            bestDiff = diff;
                            bestEl = child;
                        }
                    }
                    if (bestEl) {
                        const visibleH = scrollHost.clientHeight;
                        const targetTop = bestEl.offsetTop - (visibleH / 2) + (bestEl.offsetHeight / 2);
                        scrollHost.scrollTo({
                            top: Math.max(0, targetTop),
                            behavior: 'smooth'
                        });
                    }
                }
            }
        }
    }

    // Release the network-log brush back to its default (follow-latest,
    // rolling 2 min window). Used when the bitrate chart zoom is reset
    // so the waterfall doesn't stay pinned to a stale range.
    function clearNetworkLogTimeRange(sessionId) {
        const key = String(sessionId || '');
        if (!key) return;
        networkWaterfallBrushBySession.delete(key);
        setNetworkLogFollowMode(key, true);
        const card = document.querySelector(`.session-card[data-session-id="${key}"]`);
        if (!card) return;
        const content = card.querySelector('[data-content="network-log"]');
        if (!content || content.style.display === 'none') return;
        updateNetworkWaterfall(card, key);
    }

    // Propagate "All" / per-variant toggles in the three Fault Injection
    // scope checkbox groups (segment_failure_urls, manifest_failure_urls,
    // all_failure_urls). Used by both testing.html and testing-session.html
    // so all three groups behave identically:
    //   - Toggle "All" → every variant follows.
    //   - Toggle a variant → "All" auto-syncs to .every() of the variants.
    const SCOPE_FIELDS = new Set([
        'segment_failure_urls',
        'manifest_failure_urls',
        'all_failure_urls'
    ]);
    function applyScopeToggle(card, target) {
        if (!card || !target || !SCOPE_FIELDS.has(target.dataset.field)) return false;
        const field = target.dataset.field;
        const checks = Array.from(card.querySelectorAll(`input[data-field="${field}"]`));
        if (target.value === 'All') {
            checks.forEach(input => { input.checked = target.checked; });
            return true;
        }
        const allBox = checks.find(input => input.value === 'All');
        const scopedChecks = checks.filter(input => input.value !== 'All');
        if (allBox) allBox.checked = scopedChecks.every(input => input.checked);
        return true;
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
        applyScopeToggle,
        updateNetworkLog,
        applyNetworkLogFilters,
        updateNetworkWaterfall,
        setNetworkLogTimeRange,
        clearNetworkLogTimeRange
    };
})();
