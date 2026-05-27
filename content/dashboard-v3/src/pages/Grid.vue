<script setup lang="ts">
/**
 * Grid.vue — v3-native Mosaic. Mirrors the legacy /dashboard/grid.html:
 *
 *   - Fetches the full content catalogue from /api/content.
 *   - Filter dropdowns: Protocol (HLS/DASH/Both/All) · Codec (H264/HEVC/
 *     AV1/All) · Segment (2s/6s/LL/All) · Max Res · Content name.
 *   - Picks up to 16 entries (4×4) and renders one GridTile per cell.
 *     The same content may appear twice if both HLS and DASH protocols
 *     are available — matches the legacy expansion logic.
 *   - Random-shuffle button reorders the candidate list.
 *   - Single audio focus: clicking any tile un-mutes that one and mutes
 *     the others. Right-click → opens v3 testing-session in a new tab.
 *
 * State persists in URL params so a refresh keeps the same view:
 *   ?protocol=hls&codec=h264&segs=6&maxRes=1080&content=bucks_bunny&random=1
 */
import { computed, onMounted, ref, watch } from 'vue';
import { useUrlSearchParams } from '@vueuse/core';
import ShellLayout from '@/components/ShellLayout.vue';
import GridTile from '@/components/GridTile.vue';

interface ApiContent {
  name: string;
  has_dash?: boolean;
  has_hls?: boolean;
  max_resolution?: string | number | null;
  max_height?: number | null;
}

interface PreparedItem extends ApiContent {
  label: string;
  protocol: 'hls' | 'dash';
}

const params = useUrlSearchParams<{
  protocol?: string;
  codec?: string;
  segs?: string;
  maxRes?: string;
  content?: string;
  random?: string;
  cols?: string;
  rows?: string;
  developer?: string;
}>('history');

const developerMode = computed(() => params.developer === '1');

/* ─── Context menu (legacy /dashboard/grid.html parity) ──────────────
 * Right-clicking a tile shows the same 3-option menu as legacy:
 *   - Open in Testing Window  →  /dashboard/testing-session.html
 *   - Copy Testing URL        →  clipboard
 *   - Open in HLS.js Demo     →  hlsjs.video-dev.org (developer mode + HLS only)
 *
 * Each action mints a FRESH 8-char hex player_id (same convention as
 * legacy createPlayerId) so successive right-clicks on the same tile
 * spawn distinct testing sessions.
 */
const menu = ref({
  open: false,
  x: 0,
  y: 0,
  url: '',
  isDash: false,
});

