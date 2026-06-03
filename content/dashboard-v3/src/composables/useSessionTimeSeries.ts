/**
 * useSessionTimeSeries — single client-side consumer of the
 * /api/v2/timeseries SSE endpoint. One subscription per
 * (player_id, play_id); exposes per-stream caches with the same
 * range-query API for every renderer (line charts, events
 * timeline, network log, focus-bar rail).
 *
 * The endpoint emits typed events:
 *   - meta:        wire shape of the connection (streams, columns, live)
 *   - sample:      one row from session_snapshots
 *   - network:     one row from network_requests
 *   - event:       (future) one kind/priority-classified event
 *   - heartbeat:   keepalive (no data)
 *   - complete:    archive replay finished, server is closing
 *   - stream_error: server-side error; client may want to reconnect
 *
 * Reconnect via the standard EventSource Last-Event-ID mechanism —
 * each row's `id:` is its stable fingerprint (entry_fingerprint for
 * network, ts string for samples) so the server resumes from where
 * we left off without dupes.
 *
 * Throughput: incoming rows are queued and flushed in 1 s buckets so
 * the renderer chain (allRows recompute, chart insert, vis-timeline
 * DataSet diff, Vue patch) re-runs at most once per second per
 * stream regardless of arrival rate. Keeps the JS thread responsive
 * even when the forwarder bursts a backfill of thousands of rows.
 */
import { ref, shallowRef, triggerRef, watch, onScopeDispose, isRef, type Ref } from 'vue';
import {
  tsOf,
  binarySearchLE,
  insertSortedDedup,
  inRangeAsc,
  evictOutsideViewport,
} from './sessionTimeSeriesUtils';
// SSE event names this stream produces. Issue #474 Milestone C
// dropped 'marker' (the session_markers stream retired) and added
// 'control' (control_events — proxy/harness action log). Issue #486
// added 'avmetrics' for the iOS 18 AVMetrics raw-event comparison
// stream.
const V3_EVENT_TYPES = ['meta', 'event', 'network', 'control', 'avmetrics', 'heartbeat', 'complete', 'stream_error'];

/**
 * Stream<T> — the per-stream surface every renderer consumes. Read
 * `version` to know when to re-query; range-query via inRange or
 * lastAt; consult `rangeBounds` for the brush rail's left/right
 * edges; show `loading` / `error` in the UI.
 */
export interface Stream<T> {
  inRange(t1: number, t2: number): T[];
  lastAt(t: number): T | null;
  /** Bumped whenever the underlying cache changes shape. Use as a
   *  trigger source in computed() / watch() — read it to subscribe. */
  version: Ref<number>;
  /** Min/max ts cached client-side. Lazy-fill: caller can request
   *  ranges outside these bounds to trigger a backfill. */
  rangeBounds: Ref<{ min: number; max: number } | null>;
  loading: Ref<boolean>;
  error: Ref<string | null>;
}

export interface UseSessionTimeSeriesOpts {
  /** Comma list of streams to subscribe to. Default: all three. */
  streams?: ('events' | 'network' | 'control' | 'avmetrics')[];
  /** Bundle names. Default: charts_minimal,lanes_v1,network for samples+network. */
  bundles?: string[];
  /** Ad-hoc field list — applied to every enabled stream as a
   *  convenience. CH rejects unknown columns per stream → caller
   *  sees a 400 cleanly. */
  fields?: string[];
  /** Initial backfill window. Defaults to last 10 minutes when
   *  unset; the brush rail will then drive subsequent fetches via
   *  range queries.
   *
   *  Accepts either a plain number (set-once at subscription time)
   *  or a Ref — when a Ref, changes trigger an SSE re-subscribe with
   *  the new window. SessionViewer uses the Ref form so its
   *  "show context" toggle can widen the time range live without
   *  rebuilding the composable. */
  fromMs?: number | Ref<number | null | undefined>;
  toMs?: number | Ref<number | null | undefined>;
  /** Per-(samples) downsample bucket. Live deltas always full-res. */
  strideMs?: number;
  /** Per-stream max delta rate hint. Server coalesces above this. */
  maxHz?: number;
}

