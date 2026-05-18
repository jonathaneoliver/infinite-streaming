<script setup lang="ts">
/**
 * PlayLog.vue — time-multiplexed scroll of three sources on one
 * chronological view (post-#472 vocabulary):
 *
 *   - event   (session_events rows, via timeseries.events) — one per
 *             player metrics POST. Used to be called "snapshot".
 *   - network (network_requests rows, via timeseries.network)
 *   - marker  (session_markers rows, via timeseries.markers) — one
 *             per classifier-derived event. Used to be called "event".
 *
 * Operator-facing differences from NetworkLog: no timing bar (per
 * user request), source-toggle checkboxes, and uniform columns
 * (time / source / player_id / play_id / attempt_id / name / info)
 * that work for all three row shapes.
 *
 * No new server fetch — re-uses the three Streams the parent
 * SessionDisplay already holds from useSessionTimeSeries. Row build
 * is a reactive computed over `version` so the table grows live as
 * SSE deltas land.
 */
import { computed, nextTick, onBeforeUnmount, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { Stream } from '@/composables/useSessionTimeSeries';

const props = defineProps<{
  playerId: string;
  /** play_id from the URL, used as the fallback `play_id` value for
   *  event rows — the events SSE doesn't yet project play_id (the
   *  derivation in events_query.go doesn't carry it through the
   *  UNION ALL branches). Within a SessionViewer scope this is the
   *  same id every event would have anyway.
   *  In live mode this is null, in which case event rows show '—'. */
  playId: string | null;
  eventsStream: Stream<Record<string, unknown>>;
  networkStream: Stream<Record<string, unknown>>;
  markersStream: Stream<Record<string, unknown>>;
}>();

const playerIdRef = toRef(props, 'playerId');
usePlayer(playerIdRef); // keep the SSE subscription warm
const coord = useChartCoordination(playerIdRef);

/** Real player UUID for display purposes. The `playerId` prop is the
 *  shared cache key used by useChartCoordination + the streams cache;
 *  in archive mode SessionDisplay synthesises it as
 *  `archive:<real-uuid>:<play-or-all>` so live + archive caches stay
 *  isolated. Snapshot + network rows carry the real player_id on the
 *  raw row, so the column populates correctly for those — but event
 *  rows (events_query.go doesn't project player_id today) fall back
 *  to props.playerId and end up showing "archive:" instead of the
 *  UUID. Parse the synthetic key here so event rows show the right
 *  value too. */
function realPlayerId(): string {
  const v = props.playerId;
  if (v.startsWith('archive:')) {
    const after = v.slice('archive:'.length);
    const colon = after.indexOf(':');
    return colon > 0 ? after.slice(0, colon) : after;
  }
  return v;
}

// Filter state, one per source. Names follow the post-#472
// vocabulary: player POSTs = events, classifier output = markers,
// network = HTTP requests.
const showEvents = ref(true);
const showNetwork = ref(true);
const showMarkers = ref(true);

/** Follow-latest mirrors the page-level focus bar's "Live" state.
 *  When the operator drags the brush back to a historical window,
 *  range !== null and we stop auto-scrolling to bottom. When they
 *  toggle Live back on, range is null and the auto-scroll resumes.
 *  See BitrateChartPanelToolbar.vue for the canonical pattern. */
const liveChecked = computed(() => coord.state.range === null);
function onLiveToggleClick() {
  coord.toggleLive();
}

/** Raw column toggle — when on, the row shows a dedicated `raw` cell
 *  containing `session_json` (snapshots) or the full raw row pretty-
 *  printed (network/events). When off, the column is hidden entirely
 *  AND session_json is filtered from the kv chips. */
const showRaw = ref(false);

/** Display mode for SNAPSHOT rows only. Snapshots are dense
 *  state-dumps where most fields stay constant across heartbeats;
 *  showing every field on every row drowns out the actual deltas.
 *  Network + event rows are inherently distinct per row so they
 *  always render every field regardless of this setting.
 *
 *  - "all": every non-empty key=value pair on the snapshot row.
 *  - "changed": only the keys whose value differs from the
 *    previous chronologically-earlier snapshot row. Lets the
 *    operator scan state transitions on a thrashing player. */
type DisplayMode = 'all' | 'changed';
// Default to 'changed' — operators almost always want to see what
// actually moved between snapshots, not the ~50-field raw dump. Flip
// to 'all' for forensics on a specific row.
const displayMode = ref<DisplayMode>('changed');

/** Field-ordering knob.
 *
 *  - "alphabetic": classic name sort. Predictable, easy to scan when
 *    you know the field you're looking for.
 *  - "by-change-rate": fields ordered ascending by how often they
 *    change value between adjacent snapshots. Rarely-changing
 *    fields (state, last_event, video_resolution, …) bubble to the
 *    front; per-tick metrics like position_s and playhead_wallclock_ms
 *    sink to the back. Especially handy in "Changed only" mode where
 *    the leading fields are the interesting transitions.
 *
 *  Change rate is computed across snapshot rows only — network /
 *  event rows fall back to alphabetic since every row is distinct
 *  and there's no meaningful frequency to compute. */
type FieldOrder = 'alphabetic' | 'by-change-rate';
// Default to 'by-change-rate' — pairs naturally with 'changed' display
// mode: the leading fields are the ones that just transitioned, the
// constant-ish ones sink to the back. Alphabetic is the muscle-memory
// fallback when you know the field name and want to scan for it.
const fieldOrder = ref<FieldOrder>('by-change-rate');

type SortCol = 'time' | 'source';
type SortDir = 'asc' | 'desc';
const sortCol = ref<SortCol | null>('time');
const sortDir = ref<SortDir>('asc');

const rowsScrollRef = ref<HTMLDivElement | null>(null);

type Source = 'event' | 'network' | 'marker';
interface Row {
  ts: number;          // epoch ms
  source: Source;
  playerId: string;
  playId: string;
  attemptId: string;
  /** Original payload as it came off the stream — used to compute
   *  the per-row field list (and to diff against the previous row
   *  of the same source for "Changed fields" mode). */
  raw: Record<string, unknown>;
  /** Event rows: the value of `raw.type` lifted to a top-level
   *  badge so the event name is always visible regardless of the
   *  chip ordering / diff filter. Empty for snapshot + network
   *  rows. */
  eventName?: string;
}

interface DisplayedField {
  name: string;
  value: string;
}

/** Field keys handled by the identity columns; skip in the kv panel
 *  to avoid duplicating what's already visible on every row. Also
 *  skips monotonic-noise fields whose change-on-every-row would
 *  dominate the "Changed fields" view. `session_json` is here too —
 *  it's a huge blob, surfaced in the Raw column when that toggle is
 *  on, otherwise hidden from the chip list entirely. */
const SKIP_KEYS = new Set([
  'ts', 'timestamp', 'event_time',     // rendered as _time
  'player_id', 'id',                   // rendered as player_id
  'play_id',                           // rendered as play_id
  'attempt_id',                        // rendered as attempt_id
  'revision',                          // monotonic counter
  'server_received_at_ms',             // monotonic counter (server clock)
  'entry_fingerprint',                 // CH dedupe key
  'session_json',                      // shown only in the optional Raw column
]);

function tsOf(raw: Record<string, unknown>): number {
  const v = raw.ts ?? raw.timestamp;
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v !== 'string') return NaN;
  // ClickHouse "YYYY-MM-DD HH:MM:SS.fff" → RFC3339 by swapping the
  // space for 'T' and appending 'Z'. Already-RFC3339 strings pass
  // through Date.parse unchanged.
  const normalised = v.length > 10 && v.charAt(10) === ' '
    ? v.replace(' ', 'T') + 'Z'
    : v;
  const ms = Date.parse(normalised);
  return Number.isFinite(ms) ? ms : NaN;
}

