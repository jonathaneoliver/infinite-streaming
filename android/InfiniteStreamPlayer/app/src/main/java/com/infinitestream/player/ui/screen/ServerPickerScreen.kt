@file:kotlin.OptIn(androidx.tv.material3.ExperimentalTvMaterial3Api::class)

package com.infinitestream.player.ui.screen

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInVertically
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.grid.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Icon
import androidx.tv.material3.Text
import com.infinitestream.player.RendezvousService
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.state.ServerEnvironment
import com.infinitestream.player.state.UiState
import com.infinitestream.player.ui.component.PulseDot
import com.infinitestream.player.ui.component.StatusDot
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Space
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus

/**
 * Server picker — first thing the user sees on launch when no server is set.
 * Spec: 3-column grid of cards. Each card shows status dot · "STATUS · last
 * seen" · server name · hostname · latency · stream count. "+ Add server"
 * appears as a dashed card on the same grid. Pulse dot at the bottom while
 * scanning. Long-press OK to forget a known server.
 */
@Composable
fun ServerPickerScreen(
    state: UiState,
    vm: PlayerViewModel,
    onServerChosen: () -> Unit,
) {
    var showManual by remember { mutableStateOf(false) }
    var showPairCode by remember { mutableStateOf(false) }
    var discovered by remember { mutableStateOf<List<RendezvousService.DiscoveredServer>>(emptyList()) }

    // Run discovery on every entry to the picker, not just when the saved
    // list is empty. The Rendezvous Worker only knows about servers visible
    // from the caller's public IP, and that set changes as the user moves
    // between networks. Cards stream in below as the response arrives.
    LaunchedEffect(Unit) {
        vm.discoverServers { found -> discovered = found }
    }

    // Filter out discovered entries that are already saved (dedupe by host:port).
    val savedKeys = remember(state.servers) {
        state.servers.map { "${it.host.lowercase()}:${it.apiPort}" }.toSet()
    }
    val newDiscovered = remember(discovered, savedKeys) {
        // Rendezvous Worker can return the same URL multiple times when
        // separate server_ids advertise it (e.g. multi-NIC hosts re-
        // announcing). Compose's LazyVerticalGrid throws when two items
        // share a key, so dedupe by URL — and when two records collide,
        // keep the one with the newest `last_seen` so we surface the
        // most recently active announce.
        discovered
            .sortedByDescending { it.lastSeenMs }
            .distinctBy { it.url.lowercase() }
            .filter { d ->
                val u = android.net.Uri.parse(d.url)
                val host = u.host?.lowercase() ?: return@filter false
                val port = if (u.port >= 0) u.port else if (u.scheme.equals("https", true)) 443 else 80
                "$host:$port" !in savedKeys
            }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg)
            .padding(horizontal = Space.s8, vertical = Space.s7)
    ) {
        Column(modifier = Modifier.fillMaxSize()) {
            Text("InfiniteStream", style = AppType.bodySm.copy(color = Tokens.fgDim))
            Spacer(Modifier.height(Space.s2))
            Text("Pick a server", style = AppType.displayLg.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s1))
            Text(
                "Saved servers and any visible to your network appear below.",
                style = AppType.body.copy(color = Tokens.fgDim),
            )
            Spacer(Modifier.height(Space.s7))

            LazyVerticalGrid(
                columns = GridCells.Fixed(3),
                contentPadding = PaddingValues(bottom = Space.s7),
                horizontalArrangement = Arrangement.spacedBy(Space.s4),
                verticalArrangement = Arrangement.spacedBy(Space.s4),
                modifier = Modifier.fillMaxWidth().weight(1f),
            ) {
                // Saved servers first.
                itemsIndexed(state.servers, key = { _, s -> "saved:${s.host}:${s.apiPort}" }) { i, server ->
                    ServerCard(
                        index = i,
                        server = server,
                        active = i == state.activeServerIndex,
                        onClick = { vm.selectServer(i); onServerChosen() },
                        onForget = { vm.forgetServer(i) },
                    )
                }
                // Newly-discovered (not yet saved) below saved.
                items(newDiscovered, key = { d -> "found:${d.url}" }) { d ->
                    DiscoveredServerCard(
                        discovered = d,
                        onClick = {
                            if (vm.addServerFromUrl(d.url) >= 0) onServerChosen()
                        },
                    )
                }
                // Add-options.
                item { PairCodeCard(onClick = { showPairCode = true }) }
                item { AddServerCard(onClick = { showManual = true }) }
            }

            // Discovery indicator at the bottom — pulse dot + status text.
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (state.discovering) {
                    PulseDot(color = Tokens.accent)
                    Spacer(Modifier.width(Space.s2))
                    Text("Discovering on this network…", style = AppType.mono.copy(color = Tokens.fgDim))
                } else if (state.discoveryError != null) {
                    Text(
                        "Discovery: ${state.discoveryError}",
                        style = AppType.mono.copy(color = Tokens.fgDim),
                    )
                } else if (state.servers.isEmpty() && newDiscovered.isEmpty()) {
                    Text(
                        "No servers detected — add one manually with the + card.",
                        style = AppType.mono.copy(color = Tokens.fgDim),
                    )
                }
            }
        }
    }

    if (showManual) {
        ManualServerSheet(
            onDismiss = { showManual = false },
            onSubmit = { url ->
                showManual = false
                if (vm.addServerFromUrl(url) >= 0) onServerChosen()
            },
        )
    }

    if (showPairCode) {
        PairCodeSheet(
            onDismiss = { showPairCode = false },
            onPaired = { url ->
                showPairCode = false
                if (vm.addServerFromUrl(url) >= 0) onServerChosen()
            },
        )
    }
}

