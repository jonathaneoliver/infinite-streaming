# Content Tab UI Visual Documentation

## Tab Layout

```
┌─────────────────────────────────────────────────────────────────┐
│ Fault Injection                                                  │
├─────────────────────────────────────────────────────────────────┤
│  [Segment] [Manifest] [Master] [Transport] [Content] ←          │
│                                                  └─ New Tab      │
├─────────────────────────────────────────────────────────────────┤
│                                                                   │
│  Strip CODEC Information                                         │
│  ┌─────────────────────────────────────────────────┐            │
│  │ ☑ Remove CODEC attributes from master playlist  │            │
│  └─────────────────────────────────────────────────┘            │
│                                                                   │
│  Allowed Variants                                                │
│  ┌─────────────────────────────────────────────────┐            │
│  │ ☑ 2160p / 15000 kbps                            │            │
│  │ ☑ 1080p / 6000 kbps                             │            │
│  │ ☑ 720p / 3000 kbps                              │            │
│  │ ☐ 480p / 1500 kbps                              │            │
│  │ ☐ 360p / 800 kbps                               │            │
│  └─────────────────────────────────────────────────┘            │
│                                                                   │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ ℹ️ Note: Content modifications apply to master playlist   │  │
│  │ requests. For HLS, play content once to populate variant  │  │
│  │ list, configure settings, then replay to apply changes.   │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                   │
└─────────────────────────────────────────────────────────────────┘
```

## UI Components

### 1. Tab Button
- **Location:** Added as 5th tab in Fault Injection panel
- **Label:** "Content"
- **State:** Active when selected (blue underline)

### 2. Strip CODEC Information Control
- **Type:** Single checkbox
- **Label:** "Remove CODEC attributes from master playlist"
- **Field:** `content_strip_codecs`
- **Default:** Unchecked

### 3. Allowed Variants Control
- **Type:** Checkbox list (multiple selection)
- **Items:** Dynamically populated from `manifest_variants`
- **Format:** `{resolution} / {bandwidth} kbps`
- **Field:** `content_allowed_variants`
- **Default:** All checked (empty array = all allowed)
- **Empty State:** Shows message "Play content once to populate variant list"

### 4. Information Note
- **Type:** Static informational banner
- **Style:** Light blue background with blue border
- **Purpose:** Explains the HLS workflow requirement

## Interaction Flow

### Initial State (No Content Played)
```
Content Tab
├── Strip CODEC: [ ] Unchecked
└── Allowed Variants: "Play content once to populate variant list"
```

### After First Playback
```
Content Tab
├── Strip CODEC: [ ] Unchecked
└── Allowed Variants: 
    ├── [x] 1080p / 6000 kbps
    ├── [x] 720p / 3000 kbps
    └── [x] 480p / 1500 kbps
```

### User Configures Limitations
```
Content Tab
├── Strip CODEC: [x] Checked
└── Allowed Variants: 
    ├── [x] 1080p / 6000 kbps
    └── [ ] 720p / 3000 kbps  ← Unchecked
        [ ] 480p / 1500 kbps  ← Unchecked
```

### Settings Applied to Session
```json
{
  "content_strip_codecs": true,
  "content_allowed_variants": [
    "v1/1080p.m3u8"
  ]
}
```

## Responsive Behavior

- Tab fits alongside existing tabs without overflow
- Checkbox list scrolls if many variants exist
- Note banner wraps text on narrow screens
- Maintains consistency with existing Fault Injection tabs

## Color Scheme

- **Tab (Active):** Blue (#0066cc) underline, white background
- **Tab (Inactive):** Gray (#666) text, light gray background
- **Checkboxes:** Standard browser checkbox styling
- **Note Banner:** Light blue background (#f0f7ff), blue border (#b3d9ff)
- **Labels:** Dark gray (#333) text

## Integration Points

### JavaScript
- `renderContentVariantOptions()` - Generates variant checkboxes
- `getBool()` - Reads checkbox state
- `getStringSlice()` - Reads selected variants
- `readSessionSettings()` - Includes content settings in payload

### Backend
- `content_strip_codecs` - Boolean session field
- `content_allowed_variants` - String array session field
- `shouldApplyContentManipulation()` - Checks if settings enabled
- `manipulateHLSMaster()` - Applies modifications

### CSS
- `.content-tab-note` - Note banner styling
- `.no-variants-message` - Empty state message
- Inherits `.tab-button`, `.tab-panel`, `.fault-control-row` from existing styles
