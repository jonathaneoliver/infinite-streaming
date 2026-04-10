# InfiniteStream Player - Roku Channel

A Roku channel variant of the iOS InfiniteStream Player for testing HLS and DASH video streams.

## Overview

This Roku channel replicates the functionality of the iOS app, providing:
- Protocol selection (HLS/DASH)
- Segment duration selection (LL/2s/6s)
- Codec filtering (H264/HEVC/AV1)
- Content selection from the InfiniteStream server
- Server environment switching (Dev/Release)
- Auto-play of default content
- Stream URL display

## Features

### Supported Protocols
- **HLS**: HTTP Live Streaming (including Low-Latency HLS)
- **DASH**: Dynamic Adaptive Streaming over HTTP (including Low-Latency DASH)

### Segment Options
- **LL**: Low-Latency variant (200ms partials)
- **2s**: 2-second segments
- **6s**: 6-second segments (default)
- **All**: Any available segment duration

### Codec Support
- **H264**: Default codec, widely supported
- **H265/HEVC**: High Efficiency Video Coding
- **AV1**: AV1 codec (limited support)
- **Auto**: Automatic codec selection

### Server Environments
- **Dev**: Development server ($K3S_HOST:40000)
- **Release**: Production server (infinitestreaming.jeoliver.com:30000)

## Remote Control Navigation

- **OK Button**: Play/Pause playback
- **Left/Right Arrows**: Change protocol settings (HLS/DASH)
- **Up Arrow**: Cycle through server environments
- **Down Arrow**: Cycle through available content
- **Replay/Options**: Restart current playback
- **Back**: Exit the channel

## Directory Structure

```
roku/InfiniteStreamPlayer/
├── manifest                    # Channel manifest file
├── source/
│   └── main.brs               # Entry point
├── components/
│   ├── MainScene.xml          # Main scene UI layout
│   └── MainScene.brs          # Main scene logic
└── images/
    ├── logo_hd.png            # HD menu icon
    ├── logo_sd.png            # SD menu icon
    ├── splash_hd.png          # HD splash screen
    └── splash_sd.png          # SD splash screen
```

## Installation & Development

### Prerequisites
- Roku device or Roku OS simulator
- Roku Developer account
- Network connectivity to InfiniteStream server

### Setup Steps

1. **Enable Developer Mode on Roku Device**
   - Press Home button 3 times, Up 2 times, Right, Left, Right, Left, Right
   - Set a development password when prompted
   - Note the IP address displayed

2. **Configure Network Access**
   - Ensure your Roku device can reach the InfiniteStream server
   - Default Dev server: `$K3S_HOST:40000`
   - Default Release server: `infinitestreaming.jeoliver.com:30000`

3. **Package the Channel**
   ```bash
   cd roku/InfiniteStreamPlayer
   zip -r ../InfiniteStreamPlayer.zip . -x "*.DS_Store" -x "*/.*"
   ```

4. **Install on Roku Device**
   - Open browser to `http://<ROKU_IP>`
   - Login with your development password
   - Upload the ZIP file under "Development Application Installer"
   - Click "Install"

5. **Launch the Channel**
   - The channel will launch automatically after installation
   - Or navigate to "Dev Applications" on your Roku home screen

### Using the Roku Eclipse IDE

If you prefer using the official Roku development tools:

