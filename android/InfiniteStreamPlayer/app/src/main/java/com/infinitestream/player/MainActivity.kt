package com.infinitestream.player

import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.viewModels
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
    private val tStart = android.os.SystemClock.uptimeMillis()
    private fun tag(s: String) = android.util.Log.i(
        "InfiniteStream", "T+${android.os.SystemClock.uptimeMillis() - tStart}ms $s")

    private val vm: PlayerViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        tag("MainActivity onCreate begin")
        super.onCreate(savedInstanceState)
        // Keep the screen on while playback is active — release in onStop.
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        tag("MainActivity onCreate setContent")
        setContent {
            androidx.compose.runtime.SideEffect { tag("first composition") }
            InfiniteStreamTheme {
                Box(modifier = Modifier.fillMaxSize().background(Tokens.bg)) {
                    AppRoot()
                }
            }
        }
        tag("MainActivity onCreate end")
    }

    override fun onStop() {
        super.onStop()
        // User pressed Home / switched apps. Pause the player so we don't
        // keep decoding audio in the background while another app
        // (e.g. YouTube) is in the foreground. onStart restores nothing
        // automatically — the user has to come back and hit play; this
        // matches what the Android TV system expects of background-
        // unaware media apps.
        vm.player.pause()
    }
}

private enum class Route { ServerPicker, Home, Playback }

@Composable
private fun AppRoot() {
    val vm: PlayerViewModel = viewModel()
    val state by vm.state.collectAsStateWithLifecycle()

    // Initial route policy:
    //   - No saved servers → ServerPicker (guided setup).
    //   - skipHomeOnLaunch ON + we have a lastPlayed → Playback, so the
    //     user is back inside their stream without waiting for Home's
    //     /api/content fetch. Home will mount only if Back is pressed
    //     from Playback, at which point its visuals initialize for the
    //     first time this session.
    //   - Otherwise → Home (the previous default).
    var route by remember {
        mutableStateOf(when {
            state.servers.isEmpty() -> Route.ServerPicker
            state.skipHomeOnLaunch && state.lastPlayed.isNotEmpty() -> Route.Playback
            else -> Route.Home
        })
    }
    // One-shot auto-resume when the cold-start route is Playback. Uses
    // setSelectedContent (not the deferred variant) since there are no
    // tile decoders to wait on — Home didn't mount.
    androidx.compose.runtime.LaunchedEffect(Unit) {
        if (route == Route.Playback
            && state.selectedContent.isEmpty()
            && state.lastPlayed.isNotEmpty()) {
            vm.setSelectedContent(state.lastPlayed)
        }
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
