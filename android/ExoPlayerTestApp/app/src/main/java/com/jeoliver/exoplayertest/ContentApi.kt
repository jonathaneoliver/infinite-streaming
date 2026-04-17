package com.jeoliver.exoplayertest

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request

@Serializable
data class ContentItem(
    val name: String,
    val has_dash: Boolean = false,
    val has_hls: Boolean = false,
    val segment_duration: Int = 0,
    val max_resolution: String = "",
    val max_height: Int = 0
)

private val json = Json { ignoreUnknownKeys = true }
private val client = OkHttpClient()

suspend fun fetchContent(baseUrl: String): List<ContentItem> = withContext(Dispatchers.IO) {
    val request = Request.Builder().url("$baseUrl/api/content").build()
    val response = client.newCall(request).execute()
    val body = response.body?.string() ?: return@withContext emptyList()
    try {
        json.decodeFromString<List<ContentItem>>(body)
    } catch (_: Exception) {
        emptyList()
    }
}