/**
 * A server discovered via the rendezvous Worker but not yet saved. Tapping
 * adds it to the saved list and selects it.
 */
@Composable
private fun DiscoveredServerCard(
    discovered: RendezvousService.DiscoveredServer,
    onClick: () -> Unit,
) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .aspectRatio(1.6f)
            .tvFocus(cornerRadius = Radius.cardLg)
            .clip(RoundedCornerShape(Radius.cardLg))
            .background(Tokens.bgSoft)
            .border(1.dp, Tokens.accent.copy(alpha = 0.4f), RoundedCornerShape(Radius.cardLg))
            .clickable(onClick = onClick)
            .padding(Space.s5),
    ) {
        Column(modifier = Modifier.fillMaxSize()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                StatusDot(color = Tokens.accent)
                Spacer(Modifier.width(Space.s1))
                Text("DISCOVERED · TAP TO ADD",
                    style = AppType.label.copy(color = Tokens.accent))
            }
            Spacer(Modifier.weight(1f))
            Text(
                discovered.label.ifEmpty { discovered.url },
                style = AppType.title.copy(color = Tokens.fg),
            )
            Spacer(Modifier.height(Space.s1))
            Text(discovered.url, style = AppType.mono.copy(color = Tokens.fgDim))
        }
    }
}

/**
 * "Pair with code" card — shows a 6-character code that the user types into
 * a dashboard's "Server Info → Pair with code" widget. Used to bring a
 * server in from across networks where rendezvous-discovery can't see it.
 */
@Composable
private fun PairCodeCard(onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .aspectRatio(1.6f)
            .tvFocus(cornerRadius = Radius.cardLg)
            .clip(RoundedCornerShape(Radius.cardLg))
            .background(Tokens.bgCard)
            .border(1.dp, Tokens.line, RoundedCornerShape(Radius.cardLg))
            .clickable(onClick = onClick)
            .padding(Space.s5),
    ) {
        Column(modifier = Modifier.fillMaxSize()) {
            Text("CROSS-NETWORK", style = AppType.label.copy(color = Tokens.fgDim))
            Spacer(Modifier.weight(1f))
            Text("Pair with code", style = AppType.title.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s1))
            Text(
                "Type the code on a dashboard.",
                style = AppType.mono.copy(color = Tokens.fgDim),
                maxLines = 2,
            )
        }
    }
}

/**
 * Modal sheet that generates a pairing code and polls the rendezvous Worker
 * until the dashboard publishes a server URL. Cancel button dismisses and
 * tells the Worker to forget the code.
 */
