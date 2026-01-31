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
            { id: 'testing', icon: '🧪', text: 'Testing', href: '/dashboard/testing.html' },
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

    // Get active page
    function getActivePage() {
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
        
        const sections = [
            { title: 'MAIN', items: NAVIGATION.main },
            { title: 'CONTENT', items: NAVIGATION.content },
            { title: 'PLAYBACK', items: NAVIGATION.testing },
            { title: 'LIVE STREAMING', items: NAVIGATION.live },
            { title: 'DEVELOPMENT', items: NAVIGATION.development }
        ];

        let html = '<div class="boss-sidebar" id="sidebar">';
        
        // Sidebar header
        html += '<div class="boss-sidebar-header">';
        html += '<div class="boss-logo">';
        html += '<span class="boss-logo-icon">🎬</span>';
        html += '<span>InfiniteStream</span>';
        html += '</div>';
        html += '</div>';
        
        // Sidebar content
        html += '<div class="boss-sidebar-content">';
        
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
                const isActive = item.id === activePage ? 'active' : '';
                const warning = item.warning ? '<span class="nav-item-warning">⚠️</span>' : '';
                const external = item.external ? ' target="_blank" rel="noopener"' : '';
                
                html += `<a id="nav-${item.id}" href="${item.href}" class="nav-item ${isActive}"${external}>`;
                html += `<span class="nav-item-icon">${item.icon}</span>`;
                html += `<span class="nav-item-text">${item.text}</span>`;
                html += warning;
                html += '</a>';
            });
            
            html += '</div>';
        });
        
        html += '</div>';
        
        // Sidebar footer
        html += '<div class="boss-sidebar-footer">';
        html += '<a href="#" class="nav-item" onclick="window.BOSSNav.showInfo(); return false;">';
        html += '<span class="nav-item-icon">ℹ️</span>';
        html += '<span class="nav-item-text">Server Info</span>';
        html += '</a>';
        html += '</div>';
        
        html += '</div>';
        
        return html;
    }

    // Build header HTML
    function buildHeader() {
        let html = '<div class="boss-header">';
        
        // Left side
        html += '<div class="boss-header-left">';
        html += '<div class="boss-header-title">InfiniteStream</div>';
        html += '</div>';
        
        // Center (reserved for future)
        html += '<div class="boss-header-center"></div>';
        
        // Right side
        html += '<div class="boss-header-right">';
        html += '<div class="boss-selected-content" id="bossSelectedContent" title="Selected content"></div>';
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
        appWrapper.className = 'boss-app';
        if (showSidebar) appWrapper.classList.add('has-sidebar');
        if (fullscreen) appWrapper.classList.add('boss-fullscreen');
        
        // Build navigation HTML
        let navHTML = '';
        if (showSidebar) navHTML += buildSidebar(activePage);
        
        // Main content wrapper
        navHTML += '<div class="boss-main">';
        if (showHeader) navHTML += buildHeader();
        navHTML += '<div class="boss-content" id="boss-content"></div>';
        navHTML += '</div>';
        
        appWrapper.innerHTML = navHTML;
        
        // Move existing body content into content area
        const contentArea = appWrapper.querySelector('#boss-content');
        while (document.body.firstChild) {
            contentArea.appendChild(document.body.firstChild);
        }
        
        // Add app wrapper to body
        document.body.appendChild(appWrapper);
        
        // Setup mobile menu
        setupMobileMenu();

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
            if (event.key && event.key.startsWith('bossSelected')) {
                updateSelectedContentBadge();
            }
        });
    }

    const GLOBAL_SELECTIONS = {
        codec: { key: 'bossSelectedCodec', ids: ['codecSelect', 'codecFilter'] },
        segment: { key: 'bossSelectedSegment', ids: ['segmentSelect', 'segmentFilter'] }
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

    function updateSelectedContentBadge() {
        const badge = document.getElementById('bossSelectedContent');
        const demoLink = document.getElementById('nav-hlsjs-demo');
        const shakaLink = document.getElementById('nav-shaka-demo');
        if (!badge) return;
        const full = localStorage.getItem('bossSelectedContentFull') || localStorage.getItem('bossSelectedContent');
        const base = localStorage.getItem('bossSelectedContentBase');
        const url = localStorage.getItem('bossSelectedUrl') || '';
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
            return;
        }

        badge.textContent = '';
        const labelSpan = document.createElement('span');
        labelSpan.className = 'boss-selected-label';
        labelSpan.textContent = label ? `Selected : ${label}` : 'Selected :';
        badge.appendChild(labelSpan);

        if (url) {
            const urlSpan = document.createElement('span');
            urlSpan.className = 'boss-selected-url';
            urlSpan.textContent = url;
            badge.appendChild(urlSpan);
            badge.title = `${label ? `Selected: ${label}\n` : ''}${url}`;
            if (demoLink || shakaLink) {
                let absoluteUrl;
                if (url.startsWith('http://') || url.startsWith('https://')) {
                    try {
                        const parsed = new URL(url);
                        if (parsed.hostname === window.location.hostname && (parsed.port === '' || parsed.port === '20081' || parsed.port === '30081')) {
                            const origin = new URL(window.location.origin);
                            parsed.protocol = origin.protocol;
                            parsed.port = origin.port;
                            absoluteUrl = parsed.toString();
                        } else {
                            absoluteUrl = url;
                        }
                    } catch {
                        absoluteUrl = url;
                    }
                } else {
                    const suffix = url.startsWith('/') ? url : `/${url}`;
                    absoluteUrl = `${window.location.origin}${suffix}`;
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
        }

        badge.classList.add('active');
    }

    let lastSelectedSignature = '';
    function startSelectedContentWatcher() {
        setInterval(() => {
            const full = localStorage.getItem('bossSelectedContentFull') || localStorage.getItem('bossSelectedContent') || '';
            const base = localStorage.getItem('bossSelectedContentBase') || '';
            const url = localStorage.getItem('bossSelectedUrl') || '';
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
            localStorage.setItem('bossSelectedProtocol', 'dash');
        } else if (lower.includes('.m3u8')) {
            localStorage.setItem('bossSelectedProtocol', 'hls');
        }
        if (lower.includes('manifest_2s.mpd') || lower.includes('master_2s.m3u8') || lower.includes('/2s/')) {
            localStorage.setItem('bossSelectedSegment', '2s');
        } else if (lower.includes('manifest_6s.mpd') || lower.includes('master_6s.m3u8') || lower.includes('/6s/')) {
            localStorage.setItem('bossSelectedSegment', '6s');
        } else if (lower.includes('.mpd') || lower.includes('master.m3u8')) {
            localStorage.setItem('bossSelectedSegment', 'll');
        }
        if (lower.includes('_av1')) {
            localStorage.setItem('bossSelectedCodec', 'av1');
        } else if (lower.includes('_hevc') || lower.includes('_h265')) {
            localStorage.setItem('bossSelectedCodec', 'hevc');
        } else if (lower.includes('_h264')) {
            localStorage.setItem('bossSelectedCodec', 'h264');
        }
    }

    function setSelectedUrl(url) {
        if (url === null || url === undefined) {
            localStorage.removeItem('bossSelectedUrl');
        } else if (String(url).trim().length) {
            localStorage.setItem('bossSelectedUrl', url);
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

    // Show server info modal
    function showInfo() {
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

Version: 2.0
        `.trim();
        
        alert(info);
    }

    // Toggle fullscreen mode
    function toggleFullscreen() {
        const app = document.querySelector('.boss-app');
        if (app) {
            app.classList.toggle('boss-fullscreen');
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

    // Public API
    window.BOSSNav = {
        init: initNavigation,
        showInfo: showInfo,
        toggleFullscreen: toggleFullscreen,
        viewActiveJob: viewActiveJob,
        dismissProgress: dismissProgress,
        updateSelectedContentBadge: updateSelectedContentBadge,
        setSelectedUrl: setSelectedUrl,
        createPlayerId: function() {
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
    };

    // Auto-initialize on DOM ready (unless disabled)
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', function() {
            if (!document.body.hasAttribute('data-boss-nav-manual')) {
                const options = {};
                
                // Check for page-specific options
                if (document.body.hasAttribute('data-boss-fullscreen')) {
                    options.fullscreen = true;
                }
                if (document.body.hasAttribute('data-boss-no-sidebar')) {
                    options.showSidebar = false;
                }
                if (document.body.hasAttribute('data-boss-no-header')) {
                    options.showHeader = false;
                }
                
                window.BOSSNav.init(options);
            }
        });
    } else {
        if (!document.body.hasAttribute('data-boss-nav-manual')) {
            window.BOSSNav.init();
        }
    }
})();
