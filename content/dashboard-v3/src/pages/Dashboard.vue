<script setup lang="ts">
/**
 * Dashboard.vue — landing page. Mirrors the legacy dashboard.html:
 *   - Welcome heading
 *   - Quick-stat cards (sources / active jobs / live streams /
 *     content items) refreshed every 30s
 *   - Three "action" cards (Content Management / Playback /
 *     Live Streaming) with links to the rest of the dashboard
 *   - Recent Encoding Jobs + Recent Sources tables
 *   - API Reference card at the bottom
 *
 * Stats are fetched from the existing v1 endpoints; they remain the
 * source of truth for sources / jobs / content discovery and have
 * not been part of the v2 migration.
 */
import { onBeforeUnmount, onMounted, ref } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';

interface JobRow {
  job_id: string;
  name: string;
  status: string;
  created_at: string;
}
interface SourceRow {
  name: string;
  file_size: number;
  uploaded_at: string;
}

const sourcesCount = ref<number | null>(null);
const activeJobsCount = ref<number | null>(null);
const liveStreamsCount = ref<number | null>(null);
const contentCount = ref<number | null>(null);
const recentJobs = ref<JobRow[]>([]);
const recentSources = ref<SourceRow[]>([]);
const loadError = ref<string | null>(null);
let timer: number | null = null;

function statusClass(status: string): string {
  switch (status) {
    case 'queued': return 'badge warning';
    case 'encoding': return 'badge info';
    case 'complete': return 'badge success';
    case 'failed': return 'badge error';
    case 'cancelled': return 'badge neutral';
    default: return 'badge neutral';
  }
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(n) / Math.log(k));
  return Math.round((n / Math.pow(k, i)) * 100) / 100 + ' ' + sizes[i];
}

function formatDate(d: string): string {
  try { return new Date(d).toLocaleDateString(); } catch { return '—'; }
}

async function safeJson(url: string): Promise<any | null> {
  try {
    const r = await fetch(url);
    if (!r.ok) return null;
    return await r.json();
  } catch {
    return null;
  }
}

async function loadStats() {
  loadError.value = null;
  const [sourcesData, jobsData, statusData, contentData] = await Promise.all([
    safeJson('/api/sources'),
    safeJson('/api/jobs'),
    safeJson('/go-live/api/status'),
    safeJson('/api/content'),
  ]);

  const sources = sourcesData?.sources ?? [];
  sourcesCount.value = sources.length;
  recentSources.value = sources.slice(0, 5);

  const jobs: JobRow[] = jobsData?.jobs ?? [];
  activeJobsCount.value = jobs.filter((j) => j.status === 'queued' || j.status === 'encoding').length;
  recentJobs.value = jobs.slice(0, 5);

  if (Array.isArray(statusData?.active_streams)) {
    liveStreamsCount.value = statusData.active_streams.length;
  } else if (typeof statusData?.active_processes === 'number') {
    liveStreamsCount.value = statusData.active_processes;
  } else {
    liveStreamsCount.value = null;
  }

  if (Array.isArray(contentData)) {
    const items: any[] = contentData;
    const unique = new Set(
      items.map((it) => String(it.name ?? '').replace(/_h264|_hevc|_av1|_ts|_hw|_dash/g, '')),
    );
    contentCount.value = unique.size;
  } else {
    contentCount.value = null;
  }

  if (
    sourcesCount.value === 0 &&
    activeJobsCount.value === 0 &&
    liveStreamsCount.value === null &&
    contentCount.value === null
  ) {
    loadError.value = 'One or more backend endpoints did not respond. Showing what loaded.';
  }
}

onMounted(() => {
  loadStats();
  timer = window.setInterval(loadStats, 30_000);
});
onBeforeUnmount(() => {
  if (timer != null) clearInterval(timer);
});

function fmtCount(n: number | null): string {
  return n == null ? '—' : String(n);
}
</script>

