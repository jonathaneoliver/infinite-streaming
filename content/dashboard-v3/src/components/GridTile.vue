<script setup lang="ts">
/**
 * GridTile.vue — one cell of the v3 Mosaic. Mirrors the legacy
 * grid.html per-cell layout:
 *
 *   - Picks engine from protocol (HLS → hls.js / native, DASH → shaka).
 *   - Renders status indicator (Loading / Playing / Buffering / Error)
 *     in the upper-left corner.
 *   - Shows badge strip below the video (engine / protocol / codec /
 *     segment) and the content name label.
 *   - Click → take audio focus (un-mutes this tile, parent mutes the
 *     others). Emits `focus` so the parent can implement that.
 *   - Right-click → open v3 testing-session.html with both `player_id`
 *     and `url` query params (mirrors the legacy context menu).
 *   - Cancellation token guards `attach()` so fast prop changes (filter
 *     reloads, random-shuffle) don't end up with two engines on one
 *     <video>.
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue';

const props = defineProps<{
  contentName: string;
  /** Friendly label shown under the video. */
  label?: string;
  /** 'hls' or 'dash'. */
  protocol: 'hls' | 'dash';
  /** Segment-duration token (2s / 6s / ll). */
  segs: '2' | '6' | 'll';
  /** Optional override engine. Defaults to engine-from-protocol. */
  engine?: 'auto' | 'hlsjs' | 'shaka' | 'native';
  /** Whether THIS tile has audio focus. Mutes the <video> when false. */
  audioFocus?: boolean;
  /** Max content height in pixels from /api/content metadata, used to
   *  surface a 4K badge before the video has actually loaded. The
   *  videoHeight read at playback time is also checked. */
  maxHeight?: number | null;
  /** Stringified max_resolution from the content metadata (e.g.
   *  "3840x2160", "2160p", "2160"). Same purpose as maxHeight — parsed
   *  defensively into a pixel count for the 4K detection. */
  maxResolution?: string | number | null;
}>();

const emit = defineEmits<{
  (e: 'focus'): void;
  (e: 'contextmenu', payload: { x: number; y: number; url: string; isDash: boolean }): void;
}>();

type EngineKind = 'hlsjs' | 'shaka' | 'native';

