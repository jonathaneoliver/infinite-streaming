<script setup lang="ts">
/**
 * ChatPanel — the main AI chat surface for #497 Phase 3.
 *
 * Composes the chat-state composable (useChat) with the per-page
 * scope (provided via props), the profile/key settings panel
 * (ChatSettings), and citation rendering (CitationCard).
 *
 * Layout: top header + scrolling message stream + bottom input bar
 * with a small budget meter. Sized to fit comfortably in either a
 * sidebar slot (~360px wide) or a full-page column (`fluid` prop).
 */
import { computed, nextTick, ref, watch } from 'vue';
import { useChat } from '@/composables/useChat';
import { useChatSettings } from '@/composables/useChatSettings';
import { useLLMBudget } from '@/composables/useLLMBudget';
import { useLLMProfiles } from '@/composables/useLLMProfiles';
import { saveFinding } from '@/repo/chat-repo';
import type { ChatScope, FindingProposal } from '@/types/chat';
import CitationCard from './CitationCard.vue';
import ChatSettings from './ChatSettings.vue';

const props = withDefaults(defineProps<{
  /** Per-page scope handed to the backend's system prompt + ledger. */
  scope: ChatScope;
  /** Stable key for localStorage history. Default falls back to scope-derived. */
  scopeKey?: string;
  /** Visual variant. 'panel' = sidebar/side-panel (compact). 'fluid' = full column (Ask page). */
  variant?: 'panel' | 'fluid';
  /** Initial collapsed state for the panel variant. */
  startCollapsed?: boolean;
}>(), {
  scopeKey: '',
  variant: 'panel',
  startCollapsed: false,
});

const scopeKey = computed(() =>
  props.scopeKey || `${props.scope.kind ?? 'fleet'}:${props.scope.play_id ?? ''}:${props.scope.run_id ?? ''}`
);

const { settings, isConfigured } = useChatSettings();
const { state, committedTurns, isStreaming, send, cancel, reset, compact } = useChat({
  scopeKey: scopeKey.value,
  scope: () => props.scope,
});
const { data: budget } = useLLMBudget();
const { data: catalog } = useLLMProfiles();

// "Who am I talking to" chip in the header. Shows the active
// provider's short label + model id so a misconfigured panel
// (e.g. accidentally on HF when you meant local MLX) is obvious
// before the first send. Clicks open the settings modal so the
// chip doubles as an affordance for "fix this".
const providerChip = computed(() => {
  if (!isConfigured.value) {
    return { label: 'not configured', short: '⚠', tip: 'Click to configure profile / model / API key', warn: true };
  }
  const t = catalog.value?.templates.find(x => x.name === settings.value.profile);
  // Catalog may not be loaded yet — fall back to raw names.
  const provLabel = t?.label ?? settings.value.profile;
  const modelLabel = t?.models.find(m => m.id === settings.value.model)?.label ?? settings.value.model;
  // Short forms for the compact panel-header pill.
  // anthropic-claude → "anthropic"; mlx-local → "mlx"; etc.
  const provShort = (settings.value.profile || '')
    .replace(/-claude.*$/, '')
    .replace(/^chatgpt-via-/, '')
    .replace(/^local-?/, '')
    .replace(/^huggingface$/, 'hf');
  // Model short form: drop org prefix and well-known suffixes so
  // "mlx-community/Qwen2.5-Coder-7B-Instruct-4bit" → "Qwen2.5-Coder-7B-4bit"
  const modelShort = (settings.value.model || '')
    .replace(/^[^/]+\//, '')
    .replace(/-Instruct(?=-|$)/, '')
    .replace(/-(\d+bit)$/, '-$1');
  return {
    label: `${provLabel} · ${modelLabel}`,
    short: `${provShort} · ${modelShort}`,
    tip: `Provider: ${provLabel}\nModel: ${modelLabel}\nClick to change`,
    warn: false,
  };
});

const showSettings = ref(false);
const collapsed = ref(props.variant === 'panel' ? props.startCollapsed : false);

const draft = ref('');
const scroller = ref<HTMLElement | null>(null);

async function onSend() {
  const text = draft.value.trim();
  if (!text) return;
  draft.value = '';
  await send(text);
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    onSend();
  }
}