@Composable
private fun PairCodeSheet(
    onDismiss: () -> Unit,
    onPaired: (String) -> Unit,
) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val code = remember { RendezvousService.generateCode() }
    var error by remember { mutableStateOf<String?>(null) }

    DisposableEffect(code) {
        val canceller = RendezvousService.pollForServerURL(
            context, code,
            /* pollIntervalMs = */ 2000L,
            /* timeoutMs = */ 10L * 60L * 1000L,
        ) { url, err ->
            if (err != null) error = err
            else if (url != null) onPaired(url)
        }
        onDispose { canceller.cancel() }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg.copy(alpha = 0.85f))
            .clickable(onClick = onDismiss),
    ) {
        Column(
            modifier = Modifier
                .align(Alignment.Center)
                .width(620.dp)
                .clip(RoundedCornerShape(Radius.cardLg))
                .background(Tokens.bgSoft)
                .border(1.dp, Tokens.line, RoundedCornerShape(Radius.cardLg))
                .padding(Space.s7)
                .clickable(enabled = false) {},
        ) {
            Text("Pair with code", style = AppType.title.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s4))
            Text(
                "On any device that can reach your server, open the dashboard's " +
                    "Server Info panel and enter this code:",
                style = AppType.body.copy(color = Tokens.fgDim),
            )
            Spacer(Modifier.height(Space.s5))
            Text(code, style = AppType.displayLg.copy(color = Tokens.accent))
            Spacer(Modifier.height(Space.s5))
            Text(
                error ?: "Waiting for the dashboard to publish the URL…",
                style = AppType.mono.copy(color = Tokens.fgDim),
            )
            Spacer(Modifier.height(Space.s6))
            Row(horizontalArrangement = Arrangement.End, modifier = Modifier.fillMaxWidth()) {
                PrimaryButton("Cancel", onClick = onDismiss, accent = false)
            }
        }
    }
}

@Composable
private fun ServerCard(
    index: Int,
    server: ServerEnvironment,
    active: Boolean,
    onClick: () -> Unit,
    onForget: () -> Unit,
) {
    val online = true // Replace with a real ping when discovery is wired.
    AnimatedVisibility(
        visible = true,
        enter = fadeIn() + slideInVertically(initialOffsetY = { it / 6 }),
    ) {
        Box(
            modifier = Modifier
                .fillMaxWidth()
                .aspectRatio(1.6f)
                .alpha(if (online) 1f else 0.55f)
                .tvFocus(cornerRadius = Radius.cardLg)
                .clip(RoundedCornerShape(Radius.cardLg))
                .background(if (active) Tokens.bgCard else Tokens.bgSoft)
                .border(1.dp, Tokens.line, RoundedCornerShape(Radius.cardLg))
                .clickable(onClick = onClick)
                .padding(Space.s5),
        ) {
            Column(modifier = Modifier.fillMaxSize()) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(color = if (online) Tokens.ok else Tokens.offline)
                    Spacer(Modifier.width(Space.s1))
                    Text(
                        if (online) "ONLINE · just now" else "OFFLINE",
                        style = AppType.label.copy(color = Tokens.fgDim),
                    )
                }
                Spacer(Modifier.weight(1f))
                Text(
                    server.name,
                    style = AppType.title.copy(color = Tokens.fg),
                )
                Spacer(Modifier.height(Space.s1))
                Text(
                    "${server.host}:${server.apiPort}",
                    style = AppType.mono.copy(color = Tokens.fgDim),
                )
                Spacer(Modifier.height(Space.s2))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text("api ${server.apiPort}", style = AppType.monoSm.copy(color = Tokens.fgFaint))
                    Spacer(Modifier.width(Space.s2))
                    Text("play ${server.port}", style = AppType.monoSm.copy(color = Tokens.fgFaint))
                }
            }
            // Forget chip — bottom-right so it doesn't fight long server
            // names that wrap to two lines (e.g. jonathanoliver-ubuntu.local
            // :21000 fills almost the whole card width). Smaller footprint
            // (22 dp) since it's a secondary action, but still big enough to
            // get a visible focus ring.
            Box(
                modifier = Modifier
                    .align(Alignment.BottomEnd)
                    .size(22.dp)
                    .tvFocus(cornerRadius = 11.dp)
                    .clip(RoundedCornerShape(11.dp))
                    .background(Tokens.bgCard)
                    .clickable(onClick = onForget),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    Icons.Default.Close,
                    contentDescription = "Forget",
                    tint = Tokens.fgDim,
                    modifier = Modifier.size(14.dp),
                )
            }
        }
    }
}

