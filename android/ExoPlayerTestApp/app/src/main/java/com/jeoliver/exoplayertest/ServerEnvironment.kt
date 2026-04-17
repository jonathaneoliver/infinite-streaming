package com.jeoliver.exoplayertest

enum class ServerEnvironment(
    val label: String,
    val host: String,
    val contentPort: Int,
    val playbackPort: Int
) {
    DEV("Dev (40000)", "100.111.190.54", 40000, 40081),
    RELEASE("Release (30000)", "infinitestreaming.jeoliver.com", 30000, 30081),
    UBUNTU("Ubuntu (21000)", "192.168.0.106", 21000, 21081);

    val contentBaseUrl get() = "http://$host:$contentPort"
    val playbackBaseUrl get() = "http://$host:$playbackPort"
}