function asStr(v: unknown): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number') return String(v);
  return '';
}

/** Normalise UUID identifiers to lowercase for cross-source
 *  consistency. The iOS player POSTs `play_id` in Apple's preferred
 *  uppercase form (e.g. `3127F2F4-BBBB-4D15-...`), but the rest of
 *  the pipeline (proxy's `lowerUTF8` filters, dashboard URL params)
 *  uses lowercase. Without this normalisation the same UUID can
 *  render in two different cases depending on which row it came
 *  from, and grouping by id breaks visually. */
function asLowerId(v: unknown): string {
  const s = asStr(v);
  return s ? s.toLowerCase() : s;
}

/** Extract the path tail from a HAR URL, dropping the content-name
 *  prefix that's identical for every request on a play. */
function pathTail(url: string): string {
  if (!url) return '';
  const idx = url.indexOf('/go-live/');
  if (idx < 0) return url;
  // skip "/go-live/<content_id>/"
  const after = url.slice(idx + '/go-live/'.length);
  const slash = after.indexOf('/');
  return slash >= 0 ? after.slice(slash + 1) : after;
}

function buildSnapshotRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  return {
    ts,
    source: 'event',
    playerId: asLowerId(raw.player_id ?? realPlayerId()),
    playId: asLowerId(raw.play_id),
    attemptId: asStr(raw.attempt_id),
    raw,
    eventName: pickEventName(raw, ['event_name', 'last_event', 'trigger_type']),
  };
}

function buildNetworkRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  // Derive the operator-friendly summary fields the legacy NetworkLog
  // panel surfaces (KB, Mbps, dur) and graft them onto the row so
  // they show as kv chips. NETWORK_KEEP (below) whitelists the chip
  // list to just status + duration + KB + Mbps — every other raw
  // field is dropped from the fields column for network rows.
  const bytesOut = numOrZero(raw.bytes_out);
  const transferMs = numOrZero(raw.transfer_ms);
  const totalMs = numOrZero(raw.total_ms);
  const summed = numOrZero(raw.dns_ms) + numOrZero(raw.connect_ms)
    + numOrZero(raw.tls_ms) + numOrZero(raw.ttfb_ms) + transferMs;
  const durMs = totalMs > 0 ? totalMs : summed;
  const enriched: Record<string, unknown> = { ...raw };
  if (bytesOut > 0) enriched.KB = (bytesOut / 1024);
  if (transferMs > 0 && bytesOut > 0) {
    enriched.Mbps = (bytesOut * 8) / (transferMs * 1000);
  }
  if (durMs > 0) enriched.duration = fmtMs(durMs);
  // event_name for network rows = method + path tail (the URL's
  // content-name prefix is identical for every request and just
  // adds noise). Full URL lives in raw.url and is reachable via the
  // Raw column when the operator wants to see it verbatim.
  const method = asStr(raw.method) || 'GET';
  const path = pathTail(asStr(raw.url) || asStr(raw.path));
  const evName = path ? `${method} ${path}` : method;
  return {
    ts,
    source: 'network',
    playerId: asLowerId(raw.player_id ?? realPlayerId()),
    playId: asLowerId(raw.play_id),
    attemptId: asStr(raw.attempt_id),
    raw: enriched,
    eventName: evName,
  };
}

function numOrZero(v: unknown): number {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

/** Same human-readable ms/s formatter the NetworkLog panel uses, so
 *  durations in the Play Log line up with the waterfall above it. */
function fmtMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '';
  if (ms < 1) return ms.toFixed(2) + ' ms';
  if (ms < 1000) return ms.toFixed(0) + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}

function buildEventRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  // Since issue #469 the events stream rides on the new
  // session_events table — every event row carries player_id /
  // play_id / attempt_id stamped at ingest by the eventclass
  // classifier package. Fall back to the URL play_id for old plays
  // that landed before the cutover (their event rows don't exist in
  // session_events; if the dashboard sees one it was synthesised
  // somewhere else).
  return {
    ts,
    source: 'marker',
    playerId: asLowerId(raw.player_id ?? realPlayerId()),
    playId: asLowerId(raw.play_id ?? props.playId ?? ''),
    attemptId: asStr(raw.attempt_id),
    raw,
    eventName: pickEventName(raw, ['event_name', 'type']),
  };
}

/** Look up the first non-empty string field from a fallback chain.
 *  Lets the event_name column survive future renames — when the
 *  storage column is renamed to `event_name`, that key wins; until
 *  then we fall back to source-specific legacy names (snapshots
 *  carry `last_event` / `trigger_type`; events carry `type`). */
function pickEventName(raw: Record<string, unknown>, keys: string[]): string {
  for (const k of keys) {
    const v = asStr(raw[k]);
    if (v) return v;
  }
  return '';
}

/** Render a single field value to a short display string. JSON-stringify
 *  nested values; trim floats; tolerate everything else. */
function formatValue(v: unknown): string {
  if (v == null) return '';
  if (typeof v === 'string') return v;
  if (typeof v === 'number') {
    // Trim long float tails — 3 sig figs is enough for buffer/bitrate
    // ranges that dominate the diff view. Ints pass through unchanged.
    if (Number.isInteger(v)) return String(v);
    if (!Number.isFinite(v)) return String(v);
    return Number(v.toFixed(3)).toString();
  }
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  try { return JSON.stringify(v); } catch { return String(v); }
}

/** Walk the raw row's top-level keys (skipping identity + noise
 *  fields), emit name=value pairs sorted alphabetically. Empty
 *  values are dropped — they'd just be visual noise.
 *
 *  `extraSkip` lets a caller hide keys it's already rendering
 *  elsewhere — used for event rows to omit `type` (lifted to the
 *  eventName badge) so it doesn't appear twice. */
function fieldsFromRaw(raw: Record<string, unknown>, extraSkip?: Set<string>): DisplayedField[] {
  const out: DisplayedField[] = [];
  const keys = Object.keys(raw).sort();
  for (const k of keys) {
    if (SKIP_KEYS.has(k)) continue;
    if (extraSkip && extraSkip.has(k)) continue;
    const v = raw[k];
    if (v == null) continue;
    const formatted = formatValue(v);
    if (formatted === '') continue;
    out.push({ name: k, value: formatted });
  }
  return out;
}

/** Keys lifted into the dedicated event_name column. Mirrors the
 *  fallback chains in pickEventName so the value isn't shown twice
 *  (once in the column, once as a chip). `event_name` covers the
 *  post-rename storage; `last_event` / `trigger_type` / `type` cover
 *  the legacy storage we still read from today. */
