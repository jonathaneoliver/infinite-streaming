# Content Tab Testing Guide

## Overview
The Content tab provides real-time manipulation of HLS master playlists as they pass through the go-proxy, enabling testing of player behavior under various content constraints.

## Features

### 1. Strip CODEC Information
**Purpose:** Test player behavior when CODEC information is missing from the master playlist.

**Impact:** Players like ExoPlayer cannot use "chunkless prepare" without CODEC strings, forcing them to fetch multiple video streams to determine content characteristics, increasing startup time and bandwidth usage.

**How to Test:**
1. Start playback of HLS content to populate the variant list
2. Navigate to the Content tab in Fault Injection
3. Check "Remove CODEC attributes from master playlist"
4. Click "Apply Settings"
5. Restart playback with the same `player_id`
6. Observe increased startup latency and additional network requests

### 2. Reduce Advertised Variants
**Purpose:** Simulate content vendors who don't provide full ABR ladders.

**Impact:** Limited variant selection constrains adaptive bitrate switching, potentially causing suboptimal playback quality or increased buffering.

**How to Test:**
1. Start playback of HLS content to populate the variant list
2. Navigate to the Content tab in Fault Injection
3. Uncheck variants you want to exclude from the master playlist
4. Click "Apply Settings"
5. Restart playback with the same `player_id`
6. Observe that the player only has access to the selected variants

## HLS Workflow

Because content manipulation affects the master playlist which is typically fetched only once at session start:

1. **First Session:** Play content to allow go-proxy to intercept and parse the master playlist
   - This populates the variant list in the Content tab
   - The session data stores `manifest_variants` 

2. **Configure:** Select content limitations
   - Toggle CODEC stripping
   - Select/deselect variants

3. **Second Session:** Replay with the same `player_id`
   - The modified master playlist is served
   - Player operates under the configured constraints

## Backend Implementation

### Go-Proxy Changes
- `shouldApplyContentManipulation()`: Checks if content settings are enabled
- `applyContentManipulation()`: Routes to format-specific handlers
- `manipulateHLSMaster()`: Parses HLS master playlist, applies filters, re-encodes
- `manipulateDASHManifest()`: Placeholder for DASH support (not yet implemented)

### Session Settings
- `content_strip_codecs` (boolean): Strip CODEC attributes
- `content_allowed_variants` (string array): List of allowed variant URLs

### Request Flow
1. Master playlist request arrives at go-proxy
2. Proxy fetches upstream playlist
3. If content manipulation is enabled:
   - Parse playlist using m3u8 library
   - Filter variants if `content_allowed_variants` is set
   - Strip codecs if `content_strip_codecs` is true
   - Re-encode and serve modified playlist
4. Otherwise, serve original playlist

## UI Implementation

### Tab Structure
- New "Content" tab added to Fault Injection panel
- Appears alongside Segment/Manifest/Master/Transport tabs

### Controls
- **Strip CODEC checkbox:** Single toggle for CODEC removal
- **Allowed Variants:** Checkboxes for each variant
  - Dynamically populated from `manifest_variants`
  - Shows resolution and bandwidth
  - Empty state message if no variants available

### Settings Persistence
- Content settings integrate with existing session settings API
- Settings saved via `readSessionSettings()`
- Automatically propagate through session grouping
- Persist across page reloads

## Manual Testing Steps

1. **Build and Start Services:**
   ```bash
   docker-compose up -d
   ```

2. **Access Dashboard:**
   - Open http://localhost:21081/dashboard/testing-session.html
   - Create a new session with a unique player_id (e.g., "test-content-001")

3. **Initial Playback:**
   - Select HLS content from the playlist
   - Start playback
   - Verify master playlist is fetched and variants are populated

4. **Apply Content Limitations:**
   - Open the Content tab in Fault Injection panel
   - Configure desired limitations (CODEC stripping and/or variant filtering)
   - Click "Apply Settings"

5. **Second Playback:**
   - Stop current playback
   - Start playback again with the same player_id
   - Verify modified master playlist is served
   - Use browser DevTools Network tab to inspect master playlist content

6. **Verification:**
   - Check go-proxy logs for content manipulation messages
   - Verify player behavior matches expected constraints
   - Test with different content and variant configurations

## Expected Log Output

```
[GO-PROXY][CONTENT] Applied content manipulation to master playlist session_id=test-content-001
```

## Limitations

- DASH manifest manipulation not yet implemented
- Content tab requires initial playback to populate variant list
- Modifications only apply to master playlist requests (not media playlists or segments)
