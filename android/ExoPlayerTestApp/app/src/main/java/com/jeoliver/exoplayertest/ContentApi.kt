package com.jeoliver.exoplayertest

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request
import java.util.concurrent.TimeUnit

@Serializable
data class ContentItem(
    val name: String,
    val has_dash: Boolean = false,
    val has_hls: Boolean = false,
    val segment_duration: Int? = null,
    val max_resolution: String = "",
    val max_height: Int = 0
)

private val json = Json { ignoreUnknownKeys = true }
private val client = OkHttpClient.Builder()
    .connectTimeout(5, TimeUnit.SECONDS)
    .readTimeout(5, TimeUnit.SECONDS)
    .build()

suspend fun fetchContent(baseUrl: String): List<ContentItem> = withContext(Dispatchers.IO) {
    try {
        val url = "$baseUrl/api/content"
        android.util.Log.d("ContentApi", "Fetching content from $url")
        val request = Request.Builder().url(url).build()
        val response = client.newCall(request).execute()
        android.util.Log.d("ContentApi", "Response: ${response.code}")
        val body = response.body?.string() ?: return@withContext emptyList()
        android.util.Log.d("ContentApi", "Body length: ${body.length}")
        val items = json.decodeFromString<List<ContentItem>>(body)
        android.util.Log.d("ContentApi", "Parsed ${items.size} items")
        items
    } catch (e: Exception) {
        android.util.Log.e("ContentApi", "Fetch failed: ${e.message}", e)
        emptyList()
    }
}
