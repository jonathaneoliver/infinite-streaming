# Roku Channel Testing Guide

This guide provides step-by-step instructions for testing the InfiniteStream Player Roku channel.

## Prerequisites

1. **Roku Device or Simulator**
   - Physical Roku device (Roku TV, Streaming Stick, Express, Ultra, etc.)
   - OR Roku OS simulator (available with Roku Developer SDK)

2. **Network Setup**
   - Roku device and InfiniteStream server on same network (or server accessible via public IP)
   - Default server addresses:
     - Dev: `100.111.190.54:40000`
     - Release: `infinitestreaming.jeoliver.com:30000`

3. **Developer Account**
   - Free Roku developer account (sign up at https://developer.roku.com)

## Step 1: Enable Developer Mode on Roku Device

1. On your Roku home screen, press the following sequence on your remote:
   ```
   Home (3 times) → Up (2 times) → Right → Left → Right → Left → Right
   ```

2. The "Developer Settings" screen will appear

3. Select "Enable Installer" and confirm

4. Set a development password when prompted (remember this password!)

5. Note the IP address displayed (e.g., `192.168.1.100`)

6. The device will reboot

## Step 2: Package the Channel

From the repository root:

```bash
cd roku/InfiniteStreamPlayer
./package.sh
```

This creates `roku/InfiniteStreamPlayer.zip` containing:
- manifest
- source/main.brs
- components/MainScene.xml
- components/MainScene.brs
- images/*.png

## Step 3: Install the Channel

1. **Open the Roku Developer Web Interface**
   ```
   http://<ROKU_IP>
   ```
   Example: `http://192.168.1.100`

2. **Login**
   - Username: `rokudev` (default)
   - Password: (the password you set in Step 1)

3. **Upload the Package**
   - Scroll to "Development Application Installer"
   - Click "Browse" and select `InfiniteStreamPlayer.zip`
   - Click "Install"

4. **Wait for Installation**
   - The channel will compile and install (this may take 30-60 seconds)
   - Once complete, the channel will automatically launch

## Step 4: Test Basic Functionality

### On Launch
- [ ] Channel displays "InfiniteStream Player" title
- [ ] Status shows "Initializing..."
- [ ] Content list is fetched automatically
- [ ] Status updates to "Loaded X items"
- [ ] Default content is selected
- [ ] Video begins auto-playing

### Server Environment
- [ ] Press **UP** arrow to cycle server environments
- [ ] Display updates: "Dev (40000)" → "Release (30000)" → ...
- [ ] Content list refreshes
- [ ] Video restarts with new server

### Protocol Selection
- [ ] Press **LEFT/RIGHT** arrows to cycle protocols
- [ ] Display updates: "HLS" → "DASH" → ...
- [ ] Video restarts with new protocol
- [ ] Verify both HLS and DASH playback work

### Content Selection
- [ ] Press **DOWN** arrow to cycle through content
- [ ] Display updates with content name
- [ ] Video restarts with new content
- [ ] URL display updates

### Playback Controls
- [ ] Press **OK** to pause
- [ ] Status shows "Paused"
- [ ] Press **OK** again to resume
- [ ] Status shows "Playing"
- [ ] Press **Replay** or **Options** to restart
- [ ] Status shows "Restarting playback..."

## Step 5: Verify Stream URLs

Check that stream URLs follow the expected pattern:

### HLS URLs
- LL/All: `http://<host>:<port>/go-live/<content>/master.m3u8?player_id=roku_<timestamp>`
- 2s: `http://<host>:<port>/go-live/<content>/master_2s.m3u8?player_id=roku_<timestamp>`
- 6s: `http://<host>:<port>/go-live/<content>/master_6s.m3u8?player_id=roku_<timestamp>`

### DASH URLs
- LL/All: `http://<host>:<port>/go-live/<content>/manifest.mpd?player_id=roku_<timestamp>`
- 2s: `http://<host>:<port>/go-live/<content>/manifest_2s.mpd?player_id=roku_<timestamp>`
- 6s: `http://<host>:<port>/go-live/<content>/manifest_6s.mpd?player_id=roku_<timestamp>`

URLs are displayed on screen below the content name.

## Step 6: Test Error Handling

### No Network Connection
1. Disconnect Roku from network
2. Launch channel
3. **Expected:** Status shows "Request timeout" or "Failed to start request"

### Invalid Server
1. Edit `MainScene.brs` to use invalid host
2. Repackage and reinstall
3. **Expected:** Status shows "Failed to fetch content" or timeout

### Unsupported Content
1. Select DASH protocol
2. Choose content that only has HLS
3. **Expected:** Status shows "Content does not support DASH"

## Step 7: Debug Console Access

For detailed debugging information:

```bash
telnet <ROKU_IP> 8085
```

This provides real-time console output including:
- BrightScript print statements
- Runtime errors
- Network request details
- Video player events

## Step 8: Compare with iOS App

Test the same content on both platforms:

### iOS App
1. Launch InfiniteStreamPlayer on iOS/tvOS
2. Select same server, protocol, segment, and content
3. Note the stream URL

### Roku Channel
1. Configure same settings on Roku
2. Note the stream URL

### Verify
- [ ] URLs match (except player_id prefix)
- [ ] Both platforms play same content
- [ ] Video quality and behavior similar
- [ ] Segment selection works identically

## Common Issues and Solutions

### Channel Won't Install
- **Issue:** Upload fails or hangs
- **Solution:** 
  - Verify manifest file is valid
  - Check ZIP file isn't corrupted
  - Try disabling/re-enabling developer mode
  - Reboot Roku device

### Video Won't Play
- **Issue:** Black screen or buffering forever
- **Solution:**
  - Verify server is reachable from Roku
  - Check firewall rules
  - Test stream URL in browser/curl
  - Review debug console for errors
  - Verify content supports selected protocol

### Content List Empty
- **Issue:** "No content available" or "No compatible content found"
- **Solution:**
  - Verify `/api/content` endpoint returns data
  - Check server is running
  - Ensure H264 content exists
  - Review content filtering logic

### App Crashes on Launch
- **Issue:** Channel exits immediately
- **Solution:**
  - Check debug console for errors
  - Verify all required files are in ZIP
  - Ensure images exist and are valid PNGs
  - Review manifest for syntax errors

### Remote Not Responding
- **Issue:** Button presses don't work
- **Solution:**
  - Verify channel has focus
  - Check `onKeyEvent` handler is registered
  - Review debug console for key events
  - Try restarting channel

## Performance Testing

### Startup Time
- Measure time from launch to first frame
- **Target:** < 5 seconds on typical content

### Content Switch Time
- Measure time between content changes
- **Target:** < 3 seconds

### Buffering
- Monitor for buffering events during playback
- **Target:** Minimal buffering on stable network

### Memory Usage
- Monitor memory via debug console
- **Target:** Stable (no leaks)

## Automated Testing Notes

Roku SDK provides testing tools:

### Roku Automated Channel Testing (RACT)
- Python-based test automation
- Requires external control protocol (ECP)
- Can simulate remote button presses

### Example Test Script
```python
import requests

ROKU_IP = "192.168.1.100"

def press_button(button):
    url = f"http://{ROKU_IP}:8060/keypress/{button}"
    requests.post(url)

def launch_channel(channel_id="dev"):
    url = f"http://{ROKU_IP}:8060/launch/{channel_id}"
    requests.post(url)

# Launch dev channel
launch_channel()
time.sleep(3)

# Simulate navigation
press_button("Down")  # Next content
time.sleep(1)
press_button("Select")  # Play/Pause
```

## Success Criteria

The channel is functioning correctly if:

- ✅ Channel installs without errors
- ✅ UI displays correctly on 1080p screen
- ✅ Content list fetches and displays
- ✅ Auto-play works on launch
- ✅ HLS streams play smoothly
- ✅ DASH streams play smoothly (if supported by content)
- ✅ Remote control navigation works
- ✅ Server switching updates content and playback
- ✅ Stream URLs match iOS app pattern
- ✅ Player ID is generated and included in URLs
- ✅ Basic error handling works (no crashes)

## Next Steps

After successful testing:

1. **Document any issues** found
2. **Compare behavior** with iOS app
3. **Test on multiple Roku models** if available
4. **Validate with production server**
5. **Consider additional features** (if needed)

## Additional Resources

- [Roku Developer Documentation](https://developer.roku.com/docs/developer-program/getting-started/roku-dev-prog.md)
- [BrightScript Language Reference](https://developer.roku.com/docs/references/brightscript/language/brightscript-language-reference.md)
- [SceneGraph XML Reference](https://developer.roku.com/docs/references/scenegraph/xml-elements/xml-elements-overview.md)
- [Roku External Control Protocol (ECP)](https://developer.roku.com/docs/developer-program/debugging/external-control-api.md)
