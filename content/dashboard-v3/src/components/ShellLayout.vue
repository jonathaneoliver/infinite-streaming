<script setup lang="ts">
/**
 * ShellLayout.vue — wraps a page with the InfiniteStream sidebar +
 * header chrome so every v3 page has the same nav as the legacy
 * shared-nav.js setup.
 *
 * Props:
 *   activePage — id matching a sidebar nav item, e.g. 'dashboard',
 *   'testing', 'test-playback', 'sessions', 'monitor'. Used for the
 *   active-state highlight.
 *
 * Slots:
 *   default — the page body.
 *   header-right — optional content for the header's right side.
 *
 * Sidebar collapse state persists to localStorage under the same key
 * the legacy used (`ismSidebarCollapsed`) so the user's collapsed/
 * expanded preference survives the legacy/v3 transition.
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';

const props = defineProps<{ activePage: string }>();

interface NavItem {
  id: string;
  icon: string;
  text: string;
  href: string;
  warning?: boolean;
  alpha?: boolean;
}

interface NavSection {
  title: string;
  items: NavItem[];
}

/** Dev-mode detection. Vite serves the v3 app on port 5173; the legacy
 *  pages assume they're on the actual backend (21000-ish), where
 *  `shared-nav.js` does `port.slice(0, -3) + '081'` to derive the per-
 *  session shaper port. On 5173 that yields the bogus port 5081 and
 *  the right-click-to-testing-session URL breaks.
 *
 *  When we detect we're on the Vite dev server, rewrite legacy hrefs
 *  to point at the absolute test-dev backend so the legacy port logic
 *  has a valid origin to work from. v3 pages stay on localhost so HMR
 *  keeps working there. */
const DEV_PORT = '5173';
const DEV_BACKEND = 'https://jonathanoliver-ubuntu.local:21000';

// The v3 pages are served at clean /dashboard/<page>.html in production
// (nginx rewrites them into /content/dashboard/v3/), but the Vite dev server
// serves them under its base, /dashboard/v3/. Map the v3 page set back to
// /v3/ in dev so links stay on the Vite dev server (HMR); legacy/unmigrated
// pages are served by the real backend.
const V3_PAGES =
  /^\/dashboard\/(dashboard|testing|testing-session|sessions|session-viewer|grid|characterization|ask|hello)\.html/;
function rewriteHrefForDev(href: string): string {
  if (typeof window === 'undefined') return href;
  if (window.location.port !== DEV_PORT) return href;
  if (V3_PAGES.test(href)) return href.replace('/dashboard/', '/dashboard/v3/'); // stay on the Vite dev server
  if (href.startsWith('http')) return href;            // already absolute
  return DEV_BACKEND + href;                           // legacy pages → backend
}

// --- "Testing Playback" nav item: build a *playable* URL from the
// currently-selected content (shared with the legacy nav via the
// `ismSelectedUrl` localStorage key, written by Grid / play-10ft /
// playback / segment-duration). Reuse a stable test-playback player_id
// if one exists, else mint and persist one. Mirrors shared-nav.js. ---
function mintPlayerId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) crypto.getRandomValues(bytes);
  else for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function getOrCreateTestPlaybackPlayerId(): string {
  const KEY = 'ismTestPlaybackPlayerId';
  try {
    const existing = localStorage.getItem(KEY);
    if (existing) return existing;
    const id = mintPlayerId();
    localStorage.setItem(KEY, id);
    return id;
  } catch {
    return mintPlayerId();
  }
}

// Rewrite a content URL to the per-session shaper port and set player_id
// (same convention as Grid.vue's buildTestingUrl).
function buildTestingUrl(url: string, playerId: string): string {
  let absolute: URL;
  try {
    absolute = new URL(url, window.location.href);
  } catch {
    return url;
  }
  const currentPort =
    window.location.port || (window.location.protocol === 'https:' ? '443' : '80');
  if (currentPort !== DEV_PORT && currentPort.length >= 4) {
    absolute.hostname = window.location.hostname;
    absolute.port = currentPort.slice(0, -3) + '081';
    absolute.protocol = window.location.protocol;
  }
  absolute.searchParams.set('player_id', playerId);
  return absolute.toString();
}

// Navigate to a testing-session built from a content URL: get-or-create a
// stable player_id, rewrite the content URL to the shaper port + attach the
// player_id (so it shows in BOTH places, like Grid's right-click), and go.
function gotoTestingPlayback(contentUrl: string): void {
  const playerId = getOrCreateTestPlaybackPlayerId();
  const testingUrl = buildTestingUrl(contentUrl, playerId);
  const pageUrl =
    `/dashboard/testing-session.html?player_id=${encodeURIComponent(playerId)}` +
    `&url=${encodeURIComponent(testingUrl)}`;
  window.location.href = rewriteHrefForDev(pageUrl);
}

// "Testing Playback" nav item: always produce a playable URL. Use the
// selected content (ismSelectedUrl — written on tile focus / content select)
// if present; otherwise fall back to the first content item from the
// catalogue. Without this the link landed on the empty ?nav=1 page with no
// player_id (right-click does NOT set ismSelectedUrl, only left-click focus).
async function onNavClick(item: NavItem, e: MouseEvent): Promise<void> {
  if (item.id !== 'test-playback') return;
  e.preventDefault();
  let selected = '';
  try {
    selected = localStorage.getItem('ismSelectedUrl') || '';
  } catch {
    selected = '';
  }
  if (selected) {
    gotoTestingPlayback(selected);
    return;
  }
  // No selection — pick the first available content so the nav item still
  // generates a working, player_id-bearing URL.
  try {
    const r = await fetch('/api/content');
    const list = (await r.json()) as Array<{ name: string; has_hls?: boolean; has_dash?: boolean }>;
    const first = Array.isArray(list) ? (list.find((c) => c.has_hls !== false) ?? list[0]) : undefined;
    if (first?.name) {
      const path = first.has_hls === false && first.has_dash ? 'manifest_6s.mpd' : 'master_6s.m3u8';
      gotoTestingPlayback(`/go-live/${first.name}/${path}`);
      return;
    }
  } catch {
    /* fall through to the empty page */
  }
  window.location.href = rewriteHrefForDev('/dashboard/testing-session.html?nav=1');
}