const EVENT_SKIP = new Set(['event_name', 'type']);
const SNAPSHOT_SKIP = new Set(['event_name', 'last_event', 'trigger_type']);

/** Network rows: WHITELIST mode — only these four chips render in
 *  the fields column, in this fixed order. The URL / method / phase
 *  timings / bytes_in / fault metadata / etc. that the raw
 *  network_requests row carries are all reachable from the Raw
 *  column when the operator wants them; here the operator wants the
 *  at-a-glance request summary. Keys match the derived fields built
 *  in buildNetworkRow. `status` leads because operators scan for
 *  4xx/5xx first — every other chip is per-row noise unless the
 *  status is bad.
 */
const NETWORK_KEEP_ORDER: readonly string[] = ['status', 'duration', 'KB', 'Mbps'];
const NETWORK_KEEP = new Set(NETWORK_KEEP_ORDER);

const allRows = computed<Row[]>(() => {
  // Touch each stream's version so Vue re-runs the computed on every
  // delta, even though inRange() also touches the underlying ref.
  void props.eventsStream.version.value;
  void props.networkStream.version.value;
  void props.markersStream.version.value;
  const built: Row[] = [];
  if (showEvents.value) {
    for (const raw of props.eventsStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildSnapshotRow(raw);
      if (r) built.push(r);
    }
  }
  if (showNetwork.value) {
    for (const raw of props.networkStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildNetworkRow(raw);
      if (r) built.push(r);
    }
  }
  if (showMarkers.value) {
    for (const raw of props.markersStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildEventRow(raw);
      if (r) built.push(r);
    }
  }
  return built;
});

function sortValue(r: Row, c: SortCol): number | string {
  switch (c) {
    case 'time': return r.ts;
    case 'source': return r.source;
  }
}

/** Row + the field list to render. Computed in chronological order
 *  so the "Changed fields" diff against the previous same-source row
 *  is well-defined regardless of the display sort. */
interface RowWithFields extends Row {
  fields: DisplayedField[];
}

/** Per-field "change rate" across snapshot rows, in [0, 1]. A field
 *  whose value flips between every adjacent pair sits at 1.0; a
 *  field that's truly constant sits at 0. Recomputed reactively
 *  with the snapshot stream — touches version so deltas re-run it. */
const snapshotChangeRates = computed<Map<string, number>>(() => {
  void props.eventsStream.version.value;
  const snapshots = props.eventsStream.inRange(0, Number.MAX_SAFE_INTEGER);
  if (snapshots.length < 2) return new Map();
  // Stable chronological order. The stream's natural order is
  // typically already ts-ascending, but sort defensively — change-
  // rate math is meaningless under arbitrary order.
  const chrono = snapshots
    .map((raw) => ({ ts: tsOf(raw), raw }))
    .filter((r) => Number.isFinite(r.ts))
    .sort((a, b) => a.ts - b.ts);
  if (chrono.length < 2) return new Map();
  const changes = new Map<string, number>();
  const seen = new Map<string, number>();
  for (let i = 1; i < chrono.length; i++) {
    const prev = fieldsFromRaw(chrono[i - 1].raw);
    const cur = fieldsFromRaw(chrono[i].raw);
    const prevByKey = new Map<string, string>();
    for (const f of prev) prevByKey.set(f.name, f.value);
    const seenThisPair = new Set<string>();
    for (const f of cur) {
      seenThisPair.add(f.name);
      seen.set(f.name, (seen.get(f.name) ?? 0) + 1);
      const prevValue = prevByKey.get(f.name);
      if (prevValue !== undefined && prevValue !== f.value) {
        changes.set(f.name, (changes.get(f.name) ?? 0) + 1);
      }
    }
    // Fields present in `prev` but absent in `cur` count as a change
    // too — the value moved from "something" to "nothing". Rare in
    // practice but the math should be honest about it.
    for (const f of prev) {
      if (!seenThisPair.has(f.name)) {
        seen.set(f.name, (seen.get(f.name) ?? 0) + 1);
        changes.set(f.name, (changes.get(f.name) ?? 0) + 1);
      }
    }
  }
  const out = new Map<string, number>();
  for (const [name, total] of seen) {
    out.set(name, (changes.get(name) ?? 0) / total);
  }
  return out;
});

/** Sort comparator that picks alphabetic or change-rate-based
 *  ordering. Snapshots use change-rate when selected; network /
 *  event rows always fall back to alphabetic (no meaningful
 *  frequency to compute — every row is unique).
 *
 *  Change-rate sort puts the most-operator-interesting fields first:
 *
 *    1. Rarely-but-not-never changing fields (rate > 0, low) —
 *       state, last_event, video_resolution, … — these are
 *       surprising transitions worth seeing.
 *    2. More frequently changing fields (rate > 0, high) —
 *       position_s, playhead_wallclock_ms, per-tick metrics.
 *    3. Truly constant fields (rate == 0) — user_agent, content_id,
 *       master_manifest_url, … — informative once, then dead weight.
 *       Sink to the END.
 *
 *  Constants are demoted by treating their rate as +Infinity in the
 *  sort key, so they always lose ties to anything that's ever
 *  changed during the session. */
