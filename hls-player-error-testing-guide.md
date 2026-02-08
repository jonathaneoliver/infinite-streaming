# HLS Player Error Testing Guide

## Overview

This guide provides a comprehensive list of error scenarios to test HLS video players (Shaka Player, hls.js, and Video.js) for robustness and error handling.

---

## 1. Network Errors

### Manifest Load Errors
- Invalid/unreachable URLs for master or variant playlists
- 404/410/500/503 errors on playlist requests
- CORS misconfiguration (missing Access-Control-Allow-Origin headers)

### Segment Load Errors
- Missing video/audio segments (404/410)
- Corrupted or truncated segment data
- Network timeouts during segment download

### Intermittent Network Issues
- Abrupt connection drops mid-stream
- Packet loss simulation
- DNS resolution failures

### Transport-Level Faults (Port-Wide)
- Connect timeout on playlist URL (port-wide DROP on initial connect)
- Immediate connect failure on playlist URL (port-wide REJECT on initial connect)
- Connection reset mid-segment (TCP RST / REJECT on established flow)
- Established-flow packet drop (port-wide DROP on established flow)

### Network Throttling/Fluctuations
- Slow/unpredictable bandwidth to test ABR (Adaptive Bitrate) switching
- Sudden bandwidth drops during playback
- Jitter and latency variations

---

## 2. Playlist/Manifest Errors