// Sidebar mirrors shared/shared-nav.js. v3-native pages link to their
// v3 URLs; pages that haven't been migrated yet keep the legacy URL.
const sections: NavSection[] = [
  {
    title: 'MAIN',
    items: [
      { id: 'dashboard', icon: '📊', text: 'Dashboard', href: '/dashboard/dashboard.html' },
    ],
  },
  {
    title: 'CONTENT',
    items: [
      { id: 'upload',  icon: '📤', text: 'Upload Content', href: '/dashboard/upload.html' },
      { id: 'sources', icon: '📚', text: 'Source Library', href: '/dashboard/sources.html' },
      { id: 'jobs',    icon: '💼', text: 'Encoding Jobs',  href: '/dashboard/jobs.html' },
    ],
  },
  {
    title: 'PLAYBACK',
    items: [
      { id: 'grid',          icon: '🎮', text: 'Mosaic',          href: '/dashboard/grid.html', warning: true },
      { id: 'mosaic-10ft',   icon: '📺', text: '10ft UI',         href: '/dashboard/mosaic-10ft.html', alpha: true },
      { id: 'playback',      icon: '▶️', text: 'Playback',        href: '/dashboard/playback.html' },
      { id: 'test-playback', icon: '🧭', text: 'Testing Playback',href: '/dashboard/testing-session.html?nav=1' },
      { id: 'testing',       icon: '🧪', text: 'Testing Monitor', href: '/dashboard/testing.html' },
      { id: 'sessions',      icon: '⏪', text: 'Sessions',         href: '/dashboard/sessions.html' },
      { id: 'characterization', icon: '📈', text: 'Automated Testing', href: '/dashboard/characterization.html' },
      { id: 'quartet',       icon: '🎬', text: 'Quartet',          href: '/dashboard/quartet.html', alpha: true },
      { id: 'segment-duration', icon: '⏱️', text: 'Live Offset',   href: '/dashboard/segment-duration-comparison.html', alpha: true },
    ],
  },
  {
    title: 'LIVE STREAMING',
    items: [
      { id: 'monitor', icon: '📡', text: 'Monitor', href: '/dashboard/go-monitor.html' },
    ],
  },
];

const collapsed = ref<boolean>(localStorage.getItem('ismSidebarCollapsed') === '1');

function toggleCollapse() {
  collapsed.value = !collapsed.value;
  localStorage.setItem('ismSidebarCollapsed', collapsed.value ? '1' : '0');
}

const mobileOpen = ref(false);
function toggleMobile() { mobileOpen.value = !mobileOpen.value; }

function isActive(id: string): boolean {
  return id === props.activePage;
}

function onSidebarClickPeek(e: MouseEvent) {
  if (!collapsed.value) return;
  if ((e.target as HTMLElement).closest('.collapse-btn')) return;
  toggleCollapse();
}

// Listen to ismSidebarCollapsed changes from other tabs so the legacy
// pages and v3 pages stay in sync. Also handles changes from the same
// tab if some other component flips the setting.
function onStorage(e: StorageEvent) {
  if (e.key === 'ismSidebarCollapsed') {
    collapsed.value = e.newValue === '1';
  }
}

// Publish the header's real rendered height to --header-height on
// :root so floating siblings (notably the Teleport-to-body chat
// dock) can position their `top` flush with the header's bottom
// regardless of header height tweaks (banner row, mobile, etc.).
// Previously the chat dock relied on a 64px fallback and ended up
// overlapping the header when the var wasn't set.
const headerEl = ref<HTMLElement | null>(null);
let headerObs: ResizeObserver | null = null;
function publishHeaderHeight() {
  const h = headerEl.value?.getBoundingClientRect().height ?? 64;
  document.documentElement.style.setProperty('--header-height', `${Math.round(h)}px`);
}
onMounted(() => {
  window.addEventListener('storage', onStorage);
  // ism-header is the only <header class="ism-header"> in the tree.
  headerEl.value = document.querySelector('.ism-header');
  publishHeaderHeight();
  if (headerEl.value && typeof ResizeObserver !== 'undefined') {
    headerObs = new ResizeObserver(publishHeaderHeight);
    headerObs.observe(headerEl.value);
  }
  window.addEventListener('resize', publishHeaderHeight);
});
onBeforeUnmount(() => {
  window.removeEventListener('storage', onStorage);
  window.removeEventListener('resize', publishHeaderHeight);
  headerObs?.disconnect();
  headerObs = null;
  document.documentElement.style.removeProperty('--header-height');
});

const appClass = computed(() => ({
  'has-sidebar': true,
  'sidebar-collapsed': collapsed.value,
  'mobile-sidebar-open': mobileOpen.value,
}));

// --- Server Info modal (ported from legacy shared-nav.js showInfo) ---
// The legacy sidebar footer carried a "Server Info" item that opened a modal
// with the dashboard URL/host/port/protocol/version, a QR encoding the current
// origin, and a pair-with-code widget. The v3 shell's footer only showed a
// version tag, so the modal was missing on every migrated page. This restores
// it with the same behavior and endpoints (/api/version, /api/rendezvous,
// /api/announce-now, {rendezvous}/pair) and reuses the shared QR lib.
const infoOpen = ref(false);
const serverVersion = ref('unknown');
const serverLabel = ref('');
const rendezvousURL = ref('');
const lanWarnHost = ref(''); // non-empty → dashboard URL is LAN-only; warn
const pairCode = ref('');
const pairMsg = ref('');
const pairMsgColor = ref('#666');
const pairBusy = ref(false);
const qrCanvas = ref<HTMLCanvasElement | null>(null);

const infoUrl = computed(() => (typeof window !== 'undefined' ? window.location.origin : ''));
const infoHost = computed(() => (typeof window !== 'undefined' ? window.location.hostname : ''));
const infoPort = computed(() => (typeof window !== 'undefined' ? window.location.port || '80' : ''));
const infoProto = computed(() => (typeof window !== 'undefined' ? window.location.protocol : ''));

let qrLibPromise: Promise<unknown> | null = null;
function loadQrLib(): Promise<unknown> {
  const w = window as unknown as { qrcode?: unknown };
  if (typeof w.qrcode === 'function') return Promise.resolve(w.qrcode);
  if (qrLibPromise) return qrLibPromise;
  qrLibPromise = new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = rewriteHrefForDev('/shared/qrcode.min.js');
    s.onload = () => resolve((window as unknown as { qrcode?: unknown }).qrcode);
    s.onerror = () => reject(new Error('failed to load qrcode.min.js'));
    document.head.appendChild(s);
  });
  return qrLibPromise;
}

