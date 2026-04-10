# Testing Guide for InfiniteStream Android Player

This guide provides step-by-step instructions for testing the Android ExoPlayer app.

## Prerequisites

Before testing, ensure:
- Android Studio is installed (Arctic Fox or newer)
- Android SDK is set up with API 24+ and API 34
- Either a physical Android device with USB debugging enabled, or an Android emulator configured

## Quick Test (5 minutes)

### 1. Build the App

```bash
cd android/InfiniteStreamPlayer
./gradlew assembleDebug
```

Expected output: `BUILD SUCCESSFUL`

### 2. Install on Device

```bash
# Connect device via USB, then:
./gradlew installDebug
```

Or using Android Studio:
- Open the project in Android Studio
- Click the "Run" button (green triangle)
- Select your device/emulator

### 3. Launch and Verify UI

The app should launch in landscape mode with:
- ✅ Title: "InfiniteStream Player"
- ✅ Two buttons: "Retry Fetch" and "Restart Playback"
- ✅ Black video player area (16:9 aspect ratio)
- ✅ Five dropdown spinners (Server, Protocol, Segment, Codec, Content)
- ✅ Status text at the bottom

## Feature Testing

### Test 1: Basic HLS Playback

1. Launch the app
2. Verify default selections:
   - Server: Dev (40081)
   - Protocol: HLS
   - Segment: 6s
   - Codec: H.264
   - Content: bbb (Big Buck Bunny)
3. The video should automatically load and start playing
4. Verify playback controls work (play, pause, seek)
5. **Expected**: Smooth HLS playback with built-in controls

### Test 2: DASH Playback

1. Change Protocol to "DASH"
2. Video should automatically reload with DASH manifest
3. Verify playback continues smoothly
4. **Expected**: Seamless switch to DASH protocol

### Test 3: Low-Latency Playback

1. Change Segment to "LL"
2. Video should reload with low-latency manifest
3. **Expected**: LL-HLS or LL-DASH playback (lower latency)

### Test 4: Server Switching

1. Change Server to "Release (30081)"
2. Video should reload from the release server
3. **Expected**: Playback from `infinitestreaming.jeoliver.com:30081`

### Test 5: Codec Selection

1. Change Codec to "H.265"
2. Video should reload with H.265 encoded variant
3. **Expected**: HEVC playback (if device supports H.265)

### Test 6: Content Selection

1. Change Content to different videos:
   - `counter-10m`: 10-minute counter video
   - `sintel`: Sintel short film
   - `counter-1h`: 1-hour counter video
2. **Expected**: Each video loads and plays correctly

### Test 7: Retry Fetch

1. While video is playing, tap "Retry Fetch"
2. **Expected**: Video reloads from current position without full restart

### Test 8: Restart Playback

1. While video is playing, tap "Restart Playback"
2. **Expected**: Player stops, clears, and reloads from beginning

### Test 9: Error Handling

1. Disconnect from network or use airplane mode
2. Try changing content or protocol
3. **Expected**: Toast message with error, status text shows error details

### Test 10: Configuration Persistence

1. Select specific configuration (e.g., DASH + LL + H.265)
2. Rotate device or minimize app
3. Return to app
4. **Expected**: Configuration persists, video resumes

## Network Testing

### Local Network Access

Test connectivity to both servers:

```bash
# Dev server
curl -I http://$K3S_HOST:40081/go-live/bbb/master.m3u8

# Release server
curl -I http://infinitestreaming.jeoliver.com:30081/go-live/bbb/master.m3u8
```

Both should return `200 OK` with `Content-Type: application/x-mpegURL` or `application/vnd.apple.mpegurl`

### Firewall/Network Issues

If playback fails:
1. Check device can access the server IPs
2. Verify no corporate firewall blocking HTTP traffic
3. Try using mobile data instead of WiFi
4. Check status text for specific error messages

## Performance Testing

### Recommended Configurations for Different Networks

**Fast WiFi (>10 Mbps)**:
- Protocol: HLS or DASH
- Segment: LL
- Codec: H.265 (if supported)

**Moderate WiFi (5-10 Mbps)**:
- Protocol: HLS or DASH
- Segment: 2s
- Codec: H.264

**Slow/Mobile Network (<5 Mbps)**:
- Protocol: HLS
- Segment: 6s
- Codec: H.264

## Expected Behavior Summary

| Action | Expected Result |
|--------|----------------|
| App Launch | Opens in landscape, video auto-loads |
| Change Protocol | Video reloads with new manifest type |
| Change Segment | Video reloads with new segment duration |
| Change Codec | Video reloads with new codec variant |
| Change Content | New video loads and plays |
| Change Server | Video reloads from different server |
| Retry Fetch | Reloads current stream |
| Restart Playback | Stops and restarts from beginning |
| Network Error | Toast + status text shows error |
| Device Rotation | Video continues playing |

## Troubleshooting

### Build Fails

**Error: SDK not found**
```bash
# Set ANDROID_HOME environment variable
export ANDROID_HOME=$HOME/Android/Sdk  # or your SDK location
```

**Error: Gradle sync failed**
- In Android Studio: `File > Invalidate Caches / Restart`
- Or: `./gradlew clean build`

### App Crashes on Launch

**Check Logcat** in Android Studio:
```bash
# Or via command line:
adb logcat | grep -i "infinitestream"
```

Common issues:
- Missing ExoPlayer dependencies (should be auto-downloaded)
- Min SDK too low (need API 24+)

### Video Won't Play

1. **Check network connectivity**: Can you ping the server?
2. **Check URL in status text**: Is it correct?
3. **Check ExoPlayer logs**: Look for codec support issues
4. **Try different content**: Some codecs may not be supported on all devices

### H.265 Not Working

- Not all Android devices support H.265/HEVC
- Try H.264 instead
- Check device codec capabilities in Settings

## Validation Checklist

Use this checklist to confirm the app meets requirements:

- [ ] App builds successfully from command line
- [ ] App builds successfully in Android Studio
- [ ] App launches in landscape mode
- [ ] Video player view is visible and sized correctly
- [ ] All 5 spinners are present and functional
- [ ] Both buttons (Retry Fetch, Restart Playback) work
- [ ] Status text updates with playback state
- [ ] HLS playback works
- [ ] DASH playback works
- [ ] LL segments work
- [ ] 2s segments work
- [ ] 6s segments work
- [ ] H.264 codec works
- [ ] H.265 codec works (if device supports)
- [ ] Server switching works (Dev/Release)
- [ ] Content switching works
- [ ] Error handling shows user-friendly messages
- [ ] App handles device rotation
- [ ] App cleans up resources on exit

## Success Criteria

The test is successful if:
1. ✅ App builds without errors
2. ✅ App launches and displays the UI correctly
3. ✅ At least one video plays successfully (HLS with H.264)
4. ✅ User can change configurations and reload videos
5. ✅ Error messages are clear and actionable
6. ✅ No crashes during normal operation

## Reporting Issues

If you encounter issues, please report:
- Android version and device model
- Selected configuration (server, protocol, segment, codec, content)
- Exact error message from status text and/or toast
- Logcat output if available
- Network environment (WiFi, mobile, corporate network, etc.)
