/**
 * useChat — the main chat-state composable for the AI chat panel
 * (#497). Holds the per-conversation history, the streaming-in-flight
 * assistant turn, and the SSE invocation.
 *
 * Each ChatPanel mounts its own useChat instance. Settings (profile /
 * model / api_key) come from useChatSettings (a singleton); per-page
 * scope comes via the panel's props.
 *
 * History persists to localStorage keyed by `scopeKey` so the panel
 * survives reloads. Cap at 50 user/assistant pairs to keep payload
 * bounded.
 */

import { computed, reactive, ref, watch } from 'vue';
import { useQueryClient } from '@tanstack/vue-query';
import { streamChat } from '@/repo/chat-repo';
import { useChatSettings } from './useChatSettings';
import type { AssistantTurn, ChatMessage, ChatScope, Citation } from '@/types/chat';

const MAX_HISTORY_PAIRS = 50;
const STORAGE_PREFIX = 'isLLMChatHistory:';

interface ChatState {
  chatId: string | null;
  history: ChatMessage[];
  inflight: AssistantTurn | null;
  error: string;
}

function freshState(): ChatState {
  return { chatId: null, history: [], inflight: null, error: '' };
}

function storageKey(scopeKey: string): string {
  return STORAGE_PREFIX + (scopeKey || 'default');
}

function loadHistory(scopeKey: string): { chatId: string | null; history: ChatMessage[] } {
  if (typeof localStorage === 'undefined') return { chatId: null, history: [] };
  try {
    const raw = localStorage.getItem(storageKey(scopeKey));
    if (!raw) return { chatId: null, history: [] };
    const parsed = JSON.parse(raw);
    return {
      chatId: typeof parsed.chatId === 'string' ? parsed.chatId : null,
      history: Array.isArray(parsed.history) ? parsed.history : [],
    };
  } catch {
    return { chatId: null, history: [] };
  }
}

function saveHistory(scopeKey: string, chatId: string | null, history: ChatMessage[]) {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(storageKey(scopeKey), JSON.stringify({ chatId, history }));
  } catch {
    // localStorage may reject if quota is full — silent skip.
  }
}

export interface UseChatOptions {
  /** Stable key used to persist this chat's history. e.g. "play:abc-123". */
  scopeKey: string;
  /** Scope hint sent to the backend's system prompt + ledger. */
  scope: () => ChatScope;
}

