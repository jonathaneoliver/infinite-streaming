<script setup lang="ts">
/**
 * VideoPlayerFrame.vue — top-of-page playback block matching the
 * legacy `.player-card` in testing-session.html. Composes:
 *
 *   - PlayerBadgesRow  — protocol / engine / codec / segment / 4K / group
 *   - PlayerActionsBar — Retry · Restart · Reload · Engine select ·
 *                        Allow 4K · Auto-Recovery · PiP · Rotate play_id
 *   - <video> element  — driven by hls.js / shaka / videojs / native
 *                        based on the engine selection or content type
 *   - PlayerStatsGrid  — 9 read-only stats below the player
 *   - DebugLog         — append-only event log
 *
 * Auto-detect engine: `.m3u8` + Safari → native; `.m3u8` + others →
 * hls.js; `.mpd` → shaka. videojs is opt-in; native always falls back.
 *
 * Soak helpers (PiP, auto-recovery, play_id rotation) are best-effort
 * — the controls are wired so the muscle memory carries over from the
 * legacy page even before every soak feature is finished.
 */
import { computed, onBeforeUnmount, ref, toRef, useTemplateRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import PlayerActionsBar from './PlayerActionsBar.vue';
import PlayerBadgesRow from './PlayerBadgesRow.vue';
import PlayerStatsGrid from './PlayerStatsGrid.vue';
import DebugLog from './DebugLog.vue';
import { useSessionMetrics } from '@/composables/useSessionMetrics';

const props = defineProps<{
  playerId: string;
  /** Optional explicit stream URL — overrides the master URL discovered
   *  from the PlayerRecord. Used when deep-linking from the legacy /
   *  v3 grid where the caller already knows the URL the tile is on
   *  (and the player might not yet be registered server-side). */
  urlOverride?: string;
}>();
const { player } = usePlayer(toRef(props, 'playerId'));

/**
 * Mirror of the legacy `normalizeTestingBaseUrl` from
 * content/shared/shared-nav.js: every video URL must hit the per-session
 * shaper port (UI-port with the last 3 digits replaced by `081`), not
 * the nginx UI port. The shaper multiplexes sessions by `?player_id=`
 * and is what applies per-session fault injection / traffic shaping.
 * Going direct to nginx (port 21000 / 30000) bypasses go-proxy entirely
 * and — at least under the test-dev wiring — the video doesn't play.
 *
 * Vite dev (port 5173) is a special case: the last-3-digits trick yields
 * "5081" which is not a real port. Until we wire dev-mode to point at
 * test-dev's shaper, leave the URL untouched on 5173 and let the user
 * see whatever the dev backend gives them.
 */
function normalizeStreamUrl(url: string): string {
  try {
    const parsed = new URL(url, window.location.href);
    const currentPort =
      window.location.port || (window.location.protocol === 'https:' ? '443' : '80');
    if (currentPort === '5173') return parsed.toString();
    if (currentPort.length < 4) return parsed.toString();
    const shaperPort = currentPort.slice(0, -3) + '081';
    parsed.hostname = window.location.hostname;
    parsed.port = shaperPort;
    parsed.protocol = window.location.protocol;
    return parsed.toString();
  } catch {
    return url;
  }
}

/** Web-minted play_id for this playback. Apple/Roku clients self-report
 *  a play_id; the v3 web player previously relied on the server-side
 *  v5 fallback derivation, which made archive lookups fragile. Minting
 *  here and surfacing via `?play_id=<uuid>` lets the proxy store the
 *  same id the dashboard later queries by.
 *  Bumped on Retry / Restart via mintPlayId() so each new playback gets
 *  a fresh row in the archive. */
function mintPlayId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback for environments without crypto.randomUUID (mirrors the
  // Grid.vue helper). Hand-assembled UUIDv4 with proper version bits.
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
const webPlayId = ref<string>(mintPlayId());

const masterUrl = computed<string | null>(() => {
  // Explicit override beats whatever the player record says.
  let raw: string | null = null;
  if (props.urlOverride && props.urlOverride.length) {
    raw = props.urlOverride;
    console.log('[VPF] masterUrl ← urlOverride', raw);
  } else {
    const m = player.value?.current_play?.manifest?.master_url;
    raw = typeof m === 'string' && m.length ? m : null;
    console.log('[VPF] masterUrl ← player.master_url', raw ?? '(null)');
  }
  if (!raw) return null;
  const normalized = normalizeStreamUrl(raw);
  if (normalized !== raw) console.log('[VPF] masterUrl normalized to shaper port', normalized);
  // Attach our minted play_id so the proxy records it on session
  // creation; this is what shows up in /api/v2/players.current_play.id
  // and in the forwarder's session_snapshots.play_id column.
  try {
    const u = new URL(normalized, window.location.href);
    if (!u.searchParams.get('play_id') && webPlayId.value) {
      u.searchParams.set('play_id', webPlayId.value);
    }
    return u.toString();
  } catch {
    return normalized;
  }
});

type Engine = 'auto' | 'hlsjs' | 'shaka' | 'videojs' | 'native';
function readStored<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    if (raw == null) return fallback;
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}
function writeStored<T>(key: string, val: T) {
  try { localStorage.setItem(key, JSON.stringify(val)); } catch { /* ignore */ }
}

const engine = ref<Engine>(readStored<Engine>('v3.player.engine', 'auto'));
const prefer4k = ref<boolean>(readStored<boolean>('v3.player.prefer4k', false));
const autoRecovery = ref<boolean>(readStored<boolean>('v3.player.autoRecovery', true));
const pipOn = ref<boolean>(readStored<boolean>('v3.player.pip', false));
const rotationSeconds = ref<number>(readStored<number>('v3.player.rotateSeconds', 0));
watch(engine,          (v) => writeStored('v3.player.engine', v));
watch(prefer4k,        (v) => writeStored('v3.player.prefer4k', v));
watch(autoRecovery,    (v) => writeStored('v3.player.autoRecovery', v));
watch(pipOn,           (v) => writeStored('v3.player.pip', v));
watch(rotationSeconds, (v) => writeStored('v3.player.rotateSeconds', v));

const videoEl = useTemplateRef<HTMLVideoElement>('videoEl');
const debugLog = useTemplateRef<InstanceType<typeof DebugLog>>('debugLog');

let activeInstance: { destroy: () => void } | null = null;

// Reactive refs that the metrics composable watches. The hls.js
// instance is exposed via `hlsInst` (null on other engines / before
// load); `activeEngine` is the resolved engine name (post-auto-detect)
// so telemetry says hlsjs / native / shaka rather than 'auto'.
const hlsInst = ref<any | null>(null);
const activeEngine = ref<Engine>('auto');

useSessionMetrics({
  playerId: toRef(props, 'playerId'),
  videoEl,
  hlsInst,
  engine: activeEngine,
});

function dbg(msg: string, level: 'info' | 'warn' | 'error' = 'info') {
  debugLog.value?.push(msg, level);
}

function detectEngine(url: string): Exclude<Engine, 'auto'> {
  if (/\.mpd(\?|$)/i.test(url)) return 'shaka';
  if (/\.m3u8(\?|$)/i.test(url)) {
    // Match the legacy testing-session.html priority (line 2157):
    // assume hls.js is the right choice for HLS and let the engine
    // branch lazy-load it. Native is only a fallback when MSE isn't
    // available (iOS Safari, which we don't target here).
    //
    // hls.js follows redirects via XHR and tracks the post-redirect URL
    // as the variant/segment resolution base — Safari native HLS
    // doesn't, and that mismatch breaks playback through the shaper's
    // `21081 → per-session port` 302.
    return typeof (window as any).MediaSource !== 'undefined' ? 'hlsjs' : 'native';
  }
  return 'native';
}

async function ensureScript(src: string): Promise<void> {
  if (document.querySelector(`script[data-loaded="${src}"]`)) return;
  await new Promise<void>((resolve, reject) => {
    const s = document.createElement('script');
    s.src = src;
    s.dataset.loaded = src;
    s.onload = () => resolve();
    s.onerror = () => reject(new Error(`load failed: ${src}`));
    document.head.appendChild(s);
  });
}

async function ensureHlsJs(): Promise<any> {
  if ((window as any).Hls) return (window as any).Hls;
  await ensureScript('https://cdn.jsdelivr.net/npm/hls.js@1.5.7/dist/hls.min.js');
  return (window as any).Hls;
}
async function ensureShaka(): Promise<any> {
  if ((window as any).shaka) return (window as any).shaka;
  await ensureScript('https://cdn.jsdelivr.net/npm/shaka-player@4.7.0/dist/shaka-player.compiled.js');
  return (window as any).shaka;
}
async function ensureVideoJs(): Promise<any> {
  if ((window as any).videojs) return (window as any).videojs;
  await ensureScript('https://vjs.zencdn.net/8.21.1/video.min.js');
  return (window as any).videojs;
}

function detach() {
  try { activeInstance?.destroy(); } catch { /* ignore */ }
  activeInstance = null;
  hlsInst.value = null;
  // Don't reset activeEngine here — it'll be re-set by the next attach
  // before the metrics composable's next heartbeat fires.
}

// Cancellation token. Each call to attach() bumps the counter and
// remembers it locally. After every `await` (engine script load,
// shaka.load(), etc.) we re-check: if `attachToken` no longer matches,
// a newer attach has superseded us — bail out without touching the
// video element so we don't end up with two engines fighting over the
// same `<video>` element (the `Canvas is already in use` analog).
let attachToken = 0;

/**
 * Pre-follow HTTP redirects so the URL we hand to <video> / hls.js /
 * shaka is the FINAL post-redirect URL. The shaper's contract is:
 *
 *   GET <shaper_port>/.../master.m3u8?player_id=X  →  302 to <per_session_port>
 *
 * Safari's native HLS implementation follows the redirect for the
 * manifest fetch itself but then resolves variant playlist references
 * against the ORIGINAL src URL — and the bare variant path on the
 * shaper port (without `?player_id=`) 404s, because the shaper can't
 * route an anonymous variant request to any session. Resolving the
 * redirect up front sidesteps this: by the time `<video src>` is set,
 * the URL already points at the per-session port directly, so every
 * relative variant + segment fetch lands on the right session.
 */
async function resolveRedirects(url: string): Promise<string> {
  try {
    const resp = await fetch(url, { method: 'GET', redirect: 'follow', credentials: 'omit' });
    const finalUrl = resp.url || url;
    if (finalUrl !== url) console.log('[VPF] resolveRedirects', url, '→', finalUrl);
    return finalUrl;
  } catch (e) {
    console.warn('[VPF] resolveRedirects failed, using original URL', e);
    return url;
  }
}

async function attach(urlIn: string) {
  console.log('[VPF] attach() called', { url: urlIn, hasVideoEl: !!videoEl.value, engine: engine.value });
  detach();
  const myToken = ++attachToken;
  const v = videoEl.value;
  if (!v) {
    console.warn('[VPF] attach BAILED — videoEl not yet mounted');
    return;
  }
  // Resolve the shaper's 302 → per-session port up front so that the
  // engine (especially Safari native HLS) sees variants/segments on
  // the per-session port and not the shaper port.
  const url = await resolveRedirects(urlIn);
  if (myToken !== attachToken) {
    console.log('[VPF] attach superseded during redirect resolve, bailing');
    return;
  }
  const chosen = engine.value === 'auto' ? detectEngine(url) : engine.value;
  activeEngine.value = chosen;
  console.log('[VPF] attach proceeding with engine', chosen);
  dbg(`attach ${chosen} → ${url}`);

  const cancelled = () => myToken !== attachToken;

  try {
    if (chosen === 'native') {
      console.log('[VPF] native: setting v.src to', url);
      console.log('[VPF] native: resolved absolute URL would be', new URL(url, window.location.href).href);
      v.addEventListener('error', () => {
        const me = v.error;
        console.error('[VPF] native <video> error', {
          code: me?.code,
          message: me?.message,
          MEDIA_ERR_ABORTED: me?.code === 1,
          MEDIA_ERR_NETWORK: me?.code === 2,
          MEDIA_ERR_DECODE: me?.code === 3,
          MEDIA_ERR_SRC_NOT_SUPPORTED: me?.code === 4,
        });
      });
      v.addEventListener('loadstart', () => console.log('[VPF] native: loadstart'));
      v.addEventListener('loadedmetadata', () => console.log('[VPF] native: loadedmetadata', { duration: v.duration }));
      v.addEventListener('canplay', () => console.log('[VPF] native: canplay'));
      v.addEventListener('playing', () => console.log('[VPF] native: playing'));
      v.addEventListener('stalled', () => console.warn('[VPF] native: stalled'));
      v.addEventListener('waiting', () => console.warn('[VPF] native: waiting'));
      v.src = url;
      v.play()
        .then(() => console.log('[VPF] native: play() resolved'))
        .catch((e) => console.warn('[VPF] native: play() rejected', e?.name, e?.message));
      if (cancelled()) return;
      activeInstance = { destroy: () => { v.removeAttribute('src'); v.load(); } };
      return;
    }
    if (chosen === 'hlsjs') {
      console.log('[VPF] hlsjs: loading hls.js library');
      const Hls = await ensureHlsJs();
      console.log('[VPF] hlsjs: lib loaded, cancelled?', cancelled());
      if (cancelled()) return;
      if (!Hls.isSupported()) {
        console.warn('[VPF] hls.js not supported by browser; falling back to native');
        dbg('hls.js not supported; falling back to native', 'warn');
        v.src = url;
        v.play().catch((e) => console.warn('[VPF] native play() rejected', e));
        activeInstance = { destroy: () => { v.removeAttribute('src'); v.load(); } };
        return;
      }
      const inst = new Hls({ liveSyncDuration: 3, liveMaxLatencyDuration: 10 });
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      console.log('[VPF] hlsjs: loadSource + attachMedia', url);
      inst.loadSource(url);
      inst.attachMedia(v);
      inst.on(Hls.Events.MANIFEST_PARSED, () => {
        console.log('[VPF] hlsjs: MANIFEST_PARSED, calling play()');
        v.play().catch((e) => console.warn('[VPF] play() rejected', e));
      });
      inst.on(Hls.Events.ERROR, (_: any, data: any) => {
        console.warn('[VPF] hlsjs ERROR', data);
        dbg(`hls.js error: ${data?.details ?? 'unknown'}`, 'warn');
      });
      // Hand the live hls.js instance to the metrics composable so it
      // can attach LEVEL_SWITCHED / BUFFER_STALLED / ERROR listeners.
      hlsInst.value = inst;
      activeInstance = {
        destroy: () => {
          hlsInst.value = null;
          inst.destroy();
        },
      };
      return;
    }
    if (chosen === 'shaka') {
      const shaka = await ensureShaka();
      if (cancelled()) return;
      shaka.polyfill.installAll();
      const inst = new shaka.Player(v);
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      inst.addEventListener('error', (e: any) => dbg(`shaka error: ${e?.detail?.message ?? 'unknown'}`, 'warn'));
      await inst.load(url).catch((err: any) => dbg(`shaka load failed: ${err?.message ?? err}`, 'error'));
      if (cancelled()) { try { inst.destroy(); } catch { /* ignore */ } return; }
      v.play().catch(() => {});
      activeInstance = { destroy: () => inst.destroy() };
      return;
    }
    if (chosen === 'videojs') {
      const videojs = await ensureVideoJs();
      if (cancelled()) return;
      const inst = videojs(v, { autoplay: true, muted: true, controls: true });
      if (cancelled()) { try { inst.dispose(); } catch { /* ignore */ } return; }
      inst.src({ src: url, type: url.includes('.mpd') ? 'application/dash+xml' : 'application/x-mpegURL' });
      activeInstance = { destroy: () => inst.dispose() };
      return;
    }
  } catch (err) {
    console.error('[VPF] attach threw', err);
    dbg(`attach failed: ${(err as any)?.message ?? err}`, 'error');
  }
}

// Watching videoEl itself (a ShallowRef from useTemplateRef) makes the
// mount race a non-issue: when the <video> element first commits to the
// DOM, videoEl flips from null → element and the watcher re-fires with
// a real target. `flush: 'post'` was unreliable on `immediate: true` —
// the callback ran during setup before the template had even rendered.
watch(
  [videoEl, masterUrl, engine],
  ([el, url, eng]) => {
    console.log('[VPF] watch fired', { url, eng, hasVideoEl: !!el });
    if (el && url) attach(url);
    else if (!url) detach();
  },
  { immediate: true },
);

// Picture-in-Picture toggle.
watch(pipOn, async (on) => {
  const v = videoEl.value;
  if (!v) return;
  try {
    if (on && document.pictureInPictureEnabled && !(document as any).pictureInPictureElement) {
      await (v as any).requestPictureInPicture();
      dbg('Entered PiP');
    } else if (!on && (document as any).pictureInPictureElement) {
      await (document as any).exitPictureInPicture();
      dbg('Exited PiP');
    }
  } catch (err: any) {
    dbg(`PiP error: ${err?.message ?? err}`, 'warn');
  }
});

function onRetry() {
  const url = masterUrl.value;
  if (!url) return;
  dbg('Retry Fetch');
  attach(url);
}
function onRestart() {
  const v = videoEl.value;
  if (!v) return;
  dbg('Restart Playback');
  try {
    v.currentTime = 0;
    v.play().catch(() => {});
  } catch { /* ignore */ }
}
function onReload() {
  dbg('Reload Page');
  window.location.reload();
}

// SSE missed counter (UI tile only).
const sseMissed = ref(0);

// Track number of stalls; on auto-recovery kick the engine.
let lastStalls = 0;
let lastError: string | null = null;
watch(
  () => player.value?.player_metrics,
  (pm) => {
    if (!pm) return;
    if (typeof pm.stalls === 'number' && pm.stalls > lastStalls) {
      const delta = pm.stalls - lastStalls;
      dbg(`Stall +${delta} (total ${pm.stalls})`, 'warn');
      lastStalls = pm.stalls;
      if (autoRecovery.value && delta > 0) {
        // best-effort kick: restart playback on a stall.
        onRestart();
      }
    }
    if (pm.error && pm.error !== lastError) {
      dbg(`Player error: ${pm.error}`, 'error');
      lastError = pm.error;
    }
  },
);

onBeforeUnmount(() => detach());
</script>

<template>
  <div class="video-frame">
    <PlayerBadgesRow :player-id="playerId" :engine="engine" />

    <PlayerActionsBar
      v-model:engine="engine"
      v-model:prefer4k="prefer4k"
      v-model:auto-recovery="autoRecovery"
      v-model:pip="pipOn"
      v-model:rotation-seconds="rotationSeconds"
      @retry="onRetry"
      @restart="onRestart"
      @reload="onReload"
    />

    <div class="url-line" v-if="masterUrl" :title="masterUrl">
      {{ masterUrl }}
    </div>
    <div class="url-line muted" v-else>No master URL yet</div>

    <video
      ref="videoEl"
      controls
      muted
      playsinline
      autoplay
      class="player"
    />

    <PlayerStatsGrid :player-id="playerId" :sse-missed="sseMissed" />

    <DebugLog ref="debugLog" />
  </div>
</template>

<style scoped>
.video-frame {
  display: grid;
  gap: 8px;
}
.url-line {
  font-family: ui-monospace, monospace;
  font-size: 11px;
  color: #5f6368;
  word-break: break-all;
}
.url-line.muted { color: #9aa0a6; font-style: italic; }
.player {
  width: 100%;
  background: #000;
  border-radius: 6px;
  aspect-ratio: 16 / 9;
}
</style>
