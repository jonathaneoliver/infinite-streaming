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

// #550 helpers — the new residency + delta columns arrive in ms;
// chRowAdapter converts to seconds for tile parity with legacy fields.
// Treat them through fmtS / fmtNum / fmtPct accordingly.
type PMExt = NonNullable<typeof pm.value> & {
  // Phase 1 residency (seconds — chRowAdapter divides ms by 1000)
  playing_time_s?: number | null;
  pausing_time_s?: number | null;
  buffering_time_s?: number | null;
  stalling_time_s?: number | null;
  idling_time_s?: number | null;
  seeking_time_s?: number | null;
  trickplaying_time_s?: number | null;
  playing_count?: number | null;
  pausing_count?: number | null;
  buffering_count?: number | null;
  stalling_count?: number | null;
  idling_count?: number | null;
  seeking_count?: number | null;
  trickplaying_count?: number | null;
  stall_duration_s?: number | null;
  buffering_duration_s?: number | null;
  // Phase 2 status + error
  playback_status?: string | null;
  playback_reason?: string | null;
  error_code?: number | null;
  error_domain?: string | null;
  terminal_error_code?: number | null;
  terminal_error_domain?: string | null;
  error_count?: number | null;
  // Phase 4 device taxonomy
  os_version_major?: number | null;
  os_version_minor?: number | null;
  app_version?: string | null;
  device_class?: string | null;
  device_model?: string | null;
  player_tech?: string | null;
  screen_width_px?: number | null;
  screen_height_px?: number | null;
  screen_density?: number | null;
};

function fmtOsVersion(major?: number | null, minor?: number | null): string {
  if (major == null && minor == null) return '—';
  return `${major ?? 0}.${minor ?? 0}`;
}

function fmtScreen(w?: number | null, h?: number | null, d?: number | null): string {
  if (!w && !h) return '—';
  if (d && d > 0) return `${w ?? 0}×${h ?? 0} @${d.toFixed(1)}x`;
  return `${w ?? 0}×${h ?? 0}`;
}

// Display residency time as a human-readable mix (h/m/s) for big
// values, falling back to fmtS for short ones.
function fmtDur(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (v < 60) return fmtS(v, 2);
  const m = Math.floor(v / 60);
  const s = v - m * 60;
  if (v < 3600) return `${m}m ${s.toFixed(1)}s`;
  const h = Math.floor(v / 3600);
  const rm = m - h * 60;
  return `${h}h ${rm}m ${s.toFixed(0)}s`;
}

const playerFields = computed(() => {
  const m = pm.value as PMExt | null;
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
    // Browser / Playback Engine / Error (legacy player_error string)
    // superseded by Phase 4 player_tech (Session Details) and the
    // structured Error Code / Domain / Details / Terminal Error tiles
    // in the Outcome section below.
    // "Restarts" moved to SessionDetails as "Attempt" (attempt_id is
    // the canonical schema field; max(attempt_id) per play = total
    // attempts including recoveries).
    { label: 'Loops (player)', value: fmtNum(m.loop_count_player) },
    { label: 'Profile Shifts', value: fmtNum(m.profile_shift_count) },
    // `source` removed — superseded by Phase 4 `player_tech` in
    // Session Details.
  ];
});

// #550 Phase 1: state residency — one tile per state pair (time + count).
const residencyFields = computed(() => {
  const m = pm.value as PMExt | null;
  if (!m) return [] as { label: string; value: string }[];
  return [
    { label: 'Playing Time', value: fmtDur(m.playing_time_s) },
    { label: 'Playing Count', value: fmtNum(m.playing_count) },
    { label: 'Pausing Time', value: fmtDur(m.pausing_time_s) },
    { label: 'Pausing Count', value: fmtNum(m.pausing_count) },
    { label: 'Buffering Time', value: fmtDur(m.buffering_time_s) },
    { label: 'Buffering Count', value: fmtNum(m.buffering_count) },
    { label: 'Stalling Time', value: fmtDur(m.stalling_time_s) },
    { label: 'Stalling Count', value: fmtNum(m.stalling_count) },
    { label: 'Idling Time', value: fmtDur(m.idling_time_s) },
    { label: 'Idling Count', value: fmtNum(m.idling_count) },
    { label: 'Seeking Time', value: fmtDur(m.seeking_time_s) },
    { label: 'Seeking Count', value: fmtNum(m.seeking_count) },
    { label: 'Trickplaying Time', value: fmtDur(m.trickplaying_time_s) },
    { label: 'Trickplaying Count', value: fmtNum(m.trickplaying_count) },
    { label: 'Last Stall Duration', value: fmtS(m.stall_duration_s) },
    { label: 'Last Buffer Duration', value: fmtS(m.buffering_duration_s) },
  ];
});

// #550 Phase 2: outcome + structured error fields.
const outcomeFields = computed(() => {
  const m = pm.value as PMExt | null;
  if (!m) return [] as { label: string; value: string }[];
  return [
    { label: 'Status', value: fmtStr(m.playback_status) },
    { label: 'Reason', value: fmtStr(m.playback_reason) },
    { label: 'Error Code', value: m.error_code ? fmtNum(m.error_code) : '—' },
    { label: 'Error Domain', value: fmtStr(m.error_domain) },
    { label: 'Terminal Error Code', value: m.terminal_error_code ? fmtNum(m.terminal_error_code) : '—' },
    { label: 'Terminal Error Domain', value: fmtStr(m.terminal_error_domain) },
    { label: 'Error Count', value: fmtNum(m.error_count) },
  ];
});

// Device taxonomy lives in SessionDetails (one stamp per session
// vs per-snapshot residency/outcome), so PlayerMetrics doesn't
// duplicate the tiles here.

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
    <div class="grid">
      <div v-for="f in residencyFields" :key="f.label" class="cell">
        <div class="lbl">{{ f.label }}</div>
        <div class="val">{{ f.value }}</div>
      </div>
    </div>
    <div class="grid">
      <div v-for="f in outcomeFields" :key="f.label" class="cell">
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
