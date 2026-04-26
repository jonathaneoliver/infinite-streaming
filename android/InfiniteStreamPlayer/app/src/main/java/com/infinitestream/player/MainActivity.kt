package com.infinitestream.player

import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.ui.screen.HomeScreen
import com.infinitestream.player.ui.screen.PlaybackScreen
import com.infinitestream.player.ui.screen.ServerPickerScreen
import com.infinitestream.player.ui.screen.SettingsOverlay
import com.infinitestream.player.ui.theme.InfiniteStreamTheme
import com.infinitestream.player.ui.theme.Tokens

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Keep the screen on while playback is active — release in onStop.
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        setContent {
            InfiniteStreamTheme {
                Box(modifier = Modifier.fillMaxSize().background(Tokens.bg)) {
                    AppRoot()
                }
            }
        }
    }
}

private enum class Route { ServerPicker, Home, Playback }

@Composable
private fun AppRoot() {
    val vm: PlayerViewModel = viewModel()
    val state by vm.state.collectAsStateWithLifecycle()

    var route by remember {
        mutableStateOf(if (state.servers.isEmpty()) Route.ServerPicker else Route.Home)
    }

    BackHandler(enabled = state.settingsOpen) {
        vm.setSettingsOpen(false)
    }
    BackHandler(enabled = !state.settingsOpen && state.hudVisible && route == Route.Playback) {
        vm.setHudVisible(false)
    }
    BackHandler(enabled = !state.settingsOpen && !state.hudVisible && route == Route.Playback) {
        route = Route.Home
    }
    BackHandler(enabled = !state.settingsOpen && route == Route.Home) {
        route = Route.ServerPicker
    }

    // The main vm.player is shared across Playback / Home and keeps its
    // own lifecycle. When leaving Playback we fully stop it: pause()
    // alone would leave the audio bleeding into Home AND keep the
    // hardware decoder allocated, eating one of the chip's four
    // H.264 slots that we need for tile previews. stop() drops the
    // codec; entering Playback again calls buildUrlAndLoad() which
    // re-prepares from scratch.
    androidx.compose.runtime.LaunchedEffect(route) {
        if (route != Route.Playback) {
            vm.player.stop()
            vm.player.clearMediaItems()
            // Also clear the URL state so applyContentFilter (triggered by
            // any later setProtocol / setCodec / re-fetch) treats us as
            // "not currently playing" and doesn't silently re-spin the
            // main player on Home.
            vm.clearCurrentUrl()
        }
    }

    when (route) {
        Route.ServerPicker -> ServerPickerScreen(
            state = state, vm = vm,
            onServerChosen = { route = Route.Home },
        )
        Route.Home -> HomeScreen(
            state = state, vm = vm,
            onPlay = { route = Route.Playback },
            onOpenServerPicker = { route = Route.ServerPicker },
            onOpenSettings = { vm.setSettingsOpen(true) },
        )
        Route.Playback -> PlaybackScreen(
            state = state, vm = vm,
            onOpenSettings = { vm.setSettingsOpen(true) },
        )
    }
    // Settings drawer renders above every route, so the nav-bar's
    // "Settings" item on Home opens it just like the gear in playback.
    SettingsOverlay(
        state = state, vm = vm,
        onDismiss = { vm.setSettingsOpen(false) },
        onOpenServerPicker = {
            vm.setSettingsOpen(false)
            route = Route.ServerPicker
        },
    )
}