function renderQr(canvas: HTMLCanvasElement, text: string, sizePx: number) {
  // qrcode-generator: typeNumber=0 = auto, error-correction 'M'.
  const qrcode = (window as unknown as { qrcode: (t: number, e: string) => any }).qrcode;
  const qr = qrcode(0, 'M');
  qr.addData(text);
  qr.make();
  const moduleCount: number = qr.getModuleCount();
  const cell = Math.floor(sizePx / moduleCount);
  const dim = cell * moduleCount;
  canvas.width = dim;
  canvas.height = dim;
  canvas.style.width = `${dim}px`;
  canvas.style.height = `${dim}px`;
  const ctx = canvas.getContext('2d');
  if (!ctx) return;
  ctx.fillStyle = '#fff';
  ctx.fillRect(0, 0, dim, dim);
  ctx.fillStyle = '#000';
  for (let r = 0; r < moduleCount; r++) {
    for (let c = 0; c < moduleCount; c++) {
      if (qr.isDark(r, c)) ctx.fillRect(c * cell, r * cell, cell, cell);
    }
  }
}

// LAN-only heuristic: .local / loopback / RFC1918 / link-local hosts can't be
// reached by a TV on another network, so code-pairing would silently fail.
function computeLanWarn(urlString: string): string {
  let host = '';
  try {
    host = new URL(urlString).hostname.toLowerCase();
  } catch {
    return '';
  }
  const lanOnly =
    host === 'localhost' ||
    host.endsWith('.local') ||
    /^127\./.test(host) ||
    /^10\./.test(host) ||
    /^192\.168\./.test(host) ||
    /^172\.(1[6-9]|2\d|3[0-1])\./.test(host) ||
    /^169\.254\./.test(host) ||
    host === '::1' ||
    host.startsWith('fe80:') ||
    host.startsWith('fc') ||
    host.startsWith('fd');
  return lanOnly ? host : '';
}

async function openServerInfo() {
  infoOpen.value = true;
  pairCode.value = '';
  pairMsg.value = '';
  pairBusy.value = false;
  serverLabel.value = '';
  rendezvousURL.value = '';
  lanWarnHost.value = '';
  const url = infoUrl.value;
  // Fire-and-forget re-announce so opening the panel recovers a missed boot
  // announce. Server-side coalesces simultaneous triggers, so this is safe.
  fetch('/api/announce-now', { method: 'POST' }).catch(() => {});
  try {
    const r = await fetch('/api/version');
    serverVersion.value = r.ok
      ? String((await r.json()).version || '').trim() || 'unknown'
      : 'unknown';
  } catch {
    serverVersion.value = 'unknown';
  }
  // Render the QR once the canvas is in the DOM.
  await nextTick();
  try {
    await loadQrLib();
    if (qrCanvas.value) renderQr(qrCanvas.value, url, 180);
  } catch {
    const c = qrCanvas.value;
    const ctx = c?.getContext('2d');
    if (c && ctx) {
      c.width = 180;
      c.height = 180;
      ctx.fillStyle = '#f5f5f5';
      ctx.fillRect(0, 0, 180, 180);
      ctx.fillStyle = '#999';
      ctx.font = '12px sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('QR unavailable', 90, 90);
    }
  }
  // Label + pair widget come from the rendezvous config.
  try {
    const r = await fetch('/api/rendezvous');
    const rz = r.ok ? await r.json() : {};
    serverLabel.value = (rz && rz.label) || '';
    rendezvousURL.value = (rz && rz.url) || '';
    if (rendezvousURL.value) lanWarnHost.value = computeLanWarn(url);
  } catch {
    rendezvousURL.value = '';
  }
}

function closeServerInfo() {
  infoOpen.value = false;
}

async function submitPair() {
  const code = (pairCode.value || '').trim().toUpperCase();
  if (!/^[A-Z0-9]{4,12}$/.test(code)) {
    pairMsgColor.value = '#991b1b';
    pairMsg.value = 'Code must be 4–12 alphanumeric characters.';
    return;
  }
  pairBusy.value = true;
  pairMsgColor.value = '#666';
  pairMsg.value = '';
  try {
    const endpoint = `${rendezvousURL.value.replace(/\/$/, '')}/pair?code=${encodeURIComponent(code)}`;
    const res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ server_url: infoUrl.value }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.ok) {
      pairMsgColor.value = '#065f46';
      pairMsg.value = 'Paired. Your TV should pick this up within a couple of seconds.';
    } else {
      pairMsgColor.value = '#991b1b';
      pairMsg.value = body.error || `HTTP ${res.status}`;
    }
  } catch (e) {
    pairMsgColor.value = '#991b1b';
    pairMsg.value = e instanceof Error ? e.message : 'Network error';
  } finally {
    pairBusy.value = false;
  }
}

