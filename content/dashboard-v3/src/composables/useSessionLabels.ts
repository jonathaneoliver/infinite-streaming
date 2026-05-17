/**
 * useSessionLabels.ts — pure helpers for turning a PlayerRecord into
 * human-readable pill / banner labels. No reactivity, no fetches —
 * just string munging on fields the v2 API already returns.
 *
 * Used by Testing.vue's pillLabel + group-badge and by GroupBanner's
 * peer list + chip so the at-a-glance UX shows "iPad · sintel-6s-hls"
 * instead of a hex slice of a UUID. Raw UUIDs still live in
 * SessionDetails.vue for the "show me the IDs" surface.
 */
import type { PlayerRecord } from '@/repo/v2-repo';

interface DeviceRule {
  re: RegExp;
  label: string;
}

// Match order matters — first hit wins. Apple TV must come before
// generic iOS/Mac patterns because tvOS reports `AppleTV` in the UA
// but its underlying engine string also matches Mac patterns.
const DEVICE_RULES: DeviceRule[] = [
  { re: /AppleTV|tvOS/i,            label: 'Apple TV' },
  { re: /iPad/i,                    label: 'iPad' },
  { re: /iPhone/i,                  label: 'iPhone' },
  { re: /iPod/i,                    label: 'iPod' },
  { re: /Roku/i,                    label: 'Roku' },
  { re: /AFTM|FireTV|AFT[A-Z]/i,    label: 'Fire TV' },
  { re: /ExoPlayer|Android/i,       label: 'Android' },
  { re: /Tizen/i,                   label: 'Samsung TV' },
  { re: /Web0?OS/i,                 label: 'LG TV' },
  { re: /SmartTV|HbbTV/i,           label: 'Smart TV' },
  { re: /Macintosh|Mac OS X/i,      label: 'Mac' },
  { re: /Windows/i,                 label: 'Windows' },
  { re: /CrOS/i,                    label: 'ChromeOS' },
  { re: /Linux/i,                   label: 'Linux' },
  { re: /Edg\//i,                   label: 'Edge' },
  { re: /Chrome\//i,                label: 'Chrome' },
  { re: /Firefox\//i,               label: 'Firefox' },
  { re: /Safari\//i,                label: 'Safari' },
];

export function deviceFromUA(ua: string | null | undefined): string {
  if (!ua) return '';
  for (const rule of DEVICE_RULES) {
    if (rule.re.test(ua)) return rule.label;
  }
  return 'Web';
}

// The content slug lives in the segment right after `go-live` in
// every master URL the proxy generates (e.g. `/go-live/sintel-6s-hls/
// master.m3u8`). The basename and any deeper path components (variant
// playlists, partial segments) are not human-friendly, so we anchor
// off `go-live` rather than walking from the tail.
//
// Uploaded slugs from the encoding pipeline look like
//   INSANE_FPV_SHOTS_Hydrofoil_Windsurfing_p200_h264_20260423_212139
// — title + profile + codec + YYYYMMDD + HHMMSS — which is unusable
// in a pill. prettifyContentSlug strips that tail and visually caps
// the remainder so even verbose titles fit.
const PILL_SLUG_MAX = 22;

function prettifyContentSlug(raw: string): string {
  if (!raw) return '';
  let s = raw;
  // _<YYYYMMDD>_<HHMMSS> at the end (with or without preceding tokens).
  s = s.replace(/_\d{8}_\d{6}$/i, '');
  // _p<digits> profile and/or _<codec> trailing tokens. Repeat in case
  // both are present (e.g. `..._p200_h264`).
  for (let i = 0; i < 3; i++) {
    const next = s.replace(/_(p\d{2,4}|h264|h265|hevc|av1|vp9|vvc)$/i, '');
    if (next === s) break;
    s = next;
  }
  if (s.length > PILL_SLUG_MAX) s = s.slice(0, PILL_SLUG_MAX - 1) + '…';
  return s;
}

export function contentFromMasterUrl(url: string | null | undefined): string {
  if (!url) return '';
  let pathname = '';
  try {
    pathname = new URL(url, 'http://placeholder.local').pathname;
  } catch {
    return '';
  }
  const segments = pathname.split('/').filter(Boolean);
  const i = segments.indexOf('go-live');
  if (i < 0 || i + 1 >= segments.length) return '';
  return prettifyContentSlug(segments[i + 1]);
}

export function deviceContentLabel(p: PlayerRecord): string {
  const device = deviceFromUA(p.user_agent ?? null);
  const content = contentFromMasterUrl(p.current_play?.manifest?.master_url ?? null);
  if (device && content) return `${device} · ${content}`;
  return device || content || '';
}

/**
 * Group name derivation. Both the pill badge and the banner chip
 * should read "Group #<lowest-display_id>" so the two surfaces agree.
 * Falls back to a 6-char UUID slice only when none of the group's
 * members are currently in the players list (transient state during
 * tear-down).
 */
export function groupNameFor(
  group: { id: string; member_player_ids?: string[] | null },
  players: PlayerRecord[],
): string {
  const memberIds = group.member_player_ids ?? [];
  let lowest: number | null = null;
  for (const pid of memberIds) {
    const m = players.find((p) => p.id === pid);
    if (m?.display_id == null) continue;
    if (lowest == null || m.display_id < lowest) lowest = m.display_id;
  }
  if (lowest != null) return `Group #${lowest}`;
  return `Group ${group.id.slice(0, 6)}`;
}
