package com.infinitestream.player.state

/** One server known to the app — a row in SharedPreferences. */
data class ServerEnvironment(
    val name: String,
    val host: String,
    val port: String,    // playback (HLS/DASH) port
    val apiPort: String, // REST API port
    val scheme: String = "https",
) {
    val baseUrl: String get() = "$scheme://$host:$port"
    val apiUrl: String get() = "$scheme://$host:$apiPort"
}

/** One content item discovered from /api/content. */
data class ContentItem(
    val name: String,
    val hasHls: Boolean,
    val hasDash: Boolean,
    /** Server-computed logical clip identifier — same value across the
     *  h264/hevc/av1 encodings of one clip. Browse rows dedupe by this. */
    val clipId: String,
    /** "h264" / "hevc" / "av1" / "" — server-stripped codec hint. */
    val codec: String,
    /** Native segment duration the source was encoded at (seconds). null
     *  when the server couldn't detect it. Kept for display only — for
     *  "can I play this at length N?" use [supportsSegment], which prefers
     *  [segmentDurations]. */
    val segmentDuration: Int? = null,
    /** All segment lengths (seconds) go-live can actually serve this clip
     *  at — go-live synthesizes both a 2s and a 6s master from any HLS
     *  source, so this is `[2, 6]` for HLS content regardless of the
     *  native encode. null from a pre-#685 server (falls back to the
     *  legacy single-[segmentDuration] match). */
    val segmentDurations: List<Int>? = null,
    /** Whether the LL (low-latency) variant is available — go-live can
     *  only build it when the source carries on-disk partial-segment info.
     *  null from a pre-#685 server (treated as available). */
    val hasLL: Boolean? = null,
    /** Server-relative path to the 640-px-wide poster (default size for
     *  card / tile surfaces). Null when the server hasn't generated a
     *  thumbnail for this clip yet. */
    val thumbnailPath: String? = null,
    /** Server-relative path to the 320-px-wide poster, for small list
     *  cells / mobile rows. */
    val thumbnailPathSmall: String? = null,
    /** Server-relative path to the 1280-px-wide poster, for hero
     *  surfaces / Continue Watching backgrounds. */
    val thumbnailPathLarge: String? = null,
)

enum class Protocol(val label: String) {
    HLS("HLS"), DASH("DASH");

    companion object {
        /** Map an `is.protocol` launch-arg / wire value (iOS `StreamProtocol`
         *  rawValue: `hls` / `dash`) to the enum. null = unrecognised. #797. */
        fun fromArg(raw: String): Protocol? = when (raw.trim().lowercase()) {
            "hls" -> HLS
            "dash" -> DASH
            else -> null
        }
    }
}
enum class Segment(val label: String, val suffix: String) {
    LL("LL", ""), ONE("1s", "_1s"), TWO("2s", "_2s"), SIX("6s", "_6s");

    companion object {
        /** Map an `is.segment` launch-arg / wire value (iOS `SegmentLength`
         *  rawValue: `ll` / `s2` / `s6`) to the enum; tolerates the `2s`/`6s`
         *  label form too. null = unrecognised. #797. */
        fun fromArg(raw: String): Segment? = when (raw.trim().lowercase()) {
            "ll" -> LL
            "s1", "1s" -> ONE
            "s2", "2s" -> TWO
            "s6", "6s" -> SIX
            else -> null
        }
    }
}
enum class Codec(val label: String) {
    AUTO("Auto"), H264("H.264"), HEVC("HEVC"), AV1("AV1");

    companion object {
        /** Map an `is.codec` launch-arg / wire value (iOS `CodecFilter`
         *  rawValue: `auto` / `h264` / `hevc` / `av1`) to the enum.
         *  null = unrecognised. #797. */
        fun fromArg(raw: String): Codec? = when (raw.trim().lowercase()) {
            "auto" -> AUTO
            "h264" -> H264
            "hevc" -> HEVC
            "av1" -> AV1
            else -> null
        }
    }
}