1. Install the [Roku Eclipse IDE](https://developer.roku.com/docs/developer-program/getting-started/developer-setup.md)
2. Import this directory as a Roku project
3. Configure the Roku device IP in the IDE settings
4. Use the IDE's "Export" and "Install" features

## Implementation Details

### Content Discovery
The channel fetches available content from the InfiniteStream API:
```
GET http://<server>:<port>/api/content
```

Response is filtered to:
- Content with `has_hls` or `has_dash` set to `true`
- H264 codec content by default (matching iOS behavior)

### Stream URL Construction
URLs are built following the same pattern as the iOS app:

**HLS:**
- LL/All: `/go-live/{content}/master.m3u8?player_id={id}`
- 2s: `/go-live/{content}/master_2s.m3u8?player_id={id}`
- 6s: `/go-live/{content}/master_6s.m3u8?player_id={id}`

**DASH:**
- LL/All: `/go-live/{content}/manifest.mpd?player_id={id}`
- 2s: `/go-live/{content}/manifest_2s.mpd?player_id={id}`
- 6s: `/go-live/{content}/manifest_6s.mpd?player_id={id}`

### Player ID
Each channel instance generates a unique player ID:
```
roku_{timestamp}
```

This ID is included in stream URLs for server-side session tracking and diagnostics.

## Feature Parity with iOS App

This Roku channel implements the following features from the iOS app:

✅ Server environment selection (Dev/Release)
✅ Protocol selection (HLS/DASH)
✅ Segment duration selection (LL/2s/6s/All)
✅ Codec filtering (H264/HEVC/AV1/Auto)
✅ Content list fetching and filtering
✅ Auto-play of default content on launch
✅ Stream URL construction with player_id
✅ Basic playback controls
✅ Status display

### Differences from iOS App

The Roku implementation differs in the following ways:

1. **UI Layout**: Roku uses SceneGraph XML instead of SwiftUI
   - Simpler, TV-optimized interface
   - Remote control navigation instead of touch
   - Fixed layout for 1080p displays

2. **Diagnostics**: Limited playback diagnostics
   - Roku's video player provides less detailed metrics
   - No equivalent to AVPlayer's detailed observations
   - Basic state information only (playing/paused/buffering)

3. **Player Features**: Roku's native video player
   - Built-in HLS and DASH support
   - No external player libraries needed
   - Limited control over adaptive bitrate selection

4. **Network Requests**: Asynchronous with different patterns
   - BrightScript's roUrlTransfer instead of Swift's URLSession
   - Simpler error handling

5. **Testing Session**: Not implemented
   - iOS app has full testing session integration
   - Roku version focuses on core playback functionality

## Limitations

1. **No Testing Session Integration**
   - No failure injection controls
   - No SSE event streaming
   - No session grouping

2. **Limited Diagnostics**
   - No detailed buffer metrics
   - No bitrate tracking
   - Basic player state only

3. **Simplified UI**
   - No playback diagnostics grid
   - Minimal status information
   - Remote control only (no touch/mouse)

4. **Image Assets Required**
   - Channel requires logo and splash screen images
   - Must be created manually (see images/README.md)

## Engineering Notes

This is an **engineering-focused** implementation designed for:
- Internal testing and validation
- Protocol comparison on Roku platform
- Streaming pipeline verification
- Player behavior observation

It is **not intended** for:
- Public distribution via Roku Channel Store
- End-user consumption
- Production deployments

## Troubleshooting

### Channel won't install
- Verify developer mode is enabled
- Check ZIP file is properly formatted
- Ensure manifest file is valid

### Video won't play
- Verify server is reachable from Roku device
- Check network connectivity
- Ensure selected content supports chosen protocol
- Review Roku debug console (telnet to port 8085)

### Content list is empty
- Verify API endpoint is accessible
- Check server is running
- Ensure `/api/content` returns valid JSON
- Confirm H264 content is available

### Debug Console Access
```bash
telnet <ROKU_IP> 8085
```

This provides real-time console output for debugging.

## References

- [Roku Developer Documentation](https://developer.roku.com/docs/developer-program/getting-started/roku-dev-prog.md)
- [BrightScript Language Reference](https://developer.roku.com/docs/references/brightscript/language/brightscript-language-reference.md)
- [SceneGraph XML Reference](https://developer.roku.com/docs/references/scenegraph/xml-elements/xml-elements-overview.md)
- iOS app implementation: `apple/InfiniteStreamPlayer/`
- Product requirements: `PRD.md`

## License

Same license as the main InfiniteStream project. See LICENSE file in the repository root.