// Auto-scroll on new content.
watch(() => committedTurns.value.length + (state.inflight?.text.length ?? 0), async () => {
  await nextTick();
  if (scroller.value) {
    scroller.value.scrollTop = scroller.value.scrollHeight;
  }
});

const budgetText = computed(() => {
  if (!budget.value) return '';
  const spent = budget.value.spent_usd.toFixed(2);
  const cap = budget.value.cap_usd.toFixed(2);
  return `$${spent} / $${cap} today`;
});
const overBudget = computed(() =>
  budget.value ? budget.value.spent_usd >= budget.value.cap_usd && budget.value.cap_usd > 0 : false
);

// Running per-conversation token totals — derived from
// committedTurns' usage events. Re-computed on every turn append.
// Note: this is *cumulative session*, not "tokens about to be sent
// next turn" — but since every turn sends the FULL history, the
// last turn's input_tokens IS roughly "current context size" (plus
// any tools the LLM is about to receive in the next call).
const tokenTotals = computed(() => {
  let inSum = 0, outSum = 0, lastIn = 0;
  for (const t of committedTurns.value) {
    if (t.assistant.usage) {
      inSum += t.assistant.usage.input_tokens || 0;
      outSum += t.assistant.usage.output_tokens || 0;
      lastIn = t.assistant.usage.input_tokens || lastIn;
    }
  }
  return { inSum, outSum, lastIn };
});

const contextWindow = computed<number | null>(() => {
  const t = catalog.value?.templates.find(x => x.name === settings.value.profile);
  const m = t?.models.find(mm => mm.id === settings.value.model);
  return m?.context_window ?? null;
});

