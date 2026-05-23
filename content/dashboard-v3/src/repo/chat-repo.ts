/**
 * chat-repo.ts — typed wrappers around the forwarder's chat endpoints
 * (#497). Mirrors v2-repo.ts: HTTP shape only, no business logic.
 *
 * The streaming POST /api/v2/chat is a thin wrapper that yields parsed
 * ChatEvents over an async iterator; the consumer (useChat) handles
 * state updates + history accumulation.
 */

import type { BudgetStatus, ChatCatalog, ChatEvent, ChatMessage, ChatScope, Citation } from '@/types/chat';

const CHAT_BASE = '/analytics/api/v2/chat';

export async function fetchProfiles(): Promise<ChatCatalog> {
  const resp = await fetch(`${CHAT_BASE}/profiles`, { headers: { Accept: 'application/json' } });
  if (!resp.ok) throw new Error(`profiles: HTTP ${resp.status}`);
  return resp.json();
}

export async function fetchBudget(): Promise<BudgetStatus> {
  const resp = await fetch(`${CHAT_BASE}/budget`, { headers: { Accept: 'application/json' } });
  if (!resp.ok) throw new Error(`budget: HTTP ${resp.status}`);
  return resp.json();
}

export interface ChatRequestBody {
  chat_id?: string;
  profile: string;
  model: string;
  api_key: string;
  base_url?: string;
  messages: ChatMessage[];
  scope?: ChatScope;
  one_shot?: boolean;
  temperature?: number;
}

/**
 * streamChat opens an SSE-style POST and yields parsed events. The
 * forwarder uses `event: <type>\ndata: <json>\n\n` framing. Caller
 * passes an AbortSignal to cancel mid-stream.
 */
export async function* streamChat(body: ChatRequestBody, signal: AbortSignal): AsyncGenerator<ChatEvent> {
  const resp = await fetch(CHAT_BASE, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Accept': 'text/event-stream',
    },
    body: JSON.stringify(body),
    signal,
  });
  if (!resp.ok || !resp.body) {
    const text = await resp.text().catch(() => '');
    throw new Error(`chat: HTTP ${resp.status} ${text}`);
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    // Split on blank-line frame boundaries.
    let idx;
    while ((idx = buf.indexOf('\n\n')) !== -1) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const evt = parseSSEFrame(frame);
      if (evt) yield evt;
    }
  }
}

/**
 * Parse an SSE frame of the shape:
 *   event: <type>
 *   data: <json>
 * Returns the decoded ChatEvent or null when the frame is malformed
 * (skipped silently rather than killing the stream).
 */
function parseSSEFrame(frame: string): ChatEvent | null {
  let eventType = '';
  let dataLine = '';
  for (const raw of frame.split('\n')) {
    if (raw.startsWith('event:')) {
      eventType = raw.slice(6).trim();
    } else if (raw.startsWith('data:')) {
      dataLine = raw.slice(5).trim();
    }
  }
  if (!eventType || !dataLine) return null;
  let data: any;
  try {
    data = JSON.parse(dataLine);
  } catch {
    return null;
  }
  switch (eventType) {
    case 'meta':        return { type: 'meta',        chat_id: data.chat_id, request_id: data.request_id };
    case 'text_delta':  return { type: 'text_delta',  delta: data.delta ?? '' };
    case 'tool_call':   return { type: 'tool_call',   id: data.id, name: data.name, args: data.args };
    case 'tool_result': return { type: 'tool_result', id: data.id, ok: !!data.ok, summary: data.summary ?? '' };
    case 'citation':    return { type: 'citation',    citation: data as Citation };
    case 'usage':       return { type: 'usage',       input_tokens: data.input_tokens, output_tokens: data.output_tokens, cost_usd: data.cost_usd, duration_ms: data.duration_ms, tool_calls_count: data.tool_calls_count };
    case 'done':        return { type: 'done' };
    case 'error':       return { type: 'error',       kind: data.kind ?? '', message: data.message ?? '' };
    default:            return null;
  }
}
