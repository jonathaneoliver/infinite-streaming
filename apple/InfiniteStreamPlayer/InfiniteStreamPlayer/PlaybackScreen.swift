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
                onFirstFrame: { at in vm.markFirstFrameRendered(at: at) }
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
                    BackChevronButton { onBack() }
                    Spacer()
                    iconButton(systemName: "arrow.clockwise") { vm.retry() }
                    iconButton(systemName: "arrow.triangle.2.circlepath") { vm.reload() }
                    iconButton(systemName: "exclamationmark.triangle.fill") { vm.mark911() }
                    iconButton(systemName: "gearshape") { vm.setSettingsOpen(true) }
                }
                .padding(Space.s4)
                Spacer()
            }
            #endif
        }
        .background(Color.black.ignoresSafeArea())
        #if os(tvOS)
        .onExitCommand {
            if !vm.settingsOpen { onBack() }
        }
        #endif
    }

    #if !os(tvOS)
    private func iconButton(systemName: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: systemName)
                .font(.system(size: 18, weight: .semibold))
                .foregroundColor(Tokens.fg)
                .padding(12)
                .background(Tokens.bgSoft)
                .clipShape(Circle())
        }
        .buttonStyle(.plain)
    }
    #endif
}
