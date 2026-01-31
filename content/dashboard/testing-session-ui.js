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
        { value: 'hung', text: 'Hung' }
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

    function renderPlaylistOptions(sessionId, playlists, selected) {
        const selectedSet = new Set(selected || []);
        const list = sortedPlaylists(playlists);
        const allChecked = selectedSet.has('All');
        const checkbox = (value, label) => {
            const checked = allChecked || selectedSet.has(value) ? 'checked' : '';
            return `<label><input type="checkbox" data-field="playlist_failure_urls" value="${value}" ${checked}>${label}</label>`;
        };
        const items = [checkbox('All', 'All'), checkbox('audio', 'Audio')];
        list.forEach(playlist => {
            const resolution = playlist.resolution || 'unknown';
            const height = resolution.includes('x') ? resolution.split('x')[1] : resolution;
            const heightLabel = height === 'unknown' ? 'unknown' : `${height}p`;
            const label = `${heightLabel}/${Math.round(playlist.bandwidth / 1000)}kbps`;
            items.push(checkbox(playlist.url, label));
        });
        return items.join('');
    }

    function variantFromPlaylistUrl(url) {
        if (!url) return '';
        const parts = url.split('/');
        if (parts.length > 1) {
            return parts[0] || '';
        }
        return url.replace(/\.m3u8.*$/i, '');
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
            const value = variantFromPlaylistUrl(playlist.url);
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

    function renderSessionCard(session, options = {}) {
        const sessionId = session.session_id;
        const playlistUrls = session.playlist_urls || [];
        const playlistSelected = session.playlist_failure_urls || [];
        const segmentSelected = session.segment_failure_urls || [];
        const inlineHost = options.inlineHost || false;
        const hideTitle = options.hideTitle || false;
        const showPortItem = options.showPortItem || false;
        return `
            <div class="session-card" data-session-id="${sessionId}" data-session-port="${session.x_forwarded_port || ''}">
                <div class="session-header">
                    ${hideTitle ? '' : `<div class="session-title">Session ${sessionId}</div>`}
                    <div class="session-meta" title="Port">${session.x_forwarded_port || '—'}</div>
                </div>
                ${inlineHost ? `<div class="session-inline-player" data-inline-host="${sessionId}"></div>` : ''}
                <div class="session-grid">
                    <div class="session-item"><span class="label">User Agent</span><span class="value">${session.user_agent || '—'}</span></div>
                    <div class="session-item"><span class="label">Player IP</span><span class="value">${session.player_ip || '—'}</span></div>
                    ${showPortItem ? `<div class="session-item"><span class="label">Port</span><span class="value">${session.x_forwarded_port || '—'}</span></div>` : ''}
                    <div class="session-item"><span class="label">Last Request</span><span class="value">${formatDate(session.last_request)}</span></div>
                    <div class="session-item"><span class="label">First Request</span><span class="value">${formatDate(session.first_request_time)}</span></div>
                    <div class="session-item"><span class="label">Session Duration</span><span class="value">${formatDuration(session.session_duration)}</span></div>
                    <div class="session-item"><span class="label">Manifest URL</span><span class="value">${session.manifest_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Last Request URL</span><span class="value">${session.last_request_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Last Playlist</span><span class="value">${session.last_playlist_url || '—'}</span></div>
                    <div class="session-item"><span class="label">Counts</span><span class="value">M:${session.manifests_count || 0} P:${session.playlists_count || 0} S:${session.segments_count || 0}</span></div>
                    <div class="session-item"><span class="label">Measured Mbps</span><span class="value">${session.measured_mbps || '—'}</span></div>
                </div>
                <div class="failure-groups">
                    <div class="failure-group">
                        <div class="failure-title">Segment Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`segment_failure_type_${sessionId}`, session.segment_failure_type, segmentFailureTypes)}</div>
                        <div class="checkbox-group">${renderSegmentOptions(sessionId, playlistUrls, segmentSelected)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`segment_failure_mode_${sessionId}`, session.segment_failure_mode || 'failures_per_seconds')}
                        </div>
                        <div class="range-row">
                            <label>Consecutive</label>
                            <input type="range" min="0" max="10" step="1" data-field="segment_consecutive_failures" value="${session.segment_consecutive_failures > 0 ? session.segment_consecutive_failures : 1}">
                            <span class="range-value">${session.segment_consecutive_failures > 0 ? session.segment_consecutive_failures : 1}</span>
                        </div>
                        <div class="range-row">
                            <label>Frequency</label>
                            <input type="range" min="0" max="10" step="1" data-field="segment_failure_frequency" value="${session.segment_failure_frequency > 0 ? session.segment_failure_frequency : 6}">
                            <span class="range-value">${session.segment_failure_frequency > 0 ? session.segment_failure_frequency : 6}</span>
                        </div>
                    </div>
                    <div class="failure-group">
                        <div class="failure-title">Playlist Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`playlist_failure_type_${sessionId}`, session.playlist_failure_type)}</div>
                        <div class="checkbox-group">${renderPlaylistOptions(sessionId, playlistUrls, playlistSelected)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`playlist_failure_mode_${sessionId}`, session.playlist_failure_mode || 'failures_per_seconds')}
                        </div>
                        <div class="range-row">
                            <label>Consecutive</label>
                            <input type="range" min="0" max="10" step="1" data-field="playlist_consecutive_failures" value="${session.playlist_consecutive_failures > 0 ? session.playlist_consecutive_failures : 1}">
                            <span class="range-value">${session.playlist_consecutive_failures > 0 ? session.playlist_consecutive_failures : 1}</span>
                        </div>
                        <div class="range-row">
                            <label>Frequency</label>
                            <input type="range" min="0" max="10" step="1" data-field="playlist_failure_frequency" value="${session.playlist_failure_frequency > 0 ? session.playlist_failure_frequency : 6}">
                            <span class="range-value">${session.playlist_failure_frequency > 0 ? session.playlist_failure_frequency : 6}</span>
                        </div>
                    </div>
                    <div class="failure-group">
                        <div class="failure-title">Manifest Failures</div>
                        <div class="radio-group">${renderFailureTypeOptions(`manifest_failure_type_${sessionId}`, session.manifest_failure_type)}</div>
                        <div class="radio-group">
                            <div class="label">Units</div>
                            ${renderModeOptions(`manifest_failure_mode_${sessionId}`, session.manifest_failure_mode || 'failures_per_seconds')}
                        </div>
                        <div class="range-row">
                            <label>Consecutive</label>
                            <input type="range" min="0" max="10" step="1" data-field="manifest_consecutive_failures" value="${session.manifest_consecutive_failures > 0 ? session.manifest_consecutive_failures : 1}">
                            <span class="range-value">${session.manifest_consecutive_failures > 0 ? session.manifest_consecutive_failures : 1}</span>
                        </div>
                        <div class="range-row">
                            <label>Frequency</label>
                            <input type="range" min="0" max="10" step="1" data-field="manifest_failure_frequency" value="${session.manifest_failure_frequency > 0 ? session.manifest_failure_frequency : 6}">
                            <span class="range-value">${session.manifest_failure_frequency > 0 ? session.manifest_failure_frequency : 6}</span>
                        </div>
                    </div>
                </div>
                <div class="failure-group" data-net-shaping>
                    <div class="failure-title">Network Shaping</div>
                    <div class="range-row">
                        <label>Throughput (Mbps)</label>
                        <input type="range" min="0" max="30" step="0.1" data-field="shaping_throughput_mbps" value="${session.nftables_bandwidth_mbps || 0}">
                        <span class="range-value">${session.nftables_bandwidth_mbps || 0}</span>
                    </div>
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
                    <div class="chart-wrap">
                        <canvas class="bandwidth-chart" width="820" height="220" data-field="bandwidth_chart"></canvas>
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
        const getRadioValue = (name) => {
            const selected = card.querySelector(`input[name="${name}"]:checked`);
            return selected ? selected.value : 'none';
        };

        const segmentFailureType = getRadioValue(`segment_failure_type_${sessionId}`);
        const playlistFailureType = getRadioValue(`playlist_failure_type_${sessionId}`);
        const manifestFailureType = getRadioValue(`manifest_failure_type_${sessionId}`);

        const segmentFailureUnits = getRadioValue(`segment_failure_units_${sessionId}`) || 'requests';
        const playlistFailureUnits = getRadioValue(`playlist_failure_units_${sessionId}`) || 'requests';
        const manifestFailureUnits = getRadioValue(`manifest_failure_units_${sessionId}`) || 'requests';
        const segmentMode = getRadioValue(`segment_failure_mode_${sessionId}`) || modeFromUnits(null, null, segmentFailureUnits);
        const playlistMode = getRadioValue(`playlist_failure_mode_${sessionId}`) || modeFromUnits(null, null, playlistFailureUnits);
        const manifestMode = getRadioValue(`manifest_failure_mode_${sessionId}`) || modeFromUnits(null, null, manifestFailureUnits);
        const segmentUnits = unitsFromMode(segmentMode);
        const playlistUnits = unitsFromMode(playlistMode);
        const manifestUnits = unitsFromMode(manifestMode);

        const getRangeValue = (field) => {
            const input = card.querySelector(`input[data-field="${field}"]`);
            return input ? Number(input.value) : 0;
        };

        const playlistChecks = Array.from(card.querySelectorAll('input[data-field="playlist_failure_urls"]:checked'))
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
            playlist_failure_at: null,
            playlist_failure_recover_at: null,
            playlist_failure_type: playlistFailureType,
            playlist_failure_frequency: getRangeValue('playlist_failure_frequency'),
            playlist_consecutive_failures: getRangeValue('playlist_consecutive_failures'),
            playlist_failure_units: playlistFailureUnits,
            playlist_consecutive_units: playlistUnits.consecutiveUnits,
            playlist_frequency_units: playlistUnits.frequencyUnits,
            playlist_failure_mode: playlistMode,
            playlist_failure_urls: playlistChecks,
            manifest_failure_at: null,
            manifest_failure_recover_at: null,
            manifest_failure_type: manifestFailureType,
            manifest_failure_frequency: getRangeValue('manifest_failure_frequency'),
            manifest_consecutive_failures: getRangeValue('manifest_consecutive_failures'),
            manifest_failure_units: manifestFailureUnits,
            manifest_consecutive_units: manifestUnits.consecutiveUnits,
            manifest_frequency_units: manifestUnits.frequencyUnits,
            manifest_failure_mode: manifestMode
        };
    }

    window.TestingSessionUI = {
        renderSessionCard,
        readSessionSettings,
        formatDate,
        formatDuration
    };
})();
