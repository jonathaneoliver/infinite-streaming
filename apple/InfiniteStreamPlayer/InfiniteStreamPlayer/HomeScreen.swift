import AVFoundation
import SwiftUI

/// Home screen — Continue Watching hero on top, frequency-ordered live
/// preview row below. Mirrors the Android `HomeScreen` Composable.
///
/// Layout adapts to screen size:
///   - iPhone: vertical scroll with hero on top, preview row scrolls
///     horizontally beneath.
///   - iPad / tvOS: same vertical structure with larger surfaces.
struct HomeScreen: View {
    @ObservedObject var vm: PlayerViewModel
    let onPlay: () -> Void
    let onOpenServerPicker: () -> Void

    @Environment(\.horizontalSizeClass) private var hSizeClass

    /// True on iPhone in portrait (and iPad in slide-over). Tightens the
    /// outer padding + section spacing so the iPhone Home screen doesn't
    /// waste vertical real estate as black space below the LIVE row.
    /// iPad / tvOS keep the roomier `Space.s7` everywhere.
    private var isCompact: Bool { hSizeClass == .compact }

    private var sectionSpacing: CGFloat { isCompact ? Space.s5 : Space.s7 }
    private var hPadding: CGFloat { isCompact ? Space.s4 : Space.s7 }
    private var vPadding: CGFloat { isCompact ? Space.s4 : Space.s7 }
    private var titleSize: CGFloat { isCompact ? 30 : 38 }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: sectionSpacing) {
                header
                ContinueWatchingHero(vm: vm, onPlay: onPlay)
                LiveRow(vm: vm, onPlay: onPlay)
                Spacer()
            }
            .padding(.horizontal, hPadding)
            .padding(.vertical, vPadding)
        }
        .background(Tokens.bg.ignoresSafeArea())
        .onAppear { vm.fetchContentList() }
    }

    /// Top-of-screen header: brand label + serif title on the left,
    /// active server label below. Matches the ServerPicker header shape
    /// so the two screens read as siblings.
    private var header: some View {
        HStack(alignment: .top, spacing: Space.s4) {
            VStack(alignment: .leading, spacing: Space.s2) {
                Text("InfiniteStream")
                    .font(AppType.title(size: titleSize))
                    .foregroundColor(Tokens.fg)
                if let server = vm.activeServer {
                    Text(server.label)
                        .font(AppType.monoSm())
                        .foregroundColor(Tokens.fgFaint)
                        .lineLimit(1)
                }
            }
            Spacer()
        }
    }
}

// MARK: - Continue Watching hero

private struct ContinueWatchingHero: View {
    @ObservedObject var vm: PlayerViewModel
    let onPlay: () -> Void

