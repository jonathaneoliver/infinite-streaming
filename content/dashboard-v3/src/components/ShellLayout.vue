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
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';

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

function rewriteHrefForDev(href: string): string {
  if (typeof window === 'undefined') return href;
  if (window.location.port !== DEV_PORT) return href;
  if (href.startsWith('/dashboard/v3/')) return href; // v3 stays local
  if (href.startsWith('http')) return href;            // already absolute
  return DEV_BACKEND + href;
}

// Sidebar mirrors shared/shared-nav.js. v3-native pages link to their
// v3 URLs; pages that haven't been migrated yet keep the legacy URL.
const sections: NavSection[] = [
  {
    title: 'MAIN',
    items: [
      { id: 'dashboard', icon: '📊', text: 'Dashboard', href: '/dashboard/v3/dashboard.html' },
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
      { id: 'grid',          icon: '🎮', text: 'Mosaic',          href: '/dashboard/v3/grid.html', warning: true },
      { id: 'mosaic-10ft',   icon: '📺', text: '10ft UI',         href: '/dashboard/mosaic-10ft.html', alpha: true },
      { id: 'playback',      icon: '▶️', text: 'Playback',        href: '/dashboard/playback.html' },
      { id: 'test-playback', icon: '🧭', text: 'Testing Playback',href: '/dashboard/v3/testing-session.html?nav=1' },
      { id: 'testing',       icon: '🧪', text: 'Testing Monitor', href: '/dashboard/v3/testing.html' },
      { id: 'sessions',      icon: '⏪', text: 'Sessions',         href: '/dashboard/v3/sessions.html' },
      { id: 'characterization', icon: '📈', text: 'Automated Testing', href: '/dashboard/v3/characterization.html' },
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
        <div v-for="section in sections" :key="section.title" class="nav-section">
          <div class="nav-section-title">{{ section.title }}</div>
          <a
            v-for="item in section.items"
            :key="item.id"
            :href="rewriteHrefForDev(item.href)"
            class="nav-item"
            :class="{ active: isActive(item.id) }"
            :id="`nav-${item.id}`"
          >
            <span class="nav-item-icon">{{ item.icon }}</span>
            <span class="nav-item-text">{{ item.text }}</span>
            <span v-if="item.warning" class="nav-item-warning">⚠️</span>
            <span v-if="item.alpha" class="nav-item-alpha">ALPHA</span>
          </a>
        </div>
      </nav>

      <div class="ism-sidebar-footer">
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
        <slot />
      </main>
    </div>
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