export function useChat(opts: UseChatOptions) {
  const { settings, isConfigured } = useChatSettings();
  const queryClient = useQueryClient();

  const state = reactive<ChatState>(freshState());

  // Hydrate from localStorage on first mount.
  {
    const loaded = loadHistory(opts.scopeKey);
    state.chatId = loaded.chatId;
    state.history = loaded.history;
  }

  // Persist on every mutation.
  watch(() => [state.chatId, state.history.length] as const, () => {
    saveHistory(opts.scopeKey, state.chatId, state.history);
  });

  let abortCtrl: AbortController | null = null;
  const isStreaming = computed(() => state.inflight !== null);

  /**
   * Send a user message and stream the assistant's response. Builds
   * an inflight AssistantTurn that accumulates text + citations +
   * tool calls until the `done` event arrives.
   */
  async function send(userMessage: string): Promise<void> {
    if (!isConfigured.value) {
      state.error = 'Configure profile + model + api_key in chat settings first.';
      return;
    }
    if (state.inflight) {
      // Don't allow overlapping turns — the loop is serial per chat.
      return;
    }
    state.error = '';

    // Cap history before adding the new turn.
    if (state.history.length > MAX_HISTORY_PAIRS * 2) {
      state.history = state.history.slice(-MAX_HISTORY_PAIRS * 2);
    }

    const userMsg: ChatMessage = { role: 'user', content: userMessage };
    state.history.push(userMsg);

    state.inflight = {
      text: '',
      citations: [],
      toolCalls: [],
      done: false,
    };

    abortCtrl = new AbortController();
    try {
      const iter = streamChat({
        chat_id: state.chatId || undefined,
        profile: settings.value.profile,
        model: settings.value.model,
        api_key: settings.value.apiKey,
        // Empty override = forwarder uses the catalog's base_url.
        // Non-empty = forwarder calls THIS url (e.g. a remote
        // Ollama). NB: the URL must be reachable from the
        // forwarder's network, not just the browser's.
        base_url: settings.value.baseUrlOverride || undefined,
        messages: state.history,
        scope: opts.scope(),
      }, abortCtrl.signal);

      for await (const ev of iter) {
        if (!state.inflight) break; // cancelled mid-stream
        switch (ev.type) {
          case 'meta':
            state.chatId = ev.chat_id;
            break;
          case 'text_delta':
            state.inflight.text += ev.delta;
            break;
          case 'tool_call':
            state.inflight.toolCalls.push({ id: ev.id, name: ev.name, args: ev.args });
            break;
          case 'tool_result': {
            const tc = state.inflight.toolCalls.find(t => t.id === ev.id);
            if (tc) tc.result = { ok: ev.ok, summary: ev.summary };
            break;
          }
          case 'citation':
            state.inflight.citations.push(ev.citation);
            break;
          case 'finding_proposed':
            if (!state.inflight.findings) state.inflight.findings = [];
            state.inflight.findings.push(ev.proposal);
            break;
          case 'usage':
            state.inflight.usage = {
              input_tokens: ev.input_tokens,
              output_tokens: ev.output_tokens,
              cost_usd: ev.cost_usd,
              duration_ms: ev.duration_ms,
              tool_calls_count: ev.tool_calls_count,
            };
            break;
          case 'done':
            state.inflight.done = true;
            break;
          case 'error':
            state.inflight.error = { kind: ev.kind, message: ev.message };
            break;
        }
      }
    } catch (err: any) {
      if (err?.name !== 'AbortError') {
        state.error = err?.message ?? String(err);
      }
    } finally {
      // Commit the inflight turn into history so the next send sees it.
      if (state.inflight) {
        const inflight = state.inflight;
        const hasContent = (inflight.text ?? '').trim().length > 0;
        const hasToolCalls = inflight.toolCalls.length > 0;
        if (hasContent || hasToolCalls) {
          // Real assistant turn — append to wire history.
          const finalAssistant: ChatMessage = {
            role: 'assistant',
            content: inflight.text,
          };
          state.history.push(finalAssistant);
        } else {
          // Empty assistant turn (error / cancel mid-stream). Don't
          // poison history — Anthropic rejects assistant turns with
          // no content and no tool_calls. Also unstick the user
          // message we appended for this attempt so the next send
          // doesn't see a dangling user message followed by another
          // user message.
          if (state.history.length > 0 &&
              state.history[state.history.length - 1].role === 'user') {
            state.history.pop();
          }
        }
        // Always show the turn in the UI (text or error or just a
        // tool-call attempt) so the operator sees what happened.
        committedTurns.value.push({
          userText: userMsg.content ?? '',
          assistant: inflight,
        });
        state.inflight = null;
      }
      abortCtrl = null;
      // Refetch budget so the meter updates.
      queryClient.invalidateQueries({ queryKey: ['llm', 'budget'] });
    }
  }

  /** Cancel the in-flight turn. Useful when a streaming answer is going off. */
  function cancel() {
    if (abortCtrl) {
      abortCtrl.abort();
      abortCtrl = null;
    }
  }

  /** Clear this chat's history. Doesn't touch other scopes. */
  function reset() {
    cancel();
    state.chatId = null;
    state.history = [];
    state.inflight = null;
    state.error = '';
    committedTurns.value = [];
    saveHistory(opts.scopeKey, null, []);
  }

  /**
   * Compact the conversation: ask the LLM to summarise the current
   * history, then replace it with that summary. Keeps the thread
   * conceptually intact (the bot still knows what was discussed)
   * but collapses ~50K tokens of tool-result spam into ~1K of prose.
   *
   * The committedTurns UI is preserved so the operator can scroll
   * back through the original conversation; only the WIRE history
   * (what gets sent on the next chat call) is replaced.
   */
  async function compact() {
    if (state.history.length < 4 || state.inflight) return;
    if (!isConfigured.value) {
      state.error = 'Configure profile + model + api_key before compacting.';
      return;
    }
    const compactPrompt: ChatMessage = {
      role: 'user',
      content:
        'CONTEXT COMPACTION REQUEST: produce a ~300-word summary of this conversation ' +
        'suitable for replacing the message history. Lead with the original question. ' +
        'Then list: (1) tagged conclusions reached (with play_ids / timestamps), ' +
        '(2) tool results that mattered (label histograms, top plays found, etc.), ' +
        '(3) open questions still being investigated. Output as plain prose markdown ' +
        'with no preamble. The next turn will use your summary as its only context.',
    };
    abortCtrl = new AbortController();
    let summary = '';
    try {
      const iter = streamChat({
        chat_id: state.chatId || undefined,
        profile: settings.value.profile,
        model: settings.value.model,
        api_key: settings.value.apiKey,
        base_url: settings.value.baseUrlOverride || undefined,
        messages: [...state.history, compactPrompt],
      }, abortCtrl.signal);
      for await (const ev of iter) {
        if (ev.type === 'text_delta') summary += ev.delta;
        if (ev.type === 'done') break;
        if (ev.type === 'error') {
          state.error = `compact failed: ${ev.message}`;
          return;
        }
      }
    } catch (err: any) {
      if (err?.name !== 'AbortError') state.error = err?.message ?? String(err);
      return;
    } finally {
      abortCtrl = null;
    }
    if (!summary.trim()) {
      state.error = 'compact produced empty summary; not replacing history';
      return;
    }
    // Replace wire history with one summary turn the next chat call
    // will see as "what was said before".
    state.history = [
      {
        role: 'user',
        content:
          'EARLIER CONVERSATION SUMMARY (compacted ' + new Date().toISOString() + '):\n\n' +
          summary,
      },
      {
        role: 'assistant',
        content: 'Acknowledged. Continuing from the summary above.',
      },
    ];
    // Push a marker turn into the UI rail so the operator sees the
    // compaction happened.
    committedTurns.value.push({
      userText: '(compacted conversation history)',
      assistant: {
        text: summary,
        citations: [],
        toolCalls: [],
        done: true,
      },
    });
  }

  // A render-friendly view of the committed turns (one entry per user
  // message; the assistant turn carries text + citations + tool calls).
  const committedTurns = ref<{ userText: string; assistant: AssistantTurn }[]>([]);

  return {
    state,
    committedTurns,
    isStreaming,
    isConfigured,
    send,
    cancel,
    reset,
    compact,
  };
}