function randUuid(): string {
  if (typeof crypto !== 'undefined' && typeof (crypto as any).randomUUID === 'function') {
    return (crypto as any).randomUUID();
  }
  const bytes = new Uint8Array(16);
  if (window.crypto?.getRandomValues) window.crypto.getRandomValues(bytes);
  else for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

const tileId = ref<string>(randUuid());

/** Per-content/protocol/segs URL — mirrors legacy buildUrl():
 *    HLS:   /go-live/<name>/master_<segs>s.m3u8     (or `.m3u8` when segs=ll)
 *    DASH:  /go-live/<name>/manifest_<segs>s.mpd    (or `.mpd` when segs=ll) */
const baseUrl = computed<string>(() => {
  const ext = props.protocol === 'dash' ? 'mpd' : 'm3u8';
  const base = props.protocol === 'dash' ? 'manifest' : 'master';
  if (props.segs === 'll') return `/go-live/${props.contentName}/${base}.${ext}`;
  return `/go-live/${props.contentName}/${base}_${props.segs}s.${ext}`;
});

// Grid tiles play DIRECTLY through nginx (port 21000 / 30000) — see the
// legacy grid.html: tiles are passive viewers that bypass the per-session
// shaper entirely. The shaper (port 21081) is only relevant for
// testing-session.html, where fault injection / shaping actually matters.
// Including a `player_id` on the URL keeps the right-click → testing-session
// hand-off coherent (same id flows through), but tile playback never
// touches go-proxy.
const fullUrl = computed<string>(() => {
  const sep = baseUrl.value.includes('?') ? '&' : '?';
  return `${baseUrl.value}${sep}player_id=${encodeURIComponent(tileId.value)}`;
});

/** Codec inferred from content slug suffix (legacy convention). */
const codec = computed<'H264' | 'HEVC' | 'AV1' | '?'>(() => {
  const n = props.contentName.toLowerCase();
  if (/_av1\b|_av1_/.test(n)) return 'AV1';
  if (/_hevc\b|_hevc_/.test(n)) return 'HEVC';
  if (/_h264\b|_h264_/.test(n)) return 'H264';
  return '?';
});

const engineKind = computed<EngineKind>(() => {
  if (props.engine === 'native') return 'native';
  if (props.engine === 'shaka') return 'shaka';
  if (props.engine === 'hlsjs') return 'hlsjs';
  // Auto: pick based on protocol. For HLS, prefer hls.js over native
  // (matches legacy testing-session.html). hls.js handles the shaper's
  // 302 → per-session port redirect correctly via XHR; native Safari
  // HLS leaks the original (shaper-port) URL into variant resolution.
  if (props.protocol === 'dash') return 'shaka';
  return typeof (window as any).MediaSource !== 'undefined' ? 'hlsjs' : 'native';
});

const engineLabel = computed<string>(() => {
  switch (engineKind.value) {
    case 'shaka':  return 'Shaka';
    case 'hlsjs':  return 'HLS.js';
    case 'native': return 'Native';
  }
});

const protocolLabel = computed<string>(() => props.protocol.toUpperCase());

const segsLabel = computed<string>(() => {
  if (props.segs === 'll') return 'LL';
  return `${props.segs}S`;
});

const videoEl = ref<HTMLVideoElement | null>(null);
let activeInstance: { destroy: () => void } | null = null;
let attachToken = 0;

const status = ref<'loading' | 'playing' | 'buffering' | 'error' | 'idle'>('idle');
const errorMessage = ref<string>('');

/* ─── 4K/UHD detection ─────────────────────────────────────────────
 *  Same heuristic as the legacy `update4kBadge()`: prefer the metadata
 *  height (cheap, available before playback starts) and also check the
 *  actual videoHeight once known (covers content where metadata is
 *  missing or wrong). The badge shows when either says ≥ 2160. */
const UHD_HEIGHT = 2160;
const videoHeight = ref<number>(0);
function parseMaxResolution(v: string | number | null | undefined): number | null {
  if (v == null) return null;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  const s = String(v).trim();
  // Accept "3840x2160", "2160p", "2160".
  let m = s.match(/\d{3,4}x(\d{3,4})/);
  if (m) return Number(m[1]);
  m = s.match(/^(\d{3,4})p$/i);
  if (m) return Number(m[1]);
  m = s.match(/^(\d{3,4})$/);
  if (m) return Number(m[1]);
  return null;
}
const is4k = computed<boolean>(() => {
  const meta = props.maxHeight ?? parseMaxResolution(props.maxResolution);
  if (meta != null && meta >= UHD_HEIGHT) return true;
  if (videoHeight.value >= UHD_HEIGHT) return true;
  return false;
});

// Concurrent-safe script loader. 16 tiles mount at once and all hit
// ensureHlsJs() before any onload fires. The previous version checked
// `document.querySelector('[data-loaded]')` for dedupe, but the tag
// exists synchronously after createElement — so tiles 2-16 saw the
// tag, immediately resolved with `window.Hls` still undefined, and
// crashed on `Hls.isSupported`. Cache the Promise itself so all
// callers wait on the same onload. Stored on `window` because
// `<script setup>` blocks run per instance, not per module.
function loadScript(src: string): Promise<void> {
  const store: Map<string, Promise<void>> =
    ((window as any).__v3ScriptPromises ??= new Map());
  const cached = store.get(src);
  if (cached) return cached;
  const p = new Promise<void>((resolve, reject) => {
    const s = document.createElement('script');
    s.src = src;
    s.onload = () => resolve();
    s.onerror = () => {
      store.delete(src);
      reject(new Error(`load failed: ${src}`));
    };
    document.head.appendChild(s);
  });
  store.set(src, p);
  return p;
}
async function ensureHlsJs(): Promise<any> {
  if ((window as any).Hls) return (window as any).Hls;
  await loadScript('https://cdn.jsdelivr.net/npm/hls.js@1.5.15/dist/hls.min.js');
  return (window as any).Hls;
}
async function ensureShaka(): Promise<any> {
  if ((window as any).shaka) return (window as any).shaka;
  await loadScript('https://cdn.jsdelivr.net/npm/shaka-player@4.7.7/dist/shaka-player.compiled.js');
  return (window as any).shaka;
}

function detach() {
  try { activeInstance?.destroy(); } catch { /* ignore */ }
  activeInstance = null;
}

const tilePrefix = computed(() => `[GT ${tileId.value.slice(0, 8)} ${props.contentName.slice(0, 20)}]`);

async function attach(url: string) {
  console.log(tilePrefix.value, 'attach() called', { url, hasVideoEl: !!videoEl.value, engineKind: engineKind.value });
  detach();
  const myToken = ++attachToken;
  const v = videoEl.value;
  if (!v) {
    console.warn(tilePrefix.value, 'attach BAILED — videoEl not yet mounted');
    return;
  }
  const cancelled = () => myToken !== attachToken;

  status.value = 'loading';
  errorMessage.value = '';
  v.muted = !props.audioFocus;
  v.volume = props.audioFocus ? 1 : 0;

  try {
    if (engineKind.value === 'native') {
      console.log(tilePrefix.value, 'native: setting v.src', url);
      v.src = url;
      v.play()
        .then(() => console.log(tilePrefix.value, 'native: play() resolved'))
        .catch((e) => console.warn(tilePrefix.value, 'native: play() rejected', e?.name, e?.message));
      if (cancelled()) return;
      activeInstance = { destroy: () => { v.removeAttribute('src'); v.load(); } };
      return;
    }
    if (engineKind.value === 'hlsjs') {
      console.log(tilePrefix.value, 'hlsjs: loading lib');
      const Hls = await ensureHlsJs();
      console.log(tilePrefix.value, 'hlsjs: lib loaded, isSupported=', Hls.isSupported(), 'cancelled?', cancelled());
      if (cancelled()) return;
      if (!Hls.isSupported()) {
        console.warn(tilePrefix.value, 'hls.js not supported — falling back to native');
        v.src = url;
        v.play().catch((e) => console.warn(tilePrefix.value, 'native fallback play() rejected', e?.message));
        activeInstance = { destroy: () => { v.removeAttribute('src'); v.load(); } };
        return;
      }
      const inst = new Hls({
        maxBufferLength: 10,
        maxMaxBufferLength: 30,
        backBufferLength: 30,
        liveSyncDurationCount: 3,
        liveMaxLatencyDurationCount: 10,
        startLevel: -1,
      });
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      console.log(tilePrefix.value, 'hlsjs: loadSource + attachMedia', url);
      inst.loadSource(url);
      inst.attachMedia(v);
      inst.on(Hls.Events.MANIFEST_PARSED, () => {
        console.log(tilePrefix.value, 'hlsjs: MANIFEST_PARSED, calling play()');
        v.play().catch((e) => console.warn(tilePrefix.value, 'hlsjs play() rejected', e?.message));
      });
      inst.on(Hls.Events.ERROR, (_: any, data: any) => {
        console.warn(tilePrefix.value, 'hlsjs ERROR', { type: data?.type, details: data?.details, fatal: data?.fatal });
        if (data?.fatal) {
          status.value = 'error';
          errorMessage.value = data?.details || 'hls error';
        }
      });
      activeInstance = { destroy: () => inst.destroy() };
      return;
    }
    if (engineKind.value === 'shaka') {
      console.log(tilePrefix.value, 'shaka: loading lib');
      const shaka = await ensureShaka();
      console.log(tilePrefix.value, 'shaka: lib loaded, cancelled?', cancelled());
      if (cancelled()) return;
      shaka.polyfill.installAll();
      if (!shaka.Player.isBrowserSupported()) {
        console.error(tilePrefix.value, 'shaka not browser-supported');
        status.value = 'error';
        errorMessage.value = 'Shaka not supported';
        return;
      }
      const inst = new shaka.Player(v);
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      inst.addEventListener('error', (e: any) => {
        console.warn(tilePrefix.value, 'shaka error event', e?.detail);
        status.value = 'error';
        errorMessage.value = e?.detail?.message || 'shaka error';
      });
      console.log(tilePrefix.value, 'shaka: load()', url);
      await inst.load(url).catch((err: any) => {
        console.error(tilePrefix.value, 'shaka load failed', err?.message ?? err);
        status.value = 'error';
        errorMessage.value = err?.message || 'shaka load failed';
      });
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      console.log(tilePrefix.value, 'shaka: load done, calling play()');
      v.play().catch((e) => console.warn(tilePrefix.value, 'shaka play() rejected', e?.message));
      activeInstance = { destroy: () => inst.destroy() };
      return;
    }
  } catch (err: any) {
    console.error(tilePrefix.value, 'attach threw', err);
    status.value = 'error';
    errorMessage.value = err?.message || String(err);
  }
}

// Audio-focus changes don't reload the stream; just flip muted state.
watch(
  () => props.audioFocus,
  (focus) => {
    const v = videoEl.value;
    if (!v) return;
    v.muted = !focus;
    v.volume = focus ? 1 : 0;
  },
);

// Watch videoEl as a reactive dep so the attach fires when the <video>
// element actually commits to the DOM. `flush: 'post'` on immediate:true
// fires during setup BEFORE the template renders — same race we hit in
// VideoPlayerFrame.
watch(
  [videoEl, fullUrl],
  ([el, u]) => {
    console.log(tilePrefix.value, 'watch fired', { hasVideoEl: !!el, url: u });
    if (el && u) attach(u);
    else if (!u) detach();
  },
  { immediate: true },
);

onBeforeUnmount(() => detach());

function onVideoPlaying()    { status.value = 'playing'; }
function onVideoWaiting()    { status.value = 'buffering'; }
function onVideoError()      {
  if (status.value !== 'error') status.value = 'error';
  if (!errorMessage.value) errorMessage.value = 'video error';
}
function onLoadedMetadata() {
  const v = videoEl.value;
  if (!v) return;
  videoHeight.value = v.videoHeight || 0;
}

/** Reload this tile only. Mirrors the legacy `reloadPlayer(index)`:
 *  destroys the active engine + re-runs attach. Useful when one tile
 *  errors out and you want to recover it without nuking the whole
 *  grid. */
async function reloadTile(ev?: MouseEvent) {
  ev?.stopPropagation();
  status.value = 'loading';
  errorMessage.value = '';
  videoHeight.value = 0;
  detach();
  if (fullUrl.value) attach(fullUrl.value);
}

function onTileClick(e: MouseEvent) {
  // Plain left-click on the tile (anywhere but the native <video>
  // controls) takes audio focus.
  if ((e.target as HTMLElement).closest('video')) return;
  emit('focus');
}

/**
 * Right-click: hand the URL up to Grid.vue which renders the 3-option
 * legacy context menu (Open in Testing Window / Copy URL / HLS.js Demo).
 * Each menu action mints a FRESH player_id (legacy behaviour), so this
 * tile's `tileId` is not used downstream — the tile is just a passive
 * playback surface.
 */
function onContextMenu(e: MouseEvent) {
  e.preventDefault();
  emit('contextmenu', { x: e.clientX, y: e.clientY, url: fullUrl.value, isDash: props.protocol === 'dash' });
}

const statusLabel = computed<string>(() => {
  switch (status.value) {
    case 'loading':   return 'Loading…';
    case 'playing':   return 'Playing';
    case 'buffering': return 'Buffering…';
    case 'error':     return 'Error';
    case 'idle':      return 'Idle';
  }
});
</script>

<template>
  <div
    class="tile"
    :class="[`engine-${engineKind}`, `status-${status}`, { focus: audioFocus }]"
    @click="onTileClick"
    @contextmenu="onContextMenu"
  >
    <div class="media">
      <span class="status" :class="`s-${status}`">{{ statusLabel }}</span>
      <button
        class="reload-btn"
        type="button"
        title="Reload this tile"
        @click="reloadTile"
        @mousedown.stop
      >↻</button>
      <video
        ref="videoEl"
        controls
        muted
        playsinline
        autoplay
        class="player"
        @playing="onVideoPlaying"
        @waiting="onVideoWaiting"
        @error="onVideoError"
        @loadedmetadata="onLoadedMetadata"
      />
      <div v-if="status === 'error'" class="err-overlay">
        <strong>Error</strong>
        <span class="err-msg">{{ errorMessage }}</span>
      </div>
      <!-- Hover info overlay: full content name + URL.
           Auto-hides on cursor leave; doesn't interfere with native
           video controls (pointer-events: none on the panel). -->
      <div class="hover-info">
        <div class="hi-name" :title="contentName">{{ contentName }}</div>
        <div class="hi-url" :title="fullUrl">{{ fullUrl }}</div>
      </div>
    </div>
    <div class="meta">
      <span class="label" :title="contentName">{{ label || contentName }}</span>
      <span class="badges">
        <span class="badge" :class="`engine-${engineKind}`">{{ engineLabel }}</span>
        <span class="badge" :class="`proto-${protocol}`">{{ protocolLabel }}</span>
        <span class="badge" :class="`codec-${codec.toLowerCase()}`">{{ codec }}</span>
        <span class="badge" :class="`segs-${segs}`">{{ segsLabel }}</span>
        <span v-if="is4k" class="badge uhd" title="≥ 2160p">4K</span>
      </span>
    </div>
  </div>
</template>

<style scoped>
.tile {
  background: #0f172a;
  border: 1px solid #1e293b;
  border-radius: 6px;
  overflow: hidden;
  display: grid;
  cursor: pointer;
  transition: box-shadow 0.15s, border-color 0.15s;
}
.tile:hover { border-color: #475569; }
.tile.focus {
  border-color: #f59e0b;
  box-shadow: 0 0 0 2px rgba(245, 158, 11, 0.55);
}

.media {
  position: relative;
  background: #000;
}
.player {
  width: 100%;
  aspect-ratio: 16 / 9;
  background: #000;
  display: block;
}

.status {
  position: absolute;
  top: 6px;
  left: 6px;
  z-index: 2;
  padding: 2px 8px;
  border-radius: 4px;
  font-size: 10px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  font-family: ui-monospace, monospace;
  background: rgba(0, 0, 0, 0.6);
  color: #fff;
  pointer-events: none;
}
.status.s-loading,
.status.s-buffering { background: rgba(245, 158, 11, 0.85); color: #1f1100; }
.status.s-playing   { background: rgba(34, 197, 94, 0.85); color: #051a0a; }
.status.s-error     { background: rgba(239, 68, 68, 0.9); color: #fff; }
.status.s-idle      { background: rgba(148, 163, 184, 0.85); color: #1f2937; }

.err-overlay {
  position: absolute;
  inset: 0;
  background: rgba(127, 29, 29, 0.72);
  color: #fff;
  display: grid;
  gap: 4px;
  place-content: center;
  text-align: center;
  font-size: 11px;
  padding: 12px;
  pointer-events: none;
}
.err-msg { font-family: ui-monospace, monospace; opacity: 0.85; }

/* Reload button — top-right of the media area. Hidden until the tile
 * is hovered (so it doesn't compete with the native video controls in
 * the playing state). Sits above everything else. */
.reload-btn {
  position: absolute;
  top: 6px;
  right: 6px;
  z-index: 3;
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  background: rgba(0, 0, 0, 0.55);
  color: #fff;
  border: 1px solid rgba(255, 255, 255, 0.18);
  border-radius: 4px;
  cursor: pointer;
  font-size: 13px;
  line-height: 1;
  opacity: 0;
  transition: opacity 0.15s, background 0.15s;
}
.tile:hover .reload-btn,
.tile.status-error .reload-btn { opacity: 1; }
.reload-btn:hover { background: rgba(0, 0, 0, 0.85); }

/* Hover info overlay — centered card with the full content name + URL.
 * Solid background so the text is readable over any video frame; the
 * `pointer-events: none` keeps it from intercepting the native video
 * controls underneath. */
.hover-info {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 2;
  max-width: calc(100% - 24px);
  background: rgba(15, 23, 42, 0.94);
  border: 1px solid rgba(255, 255, 255, 0.18);
  color: #f1f5f9;
  padding: 8px 12px;
  border-radius: 6px;
  font-size: 11px;
  font-family: ui-monospace, monospace;
  pointer-events: none;
  opacity: 0;
  transition: opacity 0.15s;
  display: grid;
  gap: 4px;
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.5);
}
.tile:hover .hover-info { opacity: 1; }
.hi-name {
  font-weight: 600;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: #fff;
}
.hi-url {
  color: #cbd5e1;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.meta {
  display: grid;
  gap: 2px;
  padding: 4px 6px;
  background: #1e293b;
  color: #cbd5e1;
  font-size: 10px;
  font-family: ui-monospace, monospace;
}
.label {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  font-weight: 500;
}

.badges {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
}
.badge {
  background: #334155;
  color: #e2e8f0;
  padding: 0 6px;
  border-radius: 4px;
  font-size: 9px;
  font-weight: 700;
  letter-spacing: 0.3px;
}
.badge.engine-hlsjs   { background: #1d4ed8; }
.badge.engine-shaka   { background: #6d28d9; }
.badge.engine-native  { background: #0e7490; }
.badge.proto-hls      { background: #b45309; }
.badge.proto-dash     { background: #4338ca; }
.badge.codec-h264     { background: #15803d; }
.badge.codec-hevc     { background: #b91c1c; }
.badge.uhd            { background: #f59e0b; color: #1f1100; }
.badge.codec-av1      { background: #7e22ce; }
.badge.segs-2         { background: #be185d; }
.badge.segs-6         { background: #047857; }
.badge.segs-ll        { background: #c2410c; }
</style>
