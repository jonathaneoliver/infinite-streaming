/**
 * Shared types for the AI chat panel (#497 Phase 3).
 * Mirror the backend's wire shape — see
 * analytics/go-forwarder/llm_session_chat.go.
 */

/** Catalog entries surfaced by GET /api/v2/chat/profiles. */
export interface ChatProfile {
  name: string;
  label: string;
  base_url: string;
  requires_api_key: boolean;
  api_key_help: string;
  supports_tools: boolean;
  models: ChatModel[];
}

export interface ChatModel {
  id: string;
  label: string;
  pricing: { input_per_mtok: number; output_per_mtok: number };
}

export interface ChatCatalog {
  templates: ChatProfile[];
}

/** Per-user settings persisted to browser localStorage. */
export interface ChatSettings {
  profile: string;     // template name
  model: string;       // model id within the template
  apiKey: string;      // BYO key; never leaves the browser except per-request
  // Override the profile's catalog base_url. Empty = use the catalog
  // default. Useful for pointing at a remote Ollama / self-hosted
  // OAI-compat endpoint. NB: the forwarder makes the upstream call,
  // so the URL must be reachable from the forwarder's network — not
  // just from the browser.
  baseUrlOverride: string;
}

/** One message in the chat history. Mirrors LLMMessage on the wire. */
export interface ChatMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content?: string;
  name?: string;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
}

export interface ToolCall {
  id: string;
  type: 'function';
  function: { name: string; arguments: string };
}

/** Scope hint sent to the backend. */
export interface ChatScope {
  kind?: '' | 'fleet' | 'play' | 'range' | 'characterization';
  // player_id is helpful context on play / range scopes — the bot
  // can build deep-link citations without first calling
  // get_play_summary to look it up.
  player_id?: string;
  play_id?: string;
  from?: string;
  to?: string;
  run_id?: string;
  cycle?: number;
}

/** Citation kinds — the dashboard renders one CitationCard per kind. */
export type CitationKind = 'play' | 'range' | 'finding' | 'standard' | 'skill' | 'run';

export interface Citation {
  span_id: string;
  kind: CitationKind;
  label: string;
  // player_id is REQUIRED on play / range kinds — session-viewer
  // won't load without it. cite() enforces this server-side.
  player_id?: string;
  play_id?: string;
  at?: string;
  from?: string;
  to?: string;
  slug?: string;
  name?: string;
  run_id?: string;
  cycle?: number;
}

/** SSE event shapes emitted by /api/v2/chat. */
export type ChatEvent =
  | { type: 'meta'; chat_id: string; request_id: string }
  | { type: 'text_delta'; delta: string }
  | { type: 'tool_call'; id: string; name: string; args: string }
  | { type: 'tool_result'; id: string; ok: boolean; summary: string }
  | { type: 'citation'; citation: Citation }
  | { type: 'usage'; input_tokens: number; output_tokens: number; cost_usd: number; duration_ms: number; tool_calls_count: number }
  | { type: 'done' }
  | { type: 'error'; kind: string; message: string };

/** One assistant turn as the UI renders it — text + citations + tool calls. */
export interface AssistantTurn {
  text: string;
  citations: Citation[];
  toolCalls: { id: string; name: string; args: string; result?: { ok: boolean; summary: string } }[];
  usage?: { input_tokens: number; output_tokens: number; cost_usd: number; duration_ms: number; tool_calls_count: number };
  error?: { kind: string; message: string };
  done: boolean;
}

/** Budget surface returned by GET /api/v2/chat/budget. */
export interface BudgetStatus {
  spent_usd: number;
  cap_usd: number;
  calls_today: number;
  resets_at: string;
}
