package com.infinitestream.player.state

/** One server known to the app — a row in SharedPreferences. */
data class ServerEnvironment(
    val name: String,
    val host: String,
    val port: String,    // playback (HLS/DASH) port
    val apiPort: String, // REST API port
) {
    val baseUrl: String get() = "http://$host:$port"
    val apiUrl: String get() = "http://$host:$apiPort"
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
     *  when the server couldn't detect it. Used by the Stream picker to
     *  honour the Segment Length preference. */
    val segmentDuration: Int? = null,
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

enum class Protocol(val label: String) { HLS("HLS"), DASH("DASH") }
enum class Segment(val label: String, val suffix: String) {
    LL("LL", ""), TWO("2s", "_2s"), SIX("6s", "_6s")
}
enum class Codec(val label: String) { AUTO("Auto"), H264("H.264"), HEVC("HEVC"), AV1("AV1") }

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
    /** Stream URL goes through the per-session go-proxy port (failure
     *  injection). Off = hit the API port directly. */
    val localProxy: Boolean = true,
    /** Auto-retry the current stream on player errors. */
    val autoRecovery: Boolean = false,
    /** Seek to the live edge on every (re)load instead of letting the
     *  manifest's EXT-X-SERVER-CONTROL HOLD-BACK pick the start point. */
    val goLive: Boolean = false,
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
    /** Suppress every metrics PATCH from this device. Used to
     *  simulate a player that doesn't report analytics. Off by
     *  default — analytics flow as today. The on-device DiagnosticHud
     *  keeps rendering because it reads local state, not server
     *  roundtrips. */
    val disableAnalytics: Boolean = false,

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
     *  list. Segment length is matched against the *native* encoding
     *  duration (`segment_duration` from /api/content). go-live can serve
     *  any segment variant for any source via subsegmentation, but the
     *  Stream picker filters honour the user's preference so they can
     *  reproducibly find content encoded at the chosen duration. Items
     *  with no detected segment_duration (older content) pass through. */
    val filteredContent: List<ContentItem>
        get() = content.filter { c ->
            val protocolOk = if (protocol == Protocol.HLS) c.hasHls else c.hasDash
            if (!protocolOk) return@filter false
            if (codec != Codec.AUTO && inferCodec(c.name) != codec) return@filter false
            val sd = c.segmentDuration
            if (sd != null) {
                val segOk = when (segment) {
                    Segment.LL -> sd <= 1
                    Segment.TWO -> sd == 2
                    Segment.SIX -> sd == 6
                }
                if (!segOk) return@filter false
            }
            true
        }
}

fun inferCodec(name: String): Codec {
    val lower = name.lowercase()
    return when {
        "av1" in lower -> Codec.AV1
        "hevc" in lower || "h265" in lower -> Codec.HEVC
        else -> Codec.H264
    }
}
