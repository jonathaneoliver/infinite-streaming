<script setup lang="ts">
/**
 * SessionDetails.vue — read-only identity / lifecycle fields. No
 * inputs, no mutations — just a label/value grid bound to the model.
 * Re-renders automatically as SSE updates flow into the cache.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));

const developerMode = computed(() => {
  return new URLSearchParams(window.location.search).get('developer') === '1';
});

function fmtMbps(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v.toFixed(2)} Mbps`;
}
function fmtBytes(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (v < 1024) return `${v} B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  if (v < 1024 * 1024 * 1024) return `${(v / 1024 / 1024).toFixed(2)} MB`;
  return `${(v / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function fmtDate(iso?: string | null): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}
function fmtDuration(firstIso?: string | null, lastIso?: string | null): string {
  if (!firstIso || !lastIso) return '—';
  const a = Date.parse(firstIso);
  const b = Date.parse(lastIso);
  if (!Number.isFinite(a) || !Number.isFinite(b)) return '—';
  const sec = Math.max(0, Math.floor((b - a) / 1000));
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  if (m < 60) return `${m}m ${sec % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/**
 * effectiveLastSeenAt — server's PlayerRecord.last_seen_at is
 * legitimately null for live sessions (only set on session-end
 * lifecycle close on some code paths). Fall back to
 * server_received_at_ms, which IS updated on every snapshot —
 * effectively the "most recent server activity" signal.
 * Returns ISO string or null when neither source has a value.
 */
function effectiveLastSeenAt(p: any): string | null {
  if (typeof p?.last_seen_at === 'string' && p.last_seen_at) return p.last_seen_at;
  const ms = p?.server_received_at_ms;
  if (typeof ms === 'number' && ms > 0) return new Date(ms).toISOString();
  return null;
}

const fields = computed(() => {
  const p = player.value;
  if (!p) return [] as { label: string; value: string }[];
  const cp = p.current_play ?? null;
  const sm = p.server_metrics ?? null;
  const raw = (p as any).raw_session ?? null;
  const port = raw?.x_forwarded_port_external ?? raw?.x_forwarded_port ?? null;
  // Group ID lives on the v1 session passthrough (the v2 PlayerRecord
  // doesn't model it as a first-class field yet). Show "—" when
  // ungrouped so the operator can confirm linking actually landed.
  const gid = raw?.group_id;
  const groupValue = typeof gid === 'string' && gid.length ? gid : '—';
  // Test-framework labels: only surface `test` + `run_id` when set
  // (operator-supplied via `harness labels set` from the test runner).
  // Skip when absent — non-test sessions shouldn't see empty rows.
  // Other test labels (cycle_idx, cap_mbps, total_stalls, etc.)
  // intentionally omitted — many of those are stamped outcomes that
  // duplicate measurement data and would confuse the operator. Issue
  // followup for richer label categorization if needed.
  const labels: Record<string, string> = (p as any).labels ?? {};
  const testLabel = typeof labels.test === 'string' ? labels.test : '';
  const runIDLabel = typeof labels.run_id === 'string' ? labels.run_id : '';
  // Master URL is the manifest entry the player loaded; the legacy
  // page also showed the "Last Request URL" (the most-recent network
  // log entry's URL); we don't track that here yet so omit it.
  const out: { label: string; value: string }[] = [
    { label: 'Player ID', value: p.id ?? '—' },
    { label: 'Display ID', value: String(p.display_id ?? '—') },
    { label: 'Play ID', value: cp?.id ?? '—' },
    { label: 'Group ID', value: groupValue },
    { label: 'Origination IP', value: p.origination_ip ?? '—' },
    { label: 'Player IP', value: p.player_ip ?? '—' },
    { label: 'Port', value: port != null ? String(port) : '—' },
    { label: 'User Agent', value: p.user_agent ?? '—' },
    { label: 'Master Manifest URL', value: cp?.manifest?.master_url ?? '—' },
    { label: 'First Request', value: fmtDate(p.first_seen_at) },
    // Server's last_seen_at is null on live sessions on some paths;
    // fall back to server_received_at_ms (the "last snapshot" signal)
    // so the operator sees actual freshness, not a misleading "—".
    { label: 'Last Request', value: fmtDate(effectiveLastSeenAt(p)) },
    { label: 'Session Duration', value: fmtDuration(p.first_seen_at, effectiveLastSeenAt(p)) },
    { label: 'Loops (server)', value: String(p.loop_count_server ?? 0) },
    { label: 'Control Rev', value: p.control_revision ?? '—' },
  ];
  // Shaper Avg removed from this identity grid — it's a runtime
  // metric, not identity, so it belongs in developerFields below
  // alongside Shaper Rate / Transfer Rate / etc.
  void sm; // sm still used by developerFields; suppress unused-var warning
  // Append test labels at the END so they don't push lifecycle
  // identifiers off-screen on narrow viewports, but still surface
  // when present.
  if (testLabel) out.push({ label: 'Test', value: testLabel });
  if (runIDLabel) out.push({ label: 'Run ID', value: runIDLabel });
  return out;
});

