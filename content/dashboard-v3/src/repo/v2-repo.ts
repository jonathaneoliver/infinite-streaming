/**
 * Typed HTTP wrapper around /api/v2/*. The composables in src/composables
 * lean on this for every wire call. No business logic lives here —
 * just request/response shape + headers (If-Match for concurrency,
 * Content-Type for merge-patch, etc.).
 *
 * Throws `RepoError` (with `.status`) on non-2xx so TanStack Query's
 * onError / mutation rollback can branch on 412 / 5xx.
 */
import type { components } from '@/types/v2';

export type PlayerRecord = components['schemas']['PlayerRecord'];
export type PlayerGroup = components['schemas']['PlayerGroup'];
export type FaultRule = components['schemas']['FaultRule'];
export type Shape = components['schemas']['Shape'];
export type Pattern = components['schemas']['Pattern'];
export type TransportFault = components['schemas']['TransportFault'];
export type TransferTimeouts = components['schemas']['TransferTimeouts'];
export type ContentManipulation = components['schemas']['ContentManipulation'];
export type Labels = components['schemas']['Labels'];
export type NetworkLogEntry = components['schemas']['NetworkLogEntry'];

/**
 * Single source of truth for the "is this id pointing at a live
 * server-managed player vs. a finished play served from the
 * ClickHouse archive" branch. Archive ids are minted by the
 * session-viewer page as `archive:<sessionId>:<playId>` so getPlayer
 * et al. can route to the local snapshot store instead of HTTP, and
 * SessionDisplay can skip live-only behaviours (SSE subscription,
 * 1 Hz cache patches, live-edge brush chase).
 *
 * Use this helper instead of inlining `id.startsWith('archive:')` so
 * that future renaming of the prefix happens in one place.
 */
export function isLivePlayerId(id: string): boolean {
  return !id.startsWith('archive:');
}

export class RepoError extends Error {
  constructor(
    public readonly status: number,
    public readonly body: unknown,
    public readonly etag?: string,
  ) {
    super(`HTTP ${status}`);
    this.name = 'RepoError';
  }
}

/** Strip surrounding double quotes from a strong ETag header value. */
function parseETag(raw: string | null): string | undefined {
  if (!raw) return undefined;
  const m = raw.match(/^W?\/?"([^"]+)"$/);
  return m ? m[1] : raw;
}

async function request<T>(
  url: string,
  init: RequestInit & { ifMatch?: string } = {},
): Promise<{ data: T; etag?: string }> {
  const headers = new Headers(init.headers);
  if (init.ifMatch) headers.set('If-Match', `"${init.ifMatch}"`);
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  const resp = await fetch(url, { ...init, headers });
  const etag = parseETag(resp.headers.get('ETag'));
  if (!resp.ok) {
    let body: unknown = undefined;
    try {
      body = await resp.json();
    } catch {
      /* not JSON */
    }
    throw new RepoError(resp.status, body, etag);
  }
  // 204 No Content has no body
  if (resp.status === 204) return { data: undefined as unknown as T, etag };
  return { data: (await resp.json()) as T, etag };
}

// ----- Players -----

export async function listPlayers(): Promise<PlayerRecord[]> {
  const { data } = await request<{ items: PlayerRecord[] }>('/api/v2/players?include=raw');
  return data.items ?? [];
}

// Case-insensitive player_id resolution.
//
// The v1 SessionByPlayerID lookup the v2 server delegates to matches
// the EXACT string the player sent on the wire — iPad / Apple TV use
// uppercase CFUUIDs, web clients use lowercase. Operators paste URLs
// or type ids in arbitrary case; we must accept any casing and route
// to whatever the server stored.
//
// Strategy: send the user-supplied id verbatim first (almost always a
// hit). If the server 404s, list players, find a case-insensitive
// match, cache the canonical id, retry with that. Subsequent calls
// (PATCH / DELETE / fault rule ops) all run through `canonicalIdFor()`
// which consults this cache.
const canonicalIdCache = new Map<string, string>();

export function canonicalIdFor(id: string): string {
  return canonicalIdCache.get(id.toLowerCase()) ?? id;
}

function rememberCanonical(id: string) {
  if (!id) return;
  canonicalIdCache.set(id.toLowerCase(), id);
}

async function resolveCanonicalId(id: string): Promise<string | null> {
  const cached = canonicalIdCache.get(id.toLowerCase());
  if (cached) return cached;
  const players = await listPlayers();
  for (const p of players) {
    canonicalIdCache.set(p.id.toLowerCase(), p.id);
  }
  return canonicalIdCache.get(id.toLowerCase()) ?? null;
}

