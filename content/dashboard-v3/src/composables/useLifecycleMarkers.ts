/**
 * useLifecycleMarkers(eventsStream) — derive the player-lifecycle
 * vertical-line markers (restart / play_start / play_end) once from the
 * shared session events stream, so every chart + the events timeline can
 * draw the SAME aligned lines from a single source of truth.
 *
 * The markers are recomputed whenever the events cache changes shape
 * (`version`) or resets (`epoch`). The marker list is tiny relative to the
 * row count (a handful per play), so a full re-scan of the cached rows on
 * each bump is cheap and keeps the logic stateless — resets and pan-back
 * backfills are handled for free.
 *
 * Rows here are the RAW snake_case CH projections (same objects the charts
 * and EventsTimeline ingest), NOT the camelCased adapter shape.
 *
 * Per-type visibility (restart / play_start / play_end) is a module-level
 * reactive object persisted to localStorage, so the toggle legend and every
 * chart share one switch and it survives a reload.
 */
import { computed, reactive, type ComputedRef } from 'vue';
import type { Stream } from './useSessionTimeSeries';

export type LifecycleKind = 'restart' | 'play_start' | 'play_end' | 'user_marked';

export interface LifecycleMarker {
  /** ms-since-epoch x-position of the line. */
  ms: number;
  kind: LifecycleKind;
  /** Line colour. */
  color: string;
  /** Chart.js / canvas dash pattern ([] = solid). */
  dash: number[];
  /** Multi-line hover tooltip text. */
  detail: string;
}

/** Per-kind visual style + legend label. Restart is a single fixed colour
 *  (amber, matching the existing PLAYBACK-lane RESTART dot) regardless of
 *  reason — the reason shows on hover. */
export const LIFECYCLE_STYLE: Record<LifecycleKind, { color: string; dash: number[]; label: string }> = {
  restart:     { color: '#f59e0b', dash: [4, 3], label: 'Restart' },
  play_start:  { color: '#15803d', dash: [],     label: 'Play start' },
  play_end:    { color: '#475569', dash: [2, 3], label: 'Play end' },
  // The operator "911" forensic mark (mark911() → last_event=user_marked,
  // label critical=user_marked_911). Magenta solid so it stands out from the
  // amber/green/slate lifecycle lines and the blue user cursor.
  user_marked: { color: '#db2777', dash: [],     label: 'User mark (911)' },
};

export const LIFECYCLE_KINDS: LifecycleKind[] = ['restart', 'play_start', 'play_end', 'user_marked'];

// ---- per-type visibility (module-level, persisted) -------------------------

const VIS_STORAGE_KEY = 'dashboard_v3_lifecycle_lines';

function readVisStored(): Record<LifecycleKind, boolean> {
  const def: Record<LifecycleKind, boolean> = {
    restart: true, play_start: true, play_end: true, user_marked: true,
  };
  try {
    const raw = localStorage.getItem(VIS_STORAGE_KEY);
    if (!raw) return def;
    const o = JSON.parse(raw) as Partial<Record<LifecycleKind, boolean>>;
    // Default-ON: only an explicit `false` hides a kind, so a partial /
    // older stored object still shows everything it doesn't mention.
    return {
      restart: o.restart !== false,
      play_start: o.play_start !== false,
      play_end: o.play_end !== false,
      user_marked: o.user_marked !== false,
    };
  } catch {
    return def;
  }
}

const lineVisibility = reactive<Record<LifecycleKind, boolean>>(readVisStored());

function setLineVisibility(kind: LifecycleKind, visible: boolean) {
  lineVisibility[kind] = visible;
  try {
    localStorage.setItem(VIS_STORAGE_KEY, JSON.stringify({ ...lineVisibility }));
  } catch {
    /* ignore */
  }
}

/** Shared reactive per-kind line visibility + its persisting setter.
 *  Used by the toggle legend (writes) and every chart (reads). */
export function useLifecycleLineVisibility() {
  return { lineVisibility, setLineVisibility };
}

// ---- row helpers (mirror EventsTimeline's parsing) -------------------------

function tsOfRow(row: Record<string, unknown>): number {
  const v = row.ts;
  if (typeof v === 'number') return v;
  if (typeof v !== 'string' || !v) return NaN;
  // CH DateTime64 comes back as "YYYY-MM-DD HH:MM:SS.mmm" (UTC, no zone).
  if (v.length > 10 && v.charAt(10) === ' ') return Date.parse(v.replace(' ', 'T') + 'Z');
  return Date.parse(v);
}

function numOf(v: unknown): number | null {
  if (v == null) return null;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v === 'string') {
    const n = Number(v);
    return Number.isFinite(n) ? n : null;
  }
  return null;
}

function strOf(row: Record<string, unknown>, key: string): string {
  const v = row[key];
  return typeof v === 'string' ? v.trim() : '';
}

