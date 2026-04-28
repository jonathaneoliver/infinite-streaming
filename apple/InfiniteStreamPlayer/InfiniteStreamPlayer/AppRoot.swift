import SwiftUI

/// Top-level routing surface — replaces the old monolithic ContentView.
/// Mirrors the Android `MainActivity.AppRoot` route enum: ServerPicker
/// → Home → Playback. Settings drawer renders above every route.
struct AppRoot: View {
    @StateObject private var vm = PlayerViewModel()
    @State private var route: Route = .home

    enum Route { case serverPicker, home, playback }

    var body: some View {
        ZStack {
            Tokens.bg.ignoresSafeArea()

            Group {
                switch route {
                case .serverPicker:
                    ServerPickerScreen(vm: vm) { route = .home }
                case .home:
                    HomeScreen(
                        vm: vm,
                        onPlay: { route = .playback },
                        onOpenServerPicker: { route = .serverPicker }
                    )
                case .playback:
                    PlaybackScreen(vm: vm, onBack: { route = .home })
                }
            }
            // While the Settings drawer is open, mark the underlying
            // route as disabled so its focusables (AVPlayerViewController,
            // hero panel, preview tiles, top icons) don't compete with
            // the drawer for focus. Without this, D-pad-Left from a
            // row inside the Advanced picker could escape out of the
            // drawer and land on the video player behind it. Video
            // playback continues — `disabled` only blocks focus +
            // hit-testing in the SwiftUI hierarchy.
            .disabled(vm.settingsOpen)

            // Settings drawer renders above every route, like Android.
            SettingsOverlay(
                vm: vm,
                onOpenServerPicker: {
                    vm.setSettingsOpen(false)
                    route = .serverPicker
                }
            )
        }
        .preferredColorScheme(.dark)
        .onAppear { decideInitialRoute() }
        .onChange(of: route) { _, newRoute in
            // The main vm.player is shared across Playback / Home and
            // keeps its own lifecycle. When leaving Playback we need to
            // fully stop it: pausing alone leaves the audio renderer
            // running, which the user heard as Home-screen audio bleed
            // from the previous stream. Stop + clear drops the AVPlayer
            // item entirely; entering Playback again re-prepares from
            // scratch via buildURLAndLoad.
            if newRoute != .playback {
                vm.player.pause()
                vm.player.replaceCurrentItem(with: nil)
                // Also clear the URL marker so applyContentFilter sees
                // us as "not currently playing" and doesn't silently
                // re-spin the main player on Home.
                vm.clearCurrentURL()
            }
        }
    }

    /// Initial route policy:
    ///   - No saved servers → ServerPicker (guided setup).
    ///   - skipHomeOnLaunch ON + we have a lastPlayed → Playback.
    ///   - Otherwise → Home.
    private func decideInitialRoute() {
        if vm.servers.isEmpty {
            route = .serverPicker
        } else if vm.skipHomeOnLaunch && !vm.lastPlayed.isEmpty {
            vm.setSelectedContent(vm.lastPlayed)
            route = .playback
        } else {
            route = .home
            // Refresh the catalogue once we have a server in hand.
            vm.fetchContentList()
        }
    }
}