### Malformed Playlists
- Syntax errors and invalid M3U8 formatting
- Missing required HLS tags (e.g., #EXTM3U)
- Invalid tag parameters or values

### Corrupted Playlists
- Incorrect or missing `EXT-X-TARGETDURATION`
- Mismatched `EXT-X-MEDIA-SEQUENCE` values
- Incorrect `EXT-X-DISCONTINUITY` tag usage
- Missing or malformed `EXT-X-ENDLIST` in VOD
- Incorrect `#EXT-X-STREAM-INF:BANDWIDTH` / `AVERAGE-BANDWIDTH` values (e.g., wildly off from actual throughput)

### Unsupported Features
- Tags or codec identifiers unsupported by the player/browser
- Incompatible HLS protocol versions

### Segment Duration Issues
- Segments that violate target duration constraints
- Inconsistent segment durations
- Missing segment references
- Incorrect `#EXTINF` duration values (e.g., not matching actual segment length)

---

## 3. Media Format/Codec Errors

### Codec Mismatches
- Declared codecs in playlist don't match actual media
- Unsupported codecs for the browser/device (e.g., HEVC where unsupported)

### Corrupted Media Segments
- Truncated TS or fMP4 segments
- Incomplete or unplayable media data
- Invalid PTS/DTS timestamps
- Incorrect presentation timestamps / timeline discontinuities that break playback or A/V sync

### Unexpected Codec Changes
- Abrupt audio/video codec switches mid-stream
- Resolution changes without proper signaling

---

## 4. Adaptive Bitrate (ABR) Switching Issues

### Missing Renditions
- Remove or make quality levels unavailable mid-playback
- Test failover when preferred bitrate becomes unavailable

### Abrupt Quality Changes
- Large jumps between lowest and highest quality
- Test smooth transition handling and buffer management

### Rendition Blacklisting
- Multiple failures on specific quality levels to trigger blacklisting behavior

---

## 5. DRM/Encryption Errors

### Key Load Failures
- 403/404/500 errors on encryption key URLs (`EXT-X-KEY`)
- Timeout retrieving decryption keys

### License Server Issues
- License denial or authentication failures
- Expired or invalid licenses
- License server downtime (for Widevine/FairPlay/PlayReady)

### Encryption Mismatches
- Key format incompatibility
- Missing initialization vectors (IV)

---

## 6. Audio/Subtitle Track Issues

### Auxiliary Track Failures
- Missing alternate audio tracks or subtitle files
- 404s on `EXT-X-MEDIA` references
- Broken WebVTT/TTML subtitle files

### Synchronization Issues
- Audio/video out of sync
- Subtitle timing misalignment
- Malformed caption data

---

## 7. Live Streaming Specific Errors

### Playlist Update Issues
- Delayed or missing playlist updates
- Inconsistent media sequence numbers
- Premature `EXT-X-ENDLIST` in live streams

### Partial Segment Availability
- Segments not yet available when player requests them
- "Live edge" boundary violations

### Clock Drift
- Server time vs. player time misalignment
- `EXT-X-PROGRAM-DATE-TIME` inconsistencies

---

## 8. Browser/Device Specific Issues

### Media Source Extensions (MSE) Errors
- QuotaExceededError (buffer overflow)
- SourceBuffer append errors

### Memory Constraints
- Test on low-memory devices
- Long playback sessions causing memory leaks

### Platform Limitations
- iOS Safari specific behaviors (native HLS)
- Android fragmentation issues

---

## 9. Edge Cases & Stress Tests

### Rapid Seeking
- Multiple rapid seeks to test buffer management
- Seeking to unbuffered ranges

### Long-Running Sessions
- Multi-hour playback to detect memory leaks
- Live streams with frequent playlist updates

### Race Conditions
- Simultaneous quality switch and seek operations
- Player state changes during error recovery

---

## HTTP Response Code Handling Comparison

| HTTP Code | Shaka Player | hls.js | Video.js/VHS |
|-----------|-------------|---------|--------------|
| **404/410** | Skips segment if possible in live. Retries, may fatal if persistent. | Sk

---

## Session Grouping for Comparative Testing

### Overview

InfiniteStream supports **session grouping** to apply identical failure scenarios and network conditions across multiple streaming sessions simultaneously. This enables direct comparison of how different players (e.g., HLS.js vs Safari native, or desktop vs mobile) handle the exact same test conditions.

### Method 1: Player ID Suffix Pattern (Automatic Grouping)

Sessions are automatically grouped when their `player_id` parameter contains a matching `_G###` suffix pattern.

#### Usage Example

```bash
# Tab 1: HLS.js player
http://localhost:30081/go-live/bbb/master.m3u8?player_id=hlsjs_desktop_G001

# Tab 2: Safari native player  
http://localhost:30081/go-live/bbb/master.m3u8?player_id=safari_native_G001

# Tab 3: Video.js player (same group)
http://localhost:30081/go-live/bbb/master.m3u8?player_id=videojs_G001
```

All sessions with `_G001` suffix will be automatically grouped. Any control changes applied to one session (failure injection, network shaping, etc.) will be synchronized to all other sessions in the group.

#### Group ID Format

- Pattern: `_G` followed by digits (e.g., `_G001`, `_G042`, `_G999`)
- The player identifier before the suffix can be anything descriptive
- Group IDs are case-sensitive

#### Example Test Scenarios

**Compare HLS.js vs Safari Native:**
```
player_id=hlsjs_macos_G100
player_id=safari_native_G100
```

**Compare Desktop vs Mobile:**
```
player_id=safari_desktop_G200
player_id=safari_ios_G200
```

**Compare Multiple Player Libraries:**
```
player_id=hlsjs_v1.4_G300
player_id=shaka_v4.3_G300
player_id=videojs_v8_G300
```

### Method 2: UI-Based Manual Grouping

Sessions can also be grouped manually through the Testing UI, regardless of their `player_id`.

#### Steps:

1. Open the Testing page (`/dashboard/testing.html`)
2. Ensure you have 2 or more active sessions
3. Check the checkbox next to each session you want to group
4. Click the "Link Selected Sessions" button
5. Sessions are now grouped and will show a green group badge

#### Unlinking Sessions

- Click the "Unlink" button in the group info banner below the session tabs
- Or use the API: `POST /api/session-group/unlink` with `{"session_id": "1"}`

### Visual Indicators

Grouped sessions are displayed with:
- **Green left border** on session tabs
- **Group badge** showing the group ID (e.g., `G001`)
- **Group info banner** showing linked session details
- **Unlink button** for easy separation

### Synchronized Controls

When sessions are grouped, the following controls are synchronized across all group members:

#### Failure Injection:
- Segment failure type and frequency
- Manifest failure type and frequency  
- Master manifest failure type and frequency
- Transport fault type (DROP/REJECT)
- Transport fault timing and frequency

#### Network Shaping:
- Bandwidth throttling (rate_mbps)
- Network delay (delay_ms)
- Packet loss percentage (loss_pct)
- Network shaping patterns (multi-step bandwidth profiles)

### API Endpoints

#### Link Sessions
```bash
POST /api/session-group/link
Content-Type: application/json

{
  "session_ids": ["1", "2", "3"],
  "group_id": "G001"  # Optional - auto-generated if omitted
}
```

#### Unlink Session
```bash
POST /api/session-group/unlink
Content-Type: application/json

{
  "session_id": "1"
}
```

#### Get Group Members
```bash
GET /api/session-group/{groupId}
```

Returns all sessions in the specified group.

### Testing Workflow Example

1. **Setup**: Open 2 browser tabs/windows:
   - Tab A: HLS.js player with `?player_id=hlsjs_G500`
   - Tab B: Safari native with `?player_id=safari_G500`

2. **Configure**: In the Testing UI, select session from group G500

3. **Apply Failures**: 
   - Set segment failure to "404" every 10 requests
   - Both players will experience identical failures

4. **Apply Network Shaping**:
   - Set bandwidth to 2 Mbps
   - Both sessions will have identical bandwidth constraints

5. **Observe**: Compare how each player handles the same conditions:
   - Buffer behavior
   - ABR switching decisions
   - Error recovery timing
   - User experience differences

6. **Iterate**: Adjust failure patterns and network conditions while maintaining synchronization

### Notes

- Session grouping works with both LL-HLS and LL-DASH streams
- Groups persist until sessions are released or explicitly unlinked
- Up to 10 sessions can be grouped together
- Group settings propagate in real-time (no page refresh needed)
- Each session maintains its own performance metrics and bandwidth measurements

---