function sortFields(fields: DisplayedField[], source: Source): DisplayedField[] {
  if (fieldOrder.value === 'alphabetic' || source !== 'event') {
    // fieldsFromRaw already emits alphabetic — return as-is.
    return fields;
  }
  const rates = snapshotChangeRates.value;
  const keyFor = (name: string): number => {
    const r = rates.get(name);
    // Unknown rate (e.g. only one snapshot observed so far) →
    // demote alongside constants. Honest: "we don't have enough
    // data to call this volatile, so don't pretend it is."
    if (r == null) return Number.POSITIVE_INFINITY;
    // Truly constant fields → infinity → land at the end.
    if (r === 0) return Number.POSITIVE_INFINITY;
    return r;
  };
  return fields.slice().sort((a, b) => {
    const ka = keyFor(a.name);
    const kb = keyFor(b.name);
    if (ka !== kb) return ka - kb;
    return a.name.localeCompare(b.name); // tie → alphabetic
  });
}

const rowsWithFields = computed<RowWithFields[]>(() => {
  // Build chronological copy so the diff against the previous
  // snapshot is well-defined regardless of the display sort
  // direction the operator picks below.
  const chrono = allRows.value.slice().sort((a, b) => a.ts - b.ts);
  const mode = displayMode.value;
  // Only snapshots participate in the diff — every network /
  // event row is unique by construction so a per-row diff is
  // either uninformative (everything different every time) or
  // misleading (status=200 chip vanishes because the previous
  // request was also 200, hiding the steady-state success).
  let prevSnapshot: Record<string, unknown> | null = null;
  const out: RowWithFields[] = new Array(chrono.length);
  for (let i = 0; i < chrono.length; i++) {
    const r = chrono[i];
    let fields: DisplayedField[];
    if (r.source === 'network') {
      // Network rows: whitelist mode — only status, duration, KB,
      // Mbps render in the fields column, in NETWORK_KEEP_ORDER
      // (status first — operators scan for 4xx/5xx before anything
      // else). The URL / phase timings / fault metadata are
      // reachable from the Raw column.
      const byKey = new Map<string, DisplayedField>();
      for (const f of fieldsFromRaw(r.raw)) {
        if (NETWORK_KEEP.has(f.name)) byKey.set(f.name, f);
      }
      fields = NETWORK_KEEP_ORDER
        .map((k) => byKey.get(k))
        .filter((f): f is DisplayedField => f != null);
    } else if (r.source === 'event' && mode === 'changed' && prevSnapshot) {
      const prevByKey = new Map<string, string>();
      for (const f of fieldsFromRaw(prevSnapshot)) prevByKey.set(f.name, f.value);
      fields = fieldsFromRaw(r.raw, SNAPSHOT_SKIP).filter((f) => prevByKey.get(f.name) !== f.value);
    } else if (r.source === 'event') {
      // Snapshot in 'all' mode, or first-ever snapshot in 'changed'
      // mode (no prior to diff against).
      fields = fieldsFromRaw(r.raw, SNAPSHOT_SKIP);
    } else {
      // event row — every field except `type` (lifted to event_name).
      fields = fieldsFromRaw(r.raw, EVENT_SKIP);
    }
    fields = sortFields(fields, r.source);
    out[i] = { ...r, fields };
    if (r.source === 'event') prevSnapshot = r.raw;
  }
  return out;
});

const sortedRows = computed<RowWithFields[]>(() => {
  const list = rowsWithFields.value.slice();
  const col = sortCol.value;
  if (!col) return list;
  const sign = sortDir.value === 'asc' ? 1 : -1;
  list.sort((a, b) => {
    const av = sortValue(a, col);
    const bv = sortValue(b, col);
    if (typeof av === 'string' || typeof bv === 'string') {
      return String(av).localeCompare(String(bv)) * sign;
    }
    return (Number(av) - Number(bv)) * sign;
  });
  return list;
});

const counts = computed(() => {
  let evt = 0, net = 0, mrk = 0;
  for (const r of allRows.value) {
    if (r.source === 'event') evt++;
    else if (r.source === 'network') net++;
    else mrk++;
  }
  return { evt, net, mrk, total: allRows.value.length };
});

function clickSort(col: SortCol) {
  if (sortCol.value !== col) {
    sortCol.value = col;
    sortDir.value = col === 'time' ? 'asc' : 'asc';
  } else if (sortDir.value === 'asc') {
    sortDir.value = 'desc';
  } else {
    sortCol.value = null;
    sortDir.value = 'asc';
  }
}