export interface UseSessionTimeSeriesReturn {
  events: Stream<Record<string, unknown>>;
  network: Stream<Record<string, unknown>>;
  control: Stream<Record<string, unknown>>;
  /** iOS 18 AVMetrics raw event stream (issue #486 spike). Empty
   *  unless the caller opted in via `streams: [..., 'avmetrics']`. */
  avmetrics: Stream<Record<string, unknown>>;
  /** True if the server is actively tailing this stream. False once
   *  it sends `event:complete` (archive replay or play ended). */
  live: Ref<boolean>;
  /** SSE connection state — useful for the reconnect badge in UI. */
  connectionState: Ref<'connecting' | 'open' | 'closed'>;
  /** Force a reconnect (e.g. after an explicit "refresh" button). */
  reconnect: () => void;
  /** Detach the EventSource and free resources. */
  close: () => void;
}

const DEFAULT_BACKFILL_MS = 10 * 60 * 1000;
const FLUSH_INTERVAL_MS = 1000;

/**
 * fpOf — stable per-row fingerprint. Used by insertSortedDedup to
 * collapse duplicates (re-emitted backfill rows or an SSE retry that
 * replays via Last-Event-ID). Preference order:
 *   1. explicit entry_fingerprint (network rows; ID-stable in CH)
 *   2. ts string (samples; one snapshot per ts per session)
 *   3. ms-since-epoch fallback for anything else
 *
 * Critically this MUST read from the argument — closing over a
 * specific row's fingerprint and returning that for every call
 * causes insertSortedDedup to think every existing element matches
 * and overwrite the tail. (See the 2026-05-15 fix in drainQueue.)
 */
function fpOf(row: Record<string, unknown>): string {
  return String(
    (row.entry_fingerprint as string | undefined) ??
    (row.ts as string | undefined) ??
    tsOf(row),
  );
}

/**
 * useSessionTimeSeries — main entry point. Pass reactive refs for
 * playerId + playId; the composable re-subscribes whenever either
 * changes (so picker-swap on testing.html works without a key= hack).
 */