// Side-channel store for archive/replay PlayerRecord objects. The v3
// session-viewer page primes this map as it scrubs through historical
// snapshots, then calls qc.setQueryData/invalidate so the standard
// usePlayer() chain reads from cache without going to the network.
// Archive ids carry an `archive:` prefix; anything else falls through
// to the live `/api/v2/players/<id>` fetch path.
const archiveStore = new Map<string, PlayerRecord>();
const archiveNetworkStore = new Map<string, NetworkLogEntry[]>();
export function setArchivePlayer(id: string, p: PlayerRecord) {
  archiveStore.set(id, p);
}
export function setArchiveNetworkLog(id: string, rows: NetworkLogEntry[]) {
  archiveNetworkStore.set(id, rows);
}
export function clearArchivePlayer(id: string) {
  archiveStore.delete(id);
  archiveNetworkStore.delete(id);
}

export async function getPlayer(playerId: string): Promise<{ player: PlayerRecord; etag?: string }> {
  if (!isLivePlayerId(playerId)) {
    const cached = archiveStore.get(playerId);
    if (cached) return { player: cached, etag: undefined };
    // Throw a 4xx so usePlayer's refetchOnMount guard treats it as a
    // permanent miss and stops re-firing. The session-viewer page
    // primes the cache *before* mounting the child components, so the
    // first call should already find the entry — this branch only
    // fires if the page mounts before the snapshot stream resolves.
    throw Object.assign(new Error('archive snapshot not yet loaded'), { status: 404 });
  }
  const id = canonicalIdFor(playerId);
  try {
    const { data, etag } = await request<PlayerRecord>(
      `/api/v2/players/${encodeURIComponent(id)}?include=raw`,
    );
    rememberCanonical(data.id);
    return { player: data, etag };
  } catch (err: any) {
    if (err?.status !== 404) throw err;
    const canonical = await resolveCanonicalId(id);
    if (!canonical || canonical === id) throw err;
    const { data, etag } = await request<PlayerRecord>(
      `/api/v2/players/${encodeURIComponent(canonical)}?include=raw`,
    );
    rememberCanonical(data.id);
    return { player: data, etag };
  }
}

export async function patchPlayer(
  playerId: string,
  patch: Partial<{
    shape: Partial<Shape>;
    transfer_timeouts: Partial<TransferTimeouts>;
    content: Partial<ContentManipulation>;
    labels: Labels;
  }>,
  ifMatch?: string,
): Promise<{ player: PlayerRecord; etag?: string }> {
  const id = canonicalIdFor(playerId);
  const { data, etag } = await request<PlayerRecord>(
    `/api/v2/players/${encodeURIComponent(id)}`,
    {
      method: 'PATCH',
      ifMatch,
      headers: { 'Content-Type': 'application/merge-patch+json' },
      body: JSON.stringify(patch),
    },
  );
  return { player: data, etag };
}

export async function createPlayer(body: Partial<PlayerRecord>): Promise<PlayerRecord> {
  const { data } = await request<PlayerRecord>('/api/v2/players', {
    method: 'POST',
    body: JSON.stringify(body),
  });
  return data;
}

export async function deletePlayer(playerId: string, ifMatch?: string): Promise<void> {
  await request<void>(`/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}`, {
    method: 'DELETE',
    ifMatch,
  });
}

// ----- Fault rules (per-player sub-resource) -----

export async function upsertFaultRule(
  playerId: string,
  rule: FaultRule,
  ifMatch?: string,
): Promise<{ rule: FaultRule; etag?: string }> {
  // Try PATCH (rule exists) → 404 falls through to POST (create).
  if (rule.id) {
    try {
      const { data, etag } = await request<FaultRule>(
        `/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}/fault_rules/${encodeURIComponent(rule.id)}`,
        {
          method: 'PATCH',
          ifMatch,
          headers: { 'Content-Type': 'application/merge-patch+json' },
          body: JSON.stringify(rule),
        },
      );
      return { rule: data, etag };
    } catch (err) {
      if (!(err instanceof RepoError) || err.status !== 404) throw err;
      /* fall through to POST */
    }
  }
  const { data, etag } = await request<FaultRule>(
    `/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}/fault_rules`,
    {
      method: 'POST',
      ifMatch,
      body: JSON.stringify(rule),
    },
  );
  return { rule: data, etag };
}

export async function deleteFaultRule(
  playerId: string,
  ruleId: string,
  ifMatch?: string,
): Promise<void> {
  await request<void>(
    `/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}/fault_rules/${encodeURIComponent(ruleId)}`,
    { method: 'DELETE', ifMatch },
  );
}

// ----- Archived plays (forwarder side) -----

/**
 * Per-play summary row returned by /api/v2/plays. The full schema lives
 * in api/openapi/v2/forwarder.yaml § PlaySummary; gen-types only pulls
 * from proxy.yaml today, so the relevant fields are typed here. Extra
 * server-side fields surface via index access without breaking the
 * compile.
 */
