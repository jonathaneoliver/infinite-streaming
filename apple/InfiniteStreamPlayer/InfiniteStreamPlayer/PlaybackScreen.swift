import AVKit
import SwiftUI

/// Playback surface.
///
/// **tvOS**: AVKit's native trickplay bar owns the chrome. Retry /
/// Reload / Settings appear inside it as `transportBarCustomMenuItems`
/// (see `PlayerView`). No SwiftUI overlay on top — the player is
/// fullscreen, the Siri Remote Menu pops back to Home.
///
/// **iOS / iPadOS**: AVKit's chrome is off; we draw a back chevron at
/// top-left and three icon buttons (Retry / Reload / Settings) at
/// top-right since AVKit's iOS HUD has no extension API.
struct PlaybackScreen: View {
    @ObservedObject var vm: PlayerViewModel
    let onBack: () -> Void

    var body: some View {
        ZStack(alignment: .topLeading) {
            PlayerView(
                player: vm.player,
                onRetry: { vm.retry() },
                onReload: { vm.reload() },
                onMark911: { vm.mark911() },
                onOpenSettings: { vm.setSettingsOpen(true) },
                onFirstFrame: { at in vm.markFirstFrameRendered(at: at) },
                onDisplaySize: { size in vm.diagnostics.updateDisplaySize(size) }
            )
            .id(vm.playerEpoch)
            .ignoresSafeArea()
            .background(Color.black.ignoresSafeArea())
            .overlay(alignment: .trailing) {
                if vm.developerMode && !vm.settingsOpen {
                    DiagnosticHUD(vm: vm, diagnostics: vm.diagnostics)
                        .padding(.trailing, Space.s4)
                        .allowsHitTesting(false)
                        .accessibilityHidden(true)
                }
            }

            #if !os(tvOS)
            VStack {
                HStack(spacing: Space.s3) {
                    BackChevronButton {
                        vm.endSessionForUserBack()
                        onBack()
                    }
                        .accessibilityIdentifier("playback-back-button")
                        .help("Back to content list")
                    Spacer()
                    iconButton(systemName: "arrow.clockwise", help: "Retry: re-attempt the current playback from where it stopped (bumps attempt_id)") { vm.retry() }
                        .accessibilityIdentifier("playback-retry-button")
                    iconButton(systemName: "arrow.triangle.2.circlepath", help: "Reload: start a fresh play of the same content (new play_id)") { vm.reload() }
                        .accessibilityIdentifier("playback-reload-button")
                    iconButton(systemName: "exclamationmark.triangle.fill", help: "911: mark this moment as interesting for forensics — flags the session and pins the row in CH") { vm.mark911() }
                        .accessibilityIdentifier("playback-911-button")
                    iconButton(systemName: "gearshape", help: "Settings: codec/protocol/Advanced flags + server picker") { vm.setSettingsOpen(true) }
                        .accessibilityIdentifier("playback-settings-button")
                }
                .padding(Space.s4)
                Spacer()
            }
            #endif
        }
        .background(Color.black.ignoresSafeArea())
        #if os(tvOS)
        .onExitCommand {
            if !vm.settingsOpen {
                vm.endSessionForUserBack()
                onBack()
            }
        }
        #endif
    }

    #if !os(tvOS)
    /// Round icon button used by the iOS playback overlay. `help`
    /// powers SwiftUI's hover tooltip (iPad with mouse/trackpad/
    /// Pencil-hover, Mac Catalyst) AND the VoiceOver accessibility
    /// label — same string covers both so the operator never has to
    /// guess what an icon does. Issue #486.
    private func iconButton(systemName: String, help: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: systemName)
                .font(.system(size: 18, weight: .semibold))
                .foregroundColor(Tokens.fg)
                .padding(12)
                .background(Tokens.bgSoft)
                .clipShape(Circle())
        }
        .buttonStyle(.plain)
        .help(help)
        .accessibilityLabel(help)
    }
    #endif
}
