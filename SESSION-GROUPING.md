# Session Grouping Feature

## Quick Start

Synchronize failure injection and network shaping across multiple streaming sessions for comparative player testing.

### Automatic Grouping via Player ID

Add `_G###` suffix to your player_id parameter:

```bash
# HLS.js
http://localhost:30081/go-live/bbb/master.m3u8?player_id=hlsjs_G001

# Safari Native
http://localhost:30081/go-live/bbb/master.m3u8?player_id=safari_G001
```

Sessions with matching group IDs will automatically synchronize all control settings.

### Manual Grouping via UI

1. Open `/dashboard/testing.html`
2. Select 2+ sessions using checkboxes
3. Click "Link Selected Sessions"
4. All selected sessions are now grouped

## What Gets Synchronized

- **Failure Injection**: Segment, manifest, and transport failures
- **Network Shaping**: Bandwidth limits, packet loss, delay, and patterns
- **Transport Faults**: DROP/REJECT timing and frequency

## Visual Indicators

- Green left border on session tabs
- Group badge showing group ID
- "Linked with" banner showing other group members
- Unlink button for easy separation

## Use Cases

### Compare Player Implementations
Test how HLS.js vs Safari native handle identical network conditions.

### Cross-Device Testing
Compare desktop vs mobile player behavior under same constraints.

### Multi-Player Validation
Test multiple player libraries (HLS.js, Shaka, Video.js) simultaneously.

### A/B Testing
Compare different player configurations or versions side-by-side.

## API Reference

### Link Sessions
```bash
POST /api/session-group/link
{"session_ids": ["1", "2"], "group_id": "G001"}
```

### Unlink Session
```bash
POST /api/session-group/unlink
{"session_id": "1"}
```

### Get Group Info
```bash
GET /api/session-group/G001
```

## Implementation Details

- Backend: Go (go-proxy/cmd/server/main.go)
- Frontend: JavaScript (content/dashboard/testing.html)
- Storage: Memcache (ephemeral, session-lifetime)
- Limits: Up to 10 sessions per group

## For More Information

See the full documentation in `hls-player-error-testing-guide.md` under "Session Grouping for Comparative Testing".