// Compact "10.2K" style for the footer. Tokens grow fast; raw
// digits would push the footer wide.
function compactTokens(n: number): string {
  if (n < 1000) return `${n}`;
  if (n < 10_000) return `${(n / 1000).toFixed(1)}K`;
  if (n < 1_000_000) return `${Math.round(n / 1000)}K`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

const tokenText = computed(() => {
  const { inSum, outSum, lastIn } = tokenTotals.value;
  const cw = contextWindow.value;
  // No turns yet, nothing to show.
  if (inSum === 0 && outSum === 0) return '';
  const parts = [
    `${compactTokens(inSum)}↑`,
    `${compactTokens(outSum)}↓`,
  ];
  if (cw && lastIn > 0) {
    const pct = Math.round((lastIn / cw) * 100);
    parts.push(`${compactTokens(lastIn)} / ${compactTokens(cw)} ctx (${pct}%)`);
  } else if (lastIn > 0) {
    parts.push(`${compactTokens(lastIn)} ctx`);
  }
  return parts.join(' · ');
});

// Visual warning when context is >80% of the window — usually
// time to ⟲ clear or trigger a manual compact via a fresh chat.
const contextWarn = computed(() => {
  const cw = contextWindow.value;
  const lastIn = tokenTotals.value.lastIn;
  return cw && lastIn > 0 && (lastIn / cw) > 0.8;
});

const tokenMeterTip = computed(() =>
  contextWarn.value
    ? 'Context >80% of the model window — Clear (⟲) or Compact (📦) to free space'
    : 'Cumulative tokens; last input size shown as ctx'
);

// Pre-send projection: lastIn (full history we'd resend) + the
// new user message we're about to add. Rough token estimate uses
// the 4-bytes-per-token rule (English-heavy chat with tool JSON;
// pessimistic for prose, optimistic for code). Better than nothing.
const projectedNextInput = computed(() => {
  const lastIn = tokenTotals.value.lastIn;
  const draftTokens = Math.ceil(draft.value.length / 4);
  return lastIn + draftTokens;
});

// Block the Send button when the next request would land at >95%
// of the model window. Tighter than the 80% warning threshold —
// 80-95% the user gets the amber meter and can keep going; past
// 95% we're trying to prevent a mid-stream "context length
// exceeded" error rather than just nudging.
const SEND_BLOCK_RATIO = 0.95;
const overBudget_context = computed(() => {
  const cw = contextWindow.value;
  if (!cw) return false;
  return projectedNextInput.value / cw > SEND_BLOCK_RATIO;
});
const sendBlockedTip = computed(() => {
  if (!overBudget_context.value) return '';
  const cw = contextWindow.value!;
  const pct = Math.round((projectedNextInput.value / cw) * 100);
  return `This send would land at ~${pct}% of the model's context window. ` +
    `Compact (📦) the conversation or Clear (⟲) it before sending again.`;
});

// Save / Discard handlers for propose_finding cards.
async function onSaveFinding(p: FindingProposal) {
  const res = await saveFinding(p.slug, p.markdown);
  p.saved = res.ok;
  p.savedPath = res.path;
  p.error = res.error;
}
function onDiscardFinding(p: FindingProposal) {
  // Mark as saved-locally so the card collapses; nothing on the server
  // to roll back (we only wrote on Save).
  p.saved = true;
  p.savedPath = undefined;
  p.error = 'discarded';
}
</script>

<template>
  <aside
    class="chat-panel"
    :class="{ collapsed, fluid: variant === 'fluid' }"
  >
    <header class="chat-head">
      <button
        v-if="variant === 'panel'"
        class="collapse-btn"
        @click="collapsed = !collapsed"
        :title="collapsed ? 'Expand chat' : 'Collapse chat'"
      >{{ collapsed ? '◀' : '▶' }}</button>
      <span class="title">AI</span>
      <span class="scope-pill" v-if="scope.kind && !collapsed">
        {{ scope.kind }}{{ scope.play_id ? `:${scope.play_id.slice(0, 8)}…` : '' }}
      </span>
      <button
        v-if="!collapsed"
        class="provider-chip"
        :class="{ warn: providerChip.warn }"
        :title="providerChip.tip"
        @click="showSettings = true"
      >{{ providerChip.short }}</button>
      <span class="spacer" />
      <template v-if="!collapsed">
        <button
          class="head-btn"
          @click="compact"
          :disabled="isStreaming || committedTurns.length < 2"
          title="Compact conversation — replaces history with an LLM-generated summary, keeps the UI scroll-back"
        >📦</button>
        <button class="head-btn" @click="reset" title="Clear conversation">⟲</button>
        <button class="head-btn" @click="showSettings = true" title="Chat settings">⚙</button>
      </template>
    </header>

    <template v-if="!collapsed">
      <div v-if="!isConfigured" class="empty">
        <p>Configure a profile + model + API key to start.</p>
        <button class="primary" @click="showSettings = true">Open settings</button>
      </div>

      <div v-else class="stream" ref="scroller">
        <div v-if="committedTurns.length === 0 && !state.inflight" class="hint">
          <p>
            Ask anything about
            <strong v-if="scope.kind === 'play'">this play</strong>
            <strong v-else-if="scope.kind === 'range'">this time window</strong>
            <strong v-else-if="scope.kind === 'characterization'">this run</strong>
            <strong v-else>the fleet</strong>.
          </p>
          <p class="muted">
            The bot can call tools (find_plays, get_control_events, query, …) and emit
            clickable citations. Anything non-trivial gets cited.
          </p>
        </div>

        <div v-for="(turn, idx) in committedTurns" :key="idx" class="turn">
          <div class="bubble user">{{ turn.userText }}</div>
          <div class="bubble assistant">
            <div class="assistant-text">{{ turn.assistant.text || '(no text)' }}</div>
            <div v-if="turn.assistant.citations.length" class="cite-rail">
              <CitationCard
                v-for="c in turn.assistant.citations"
                :key="c.span_id"
                :citation="c"
              />
            </div>
            <div v-if="turn.assistant.findings && turn.assistant.findings.length" class="findings-rail">
              <div v-for="p in turn.assistant.findings" :key="p.slug" class="finding-card" :class="{ saved: p.saved }">
                <div class="finding-head">
                  <span class="finding-icon">📓</span>
                  <code class="finding-slug">{{ p.slug }}</code>
                  <span v-if="p.saved && p.savedPath" class="finding-status ok" :title="p.savedPath">✓ saved</span>
                  <span v-else-if="p.saved && p.error === 'discarded'" class="finding-status muted">discarded</span>
                  <span v-else-if="p.error" class="finding-status err">{{ p.error }}</span>
                </div>
                <details class="finding-body">
                  <summary>preview ({{ p.markdown.length }} bytes)</summary>
                  <pre>{{ p.markdown }}</pre>
                </details>
                <div v-if="!p.saved" class="finding-actions">
                  <button class="primary" @click="onSaveFinding(p)">Save</button>
                  <button class="secondary" @click="onDiscardFinding(p)">Discard</button>
                </div>
              </div>
            </div>
            <details v-if="turn.assistant.toolCalls.length" class="tools">
              <summary>{{ turn.assistant.toolCalls.length }} tool call{{ turn.assistant.toolCalls.length === 1 ? '' : 's' }}</summary>
              <ul>
                <li v-for="tc in turn.assistant.toolCalls" :key="tc.id">
                  <code>{{ tc.name }}</code>
                  <span v-if="tc.result" :class="{ ok: tc.result.ok, fail: !tc.result.ok }">
                    {{ tc.result.ok ? '✓' : '✗' }} {{ tc.result.summary }}
                  </span>
                </li>
              </ul>
            </details>
            <div v-if="turn.assistant.usage" class="usage">
              {{ turn.assistant.usage.input_tokens }}↑ / {{ turn.assistant.usage.output_tokens }}↓
              · ${{ turn.assistant.usage.cost_usd.toFixed(4) }}
              · {{ (turn.assistant.usage.duration_ms / 1000).toFixed(1) }}s
            </div>
            <div v-if="turn.assistant.error" class="err">
              {{ turn.assistant.error.kind }}: {{ turn.assistant.error.message }}
            </div>
          </div>
        </div>

        <div v-if="state.inflight" class="turn streaming">
          <div class="bubble assistant">
            <div class="assistant-text">{{ state.inflight.text }}<span class="cursor">▋</span></div>
            <div v-if="state.inflight.citations.length" class="cite-rail">
              <CitationCard
                v-for="c in state.inflight.citations"
                :key="c.span_id"
                :citation="c"
              />
            </div>
            <div v-if="state.inflight.toolCalls.length" class="tools-inflight">
              running: {{ state.inflight.toolCalls.map(t => t.name).join(', ') }}
            </div>
          </div>
        </div>

        <div v-if="state.error" class="err global-err">{{ state.error }}</div>
      </div>

      <footer class="chat-foot">
        <textarea
          v-model="draft"
          :placeholder="isConfigured ? 'Ask…' : 'Configure first'"
          :disabled="!isConfigured || isStreaming"
          rows="2"
          @keydown="onKey"
        />
        <div class="foot-actions">
          <span class="budget" :class="{ over: overBudget }">{{ budgetText }}</span>
          <button v-if="isStreaming" class="cancel" @click="cancel">Stop</button>
          <button
            v-else
            class="send"
            @click="onSend"
            :disabled="!isConfigured || !draft.trim() || overBudget_context"
            :title="overBudget_context ? sendBlockedTip : ''"
          >Send</button>
        </div>
        <div v-if="overBudget_context" class="ctx-block-banner">
          {{ sendBlockedTip }}
        </div>
        <div v-if="tokenText" class="token-meter" :class="{ warn: contextWarn }" :title="tokenMeterTip">
          {{ tokenText }}
        </div>
      </footer>
    </template>

    <Teleport to="body">
      <ChatSettings v-if="showSettings" @close="showSettings = false" />
    </Teleport>
  </aside>
</template>

<style scoped>
.chat-panel {
  display: flex;
  flex-direction: column;
  width: 380px;
  max-width: 100%;
  height: 100%;
  background: #fff;
  border-left: 1px solid var(--border);
  font-family: 'Google Sans', system-ui, sans-serif;
  overflow: hidden;
}
.chat-panel.collapsed {
  width: 36px;
  align-items: stretch;
}
.chat-panel.fluid {
  width: 100%;
  border-left: none;
  border-radius: var(--radius-lg);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-sm);
}

