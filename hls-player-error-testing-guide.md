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
