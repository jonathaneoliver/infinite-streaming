package com.jeoliver.exoplayertest

enum class ServerEnvironment(
    val label: String,
    val host: String,
    val contentPort: Int,
    val playbackPort: Int,
    val scheme: String,
) {
    // Docker Compose / k3s default — plain HTTP.
    LOCAL("Local (30000)", "localhost", 30000, 30081, "http"),
    // k3d dev cluster — plain HTTP.
    LOCAL_DEV("Dev (40000)", "localhost", 40000, 40081, "http"),
    // test-dev deploy — TLS (mkcert) since tests/deploy/override-dev.yml.
    LOCAL_TEST("Test (21000)", "localhost", 21000, 21081, "https");

    val contentBaseUrl get() = "$scheme://$host:$contentPort"
    val playbackBaseUrl get() = "$scheme://$host:$playbackPort"
}
