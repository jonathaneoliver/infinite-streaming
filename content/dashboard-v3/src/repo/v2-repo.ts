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

export async function getPlayer(playerId: string): Promise<{ player: PlayerRecord; etag?: string }> {
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

// ----- Network log -----

export async function getPlayerNetworkLog(
  playerId: string,
  limit = 200,
): Promise<NetworkLogEntry[]> {
  const { data } = await request<{ items: NetworkLogEntry[] }>(
    `/api/v2/players/${encodeURIComponent(canonicalIdFor(playerId))}/network?limit=${limit}`,
  );
  return data.items ?? [];
}

// ----- Player groups -----

export async function listGroups(): Promise<PlayerGroup[]> {
  const { data } = await request<{ items: PlayerGroup[] }>('/api/v2/player-groups');
  return data.items ?? [];
}

export async function linkGroup(playerIds: string[]): Promise<PlayerGroup> {
  const { data } = await request<PlayerGroup>('/api/v2/player-groups', {
    method: 'POST',
    body: JSON.stringify({ player_ids: playerIds }),
  });
  return data;
}

export async function disbandGroup(groupId: string, ifMatch?: string): Promise<void> {
  await request<void>(`/api/v2/player-groups/${encodeURIComponent(groupId)}`, {
    method: 'DELETE',
    ifMatch,
  });
}

// ----- Diagnostics -----

export async function getInfo(): Promise<components['schemas']['Info']> {
  const { data } = await request<components['schemas']['Info']>('/api/v2/info');
  return data;
}