/** Row labels[] as a string array (top-level field; `<sev>=<event>` entries). */
function labelsOf(row: Record<string, unknown>): string[] {
  const v = row.labels;
  if (Array.isArray(v)) return v.map((x) => String(x));
  // Defensive: some transports stringify the array.
  if (typeof v === 'string' && v) return v.split(',').map((s) => s.trim());
  return [];
}

/** The client's restart reason, recovered from labels[] (restart_reason is
 *  NOT a stored CH column — the forwarder folds it into `<sev>=restart_<reason>`
 *  in labels.go). Returns e.g. "auto_recovery_stuck" / "user_retry", or '' . */
function restartReasonFromLabels(row: Record<string, unknown>): string {
  for (const l of labelsOf(row)) {
    const val = l.includes('=') ? l.slice(l.indexOf('=') + 1) : l;
    if (val.startsWith('restart_')) return val.slice('restart_'.length);
  }
  return '';
}

/** Local HH:MM:SS — dashboard timestamps are shown in local time. */
function fmtClock(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function useLifecycleMarkers(
  stream: Stream<Record<string, unknown>>,
): { markers: ComputedRef<LifecycleMarker[]> } {
  const markers = computed<LifecycleMarker[]>(() => {
    // Subscribe to cache mutations + resets.
    void stream.version.value;
    void stream.epoch.value;

    const rows = stream.inRange(0, Number.MAX_SAFE_INTEGER);
    const out: LifecycleMarker[] = [];

    const seenPlay = new Set<string>();
    const playStartMs = new Map<string, number>();
    let prevPlayId: string | null = null;
    // Restart count is cumulative per play; seed silently on the first row of
    // each play so only mid-play increments mark (mirrors EventsTimeline).
    let prevRestarts: number | null = null;

    for (const r of rows) {
      const ms = tsOfRow(r);
      if (!Number.isFinite(ms)) continue;

      const playId = strOf(r, 'play_id');
      const lastEvent = strOf(r, 'last_event');
      const restarts = numOf(r.player_restarts);

      if (playId !== prevPlayId) {
        prevRestarts = null;
        prevPlayId = playId;
      }

      // PLAY START — first time a play_id is seen (covers the first play too).
      // Robust against the synthetic-id flicker via the seen-set, same as the
      // timeline's NEW PLAY marker.
      if (playId && !seenPlay.has(playId)) {
        seenPlay.add(playId);
        playStartMs.set(playId, ms);
        const content = strOf(r, 'content_name');
        const st = LIFECYCLE_STYLE.play_start;
        out.push({
          ms,
          kind: 'play_start',
          color: st.color,
          dash: st.dash,
          detail:
            `Play start\nplay_id ${playId.slice(0, 8)}…` +
            (content ? `\n${content}` : '') +
            `\n${fmtClock(ms)}`,
        });
      }

      // PLAY END — explicit terminal event from the client.
      if (lastEvent === 'play_end') {
        const status = strOf(r, 'playback_status');
        const reason = strOf(r, 'playback_reason');
        const startedAt = playId ? playStartMs.get(playId) : undefined;
        const durS = startedAt != null ? (ms - startedAt) / 1000 : null;
        const st = LIFECYCLE_STYLE.play_end;
        out.push({
          ms,
          kind: 'play_end',
          color: st.color,
          dash: st.dash,
          detail:
            `Play end${status ? `: ${status}` : ''}` +
            (reason ? `\n${reason}` : '') +
            (playId ? `\nplay_id ${playId.slice(0, 8)}…` : '') +
            (durS != null ? `\nduration ${durS.toFixed(1)}s` : '') +
            `\n${fmtClock(ms)}`,
        });
      }

      // RESTART — player_restarts counter increment. Reason from labels[]
      // (restart_reason is not a stored column).
      if (restarts != null) {
        if (prevRestarts != null && restarts > prevRestarts) {
          const reason = restartReasonFromLabels(r) || 'reason n/a';
          const st = LIFECYCLE_STYLE.restart;
          out.push({
            ms,
            kind: 'restart',
            color: st.color,
            dash: st.dash,
            detail: `Restart: ${reason}\n+${restarts - prevRestarts} (total ${restarts})\n${fmtClock(ms)}`,
          });
        }
        prevRestarts = restarts;
      }

      // USER MARK (911) — operator forensic mark (mark911()).
      if (lastEvent === 'user_marked') {
        const st = LIFECYCLE_STYLE.user_marked;
        out.push({
          ms,
          kind: 'user_marked',
          color: st.color,
          dash: st.dash,
          detail: `User mark (911)${playId ? `\nplay_id ${playId.slice(0, 8)}…` : ''}\n${fmtClock(ms)}`,
        });
      }
    }

    return out;
  });

  return { markers };
}
