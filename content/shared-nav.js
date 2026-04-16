/**
 * InfiniteStream - Shared Navigation Component
 * Injects sidebar and header navigation into pages
 */

(function() {
    'use strict';

    // Configuration
    const NAVIGATION = {
        main: [
            { id: 'dashboard', icon: '📊', text: 'Dashboard', href: '/dashboard/dashboard.html' }
        ],
        content: [
            { id: 'upload', icon: '📤', text: 'Upload Content', href: '/dashboard/upload.html' },
            { id: 'sources', icon: '📚', text: 'Source Library', href: '/dashboard/sources.html' },
            { id: 'jobs', icon: '💼', text: 'Encoding Jobs', href: '/dashboard/jobs.html' }
        ],
        testing: [
            { id: 'playback', icon: '▶️', text: 'Playback', href: '/dashboard/playback.html' },
            { id: 'test-playback', icon: '🧭', text: 'Testing Playback', href: '/dashboard/testing-session.html?nav=1' },
            { id: 'testing', icon: '🧪', text: 'Testing Monitor', href: '/dashboard/testing.html' },
            { id: 'quartet', icon: '🎬', text: 'Quartet', href: '/dashboard/quartet.html' },
            { id: 'grid', icon: '🎮', text: 'Mosaic', href: '/dashboard/grid.html', warning: true },
            { id: 'segment-duration', icon: '⏱️', text: 'Live Offset', href: '/dashboard/segment-duration-comparison.html' }
        ],
        live: [
            { id: 'monitor', icon: '📡', text: 'Monitor', href: '/dashboard/go-monitor.html' }
        ],
        development: [
            { id: 'hlsjs-demo', icon: '🧭', text: 'HLS.js Demo', href: '/testing/hlsjs/index.html' },
            { id: 'shaka-demo', icon: '🛰️', text: 'Shaka Analysis', href: '/testing/shaka-player-test.html' }
        ]
    };

    // Check if developer mode is enabled
    function isDeveloperMode() {
        const urlParams = new URLSearchParams(window.location.search);
        return urlParams.get('developer') === '1';
    }

    function isExpertUrl() {
        const urlParams = new URLSearchParams(window.location.search);
        return urlParams.get('expert') === '1';
    }

    function isJeoliverHost(hostname) {
        if (!hostname) return false;
        const host = String(hostname).toLowerCase();
        return host === 'jeoliver.com' || host.endsWith('.jeoliver.com');
    }

    function isInternalNetworkHost(hostname) {
        if (!hostname) return false;
        const host = String(hostname).toLowerCase();
        if (host === 'localhost' || host === '::1' || host.startsWith('127.')) return true;
        if (host.endsWith('.local')) return true;
        if (!host.includes('.')) return true;
        if (/^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
        if (/^192\.168\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
        if (/^172\.(1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
        if (/^100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
        return false;
    }

    function shouldRestrictContentManagement() {
        const host = window.location.hostname || '';
        if (isJeoliverHost(host)) return true;
        return !isInternalNetworkHost(host);
    }

    function shouldRestrictMonitorAccess() {
        return shouldRestrictContentManagement();
    }

    function resolvePreferredStreamHost(sourceHostname) {
        const currentHost = (window.location.hostname || '').toLowerCase();
        const sourceHost = (sourceHostname || '').toLowerCase();
        const baseHost = currentHost || sourceHost;
        if (!baseHost) return '';
        if (isJeoliverHost(baseHost)) {
            if (baseHost === 'jeoliver.com' || baseHost === 'www.jeoliver.com') {
                return 'infinitestreaming.jeoliver.com';
            }
            return baseHost;
        }
        return baseHost;
    }

    function isTestingPort(port) {
        const parsed = Number(port);
        if (!Number.isInteger(parsed) || parsed < 1000) return false;
        const suffix = parsed % 1000;
        return suffix >= 81 && suffix <= 881 && suffix % 100 === 81;
    }

    function deriveProxyPort(uiPort) {
        return uiPort.slice(0, -3) + '081';
    }

    function resolveTestingPort(sourcePort) {
        const currentPort = window.location.port || (window.location.protocol === 'https:' ? '443' : '80');
        if (isTestingPort(currentPort)) {
            return String(currentPort);
        }
        if (isTestingPort(sourcePort)) {
            return String(sourcePort);
        }
        return deriveProxyPort(currentPort);
    }

    function normalizeTestingBaseUrl(url) {
        const parsed = new URL(url, window.location.origin);
        const preferredHost = resolvePreferredStreamHost(parsed.hostname);
        if (preferredHost) {
            parsed.hostname = preferredHost;
        }
        parsed.port = resolveTestingPort(parsed.port);
        parsed.protocol = window.location.protocol;
        return parsed.toString();
    }

    function buildTestingUrl(url, playerId) {
        const base = normalizeTestingBaseUrl(url);
        const separator = base.includes('?') ? '&' : '?';
        return `${base}${separator}player_id=${encodeURIComponent(playerId)}`;
    }

    function createPlayerId() {
        if (window.crypto && window.crypto.getRandomValues) {
            const bytes = new Uint8Array(6);
            window.crypto.getRandomValues(bytes);
            let value = '';
            bytes.forEach(byte => {
                value += byte.toString(16).padStart(2, '0');
            });
            return value.slice(0, 8);
        }
        return Math.random().toString(36).slice(2, 10);
    }

    function getOrCreateTestPlaybackPlayerId() {
        const storageKey = 'ismTestPlaybackPlayerId';
        try {
            const stored = localStorage.getItem(storageKey);
            if (stored) return stored;
        } catch {
            return createPlayerId();
        }
        const id = createPlayerId();
        try {
            localStorage.setItem(storageKey, id);
        } catch {
            // Ignore storage failures (e.g., quota, disabled storage).
        }
        return id;
    }

    // Get active page
    function getActivePage() {
        const body = document.body;
        if (body && body.dataset && body.dataset.ismActivePage) {
            return body.dataset.ismActivePage;
        }
        const path = window.location.pathname;
        const filename = path.split('/').pop() || 'dashboard.html';
        
        // Check all navigation items
        for (const section in NAVIGATION) {
            const item = NAVIGATION[section].find(item => 
                item.href && item.href.includes(filename)
            );
            if (item) return item.id;
        }
        
        // Default to dashboard
        return 'dashboard';
    }

    // Build sidebar HTML
    function buildSidebar(activePage) {
        const isDeveloper = isDeveloperMode();
        const restrictContent = shouldRestrictContentManagement();
        const restrictMonitor = shouldRestrictMonitorAccess();
        
        const sections = [
            { title: 'MAIN', items: NAVIGATION.main },
            { title: 'CONTENT', items: NAVIGATION.content },
            { title: 'PLAYBACK', items: NAVIGATION.testing },
            { title: 'LIVE STREAMING', items: NAVIGATION.live },
            { title: 'DEVELOPMENT', items: NAVIGATION.development }
        ];

        let html = '<div class="ism-sidebar" id="sidebar">';
        
        // Sidebar header
        html += '<div class="ism-sidebar-header">';
        html += '<div class="ism-logo">';
        html += '<span class="ism-logo-icon">🎬</span>';
        html += '<span class="ism-logo-text">InfiniteStream</span>';
        html += '</div>';
        html += '<button class="ism-sidebar-collapse" id="sidebarCollapseBtn" title="Collapse sidebar" aria-label="Collapse sidebar">⇤</button>';
        html += '</div>';
        
        // Sidebar content
        html += '<div class="ism-sidebar-content">';
        
        sections.forEach(section => {
            if (section.title === 'DEVELOPMENT' && !isDeveloper) {
                return;
            }
            // Filter out developer-only items if not in developer mode
            const visibleItems = section.items.filter(item => 
                !item.developerOnly || isDeveloper
            );
            
            // Skip section if no visible items
            if (visibleItems.length === 0) return;
            
            html += '<div class="nav-section">';
            html += `<div class="nav-section-title">${section.title}</div>`;
            
            visibleItems.forEach(item => {
                const isContentRestrictedItem = restrictContent && section.title === 'CONTENT';
                const isMonitorRestrictedItem = restrictMonitor && section.title === 'LIVE STREAMING' && item.id === 'monitor';
                const isRestrictedItem = isContentRestrictedItem || isMonitorRestrictedItem;
                const isActive = item.id === activePage ? 'active' : '';
                const isDisabled = isRestrictedItem ? 'disabled' : '';
                const warning = item.warning ? '<span class="nav-item-warning">⚠️</span>' : '';
                const external = item.external ? ' target="_blank" rel="noopener"' : '';
                const href = isRestrictedItem ? '#' : item.href;
                const disabledAttrs = isRestrictedItem
                    ? ' aria-disabled="true" tabindex="-1" title="Available only on internal network hosts."'
                    : '';
                
                html += `<a id="nav-${item.id}" href="${href}" class="nav-item ${isActive} ${isDisabled}"${external}${disabledAttrs}>`;
                html += `<span class="nav-item-icon">${item.icon}</span>`;
                html += `<span class="nav-item-text">${item.text}</span>`;
                html += warning;
                html += '</a>';
            });
            
            html += '</div>';
        });
        
        html += '</div>';
        
        // Sidebar footer
        html += '<div class="ism-sidebar-footer">';
        html += '<a href="#" class="nav-item" onclick="window.BOSSNav.showInfo(); return false;">';
        html += '<span class="nav-item-icon">ℹ️</span>';
        html += '<span class="nav-item-text">Server Info</span>';
        html += '</a>';
        html += '<div class="nav-item nav-item-toggle">';
        html += '<label class="nav-toggle">';
        html += '<input type="checkbox" id="expertModeToggle">';
        html += '<span>Expert Mode</span>';
        html += '</label>';
        html += '</div>';
        html += '</div>';
        
        html += '</div>';
        
        return html;
    }

    // Build header HTML
    function buildHeader() {
        let html = '<div class="ism-header">';
        
        // Left side
        html += '<div class="ism-header-left">';
        html += '<div class="ism-header-title">InfiniteStream</div>';
        html += '</div>';
        
        // Center (reserved for future)
        html += '<div class="ism-header-center"></div>';
        
        // Right side
        html += '<div class="ism-header-right">';
        html += '<div class="ism-selected-content" id="ismSelectedContent" title="Selected content"></div>';
        html += '</div>';
        
        html += '</div>';
        
        return html;
    }

    // Build global progress indicator HTML
    function buildProgressIndicator() {
        return `
            <div class="global-progress-bar" id="globalProgressBar">
                <div class="global-progress-fill" id="globalProgressFill"></div>
            </div>
            <div class="global-progress-badge" id="globalProgressBadge">
                <div class="badge-header">
                    <span class="badge-title" id="badgeTitle">Uploading...</span>
                    <span class="badge badge-info badge-status" id="badgeStatus">0%</span>
                </div>
                <div class="badge-body">
                    <div class="badge-job-name" id="badgeJobName">video.mp4</div>
                    <div class="progress">
                        <div class="progress-bar" id="badgeProgressBar"></div>
                    </div>
                    <div class="badge-progress-text" id="badgeProgressText">Starting...</div>
                </div>
                <div class="badge-footer">
                    <button class="btn btn-sm btn-secondary" onclick="window.BOSSNav.dismissProgress()">Dismiss</button>
                    <button class="btn btn-sm btn-primary" onclick="window.BOSSNav.viewActiveJob()">View Details</button>
                </div>
            </div>
        `;
    }

    // Initialize navigation
    function initNavigation(options = {}) {
        const {
            showSidebar = true,
            showHeader = true,
            fullscreen = false
        } = options;

        const activePage = getActivePage();
        
        // Create app wrapper
        const appWrapper = document.createElement('div');
        appWrapper.className = 'ism-app';
        if (showSidebar) appWrapper.classList.add('has-sidebar');
        if (fullscreen) appWrapper.classList.add('ism-fullscreen');
        
        // Build navigation HTML
        let navHTML = '';
        if (showSidebar) navHTML += buildSidebar(activePage);
        
        // Main content wrapper
        navHTML += '<div class="ism-main">';
        if (showHeader) navHTML += buildHeader();
        navHTML += '<div class="ism-content" id="ism-content"></div>';
        navHTML += '</div>';
        
        appWrapper.innerHTML = navHTML;
        
        // Move existing body content into content area
        const contentArea = appWrapper.querySelector('#ism-content');
        while (document.body.firstChild) {
            contentArea.appendChild(document.body.firstChild);
        }
        
        // Add app wrapper to body
        document.body.appendChild(appWrapper);
        
        // Setup mobile menu
        setupMobileMenu();
        attachExpertToggle();
        setupSidebarCollapse();

        // Apply global selection state (codec/segment) across pages
        applyGlobalSelections();
        attachGlobalSelectionHandlers();
        
        // Add stats badge if on jobs page
        if (activePage === 'jobs') {
            fetchJobStats();
        }

        updateSelectedContentBadge();
        startSelectedContentWatcher();
        window.addEventListener('storage', (event) => {
            if (event.key === 'ismExpertMode') {
                renderPageHelp(getActivePage());
                return;
            }
            if (event.key && event.key.startsWith('ismSelected')) {
                updateSelectedContentBadge();
            }
        });

        initSetupExperience(activePage);

        renderPageHelp(activePage);
    }

    const GLOBAL_SELECTIONS = {
        codec: { key: 'ismSelectedCodec', ids: ['codecSelect', 'codecFilter'] },
        segment: { key: 'ismSelectedSegment', ids: ['segmentSelect', 'segmentFilter'] }
    };

    function applyGlobalSelections() {
        Object.values(GLOBAL_SELECTIONS).forEach(({ key, ids }) => {
            const stored = localStorage.getItem(key);
            if (!stored) return;
            ids.forEach((id) => {
                const el = document.getElementById(id);
                if (!el || el.value === stored) return;
                el.value = stored;
                el.dispatchEvent(new Event('change'));
            });
        });
    }

    function attachGlobalSelectionHandlers() {
        Object.values(GLOBAL_SELECTIONS).forEach(({ key, ids }) => {
            ids.forEach((id) => {
                const el = document.getElementById(id);
                if (!el) return;
                el.addEventListener('change', () => {
                    localStorage.setItem(key, el.value);
                });
            });
        });
    }

    // Setup mobile menu behavior (tap outside to close)
    function setupMobileMenu() {
        const sidebar = document.getElementById('sidebar');
        if (!sidebar) return;
        document.addEventListener('click', function(e) {
            if (window.innerWidth <= 768) {
                const isClickInsideSidebar = sidebar.contains(e.target);
                if (!isClickInsideSidebar && sidebar.classList.contains('mobile-open')) {
                    sidebar.classList.remove('mobile-open');
                }
            }
        });
    }

    function setupSidebarCollapse() {
        const app = document.querySelector('.ism-app');
        const btn = document.getElementById('sidebarCollapseBtn');
        if (!app) return;
        const collapsed = localStorage.getItem('ismSidebarCollapsed') === '1';
        if (collapsed) app.classList.add('sidebar-collapsed');
        if (!btn) return;
        const updateBtn = () => {
            const isCollapsed = app.classList.contains('sidebar-collapsed');
            btn.textContent = isCollapsed ? '⇥' : '⇤';
            btn.title = isCollapsed ? 'Expand sidebar' : 'Collapse sidebar';
            btn.setAttribute('aria-label', btn.title);
        };
        updateBtn();
        btn.addEventListener('click', () => {
            const nowCollapsed = !app.classList.contains('sidebar-collapsed');
            app.classList.toggle('sidebar-collapsed', nowCollapsed);
            localStorage.setItem('ismSidebarCollapsed', nowCollapsed ? '1' : '0');
            updateBtn();
        });
        // When collapsed, clicking anywhere on the sidebar peek expands it briefly
        const sidebar = document.getElementById('sidebar');
        if (sidebar) {
            sidebar.addEventListener('click', (e) => {
                if (!app.classList.contains('sidebar-collapsed')) return;
                if (e.target.closest('#sidebarCollapseBtn')) return;
                app.classList.remove('sidebar-collapsed');
                localStorage.setItem('ismSidebarCollapsed', '0');
                updateBtn();
            });
        }
    }

    function updateSelectedContentBadge() {
        const badge = document.getElementById('ismSelectedContent');
        const demoLink = document.getElementById('nav-hlsjs-demo');
        const shakaLink = document.getElementById('nav-shaka-demo');
        const testPlaybackLink = document.getElementById('nav-test-playback');
        if (!badge) return;
        const full = localStorage.getItem('ismSelectedContentFull') || localStorage.getItem('ismSelectedContent');
        const base = localStorage.getItem('ismSelectedContentBase');
        const url = localStorage.getItem('ismSelectedUrl') || '';
        let label = full || base || '';
        if (!label && url) {
            try {
                const parsed = new URL(url, window.location.origin);
                const parts = parsed.pathname.split('/').filter(Boolean);
                label = decodeURIComponent(parts[parts.length - 1] || '');
            } catch {
                const cleaned = url.split('?')[0].split('#')[0];
                const parts = cleaned.split('/').filter(Boolean);
                label = decodeURIComponent(parts[parts.length - 1] || '');
            }
        }
        if (!label && !url) {
            badge.textContent = '';
            badge.classList.remove('active');
            if (demoLink) {
                demoLink.href = 'https://hlsjs.video-dev.org/demo/';
                demoLink.removeAttribute('aria-disabled');
            }
            if (testPlaybackLink) {
                testPlaybackLink.href = '/dashboard/testing-session.html?nav=1';
                testPlaybackLink.removeAttribute('aria-disabled');
            }
            return;
        }

        badge.textContent = '';
        const labelSpan = document.createElement('span');
        labelSpan.className = 'ism-selected-label';
        labelSpan.textContent = label ? `Selected : ${label}` : 'Selected :';
        badge.appendChild(labelSpan);

        if (url) {
            const urlSpan = document.createElement('span');
            urlSpan.className = 'ism-selected-url';
            urlSpan.textContent = url;
            badge.appendChild(urlSpan);
            badge.title = `${label ? `Selected: ${label}\n` : ''}${url}`;
            if (demoLink || shakaLink || testPlaybackLink) {
                let absoluteUrl;
                try {
                    absoluteUrl = normalizeTestingBaseUrl(url);
                } catch {
                    if (url.startsWith('http://') || url.startsWith('https://')) {
                        absoluteUrl = url;
                    } else {
                        const suffix = url.startsWith('/') ? url : `/${url}`;
                        absoluteUrl = `${window.location.origin}${suffix}`;
                    }
                }
                if (testPlaybackLink) {
                    const playerId = getOrCreateTestPlaybackPlayerId();
                    testPlaybackLink.href = `/dashboard/testing-session.html?url=${encodeURIComponent(absoluteUrl)}&player_id=${encodeURIComponent(playerId)}&nav=1`;
                    testPlaybackLink.removeAttribute('aria-disabled');
                    testPlaybackLink.title = 'Open selected stream in Testing Playback';
                }
                if (demoLink) {
                    demoLink.href = `${window.location.origin}/testing/hlsjs/index.html?src=${encodeURIComponent(absoluteUrl)}`;
                    demoLink.removeAttribute('aria-disabled');
                    demoLink.title = 'Open selected stream in local HLS.js demo';
                }
                if (shakaLink) {
                    shakaLink.href = `${window.location.origin}/testing/shaka-player-test.html?src=${encodeURIComponent(absoluteUrl)}`;
                    shakaLink.removeAttribute('aria-disabled');
                    shakaLink.title = 'Open selected stream in local Shaka demo';
                }
            }
        } else {
            badge.title = label ? `Selected: ${label}` : 'Selected';
            if (demoLink) {
                demoLink.href = `${window.location.origin}/testing/hlsjs/index.html`;
                demoLink.removeAttribute('aria-disabled');
            }
            if (shakaLink) {
                shakaLink.href = `${window.location.origin}/testing/shaka-player-test.html`;
                shakaLink.removeAttribute('aria-disabled');
            }
            if (testPlaybackLink) {
                testPlaybackLink.href = '/dashboard/testing-session.html?nav=1';
                testPlaybackLink.removeAttribute('aria-disabled');
            }
        }

        badge.classList.add('active');
    }

    let lastSelectedSignature = '';
    function startSelectedContentWatcher() {
        setInterval(() => {
            const full = localStorage.getItem('ismSelectedContentFull') || localStorage.getItem('ismSelectedContent') || '';
            const base = localStorage.getItem('ismSelectedContentBase') || '';
            const url = localStorage.getItem('ismSelectedUrl') || '';
            const signature = `${full}|${base}|${url}`;
            if (signature !== lastSelectedSignature) {
                lastSelectedSignature = signature;
                updateSelectedContentBadge();
            }
        }, 1000);
    }

    function inferProtocolSegmentCodecFromUrl(url) {
        if (!url) {
            return;
        }
        const lower = String(url).toLowerCase();
        if (lower.includes('.mpd')) {
            localStorage.setItem('ismSelectedProtocol', 'dash');
        } else if (lower.includes('.m3u8')) {
            localStorage.setItem('ismSelectedProtocol', 'hls');
        }
        if (lower.includes('manifest_2s.mpd') || lower.includes('master_2s.m3u8') || lower.includes('/2s/')) {
            localStorage.setItem('ismSelectedSegment', '2s');
        } else if (lower.includes('manifest_6s.mpd') || lower.includes('master_6s.m3u8') || lower.includes('/6s/')) {
            localStorage.setItem('ismSelectedSegment', '6s');
        } else if (lower.includes('.mpd') || lower.includes('master.m3u8')) {
            localStorage.setItem('ismSelectedSegment', 'll');
        }
        if (lower.includes('_av1')) {
            localStorage.setItem('ismSelectedCodec', 'av1');
        } else if (lower.includes('_hevc') || lower.includes('_h265')) {
            localStorage.setItem('ismSelectedCodec', 'hevc');
        } else if (lower.includes('_h264')) {
            localStorage.setItem('ismSelectedCodec', 'h264');
        }
    }

    function setSelectedUrl(url) {
        if (url === null || url === undefined) {
            localStorage.removeItem('ismSelectedUrl');
        } else if (String(url).trim().length) {
            localStorage.setItem('ismSelectedUrl', url);
            inferProtocolSegmentCodecFromUrl(url);
        }
        updateSelectedContentBadge();
    }

    // Fetch job stats for badge
    async function fetchJobStats() {
        try {
            const response = await fetch('/api/jobs');
            const data = await response.json();
            const activeJobs = data.jobs.filter(j => 
                j.status === 'queued' || j.status === 'encoding'
            ).length;
            
            if (activeJobs > 0) {
                const jobsLink = document.querySelector('.nav-item[href="/dashboard/jobs.html"]');
                if (jobsLink && !jobsLink.querySelector('.nav-item-badge')) {
                    const badge = document.createElement('span');
                    badge.className = 'nav-item-badge';
                    badge.textContent = activeJobs;
                    jobsLink.appendChild(badge);
                }
            }
        } catch (error) {
            console.log('Could not fetch job stats:', error);
        }
    }

    let cachedServerVersion = null;

    async function fetchServerVersion() {
        if (cachedServerVersion) return cachedServerVersion;
        try {
            const response = await fetch('/api/version');
            if (!response.ok) {
                throw new Error(`HTTP ${response.status}`);
            }
            const data = await response.json();
            const version = String(data.version || '').trim();
            cachedServerVersion = version || 'unknown';
        } catch (error) {
            cachedServerVersion = 'unknown';
        }
        return cachedServerVersion;
    }

    // Show server info modal
    async function showInfo() {
        const version = await fetchServerVersion();
        const info = `
InfiniteStream - Media Testing Platform

Port: ${window.location.port || '80'}
Host: ${window.location.hostname}
Protocol: ${window.location.protocol}

Features:
• Video content upload & transcoding
• Multi-codec ABR ladder generation
• Source content library & re-encoding
• 16-player grid testing
• Live streaming simulation
• Network shaping & throttling

Version: ${version}
        `.trim();
        
        alert(info);
    }

    // Toggle fullscreen mode
    function toggleFullscreen() {
        const app = document.querySelector('.ism-app');
        if (app) {
            app.classList.toggle('ism-fullscreen');
        }
    }

    // Progress tracking state
    let activeJobId = null;
    let progressCheckInterval = null;

    // Setup progress indicator click handler
    function setupProgressIndicator() {
        const badge = document.getElementById('globalProgressBadge');
        if (badge) {
            badge.addEventListener('click', function(e) {
                // Don't trigger if clicking buttons
                if (!e.target.closest('button')) {
                    viewActiveJob();
                }
            });
        }
    }

    // Check for active jobs
    async function checkActiveJobs() {
        try {
            const response = await fetch('/api/jobs');
            const data = await response.json();
            
            // Find first active job (uploading or encoding)
            const activeJob = data.jobs.find(j => 
                j.status === 'uploading' || j.status === 'encoding'
            );
            
            if (activeJob) {
                activeJobId = activeJob.job_id;
                updateProgressIndicator(activeJob);
            } else {
                hideProgressIndicator();
                activeJobId = null;
            }
        } catch (error) {
            console.error('Failed to check active jobs:', error);
        }
    }

    // Update progress indicator UI
    function updateProgressIndicator(job) {
        const bar = document.getElementById('globalProgressBar');
        const badge = document.getElementById('globalProgressBadge');
        const fill = document.getElementById('globalProgressFill');
        const badgeFill = document.getElementById('badgeProgressBar');
        
        if (!bar || !badge || !fill || !badgeFill) return;
        
        // Show indicators
        bar.classList.add('active');
        badge.classList.add('active');
        
        // Update progress
        const progress = job.progress || 0;
        fill.style.width = progress + '%';
        badgeFill.style.width = progress + '%';
        
        // Update badge content
        const titleEl = document.getElementById('badgeTitle');
        const statusEl = document.getElementById('badgeStatus');
        const nameEl = document.getElementById('badgeJobName');
        const textEl = document.getElementById('badgeProgressText');
        
        if (titleEl) {
            titleEl.textContent = job.status === 'uploading' ? 'Uploading...' : 'Encoding...';
        }
        if (statusEl) {
            statusEl.textContent = progress + '%';
            statusEl.className = 'badge badge-status';
            if (job.status === 'uploading') {
                statusEl.classList.add('badge-info');
            } else if (job.status === 'encoding') {
                statusEl.classList.add('badge-warning');
            }
        }
        if (nameEl) {
            nameEl.textContent = job.name || 'Processing...';
        }
        if (textEl) {
            textEl.textContent = `${job.status} - ${progress}%`;
        }
        
        // Add pulsing animation for encoding
        if (job.status === 'encoding') {
            badge.classList.add('encoding');
        } else {
            badge.classList.remove('encoding');
        }
    }

    // Hide progress indicator
    function hideProgressIndicator() {
        const bar = document.getElementById('globalProgressBar');
        const badge = document.getElementById('globalProgressBadge');
        
        if (bar) bar.classList.remove('active');
        if (badge) badge.classList.remove('active', 'encoding');
    }

    // Start progress tracking
    function startProgressTracking() {
        checkActiveJobs(); // Immediate check
        progressCheckInterval = setInterval(checkActiveJobs, 2000); // Check every 2s
        
        // Also connect to SharedWorker for live upload progress (if supported)
        connectToUploadWorker();
    }

    // Connect to SharedWorker for live upload progress
    function connectToUploadWorker() {
        if (typeof SharedWorker === 'undefined') {
            console.log('[Nav] SharedWorker not supported, using API polling only');
            return;
        }
        
        try {
            const uploadWorker = new SharedWorker('/upload-worker.js');
            uploadWorker.port.start();
            
            console.log('[Nav] Connected to upload SharedWorker');
            
            // Listen for progress updates
            uploadWorker.port.onmessage = (e) => {
                const { type, jobId, progress, bytesUploaded, totalBytes } = e.data;
                
                // Only update if this is the active job we're tracking
                if (type === 'PROGRESS' && jobId === activeJobId) {
                    // Create a synthetic job object for the UI
                    const syntheticJob = {
                        job_id: jobId,
                        status: 'uploading',
                        progress: progress,
                        name: `Upload (${Math.round(bytesUploaded / (1024 * 1024))}MB / ${Math.round(totalBytes / (1024 * 1024))}MB)`
                    };
                    updateProgressIndicator(syntheticJob);
                }
            };
            
            uploadWorker.port.onerror = (error) => {
                console.error('[Nav] SharedWorker error:', error);
            };
            
        } catch (error) {
            console.warn('[Nav] Failed to connect to SharedWorker:', error);
        }
    }

    // View active job details
    function viewActiveJob() {
        if (activeJobId) {
            window.location.href = `/dashboard/job-detail.html?id=${activeJobId}`;
        }
    }

    // Dismiss progress badge
    function dismissProgress() {
        const badge = document.getElementById('globalProgressBadge');
        if (badge) {
            badge.classList.remove('active');
        }
    }

    // Cleanup on page unload
    window.addEventListener('beforeunload', function() {
        if (progressCheckInterval) {
            clearInterval(progressCheckInterval);
        }
    });

    const SETUP_PAGES_REQUIRE_CONTENT = new Set([
        'playback',
        'testing',
        'quartet',
        'grid',
        'segment-duration'
    ]);

    function initSetupExperience(activePage) {
        fetchSetupStatus()
            .then((status) => {
                renderSetupBanner(status, activePage);
                maybeShowSetupModal(status);
            })
            .catch((error) => {
                console.warn('Setup check failed:', error);
            });
    }

    async function fetchSetupStatus() {
        const response = await fetch('/api/setup');
        if (!response.ok) {
            throw new Error(`Setup status failed: ${response.status}`);
        }
        return response.json();
    }

    function renderSetupBanner(status, activePage) {
        const contentArea = document.querySelector('#ism-content');
        if (!contentArea) return;

        const existing = document.getElementById('setupBanner');
        if (existing) {
            existing.remove();
        }

        const hasIssues = status && Array.isArray(status.issues) && status.issues.length > 0;
        if (!hasIssues) {
            return;
        }

        const banner = document.createElement('div');
        banner.id = 'setupBanner';
        banner.className = 'alert alert-warning setup-banner';

        const issueLines = status.issues.map((issue) => `<div class="setup-issue">${issue}</div>`).join('');
        const recommendations = status.recommendations || [];
        const recLines = recommendations.map((rec) => `<li>${rec}</li>`).join('');

        const showContentActions = status.content_empty && SETUP_PAGES_REQUIRE_CONTENT.has(activePage);
        const isUploadPage = activePage === 'upload';

        const actions = [];
        actions.push('<button class="btn btn-sm btn-secondary" id="setupRunDiagnostics">Run Diagnostics</button>');
        actions.push('<button class="btn btn-sm btn-secondary" id="setupOpenGuide">Open Setup Guide</button>');
        if (!status.initialized) {
            actions.push('<button class="btn btn-sm btn-secondary" id="setupMarkInitialized">Mark Setup Complete</button>');
        }
        if (showContentActions && !isUploadPage) {
            actions.push('<button class="btn btn-sm btn-primary" id="setupGoUpload">Go to Upload</button>');
            actions.push('<button class="btn btn-sm btn-secondary" id="setupSeedSample">Seed Sample Content</button>');
        }

        banner.innerHTML = `
            <div class="alert-icon">⚠️</div>
            <div class="alert-content">
                <div class="alert-title">Setup attention needed</div>
                <div class="setup-issues">${issueLines}</div>
                ${recLines ? `<ul class="setup-recommendations">${recLines}</ul>` : ''}
                <div class="setup-actions">${actions.join('')}</div>
                ${showContentActions && !isUploadPage ? '<div class="setup-redirect" id="setupRedirect"></div>' : ''}
            </div>
        `;

        contentArea.prepend(banner);

        const diagBtn = document.getElementById('setupRunDiagnostics');
        if (diagBtn) {
            diagBtn.addEventListener('click', async () => {
                const updated = await fetchSetupStatus();
                renderSetupBanner(updated, activePage);
            });
        }
        const guideBtn = document.getElementById('setupOpenGuide');
        if (guideBtn) {
            guideBtn.addEventListener('click', () => showSetupModal(status));
        }
        const markBtn = document.getElementById('setupMarkInitialized');
        if (markBtn) {
            markBtn.addEventListener('click', async () => {
                await fetch('/api/setup/initialize', { method: 'POST' });
                const updated = await fetchSetupStatus();
                renderSetupBanner(updated, activePage);
            });
        }
        const uploadBtn = document.getElementById('setupGoUpload');
        if (uploadBtn) {
            uploadBtn.addEventListener('click', () => {
                window.location.href = '/dashboard/upload.html';
            });
        }
        const seedBtn = document.getElementById('setupSeedSample');
        if (seedBtn) {
            seedBtn.addEventListener('click', async () => {
                seedBtn.disabled = true;
                seedBtn.textContent = 'Seeding...';
                try {
                    await fetch('/api/setup/seed', { method: 'POST' });
                    seedBtn.textContent = 'Seeded';
                } catch (err) {
                    seedBtn.textContent = 'Seed Failed';
                }
            });
        }

        if (showContentActions && !isUploadPage) {
            setupAutoRedirect(status);
        }
    }

    function setupAutoRedirect(status) {
        if (!status || !status.content_empty) {
            return;
        }
        if (sessionStorage.getItem('ismSkipUploadRedirect') === '1') {
            return;
        }
        const redirectEl = document.getElementById('setupRedirect');
        if (!redirectEl) {
            return;
        }
        let seconds = 5;
        const cancel = () => {
            sessionStorage.setItem('ismSkipUploadRedirect', '1');
            redirectEl.textContent = 'Auto-redirect canceled.';
        };
        const tick = () => {
            if (seconds <= 0) {
                window.location.href = '/dashboard/upload.html';
                return;
            }
            redirectEl.innerHTML = `No content detected. Redirecting to Upload in ${seconds}s... <button class="btn btn-sm btn-secondary" id="setupCancelRedirect">Cancel</button>`;
            const cancelBtn = document.getElementById('setupCancelRedirect');
            if (cancelBtn) {
                cancelBtn.addEventListener('click', (event) => {
                    event.preventDefault();
                    cancel();
                });
            }
            seconds -= 1;
            setTimeout(tick, 1000);
        };
        tick();
    }

    function maybeShowSetupModal(status) {
        if (!status || status.initialized) {
            return;
        }
        if (sessionStorage.getItem('ismSetupModalShown') === '1') {
            return;
        }
        sessionStorage.setItem('ismSetupModalShown', '1');
        showSetupModal(status);
    }

    function showSetupModal(status) {
        const existing = document.getElementById('setupModalBackdrop');
        if (existing) {
            existing.remove();
        }
        const root = (status && status.root) ? status.root : '/media';
        const backdrop = document.createElement('div');
        backdrop.className = 'setup-modal-backdrop';
        backdrop.id = 'setupModalBackdrop';
        backdrop.innerHTML = `
            <div class="setup-modal">
                <div class="setup-modal-header">
                    <div class="setup-modal-title">First-Run Setup</div>
                    <button class="setup-modal-close" id="setupModalClose">✕</button>
                </div>
                <div class="setup-modal-body">
                    <ol class="setup-steps">
                        <li><strong>Mount a host folder</strong> to <code>${root}</code>.</li>
                        <li><strong>Upload content</strong> or seed a sample clip.</li>
                        <li><strong>Open Mosaic</strong> to preview streams.</li>
                    </ol>
                    <div class="setup-snippet">
                        <div class="setup-snippet-title">Docker Compose example</div>
                        <pre>services:
  infinite-streaming:
    volumes:
      - /path/to/InfiniteStream:${root}</pre>
                    </div>
                    <div class="setup-diagnostics">
                        <div class="setup-snippet-title">Diagnostics</div>
                        <pre>${formatSetupStatus(status)}</pre>
                    </div>
                </div>
                <div class="setup-modal-footer">
                    <button class="btn btn-sm btn-secondary" id="setupSeedSampleModal">Seed Sample Content</button>
                    <button class="btn btn-sm btn-secondary" id="setupOpenUploadModal">Open Upload</button>
                    <button class="btn btn-sm btn-primary" id="setupDoneModal">Mark Setup Complete</button>
                </div>
            </div>
        `;
        document.body.appendChild(backdrop);

        const closeBtn = document.getElementById('setupModalClose');
        if (closeBtn) {
            closeBtn.addEventListener('click', () => backdrop.remove());
        }
        const seedBtn = document.getElementById('setupSeedSampleModal');
        if (seedBtn) {
            seedBtn.addEventListener('click', async () => {
                seedBtn.disabled = true;
                seedBtn.textContent = 'Seeding...';
                try {
                    await fetch('/api/setup/seed', { method: 'POST' });
                    seedBtn.textContent = 'Seeded';
                } catch (err) {
                    seedBtn.textContent = 'Seed Failed';
                }
            });
        }
        const uploadBtn = document.getElementById('setupOpenUploadModal');
        if (uploadBtn) {
            uploadBtn.addEventListener('click', () => {
                window.location.href = '/dashboard/upload.html';
            });
        }
        const doneBtn = document.getElementById('setupDoneModal');
        if (doneBtn) {
            doneBtn.addEventListener('click', async () => {
                await fetch('/api/setup/initialize', { method: 'POST' });
                backdrop.remove();
            });
        }
    }

    function formatSetupStatus(status) {
        if (!status) {
            return 'No diagnostics available.';
        }
        const lines = [];
        lines.push(`Root: ${status.root}`);
        lines.push(`Mounted: ${status.root_mounted ? 'yes' : 'no'}`);
        lines.push(`Writable: ${status.root_writable ? 'yes' : 'no'}`);
        lines.push(`Content items: ${status.content_count}`);
        lines.push(`Source files: ${status.sources_count}`);
        lines.push(`Output dirs: ${status.outputs_count}`);
        if (status.issues && status.issues.length) {
            lines.push(`Issues: ${status.issues.join(', ')}`);
        }
        return lines.join('\n');
    }

    const PANEL_HELP = {
        dashboard: {
            title: 'Getting Started',
            purpose: 'Pick a workflow: compare players, compare encodings, or test errors.',
            steps: [
                'Use the sidebar to open Mosaic, Playback, Quartet, or Live Offset.',
                'Start with Mosaic to preview available streams quickly.'
            ]
        },
        grid: {
            title: 'Mosaic',
            purpose: 'Quickly preview all available streams.',
            steps: [
                'Select content, codec, and segment length to filter tiles.',
                'Left-click a tile to make it the currently selected stream.',
                'Right-click a tile to open testing tools.'
            ],
            needsContent: true
        },
        playback: {
            title: 'Playback',
            purpose: 'Deep-dive a single stream with player diagnostics.',
            steps: [
                'Choose content, codec, and segment length.'            ],
            needsContent: true
        },
        quartet: {
            title: 'Quartet',
            purpose: 'Compare player implementations side-by-side on the same stream.',
            steps: [
                'Pick content, protocol, codec, and segment length.',
                'Use tabs to switch between player, encoding, and variant views.'
            ],
            needsContent: true
        },
        'segment-duration': {
            title: 'Live Offset',
            purpose: 'See how segment duration affects live latency and stability.',
            steps: [
                'Choose content and codec.',
                'Compare LL, 2s, and 6s streams in parallel.'
            ],
            needsContent: true
        },
        testing: {
            title: 'Testing',
            purpose: 'Monitor ALL testing sessions and inject failures.',
            steps: [
                'Start a session and open a testing player.',
                'Adjust failure controls while the stream plays.'
            ],
            needsContent: true
        },
        'testing-session': {
            title: 'Testing Session',
            purpose: 'Experiment with streaming failures and watch how players react in real time.',
            steps: [
                'Use Retry Fetch, Restart Playback, and Reload Page to force immediate player actions.',
                'Select the player engine (Auto, HTML5, HLS.js, Shaka, Video.js) to compare behavior.',
                'Configure Segment/Playlist/Manifest failures (type, frequency, consecutive, and variants) to simulate errors.',
                'Adjust network shaping sliders (throughput, delay, loss) when supported to test bandwidth constraints.',                
                'Automatic throughput patterns for test ABR rampup/down/pyramid.',
                'Grouping of individual streaming session so they all share the same failure and network conditions.',
                'Watch the bandwidth chart to compare selected limits vs actual throughput over time.',
                'Use the right-click menu to open the stream in external test pages (e.g., HLS.js demo) for deeper logs.'
            ],
            needsContent: true
        },
        sources: {
            title: 'Source Library',
            purpose: 'Browse raw content and discover available assets.',
            steps: [
                'Search and filter to locate source clips.',
                'Re-encode any item to generate new variants.'
            ],
            needsContent: true
        },
        upload: {
            title: 'Upload Content',
            purpose: 'Add content to generate HLS/DASH test streams.',
            steps: [
                'Upload an MP4, then watch encoding jobs.',
                'Once complete, streams appear in Mosaic and Playback.'
            ]
        },
        jobs: {
            title: 'Encoding Jobs',
            purpose: 'Track encoding progress and troubleshoot failures.',
            steps: [
                'Open a job to see logs and outputs.',
                'Retry failed jobs after adjusting settings.'
            ]
        },
        monitor: {
            title: 'Monitor',
            purpose: 'Watch live generation status and health in real time.',
            steps: [
                'Check health and active streams.'            ]
        }
    };

    function isExpertMode() {
        if (isExpertUrl()) {
            return true;
        }
        return localStorage.getItem('ismExpertMode') === '1';
    }

    function attachExpertToggle() {
        const toggle = document.getElementById('expertModeToggle');
        if (!toggle) return;
        const forced = isExpertUrl();
        toggle.checked = forced || localStorage.getItem('ismExpertMode') === '1';
        toggle.disabled = forced;
        toggle.title = forced ? 'Disabled via ?expert=1 in the URL' : '';
        toggle.addEventListener('change', () => {
            localStorage.setItem('ismExpertMode', toggle.checked ? '1' : '0');
            renderPageHelp(getActivePage());
        });
    }

    function renderPageHelp(activePage) {
        const contentArea = document.querySelector('#ism-content');
        if (!contentArea) return;

        const help = PANEL_HELP[activePage];
        if (!help) return;

        const existing = document.getElementById('panelHelp');
        if (existing) {
            existing.remove();
        }

        const expertMode = isExpertMode();
        if (expertMode && activePage !== 'testing-session') {
            return;
        }

        const helpContainer = document.querySelector('.ism-content-standard, .ism-content-narrow, .ism-content-wide') || contentArea;

        const steps = help.steps
            ? help.steps.map((step) => `<li>${step}</li>`).join('')
            : '';

        const needsContentNote = help.needsContent
            ? `<div class="panel-help-note">If you don’t see any content, upload media first. <a href="/dashboard/upload.html">Go to Upload</a></div>`
            : '';

        const panel = document.createElement('div');
        panel.id = 'panelHelp';
        panel.className = 'panel-help';
        panel.innerHTML = `
            <div class="panel-help-title">Help</div>
            <label class="panel-help-expert">
                <input type="checkbox" id="panelHelpExpertToggle">
                Expert
            </label>
            <div class="panel-help-purpose">${help.purpose}</div>
            ${steps ? `<ul class="panel-help-steps">${steps}</ul>` : ''}
            ${needsContentNote}
        `;

        helpContainer.appendChild(panel);

        const toggle = document.getElementById('panelHelpExpertToggle');
        if (toggle) {
            toggle.checked = expertMode;
            toggle.addEventListener('change', () => {
                localStorage.setItem('ismExpertMode', toggle.checked ? '1' : '0');
                renderPageHelp(activePage);
            });
        }
    }

    // Public API
    window.BOSSNav = {
        init: initNavigation,
        showInfo: showInfo,
        toggleFullscreen: toggleFullscreen,
        viewActiveJob: viewActiveJob,
        dismissProgress: dismissProgress,
        updateSelectedContentBadge: updateSelectedContentBadge,
        setSelectedUrl: setSelectedUrl,
        normalizeTestingBaseUrl: normalizeTestingBaseUrl,
        buildTestingUrl: buildTestingUrl,
        createPlayerId: createPlayerId,
        isContentManagementRestricted: shouldRestrictContentManagement,
        isMonitorRestricted: shouldRestrictMonitorAccess
    };

    // Auto-initialize on DOM ready (unless disabled)
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', function() {
            if (!document.body.hasAttribute('data-ism-nav-manual')) {
                const options = {};
                
                // Check for page-specific options
                if (document.body.hasAttribute('data-ism-fullscreen')) {
                    options.fullscreen = true;
                }
                if (document.body.hasAttribute('data-ism-no-sidebar')) {
                    options.showSidebar = false;
                }
                if (document.body.hasAttribute('data-ism-no-header')) {
                    options.showHeader = false;
                }
                
                window.BOSSNav.init(options);
            }
        });
    } else {
        if (!document.body.hasAttribute('data-ism-nav-manual')) {
            window.BOSSNav.init();
        }
    }
})();
