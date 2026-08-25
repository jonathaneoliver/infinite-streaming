/**
 * urlTimeFormat — shared helpers for the human-visible URL params
 * that link to /dashboard/session-viewer.html (and any other page
 * that scopes by player_id + play_id + time window).
 *
 * The compact format keeps URLs short and free of percent-encoding:
 *   - UUIDs are lowercase (ClickHouse canonical form; iOS-uppercase
 *     gets normalised so the SSE play_id filter actually matches —
 *     see [[case_sensitivity_ids]]).
 *   - Timestamps are ISO 8601 BASIC form (`YYYYMMDDTHHMMSSZ`) — no
 *     `:` to percent-encode, no millisecond noise.
 *   - Param names are `from` / `to` (shorter than `start_time` /
 *     `end_time`); the parser ALSO accepts the legacy names so old
 *     bookmarks keep working.
 *
 * Do NOT use this for the forwarder SSE / read-API calls
 * (useSessionTimeSeries / useSessionMetrics) — those talk to
 * ClickHouse via the forwarder and expect canonical ISO with colons.
 * This module is for browser-visible URLs only.
 */

/**
 * formatTimeCompact — ms-epoch → "20260522T170417Z" (ISO basic).
 *
 * The compact form drops separators + ms precision. One-second
 * resolution is plenty for a time window the operator sees in a
 * browser URL bar; sub-second precision would just clutter the bar
 * and surface millisecond drift between equally-good runs.
 */
export function formatTimeCompact(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}T${pad(d.getUTCHours())}${pad(d.getUTCMinutes())}${pad(d.getUTCSeconds())}Z`;
}

/**
 * parseTimeAny — accepts either format and returns ms-epoch, or
 * null when the value isn't parseable. Tries the compact form first
 * (because it can't be parsed by `Date.parse` directly), then falls
 * back to the standard parser for traditional ISO strings.
 *
 *   "20260522T170417Z"        → 1748113457000
 *   "2026-05-22T17:04:17Z"    → 1748113457000
 *   "2026-05-22T17:04:17.841Z"→ 1748113457841
 *   "live" / "now" / ""       → null
 */
export function parseTimeAny(v: string | null | undefined): number | null {
  if (!v) return null;
  if (v === 'live' || v === 'now') return null;
  // Compact form: YYYYMMDDTHHMMSSZ — 16 chars, no separators.
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(v);
  if (m) {
    return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
  }
  // Traditional ISO 8601 — leans on the platform parser.
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : null;
}

/**
 * canonicalUUID — lowercase a UUID string. The CH archive stores
 * UUIDs lowercase; iOS sometimes emits uppercase; URLs that mix
 * cases silently match zero rows in the dashboard's play_id filter.
 * See [[case_sensitivity_ids]].
 *
 * Pass-through for non-UUID strings — the regex check keeps random
 * text from being mangled.
 */
export function canonicalUUID(s: string | null | undefined): string {
  if (!s) return '';
  // Loose check — anything UUID-shaped gets normalised; everything
  // else (already-empty, "—", "(no play)") passes through.
  if (/^[0-9a-f-]{32,36}$/i.test(s)) return s.toLowerCase();
  return s;
}

/**
 * sessionViewerURL — single place to build the canonical link.
 * Returns a complete `/dashboard/session-viewer.html?…` string.
 *
 * Inputs: ms-epoch numbers for the window; nulls/NaNs are dropped
 * silently (the viewer falls back to its own bounds detection when
 * either side is missing).
 *
 * Use this from every site that links to the viewer — keeps URL
 * shape consistent and gives us one chokepoint to lengthen / shorten
 * the format later.
 */
/** One member of an archive compare set (#736). `tag` is the short
 *  session number used for the legend (`S<tag>`); kept in the URL so the
 *  viewer doesn't need a live player lookup to label historical overlays. */
export interface CompareMember {
  playerId: string;
  playId: string;
  tag?: string;
}

export function sessionViewerURL(opts: {
  playerId: string;
  playId?: string | null;
  fromMs?: number | null;
  toMs?: number | null;
  /** #736: the whole grouped set (including the active play) to overlay in
   *  compare mode. Serialised as `compare=<player>~<play>~<tag>,…` — `~`
   *  needs no percent-encoding, keeping the URL clean. */
  compare?: CompareMember[];
}): string {
  if (!opts.playerId) return '#';
  const p = new URLSearchParams();
  p.set('player_id', canonicalUUID(opts.playerId));
  if (opts.playId) p.set('play_id', canonicalUUID(opts.playId));
  if (opts.fromMs != null && Number.isFinite(opts.fromMs)) {
    p.set('from', formatTimeCompact(opts.fromMs));
  }
  if (opts.toMs != null && Number.isFinite(opts.toMs)) {
    p.set('to', formatTimeCompact(opts.toMs));
  }
  if (opts.compare && opts.compare.length > 1) {
    const enc = opts.compare
      .filter((m) => m.playerId && m.playId)
      .map((m) => `${canonicalUUID(m.playerId)}~${canonicalUUID(m.playId)}~${m.tag ?? ''}`)
      .join(',');
    if (enc) p.set('compare', enc);
  }
  // URLSearchParams encodes the `:` we'd otherwise smuggle through;
  // because the compact form has no `:` at all, the resulting URL is
  // free of `%3A` clutter.
  return `/dashboard/session-viewer.html?${p.toString()}`;
}

/** parseCompareParam — inverse of sessionViewerURL's `compare=` encoding.
 *  Returns [] for a missing/empty/garbled param so the viewer simply skips
 *  compare mode. */
export function parseCompareParam(raw: string | null | undefined): CompareMember[] {
  if (!raw) return [];
  return raw
    .split(',')
    .map((seg) => {
      const [playerId, playId, tag] = seg.split('~');
      return { playerId: canonicalUUID(playerId), playId: canonicalUUID(playId), tag: tag || undefined };
    })
    .filter((m) => m.playerId && m.playId);
}
