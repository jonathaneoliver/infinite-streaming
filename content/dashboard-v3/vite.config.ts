import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';
import { resolve } from 'node:path';

// Build outputs to ../dashboard/v3 so the existing nginx static-file route
// (/dashboard/ → content/dashboard/) serves the Vue bundle without any
// route changes. Each page is its own multi-page HTML entry so we can flip
// nginx route-by-route (testing.html / testing-session.html / dashboard.html)
// rather than all-or-nothing.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': resolve(__dirname, 'src') }
  },
  base: '/dashboard/v3/',
  build: {
    // Default: write to the repo's content/dashboard/v3/ so the existing
    // nginx /dashboard/ alias serves the bundle. Override with
    // VITE_BUILD_OUTDIR for Docker builds (where the path layout differs).
    outDir: process.env.VITE_BUILD_OUTDIR
      ? resolve(process.env.VITE_BUILD_OUTDIR)
      : resolve(__dirname, '../dashboard/v3'),
    emptyOutDir: true,
    rollupOptions: {
      input: {
        hello: resolve(__dirname, 'hello.html'),
        // Stage 2 — testing-session.html v3 entry.
        'testing-session': resolve(__dirname, 'testing-session.html'),
        // Stage 3 — testing.html v3 entry (multi-session monitor).
        testing: resolve(__dirname, 'testing.html'),
        // Stage 4 — dashboard.html v3 landing.
        dashboard: resolve(__dirname, 'dashboard.html'),
        // Stage 5 — v3-native Mosaic. Lives at /dashboard/v3/grid.html
        // and right-clicking a tile deep-links into v3 testing-session
        // (the legacy /dashboard/grid.html still works and opens the
        // legacy testing-session.html; this is the parallel path).
        grid: resolve(__dirname, 'grid.html'),
        // Stage 6 — v3-native archive replay page. Parallel to legacy
        // /dashboard/session-viewer.html (which uses session-shell.js +
        // session-replay.js); this build reuses the live page's
        // composables in replay mode driven by archived snapshots.
        'session-viewer': resolve(__dirname, 'session-viewer.html'),
        // Stage 6 (cont.) — archived-sessions picker. The session-viewer
        // entry above expects a deep-link with session_id; this is the
        // browse page that lists every play.
        sessions: resolve(__dirname, 'sessions.html'),
        // Stage 7 — characterization-run summary page. Pre-filters
        // /api/v2/plays on info=test_* labels and groups by run_id.
        characterization: resolve(__dirname, 'characterization.html'),
        // #772 — automated fault-sweep queue monitor.
        sweep: resolve(__dirname, 'sweep.html'),
        // Stage 8 (#497) — AI chat fleet-mode entry. Lives at
        // /dashboard/v3/ask.html. The side-panel variant of the same
        // chat is also mounted on other v3 pages.
        ask: resolve(__dirname, 'ask.html')
      }
    }
  },
  server: {
    port: 5173,
    proxy: (() => {
      const BACKEND = process.env.BACKEND_URL || 'http://jonathanoliver-ubuntu.local:21000';
      const SHAPER = process.env.SHAPER_URL
        || BACKEND.replace(/:(\d+)$/, (_, p) => ':' + (Number(p) + 81));
      return {
        // Dev server: proxy API + SSE through to whichever backend has the
        // player you're testing against. Override per-session with
        // BACKEND_URL=... npm run dev. Examples:
        //   BACKEND_URL=http://localhost:30000              (local Docker Compose)
        //   BACKEND_URL=http://lenovo.local:30000           (k3d release)
        //   BACKEND_URL=http://lenovo.local:40000           (k3d dev)
        //   BACKEND_URL=http://jonathanoliver-ubuntu.local:21000  (test-dev — default)
        '/api': {
          target: BACKEND,
          changeOrigin: true,
          configure: (proxy: any) => {
            proxy.on('proxyReq', (proxyReq: any) => {
              proxyReq.setHeader('X-Accel-Buffering', 'no');
            });
          },
        },
        // `/go-live/*` (HLS / DASH manifests + segments) is served by
        // nginx on the BACKEND port — it proxies internally to the
        // go-live binary on :8010. The mosaic / playback / grid pages
        // request these as relative `/go-live/...` URLs, so route them
        // through the same backend (NOT the per-session shaper port,
        // which only serves shaped per-session streams). The per-
        // session video player on testing-session.html uses absolute
        // URLs from `current_play.manifest.master_url` — those bypass
        // this Vite proxy entirely.
        '/go-live': { target: BACKEND, changeOrigin: true },
        // Legacy dashboard pages + their shared assets. Vite owns
        // `/dashboard/v3/*` (the Vue rewrite) directly; everything else
        // under `/dashboard/` gets proxied to the deployed backend so the
        // sidebar nav (Upload / Sources / Jobs / Mosaic / Playback /
        // Sessions / Quartet / Live Offset / Monitor) is clickable from
        // the dev server.
        '/dashboard': {
          target: BACKEND,
          changeOrigin: true,
          // `bypass` returns the URL to serve locally (any string), or
          // null/undefined to actually proxy. Returning `req.url` tells
          // Vite "serve this path from your own pipeline" — i.e. let the
          // Vite dev server handle the v3 HTML + module graph instead of
          // round-tripping to nginx.
          bypass: (req: any) => {
            if (req.url && req.url.startsWith('/dashboard/v3')) return req.url;
            return null;
          },
        },
        // Assets the legacy pages pull from the document root.
        '/shared-styles.css': { target: BACKEND, changeOrigin: true },
        '/shared':            { target: BACKEND, changeOrigin: true },
        '/testing':           { target: BACKEND, changeOrigin: true },
        // Optional sidecar surfaces that the legacy nav links to.
        '/analytics':         { target: BACKEND, changeOrigin: true },
        '/grafana':           { target: BACKEND, changeOrigin: true },
      };
    })(),
  }
});