<template>
  <ShellLayout active-page="dashboard">
    <div class="page">
      <section class="welcome">
        <h1>Welcome to InfiniteStream</h1>
        <p>Media testing platform for HLS, DASH, and live streaming.</p>
      </section>

      <div v-if="loadError" class="alert">
        {{ loadError }}
      </div>

      <section class="stats">
        <a class="stat" href="/dashboard/sources.html">
          <div class="num primary">{{ fmtCount(sourcesCount) }}</div>
          <div class="label">Sources</div>
        </a>
        <a class="stat" href="/dashboard/jobs.html">
          <div class="num warning">{{ fmtCount(activeJobsCount) }}</div>
          <div class="label">Active Jobs</div>
        </a>
        <a class="stat" href="/dashboard/go-monitor.html">
          <div class="num success">{{ fmtCount(liveStreamsCount) }}</div>
          <div class="label">Live Streams</div>
        </a>
        <div class="stat static">
          <div class="num muted">{{ fmtCount(contentCount) }}</div>
          <div class="label">Content Items</div>
        </div>
      </section>

      <section class="actions">
        <article class="action-card">
          <header>
            <div class="title">📤 Content Management</div>
            <div class="subtitle">Upload and encode video content</div>
          </header>
          <p>
            Upload MP4 files and transcode them into multi-bitrate ABR streaming packages
            with H.264 and HEVC codecs.
          </p>
          <div class="links">
            <a class="btn primary" href="/dashboard/upload.html">Upload New Content</a>
            <a class="btn secondary" href="/dashboard/sources.html">Browse Source Library</a>
            <a class="btn secondary" href="/dashboard/jobs.html">View Encoding Jobs</a>
          </div>
        </article>

        <article class="action-card">
          <header>
            <div class="title">🎮 Playback</div>
            <div class="subtitle">Player testing and debugging</div>
          </header>
          <p>
            Test your streams with multiple players, debug playback issues, and compare
            player behaviour across devices.
          </p>
          <div class="links">
            <a class="btn primary" href="/dashboard/testing.html">Testing Monitor →</a>
            <a class="btn secondary" href="/dashboard/testing-session.html?nav=1">Testing Playback →</a>
            <a class="btn secondary" href="/dashboard/playback.html">Playback</a>
            <a class="btn secondary" href="/dashboard/quartet.html">Quartet Comparison</a>
            <a class="btn secondary" href="/dashboard/grid.html">Mosaic</a>
            <a class="btn secondary" href="/dashboard/mosaic-10ft.html">10ft UI</a>
            <a class="btn secondary" href="/dashboard/segment-duration-comparison.html">Live Offset (2s/4s/6s)</a>
          </div>
        </article>

        <article class="action-card">
          <header>
            <div class="title">🔴 Live Streaming</div>
            <div class="subtitle">Live stream simulation &amp; testing</div>
          </header>
          <p>
            Simulate live streaming scenarios, test low-latency playback, and monitor
            stream health.
          </p>
          <div class="links">
            <a class="btn primary" href="/dashboard/go-monitor.html">Stream Monitor</a>
            <a class="btn secondary" href="/dashboard/sessions.html">Archived Sessions</a>
          </div>
        </article>
      </section>

      <section class="recent">
        <article class="card">
          <header class="card-head">
            <div class="card-title">Recent Encoding Jobs</div>
            <a class="card-link" href="/dashboard/jobs.html">View All</a>
          </header>
          <div v-if="!recentJobs.length" class="empty">No encoding jobs yet</div>
          <table v-else class="table">
            <tbody>
              <tr v-for="j in recentJobs" :key="j.job_id">
                <td>
                  <a :href="`/dashboard/job-detail.html?id=${j.job_id}`" class="link">{{ j.name }}</a>
                </td>
                <td><span :class="statusClass(j.status)">{{ j.status }}</span></td>
                <td class="muted right">{{ formatDate(j.created_at) }}</td>
              </tr>
            </tbody>
          </table>
        </article>

        <article class="card">
          <header class="card-head">
            <div class="card-title">Recent Sources</div>
            <a class="card-link" href="/dashboard/sources.html">View All</a>
          </header>
          <div v-if="!recentSources.length" class="empty">No sources uploaded yet</div>
          <table v-else class="table">
            <tbody>
              <tr v-for="s in recentSources" :key="s.name">
                <td><a class="link" href="/dashboard/sources.html">{{ s.name }}</a></td>
                <td class="muted">{{ formatBytes(s.file_size) }}</td>
                <td class="muted right">{{ formatDate(s.uploaded_at) }}</td>
              </tr>
            </tbody>
          </table>
        </article>
      </section>

      <section class="api-ref card">
        <div class="api-meta">
          <div class="title">📖 API Reference</div>
          <p>
            Live OpenAPI docs for the go-proxy and forwarder HTTP surfaces. v1 is
            generated from the running server; v2 is the player/play model under
            design.
          </p>
        </div>
        <div class="api-links">
          <a class="btn primary" href="/dashboard/api-docs/">All Specs</a>
          <div class="group">
            <span class="group-label">v1</span>
            <a class="btn secondary" href="/dashboard/api-docs/proxy.html">go-proxy</a>
            <a class="btn secondary" href="/dashboard/api-docs/forwarder.html">forwarder</a>
          </div>
          <div class="group">
            <span class="group-label">v2 (draft)</span>
            <a class="btn secondary" href="/dashboard/api-docs/proxy-v2.html">go-proxy</a>
            <a class="btn secondary" href="/dashboard/api-docs/forwarder-v2.html">forwarder</a>
          </div>
        </div>
      </section>
    </div>
  </ShellLayout>
