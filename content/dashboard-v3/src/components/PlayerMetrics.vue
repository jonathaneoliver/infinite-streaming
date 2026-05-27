<script setup lang="ts">
/**
 * PlayerMetrics.vue — read-only player telemetry (~30 fields).
 * Pure model→DOM binding. Updates every metrics tick (~1Hz) without
 * touching control state thanks to mergeMetricsOnly in the ingest.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));

// Match legacy display precision: seconds → 3 decimals, ms → integer,
// Mbps → 2 decimals, percent → 1 decimal followed by "%" (no space).
function fmtS(v: number | null | undefined, digits = 3): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v.toFixed(digits)}s`;
}
function fmtMs(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${Math.round(v)}ms`;
}
function fmtMbps(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v.toFixed(2)} Mbps`;
}
function fmtPct(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v.toFixed(1)}%`;
}
function fmtNum(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return String(v);
}
function fmtStr(v: string | null | undefined): string {
  return v && v.length ? v : '—';
}
function fmtTime(iso?: string | null): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleTimeString();
  } catch {
    return iso;
  }
}

const pm = computed(() => player.value?.player_metrics ?? null);
const sm = computed(() => player.value?.server_metrics ?? null);

const playerFields = computed(() => {
  const m = pm.value;
  if (!m) return [] as { label: string; value: string }[];
  return [
    { label: 'Last Event', value: fmtStr(m.last_event) },
    { label: 'Trigger Type', value: fmtStr(m.trigger_type) },
    { label: 'State', value: fmtStr(m.state) },
    { label: 'Event Time', value: fmtTime(m.event_time) },
    { label: 'Position', value: fmtS(m.position_s) },
    { label: 'Playback Rate', value: m.playback_rate != null ? `${m.playback_rate}x` : '—' },
    { label: 'Buffer Depth', value: fmtS(m.buffer_depth_s) },
    { label: 'Buffer End', value: fmtS(m.buffer_end_s) },
    { label: 'Seekable End', value: fmtS(m.seekable_end_s) },
    { label: 'Live Edge', value: fmtS(m.live_edge_s) },
    { label: 'Live Offset', value: fmtS(m.live_offset_s) },
    { label: 'True Offset', value: fmtS(m.true_offset_s) },
    { label: 'Display Res', value: fmtStr(m.display_resolution) },
    { label: 'Video Res', value: fmtStr(m.video_resolution) },
    { label: 'First Frame', value: fmtS(m.first_frame_time_s) },
    { label: 'Video Start', value: fmtS(m.video_start_time_s) },
    { label: 'Video Bitrate', value: fmtMbps(m.video_bitrate_mbps) },
    { label: 'Avg Network', value: fmtMbps(m.avg_network_bitrate_mbps) },
    { label: 'Network Bitrate', value: fmtMbps(m.network_bitrate_mbps) },
    { label: 'Video Quality', value: fmtPct(m.video_quality_pct) },
    { label: 'Frames Shown', value: fmtNum(m.frames_displayed) },
    { label: 'Dropped Frames', value: fmtNum(m.dropped_frames) },
    { label: 'Stalls', value: fmtNum(m.stalls) },
    { label: 'Stall Time', value: fmtS(m.stall_time_s) },
    { label: 'Last Stall', value: fmtS(m.last_stall_time_s) },
    { label: 'Browser', value: fmtStr(m.browser_family) },
    { label: 'Playback Engine', value: fmtStr(m.playback_engine) },
    { label: 'Error', value: fmtStr(m.error) },
    { label: 'Restarts', value: fmtNum(m.player_restarts) },
    { label: 'Loops (player)', value: fmtNum(m.loop_count_player) },
    { label: 'Profile Shifts', value: fmtNum(m.profile_shift_count) },
    { label: 'Source', value: fmtStr(m.source) },
  ];
});

const serverFields = computed(() => {
  const s = sm.value;
  if (!s) return [] as { label: string; value: string }[];
  return [
    { label: 'Mbps In', value: fmtMbps(s.mbps_in) },
    { label: 'Mbps Out', value: fmtMbps(s.mbps_out) },
    { label: 'Mbps In (avg)', value: fmtMbps(s.mbps_in_avg) },
    { label: 'Shaper Rate', value: fmtMbps(s.mbps_shaper_rate) },
    { label: 'RTT', value: fmtMs(s.rtt_ms) },
    { label: 'RTT min', value: fmtMs(s.rtt_min_ms) },
    { label: 'RTT max', value: fmtMs(s.rtt_max_ms) },
    { label: 'RTT var', value: fmtMs(s.rtt_var_ms) },
    { label: 'RTO', value: fmtMs(s.rto_ms) },
    { label: 'Path ping', value: fmtMs(s.path_ping_rtt_ms) },
    { label: 'Bytes in', value: fmtNum(s.bytes_in_total) },
    { label: 'Bytes out', value: fmtNum(s.bytes_out_total) },
    { label: 'Server Rendition', value: fmtStr(s.server_rendition) },
    { label: 'Server Rendition Mbps', value: fmtMbps(s.rendition_mbps) },
  ];
});
</script>

<template>
  <div v-if="player">
    <div class="grid">
      <div v-for="f in playerFields" :key="f.label" class="cell">
        <div class="lbl">{{ f.label }}</div>
        <div class="val">{{ f.value }}</div>
      </div>
    </div>
    <h3>Server</h3>
    <div class="grid">
      <div v-for="f in serverFields" :key="f.label" class="cell">
        <div class="lbl">{{ f.label }}</div>
        <div class="val">{{ f.value }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
  gap: 8px 16px;
  margin-bottom: 16px;
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
}

h3 {
  font-size: 12px;
  font-weight: 600;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  margin: 16px 0 8px 0;
}
</style>