// --- Access restrictions (ported from shared-nav.js) ---
// Hide content-management (Upload / Sources / Jobs) and the Monitor item when
// the dashboard is reached over a non-internal (public) host. This is a soft
// UI hide matching the legacy nav — real auth is the optional htpasswd.
function isInternalNetworkHost(hostname: string): boolean {
  const host = (hostname || '').toLowerCase();
  if (!host) return false;
  if (host === 'localhost' || host === '::1' || host.startsWith('127.')) return true;
  if (host.endsWith('.local')) return true;
  if (!host.includes('.')) return true; // bare single-label hostname
  if (/^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
  if (/^192\.168\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
  if (/^172\.(1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3}$/.test(host)) return true;
  if (/^100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}$/.test(host)) return true; // Tailscale CGNAT
  return false;
}
const restrictContent = computed(
  () => typeof window !== 'undefined' && !isInternalNetworkHost(window.location.hostname),
);
const visibleSections = computed<NavSection[]>(() =>
  sections
    .filter((s) => !(restrictContent.value && s.title === 'CONTENT'))
    .map((s) =>
      restrictContent.value && s.title === 'LIVE STREAMING'
        ? { ...s, items: s.items.filter((i) => i.id !== 'monitor') }
        : s,
    )
    .filter((s) => s.items.length > 0),
);

// --- First-run setup experience (ported from shared-nav.js) ---
interface SetupStatus {
  root?: string;
  root_mounted?: boolean;
  root_writable?: boolean;
  content_count?: number;
  sources_count?: number;
  outputs_count?: number;
  content_empty?: boolean;
  initialized?: boolean;
  issues?: string[];
  recommendations?: string[];
}
const SETUP_PAGES_REQUIRE_CONTENT = new Set(['playback', 'testing', 'quartet', 'grid', 'segment-duration']);
const setup = ref<SetupStatus | null>(null);
const setupModalOpen = ref(false);
const setupSeedState = ref<'idle' | 'seeding' | 'seeded' | 'failed'>('idle');
const setupRedirectMsg = ref('');
let setupRedirectTimer: ReturnType<typeof setTimeout> | null = null;

const setupHasIssues = computed(() => !!setup.value?.issues?.length);
const setupShowContentActions = computed(
  () =>
    !!setup.value?.content_empty &&
    SETUP_PAGES_REQUIRE_CONTENT.has(props.activePage) &&
    props.activePage !== 'upload',
);
const setupRoot = computed(() => setup.value?.root || '/media');
const setupDiagnostics = computed(() => {
  const s = setup.value;
  if (!s) return 'No diagnostics available.';
  const lines = [
    `Root: ${s.root ?? '(unknown)'}`,
    `Mounted: ${s.root_mounted ? 'yes' : 'no'}`,
    `Writable: ${s.root_writable ? 'yes' : 'no'}`,
    `Content items: ${s.content_count ?? 0}`,
    `Source files: ${s.sources_count ?? 0}`,
    `Output dirs: ${s.outputs_count ?? 0}`,
  ];
  if (s.issues?.length) lines.push(`Issues: ${s.issues.join(', ')}`);
  return lines.join('\n');
});

async function loadSetup() {
  try {
    const r = await fetch('/api/setup');
    if (!r.ok) return;
    setup.value = await r.json();
    maybeAutoRedirect();
    maybeShowSetupModal();
  } catch {
    /* setup check is best-effort */
  }
}
async function setupSeedSample() {
  setupSeedState.value = 'seeding';
  try {
    await fetch('/api/setup/seed', { method: 'POST' });
    setupSeedState.value = 'seeded';
  } catch {
    setupSeedState.value = 'failed';
  }
}
async function setupMarkInitialized() {
  try {
    await fetch('/api/setup/initialize', { method: 'POST' });
  } catch {
    /* ignore */
  }
  setupModalOpen.value = false;
  await loadSetup();
}
function setupGoUpload() {
  window.location.href = '/dashboard/upload.html';
}
function maybeShowSetupModal() {
  if (!setup.value || setup.value.initialized) return;
  if (sessionStorage.getItem('ismSetupModalShown') === '1') return;
  sessionStorage.setItem('ismSetupModalShown', '1');
  setupModalOpen.value = true;
}
function maybeAutoRedirect() {
  if (!setup.value?.content_empty) return;
  if (!SETUP_PAGES_REQUIRE_CONTENT.has(props.activePage) || props.activePage === 'upload') return;
  if (sessionStorage.getItem('ismSkipUploadRedirect') === '1') return;
  let secs = 5;
  const tick = () => {
    if (secs <= 0) {
      window.location.href = '/dashboard/upload.html';
      return;
    }
    setupRedirectMsg.value = `No content detected. Redirecting to Upload in ${secs}s…`;
    secs -= 1;
    setupRedirectTimer = setTimeout(tick, 1000);
  };
  tick();
}
function cancelSetupRedirect() {
  if (setupRedirectTimer) {
    clearTimeout(setupRedirectTimer);
    setupRedirectTimer = null;
  }
  sessionStorage.setItem('ismSkipUploadRedirect', '1');
  setupRedirectMsg.value = 'Auto-redirect canceled.';
}

// --- Global active-job progress badge (ported from shared-nav.js) ---
// Polls /api/jobs and surfaces the first uploading/encoding job as a floating
// badge on every v3 page (legacy showed it via shared-nav). The legacy
// SharedWorker live-upload feed is omitted; 2s polling covers the core.
interface ActiveJob {
  job_id: string;
  status: string;
  progress?: number;
  name?: string;
}
const activeJob = ref<ActiveJob | null>(null);
const progressDismissed = ref(false);
let jobsTimer: ReturnType<typeof setInterval> | null = null;

async function checkActiveJobs() {
  try {
    const r = await fetch('/api/jobs');
    if (!r.ok) return;
    const data = await r.json();
    const job = (data.jobs || []).find(
      (j: ActiveJob) => j.status === 'uploading' || j.status === 'encoding',
    );
    if (!job) progressDismissed.value = false; // re-arm for the next job
    activeJob.value = job || null;
  } catch {
    /* polling is best-effort */
  }
}
const progressTitle = computed(() =>
  activeJob.value?.status === 'uploading' ? 'Uploading…' : 'Encoding…',
);
const progressPct = computed(() => Math.round(activeJob.value?.progress ?? 0));
function viewActiveJob() {
  if (activeJob.value) {
    window.location.href = `/dashboard/job-detail.html?id=${activeJob.value.job_id}`;
  }
}

// --- Version notice: "what's new in this build" + "newer release available" ---
// Two jobs: surface what shipped in the running build (once per upgrade, no
// network), and nudge when a newer release exists (best-effort GitHub check).
const REPO_SLUG = 'jonathaneoliver/infinite-streaming';
const runningVersion = ref('');
const latestVersion = ref('');
const whatsNewDismissed = ref(false);
const upgradeDismissed = ref(false);

function normVer(v: string): string {
  return (v || '').trim().replace(/^v/i, '');
}
// -1 if a<b, 1 if a>b, 0 if equal OR unparseable (unparseable → no upgrade nudge).
function cmpSemver(a: string, b: string): number {
  const pa = normVer(a).split('.').map((n) => parseInt(n, 10));
  const pb = normVer(b).split('.').map((n) => parseInt(n, 10));
  if (pa.some(Number.isNaN) || pb.some(Number.isNaN)) return 0;
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const x = pa[i] || 0;
    const y = pb[i] || 0;
    if (x !== y) return x < y ? -1 : 1;
  }
  return 0;
}

async function loadVersionInfo() {
  try {
    const r = await fetch('/api/version');
    if (r.ok) runningVersion.value = String((await r.json()).version || '').trim();
  } catch {
    /* best-effort */
  }
  if (!runningVersion.value) return;
  // Latest release tag, cached ~6h. GitHub's API is CORS-enabled for public
  // repos; any failure (offline / rate-limited / air-gapped) is silent so the
  // upgrade nudge simply never shows.
  const TTL = 6 * 60 * 60 * 1000;
  try {
    const cached = JSON.parse(localStorage.getItem('ismLatestRelease') || 'null') as {
      tag: string;
      ts: number;
    } | null;
    if (cached?.tag && Date.now() - cached.ts < TTL) {
      latestVersion.value = cached.tag;
      return;
    }
    const r = await fetch(`https://api.github.com/repos/${REPO_SLUG}/releases/latest`);
    if (r.ok) {
      const tag = String((await r.json()).tag_name || '').trim();
      if (tag) {
        latestVersion.value = tag;
        localStorage.setItem('ismLatestRelease', JSON.stringify({ tag, ts: Date.now() }));
      }
    }
  } catch {
    /* upgrade check is best-effort */
  }
}

