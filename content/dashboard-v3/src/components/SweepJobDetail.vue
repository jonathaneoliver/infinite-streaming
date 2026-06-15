<script setup lang="ts">
/**
 * SweepJobDetail — the full per-job detail block for a sweep experiment, reused
 * by the Sweep tab's board (click-to-expand a card) and its lineage graph (click
 * a node). Renders the recipe (from raw_json), provenance + trigger, outcome, and
 * a derived next-action — so a job is fully understandable from CH state alone.
 */

export interface Experiment {
  exp_id: string;
  class: string;
  status: string;
  kind: string;
  platform: string;
  protocol: string;
  mode: string;
  recipe: string;
  arm: string;
  group_id: string;
  parent: string;
  depth: number;
  why: string;
  why_text: string;
  verdict: string;
  player_id: string;
  play_id: string;
  owner: string;
  claimed_at: string;
  raw_json: string;
  score: number;
  created_at: string;
  updated_at: string;
}

interface RawExperiment {
  content?: string;
  duration_s?: number;
  reps?: number;
  rep_group?: string;
  fault?: Record<string, unknown>;
  shape?: Record<string, unknown>;
  content_manipulation?: Record<string, unknown>;
  transfer_timeouts?: Record<string, unknown>;
  result?: { verdict?: string; labels?: string[]; note?: string };
  issue_url?: string;
}

defineProps<{ e: Experiment }>();

// Parse + cache the full recipe-of-record out of raw_json (legacy rows have none).
const rawCache = new Map<string, RawExperiment>();
function rawOf(e: Experiment): RawExperiment {
  if (!e.raw_json) return {};
  let r = rawCache.get(e.raw_json);
  if (!r) {
    try { r = JSON.parse(e.raw_json) as RawExperiment; } catch { r = {}; }
    rawCache.set(e.raw_json, r);
  }
  return r;
}

function kv(obj?: Record<string, unknown>): string {
  if (!obj) return '';
  return Object.entries(obj)
    .filter(([, v]) => v !== '' && v !== null && v !== undefined)
    .map(([k, v]) => `${k}=${v}`)
    .join(' · ');
}

function chDateToLocal(s: string): string {
  if (!s || s.startsWith('1970')) return '—';
  const d = new Date(s.replace(' ', 'T') + 'Z');
  return isNaN(d.getTime()) ? s : d.toLocaleString();
}

function isStaleClaim(claimedAt: string): boolean {
  if (!claimedAt || claimedAt.startsWith('1970')) return false;
  const d = new Date(claimedAt.replace(' ', 'T') + 'Z');
  return !isNaN(d.getTime()) && Date.now() - d.getTime() > 60 * 60 * 1000; // > 60 min
}

function verdictClass(v: string): string {
  switch (v) {
    case 'aberration': return 'v-aberration';
    case 'notable': return 'v-notable';
    case 'clean': return 'v-clean';
    case 'inconclusive': return 'v-inconclusive';
    default: return 'v-none';
  }
}

// Derived "what to do next" — a pure function of the row (the agenda logic), so
// the page tells you how to keep going from CH state alone.
function nextAction(e: Experiment): { label: string; tone: 'go' | 'warn' | 'muted' } {
  switch (e.status) {
    case 'backlog':
      return { label: 'Claim & run', tone: 'go' };
    case 'running':
      if (!e.play_id) return isStaleClaim(e.claimed_at)
        ? { label: 'Reap & re-claim (stale)', tone: 'warn' }
        : { label: 'Running — probe in flight', tone: 'muted' };
      if (!e.verdict) return { label: `Analyze play ${e.play_id.slice(0, 8)}`, tone: 'go' };
      return { label: 'Analyzed — awaiting move', tone: 'muted' };
    case 'found':
      return { label: 'Investigate → isolate → promote', tone: 'go' };
    case 'review':
    case 'feedback':
      return { label: 'Needs a human', tone: 'warn' };
    case 'done':
      return { label: 'Done (clean)', tone: 'muted' };
    default:
      return { label: '—', tone: 'muted' };
  }
}
</script>

