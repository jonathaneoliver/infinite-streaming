/**
 * Pure helpers shared between useSessionTimeSeries and consumers
 * that want to do their own range queries against the same data
 * (e.g. compare-charts overlays in the future).
 *
 * No Vue imports — keep this side-effect-free so it can be unit-
 * tested without a Vue runtime.
 */

/**
 * tsOf — coerces a row's timestamp field to ms-since-epoch.
 *
 * Accepts:
 *   - DateTime64(3) string from CH JSONEachRow ("YYYY-MM-DD HH:MM:SS.fff")
 *   - ISO-8601 string ("2026-05-15T13:17:15.375Z")
 *   - number (already ms)
 *
 * Returns NaN for unparseable inputs; callers filter these out.
 *
 * The CH-format string lacks a trailing 'Z' but represents UTC; we
 * append 'Z' to force Date.parse to treat it as UTC instead of the
 * browser's local TZ.
 */
export function tsOf(row: unknown): number {
  if (row == null) return NaN;
  const r = row as Record<string, unknown>;
  const v = r.ts ?? r.timestamp;
  if (typeof v === 'number') return v;
  if (typeof v !== 'string' || !v) return NaN;
  // CH format "YYYY-MM-DD HH:MM:SS.fff" — replace the space with 'T'
  // and append 'Z'. ISO format already parses correctly.
  if (v.length > 10 && v.charAt(10) === ' ') {
    return Date.parse(v.replace(' ', 'T') + 'Z');
  }
  return Date.parse(v);
}

/**
 * binarySearch — index of greatest element ≤ target by `keyOf`.
 * Returns -1 if every element is greater than target.
 *
 * Used by lastAt(t) for chart cursor positioning ("what's the most
 * recent sample at time t?") — O(log n) on a sorted-asc array.
 */
export function binarySearchLE<T>(arr: T[], target: number, keyOf: (e: T) => number): number {
  let lo = 0;
  let hi = arr.length - 1;
  let best = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >>> 1;
    const k = keyOf(arr[mid]);
    if (k <= target) {
      best = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return best;
}

/**
 * insertSortedDedup — splice `entry` into `arr` (sorted asc by
 * keyOf) so the result stays sorted. If an existing element shares
 * the dedup fingerprint with `entry`, replace it in place. Returns
 * true if the array was modified.
 *
 * Used to merge SSE deltas into the cache as they arrive. O(log n)
 * search + O(n) shift; chart render cost dominates.
 */
export function insertSortedDedup<T>(
  arr: T[],
  entry: T,
  keyOf: (e: T) => number,
  fpOf: (e: T) => string,
): boolean {
  const targetK = keyOf(entry);
  const targetFP = fpOf(entry);
  // Binary search for the first index whose key > targetK.
  let lo = 0;
  let hi = arr.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (keyOf(arr[mid]) <= targetK) lo = mid + 1;
    else hi = mid;
  }
  // Sweep backward from lo-1 to check for an existing fingerprint
  // match at the same key (multiple rows can share a ts in network
  // streams). Bounded sweep — if we don't find a match within the
  // same-key cluster, splice.
  for (let i = lo - 1; i >= 0 && keyOf(arr[i]) === targetK; i--) {
    if (fpOf(arr[i]) === targetFP) {
      arr[i] = entry;
      return true;
    }
  }
  arr.splice(lo, 0, entry);
  return true;
}

/**
 * inRangeAsc — slice the rows in [t1, t2] inclusive from a sorted-
 * asc array. Binary-searches both ends for O(log n + k).
 */
export function inRangeAsc<T>(arr: T[], t1: number, t2: number, keyOf: (e: T) => number): T[] {
  if (!arr.length || t1 > t2) return [];
  // Find first index whose key ≥ t1.
  let lo = 0;
  let hi = arr.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (keyOf(arr[mid]) < t1) lo = mid + 1;
    else hi = mid;
  }
  const startIdx = lo;
  // Find first index whose key > t2.
  lo = startIdx;
  hi = arr.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (keyOf(arr[mid]) <= t2) lo = mid + 1;
    else hi = mid;
  }
  return arr.slice(startIdx, lo);
}

/**
 * evictOutsideViewport — drop entries whose ts is outside the
 * eviction window `[viewport.t1 − 2·span, viewport.t2 + 2·span]`.
 * Mutates `arr` in place; returns the number of entries removed.
 *
 * Caller calls this when the cache exceeds its soft memory budget;
 * the 2·span guardband keeps adjacent panning cheap (no immediate
 * re-fetch when the operator nudges the brush a few seconds).
 */
export function evictOutsideViewport<T>(
  arr: T[],
  viewportMin: number,
  viewportMax: number,
  keyOf: (e: T) => number,
): number {
  if (!arr.length) return 0;
  const span = Math.max(0, viewportMax - viewportMin);
  const lo = viewportMin - 2 * span;
  const hi = viewportMax + 2 * span;
  const kept: T[] = [];
  for (const e of arr) {
    const k = keyOf(e);
    if (k >= lo && k <= hi) kept.push(e);
  }
  const removed = arr.length - kept.length;
  if (removed > 0) {
    // Replace contents in place so existing references stay valid.
    arr.length = 0;
    Array.prototype.push.apply(arr, kept);
  }
  return removed;
}