@Composable
private fun AddServerCard(onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .aspectRatio(1.6f)
            .tvFocus(cornerRadius = Radius.cardLg)
            .clip(RoundedCornerShape(Radius.cardLg))
            .drawBehind {
                // Dashed outline — draw manually because Compose
                // doesn't have a stock dashed-border modifier yet.
                // Stroke is bumped to 3 dp + a fgDim color (vs the previous
                // 2 dp / fgFaint) because at 1080p on a 4 m couch the old
                // dashes were nearly invisible against the pure-black bg.
                val stroke = Stroke(
                    width = 3.dp.toPx(),
                    pathEffect = androidx.compose.ui.graphics.PathEffect.dashPathEffect(
                        floatArrayOf(18f, 12f), 0f
                    ),
                )
                drawRoundRect(
                    color = Tokens.fgDim,
                    size = Size(size.width, size.height),
                    cornerRadius = CornerRadius(Radius.cardLg.toPx(), Radius.cardLg.toPx()),
                    style = stroke,
                )
            }
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Icon(Icons.Default.Add, contentDescription = null, tint = Tokens.fg)
            Spacer(Modifier.height(Space.s1))
            Text("Add server manually", style = AppType.label.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s1))
            Text("hostname or IP", style = AppType.monoSm.copy(color = Tokens.fgDim))
        }
    }
}

@Composable
private fun ManualServerSheet(
    onDismiss: () -> Unit,
    onSubmit: (String) -> Unit,
) {
    var host by remember { mutableStateOf("") }
    var port by remember { mutableStateOf("30000") }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg.copy(alpha = 0.85f))
            .clickable(onClick = onDismiss),
    ) {
        Column(
            modifier = Modifier
                .align(Alignment.Center)
                .width(560.dp)
                .clip(RoundedCornerShape(Radius.cardLg))
                .background(Tokens.bgSoft)
                .border(1.dp, Tokens.line, RoundedCornerShape(Radius.cardLg))
                .padding(Space.s7)
                .clickable(enabled = false) {},
        ) {
            Text("Add server", style = AppType.title.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s4))

            FieldRow("Hostname", host, KeyboardType.Uri) { host = it }
            Spacer(Modifier.height(Space.s2))
            FieldRow("API port", port, KeyboardType.Number) { port = it }

            Spacer(Modifier.height(Space.s5))
            Row(horizontalArrangement = Arrangement.End, modifier = Modifier.fillMaxWidth()) {
                PrimaryButton("Cancel", onClick = onDismiss, accent = false)
                Spacer(Modifier.width(Space.s2))
                PrimaryButton("Add", onClick = {
                    val cleanHost = host.trim()
                    val cleanPort = port.toIntOrNull() ?: 30000
                    if (cleanHost.isNotEmpty() && cleanPort in 1..65535) {
                        onSubmit("http://$cleanHost:$cleanPort")
                    }
                }, accent = true)
            }
        }
    }
}

@Composable
private fun FieldRow(label: String, value: String, kt: KeyboardType, onChange: (String) -> Unit) {
    Column(modifier = Modifier.fillMaxWidth()) {
        Text(label.uppercase(), style = AppType.label.copy(color = Tokens.fgDim))
        Spacer(Modifier.height(Space.s1))
        Box(
            modifier = Modifier
                .fillMaxWidth()
                .height(48.dp)
                .tvFocus(cornerRadius = Radius.row)
                .clip(RoundedCornerShape(Radius.row))
                .background(Tokens.bgCard)
                .border(1.dp, Tokens.line, RoundedCornerShape(Radius.row))
                .padding(horizontal = Space.s3),
            contentAlignment = Alignment.CenterStart,
        ) {
            BasicTextField(
                value = value,
                onValueChange = onChange,
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = kt),
                cursorBrush = SolidColor(Tokens.accent),
                textStyle = AppType.mono.copy(color = Tokens.fg),
                modifier = Modifier.fillMaxWidth(),
            )
        }
    }
}

@Composable
internal fun PrimaryButton(text: String, onClick: () -> Unit, accent: Boolean = false) {
    Box(
        modifier = Modifier
            .height(44.dp)
            .tvFocus(cornerRadius = Radius.pill)
            .clip(RoundedCornerShape(Radius.pill))
            .background(if (accent) Tokens.accent else Tokens.bgCard)
            .border(1.dp, if (accent) Color.Transparent else Tokens.line, RoundedCornerShape(Radius.pill))
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s5),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text,
            style = AppType.button.copy(color = if (accent) Tokens.bg else Tokens.fg),
            textAlign = TextAlign.Center,
        )
    }
}