.chat-head {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border-bottom: 1px solid var(--border-light);
  background: var(--surface);
  font-size: 13px;
}
.title { font-weight: 600; color: var(--text-primary); }
.scope-pill {
  font-size: 11px;
  color: var(--text-secondary);
  background: var(--surface-hover);
  padding: 1px 6px;
  border-radius: var(--radius-full);
}
/* Provider/model chip — clickable; opens settings. The whole
   point is that "which LLM am I talking to" is obvious before any
   send. Slight visual weight (border + monospace) sets it apart
   from the lighter scope pill. */
.provider-chip {
  font: 600 10px ui-monospace, SFMono-Regular, monospace;
  color: #0f766e;
  background: #ecfdf5;
  border: 1px solid #6ee7b7;
  padding: 1px 6px;
  border-radius: var(--radius-full);
  cursor: pointer;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.provider-chip:hover { background: #d1fae5; }
.provider-chip.warn {
  color: #7c2d12;
  background: #fef3c7;
  border-color: #fcd34d;
}
.spacer { flex: 1; }
.collapse-btn, .head-btn {
  background: none;
  border: none;
  font-size: 13px;
  cursor: pointer;
  color: var(--text-secondary);
  padding: 2px 6px;
  border-radius: var(--radius-sm);
}
.collapse-btn:hover, .head-btn:hover { background: var(--surface-hover); }

.empty {
  flex: 1;
  display: grid;
  place-items: center;
  text-align: center;
  padding: 24px;
  color: var(--text-secondary);
}
.empty p { margin-bottom: 12px; }

.stream {
  flex: 1;
  overflow-y: auto;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.hint {
  text-align: center;
  padding: 16px;
  color: var(--text-secondary);
  font-size: 13px;
}
.hint p { margin-bottom: 6px; }
.hint .muted { font-size: 12px; opacity: 0.8; }

.turn {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.bubble {
  padding: 8px 12px;
  border-radius: var(--radius-md);
  font-size: 13px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-wrap: break-word;
}
.bubble.user {
  background: var(--primary-blue-light);
  color: var(--text-primary);
  align-self: flex-end;
  max-width: 85%;
}
.bubble.assistant {
  background: var(--surface);
  color: var(--text-primary);
  align-self: flex-start;
  max-width: 95%;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.assistant-text { white-space: pre-wrap; }
.cursor { animation: blink 1s steps(2, start) infinite; }
@keyframes blink { to { visibility: hidden; } }

.cite-rail {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.tools {
  font-size: 11px;
  color: var(--text-secondary);
}
.tools summary {
  cursor: pointer;
  padding: 2px 0;
  user-select: none;
}
.tools ul { list-style: none; padding-left: 12px; margin-top: 4px; }
.tools li { padding: 2px 0; }
.tools code {
  background: var(--surface-hover);
  padding: 1px 4px;
  border-radius: var(--radius-sm);
  font-family: monospace;
}
.tools .ok { color: var(--success); margin-left: 4px; }
.tools .fail { color: var(--error); margin-left: 4px; }
.tools-inflight {
  font-size: 11px;
  color: var(--text-secondary);
  font-style: italic;
}

.usage {
  font-size: 10px;
  color: var(--text-secondary);
  font-variant-numeric: tabular-nums;
}

.err {
  font-size: 12px;
  color: var(--error);
  padding: 4px 6px;
  background: var(--error-light);
  border-radius: var(--radius-sm);
}
.global-err { margin-top: 8px; }

.chat-foot {
  border-top: 1px solid var(--border-light);
  padding: 8px 10px;
  background: var(--surface);
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.chat-foot textarea {
  width: 100%;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 8px;
  font: inherit;
  font-size: 13px;
  resize: vertical;
  min-height: 44px;
  max-height: 200px;
}
.chat-foot textarea:focus {
  outline: 2px solid var(--primary-blue-light);
  border-color: var(--primary-blue);
}
.foot-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  justify-content: flex-end;
}
.budget {
  flex: 1;
  font-size: 11px;
  color: var(--text-secondary);
  font-variant-numeric: tabular-nums;
}
.budget.over { color: var(--error); font-weight: 600; }
.token-meter {
  font: 500 10px ui-monospace, SFMono-Regular, monospace;
  color: var(--text-secondary);
  text-align: right;
  font-variant-numeric: tabular-nums;
  margin-top: -2px;
}
.token-meter.warn {
  color: var(--warning);
  font-weight: 700;
}
.ctx-block-banner {
  font-size: 11px;
  color: var(--error);
  background: var(--error-light);
  padding: 6px 8px;
  border-radius: var(--radius-sm);
  line-height: 1.4;
  margin-top: 4px;
}

/* Finding proposal cards — emitted by propose_finding(), rendered
   inline under the assistant turn. Save commits the file via the
   forwarder; Discard just dismisses (no server state to roll back). */
.findings-rail { display: flex; flex-direction: column; gap: 6px; }
.finding-card {
  border: 1px solid #a855f7;
  background: #faf5ff;
  border-radius: var(--radius-md);
  padding: 8px 10px;
  font-size: 12px;
}
.finding-card.saved { opacity: 0.65; }
.finding-head { display: flex; align-items: center; gap: 6px; }
.finding-icon { font-size: 13px; }
.finding-slug { font: 600 11px ui-monospace, SFMono-Regular, monospace; }
.finding-status { font-size: 11px; margin-left: auto; }
.finding-status.ok { color: var(--success); font-weight: 600; }
.finding-status.muted { color: var(--text-secondary); font-style: italic; }
.finding-status.err { color: var(--error); }
.finding-body { font-size: 11px; margin-top: 6px; }
.finding-body summary { cursor: pointer; color: var(--text-secondary); }
.finding-body pre {
  margin-top: 4px;
  padding: 6px;
  background: #fff;
  border: 1px solid var(--border-light);
  border-radius: var(--radius-sm);
  white-space: pre-wrap;
  font: 11px ui-monospace, SFMono-Regular, monospace;
  max-height: 240px;
  overflow-y: auto;
}
.finding-actions { display: flex; gap: 6px; justify-content: flex-end; margin-top: 6px; }
.finding-actions .primary, .finding-actions .secondary {
  font: 500 12px 'Google Sans', system-ui, sans-serif;
  border-radius: var(--radius-sm);
  padding: 4px 12px;
  cursor: pointer;
  border: none;
}
.finding-actions .primary { background: #a855f7; color: #fff; }
.finding-actions .secondary { background: var(--surface); color: var(--text-primary); border: 1px solid var(--border); }
.send, .cancel, .primary {
  background: var(--primary-blue);
  color: #fff;
  border: none;
  border-radius: var(--radius-sm);
  padding: 6px 14px;
  font: 500 13px 'Google Sans', system-ui, sans-serif;
  cursor: pointer;
}
.send:disabled { opacity: 0.4; cursor: not-allowed; }
.send:hover:not(:disabled), .primary:hover { background: var(--primary-blue-hover); }
.cancel {
  background: var(--error);
}
.cancel:hover { background: #b6261c; }

/* Collapsed-state strip: just shows the AI badge vertically */
.chat-panel.collapsed .chat-head {
  flex-direction: column;
  padding: 8px 4px;
  border-bottom: none;
  height: 100%;
}
.chat-panel.collapsed .title {
  writing-mode: vertical-rl;
  transform: rotate(180deg);
  margin-top: 8px;
}
</style>