const runningTag = computed(() => `v${normVer(runningVersion.value)}`);
const whatsNewVisible = computed(
  () =>
    !!runningVersion.value &&
    !whatsNewDismissed.value &&
    localStorage.getItem('ismWhatsNewSeen') !== runningVersion.value,
);
const upgradeVisible = computed(
  () =>
    !!runningVersion.value &&
    !!latestVersion.value &&
    !upgradeDismissed.value &&
    cmpSemver(runningVersion.value, latestVersion.value) < 0 &&
    localStorage.getItem('ismUpgradeDismissed') !== latestVersion.value,
);
const whatsNewUrl = computed(() => `https://github.com/${REPO_SLUG}/releases/tag/${runningTag.value}`);
const latestUrl = computed(() => `https://github.com/${REPO_SLUG}/releases/latest`);

function dismissWhatsNew() {
  // localStorage is already marked on first impression (watcher
  // below); the × button only needs to hide the banner in this
  // window. Set is harmless / idempotent so we keep it.
  localStorage.setItem('ismWhatsNewSeen', runningVersion.value);
  whatsNewDismissed.value = true;
}

// Auto-mark the "what's new" banner as seen on first display so it
// doesn't reappear in every new tab/window until manually dismissed.
// User can still click × in the current window to hide it; but
// localStorage is set the moment the banner becomes visible, which
// means the very next tab/window check passes and the banner stays
// hidden until the next deploy bumps `runningVersion`. The computed
// `whatsNewVisible` doesn't re-evaluate on localStorage writes (it
// reads localStorage non-reactively), so the banner remains visible
// in THIS window until the user explicitly dismisses or navigates
// away.
watch(whatsNewVisible, (visible) => {
  if (visible && runningVersion.value) {
    localStorage.setItem('ismWhatsNewSeen', runningVersion.value);
  }
});
function dismissUpgrade() {
  localStorage.setItem('ismUpgradeDismissed', latestVersion.value);
  upgradeDismissed.value = true;
}

onMounted(() => {
  loadSetup();
  checkActiveJobs();
  jobsTimer = setInterval(checkActiveJobs, 2000);
  loadVersionInfo();
});
onBeforeUnmount(() => {
  if (jobsTimer) clearInterval(jobsTimer);
  if (setupRedirectTimer) clearTimeout(setupRedirectTimer);
});
</script>