function arrow(col: SortCol): string {
  if (sortCol.value !== col) return '';
  return sortDir.value === 'asc' ? ' ▲' : ' ▼';
}

function fmtTime(ts: number): string {
  const d = new Date(ts);
  return (
    String(d.getHours()).padStart(2, '0') + ':' +
    String(d.getMinutes()).padStart(2, '0') + ':' +
    String(d.getSeconds()).padStart(2, '0') + '.' +
    String(d.getMilliseconds()).padStart(3, '0')
  );
}

/** Short 8-char prefix of a UUID for display. Returns '—' on empty. */
function shortId(id: string): string {
  if (!id) return '—';
  return id.length > 8 ? id.slice(0, 8) : id;
}

/** Raw column value — full `session_json` for snapshot rows, falls
 *  back to a pretty-print of the whole raw row for network / event
 *  rows (which don't have a session_json field). The cell template
 *  truncates visually; the value lives in the title attr for hover. */
function rawValueFor(r: Row): string {
  if (r.source === 'event') {
    const sj = r.raw['session_json'];
    if (typeof sj === 'string' && sj.length > 0) return sj;
  }
  try {
    return JSON.stringify(r.raw, null, 2);
  } catch {
    return String(r.raw);
  }
}

// Follow-latest: snap the inner scroll container to the end when new
// rows arrive — gated by the page-level focus bar's live state.
// When the brush is pinned (coord.state.range !== null) the operator
// is reading history; don't yank the scroll out from under them.
watch(
  () => sortedRows.value.length,
  () => {
    if (!liveChecked.value) return;
    nextTick(() => {
      const el = rowsScrollRef.value;
      if (!el) return;
      el.scrollTop = sortDir.value === 'asc' ? el.scrollHeight : 0;
    });
  },
);

/* ─── Wheel hijack ─────────────────────────────────────────────────
 * Plain wheel inside the inner scroll container moves the PAGE; only
 * Alt+wheel scrolls the rows. Matches NetworkLog.vue so operators
 * don't have to learn two different wheel behaviours for similar
 * tables in the same panel. */
let attachedRowsEl: HTMLDivElement | null = null;
watch(rowsScrollRef, (el) => {
  if (attachedRowsEl && attachedRowsEl !== el) {
    attachedRowsEl.removeEventListener('wheel', onRowsWheel, { capture: true } as EventListenerOptions);
    attachedRowsEl = null;
  }
  if (el && el !== attachedRowsEl) {
    el.addEventListener('wheel', onRowsWheel, { capture: true, passive: false });
    attachedRowsEl = el;
  }
}, { immediate: true });
onBeforeUnmount(() => {
  if (attachedRowsEl) {
    attachedRowsEl.removeEventListener('wheel', onRowsWheel, { capture: true } as EventListenerOptions);
    attachedRowsEl = null;
  }
});
function onRowsWheel(e: WheelEvent) {
  if (e.altKey) return;
  e.preventDefault();
  window.scrollBy({ top: e.deltaY, left: e.deltaX, behavior: 'auto' });
}
</script>

