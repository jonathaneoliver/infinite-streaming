// Browser-side play_id helper. Mirrors what the Apple (PlayerViewModel.swift)
// and Android (PlayerViewModel.kt) clients already do for issue #280:
// every `loadStream()` boundary mints a fresh UUID and stamps it as
// `?play_id=<uuid>` on every URL the player issues to go-proxy. This lets
// the analytics pipeline (HAR snapshots, ClickHouse session_snapshots,
// Grafana dashboard, Session Viewer picker) partition by playback episode
// rather than by go-proxy session.
//
// Each helper takes a `getPlayId` function (not a static value) so callers
// can roll the play_id without re-installing the interceptor — important
// for retry / restart paths that should look like a fresh playback.
(function () {
    'use strict';

    // Mint a fresh UUID. Uses crypto.randomUUID() where available
    // (Safari 15.4+, Chrome 92+, Firefox 95+); falls back to a manually
    // assembled v4-shaped string elsewhere.
    function mintPlayId() {
        if (window.crypto && typeof window.crypto.randomUUID === 'function') {
            return window.crypto.randomUUID();
        }
        const bytes = new Uint8Array(16);
        if (window.crypto && window.crypto.getRandomValues) {
            window.crypto.getRandomValues(bytes);
        } else {
            for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
        }
        bytes[6] = (bytes[6] & 0x0f) | 0x40;
        bytes[8] = (bytes[8] & 0x3f) | 0x80;
        const hex = Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
        return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
    }

    // Add or replace `play_id=<id>` on a URL string. Returns a new string.
    // Handles relative URLs by anchoring against location.origin.
    function applyPlayId(url, playId) {
        if (!url || !playId) return url;
        try {
            const u = new URL(url, window.location.origin);
            u.searchParams.set('play_id', playId);
            // Re-emit relative if the input was relative.
            if (!/^https?:\/\//i.test(url)) {
                return u.pathname + (u.search ? u.search : '') + u.hash;
            }
            return u.toString();
        } catch {
            // Not a parseable URL — fall back to a string append.
            const sep = url.includes('?') ? '&' : '?';
            const stripped = url.replace(/([?&])play_id=[^&]*(&|$)/, (_m, p1, p2) => p2 ? p1 : '');
            return stripped + sep + 'play_id=' + encodeURIComponent(playId);
        }
    }

    // HLS.js: hooks an `xhrSetup` callback into the config so every
    // request the loader issues (manifests, parts, segments, init
    // segments, key files) carries the current play_id.
    function installPlayIdHls(hlsConfig, getPlayId) {
        const prior = hlsConfig.xhrSetup;
        hlsConfig.xhrSetup = function (xhr, url) {
            const playId = typeof getPlayId === 'function' ? getPlayId() : getPlayId;
            const stamped = applyPlayId(url, playId);
            // hls.js has already opened the XHR by the time xhrSetup
            // fires, but it accepts the rewritten URL via xhr's
            // re-open machinery. Easiest: intercept xhr.open up front.
            if (stamped !== url) {
                const originalOpen = xhr.open;
                xhr.open = function (method, _u, ...rest) {
                    return originalOpen.call(xhr, method, stamped, ...rest);
                };
            }
            if (typeof prior === 'function') {
                try { prior.call(this, xhr, url); } catch (e) { console.warn(e); }
            }
        };
        return hlsConfig;
    }

    // Shaka: registers a request filter on the player's networking
    // engine so every outgoing request gets play_id appended. Filter
    // runs for manifests AND segments AND license requests.
    function installPlayIdShaka(player, getPlayId) {
        const eng = player && typeof player.getNetworkingEngine === 'function'
            ? player.getNetworkingEngine() : null;
        if (!eng) return;
        eng.registerRequestFilter((_type, request) => {
            const playId = typeof getPlayId === 'function' ? getPlayId() : getPlayId;
            if (!playId || !Array.isArray(request.uris)) return;
            request.uris = request.uris.map(u => applyPlayId(u, playId));
        });
    }

    // Video.js (videojs-http-streaming) exposes the underlying VHS via
    // `xhr-hooks`. We register a beforeRequest hook that rewrites the
    // outgoing URL on every request.
    function installPlayIdVideoJs(videojsLib, getPlayId) {
        if (!videojsLib || !videojsLib.Vhs || typeof videojsLib.Vhs.xhr !== 'function') return;
        videojsLib.Vhs.xhr.beforeRequest = function (options) {
            const playId = typeof getPlayId === 'function' ? getPlayId() : getPlayId;
            if (options && options.uri) options.uri = applyPlayId(options.uri, playId);
            else if (options && options.url) options.url = applyPlayId(options.url, playId);
            return options;
        };
    }

    // Native HTML5: there's no programmatic way to intercept the
    // browser's internal segment fetches once it has parsed the
    // manifest. Best we can do is stamp the master URL at video.src
    // time; play_id only flows on that single request. The analytics
    // pipeline still gets it (it's all the proxy needs to associate
    // the episode), but per-segment requests will be missing it.
    function applyPlayIdToNativeSrc(url, playId) {
        return applyPlayId(url, playId);
    }

    window.PlayId = {
        mint: mintPlayId,
        apply: applyPlayId,
        installHls: installPlayIdHls,
        installShaka: installPlayIdShaka,
        installVideoJs: installPlayIdVideoJs,
        applyNative: applyPlayIdToNativeSrc
    };
})();
