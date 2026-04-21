package com.jeoliver.exoplayertest

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.*
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.media3.ui.PlayerView

enum class ProtocolFilter(val label: String) { HLS("HLS"), DASH("DASH") }
enum class SegmentFilter(val label: String) { ALL("All"), S2("2s"), S6("6s") }
enum class CodecFilter(val label: String) { ALL("Auto"), H264("H264"), HEVC("HEVC"), AV1("AV1") }

// TV-friendly modifier: thick yellow border when focused
@Composable
fun Modifier.tvFocusHighlight(): Modifier {
    var focused by remember { mutableStateOf(false) }
    return this
        .onFocusChanged { focused = it.isFocused }
        .then(
            if (focused) Modifier.border(3.dp, Color(0xFFFFD600), RoundedCornerShape(8.dp))
            else Modifier
        )
        .focusable()
}

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
                    .fillMaxWidth(0.6f)
                    .fillMaxHeight(0.8f)
            ) {
                Column {
                    Text(
                        "Select Content (${filteredContent.size} items)",
                        style = MaterialTheme.typography.titleMedium,
                        modifier = Modifier.padding(16.dp)
                    )
                    HorizontalDivider()
                    LazyColumn(modifier = Modifier.fillMaxSize()) {
                        items(filteredContent) { item ->
                            val isPlaying = item.name == currentContent
                            var itemFocused by remember { mutableStateOf(false) }
                            ListItem(
                                modifier = Modifier
                                    .onFocusChanged { itemFocused = it.isFocused }
                                    .then(
                                        if (itemFocused) Modifier.border(2.dp, Color(0xFFFFD600))
                                        else Modifier
                                    )
                                    .focusable()
                                    .clickable {
                                        vm.playContent(item.name, protocolFilter, segmentFilter)
                                        showContentPicker = false
                                    }
                                    .onKeyEvent { event ->
                                        if (event.type == KeyEventType.KeyUp && (event.key == Key.Enter || event.key == Key.DirectionCenter)) {
                                            vm.playContent(item.name, protocolFilter, segmentFilter)
                                            showContentPicker = false
                                            true
                                        } else false
                                    },
                                headlineContent = {
                                    Text(
                                        item.name.replace("_", " "),
                                        color = when {
                                            itemFocused -> Color(0xFFFFD600)
                                            isPlaying -> MaterialTheme.colorScheme.primary
                                            else -> MaterialTheme.colorScheme.onSurface
                                        },
                                        style = MaterialTheme.typography.bodyMedium
                                    )
                                },
                                supportingContent = {
                                    val formats = listOfNotNull(
                                        if (item.has_hls) "HLS" else null,
                                        if (item.has_dash) "DASH" else null
                                    ).joinToString(" · ")
                                    Text(
                                        "$formats · ${item.max_resolution}${item.segment_duration?.let { " · ${it}s" } ?: ""}",
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

    // TV layout: video left, controls right
    Column(modifier = Modifier.fillMaxSize().padding(24.dp)) {
        // Title + status bar
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically
        ) {
            Text("InfiniteStream Player", style = MaterialTheme.typography.titleLarge)
            Text(
                "ID: ${vm.playerId} · $playerState",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
        }

        Spacer(Modifier.height(12.dp))

        // Main content: video left, controls right
        Row(
            modifier = Modifier.weight(1f).fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(20.dp)
        ) {
            // Left: video player
            Box(
                modifier = Modifier
                    .weight(1.4f)
                    .fillMaxHeight(),
                contentAlignment = Alignment.Center
            ) {
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
                        modifier = Modifier.fillMaxSize()
                    )
                } else {
                    Text(
                        if (contentList.isEmpty()) "Loading content..." else "Select content to play",
                        style = MaterialTheme.typography.bodyLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
            }

            // Right: controls panel
            Column(
                modifier = Modifier.weight(0.6f).fillMaxHeight(),
                verticalArrangement = Arrangement.spacedBy(10.dp)
            ) {
                // Action buttons
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Button(
                        onClick = { vm.loadContent() },
                        modifier = Modifier.tvFocusHighlight()
                    ) { Text("Retry") }
                    OutlinedButton(
                        onClick = { currentContent?.let { vm.playContent(it, protocolFilter, segmentFilter) } },
                        enabled = currentContent != null,
                        modifier = Modifier.tvFocusHighlight()
                    ) { Text("Restart") }
                    OutlinedButton(
                        onClick = { vm.stopPlayback() },
                        enabled = currentContent != null,
                        modifier = Modifier.tvFocusHighlight()
                    ) { Text("Stop") }
                }

                // Status
                if (statusMessage.isNotEmpty()) {
                    Text(statusMessage, style = MaterialTheme.typography.bodySmall, maxLines = 2)
                }

                HorizontalDivider()

                // Server picker
                Text("Server", style = MaterialTheme.typography.labelLarge)
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    ServerEnvironment.entries.forEach { serverEnv ->
                        FilterChip(
                            selected = serverEnv == env,
                            onClick = { vm.setEnvironment(serverEnv) },
                            label = { Text(serverEnv.label) },
                            modifier = Modifier.tvFocusHighlight()
                        )
                    }
                }

                // Protocol
                Text("Protocol", style = MaterialTheme.typography.labelLarge)
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    ProtocolFilter.entries.forEach { pf ->
                        FilterChip(
                            selected = pf == protocolFilter,
                            onClick = { protocolFilter = pf },
                            label = { Text(pf.label) },
                            modifier = Modifier.tvFocusHighlight()
                        )
                    }
                }

                // Segment + Codec
                Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                    Column {
                        Text("Segment", style = MaterialTheme.typography.labelLarge)
                        Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                            SegmentFilter.entries.forEach { sf ->
                                FilterChip(
                                    selected = sf == segmentFilter,
                                    onClick = { segmentFilter = sf },
                                    label = { Text(sf.label) },
                                    modifier = Modifier.tvFocusHighlight()
                                )
                            }
                        }
                    }
                    Column {
                        Text("Codec", style = MaterialTheme.typography.labelLarge)
                        Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                            CodecFilter.entries.forEach { cf ->
                                FilterChip(
                                    selected = cf == codecFilter,
                                    onClick = { codecFilter = cf },
                                    label = { Text(cf.label) },
                                    modifier = Modifier.tvFocusHighlight()
                                )
                            }
                        }
                    }
                }

                Spacer(Modifier.weight(1f))

                // Content selector
                Button(
                    onClick = { showContentPicker = true },
                    modifier = Modifier.fillMaxWidth().tvFocusHighlight()
                ) {
                    Text(
                        currentContent?.replace("_", " ") ?: "Select Content (${filteredContent.size})",
                        maxLines = 1,
                        style = MaterialTheme.typography.bodyMedium
                    )
                }
            }
        }
    }
}
