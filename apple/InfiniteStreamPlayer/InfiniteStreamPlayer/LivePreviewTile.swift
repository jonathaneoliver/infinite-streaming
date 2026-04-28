import AVFoundation
import AVKit
import SwiftUI

/// A small autoplay video card used on the Home screen.
///
/// Mirrors the Android `LivePreviewTile`:
///   - `active` and not appBackgrounded → owns an AVPlayer hardwired to
///     the 360p HLS rendition, muted, looping.
///   - `active=false` → static placeholder with poster + LIVE/QUEUED chip.
///
/// URL: `http://{host}:{contentPort}/go-live/{name}/playlist_6s_360p.m3u8`
///
/// The 6 s segment + 360p variant keeps decoder workload light. Apple
/// chips have plenty of headroom for many simultaneous decodes — unlike
/// Android TV's MediaCodec budget — so we don't do lease counting here.
struct LivePreviewTile: View {
    let content: ContentItem
    let server: ServerProfile
    /// When false, the tile renders the static thumbnail only and skips
    /// the AVPlayer decode — saves the per-tile decoder + network
    /// stream cost on devices with thermal / battery / bandwidth
    /// constraints. Bound to `Settings → Advanced → Preview video`.
    let videoEnabled: Bool
    let onTap: (ContentItem) -> Void

    /// Self-gating decode state. The tile only instantiates its
    /// AVPlayer when SwiftUI mounts the view (i.e. it's inside the
    /// LazyHStack's render window — visible or near-visible). Off-screen
    /// tiles get torn down via `.onDisappear` so we never hold more
    /// decoders than tiles currently on (or close to) screen.
    @State private var active: Bool = false

    /// Stable identity used to claim a slot from `DecodeBudget`. Must
    /// outlive view re-evaluations, so it's `@State` (not a computed
    /// property and not derived from `content.id`, which is shared
    /// across re-renders of the same logical tile).
    @State private var leaseID: UUID = UUID()
    /// True iff this tile currently holds a decoder slot. Re-evaluated
    /// on appear, when the budget's `revision` bumps (a sibling tile
    /// released a slot), and when `videoEnabled` flips.
    @State private var hasSlot: Bool = false

    @ObservedObject private var budget = DecodeBudget.shared

    /// True iff the AVPlayer should actually be instantiated right now.
    /// Requires three things: the view is mounted (`active`), the user
    /// has the preview-video toggle on, and the decode budget granted
    /// a slot (`hasSlot`). When any one is false the tile shows the
    /// static thumbnail.
    private var videoActive: Bool { active && videoEnabled && hasSlot }