<template>
  <div class="play-log">
    <div class="toolbar">
      <label class="opt"><input type="checkbox" v-model="showEvents" /> Events ({{ counts.evt }})</label>
      <label class="opt"><input type="checkbox" v-model="showNetwork" /> Network ({{ counts.net }})</label>
      <label class="opt"><input type="checkbox" v-model="showMarkers" /> Markers ({{ counts.mrk }})</label>
      <label class="opt"><input type="checkbox" v-model="showRaw" /> Raw</label>
      <span class="count">{{ counts.total }} row{{ counts.total === 1 ? '' : 's' }}</span>
      <button
        type="button"
        class="btn live-toggle"
        :class="{ checked: liveChecked }"
        @click="onLiveToggleClick"
        :title="liveChecked
          ? 'Pause auto-scroll at the current row. Drops follow-latest until you toggle back.'
          : 'Resume following live — drops any pinned brush window.'"
      >
        {{ liveChecked ? '●' : '○' }} Live
      </button>
    </div>

    <div class="toolbar mode-row">
      <span class="mode-label">Snapshot fields:</span>
      <label class="opt"><input type="radio" value="all" v-model="displayMode" /> All</label>
      <label class="opt"><input type="radio" value="changed" v-model="displayMode" /> Changed only (vs previous snapshot)</label>
      <span class="mode-hint">Network &amp; event rows always show every field.</span>
    </div>

    <div class="toolbar mode-row">
      <span class="mode-label">Field order:</span>
      <label class="opt"><input type="radio" value="alphabetic" v-model="fieldOrder" /> Alphabetic</label>
      <label class="opt"><input type="radio" value="by-change-rate" v-model="fieldOrder" /> By interest (changing fields first, constants last)</label>
      <span class="mode-hint">Snapshots only; network &amp; event rows stay alphabetic.</span>
    </div>

    <p class="note">
      Time-ordered merge of three sources. <strong>Snapshot</strong> = one
      `session_snapshots` row (player heartbeat or state-change post).
      <strong>Network</strong> = one `network_requests` row. <strong>Event</strong>
      = one derived row from the typed event taxonomy. `attempt_id` on
      event rows is blank — the events SSE doesn't yet project it (see
      `events_query.go`); within a single SessionViewer scope the
      `play_id` shown is the URL's play_id.
    </p>

    <div v-if="!sortedRows.length" class="empty">No rows yet.</div>
    <div v-else class="table-wrap" :class="{ 'with-raw': showRaw }">
      <div class="row head">
        <div class="cell c-time sortable" @click="clickSort('time')">_time<span class="arr">{{ arrow('time') }}</span></div>
        <div class="cell c-source sortable" @click="clickSort('source')">source<span class="arr">{{ arrow('source') }}</span></div>
        <div class="cell c-player">player_id</div>
        <div class="cell c-play">play_id</div>
        <div class="cell c-attempt">attempt_id</div>
        <div class="cell c-eventname">event_name</div>
        <div class="cell c-fields">fields</div>
        <div v-if="showRaw" class="cell c-raw">raw</div>
      </div>

      <div class="rows" ref="rowsScrollRef">
        <div
          v-for="(r, i) in sortedRows"
          :key="i"
          class="row"
          :class="`src-${r.source}`"
        >
          <div class="cell c-time">{{ fmtTime(r.ts) }}</div>
          <div class="cell c-source">
            <span class="src-tag" :class="`tag-${r.source}`">{{ r.source }}</span>
          </div>
          <div class="cell c-player" :title="r.playerId">{{ shortId(r.playerId) }}</div>
          <div class="cell c-play" :title="r.playId">{{ shortId(r.playId) }}</div>
          <div class="cell c-attempt" :title="r.attemptId">{{ shortId(r.attemptId) }}</div>
          <div class="cell c-eventname">
            <span
              v-if="r.eventName"
              class="event-name"
              :class="`event-name-${r.source}`"
              :title="r.eventName"
            >{{ r.eventName }}</span>
            <span v-else class="event-name-empty">—</span>
          </div>
          <div class="cell c-fields">
            <span v-if="r.fields.length === 0" class="kv-empty">—</span>
            <span
              v-for="f in r.fields"
              :key="f.name"
              class="kv"
              :title="f.name + '=' + f.value"
            ><span class="kv-name">{{ f.name }}</span>=<span class="kv-value">{{ f.value }}</span></span>
          </div>
          <div v-if="showRaw" class="cell c-raw" :title="rawValueFor(r)">
            <pre class="raw-pre">{{ rawValueFor(r) }}</pre>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.play-log {
  display: grid;
  gap: 8px;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 12px;
  color: #6b7280;
  flex-wrap: wrap;
}
.btn {
  background: #f3f4f6;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  padding: 4px 10px;
  font-size: 12px;
  cursor: pointer;
}
.btn:hover { background: #e5e7eb; }
.opt { display: inline-flex; align-items: center; gap: 4px; cursor: pointer; }
.count { font-variant-numeric: tabular-nums; }

/* Live toggle — same scheme as BitrateChartPanelToolbar /
 * MetricsLineChart / EventsTimeline so all the toggles in the
 * session card match visually. Anchored to the right of the
 * toolbar so it lands in the same screen position as the chart
 * panels' Live buttons. */
.btn.live-toggle {
  margin-left: auto;
}
.btn.live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: white;
  font-weight: 600;
}
.btn.live-toggle.checked:hover { background: #059669; }
.btn.live-toggle:not(.checked) {
  background: #f3f4f6;
  border-color: #d1d5db;
  color: #6b7280;
}
.btn.live-toggle:not(.checked):hover { background: #e5e7eb; color: #374151; }

.empty {
  font-size: 13px;
  color: #9ca3af;
  padding: 24px;
  text-align: center;
}

.note {
  font-size: 12px;
  background: #eff6ff;
  border: 1px solid #bfdbfe;
  color: #1e3a8a;
  padding: 8px 10px;
  border-radius: 6px;
  margin: 0;
  line-height: 1.4;
}

.table-wrap {
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  overflow: hidden;
  background: #fff;
}

.mode-row {
  margin-top: -2px;
}
.mode-label {
  font-weight: 600;
  color: #374151;
}
.mode-hint {
  color: #9ca3af;
  font-size: 11px;
}

.row {
  display: grid;
  grid-template-columns:
    var(--c-time, 96px)
    var(--c-source, 76px)
    var(--c-player, 90px)
    var(--c-play, 90px)
    var(--c-attempt, 90px)
    var(--c-eventname, minmax(140px, 280px))
    var(--c-fields, minmax(280px, 4fr));
  gap: 8px;
  padding: 4px 8px;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  align-items: start;
  border-top: 1px solid #f3f4f6;
}

/* When the Raw column is toggled on, the row grid grows by one slot
 * and the fields column tightens so the raw cell has room. */
.table-wrap.with-raw .row {
  grid-template-columns:
    var(--c-time, 96px)
    var(--c-source, 76px)
    var(--c-player, 90px)
    var(--c-play, 90px)
    var(--c-attempt, 90px)
    var(--c-eventname, minmax(140px, 280px))
    var(--c-fields, minmax(200px, 2fr))
    var(--c-raw, minmax(280px, 3fr));
}

.row.head {
  background: #f3f4f6;
  font-family: system-ui;
  font-weight: 600;
  color: #4b5563;
  text-transform: uppercase;
  font-size: 10px;
  letter-spacing: 0.4px;
  padding: 6px 8px;
  border-top: none;
  position: sticky;
  top: 0;
  z-index: 1;
}

.rows {
  max-height: 480px;
  overflow-y: auto;
}

.row:hover { background: #f9fafb; }

.row.src-event { background: #fafafa; }
.row.src-event:hover { background: #f3f4f6; }
.row.src-network  { background: #ffffff; }
.row.src-marker    { background: #fef9c3; }
.row.src-marker:hover { background: #fef08a; }

.cell {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.c-time { color: #6b7280; }
.c-player, .c-play, .c-attempt {
  color: #374151;
  font-variant-numeric: tabular-nums;
}

.src-tag {
  display: inline-block;
  padding: 1px 6px;
  border-radius: 8px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  border: 1px solid transparent;
}
.tag-event   { background: #e5e7eb; color: #374151; border-color: #d1d5db; }
.tag-network { background: #dbeafe; color: #1e3a8a; border-color: #bfdbfe; }
.tag-marker  { background: #fde68a; color: #92400e; border-color: #fcd34d; }

.sortable { cursor: pointer; user-select: none; }
.sortable:hover { color: #1f2937; }
.arr { font-size: 9px; margin-left: 2px; }

/* Field column — flowing kv chips. Each chip wraps when the row is
 * narrow; long values clamp on overflow so the row stays one
 * "paragraph" of fields rather than smearing across the layout. */
.c-fields {
  white-space: normal;
  overflow: hidden;
  display: flex;
  flex-wrap: wrap;
  gap: 3px 8px;
  line-height: 1.55;
}
.kv {
  background: #f3f4f6;
  border: 1px solid #e5e7eb;
  border-radius: 4px;
  padding: 0 5px;
  font-size: 10px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: inline-block;
}
.kv-name {
  color: #4b5563;
  font-weight: 600;
}
.kv-value {
  color: #111827;
}
.kv-empty {
  color: #9ca3af;
  font-size: 10px;
}

/* event_name column — dedicated cell after attempt_id. Lifted out
 * of the alphabetical chip list since the operator always wants to
 * see "stall" / "downshift" / "buffering_start" / etc. on a fixed
 * column position regardless of mode or sort. */
.c-eventname {
  overflow: hidden;
  text-overflow: ellipsis;
}
.event-name {
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  border-radius: 4px;
  padding: 1px 8px;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.3px;
}
/* Source-tinted: events read as the "interesting thing", so they
 * keep the loud amber. Snapshots are background heartbeat-ish data,
 * so use a calmer grey-blue. */
.event-name-marker   { background: #f59e0b; color: #fff; }
.event-name-event    { background: #e5e7eb; color: #1f2937; font-weight: 600; }
.event-name-network  { background: #dbeafe; color: #1e3a8a; }
.event-name-empty {
  color: #9ca3af;
  font-size: 11px;
}

/* Raw column — JSON blob, monospace, capped height. The full value
 * lives in the title attr for hover, the cell shows the head with
 * inner scroll for the rest. */
.c-raw {
  overflow: hidden;
  max-width: 100%;
}
.raw-pre {
  margin: 0;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  font-size: 10px;
  line-height: 1.35;
  color: #1f2937;
  background: #f9fafb;
  border: 1px solid #e5e7eb;
  border-radius: 3px;
  padding: 4px 6px;
  max-height: 96px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

/* Source-specific tints on the kv chips so a glance at a row tells
 * you which table it came from even when the fields column is the
 * dominant visual area. */
.row.src-network .kv {
  background: #eff6ff;
  border-color: #bfdbfe;
}
.row.src-network .kv-name { color: #1e3a8a; }

.row.src-marker .kv {
  background: #fef3c7;
  border-color: #fcd34d;
}
.row.src-marker .kv-name { color: #92400e; }
</style>
