# HTTP API Reference

The dashboard is a thin client over this API. Every fault-injection action, every shaping change, every session control the UI exposes is available here — so anything you can do in the browser, a test script or CI job can do too. There are no UI-only controls.

All endpoints are exposed through nginx on port `30000` (Docker Compose and k3s release) or `40000` (k3s dev). nginx routes them to the backing service based on path.

For the **fault-injection** surface (`/api/session/*` patch payloads, `/api/nftables/*` shaping), see [`docs/FAULT_INJECTION.md`](FAULT_INJECTION.md) — this page only summarises those endpoints and points to the full reference.

## go-live (manifest generation)

Prefix: `/go-live/`

| Method | Path | Purpose |
|---|---|---|
| GET | `/go-live/healthz` | Liveness check |
| GET | `/go-live/api/status` | All active workers, streams, and per-stream stats |
| GET | `/go-live/api/tick-stats/{content}` | LL-HLS tick cadence for one content worker (last tick, 5m avg, variant/audio counts) |
| GET | `/go-live/api/dash-tick-stats/{content}?duration=ll\|2s\|6s` | DASH tick stats per window |
| DELETE | `/go-live/api/stop/{id}` | Stop a running worker |

Streaming URLs (generated on demand):

| Method | Path | Purpose |
|---|---|---|
| GET | `/go-live/{content}/master.m3u8` | LL-HLS master |
| GET | `/go-live/{content}/master_{2s\|6s}.m3u8` | Segmented-variant master |
| GET | `/go-live/{content}/playlist_{variant}.m3u8` | LL variant playlist |
| GET | `/go-live/{content}/playlist_{2s\|6s}_{variant}.m3u8` | Segmented variant playlist |
| GET | `/go-live/{content}/manifest.mpd` | LL-DASH |
| GET | `/go-live/{content}/manifest_{2s\|6s}.mpd` | Segmented DASH |
| GET | `/go-live/{content}/{path}.{m4s\|ts\|mp4\|m4a\|cmfv\|cmfa\|webm\|aac\|webvtt}` | Media segments — **served directly by nginx from disk**, not by go-live |

## go-upload (content + jobs)

### Content and sources

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness check |
| GET | `/api/content` | List encoded output content (everything under `/media/dynamic_content`) |
| GET | `/api/sources` | List source files (everything under `/media/originals`) |
| GET | `/api/sources/{source_id}` | Source metadata: duration, resolution, codec |
| DELETE | `/api/sources/{source_id}` | Delete source (rejected if active encoding job references it) |
| POST | `/api/sources/scan-originals` | Re-scan `/media/originals` and index new files |
| POST | `/api/sources/{source_id}/reencode` | Kick a re-encode. Form: `output_name, codec_selection, partial_durations[]` |
| POST | `/api/sources/batch-reencode-json` | Batch re-encode. JSON: `{source_ids:[], codec_selection, hls_format, segment_duration}` |
| POST | `/api/content/{content_name}/generate-byteranges` | Rebuild DASH byterange map |

### Upload

Two flavours: single-shot (small files) and chunked (large files).

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/upload/active` | Count of in-flight uploads + queue depth |
| POST | `/api/upload` | Single-step: multipart file + encoding config → enqueue |
| POST | `/api/upload/init` | Start chunked session. Form: `filename, file_size, codec_selection, output_name, hls_format, padding, segment_duration` → returns `job_id` |
| POST | `/api/upload/chunk/{job_id}` | Multipart chunk append |
| POST | `/api/upload/complete/{job_id}` | Finalize, validate hash, enqueue encode |

### Jobs

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/jobs` | List all encoding jobs with status |
| GET | `/api/jobs/{job_id}` | Job detail + config |
| POST | `/api/jobs/{job_id}/cancel` | Cancel queued, uploading, or running job |
| DELETE | `/api/jobs/{job_id}` | Delete job record and its source file |
| WS | `/api/jobs/{job_id}/stream` | WebSocket stream of live encoder log lines |

### First-run setup

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/setup` | Wizard status (`ready`, `incomplete`) |
| POST | `/api/setup/initialize` | Create storage layout + indexes |
| POST | `/api/setup/seed` | Seed a demo content item |

## go-proxy (sessions + faults)

### Session listing and lifecycle

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/sessions` | All active sessions with bound ports and state |
| GET | `/api/sessions/stream` | **SSE** — live-stream of session updates (buffering disabled, 1h read timeout) |
| POST | `/api/clear-sessions` | Destroy every session (release proxy ports + nftables rules) |
| GET | `/api/session/{id}` | Session detail |
| DELETE | `/api/session/{id}` | Remove one session |
| PATCH | `/api/session/{id}` | **Primary fault/shaping configuration** — see [`FAULT_INJECTION.md`](FAULT_INJECTION.md) |
| POST | `/api/session/{id}/update` | Legacy alias for PATCH |
| POST | `/api/session/{id}/metrics` | Post observational metrics (no `control_revision` bump) |
| GET | `/api/session/{id}/network` | Network log ring buffer (last 200 entries) |
| POST | `/api/failure-settings/{id}` | Legacy per-session fault settings (PATCH is preferred) |

Network log response shape:

```json
{
  "session_id": "...",
  "count": 42,
  "entries": [
    {
      "timestamp": "...",
      "method": "GET",
      "url": "...",
      "path": "...",
      "request_kind": "segment|manifest|master_manifest",
      "status": 200,
      "bytes_in": 1234,
      "bytes_out": 1234567,
      "content_type": "video/mp4",
      "dns_ms": 2.5, "connect_ms": 5.2, "tls_ms": 0,
      "ttfb_ms": 12.8, "transfer_ms": 45.3, "total_ms": 65.8,
      "faulted": false,
      "fault_type": "", "fault_action": "", "fault_category": ""
    }
  ]
}
```

### Session grouping

| Method | Path | Body |
|---|---|---|
| POST | `/api/session-group/link` | `{session_ids:[...], group_id?:"..."}` — creates or extends a group (max 10) |
| POST | `/api/session-group/unlink` | `{session_id:"..."}` |
| GET | `/api/session-group/{groupId}` | Members |

### Network shaping (`tc` / nftables)

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/nftables/capabilities` | Is shaping available on this host? (Linux only) |
| GET | `/api/nftables/status` | Per-port state across all sessions |
| GET | `/api/nftables/port/{port}` | One port's shaping config |
| POST | `/api/nftables/bandwidth/{port}` | `{rate_mbps}` |
| POST | `/api/nftables/loss/{port}` | `{percent}` |
| POST | `/api/nftables/shape/{port}` | Full shape config |
| POST | `/api/nftables/pattern/{port}` | Step-pattern config |

### Diagnostics

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/external-ips` | Rolling + lifetime unique client IPs seen |
| GET | `/api/version` | Build version / commit SHA |
| GET | `/debug` | Internal debug HTML dashboard |

## CORS and caching

All API responses include `Access-Control-Allow-Origin: *` and `Cache-Control: no-cache, no-store, must-revalidate` headers (applied by nginx). Media segments are the exception — they are immutable and served with `expires 1y`.
