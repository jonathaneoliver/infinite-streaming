package com.jeoliver.exoplayertest

import kotlinx.coroutines.*
import kotlinx.serialization.json.*
import okhttp3.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.RequestBody.Companion.toRequestBody
import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

class MetricsReporter(
    private val controlBaseUrl: String
) {
    private val client = OkHttpClient()
    private val jsonMediaType = "application/json".toMediaType()
    private val isoFormatter = DateTimeFormatter.ofPattern("yyyy-MM-dd'T'HH:mm:ss.SSS'Z'")
        .withZone(ZoneOffset.UTC)

    fun nowISO(): String = isoFormatter.format(Instant.now())

    suspend fun postMetrics(sessionId: String, metrics: Map<String, Any?>) = withContext(Dispatchers.IO) {
        val filtered = metrics.filterValues { it != null }
        if (filtered.isEmpty()) return@withContext

        val set = buildJsonObject {
            for ((key, value) in filtered) {
                when (value) {
                    is String -> put(key, value)
                    is Int -> put(key, value)
                    is Long -> put(key, value)
                    is Double -> put(key, value)
                    is Float -> put(key, value.toDouble())
                    is Boolean -> put(key, value)
                    else -> put(key, value.toString())
                }
            }
        }
        val body = buildJsonObject {
            put("set", set)
            put("fields", JsonArray(filtered.keys.map { JsonPrimitive(it) }))
        }

        val request = Request.Builder()
            .url("$controlBaseUrl/api/session/$sessionId/metrics")
            .post(body.toString().toRequestBody(jsonMediaType))
            .build()
        try {
            client.newCall(request).execute().close()
        } catch (_: Exception) {}
    }
}
