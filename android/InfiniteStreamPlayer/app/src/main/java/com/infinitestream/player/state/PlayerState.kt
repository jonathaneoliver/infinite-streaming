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

    val protocol: Protocol = Protocol.HLS,
    val segment: Segment = Segment.SIX,
    val codec: Codec = Codec.AUTO,

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

    /** True when HUD is visible on the playback screen. */
    val hudVisible: Boolean = false,
    /** True when the settings drawer is open over the playback screen. */
    val settingsOpen: Boolean = false,
) {
    val activeServer: ServerEnvironment?
        get() = servers.getOrNull(activeServerIndex)

    /** Apply protocol + codec filter to the raw content list. */
    val filteredContent: List<ContentItem>
        get() = content.filter { c ->
            val protocolOk = if (protocol == Protocol.HLS) c.hasHls else c.hasDash
            if (!protocolOk) return@filter false
            if (codec == Codec.AUTO) return@filter true
            inferCodec(c.name) == codec
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