function mintPlayerId(): string {
  // Mint a canonical v4 UUID so the player_id is already in the v2
  // form everywhere (URL, v1 shaper register, /api/v2/players, archive
  // lookup). Previously this returned an 8-char hex short form, which
  // the v2 layer transparently v5-derived to a UUID on read — but the
  // forwarder then stored the raw short form and the v3 archive
  // queries (built from /api/v2/players.id) missed every row.
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback for ancient environments without crypto.randomUUID:
  // hand-assemble a UUIDv4 with proper version/variant bits.
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40; // version 4
  bytes[8] = (bytes[8] & 0x3f) | 0x80; // variant
  const hex = Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** Rewrite the tile URL to the shaper port (last 3 digits → `081`) and
 *  REPLACE any existing `player_id` query with the fresh one. Critical:
 *  the tile URL already carries its own `player_id=<tileId>` (for the
 *  grid's own playback context), but the testing-session needs to use
 *  the fresh id so the shaper registers a session under THAT id — not
 *  the tile's — and the v2 GET on the page can find it. Appending
 *  (legacy bug we hit on 2026-05-12) leaves both params in the URL and
 *  the shaper picks the first match. */
function buildTestingUrl(url: string, playerId: string): string {
  let absolute: URL;
  try {
    absolute = new URL(url, window.location.href);
  } catch {
    return url;
  }
  const currentPort =
    window.location.port || (window.location.protocol === 'https:' ? '443' : '80');
  if (currentPort !== '5173' && currentPort.length >= 4) {
    absolute.hostname = window.location.hostname;
    absolute.port = currentPort.slice(0, -3) + '081';
    // Inherit the page's protocol — go-proxy now serves TLS on the
    // shaper ports so the dashboard's HTTPS page can embed playback
    // without mixed-content blocking. `window.location.protocol`
    // includes the trailing colon, exactly what URL.protocol expects.
    absolute.protocol = window.location.protocol;
  }
  absolute.searchParams.set('player_id', playerId);
  return absolute.toString();
}

function onTileContextMenu(payload: { x: number; y: number; url: string; isDash: boolean }) {
  menu.value = { open: true, x: payload.x, y: payload.y, url: payload.url, isDash: payload.isDash };
}

function closeMenu() { menu.value.open = false; }

function menuOpenTesting() {
  const playerId = mintPlayerId();
  const testingUrl = buildTestingUrl(menu.value.url, playerId);
  const pageUrl = `/dashboard/testing-session.html?player_id=${encodeURIComponent(playerId)}&url=${encodeURIComponent(testingUrl)}`;
  window.open(pageUrl, '_blank', 'noopener');
  closeMenu();
}

async function menuCopyTesting() {
  const playerId = mintPlayerId();
  const testingUrl = buildTestingUrl(menu.value.url, playerId);
  try {
    await navigator.clipboard.writeText(testingUrl);
  } catch {
    // Fallback for non-secure contexts / older browsers.
    const inp = document.createElement('input');
    inp.value = testingUrl;
    document.body.appendChild(inp);
    inp.select();
    document.execCommand('copy');
    document.body.removeChild(inp);
  }
  closeMenu();
}

function menuOpenHlsjsDemo() {
  if (menu.value.isDash) return;
  const playerId = mintPlayerId();
  const testingUrl = buildTestingUrl(menu.value.url, playerId);
  const demoUrl = `https://hlsjs.video-dev.org/demo/?src=${encodeURIComponent(testingUrl)}`;
  window.open(demoUrl, '_blank', 'noopener');
  closeMenu();
}

// Close menu on any outside click — matches legacy `document.addEventListener('click', …)`.
onMounted(() => {
  document.addEventListener('click', closeMenu);
  document.addEventListener('contextmenu', (e) => {
    // Only auto-close on right-clicks OUTSIDE a tile (so the next tile's
    // right-click reopens correctly without us closing the new menu).
    if (!(e.target as HTMLElement).closest('.tile')) closeMenu();
  });
});

const DEFAULT_COLS = 4;
const DEFAULT_ROWS = 4;

function clampInt(v: string | undefined, dflt: number, min: number, max: number): number {
  const n = Number(v);
  if (!Number.isFinite(n) || n < min || n > max) return dflt;
  return Math.floor(n);
}

const cols = computed(() => clampInt(params.cols, DEFAULT_COLS, 1, 8));
const rows = computed(() => clampInt(params.rows, DEFAULT_ROWS, 1, 6));
const total = computed(() => cols.value * rows.value);
const gridStyle = computed(() => ({ gridTemplateColumns: `repeat(${cols.value}, 1fr)` }));

// Default filters target the most common test combo (HLS + H264 + 6 s
// segments) so a fresh page load lands on a useful slice without the
// operator picking from "all" each time. URL params still win — drop
// `?protocol=…&codec=…&segs=…` to override.
const protocol = computed(() => params.protocol || 'hls'); // hls | dash | both | all
const codec    = computed(() => params.codec    || 'h264'); // h264 | hevc | av1 | all
const segs     = computed<'2' | '6' | 'll' | 'all'>(() => {
  const v = params.segs ?? '6';
  if (v === '2' || v === '6' || v === 'll') return v;
  return 'all';
});
const maxResFilter = computed(() => params.maxRes || 'all');  // 360 | 540 | 720 | 1080 | 2160 | all
const contentFilter = computed(() => params.content || 'all');
const random = computed(() => params.random === '1');

const allContent = ref<ApiContent[]>([]);
const fetchError = ref<string | null>(null);

async function fetchContent() {
  try {
    const r = await fetch('/api/content');
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const data = (await r.json()) as ApiContent[];
    allContent.value = Array.isArray(data) ? data : [];
    fetchError.value = null;
  } catch (err: any) {
    fetchError.value = err?.message || String(err);
  }
}

onMounted(fetchContent);

function friendlyLabel(name: string): string {
  // Strip noisy timestamp + codec suffix for the on-tile label.
  return name
    .replace(/_(h264|hevc|av1|ts|hw|dash)/gi, '')
    .replace(/_\d{8}_\d{6}/i, '')
    .replace(/_p\d+/i, '')
    .replace(/_/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

function distinctNames(): string[] {
  const seen = new Set<string>();
  for (const c of allContent.value) {
    const stripped = c.name.replace(/_h264|_hevc|_av1|_ts|_hw|_dash/g, '');
    seen.add(stripped);
  }
  return Array.from(seen).sort();
}

const contentNameOptions = computed(() => distinctNames());

/** Apply the filter dropdowns to allContent → list of {name, protocol}
 *  candidates. Mirrors legacy applyFilters() + expansion-by-protocol. */
const candidates = computed<PreparedItem[]>(() => {
  const filtered = allContent.value.filter((item) => {
    if (contentFilter.value !== 'all' && !item.name.includes(contentFilter.value)) return false;
    if (codec.value !== 'all' && !item.name.toLowerCase().includes(codec.value)) return false;
    if (protocol.value === 'dash' && !item.has_dash) return false;
    if (protocol.value === 'hls' && !item.has_hls) return false;
    if (protocol.value === 'both' && (!item.has_dash || !item.has_hls)) return false;
    if (maxResFilter.value !== 'all') {
      const max = item.max_height ?? Number(item.max_resolution ?? 0);
      if (!max || max < Number(maxResFilter.value)) return false;
    }
    return true;
  });

  // Expand: emit one entry per (content × protocol) combination available.
  const expanded: PreparedItem[] = [];
  for (const item of filtered) {
    if (item.has_dash && (protocol.value === 'all' || protocol.value === 'dash' || protocol.value === 'both')) {
      expanded.push({ ...item, protocol: 'dash', label: friendlyLabel(item.name) });
    }
    if (item.has_hls && (protocol.value === 'all' || protocol.value === 'hls' || protocol.value === 'both')) {
      expanded.push({ ...item, protocol: 'hls', label: friendlyLabel(item.name) });
    }
  }

  // Sort alphabetically; DASH before HLS at the same name.
  expanded.sort((a, b) => {
    const n = a.name.localeCompare(b.name);
    if (n) return n;
    return a.protocol.localeCompare(b.protocol);
  });

  return expanded;
});

const shuffleSeed = ref(0);
const visible = computed<PreparedItem[]>(() => {
  let list = candidates.value;
  if (random.value) {
    // Stable shuffle using shuffleSeed so changing filters doesn't reshuffle.
    const seeded = list.map((item, i) => ({ item, k: hashSeed(item.name + item.protocol + shuffleSeed.value + i) }));
    seeded.sort((a, b) => a.k - b.k);
    list = seeded.map((s) => s.item);
  }
  const out = list.slice(0, total.value);
  console.log('[Grid] visible computed', { count: out.length, allContent: allContent.value.length, candidates: candidates.value.length, items: out.map((i) => `${i.name}/${i.protocol}`) });
  return out;
});

function hashSeed(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

function randomize() {
  shuffleSeed.value = Date.now();
  params.random = '1';
}

function setParam<K extends keyof typeof params>(key: K, value: string) {
  params[key] = value as any;
}

// Available segment durations a content slug actually provides.
// Without a manifest probe we just expose all four — most content has
// 2s / 6s / LL. The legacy was equally optimistic.
const SEGMENT_CHOICES: { v: '2' | '6' | 'll' | 'all'; label: string }[] = [
  { v: 'all', label: 'All' },
  { v: '2', label: '2s' },
  { v: '6', label: '6s' },
  { v: 'll', label: 'LL' },
];

const PROTOCOL_CHOICES = [
  { v: 'all',  label: 'All' },
  { v: 'hls',  label: 'HLS only' },
  { v: 'dash', label: 'DASH only' },
  { v: 'both', label: 'Both (HLS+DASH)' },
];

const CODEC_CHOICES = [
  { v: 'all',  label: 'All' },
  { v: 'h264', label: 'H264' },
  { v: 'hevc', label: 'HEVC' },
  { v: 'av1',  label: 'AV1' },
];

const MAX_RES_CHOICES = [
  { v: 'all',  label: 'Any res' },
  { v: '2160', label: '4K (≥2160p)' },
  { v: '1080', label: '≥1080p' },
  { v: '720',  label: '≥720p' },
  { v: '540',  label: '≥540p' },
];

// Effective per-tile segs token. When the filter is 'all' we still
// need one to pick a manifest path; default to 6s (legacy default).
const effectiveSegs = computed<'2' | '6' | 'll'>(() => {
  return segs.value === 'all' ? '6' : segs.value;
});

// Audio focus — exactly one tile may be unmuted at a time.
const focusIndex = ref<number | null>(null);

/* ─── Persisted state (legacy parity) ──────────────────────────────
 *  The legacy grid uses these localStorage keys to share "the URL I
 *  was last watching" with testing-session.html and dashboard.html.
 *  v3 writes the same keys so cross-page handoff keeps working — and
 *  also remembers the focused content name on reload. */
const LS_SELECTED_URL = 'ismSelectedUrl';
const LS_SELECTED_CONTENT = 'ismSelectedContent';
const LS_SELECTED_CONTENT_BASE = 'ismSelectedContentBase';
const LS_AUDIO_FOCUS_CONTENT = 'ismAudioFocusContent';
const LS_AUDIO_MUTED = 'ismAudioMuted';
const LS_KNOWN_4K_PREFIX = 'ismKnown4k:';

function stripContentSuffix(name: string): string {
  // Strip the trailing "_pNNN_codec_YYYYMMDD_HHMMSS" timestamp so the
  // base name (just the human-readable title) can survive re-encodes.
  return name.replace(/_p\d+_[a-z0-9]+_\d{8}_\d{6}.*$/i, '');
}

function persistFocus(item: typeof visible.value[number] | null) {
  if (!item) {
    try { localStorage.setItem(LS_AUDIO_MUTED, 'true'); } catch { /* ignore */ }
    return;
  }
  try {
    localStorage.setItem(LS_AUDIO_MUTED, 'false');
    localStorage.setItem(LS_AUDIO_FOCUS_CONTENT, item.name);
    localStorage.setItem(LS_SELECTED_CONTENT, item.name);
    localStorage.setItem(LS_SELECTED_CONTENT_BASE, stripContentSuffix(item.name));
    const segs = effectiveSegs.value;
    const ext = item.protocol === 'dash' ? 'mpd' : 'm3u8';
    const base = item.protocol === 'dash' ? 'manifest' : 'master';
    const url = segs === 'll'
      ? `/go-live/${item.name}/${base}.${ext}`
      : `/go-live/${item.name}/${base}_${segs}s.${ext}`;
    localStorage.setItem(LS_SELECTED_URL, url);
    // Remember 4K status keyed by content name — other pages use this
    // when the API doesn't surface max_height yet.
    if ((item.max_height ?? 0) >= 2160) {
      localStorage.setItem(LS_KNOWN_4K_PREFIX + item.name, 'true');
    }
  } catch {
    /* localStorage can throw in private-mode / quota-exceeded; ignore */
  }
}

function setFocus(i: number) {
  focusIndex.value = focusIndex.value === i ? null : i;
  if (focusIndex.value == null) {
    persistFocus(null);
  } else {
    persistFocus(visible.value[focusIndex.value]);
  }
}

watch(visible, () => {
  // If the visible set changes (filter / shuffle) and the focused tile
  // dropped out, clear the focus so nothing is left blaring.
  if (focusIndex.value != null && focusIndex.value >= visible.value.length) {
    focusIndex.value = null;
  }
});

// Restore "which tile had audio" on load, if that content is still in
// view. Matches the legacy AUDIO_MUTED_KEY + ismAudioFocusContent
// behavior.
onMounted(() => {
  try {
    if (localStorage.getItem(LS_AUDIO_MUTED) === 'false') {
      const wantName = localStorage.getItem(LS_AUDIO_FOCUS_CONTENT);
      if (wantName) {
        // Wait one tick for `visible` to populate from fetchContent.
        const stop = watch(
          visible,
          (list) => {
            if (!list.length) return;
            const idx = list.findIndex((it) => it.name === wantName);
            if (idx >= 0) focusIndex.value = idx;
            stop();
          },
          { immediate: true },
        );
      }
    }
  } catch {
    /* ignore */
  }
});

/* ─── Fullscreen / immersive toggle ──────────────────────────────── */
const isFullscreen = ref(false);
function toggleFullscreen() {
  isFullscreen.value = !isFullscreen.value;
  document.body.classList.toggle('ism-fullscreen', isFullscreen.value);
}
// Make sure we don't leave the class behind when the user navigates
// away to another v3 page.
import { onBeforeUnmount } from 'vue';
onBeforeUnmount(() => {
  if (isFullscreen.value) document.body.classList.remove('ism-fullscreen');
});
</script>

<template>
  <ShellLayout active-page="grid">
    <div class="page">
      <header class="header">
        <div>
          <div class="title">
            Mosaic <span class="warn">⚠️ resource-intensive</span>
          </div>
          <div class="subtitle">
            {{ visible.length }} of {{ candidates.length }} candidates · {{ allContent.length }} content items ·
            click a tile to take audio focus · right-click to open in v3 Testing Session
          </div>
        </div>
        <button
          type="button"
          class="fullscreen-btn"
          :title="isFullscreen ? 'Exit immersive mode' : 'Hide nav for an immersive video wall'"
          @click="toggleFullscreen"
        >{{ isFullscreen ? '✕ Exit fullscreen' : '⛶ Fullscreen' }}</button>
      </header>

      <div class="controls">
        <label>
          Protocol
          <select :value="protocol" @change="setParam('protocol', ($event.target as HTMLSelectElement).value)">
            <option v-for="o in PROTOCOL_CHOICES" :key="o.v" :value="o.v">{{ o.label }}</option>
          </select>
        </label>
        <label>
          Codec
          <select :value="codec" @change="setParam('codec', ($event.target as HTMLSelectElement).value)">
            <option v-for="o in CODEC_CHOICES" :key="o.v" :value="o.v">{{ o.label }}</option>
          </select>
        </label>
        <label>
          Segs
          <select :value="segs" @change="setParam('segs', ($event.target as HTMLSelectElement).value)">
            <option v-for="o in SEGMENT_CHOICES" :key="o.v" :value="o.v">{{ o.label }}</option>
          </select>
        </label>
        <label>
          Max res
          <select :value="maxResFilter" @change="setParam('maxRes', ($event.target as HTMLSelectElement).value)">
            <option v-for="o in MAX_RES_CHOICES" :key="o.v" :value="o.v">{{ o.label }}</option>
          </select>
        </label>
        <label>
          Content
          <select :value="contentFilter" @change="setParam('content', ($event.target as HTMLSelectElement).value)">
            <option value="all">All</option>
            <option v-for="n in contentNameOptions" :key="n" :value="n">{{ n }}</option>
          </select>
        </label>
        <label>
          Cols
          <input type="number" min="1" max="8" :value="cols" @change="setParam('cols', ($event.target as HTMLInputElement).value)" />
        </label>
        <label>
          Rows
          <input type="number" min="1" max="6" :value="rows" @change="setParam('rows', ($event.target as HTMLInputElement).value)" />
        </label>
        <button class="btn" type="button" @click="randomize">🎲 Shuffle</button>
        <button class="btn" type="button" @click="fetchContent">↻ Reload</button>
      </div>

      <div v-if="fetchError" class="banner banner-error">
        Couldn't load /api/content: {{ fetchError }}
      </div>
      <div v-else-if="!allContent.length" class="banner">Loading content…</div>
      <div v-else-if="!visible.length" class="banner">
        No content matches the current filters. Loosen them or click ↻ Reload.
      </div>

      <div class="grid" :style="gridStyle">
        <GridTile
          v-for="(item, i) in visible"
          :key="`${item.name}|${item.protocol}|${effectiveSegs}|${i}`"
          :content-name="item.name"
          :label="item.label"
          :protocol="item.protocol"
          :segs="effectiveSegs"
          :max-height="item.max_height ?? null"
          :max-resolution="item.max_resolution ?? null"
          :audio-focus="focusIndex === i"
          @focus="setFocus(i)"
          @contextmenu="onTileContextMenu"
        />
      </div>

      <!-- Right-click menu: ports the legacy /dashboard/grid.html
           `#gridContextMenu`. Click anywhere closes (handled in onMounted). -->
      <div
        v-show="menu.open"
        class="context-menu"
        :style="{ left: `${menu.x}px`, top: `${menu.y}px` }"
        @click.stop
      >
        <button @click="menuOpenTesting">Open in Testing Window</button>
        <button @click="menuCopyTesting">Copy Testing URL</button>
        <button
          v-if="developerMode"
          :disabled="menu.isDash"
          @click="menuOpenHlsjsDemo"
        >Open in HLS.js Demo</button>
      </div>
    </div>
  </ShellLayout>
</template>

<style scoped>
.page {
  padding: 16px 24px 24px;
  max-width: 1800px;
  margin: 0 auto;
}

/* Context menu — mirrors `.context-menu` in legacy /dashboard/grid.html. */
.context-menu {
  position: fixed;
  z-index: 1000;
  background: #1f2937;
  border: 1px solid #374151;
  border-radius: 6px;
  padding: 4px;
  min-width: 200px;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
  display: flex;
  flex-direction: column;
}
.context-menu button {
  background: transparent;
  border: none;
  color: #e5e7eb;
  text-align: left;
  padding: 8px 12px;
  font-size: 13px;
  cursor: pointer;
  border-radius: 4px;
}
.context-menu button:hover:not(:disabled) {
  background: #374151;
}
.context-menu button:disabled {
  color: #6b7280;
  cursor: not-allowed;
}

.header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  flex-wrap: wrap;
  gap: 16px;
  margin-bottom: 12px;
}
.title {
  font-size: 22px;
  font-weight: 600;
  color: #202124;
}
.warn {
  font-size: 12px;
  font-weight: 500;
  color: #b06000;
  margin-left: 6px;
}
.subtitle {
  font-size: 12px;
  color: #5f6368;
  margin-top: 2px;
}

.controls {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  font-size: 12px;
  color: #374151;
  align-items: center;
  margin-bottom: 12px;
}
.controls label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.controls select,
.controls input {
  background: #fff;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 4px 8px;
  font-size: 12px;
  color: #202124;
}
.controls input { width: 56px; text-align: right; }

.btn {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 4px 12px;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
}
.btn:hover { background: #e8eaed; }

.banner {
  font-size: 12px;
  padding: 10px 14px;
  background: #f8f9fa;
  border: 1px solid #e8eaed;
  border-radius: 6px;
  color: #5f6368;
  margin-bottom: 12px;
}
.banner-error {
  background: #fce8e6;
  border-color: #fca5a5;
  color: #991b1b;
}

.grid {
  display: grid;
  gap: 8px;
}

/* Fullscreen toggle button — sits at the top-right of the header. */
.fullscreen-btn {
  background: #1f2937;
  color: #e5e7eb;
  border: 1px solid #374151;
  padding: 6px 12px;
  border-radius: 4px;
  font-size: 12px;
  cursor: pointer;
}
.fullscreen-btn:hover { background: #374151; }
.header { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; }
</style>

<!-- Un-scoped style: the immersive-mode toggle adds `ism-fullscreen` to
     <body> and we want the rule to hit the ShellLayout sidebar (which
     is outside this page's scoped subtree). Mirrors the legacy CSS in
     /dashboard/grid.html: hide the sidebar, give the grid the full
     viewport width. -->
<style>
body.ism-fullscreen .ism-sidebar { display: none !important; }
body.ism-fullscreen .ism-app.has-sidebar > main,
body.ism-fullscreen .ism-app.has-sidebar { margin-left: 0 !important; padding-left: 0 !important; }
body.ism-fullscreen { background: #000; }
</style>