    var body: some View {
        ZStack(alignment: .topLeading) {
            // Poster underneath the video so first-segment buffer doesn't
            // show black. Falls back to a flat fill when no thumbnail.
            ThumbnailBackground(server: server, content: content)

            if videoActive {
                MutedLoopingTile(server: server, content: content)
            }

            // Bottom gradient + title overlay so the name stays legible.
            LinearGradient(
                stops: [
                    .init(color: .clear, location: 0),
                    .init(color: .clear, location: 0.55),
                    .init(color: Color.black.opacity(0.85), location: 1.0),
                ],
                startPoint: .top, endPoint: .bottom
            )

            // LIVE / QUEUED badge. With preview-video off, the tile is
            // a static poster — drop the chip entirely so we don't
            // imply motion that isn't there. With preview-video on but
            // no decoder slot, show "QUEUED" so the user knows a slot
            // will be claimed when one frees up.
            if videoEnabled {
                HStack(spacing: 4) {
                    Circle()
                        .fill(videoActive ? Tokens.live : Tokens.fgFaint)
                        .frame(width: 8, height: 8)
                    Text(videoActive ? "LIVE" : "QUEUED")
                        .font(AppType.monoSm())
                        .foregroundColor(videoActive ? Tokens.live : Tokens.fgDim)
                }
                .padding(8)
            }

            VStack {
                Spacer()
                HStack {
                    Text(content.displayName)
                        .font(AppType.monoSm())
                        .foregroundColor(Tokens.fg)
                        .lineLimit(1)
                    Spacer()
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
            }
        }
        .frame(width: 220, height: 124)
        .clipShape(RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
        .background(Tokens.bgSoft)
        .cinematicFocus(cornerRadius: Radius.card) { focused in
            // Priority claim: the highlighted tile must always have
            // a decoder if any are configured. If the budget is full,
            // notifyFocused evicts a sibling.
            if focused, videoEnabled, active {
                hasSlot = budget.notifyFocused(leaseID)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture { onTap(content) }
        .onAppear {
            active = true
            if videoEnabled {
                hasSlot = budget.acquire(leaseID)
            }
        }
        .onDisappear {
            active = false
            budget.release(leaseID)
            hasSlot = false
        }
        .onChange(of: videoEnabled) { _, enabled in
            // Toggle in Settings flipped — claim or release immediately
            // so the change reflects without waiting for a remount.
            if enabled, active {
                hasSlot = budget.acquire(leaseID)
            } else {
                budget.release(leaseID)
                hasSlot = false
            }
        }
        .onChange(of: budget.revision) { _, _ in
            // Reconcile with the budget's authoritative state on every
            // revision bump. Three cases to handle:
            //   1. Budget granted us a slot we didn't know about (rare).
            //   2. We thought we had a slot but got evicted (a focused
            //      sibling preempted us via notifyFocused).
            //   3. We don't have a slot and never did — try to claim
            //      one now that something may have freed.
            guard videoEnabled, active else {
                hasSlot = false
                return
            }
            let actuallyHasGrant = budget.hasGrant(leaseID)
            if actuallyHasGrant != hasSlot {
                hasSlot = actuallyHasGrant
            }
            if !hasSlot {
                hasSlot = budget.acquire(leaseID)
            }
        }
    }
}

// MARK: - Inner pieces

private struct ThumbnailBackground: View {
    let server: ServerProfile
    let content: ContentItem

    var body: some View {
        if let url = StreamURLBuilder.thumbnailURL(server: server, path: content.thumbnailPath) {
            AsyncImage(url: url) { phase in
                switch phase {
                case .success(let image):
                    image.resizable().scaledToFill()
                default:
                    Tokens.bgCard
                }
            }
        } else {
            Tokens.bgCard
        }
    }
}

/// AVPlayer-backed muted/looping 360p tile. We use a `UIViewRepresentable`
/// rather than `VideoPlayer` so we can hide controls + force aspect-fill
/// + suppress focus on the embedded view (tvOS).
private struct MutedLoopingTile: UIViewRepresentable {
    let server: ServerProfile
    let content: ContentItem

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> UIView {
        let view = TileLayerView(frame: .zero)
        guard let url = StreamURLBuilder.tilePreviewURL(server: server, contentName: content.name) else {
            return view
        }
        let item = AVPlayerItem(url: url)
        let player = AVPlayer(playerItem: item)
        player.isMuted = true
        player.actionAtItemEnd = .none
        view.playerLayer.player = player
        view.playerLayer.videoGravity = .resizeAspectFill
        // Loop on end-of-item — live streams shouldn't actually end, but
        // segment churn during go-live restarts can produce a transient
        // EndTime. Restart from the live edge.
        let loop = NotificationCenter.default.addObserver(
            forName: .AVPlayerItemDidPlayToEndTime,
            object: item,
            queue: .main
        ) { [weak player] _ in
            player?.seek(to: .zero)
            player?.play()
        }
        context.coordinator.player = player
        context.coordinator.loopObserver = loop
        player.play()
        return view
    }

    func updateUIView(_ uiView: UIView, context: Context) {}

    static func dismantleUIView(_ uiView: UIView, coordinator: Coordinator) {
        coordinator.player?.pause()
        coordinator.player?.replaceCurrentItem(with: nil)
        if let o = coordinator.loopObserver { NotificationCenter.default.removeObserver(o) }
    }

    final class Coordinator {
        var player: AVPlayer?
        var loopObserver: NSObjectProtocol?
    }
}

private final class TileLayerView: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }
    var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
}
