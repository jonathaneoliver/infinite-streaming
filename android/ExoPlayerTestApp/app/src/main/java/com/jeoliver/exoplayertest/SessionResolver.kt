package com.jeoliver.exoplayertest

import kotlinx.coroutines.*
import kotlinx.serialization.json.*
import okhttp3.*
import okhttp3.sse.*
import java.util.concurrent.TimeUnit

class SessionResolver(
    private val controlBaseUrl: String,
    private val playerId: String,
    private val onSessionId: (String) -> Unit
) {
    private var eventSource: EventSource? = null
    private var pollJob: Job? = null
    private val client = OkHttpClient.Builder()
        .readTimeout(0, TimeUnit.MILLISECONDS)
        .build()
    private val json = Json { ignoreUnknownKeys = true }

    fun start(scope: CoroutineScope) {
        startSSE()
        pollJob = scope.launch(Dispatchers.IO) {
            while (isActive) {
                pollSessions()
                delay(5000)
            }
        }
    }

    fun stop() {
        eventSource?.cancel()
        eventSource = null
        pollJob?.cancel()
        pollJob = null
    }

    private fun startSSE() {
        val url = "$controlBaseUrl/api/sessions/stream?player_id=${playerId}"
        val request = Request.Builder().url(url).build()
        eventSource = EventSources.createFactory(client).newEventSource(request, object : EventSourceListener() {
            override fun onEvent(eventSource: EventSource, id: String?, type: String?, data: String) {
                if (type != "sessions") return
                try {
                    val payload = json.parseToJsonElement(data).jsonObject
                    val sessions = payload["sessions"]?.jsonArray ?: return
                    for (session in sessions) {
                        val obj = session.jsonObject
                        val pid = obj["player_id"]?.jsonPrimitive?.contentOrNull
                        val sid = obj["session_id"]?.jsonPrimitive?.contentOrNull
                        if (pid == playerId && sid != null) {
                            onSessionId(sid)
                            return
                        }
                    }
                } catch (_: Exception) {}
            }
        })
    }

    private fun pollSessions() {
        try {
            val request = Request.Builder().url("$controlBaseUrl/api/sessions").build()
            val response = client.newCall(request).execute()
            val body = response.body?.string() ?: return
            val sessions = json.parseToJsonElement(body).jsonArray
            for (session in sessions) {
                val obj = session.jsonObject
                val pid = obj["player_id"]?.jsonPrimitive?.contentOrNull
                val sid = obj["session_id"]?.jsonPrimitive?.contentOrNull
                if (pid == playerId && sid != null) {
                    onSessionId(sid)
                    return
                }
            }
        } catch (_: Exception) {}
    }
}
