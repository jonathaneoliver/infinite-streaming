# UI Layout Description

This document describes the visual layout of the InfiniteStream Android Player app since actual screenshots require a running Android device/emulator.

## Screen Layout (Landscape Mode)

```
┌─────────────────────────────────────────────────────────────────────┐
│ InfiniteStream Player                                              │
│                                                                     │
│ [Retry Fetch]  [Restart Playback]                                 │
│                                                                     │
│ ┌─────────────────────────────────────────────────────────────┐   │
│ │                                                             │   │
│ │                                                             │   │
│ │                   Video Player (16:9)                       │   │
│ │                                                             │   │
│ │                    ExoPlayer Controls                       │   │
│ │                  [Play/Pause] [Seek Bar]                    │   │
│ │                                                             │   │
│ └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│ Server:    [Dev (40081)              ▼]                           │
│                                                                     │
│ Protocol:  [HLS                       ▼]                           │
│                                                                     │
│ Segment:   [6s                        ▼]                           │
│                                                                     │
│ Codec:     [H.264                     ▼]                           │
│                                                                     │
│ Content:   [bbb                       ▼]                           │
│                                                                     │
│ Status: Ready - http://localhost:40081/go-live/bbb/master...        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## Component Breakdown

### Header Section
- **Title**: "InfiniteStream Player" in large, bold white text
- **Background**: Black

### Control Buttons
- **Retry Fetch**: Material Design button, primary color
- **Restart Playback**: Material Design button, primary color
- Layout: Horizontal, side-by-side with 8dp spacing

### Video Player
- **Dimensions**: Full width, 16:9 aspect ratio
- **Background**: Black when no video
- **Controls**: ExoPlayer built-in controls overlay
  - Play/Pause button (center)
  - Seek bar (bottom)
  - Timestamp (current/duration)
  - Fullscreen toggle
  - Volume control

### Configuration Spinners

Each spinner follows the same pattern:
- **Label**: White text, 14sp, left-aligned
- **Spinner**: Material Design dropdown, full width after label
- **Background**: Semi-transparent white
- **Spacing**: 12dp between each spinner

**Server Options**:
- Dev (40081)
- Release (30081)

**Protocol Options**:
- HLS
- DASH

**Segment Options**:
- LL (Low Latency)
- 2s
- 6s

**Codec Options**:
- H.264
- H.265

**Content Options**:
- bbb (Big Buck Bunny)
- counter-10m
- counter-1h
- sintel

### Status Bar
- **Position**: Bottom of screen
- **Text**: Gray text (12sp)
- **Content**: Shows playback state and current URL
- **Examples**:
  - "Ready - http://..."
  - "Buffering..."
  - "Ready - Playing"
  - "Error: Network timeout"

## Color Scheme

```
Background:        #000000 (Black)
Title Text:        #FFFFFF (White)
Label Text:        #FFFFFF (White)
Status Text:       #CCCCCC (Light Gray)
Primary Accent:    #2196F3 (Material Blue)
Primary Dark:      #1976D2 (Dark Blue)
Secondary Accent:  #FF4081 (Material Pink)
```

## Spacing & Dimensions

- **Padding**: 16dp around main container
- **Title Top Margin**: 0dp
- **Buttons Top Margin**: 12dp
- **Player Top Margin**: 12dp
- **First Spinner Top Margin**: 16dp
- **Spinner Spacing**: 12dp between each
- **Status Top Margin**: 16dp

## Typography

- **Title**: 24sp, Bold
- **Labels**: 14sp, Regular
- **Status**: 12sp, Regular
- **Button Text**: 14sp, Medium

## Interaction States

### Spinners
- **Default**: Semi-transparent background
- **Pressed**: Ripple effect
- **Open**: Full dropdown list with scroll if needed

### Buttons
- **Default**: Primary color background
- **Pressed**: Darker shade with ripple
- **Disabled**: Gray (not used in this app)

### Player
- **Loading**: Shows buffering spinner
- **Error**: Shows error icon overlay
- **Playing**: Controls auto-hide after 3 seconds
- **Paused**: Controls remain visible

## Landscape vs Portrait

The app is **locked to landscape** for optimal video viewing:
- Manifest specifies: `android:screenOrientation="landscape"`
- This prevents awkward portrait mode with small player
- Maximizes video viewing area

## Accessibility

- All interactive elements have minimum 48dp touch target
- Spinner labels provide context for screen readers
- Status text provides playback feedback
- Material Design components ensure good contrast ratios

## Comparison with iOS App

The Android layout mirrors the iOS Swift app with these differences:

| Element | iOS | Android |
|---------|-----|---------|
| Layout | SwiftUI ScrollView | ConstraintLayout in ScrollView |
| Buttons | SwiftUI Button | Material Button |
| Dropdowns | SwiftUI Picker | Material Spinner |
| Player | AVPlayerViewController | ExoPlayer PlayerView |
| Typography | SF Pro | Roboto |
| Colors | iOS blue | Material blue |

Despite these platform differences, the **visual appearance and functionality are nearly identical**.

## Screenshots

To generate actual screenshots:
1. Build and run the app on an Android device or emulator
2. Use Android Studio's screenshot tool:
   - `View > Tool Windows > Logcat`
   - Click the camera icon in the toolbar
3. Or use ADB:
   ```bash
   adb shell screencap -p /sdcard/screenshot.png
   adb pull /sdcard/screenshot.png
   ```

Recommended screenshots to capture:
- [ ] Main screen with default configuration
- [ ] Video playing (HLS)
- [ ] Video playing (DASH)
- [ ] Dropdown menu expanded
- [ ] Error state with toast message
- [ ] Buffering state