    var body: some View {
        if let server = vm.activeServer,
           let item = heroItem {
            VStack(alignment: .leading, spacing: Space.s3) {
                Text(heroLabel)
                    .font(AppType.label())
                    .foregroundColor(Tokens.fgDim)

                ZStack(alignment: .bottomLeading) {
                    // Poster underneath the video as the boot frame so
                    // there's no black flash while the first segment
                    // buffers. Live video covers it once it starts.
                    if let url = StreamURLBuilder.thumbnailURL(server: server, path: item.thumbnailPathLarge ?? item.thumbnailPath) {
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

                    // Live video — same 360p H.264 path the small preview
                    // tiles use, so the hero shares the same decode
                    // budget. AspectFill so the live frame covers the
                    // hero area at any aspect ratio.
                    // .id keyed on item — when the user picks a
                    // different stream in Settings, SwiftUI tears down
                    // the old AVPlayer and instantiates a fresh one
                    // pointing at the new clip's 720p URL. Without
                    // .id, updateUIView would run with the new content
                    // value but the AVPlayer would keep the old URL.
                    //
                    // Skip while playbackActive — the main Playback
                    // screen is on top and we don't need a second
                    // AVPlayer running behind it. SwiftUI dismantles
                    // the UIViewRepresentable cleanly via the `if`
                    // omission, so the AVPlayer is fully gone.
                    if !vm.playbackActive {
                        HeroLiveVideo(server: server, content: item)
                            .id(item.id)
                    }

                    LinearGradient(
                        stops: [
                            .init(color: .clear, location: 0),
                            .init(color: .clear, location: 0.45),
                            .init(color: Color.black.opacity(0.85), location: 1.0),
                        ],
                        startPoint: .top, endPoint: .bottom
                    )

                    VStack(alignment: .leading, spacing: Space.s2) {
                        Text(item.displayName)
                            .font(AppType.title(size: 36))
                            .foregroundColor(Tokens.fg)
                            .lineLimit(2)
                        HStack(spacing: 4) {
                            Circle()
                                .fill(Tokens.live)
                                .frame(width: 8, height: 8)
                            Text("LIVE")
                                .font(AppType.monoSm())
                                .foregroundColor(Tokens.live)
                        }
                    }
                    .padding(Space.s5)
                }
                .frame(maxWidth: .infinity, minHeight: 280, maxHeight: 360)
                .clipShape(RoundedRectangle(cornerRadius: Radius.panel, style: .continuous))
                .cinematicFocus(cornerRadius: Radius.panel)
                .contentShape(Rectangle())
                .onTapGesture {
                    vm.setSelectedContent(item.name)
                    onPlay()
                }
            }
        }
    }

    /// Header label above the hero. Reflects whatever the hero is
    /// actually showing: NOW PLAYING when there's an explicit Stream
    /// selection, CONTINUE WATCHING for the last successfully-played
    /// clip, or FEATURED for the cold-start "first thing in catalogue"
    /// case.
    private var heroLabel: String {
        if !vm.selectedContent.isEmpty { return "NOW PLAYING" }
        if !vm.lastPlayed.isEmpty { return "CONTINUE WATCHING" }
        return "FEATURED"
    }

    /// Hero target — prefers the *currently selected* content (set by
    /// the Stream picker, the preview row, or auto-resume) and only
    /// falls back to `lastPlayed` (the most recent clip with a rendered
    /// first frame) and the catalogue's first entry otherwise. Picking
    /// a clip in Settings → Stream now updates the hero immediately,
    /// without waiting for it to actually start playing.
    private var heroItem: ContentItem? {
        let selectedClipId: String? = vm.selectedContent.isEmpty
            ? nil
            : ContentItem.deriveClipId(from: vm.selectedContent)
        if let cid = selectedClipId,
           let item = vm.previewContent.first(where: { $0.clipId == cid }) {
            return item
        }
        if !vm.lastPlayed.isEmpty,
           let item = vm.previewContent.first(where: { $0.clipId == ContentItem.deriveClipId(from: vm.lastPlayed) }) {
            return item
        }
        return vm.previewContent.first
    }
}

// MARK: - Live preview row

private struct LiveRow: View {
    @ObservedObject var vm: PlayerViewModel
    let onPlay: () -> Void

    /// Tile width — keep in sync with `LivePreviewTile.frame(width:)`.
    /// Used to compute the leading/trailing padding that lets any tile
    /// (including the first and last) scroll to dead-centre.
    private let tileWidth: CGFloat = 220
    /// Row height — must accommodate the 124-pt tile + the 1.04× focus
    /// scale-up + a small breathing margin. Fixed so GeometryReader has
    /// a finite vertical to fill.
    private let rowHeight: CGFloat = 144

    /// Currently centred clip ID — selected content takes priority,
    /// falling back to the last-played clip. Used to rotate the preview
    /// pool so the centred clip lands at the middle of the list with
    /// neighbours wrapping in from either end.
    private var centerClipId: String? {
        let key = vm.selectedContent.isEmpty ? vm.lastPlayed : vm.selectedContent
        guard !key.isEmpty else { return nil }
        return ContentItem.deriveClipId(from: key)
    }