<template>
  <div class="ism-app" :class="appClass">
    <aside class="ism-sidebar" @click="onSidebarClickPeek">
      <div class="ism-sidebar-header">
        <div class="ism-logo">
          <span class="ism-logo-icon">
            <svg xmlns="http://www.w3.org/2000/svg" width="28" height="28" viewBox="0 0 48 48">
              <rect width="48" height="48" rx="10" fill="#091929"/>
              <path d="M24,24 C28,17 34,12 38,15 C42,18 44,22 44,24 C44,26 42,30 38,33 C34,36 28,31 24,24 Z" fill="none" stroke="#0077B6" stroke-width="7.5" stroke-linecap="round" stroke-linejoin="round"/>
              <path d="M24,24 C20,17 14,12 10,15 C6,18 4,22 4,24 C4,26 6,30 10,33 C14,36 20,31 24,24 Z" fill="none" stroke="#0077B6" stroke-width="7.5" stroke-linecap="round" stroke-linejoin="round"/>
              <path d="M24,24 C28,17 34,12 38,15 C42,18 44,22 44,24 C44,26 42,30 38,33 C34,36 28,31 24,24 Z" fill="none" stroke="#00B4D8" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/>
              <path d="M24,24 C20,17 14,12 10,15 C6,18 4,22 4,24 C4,26 6,30 10,33 C14,36 20,31 24,24 Z" fill="none" stroke="#00B4D8" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/>
              <circle cx="24" cy="24" r="4" fill="#0077B6"/>
              <circle cx="24" cy="24" r="2.2" fill="#48CAE4"/>
            </svg>
          </span>
          <span class="ism-logo-text">InfiniteStream</span>
        </div>
        <button class="collapse-btn" type="button" @click.stop="toggleCollapse" :title="collapsed ? 'Expand sidebar' : 'Collapse sidebar'">
          {{ collapsed ? '⇥' : '⇤' }}
        </button>
      </div>

      <nav class="ism-sidebar-content">
        <div v-for="section in visibleSections" :key="section.title" class="nav-section">
          <div class="nav-section-title">{{ section.title }}</div>
          <a
            v-for="item in section.items"
            :key="item.id"
            :href="rewriteHrefForDev(item.href)"
            class="nav-item"
            :class="{ active: isActive(item.id) }"
            :id="`nav-${item.id}`"
            @click="onNavClick(item, $event)"
          >
            <span class="nav-item-icon">{{ item.icon }}</span>
            <span class="nav-item-text">{{ item.text }}</span>
            <span v-if="item.warning" class="nav-item-warning">⚠️</span>
            <span v-if="item.alpha" class="nav-item-alpha">ALPHA</span>
          </a>
        </div>
      </nav>

      <div class="ism-sidebar-footer">
        <a href="#" class="nav-item" id="nav-server-info" @click.prevent="openServerInfo">
          <span class="nav-item-icon">ℹ️</span>
          <span class="nav-item-text">Server Info</span>
        </a>
        <div class="footer-meta">v3 · vue</div>
      </div>
    </aside>

    <div class="ism-main">
      <header class="ism-header">
        <div class="ism-header-left">
          <button class="mobile-toggle" type="button" @click="toggleMobile" aria-label="Toggle navigation">☰</button>
          <div class="ism-header-title">InfiniteStream</div>
        </div>
        <div class="ism-header-center">
          <slot name="header-center" />
        </div>
        <div class="ism-header-right">
          <slot name="header-right" />
        </div>
      </header>

      <main class="ism-content">
        <div v-if="whatsNewVisible" class="ism-version-banner whatsnew">
          <div class="ism-version-icon">🎉</div>
          <div class="ism-version-body">
            <strong>You're now running {{ runningTag }}.</strong>
            New in this release: an in-dashboard AI chat bot for your sessions and Claude Code CLI
            session querying.
            <a :href="whatsNewUrl" target="_blank" rel="noopener" class="ism-version-link">What's new →</a>
          </div>
          <button class="ism-version-dismiss" type="button" aria-label="Dismiss" @click="dismissWhatsNew">&times;</button>
        </div>
        <div v-if="upgradeVisible" class="ism-version-banner upgrade">
          <div class="ism-version-icon">⬆️</div>
          <div class="ism-version-body">
            <strong>{{ latestVersion }} is available</strong> — you're on {{ runningTag }}.
            <a :href="latestUrl" target="_blank" rel="noopener" class="ism-version-link">View release →</a>
          </div>
          <button class="ism-version-dismiss" type="button" aria-label="Dismiss" @click="dismissUpgrade">&times;</button>
        </div>
        <div v-if="setupHasIssues" class="ism-setup-banner">
          <div class="ism-setup-icon">⚠️</div>
          <div class="ism-setup-body">
            <div class="ism-setup-title">Setup attention needed</div>
            <div v-for="issue in setup?.issues || []" :key="issue" class="ism-setup-issue">{{ issue }}</div>
            <ul v-if="setup?.recommendations?.length" class="ism-setup-recs">
              <li v-for="rec in setup?.recommendations || []" :key="rec">{{ rec }}</li>
            </ul>
            <div class="ism-setup-actions">
              <button class="ism-btn-sm" type="button" @click="loadSetup">Run Diagnostics</button>
              <button class="ism-btn-sm" type="button" @click="setupModalOpen = true">Open Setup Guide</button>
              <button v-if="!setup?.initialized" class="ism-btn-sm" type="button" @click="setupMarkInitialized">Mark Setup Complete</button>
              <button v-if="setupShowContentActions" class="ism-btn-sm primary" type="button" @click="setupGoUpload">Go to Upload</button>
              <button
                v-if="setupShowContentActions"
                class="ism-btn-sm"
                type="button"
                :disabled="setupSeedState === 'seeding'"
                @click="setupSeedSample"
              >
                {{ setupSeedState === 'seeded' ? 'Seeded' : setupSeedState === 'seeding' ? 'Seeding…' : setupSeedState === 'failed' ? 'Seed Failed' : 'Seed Sample Content' }}
              </button>
            </div>
            <div v-if="setupRedirectMsg" class="ism-setup-redirect">
              {{ setupRedirectMsg }}
              <button v-if="!setupRedirectMsg.includes('canceled')" class="ism-btn-sm" type="button" @click="cancelSetupRedirect">Cancel</button>
            </div>
          </div>
        </div>
        <slot />
      </main>
    </div>

    <Teleport to="body">
      <div v-if="infoOpen" class="ism-info-overlay" @click.self="closeServerInfo">
        <div class="ism-info-panel">
          <div class="ism-info-head">
            <h2>{{ serverLabel ? `Server Info — ${serverLabel}` : 'Server Info' }}</h2>
            <button class="ism-info-close" type="button" @click="closeServerInfo">&times;</button>
          </div>
          <div class="ism-info-body">
            <div class="ism-info-fields">
              <div v-if="serverLabel"><strong>Name:</strong> {{ serverLabel }}</div>
              <div><strong>URL:</strong> <code class="ism-info-sel">{{ infoUrl }}</code></div>
              <div><strong>Host:</strong> {{ infoHost || '(none)' }}</div>
              <div><strong>Port:</strong> {{ infoPort }}</div>
              <div><strong>Protocol:</strong> {{ infoProto }}</div>
              <div><strong>Version:</strong> {{ serverVersion }}</div>
            </div>
            <div class="ism-info-qr">
              <canvas ref="qrCanvas"></canvas>
              <div class="ism-info-qr-cap">Scan from a phone or TV app<br />to pair with this server</div>
            </div>
          </div>
          <p class="ism-info-note">
            The QR encodes the URL you used to reach this dashboard. Devices that scan it must be
            able to reach the same URL (same LAN, Tailscale, public DNS, etc.).
          </p>
          <hr class="ism-info-hr" />
          <div class="ism-info-discovery">
            <strong>Cloud discovery (no code needed)</strong><br />
            This server is announcing itself to the pairing rendezvous (Cloudflare Worker). TVs and
            phones whose <em>public IP matches this server's public IP</em> — i.e. on the same WAN —
            will see this server in their app's "+ Add server" / "Pair…" screen automatically. Just
            tap to add. Opening this panel triggers a fresh announce, so the server should be visible
            within a few seconds.
          </div>
          <div class="ism-pair">
            <h3>Pair with code</h3>
            <p class="ism-info-note">
              Enter the 6-character code shown on the TV here; the TV will receive
              <em>this dashboard's URL</em> via the rendezvous and connect.
            </p>
            <div v-if="lanWarnHost" class="ism-pair-warn">
              <strong>Heads up:</strong> the URL above looks LAN-only (<code>{{ lanWarnHost }}</code>).
              Pairing will only work if the TV can resolve and reach that URL on its network. For
              cross-network pairing (cellular, VPN, etc.) you'd need a publicly reachable URL — port
              forward, Tailscale MagicDNS name, a tunnel, etc.
            </div>
            <div v-if="rendezvousURL" class="ism-pair-form">
              <div class="ism-pair-row">
                <input
                  v-model="pairCode"
                  type="text"
                  placeholder="ABC123"
                  maxlength="12"
                  class="ism-pair-input"
                  @input="pairCode = pairCode.toUpperCase()"
                  @keyup.enter="submitPair"
                />
                <button class="ism-pair-btn" type="button" :disabled="pairBusy" @click="submitPair">
                  {{ pairBusy ? 'Pairing…' : 'Pair' }}
                </button>
              </div>
              <div class="ism-pair-msg" :style="{ color: pairMsgColor }">{{ pairMsg }}</div>
            </div>
            <div v-else class="ism-pair-unconfigured">
              Pairing rendezvous URL not configured. Set
              <code>INFINITE_STREAM_RENDEZVOUS_URL</code> on the server to enable. See
              <a
                href="https://github.com/jonathaneoliver/infinite-streaming/tree/main/cloudflare/pair-rendezvous"
                target="_blank"
                rel="noopener"
                >cloudflare/pair-rendezvous/</a
              >.
            </div>
          </div>
        </div>
      </div>
    </Teleport>

    <Teleport to="body">
      <div
        v-if="activeJob && !progressDismissed"
        class="ism-progress-badge"
        :class="{ encoding: activeJob.status === 'encoding' }"
        @click="viewActiveJob"
      >
        <div class="ism-progress-head">
          <span class="ism-progress-title">{{ progressTitle }}</span>
          <button class="ism-progress-dismiss" type="button" @click.stop="progressDismissed = true">&times;</button>
        </div>
        <div class="ism-progress-name">{{ activeJob.name || 'Processing…' }}</div>
        <div class="ism-progress-track"><div class="ism-progress-fill" :style="{ width: progressPct + '%' }"></div></div>
        <div class="ism-progress-pct">{{ progressPct }}%</div>
      </div>
    </Teleport>

    <Teleport to="body">
      <div v-if="setupModalOpen" class="ism-info-overlay" @click.self="setupModalOpen = false">
        <div class="ism-info-panel">
          <div class="ism-info-head">
            <h2>First-Run Setup</h2>
            <button class="ism-info-close" type="button" @click="setupModalOpen = false">&times;</button>
          </div>
          <ol class="ism-setup-steps">
            <li><strong>Mount a host folder</strong> to <code>{{ setupRoot }}</code>.</li>
            <li><strong>Upload content</strong> or seed a sample clip.</li>
            <li><strong>Open Mosaic</strong> to preview streams.</li>
          </ol>
          <div class="ism-setup-snippet">
            <div class="ism-info-note">Docker Compose example</div>
            <pre>services:
  infinite-streaming:
    volumes:
      - /path/to/InfiniteStream:{{ setupRoot }}</pre>
          </div>
          <div class="ism-setup-snippet">
            <div class="ism-info-note">Diagnostics</div>
            <pre>{{ setupDiagnostics }}</pre>
          </div>
          <div class="ism-setup-modal-footer">
            <button class="ism-btn-sm" type="button" :disabled="setupSeedState === 'seeding'" @click="setupSeedSample">
              {{ setupSeedState === 'seeded' ? 'Seeded' : setupSeedState === 'seeding' ? 'Seeding…' : 'Seed Sample Content' }}
            </button>
            <button class="ism-btn-sm" type="button" @click="setupGoUpload">Open Upload</button>
            <button class="ism-btn-sm primary" type="button" @click="setupMarkInitialized">Mark Setup Complete</button>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.ism-app {
  display: flex;
  min-height: 100vh;
  background: #f8f9fa;
  font-family: 'Google Sans', 'Roboto', -apple-system, BlinkMacSystemFont, 'Segoe UI', Arial, sans-serif;
  color: #202124;
}

