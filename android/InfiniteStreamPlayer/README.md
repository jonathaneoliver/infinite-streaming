# InfiniteStream Player for Android

A minimal, one-page ExoPlayer-based Android application for testing HLS and DASH streams from the InfiniteStream server. This app mirrors the design and functionality of the iOS/tvOS Swift app.

## Features

- **ExoPlayer Integration**: Uses Google's ExoPlayer (Media3) for robust HLS and DASH playback
- **Clean Single-Page UI**: Minimal interface with essential playback controls
- **Server Environment Switching**: Toggle between Dev and Release environments
- **Protocol Selection**: Choose between HLS and DASH
- **Segment Duration Options**: LL (Low Latency), 2s, or 6s segments
- **Codec Selection**: H.264 or H.265 (HEVC)
- **Content Selection**: Quick access to test content (BBB, Sintel, counters)
- **Playback Controls**: Retry fetch and restart playback functionality

## Requirements

- **Android Studio**: Arctic Fox (2020.3.1) or newer
- **Minimum SDK**: API 24 (Android 7.0)
- **Target SDK**: API 34 (Android 14)
- **Java**: 8 or higher

## Build Instructions

### Option 1: Android Studio (Recommended)

1. **Open the project**:
   ```bash
   cd android/InfiniteStreamPlayer
   ```
   Then open this directory in Android Studio.

2. **Sync Gradle**: Android Studio will automatically sync Gradle dependencies.

3. **Build the app**:
   - Select `Build > Make Project` from the menu
   - Or use the keyboard shortcut: `Cmd+F9` (Mac) / `Ctrl+F9` (Windows/Linux)

4. **Run on device/emulator**:
   - Connect an Android device via USB (with USB debugging enabled)
   - Or start an Android emulator
   - Click the "Run" button or press `Shift+F10`

### Option 2: Command Line

1. **Navigate to the project directory**:
   ```bash
   cd android/InfiniteStreamPlayer
   ```

2. **Build the APK**:
   ```bash
   ./gradlew assembleDebug
   ```
   The APK will be generated at: `app/build/outputs/apk/debug/app-debug.apk`

3. **Install on a connected device**:
   ```bash
   ./gradlew installDebug
   ```

4. **Build and install in one step**:
   ```bash
   ./gradlew installDebug
   ```

### Gradle Wrapper Setup

If you don't have the Gradle wrapper files, generate them:

```bash
gradle wrapper --gradle-version 8.2
```

## Testing the App

### Quick Start

1. **Launch the app** on your Android device or emulator
2. The app opens in landscape mode with the player ready
3. **Select your configuration**:
   - Server: Dev (40081) or Release (30081)
   - Protocol: HLS or DASH
   - Segment: LL, 2s, or 6s
   - Codec: H.264 or H.265
   - Content: Choose from available test videos
4. The stream will automatically load and start playing

### Controls

- **Retry Fetch**: Reloads the current stream without stopping the player
- **Restart Playback**: Stops the player, clears media, and reloads the stream
- **Player Controls**: Use the built-in ExoPlayer controls for play/pause, seek, and volume

### Testing Different Configurations

The app automatically rebuilds the stream URL when you change any selection:

- **Test LL-HLS**: Select HLS protocol + LL segment
- **Test DASH with 6s segments**: Select DASH protocol + 6s segment
- **Test H.265 encoding**: Select H.265 codec (available for supported content)
- **Switch servers**: Toggle between Dev and Release environments

### Network Requirements

- The app requires an active internet connection
- Cleartext traffic is enabled for testing (HTTP URLs)
- Ensure your device can reach the InfiniteStream server:
  - Dev: `http://100.111.190.54:40081`
  - Release: `http://infinitestreaming.jeoliver.com:30081`

### Troubleshooting

**Build Errors**:
- Ensure you have the latest Android SDK platform tools
- Sync Gradle files: `File > Sync Project with Gradle Files`
- Clean and rebuild: `Build > Clean Project` then `Build > Rebuild Project`

**Playback Issues**:
- Check the status text at the bottom of the screen for error messages
- Verify network connectivity to the InfiniteStream server
- Try the "Retry Fetch" button to reload the stream
- Use "Restart Playback" to reset the player completely

**Performance**:
- LL-HLS streams require a stable network connection
- For testing on slower networks, try 2s or 6s segments
- Emulators may have limited performance; physical devices are recommended

## Architecture

The app follows Android best practices with a single Activity architecture:

- **MainActivity.java**: Main activity with ExoPlayer integration and UI logic
- **activity_main.xml**: Layout with player view and control spinners
- **ExoPlayer (Media3)**: Modern Android media player with HLS/DASH support

### Dependencies

```gradle
// ExoPlayer (Media3)
androidx.media3:media3-exoplayer:1.2.1
androidx.media3:media3-exoplayer-dash:1.2.1
androidx.media3:media3-exoplayer-hls:1.2.1
androidx.media3:media3-ui:1.2.1
```

## Comparison with Swift App

This Android app mirrors the iOS/tvOS Swift app functionality:

| Feature | iOS (Swift) | Android (ExoPlayer) |
|---------|-------------|---------------------|
| Single-page UI | ✓ | ✓ |
| Server selection | ✓ | ✓ |
| Protocol selection | ✓ | ✓ |
| Segment options | ✓ | ✓ |
| Codec selection | ✓ | ✓ |
| Content selection | ✓ | ✓ |
| Playback controls | ✓ | ✓ |
| Minimal dependencies | ✓ | ✓ |

## License

See the main repository LICENSE file for details.

## Related Documentation

- [InfiniteStream Main README](../../README.md)
- [iOS/tvOS App](../../apple/InfiniteStreamPlayer/)
- [Roku Channel](../../roku/InfiniteStreamPlayer/)