<template>
  <div class="detail">
    <div class="na" :class="'na-' + nextAction(e).tone">
      <span class="na-k">next</span> {{ nextAction(e).label }}
    </div>

    <h3>Recipe</h3>
    <dl>
      <div><dt>class</dt><dd>{{ e.class || 'config' }}</dd></div>
      <div><dt>content</dt><dd>{{ rawOf(e).content || '—' }}</dd></div>
      <div><dt>mode</dt><dd>{{ e.mode }}<span v-if="rawOf(e).duration_s"> · {{ rawOf(e).duration_s }}s</span></dd></div>
      <div v-if="rawOf(e).fault"><dt>fault</dt><dd>{{ kv(rawOf(e).fault) }}</dd></div>
      <div v-if="rawOf(e).shape"><dt>shape</dt><dd>{{ kv(rawOf(e).shape) }}</dd></div>
      <div v-if="rawOf(e).content_manipulation"><dt>content&nbsp;manip</dt><dd>{{ kv(rawOf(e).content_manipulation) }}</dd></div>
      <div v-if="rawOf(e).transfer_timeouts"><dt>xfer&nbsp;timeout</dt><dd>{{ kv(rawOf(e).transfer_timeouts) }}</dd></div>
    </dl>

    <h3>Provenance &amp; trigger</h3>
    <dl>
      <div><dt>kind</dt><dd>{{ e.kind }}<span v-if="e.arm"> · {{ e.arm }}</span></dd></div>
      <div v-if="e.group_id"><dt>group</dt><dd>{{ e.group_id }}</dd></div>
      <div v-if="e.parent"><dt>parent</dt><dd>{{ e.parent }} · depth {{ e.depth }}</dd></div>
      <div v-if="rawOf(e).reps"><dt>reps</dt><dd>{{ rawOf(e).reps }}<span v-if="rawOf(e).rep_group"> · {{ rawOf(e).rep_group }}</span></dd></div>
      <div><dt>why</dt><dd>{{ e.why_text || e.why || '— (no trigger recorded)' }}</dd></div>
    </dl>

    <h3>Outcome</h3>
    <dl>
      <div><dt>verdict</dt><dd><span v-if="e.verdict" class="verdict" :class="verdictClass(e.verdict)">{{ e.verdict }}</span><span v-else>—</span></dd></div>
      <div v-if="rawOf(e).result?.note"><dt>note</dt><dd>{{ rawOf(e).result?.note }}</dd></div>
      <div v-if="rawOf(e).result?.labels?.length"><dt>labels</dt><dd class="labels"><code v-for="l in rawOf(e).result?.labels" :key="l">{{ l }}</code></dd></div>
      <div v-if="e.owner"><dt>owner</dt><dd>{{ e.owner }} · claimed {{ chDateToLocal(e.claimed_at) }}</dd></div>
      <div v-if="e.play_id"><dt>play</dt><dd>{{ e.play_id }}</dd></div>
      <div v-if="rawOf(e).issue_url"><dt>issue</dt><dd><a :href="rawOf(e).issue_url" target="_blank" rel="noopener">{{ rawOf(e).issue_url }}</a></dd></div>
      <div><dt>created</dt><dd>{{ chDateToLocal(e.created_at) }} · updated {{ chDateToLocal(e.updated_at) }}</dd></div>
    </dl>
  </div>
</template>

<style scoped>
.detail { margin: .5rem 0 .2rem; padding-top: .5rem; border-top: 1px solid var(--border-light, #e8eaed); font-size: .82rem; }
.detail h3 { font-size: .66rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-secondary, #5f6368); margin: .55rem 0 .25rem; }
.detail h3:first-of-type { margin-top: .4rem; }
.detail dl { margin: 0; }
.detail dl > div { display: flex; gap: .5rem; padding: .08rem 0; align-items: baseline; }
.detail dt { flex: none; width: 6.5rem; color: var(--text-disabled, #9aa0a6); font-size: .72rem; }
.detail dd { margin: 0; color: var(--text-primary, #202124); font-size: .76rem; word-break: break-word; min-width: 0; }
.detail dd.labels { display: flex; flex-wrap: wrap; gap: .25rem; }
.detail dd.labels code { background: var(--surface-hover, #f1f3f4); border-radius: 3px; padding: 0 .3rem; font-size: .68rem; }
.na { border-radius: 5px; padding: .25rem .5rem; font-size: .76rem; font-weight: 600; margin-bottom: .4rem; }
.na-k { font-size: .62rem; text-transform: uppercase; letter-spacing: .04em; opacity: .7; margin-right: .35rem; }
.na-go { background: var(--success-light, #e6f4ea); color: var(--success, #1e8e3e); }
.na-warn { background: var(--warning-light, #fef7e0); color: #a86a00; }
.na-muted { background: var(--surface-hover, #f1f3f4); color: var(--text-secondary, #5f6368); }
.verdict { font-size: .7rem; font-weight: 600; padding: 0 .4rem; border-radius: 8px; }
.v-aberration { background: var(--error-light, #fce8e6); color: var(--error, #d93025); }
.v-notable { background: var(--warning-light, #fef7e0); color: #a86a00; }
.v-clean { background: var(--success-light, #e6f4ea); color: var(--success, #1e8e3e); }
.v-inconclusive { background: var(--surface-hover, #f1f3f4); color: var(--text-disabled, #9aa0a6); }
</style>
