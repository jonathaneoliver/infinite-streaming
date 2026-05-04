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
    /// Fired exactly once per playback when the embedded `AVPlayerLayer`
    /// reports `isReadyForDisplay = true`. The closure receives the
    /// instant the flip was observed; the consumer (PlayerViewModel)
    /// is responsible for idempotency (resetting on item replace).
    var onFirstFrame: ((Date) -> Void)? = nil

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    func makeUIViewController(context: Context) -> AVPlayerViewController {
        let controller = AVPlayerViewController()
        controller.player = player
        controller.videoGravity = .resizeAspect
        #if os(tvOS)
        controller.showsPlaybackControls = true
        #else
        controller.showsPlaybackControls = false
        #endif
        context.coordinator.onFirstFrame = onFirstFrame
        context.coordinator.attach(to: controller, player: player)
        return controller
    }

    static func dismantleUIViewController(_ uiViewController: AVPlayerViewController, coordinator: Coordinator) {
        coordinator.detach()
    }

    func updateUIViewController(_ uiViewController: AVPlayerViewController, context: Context) {
        if uiViewController.player !== player {
            uiViewController.player = player
        }
        context.coordinator.onFirstFrame = onFirstFrame
        context.coordinator.attach(to: uiViewController, player: player)
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

    /// Holds the AVPlayerLayer KVO state across SwiftUI re-renders.
    /// AVPlayerViewController doesn't expose its embedded `AVPlayerLayer`
    /// directly, so we walk `view.layer.sublayers` to find it. The walk
    /// re-runs on player swap (Reload / Retry recreates AVPlayer) and on
    /// item replace, so the observer always tracks the live layer.
    final class Coordinator: NSObject {
        var onFirstFrame: ((Date) -> Void)?

        private weak var observedPlayer: AVPlayer?
        private weak var observedLayer: AVPlayerLayer?
        private var readyObservation: NSKeyValueObservation?
        private var sublayerObservation: NSKeyValueObservation?
        private var didReportForCurrentLayer: Bool = false

        func attach(to controller: AVPlayerViewController, player: AVPlayer) {
            // Player swap = brand-new AVPlayerLayer underneath.
            // Tear down the old observer and start fresh.
            if observedPlayer !== player {
                detachLayer()
                observedPlayer = player
            }
            // The AVPlayerLayer can be added to the controller's view
            // hierarchy after `makeUIViewController` returns. If we
            // can't find it yet, install a one-shot KVO on the parent
            // layer's `sublayers` and retry when it appears.
            // AVPlayerViewController on iOS doesn't reliably expose
            // its embedded AVPlayerLayer as a sublayer of view.layer
            // on initial mount — Apple uses a private rendering path.
            // After Reload (when the controller's view tree has
            // settled), the layer is sometimes findable. The fallback
            // synthesis in PlayerViewModel's $currentTime sink covers
            // cold-start where this KVO never fires. Debug prints
            // tagged `[FIRSTFRAME]` show which path won for each
            // mount — useful for spotting regressions / OS changes.
            if let layer = findAVPlayerLayer(in: controller.view.layer) {
                print("[FIRSTFRAME] attach: found AVPlayerLayer immediately depth=\(layerDepth(of: layer, in: controller.view.layer)) ready=\(layer.isReadyForDisplay)")
                installReadyObserver(on: layer)
            } else if sublayerObservation == nil {
                let topSubs = controller.view.layer.sublayers?.count ?? 0
                print("[FIRSTFRAME] attach: no AVPlayerLayer yet — topSubs=\(topSubs) — installing sublayer KVO")
                let parent = controller.view.layer
                sublayerObservation = parent.observe(\.sublayers, options: [.new]) { [weak self, weak controller] _, _ in
                    guard let self, let controller else { return }
                    if let layer = self.findAVPlayerLayer(in: controller.view.layer) {
                        print("[FIRSTFRAME] sublayer KVO fired: AVPlayerLayer appeared, ready=\(layer.isReadyForDisplay)")
                        self.installReadyObserver(on: layer)
                        self.sublayerObservation?.invalidate()
                        self.sublayerObservation = nil
                    }
                }
            }
        }

        private func layerDepth(of target: CALayer, in root: CALayer, depth: Int = 0) -> Int {
            if root === target { return depth }
            for sub in root.sublayers ?? [] {
                let d = layerDepth(of: target, in: sub, depth: depth + 1)
                if d >= 0 { return d }
            }
            return -1
        }

        func detach() {
            detachLayer()
            observedPlayer = nil
        }

        private func detachLayer() {
            readyObservation?.invalidate()
            readyObservation = nil
            sublayerObservation?.invalidate()
            sublayerObservation = nil
            observedLayer = nil
            didReportForCurrentLayer = false
        }

        private func installReadyObserver(on layer: AVPlayerLayer) {
            if observedLayer === layer { return }
            readyObservation?.invalidate()
            observedLayer = layer
            didReportForCurrentLayer = false
            // `.initial` lets us fire immediately if the layer is
            // already ready (e.g. SwiftUI re-rendered after first frame
            // already landed).
            readyObservation = layer.observe(\.isReadyForDisplay, options: [.new, .initial]) { [weak self] observed, _ in
                guard let self, observed.isReadyForDisplay, !self.didReportForCurrentLayer else { return }
                self.didReportForCurrentLayer = true
                let now = Date()
                if Thread.isMainThread {
                    self.onFirstFrame?(now)
                } else {
                    DispatchQueue.main.async { [weak self] in
                        self?.onFirstFrame?(now)
                    }
                }
            }
        }

        private func findAVPlayerLayer(in layer: CALayer) -> AVPlayerLayer? {
            if let avLayer = layer as? AVPlayerLayer { return avLayer }
            for sub in layer.sublayers ?? [] {
                if let found = findAVPlayerLayer(in: sub) { return found }
            }
            return nil
        }
    }
}