export function useSessionTimeSeries(
  playerId: Ref<string>,
  playId: Ref<string | null>,
  opts: UseSessionTimeSeriesOpts = {},
): UseSessionTimeSeriesReturn {

  // Normalise fromMs / toMs to refs so the watcher below picks up
  // changes uniformly whether the caller passed a plain number or
  // a Ref. Plain numbers become non-reactive refs that just sit at
  // their initial value.
  const fromMsRef: Ref<number | null | undefined> = isRef(opts.fromMs)
    ? (opts.fromMs as Ref<number | null | undefined>)
    : ref(opts.fromMs);
  const toMsRef: Ref<number | null | undefined> = isRef(opts.toMs)
    ? (opts.toMs as Ref<number | null | undefined>)
    : ref(opts.toMs);

  const eventsArr = shallowRef<Record<string, unknown>[]>([]);
  const networkArr = shallowRef<Record<string, unknown>[]>([]);
  const controlArr = shallowRef<Record<string, unknown>[]>([]);
  const avmetricsArr = shallowRef<Record<string, unknown>[]>([]);

  const eventsVersion = ref(0);
  const networkVersion = ref(0);
  const controlVersion = ref(0);
  const avmetricsVersion = ref(0);

  const eventsBounds = ref<{ min: number; max: number } | null>(null);
  const networkBounds = ref<{ min: number; max: number } | null>(null);
  const controlBounds = ref<{ min: number; max: number } | null>(null);
  const avmetricsBounds = ref<{ min: number; max: number } | null>(null);

  const eventsLoading = ref(false);
  const networkLoading = ref(false);
  const controlLoading = ref(false);
  const avmetricsLoading = ref(false);

  const eventsError = ref<string | null>(null);
  const networkError = ref<string | null>(null);
  const controlError = ref<string | null>(null);
  const avmetricsError = ref<string | null>(null);

  const live = ref(true);
  const connectionState = ref<'connecting' | 'open' | 'closed'>('closed');

  let es: EventSource | null = null;
  let lastEventId = '';

  // Per-stream pending queues. Rows arrive on the SSE event
  // listeners and are appended here without touching reactivity.
  // The flush timer drains all three queues every FLUSH_INTERVAL_MS,
  // sorts/dedupes them into the main arrays in a single pass, then
  // bumps version + triggerRef once per stream that received rows.
  // This caps re-render cost at one pass per stream per second
  // regardless of upstream burst rate.
  const eventsPending: Record<string, unknown>[] = [];
  const networkPending: Record<string, unknown>[] = [];
  const controlPending: Record<string, unknown>[] = [];
  const avmetricsPending: Record<string, unknown>[] = [];
  let flushTimer: ReturnType<typeof setInterval> | null = null;

  function teardown() {
    if (es) {
      try { es.close(); } catch { /* ignore */ }
      es = null;
    }
    if (flushTimer) {
      clearInterval(flushTimer);
      flushTimer = null;
    }
    connectionState.value = 'closed';
  }

  function resetCaches() {
    eventsArr.value = [];
    networkArr.value = [];
    controlArr.value = [];
    avmetricsArr.value = [];
    eventsPending.length = 0;
    networkPending.length = 0;
    controlPending.length = 0;
    avmetricsPending.length = 0;
    eventsVersion.value++;
    networkVersion.value++;
    controlVersion.value++;
    avmetricsVersion.value++;
    eventsBounds.value = null;
    networkBounds.value = null;
    controlBounds.value = null;
    avmetricsBounds.value = null;
    eventsLoading.value = false;
    networkLoading.value = false;
    controlLoading.value = false;
    avmetricsLoading.value = false;
    eventsError.value = null;
    networkError.value = null;
    controlError.value = null;
    avmetricsError.value = null;
    lastEventId = '';
  }

  /** Build the SSE URL from current opts + identity refs. */
  function buildUrl(): string | null {
    const pid = playerId.value;
    if (!pid) return null;
    const params = new URLSearchParams();
    params.set('player_id', pid);
    if (playId.value) params.set('play_id', playId.value);
    const streams = opts.streams ?? ['events', 'network'];
    params.set('streams', streams.join(','));
    if (opts.bundles && opts.bundles.length) {
      params.set('bundles', opts.bundles.join(','));
    } else {
      // Default bundle picks per stream; keeps the wire ergonomic.
      const defaults: string[] = [];
      if (streams.includes('events')) defaults.push('charts_minimal', 'lanes_v1');
      if (streams.includes('network')) defaults.push('network');
      if (streams.includes('control')) defaults.push('control');
      if (streams.includes('avmetrics')) defaults.push('avmetrics');
      if (defaults.length) params.set('bundles', defaults.join(','));
    }
    if (opts.fields && opts.fields.length) {
      params.set('fields', opts.fields.join(','));
    }
    const fromMs = fromMsRef.value;
    const toMs = toMsRef.value;
    if (fromMs && fromMs > 0) {
      params.set('from', new Date(fromMs).toISOString());
    } else if (!playId.value) {
      // Live mode (no specific play_id) and no explicit from: default
      // backfill is the last 10 minutes so a fresh page load doesn't
      // drag down a 24h history. Archive replay (playId set) gets no
      // default `from` so the server returns ALL rows for that play
      // (the play's bounded range naturally caps the row count below
      // the server's per-stream limit). When playId is null AND an
      // explicit fromMs is set (SessionViewer's "show context" toggle),
      // we fall through the first branch above with the operator-
      // supplied window.
      params.set('from', new Date(Date.now() - DEFAULT_BACKFILL_MS).toISOString());
    }
    if (toMs && toMs > 0) {
      params.set('to', new Date(toMs).toISOString());
    }
    if (opts.strideMs && opts.strideMs > 0) params.set('stride_ms', String(opts.strideMs));
    if (opts.maxHz && opts.maxHz > 0) params.set('max_hz', String(opts.maxHz));
    return '/analytics/api/v2/timeseries?' + params.toString();
  }

  function connect() {
    teardown();
    resetCaches();
    const url = buildUrl();
    if (!url) return;
    connectionState.value = 'connecting';
    eventsLoading.value = true;
    networkLoading.value = true;

    es = new EventSource(url);
    es.onopen = () => { connectionState.value = 'open'; };
    es.onerror = () => {
      // EventSource auto-reconnects on transient network blips; we
      // only flip to 'closed' once the browser gives up. Surface
      // connecting/closed states for the SSE badge in the UI.
      if (es && es.readyState === EventSource.CLOSED) connectionState.value = 'closed';
      else connectionState.value = 'connecting';
    };
    for (const t of V3_EVENT_TYPES) {
      es.addEventListener(t, (ev: MessageEvent) => {
        handleStreamEvent(t, ev.data, ev.lastEventId);
      });
    }

    flushTimer = setInterval(flushAll, FLUSH_INTERVAL_MS);
  }

  /** Dispatch one SSE event by type. Same shape regardless of the
   *  event source so adding a new event type only requires adding
   *  a case below + listing it in V3_EVENT_TYPES. */
  function handleStreamEvent(eventType: string, data: string, evtLastEventId: string) {
    if (evtLastEventId) lastEventId = evtLastEventId;
    switch (eventType) {
      case 'meta':
        try {
          const m = JSON.parse(data);
          if (typeof m?.live === 'boolean') live.value = m.live;
        } catch { /* ignore malformed meta */ }
        return;
      case 'event':
        enqueueRow(data, eventsPending);
        return;
      case 'network':
        enqueueRow(data, networkPending);
        return;
      case 'control':
        enqueueRow(data, controlPending);
        return;
      case 'avmetrics':
        enqueueRow(data, avmetricsPending);
        return;
      case 'heartbeat':
        return;
      case 'complete':
        // Drain anything queued before signalling done so the final
        // archive rows make it into the cache.
        flushAll();
        live.value = false;
        eventsLoading.value = false;
        networkLoading.value = false;
        controlLoading.value = false;
        avmetricsLoading.value = false;
        teardown();
        return;
      case 'stream_error':
        try {
          const m = JSON.parse(data);
          const msg = String(m?.message ?? 'stream error');
          eventsError.value = msg;
          networkError.value = msg;
          controlError.value = msg;
          avmetricsError.value = msg;
        } catch {
          eventsError.value = 'stream error';
        }
        return;
    }
  }

  /** Append one parsed row to the named pending queue. Cheap: no
   *  reactive writes happen here. */
  function enqueueRow(data: string, queue: Record<string, unknown>[]) {
    let row: Record<string, unknown>;
    try { row = JSON.parse(data); } catch { return; }
    queue.push(row);
  }

  /** Drain all three pending queues into their backing arrays in one
   *  pass per stream. Bumps version + triggerRef once per stream
   *  that actually received rows. */
  function flushAll() {
    drainQueue(eventsPending, eventsArr, eventsVersion, eventsBounds, eventsLoading);
    drainQueue(networkPending, networkArr, networkVersion, networkBounds, networkLoading);
    drainQueue(controlPending, controlArr, controlVersion, controlBounds, controlLoading);
    drainQueue(avmetricsPending, avmetricsArr, avmetricsVersion, avmetricsBounds, avmetricsLoading);
  }

  function drainQueue(
    pending: Record<string, unknown>[],
    arr: Ref<Record<string, unknown>[]>,
    versionRef: Ref<number>,
    boundsRef: Ref<{ min: number; max: number } | null>,
    loadingRef: Ref<boolean>,
  ) {
    if (pending.length === 0) return;
    const list = arr.value;
    let curMin = boundsRef.value?.min ?? Infinity;
    let curMax = boundsRef.value?.max ?? -Infinity;
    for (const row of pending) {
      const ms = tsOf(row);
      if (!Number.isFinite(ms)) continue;
      // keyOf / fpOf must inspect the element passed in, not close
      // over the new row — insertSortedDedup calls them on existing
      // arr[i] entries to find dedupe candidates. If they ignore the
      // argument and return the new row's values, every insert
      // overwrites the last existing row and the cache stays at
      // length 1. (Bug fixed 2026-05-15.)
      insertSortedDedup(list, row, tsOf, fpOf);
      if (ms < curMin) curMin = ms;
      if (ms > curMax) curMax = ms;
    }
    pending.length = 0;
    // shallowRef + triggerRef: avoid Vue's deep proxy overhead on the
    // potentially-large row array; downstream watchers re-fire.
    triggerRef(arr);
    versionRef.value++;
    if (curMin !== Infinity) {
      const cur = boundsRef.value;
      if (!cur || cur.min !== curMin || cur.max !== curMax) {
        boundsRef.value = { min: curMin, max: curMax };
      }
    }
    if (loadingRef.value) loadingRef.value = false;
  }

  // Re-subscribe ONLY when the (playerId, playId) string values
  // actually change AND only after they've been stable for at
  // least RECONNECT_DEBOUNCE_MS. The upstream usePlayer cache
  // absorbs all-pool SSE updates that occasionally cause our
  // identity refs to ping-pong; without debouncing, the EventSource
  // tears down and re-opens every 100–300 ms, each reconnect
  // spawning a fresh CH backfill SELECT that piles up faster than
  // ClickHouse can drain (TOO_MANY_SIMULTANEOUS_QUERIES). 500 ms
  // is enough to absorb the typical churn without being
  // user-noticeable on a genuine player swap.
  const RECONNECT_DEBOUNCE_MS = 500;
  let lastPid = '';
  let lastPlayid: string | null = null;
  let lastFrom: number | null = null;
  let lastTo: number | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  watch(
    // fromMsRef / toMsRef join playerId + playId in the watch list so
    // the SSE re-subscribes when SessionViewer's "show context" toggle
    // widens the time window. Re-subscribe debounce + cache-reset
    // semantics apply uniformly — caches are cleared on every change
    // so a wider window doesn't leak rows from the prior subscription.
    [playerId, playId, fromMsRef, toMsRef],
    ([pid, plyid, fromMs, toMs]) => {
      const newPid = pid ?? '';
      const newPlayid = plyid ?? null;
      const newFrom = fromMs ?? null;
      const newTo = toMs ?? null;
      if (newPid === lastPid && newPlayid === lastPlayid
          && newFrom === lastFrom && newTo === lastTo) return;
      lastPid = newPid;
      lastPlayid = newPlayid;
      lastFrom = newFrom;
      lastTo = newTo;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      // Immediate teardown + resetCaches so the OLD player's EventSource
      // stops streaming and the cache doesn't leak stale samples into
      // the next subscriber's drain. Without this, the 500 ms debounce
      // below leaves the old stream open AND the cache populated;
      // EventsTimeline + the charts then ingest old-player rows as if
      // they belonged to the new player_id (lane "freeze" bug observed
      // 2026-05-16 — picker switch sometimes showed previous player's
      // VARIANT shifts). connect() runs after the debounce; it will
      // teardown+reset again, idempotently.
      teardown();
      resetCaches();
      reconnectTimer = setTimeout(() => {
        reconnectTimer = null;
        const finalPid = playerId.value ?? '';
        const finalPlayid = playId.value ?? null;
        const finalFrom = fromMsRef.value ?? null;
        const finalTo = toMsRef.value ?? null;
        if (finalPid !== lastPid || finalPlayid !== lastPlayid
            || finalFrom !== lastFrom || finalTo !== lastTo) {
          lastPid = finalPid;
          lastPlayid = finalPlayid;
          lastFrom = finalFrom;
          lastTo = finalTo;
        }
        connect();
      }, RECONNECT_DEBOUNCE_MS);
    },
    { immediate: true },
  );

  onScopeDispose(() => {
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    teardown();
  });

  // Stream<T> facade builders.
  function makeStream<T extends Record<string, unknown>>(
    arr: Ref<T[]>,
    versionRef: Ref<number>,
    boundsRef: Ref<{ min: number; max: number } | null>,
    loadingRef: Ref<boolean>,
    errorRef: Ref<string | null>,
  ): Stream<T> {
    return {
      inRange(t1, t2) { return inRangeAsc(arr.value, t1, t2, tsOf); },
      lastAt(t) {
        const idx = binarySearchLE(arr.value, t, tsOf);
        return idx >= 0 ? arr.value[idx] : null;
      },
      version: versionRef,
      rangeBounds: boundsRef,
      loading: loadingRef,
      error: errorRef,
    };
  }

  // Recency cap (issue #582). The previous eviction called
  // evictOutsideViewport with the streams' OWN merged data bounds —
  // which keeps `[min − 2·span, max + 2·span]` of the entire cached
  // range, i.e. it never actually drops anything as the range grows.
  // So the caches grew unbounded for the life of the tab (a primary
  // contributor to the 12 GB renderer). Replace with a hard recency
  // cap: keep only the most recent N rows per stream. Arrays are sorted
  // ascending by ts (insertSortedDedup), so the oldest are at the front.
  // Panning past the retained window triggers a fresh range fetch at the
  // upper layer.
  const SOFT_CAP_SAMPLES = 10000;
  const SOFT_CAP_NETWORK = 2000;
  const SOFT_CAP_EVENTS = 10000;
  function trimToCap<T>(arr: T[], cap: number): boolean {
    if (arr.length > cap) {
      arr.splice(0, arr.length - cap);
      return true;
    }
    return false;
  }
  watch(
    [eventsVersion, networkVersion, controlVersion, avmetricsVersion],
    () => {
      if (trimToCap(eventsArr.value, SOFT_CAP_SAMPLES)) triggerRef(eventsArr);
      if (trimToCap(networkArr.value, SOFT_CAP_NETWORK)) triggerRef(networkArr);
      if (trimToCap(controlArr.value, SOFT_CAP_EVENTS)) triggerRef(controlArr);
      if (trimToCap(avmetricsArr.value, SOFT_CAP_EVENTS)) triggerRef(avmetricsArr);
    },
  );

  return {
    events: makeStream(eventsArr, eventsVersion, eventsBounds, eventsLoading, eventsError),
    network: makeStream(networkArr, networkVersion, networkBounds, networkLoading, networkError),
    control: makeStream(controlArr, controlVersion, controlBounds, controlLoading, controlError),
    avmetrics: makeStream(avmetricsArr, avmetricsVersion, avmetricsBounds, avmetricsLoading, avmetricsError),
    live,
    connectionState,
    reconnect: connect,
    close: teardown,
  };
}

function mergedBounds(
  ...sources: ({ min: number; max: number } | null)[]
): { min: number; max: number } | null {
  let min = Infinity;
  let max = -Infinity;
  let any = false;
  for (const x of sources) {
    if (!x) continue;
    if (x.min < min) min = x.min;
    if (x.max > max) max = x.max;
    any = true;
  }
  if (!any) return null;
  return { min, max };
}