/** Snapshot of UI state — observed by every screen. */
data class UiState(
    val servers: List<ServerEnvironment> = emptyList(),
    val activeServerIndex: Int = 0,
    val discovering: Boolean = false,
    val discoveryError: String? = null,

    val content: List<ContentItem> = emptyList(),
    val selectedContent: String = "",
    /** Name of the last content the player rendered a first frame on.
     *  Persisted across app restarts; powers the Continue Watching hero. */
    val lastPlayed: String = "",
    /** Per-clip_id play counts, incremented on every successful first
     *  frame (codec-agnostic — h264/hevc/av1 plays of the same logical
     *  clip share a tally). Persisted as JSON; powers the
     *  "frequently viewed" ordering of the preview row. */
    val viewCounts: Map<String, Int> = emptyMap(),

    val protocol: Protocol = Protocol.HLS,
    val segment: Segment = Segment.SIX,
    // H.264 is the default codec because every TV chip hardware-decodes it
    // — that's the surface most likely to give a clean first-launch
    // experience. AUTO is still selectable from Settings → Codec to widen
    // the catalogue to HEVC + AV1 once the user has confirmed playback works.
    val codec: Codec = Codec.H264,

    val statusText: String = "",
    val currentUrl: String = "",

    /**
     * Advanced flags surfaced in Settings → Advanced. All persisted alongside
     * developerMode.
     */
    val developerMode: Boolean = false,
    /** Allow renditions above 1080p. Off = cap at 1080p (saves decode cost). */
    val allow4K: Boolean = true,
    /** Mute audio. Defaults muted — when testing streaming we rarely want to
     *  hear the audio. Maps the player volume to 0f / 1f. #838. */
    val muted: Boolean = true,
    /** Stream URL goes through the per-session go-proxy port (failure
     *  injection). Off = hit the API port directly. */
    val localProxy: Boolean = true,
    /** Auto-retry the current stream on player errors. */
    val autoRecovery: Boolean = false,
    /** Seek to the live edge on every (re)load instead of letting the
     *  manifest's EXT-X-SERVER-CONTROL HOLD-BACK pick the start point. */
    val goLive: Boolean = false,
    /** User-configurable live-edge offset in seconds. 0 = "use whatever the
     *  manifest's HOLD-BACK or Go Live setting picks" (default). When > 0
     *  the player is pinned to that offset behind live via ExoPlayer's
     *  `LiveConfiguration` target/min/max so ABR rate-adjustment holds it
     *  rather than drifting away. Go Live takes precedence when both are on
     *  (snap to edge). Mirrors the Apple `liveOffsetSeconds` flag and the
     *  `live_offset_s` query param on `testing-session.html` (issues #266 /
     *  #793). Drivable at launch for tests via the `is.flag.live_offset_s`
     *  intent extra. */
    val liveOffsetSeconds: Int = 0,
    /** ABR peak-bitrate ceiling in Mbps. 0 = no cap (default). When > 0 the
     *  ExoPlayer track selector won't pick a video rung whose bitrate exceeds
     *  this — the analog of iOS `AVPlayerItem.preferredPeakBitRate`. Persisted
     *  alongside the other Advanced flags, and drivable at launch for tests
     *  via the `is.flag.peak_bitrate_mbps` intent extra (issue #797). */
    val peakBitrateMbps: Int = 0,
    /** Pin the *startup* rendition to the lowest rung, then let ABR adapt
     *  upward once the first frame is on screen. Off = ABR picks the start
     *  rung normally (default). The analog of iOS
     *  `AVPlayerItem.startsOnFirstEligibleVariant`; on Android it's a
     *  one-shot `setForceLowestBitrate(true)` lock released at first frame.
     *  Persisted alongside the other Advanced flags, and drivable at launch
     *  for tests via the `is.flag.starts_first_variant` intent extra (#797). */
    val startsFirstVariant: Boolean = false,
    /** Skip the Home screen on cold launch when a saved server and a
     *  lastPlayed clip both exist — go straight to Playback so the user
     *  is back inside their stream without waiting for /api/content
     *  to populate Home. Back from Playback still routes to Home,
     *  which loads its visuals at that point. Off by default. */
    val skipHomeOnLaunch: Boolean = false,
    /** Number of simultaneous live-preview ExoPlayer decodes allowed
     *  on Home. 0 = preview video off (every tile shows its
     *  thumbnail). Defaults to the platform-specific hardware cap on
     *  first launch (see `DecodeBudget.maxConcurrent`); users can
     *  lower the number in Settings → Advanced for thermal /
     *  battery / network reasons. Cannot exceed the hardware cap.
     *  Mirrors the Apple `previewVideoSlots` flag. */
    val previewVideoSlots: Int = -1, // -1 sentinel = "use hardware default"
    /** Auto-rotate `play_id` every N seconds for long soak runs (issue
     *  #403). 0 = disabled. Helper enforces a 60s minimum at fire time;
     *  Settings UI offers a small set of presets (5m, 30m, 1h, 6h). */
    val playIdRotationSeconds: Int = 0,

    /** True when HUD is visible on the playback screen. */
    val hudVisible: Boolean = false,
    /** True when the settings drawer is open over the playback screen. */
    val settingsOpen: Boolean = false,

    /** Bumped whenever the underlying ExoPlayer instance is replaced (Reload).
     *  PlaybackScreen keys its PlayerView AndroidView on this so the view
     *  remounts and re-binds to the new player. */
    val playerEpoch: Int = 0,
) {
    val activeServer: ServerEnvironment?
        get() = servers.getOrNull(activeServerIndex)

    /** Apply protocol + codec + segment-length filter to the raw content
     *  list. Segment length is matched against the set go-live can actually
     *  serve (`segment_durations` + `has_ll` from /api/content): go-live
     *  synthesizes both a 2s and a 6s master from any HLS source, while LL
     *  additionally needs on-disk partial info. Pre-#685 servers report
     *  neither field, so we fall back to the legacy native-`segmentDuration`
     *  match (and items with no detected duration pass through). */
    val filteredContent: List<ContentItem>
        get() = content.filter { c ->
            val protocolOk = if (protocol == Protocol.HLS) c.hasHls else c.hasDash
            if (!protocolOk) return@filter false
            if (codec != Codec.AUTO && inferCodec(c.name) != codec) return@filter false
            when (segment) {
                Segment.LL -> c.hasLL ?: true
                Segment.ONE -> c.supportsSegment(1)
                Segment.TWO -> c.supportsSegment(2)
                Segment.SIX -> c.supportsSegment(6)
            }
        }
}

/** Whether go-live can serve this clip at the given integer segment length.
 *  Prefers the server's [ContentItem.segmentDurations] set; falls back to the
 *  legacy single [ContentItem.segmentDuration] (or shows the clip when the
 *  server reports neither, so a pre-#685 server doesn't hide everything). */
fun ContentItem.supportsSegment(seconds: Int): Boolean {
    segmentDurations?.let { if (it.isNotEmpty()) return seconds in it }
    return segmentDuration == seconds || segmentDuration == null
}

fun inferCodec(name: String): Codec {
    val lower = name.lowercase()
    return when {
        "av1" in lower -> Codec.AV1
        "hevc" in lower || "h265" in lower -> Codec.HEVC
        else -> Codec.H264
    }
}
