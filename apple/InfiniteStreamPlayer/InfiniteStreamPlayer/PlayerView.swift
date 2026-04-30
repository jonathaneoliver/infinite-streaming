import AVKit
import SwiftUI

/// SwiftUI wrapper around `AVPlayerViewController`.
///
/// **tvOS** uses AVKit's native trickplay bar with Retry / Reload /
/// Settings appended as inline `transportBarCustomMenuItems` (passed as
/// `[UIAction]`, not wrapped in a `UIMenu`, so they merge alongside
/// audio / subtitle pickers without an extra submenu layer).
///
/// **iOS / iPadOS** keeps AVKit's chrome off — Apple's iOS HUD has no
/// equivalent extension API, so `PlaybackScreen` paints a top-right
/// icon row for the same three actions.
struct PlayerView: UIViewControllerRepresentable {
    let player: AVPlayer
    var onRetry: (() -> Void)? = nil
    var onReload: (() -> Void)? = nil
    var onMark911: (() -> Void)? = nil
    var onOpenSettings: (() -> Void)? = nil

    func makeUIViewController(context: Context) -> AVPlayerViewController {
        let controller = AVPlayerViewController()
        controller.player = player
        controller.videoGravity = .resizeAspect
        #if os(tvOS)
        controller.showsPlaybackControls = true
        #else
        controller.showsPlaybackControls = false
        #endif
        return controller
    }

    func updateUIViewController(_ uiViewController: AVPlayerViewController, context: Context) {
        if uiViewController.player !== player {
            uiViewController.player = player
        }
        #if os(tvOS)
        // Rebuild every update so each UIAction closure captures the
        // freshest callback (the parent passes closures that reference
        // @ObservedObject state).
        var actions: [UIAction] = []
        if let onRetry {
            actions.append(UIAction(
                title: "Retry",
                image: UIImage(systemName: "arrow.clockwise")
            ) { _ in onRetry() })
        }
        if let onReload {
            actions.append(UIAction(
                title: "Reload",
                image: UIImage(systemName: "arrow.triangle.2.circlepath")
            ) { _ in onReload() })
        }
        if let onMark911 {
            // 911 — capture a HAR snapshot of the moment for forensic
            // review later. Sits right of Reload to match the iOS
            // overlay layout.
            actions.append(UIAction(
                title: "911",
                image: UIImage(systemName: "exclamationmark.triangle.fill")
            ) { _ in onMark911() })
        }
        if let onOpenSettings {
            actions.append(UIAction(
                title: "Settings",
                image: UIImage(systemName: "gearshape")
            ) { _ in onOpenSettings() })
        }
        uiViewController.transportBarCustomMenuItems = actions
        #endif
    }
}

