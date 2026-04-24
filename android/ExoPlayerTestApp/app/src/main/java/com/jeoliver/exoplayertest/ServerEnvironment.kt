package com.jeoliver.exoplayertest

enum class ServerEnvironment(
    val label: String,
    val host: String,
    val contentPort: Int,
    val playbackPort: Int
) {
    LOCAL("Local (30000)", "localhost", 30000, 30081),
    LOCAL_DEV("Dev (40000)", "localhost", 40000, 40081),
    LOCAL_TEST("Test (21000)", "localhost", 21000, 21081);

    val contentBaseUrl get() = "http://$host:$contentPort"
    val playbackBaseUrl get() = "http://$host:$playbackPort"
}