const developerFields = computed(() => {
  const p = player.value;
  const sm = p?.server_metrics;
  if (!p || !sm) return [] as { label: string; value: string }[];
  return [
    { label: 'Shaper Rate',         value: fmtMbps(sm.mbps_shaper_rate) },
    // Shaper Avg moved from identity grid into metrics 2026-05-26
    // (it's runtime telemetry, not session identity). Renders "—"
    // when no shaping is active — both shaper_rate and shaper_avg
    // are absent from server_metrics until a shape rule is applied.
    { label: 'Shaper (avg)',        value: fmtMbps(sm.mbps_shaper_avg) },
    { label: 'Transfer Rate',       value: fmtMbps(sm.mbps_transfer_rate) },
    { label: 'Transfer (avg)',      value: fmtMbps(sm.mbps_transfer_complete) },
    { label: 'Mbps In',             value: fmtMbps(sm.mbps_in) },
    { label: 'Mbps Out',            value: fmtMbps(sm.mbps_out) },
    { label: 'Mbps In (avg)',       value: fmtMbps(sm.mbps_in_avg) },
    { label: 'Mbps In (active)',    value: fmtMbps(sm.mbps_in_active) },
    { label: 'Measured Mbps',       value: fmtMbps(sm.measured_mbps) },
    { label: 'Bytes in (total)',    value: fmtBytes(sm.bytes_in_total) },
    { label: 'Bytes out (total)',   value: fmtBytes(sm.bytes_out_total) },
    { label: 'Bytes in (last)',     value: fmtBytes(sm.bytes_in_last) },
    { label: 'Bytes out (last)',    value: fmtBytes(sm.bytes_out_last) },
    { label: 'I/O window',          value: sm.measurement_window_io != null ? `${sm.measurement_window_io.toFixed(2)} s` : '—' },
    { label: 'Active window',       value: sm.measurement_window_active != null ? `${sm.measurement_window_active.toFixed(2)} s` : '—' },
  ];
});
</script>

<template>
  <div v-if="player">
    <div class="session-details">
      <div v-for="f in fields" :key="f.label" class="cell">
        <div class="lbl">{{ f.label }}</div>
        <div class="val" :title="f.value">{{ f.value }}</div>
      </div>
    </div>

    <details v-if="developerMode && developerFields.length" class="dev-block">
      <summary>Developer · raw transfer counters</summary>
      <div class="session-details">
        <div v-for="f in developerFields" :key="f.label" class="cell">
          <div class="lbl">{{ f.label }}</div>
          <div class="val" :title="f.value">{{ f.value }}</div>
        </div>
      </div>
    </details>
  </div>
</template>

<style scoped>
.session-details {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 8px 16px;
}

.cell {
  display: grid;
  gap: 2px;
  font-size: 13px;
}

.lbl {
  color: #6b7280;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}

.val {
  color: #111827;
  font-family: ui-monospace, monospace;
  /* Wrap long values (player_id UUID, master URL, user agent) so the
   * full string is visible and selectable. The grid column stays the
   * same width — long values just expand the row height. */
  word-break: break-all;
  white-space: normal;
  user-select: text;
}

.dev-block {
  margin-top: 12px;
  padding-top: 8px;
  border-top: 1px dashed #e5e7eb;
}
.dev-block > summary {
  font-size: 11px;
  color: #6b7280;
  cursor: pointer;
  user-select: none;
  padding: 4px 0;
  text-transform: uppercase;
  letter-spacing: 0.4px;
}
.dev-block > summary::-webkit-details-marker { display: none; }
.dev-block > summary::before {
  content: '▸ ';
  display: inline-block;
  width: 12px;
  color: #9ca3af;
}
.dev-block[open] > summary::before { content: '▾ '; }
.dev-block .session-details { margin-top: 8px; }
</style>