.ism-sidebar {
  width: 240px;
  background: #ffffff;
  border-right: 1px solid #e8eaed;
  display: flex;
  flex-direction: column;
  position: sticky;
  top: 0;
  height: 100vh;
  flex-shrink: 0;
  transition: width 0.15s ease;
  z-index: 100;
}
.sidebar-collapsed .ism-sidebar {
  width: 56px;
  cursor: pointer;
}

.ism-sidebar-header {
  min-height: 64px;
  border-bottom: 1px solid #e8eaed;
  padding: 0 16px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.ism-logo {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  flex: 1;
}
.ism-logo-icon { display: inline-flex; flex-shrink: 0; }
.ism-logo-text {
  font-size: 16px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sidebar-collapsed .ism-logo-text { display: none; }

.collapse-btn {
  background: transparent;
  border: none;
  color: #5f6368;
  font-size: 16px;
  cursor: pointer;
  padding: 4px 6px;
  border-radius: 4px;
  flex-shrink: 0;
}
.collapse-btn:hover { background: #f1f3f4; }
.sidebar-collapsed .collapse-btn { display: none; }

.ism-sidebar-content {
  flex: 1;
  overflow-y: auto;
  padding: 12px 8px;
}
.nav-section { margin-bottom: 16px; }
.nav-section-title {
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.6px;
  color: #5f6368;
  padding: 8px 12px 4px;
}
.sidebar-collapsed .nav-section-title { display: none; }

.nav-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 12px;
  border-radius: 6px;
  font-size: 13px;
  color: #202124;
  text-decoration: none;
  transition: background 0.1s ease;
  white-space: nowrap;
  overflow: hidden;
}
.nav-item:hover { background: #f1f3f4; }
.nav-item.active {
  background: #e8f0fe;
  color: #1a73e8;
  font-weight: 600;
}
.nav-item-icon {
  font-size: 16px;
  width: 20px;
  text-align: center;
  flex-shrink: 0;
}
.nav-item-text {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sidebar-collapsed .nav-item-text,
.sidebar-collapsed .nav-item-warning,
.sidebar-collapsed .nav-item-alpha { display: none; }

.nav-item-warning { font-size: 12px; }
.nav-item-alpha {
  background: #fef7e0;
  color: #b06000;
  font-size: 9px;
  font-weight: 700;
  padding: 1px 6px;
  border-radius: 8px;
  letter-spacing: 0.5px;
}

.ism-sidebar-footer {
  border-top: 1px solid #e8eaed;
  padding: 12px 16px;
  font-size: 11px;
  color: #9aa0a6;
}
.sidebar-collapsed .footer-meta { display: none; }

/* Server Info modal (Teleported to body; scoped styles still apply via the
   component's data attribute). */
.ism-info-overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.6);
  z-index: 10000;
  display: flex;
  align-items: center;
  justify-content: center;
}
.ism-info-panel {
  background: #fff;
  border-radius: 12px;
  padding: 28px;
  max-width: 520px;
  width: 90%;
  max-height: 90vh;
  overflow: auto;
  box-shadow: 0 10px 40px rgba(0, 0, 0, 0.3);
  color: #202124;
  font-size: 14px;
  line-height: 1.4;
}
.ism-info-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 16px;
}
.ism-info-head h2 { margin: 0; font-size: 1.4em; }
.ism-info-close {
  background: none;
  border: none;
  font-size: 1.6em;
  line-height: 1;
  cursor: pointer;
  color: #5f6368;
}
.ism-info-body { display: flex; gap: 24px; align-items: flex-start; flex-wrap: wrap; }
.ism-info-fields { flex: 1; min-width: 200px; }
.ism-info-fields > div { margin-bottom: 8px; }
.ism-info-sel { user-select: all; }
.ism-info-qr { text-align: center; }
.ism-info-qr canvas {
  background: #fff;
  padding: 8px;
  border: 1px solid #ddd;
  border-radius: 4px;
}
.ism-info-qr-cap { font-size: 0.8em; color: #666; margin-top: 6px; }
.ism-info-note { margin-top: 20px; font-size: 0.85em; color: #666; }
.ism-info-hr { margin: 20px 0; border: none; border-top: 1px solid #eee; }
.ism-info-discovery {
  margin-bottom: 18px;
  padding: 10px 12px;
  background: #ecfdf5;
  border-left: 3px solid #10b981;
  border-radius: 4px;
  font-size: 0.85em;
  color: #065f46;
}
.ism-pair h3 { margin: 0 0 8px 0; font-size: 1em; }
.ism-pair-warn {
  margin: 0 0 12px 0;
  padding: 8px 10px;
  background: #fef3c7;
  border-left: 3px solid #f59e0b;
  border-radius: 4px;
  font-size: 0.8em;
  color: #78350f;
}
.ism-pair-row { display: flex; gap: 8px; align-items: center; }
.ism-pair-input {
  flex: 1;
  padding: 8px 10px;
  font-family: ui-monospace, monospace;
  text-transform: uppercase;
  letter-spacing: 3px;
  border: 1px solid #ccc;
  border-radius: 6px;
  font-size: 1em;
}
.ism-pair-btn {
  padding: 8px 16px;
  background: #2563eb;
  color: #fff;
  border: 0;
  border-radius: 6px;
  cursor: pointer;
  font-size: 0.9em;
}
.ism-pair-btn:disabled { opacity: 0.6; cursor: default; }
.ism-pair-msg { margin-top: 10px; font-size: 0.85em; }
.ism-pair-unconfigured { font-size: 0.85em; color: #888; font-style: italic; }

/* Version notice banners (what's-new + upgrade) */
.ism-version-banner {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin: 0 0 16px 0;
  padding: 12px 16px;
  border-radius: 8px;
  font-size: 13px;
}
.ism-version-banner.whatsnew {
  background: #ecfdf5;
  border: 1px solid #a7f3d0;
  border-left: 3px solid #10b981;
  color: #065f46;
}
.ism-version-banner.upgrade {
  background: #eff6ff;
  border: 1px solid #bfdbfe;
  border-left: 3px solid #3b82f6;
  color: #1e3a8a;
}
.ism-version-icon { font-size: 18px; line-height: 1.2; }
.ism-version-body { flex: 1; min-width: 0; }
.ism-version-link { font-weight: 600; white-space: nowrap; margin-left: 6px; }
.ism-version-banner.whatsnew .ism-version-link { color: #047857; }
.ism-version-banner.upgrade .ism-version-link { color: #1d4ed8; }
.ism-version-dismiss {
  background: none;
  border: none;
  font-size: 18px;
  line-height: 1;
  cursor: pointer;
  color: inherit;
  opacity: 0.6;
}
.ism-version-dismiss:hover { opacity: 1; }

/* First-run setup banner */
.ism-setup-banner {
  display: flex;
  gap: 12px;
  margin: 0 0 16px 0;
  padding: 14px 16px;
  background: #fef3c7;
  border: 1px solid #fcd34d;
  border-left: 3px solid #f59e0b;
  border-radius: 8px;
  color: #78350f;
  font-size: 13px;
}
.ism-setup-icon { font-size: 18px; line-height: 1; }
.ism-setup-body { flex: 1; min-width: 0; }
.ism-setup-title { font-weight: 600; margin-bottom: 6px; }
.ism-setup-issue { margin-bottom: 2px; }
.ism-setup-recs { margin: 6px 0; padding-left: 18px; }
.ism-setup-actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; }
.ism-setup-redirect { margin-top: 10px; font-size: 12px; display: flex; align-items: center; gap: 8px; }

.ism-btn-sm {
  padding: 5px 12px;
  font-size: 12px;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  background: #fff;
  color: #374151;
  cursor: pointer;
}
.ism-btn-sm:hover { background: #f3f4f6; }
.ism-btn-sm:disabled { opacity: 0.6; cursor: default; }
.ism-btn-sm.primary { background: #2563eb; border-color: #2563eb; color: #fff; }
.ism-btn-sm.primary:hover { background: #1d4ed8; }

/* First-run setup modal extras (reuses .ism-info-overlay / .ism-info-panel) */
.ism-setup-steps { margin: 0 0 16px 0; padding-left: 20px; }
.ism-setup-steps li { margin-bottom: 6px; }
.ism-setup-snippet { margin-bottom: 14px; }
.ism-setup-snippet pre {
  margin: 4px 0 0 0;
  padding: 10px 12px;
  background: #f6f8fa;
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  font-size: 12px;
  overflow: auto;
  white-space: pre;
}
.ism-setup-modal-footer { display: flex; flex-wrap: wrap; gap: 8px; justify-content: flex-end; }

/* Global active-job progress badge */
.ism-progress-badge {
  position: fixed;
  right: 20px;
  bottom: 20px;
  z-index: 9999;
  width: 240px;
  padding: 12px 14px;
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.18);
  cursor: pointer;
  font-size: 13px;
}
.ism-progress-badge.encoding { border-left: 3px solid #f59e0b; }
.ism-progress-head { display: flex; justify-content: space-between; align-items: center; }
.ism-progress-title { font-weight: 600; }
.ism-progress-dismiss { background: none; border: none; font-size: 16px; line-height: 1; cursor: pointer; color: #9aa0a6; }
.ism-progress-name { color: #5f6368; margin: 4px 0 8px 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ism-progress-track { height: 6px; background: #eef0f2; border-radius: 3px; overflow: hidden; }
.ism-progress-fill { height: 100%; background: #2563eb; transition: width 0.3s ease; }
.ism-progress-badge.encoding .ism-progress-fill { background: #f59e0b; }
.ism-progress-pct { text-align: right; font-size: 11px; color: #5f6368; margin-top: 4px; }

.ism-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  /* Reserve space for the floating ChatPanel dock (when present)
     so widening the panel reflows the page instead of covering
     the chart content. ChatPanel writes its current width into
     --chat-panel-width on :root (see ChatPanel.vue's onMounted
     / resize handler); default 0 means no panel mounted. */
  padding-right: var(--chat-panel-width, 0px);
  transition: padding-right 60ms ease-out;
}

.ism-header {
  height: 64px;
  background: #ffffff;
  border-bottom: 1px solid #e8eaed;
  display: flex;
  align-items: center;
  padding: 0 24px;
  gap: 16px;
  position: sticky;
  top: 0;
  z-index: 50;
  box-shadow: 0 1px 2px rgba(60, 64, 67, 0.05);
}
.ism-header-left {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}
.ism-header-title {
  font-size: 18px;
  font-weight: 500;
  color: #202124;
}
.ism-header-center {
  flex: 1;
  min-width: 0;
}
.ism-header-right {
  display: flex;
  align-items: center;
  gap: 12px;
}

.mobile-toggle {
  display: none;
  background: transparent;
  border: none;
  font-size: 22px;
  color: #5f6368;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 4px;
}
.mobile-toggle:hover { background: #f1f3f4; }

.ism-content {
  flex: 1;
  min-width: 0;
}

@media (max-width: 900px) {
  .ism-sidebar {
    position: fixed;
    top: 0;
    left: 0;
    transform: translateX(-100%);
    transition: transform 0.2s ease;
  }
  .mobile-sidebar-open .ism-sidebar {
    transform: translateX(0);
  }
  .mobile-toggle { display: inline-block; }
}
</style>