</template>

<style scoped>
.page {
  padding: 24px;
  max-width: 1200px;
  margin: 0 auto;
}

.welcome { margin-bottom: 24px; }
.welcome h1 {
  font-size: 28px;
  font-weight: 400;
  color: #202124;
  margin: 0 0 6px 0;
}
.welcome p {
  font-size: 14px;
  color: #5f6368;
  margin: 0;
}

.alert {
  background: #fef7e0;
  border: 1px solid #f9ab00;
  color: #b06000;
  padding: 10px 14px;
  border-radius: 6px;
  font-size: 13px;
  margin-bottom: 16px;
}

.stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
  margin-bottom: 24px;
}
.stat {
  background: #fff;
  border: 1px solid #e8eaed;
  border-radius: 8px;
  padding: 16px;
  text-align: center;
  text-decoration: none;
  color: inherit;
  transition: transform 0.15s, box-shadow 0.15s;
}
.stat:hover:not(.static) {
  transform: translateY(-2px);
  box-shadow: 0 4px 12px rgba(0,0,0,0.08);
}
.stat .num {
  font-size: 30px;
  font-weight: 400;
  margin-bottom: 4px;
  font-variant-numeric: tabular-nums;
}
.stat .num.primary { color: #1a73e8; }
.stat .num.warning { color: #f9ab00; }
.stat .num.success { color: #1e8e3e; }
.stat .num.muted   { color: #5f6368; }
.stat .label {
  font-size: 12px;
  color: #5f6368;
}

.actions {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 16px;
  margin-bottom: 24px;
}
.action-card {
  background: #fff;
  border: 1px solid #e8eaed;
  border-radius: 8px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.action-card header .title {
  font-size: 16px;
  font-weight: 600;
}
.action-card header .subtitle {
  font-size: 12px;
  color: #5f6368;
  margin-top: 2px;
}
.action-card p {
  color: #5f6368;
  margin: 0;
  font-size: 13px;
}
.links {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.btn {
  display: inline-block;
  padding: 8px 14px;
  border-radius: 6px;
  font-size: 13px;
  font-weight: 500;
  text-decoration: none;
  text-align: center;
  border: 1px solid transparent;
  cursor: pointer;
  transition: background 0.1s;
}
.btn.primary {
  background: #1a73e8;
  color: white;
}
.btn.primary:hover { background: #1765cc; }
.btn.secondary {
  background: #fff;
  color: #1a73e8;
  border-color: #dadce0;
}
.btn.secondary:hover { background: #f1f3f4; }

.recent {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 16px;
  margin-bottom: 24px;
}
.card {
  background: #fff;
  border: 1px solid #e8eaed;
  border-radius: 8px;
  overflow: hidden;
}
.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 16px;
  border-bottom: 1px solid #e8eaed;
}
.card-title {
  font-size: 14px;
  font-weight: 600;
}
.card-link {
  font-size: 12px;
  color: #1a73e8;
  text-decoration: none;
}
.card-link:hover { text-decoration: underline; }

.empty {
  text-align: center;
  padding: 30px;
  color: #9aa0a6;
  font-size: 13px;
}

.table {
  width: 100%;
  border-collapse: collapse;
}
.table td {
  padding: 8px 16px;
  font-size: 13px;
  border-bottom: 1px solid #f1f3f4;
}
.table tr:last-child td { border-bottom: none; }
.table .right { text-align: right; }
.table .muted { color: #5f6368; font-size: 12px; }
.link { color: #1a73e8; text-decoration: none; }
.link:hover { text-decoration: underline; }

.badge {
  display: inline-block;
  font-size: 11px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 10px;
  text-transform: capitalize;
}
.badge.warning { background: #fef7e0; color: #b06000; }
.badge.info    { background: #e8f0fe; color: #1a73e8; }
.badge.success { background: #e6f4ea; color: #1e8e3e; }
.badge.error   { background: #fce8e6; color: #d93025; }
.badge.neutral { background: #f1f3f4; color: #5f6368; }

.api-ref {
  display: flex;
  align-items: center;
  gap: 24px;
  flex-wrap: wrap;
  padding: 20px;
}
.api-meta { flex: 1; min-width: 240px; }
.api-meta .title { font-weight: 600; margin-bottom: 6px; }
.api-meta p { font-size: 13px; color: #5f6368; margin: 0; }
.api-links {
  display: flex;
  gap: 16px;
  flex-wrap: wrap;
  align-items: center;
}
.group { display: flex; gap: 6px; flex-wrap: wrap; align-items: center; }
.group-label {
  font-size: 12px;
  color: #5f6368;
  margin-right: 2px;
}
</style>
