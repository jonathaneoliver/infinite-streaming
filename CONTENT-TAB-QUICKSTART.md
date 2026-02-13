# Content Tab - Quick Reference

## 🎯 What It Does
Modifies HLS master playlists on-the-fly to test player behavior under content constraints.

## 🚀 Quick Start

### Step 1: Initial Playback
1. Open testing dashboard
2. Create session (e.g., player_id: `test-001`)
3. Play HLS content
4. Wait for master playlist to load

### Step 2: Configure
1. Open **Fault Injection** → **Content** tab
2. Choose options:
   - ☑ **Strip CODEC** - Remove codec attributes
   - ☐ **Uncheck variants** - Hide quality levels
3. Click **Apply Settings**

### Step 3: Test
1. Stop playback
2. Replay with same player_id
3. Modified playlist is now active!

## 📋 Features

### Strip CODEC Information
```
Before: CODEC="avc1.640028,mp4a.40.2"
After:  CODEC=""
```
**Tests:** Player startup behavior without codec info

### Reduce Variants
```
Before: 4K, 1080p, 720p, 480p, 360p
After:  1080p, 720p  (others hidden)
```
**Tests:** Player adaptation with limited quality ladder

## 🔍 What to Look For

### With CODEC Stripping
- ⏱️ Longer startup time
- 📡 Extra segment downloads during init
- 🔧 Player unable to use "chunkless prepare"

### With Variant Filtering
- 📊 Limited quality switching
- 🎥 Different initial quality selection
- ⚠️ Possible buffering if constrained

## 💡 Tips

- **First playback populates variant list** - Required!
- **Same player_id required** - Use consistent naming
- **Check browser DevTools Network tab** - Inspect master.m3u8
- **Go-proxy logs show manipulation** - Look for `[CONTENT]` prefix
- **Works with session grouping** - Settings propagate to group

## 🐛 Troubleshooting

**Q: Variant list is empty**
- A: Play content once first to populate the list

**Q: Changes not applying**
- A: Ensure you're using the same player_id
- A: Check that content_strip_codecs or content_allowed_variants is set

**Q: Works in first session but not second**
- A: Restart with exact same player_id
- A: Clear browser cache if needed

## 📝 Example Session

```javascript
// Session after configuration
{
  "player_id": "test-001",
  "content_strip_codecs": true,
  "content_allowed_variants": [
    "v1/1080p.m3u8",
    "v1/720p.m3u8"
  ]
}
```

## 🎓 Use Cases

| Scenario | Configuration | Expected Impact |
|----------|---------------|-----------------|
| **Missing Codec Info** | Strip CODEC ☑ | Slower startup, extra downloads |
| **Budget Streaming** | Only 720p, 480p | Limited quality, more buffering |
| **Premium Only** | Only 4K, 1080p | Better quality, more bandwidth |
| **Extreme Constraint** | Single variant | No adaptation possible |

## 📖 See Also

- **CONTENT-TAB-TESTING.md** - Detailed testing procedures
- **CONTENT-TAB-UI.md** - UI component documentation
- **CONTENT-TAB-SUMMARY.md** - Technical architecture

## ⚠️ Limitations

- ❌ **DASH not supported** - HLS only (for now)
- ℹ️ **Master playlist only** - Doesn't affect segments
- 🔄 **Requires replay** - Changes apply to next session

## 🎉 Success!

If you see this log message, it's working:
```
[GO-PROXY][CONTENT] Applied content manipulation to master playlist session_id=test-001
```
