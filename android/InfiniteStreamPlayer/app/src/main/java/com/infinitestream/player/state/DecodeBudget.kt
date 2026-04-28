package com.infinitestream.player.state

import android.media.MediaCodecInfo
import android.media.MediaCodecList
import android.media.MediaFormat
import android.util.Log

/**
 * Caps the number of simultaneous live-preview ExoPlayer decodes on the
 * Home screen to what the chip's H.264 decoder will actually allow.
 *
 * Probes [MediaCodecInfo.VideoCapabilities.maxSupportedInstances] for the
 * default AVC decoder. Apple's iOS / tvOS counterpart hardcodes per-device
 * caps because Apple doesn't expose a public concurrent-decoder API; on
 * Android we have one, so we use it. Result is computed once and cached
 * for the process lifetime — the answer can't change without a chip swap.
 *
 * The cap is intentionally bounded:
 *   - Floor 1: always allow at least the active clip to decode.
 *   - Ceiling 3: layout decision — the carousel is built around three
 *     tiles being on-screen at a time. A more capable chip can decode
 *     more, but we don't gain anything by showing more cards than fit.
 */
object DecodeBudget {
    private const val TAG = "DecodeBudget"
    private const val LAYOUT_CEILING = 3
    private const val FALLBACK = 3

    /**
     * Max simultaneous LIVE preview tiles that should hold an ExoPlayer.
     * Lazily probed on first read.
     */
    val maxConcurrent: Int by lazy { probe() }

    private fun probe(): Int {
        val raw = runCatching { probeAvcMaxInstances() }.getOrNull()
        val effective = (raw ?: FALLBACK).coerceIn(1, LAYOUT_CEILING)
        Log.i(TAG, "AVC maxSupportedInstances=${raw ?: "unknown"} → cap=$effective")
        return effective
    }

    private fun probeAvcMaxInstances(): Int? {
        val list = MediaCodecList(MediaCodecList.REGULAR_CODECS)
        val format = MediaFormat.createVideoFormat(MediaFormat.MIMETYPE_VIDEO_AVC, 640, 360)
        val name = list.findDecoderForFormat(format) ?: return null
        for (info in list.codecInfos) {
            if (info.isEncoder) continue
            if (info.name != name) continue
            val caps = info.getCapabilitiesForType(MediaFormat.MIMETYPE_VIDEO_AVC)
            return caps.maxSupportedInstances
        }
        return null
    }
}