    /// The preview pool, rotated so the centred clip sits at index
    /// `count / 2`. Without this, when the centred clip happens to be
    /// at the start of the catalogue the LIVE row appears with empty
    /// space to its left at first paint — `scrollTo(_, .center)` can't
    /// scroll past slot 0. Rotating shifts later items in front of the
    /// centred clip so the leading edge always has neighbours, even
    /// when the catalogue's natural order would put the centred clip
    /// first.
    private func arrangedPool() -> [ContentItem] {
        let pool = vm.previewPool()
        guard pool.count > 1, let target = centerClipId else { return pool }
        guard let idx = pool.firstIndex(where: { $0.clipId == target }) else { return pool }
        let centerIdx = pool.count / 2
        let shift = (centerIdx - idx + pool.count) % pool.count
        if shift == 0 { return pool }
        return Array(pool.suffix(shift)) + Array(pool.prefix(pool.count - shift))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Space.s3) {
            Text("LIVE")
                .font(AppType.label())
                .foregroundColor(Tokens.fgDim)
            GeometryReader { geo in
                // Pad both ends so scrollTo(_, anchor: .center) can
                // resolve cleanly for any tile — including slot 0 and
                // the last slot, which would otherwise clamp to the
                // viewport edge instead of centring.
                let edgePad = max(0, (geo.size.width - tileWidth) / 2)
                ScrollViewReader { proxy in
                    ScrollView(.horizontal, showsIndicators: false) {
                        // LazyHStack so off-screen tiles never instantiate.
                        // Each tile self-gates its AVPlayer via `.onAppear` /
                        // `.onDisappear` — no decode work happens for clips
                        // off the left or right of the visible window.
                        // `.focusSection()` (tvOS) keeps D-pad-Right/Left
                        // contained inside the row so the focus engine
                        // doesn't bail out into the underlying VStack.
                        LazyHStack(spacing: Space.s4) {
                            if let server = vm.activeServer {
                                ForEach(Array(arrangedPool().enumerated()), id: \.element.id) { _, item in
                                    LivePreviewTile(
                                        content: item,
                                        server: server,
                                        // Disable preview AVPlayers entirely while
                                        // the main Playback screen is up — see the
                                        // playbackActive comment on PlayerViewModel
                                        // (issue #348). Falls back to static
                                        // thumbnail; SwiftUI dismantles the
                                        // MutedLoopingTile UIViewRepresentable so
                                        // the AVPlayer is gone, not just paused.
                                        videoEnabled: vm.previewVideoSlots > 0 && !vm.playbackActive
                                    ) { tapped in
                                        vm.setSelectedContent(tapped.name)
                                        onPlay()
                                    }
                                    .id(item.id)
                                }
                            }
                        }
                        .padding(.horizontal, edgePad)
                        .padding(.vertical, 4)
                    }
                    .tvFocusSection()
                    .onAppear { centerOnCurrent(proxy: proxy) }
                    .onChange(of: vm.selectedContent) { _, _ in centerOnCurrent(proxy: proxy) }
                    .onChange(of: vm.lastPlayed) { _, _ in centerOnCurrent(proxy: proxy) }
                }
            }
            .frame(height: rowHeight)
        }
    }

    /// Scroll the live row so the currently-selected (or last-played)
    /// clip is centred — matched by `clipId` so the row stays anchored
    /// even when the tile shows the codec-agnostic H.264 variant and
    /// the user's selection is the HEVC re-encoding of the same clip.
    private func centerOnCurrent(proxy: ScrollViewProxy) {
        guard let targetClipId = centerClipId else { return }
        guard let target = arrangedPool().first(where: { $0.clipId == targetClipId }) else { return }
        // Defer one runloop so the LazyHStack has measured before we
        // ask SwiftUI to scroll — without this the .center anchor can
        // resolve before the tiles have laid out and end up at slot 0.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.05) {
            withAnimation(.easeOut(duration: 0.25)) {
                proxy.scrollTo(target.id, anchor: .center)
            }
        }
    }
}

// MARK: - Hero live video

/// AVPlayer-backed muted/looping 360p tile — same decode-cost path as
/// `LivePreviewTile`'s inner player, scaled to the hero's larger surface.
/// Hits the API port directly (no go-proxy in front), no `player_id`.
private struct HeroLiveVideo: UIViewRepresentable {
    let server: ServerProfile
    let content: ContentItem

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> UIView {
        let view = HeroLayerView(frame: .zero)
        // 720p variant — the hero surface is the visual centerpiece of
        // Home, so we trade the extra decode cost for the higher
        // resolution. Tiles stay at 360p for budget reasons.
        guard let url = StreamURLBuilder.tilePreviewURL(server: server, contentName: content.name, resolution: 720) else {
            return view
        }
        let item = AVPlayerItem(url: url)
        let player = AVPlayer(playerItem: item)
        player.isMuted = true
        player.actionAtItemEnd = .none
        view.playerLayer.player = player
        view.playerLayer.videoGravity = .resizeAspectFill
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

private final class HeroLayerView: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }
    var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
}
