package com.jeoliver.exoplayertest

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.media3.ui.PlayerView

enum class ProtocolFilter(val label: String) { HLS("HLS"), DASH("DASH") }
enum class SegmentFilter(val label: String) { ALL("All"), S2("2s"), S6("6s") }
enum class CodecFilter(val label: String) { ALL("Auto"), H264("H264"), HEVC("HEVC"), AV1("AV1") }

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            MaterialTheme {
                Surface(modifier = Modifier.fillMaxSize()) {
                    AppScreen()
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppScreen(vm: PlayerViewModel = viewModel()) {
    val context = LocalContext.current
    val env by vm.env.collectAsState()
    val contentList by vm.contentList.collectAsState()
    val currentContent by vm.currentContent.collectAsState()
    val playerState by vm.playerState.collectAsState()
    val statusMessage by vm.statusMessage.collectAsState()

    var protocolFilter by remember { mutableStateOf(ProtocolFilter.HLS) }
    var segmentFilter by remember { mutableStateOf(SegmentFilter.S2) }
    var codecFilter by remember { mutableStateOf(CodecFilter.H264) }
    var showContentPicker by remember { mutableStateOf(false) }

    val filteredContent = contentList.filter { item ->
        val hasProtocol = when (protocolFilter) {
            ProtocolFilter.HLS -> item.has_hls
            ProtocolFilter.DASH -> item.has_dash
        }
        val matchesCodec = when (codecFilter) {
            CodecFilter.ALL -> true
            CodecFilter.H264 -> item.name.contains("h264", ignoreCase = true)
            CodecFilter.HEVC -> item.name.contains("hevc", ignoreCase = true)
            CodecFilter.AV1 -> item.name.contains("av1", ignoreCase = true)
        }
        hasProtocol && matchesCodec
    }

    LaunchedEffect(Unit) {
        vm.initPlayer(context)
        vm.loadContent()
    }

    // Content picker dialog
    if (showContentPicker) {
        Dialog(onDismissRequest = { showContentPicker = false }) {
            Card(
                modifier = Modifier
                    .fillMaxWidth()
                    .fillMaxHeight(0.7f)
            ) {
                Column {
                    Text(
                        "Select Content (${filteredContent.size} items)",
                        style = MaterialTheme.typography.titleMedium,
                        modifier = Modifier.padding(16.dp)
                    )
                    HorizontalDivider()
                    LazyColumn(
                        modifier = Modifier.fillMaxSize()
                    ) {
                        items(filteredContent) { item ->
                            val isPlaying = item.name == currentContent
                            ListItem(
                                modifier = Modifier.clickable {
                                    vm.playContent(item.name, protocolFilter, segmentFilter)
                                    showContentPicker = false
                                },
                                headlineContent = {
                                    Text(
                                        item.name.replace("_", " "),
                                        color = if (isPlaying) MaterialTheme.colorScheme.primary
                                                else MaterialTheme.colorScheme.onSurface,
                                        style = MaterialTheme.typography.bodyMedium
                                    )
                                },
                                supportingContent = {
                                    val formats = listOfNotNull(
                                        if (item.has_hls) "HLS" else null,
                                        if (item.has_dash) "DASH" else null
                                    ).joinToString(" · ")
                                    Text(
                                        "$formats · ${item.max_resolution} · ${item.segment_duration}s",
                                        style = MaterialTheme.typography.bodySmall
                                    )
                                }
                            )
                        }
                    }
                }
            }
        }
    }

    Column(modifier = Modifier.fillMaxSize()) {
        // Server picker
        Row(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 4.dp),
            horizontalArrangement = Arrangement.spacedBy(4.dp),
            verticalAlignment = Alignment.CenterVertically
        ) {
            Text("Server:", style = MaterialTheme.typography.labelMedium)
            ServerEnvironment.entries.forEach { serverEnv ->
                FilterChip(
                    selected = serverEnv == env,
                    onClick = { vm.setEnvironment(serverEnv) },
                    label = { Text(serverEnv.label, style = MaterialTheme.typography.labelSmall) }
                )
            }
        }

        // Filters row
        Row(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically
        ) {
            Text("Protocol:", style = MaterialTheme.typography.labelSmall)
            ProtocolFilter.entries.forEach { pf ->
                FilterChip(
                    selected = pf == protocolFilter,
                    onClick = { protocolFilter = pf },
                    label = { Text(pf.label, style = MaterialTheme.typography.labelSmall) }
                )
            }
            Text("Seg:", style = MaterialTheme.typography.labelSmall)
            SegmentFilter.entries.forEach { sf ->
                FilterChip(
                    selected = sf == segmentFilter,
                    onClick = { segmentFilter = sf },
                    label = { Text(sf.label, style = MaterialTheme.typography.labelSmall) }
                )
            }
            Text("Codec:", style = MaterialTheme.typography.labelSmall)
            CodecFilter.entries.forEach { cf ->
                FilterChip(
                    selected = cf == codecFilter,
                    onClick = { codecFilter = cf },
                    label = { Text(cf.label, style = MaterialTheme.typography.labelSmall) }
                )
            }
        }

        // Content selector + status
        Row(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 4.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically
        ) {
            Button(onClick = { showContentPicker = true }) {
                Text(currentContent?.replace("_", " ")?.take(30) ?: "Select Content")
            }
            if (currentContent != null) {
                OutlinedButton(onClick = {
                    currentContent?.let { vm.playContent(it, protocolFilter, segmentFilter) }
                }) { Text("Restart") }
                OutlinedButton(onClick = { vm.stopPlayback() }) { Text("Stop") }
            }
            Spacer(Modifier.weight(1f))
            Text(
                "ID: ${vm.playerId} · $playerState",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
        }

        // Status message
        if (statusMessage.isNotEmpty()) {
            Text(
                statusMessage,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(horizontal = 8.dp)
            )
        }

        // Player view — fills remaining space
        val player = vm.player
        if (currentContent != null && player != null) {
            AndroidView(
                factory = { ctx ->
                    PlayerView(ctx).apply {
                        this.player = player
                        useController = true
                    }
                },
                update = { view ->
                    view.player = player
                },
                modifier = Modifier
                    .fillMaxWidth()
                    .weight(1f)
                    .padding(8.dp)
            )
        } else {
            Box(
                modifier = Modifier.fillMaxWidth().weight(1f),
                contentAlignment = Alignment.Center
            ) {
                Text(
                    if (contentList.isEmpty()) "Loading content..." else "Select content to play",
                    style = MaterialTheme.typography.bodyLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant
                )
            }
        }
    }
}