export interface PlaySummary {
  play_id: string;
  player_id?: string;
  session_id?: string;
  group_id?: string;
  content_id?: string;
  attempt_id?: number;
  attempt_count?: number;
  started_at?: string;
  last_seen_at?: string;
  classification?: string;
  last_state?: string;
  last_player_error?: string;
  metric_events?: number;
  net_events?: number;
  net_errors?: number;
  net_faults?: number;
  stalls?: number;
  dropped_frames?: number;
  master_manifest_failures?: number;
  manifest_failures?: number;
  segment_failures?: number;
  all_failures?: number;
  transport_failures?: number;
  active_timeouts?: number;
  idle_timeouts?: number;
  bitrate_shifts?: number;
  downshifts?: number;
  upshifts?: number;
  resolution_changes?: number;
  avg_quality_pct?: number;
  min_quality_pct?: number;
  frames_displayed?: number;
  first_frame_s?: number;
  user_marked_count?: number;
  frozen_count?: number;
  segment_stall_count?: number;
  restart_count?: number;
  error_event_count?: number;
  label_histogram?: [string, number][];
  labels_total?: number;
  labels_distinct_count?: number;
  [k: string]: unknown;
}

export interface ListPlaysParams {
  from?: string;
  to?: string;
  player_id?: string;
  play_id?: string;
  classification?: string;
  limit?: number;
}

export async function listPlays(params: ListPlaysParams = {}): Promise<PlaySummary[]> {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue;
    qs.set(k, String(v));
  }
  const url = '/analytics/api/v2/plays' + (qs.toString() ? '?' + qs : '');
  const { data } = await request<{ items: PlaySummary[]; next_cursor: unknown }>(url);
  return data.items ?? [];
}

/**
 * Fetch one archived play's summary. Returns null on 404 (the play
 * hasn't archived any rows yet — common right after the live session
 * ends but before the next forwarder flush). Other HTTP errors throw.
 */
export async function getPlay(playId: string): Promise<PlaySummary | null> {
  try {
    const { data } = await request<PlaySummary>(
      `/analytics/api/v2/plays/${encodeURIComponent(playId)}`,
    );
    return data;
  } catch (err) {
    if (err instanceof RepoError && err.status === 404) return null;
    throw err;
  }
}

/**
 * Update an archived play's tiered-retention classification (#342).
 * `value` is one of: 'favourite' | 'interesting' | 'other' | 'auto'.
 * 'auto' re-runs the auto-classifier server-side; the response body
 * carries the settled value the caller should write back into cache.
 */
export async function patchPlayClassification(
  playId: string,
  value: 'favourite' | 'interesting' | 'other' | 'auto',
): Promise<PlaySummary> {
  const { data } = await request<PlaySummary>(
    `/analytics/api/v2/plays/${encodeURIComponent(playId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/merge-patch+json' },
      body: JSON.stringify({ classification: value }),
    },
  );
  return data;
}

// ----- Network log -----

export async function getPlayerNetworkLog(
  playerId: string,
  limit = 200,
): Promise<NetworkLogEntry[]> {
  if (!isLivePlayerId(playerId)) {
    return archiveNetworkStore.get(playerId) ?? [];
  }
  try {
    const { data } = await request<{ items: NetworkLogEntry[] }>(
      `/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}/network?limit=${limit}`,
    );
    return data.items ?? [];
  } catch (err) {
    // Treat 404 as "player no longer exists" rather than an error.
    // When a session is released, SSE-driven cache invalidations can
    // race a refetch for the just-deleted player_id, producing a
    // noisy console 404 that's already correct behaviour on the
    // server. Return empty so the UI just shows an empty log instead
    // of an error state.
    if ((err as any)?.status === 404) return [];
    throw err;
  }
}

// ----- Player groups -----

export async function listGroups(): Promise<PlayerGroup[]> {
  const { data } = await request<{ items: PlayerGroup[] }>('/api/v2/player-groups');
  return data.items ?? [];
}

export async function linkGroup(playerIds: string[]): Promise<PlayerGroup> {
  // The v2 spec names the field `member_player_ids` (matches
  // PlayerGroup.member_player_ids on read). The earlier `player_ids`
  // payload was a v1-ism — the handler rejected it with 400
  // "member_player_ids required" and the useMutation swallowed the
  // error, which is why bulk-link in Testing.vue silently no-op'd.
  const { data } = await request<PlayerGroup>('/api/v2/player-groups', {
    method: 'POST',
    body: JSON.stringify({ member_player_ids: playerIds }),
  });
  return data;
}

export async function disbandGroup(groupId: string, ifMatch?: string): Promise<void> {
  await request<void>(`/api/v2/player-groups/${encodeURIComponent(groupId)}`, {
    method: 'DELETE',
    ifMatch,
  });
}

/** PATCH a group's membership. The handler diffs against the current
 *  set and removes/adds via the v1 store accordingly. If-Match comes
 *  from the group's `control_revision`. */
export async function updateGroupMembers(
  groupId: string,
  memberPlayerIds: string[],
  ifMatch: string,
): Promise<PlayerGroup> {
  const { data } = await request<PlayerGroup>(
    `/api/v2/player-groups/${encodeURIComponent(groupId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/merge-patch+json' },
      body: JSON.stringify({ member_player_ids: memberPlayerIds }),
      ifMatch,
    },
  );
  return data;
}

// ----- Diagnostics -----

export async function getInfo(): Promise<components['schemas']['Info']> {
  const { data } = await request<components['schemas']['Info']>('/api/v2/info');
  return data;
}
