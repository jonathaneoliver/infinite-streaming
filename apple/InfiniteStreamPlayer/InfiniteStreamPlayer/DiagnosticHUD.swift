import SwiftUI

/// Mid-right semi-transparent diagnostic readout. Gated on Developer
/// mode in `PlaybackScreen`. Pure formatting over the live `@Published`
/// fields on `PlaybackDiagnostics` — no new collection, no polling.
///
/// Companion to the Android `DiagnosticHud` composable; field list and
/// units must stay in lockstep so an operator can read either readout
/// during a cross-platform soak run.
struct DiagnosticHUD: View {
    @ObservedObject var vm: PlayerViewModel
    // SwiftUI's @ObservedObject only tracks changes on the top-level
    // object, not nested ObservableObjects. Most HUD fields live on
    // PlaybackDiagnostics (nested), so we observe it explicitly here
    // — without this, the HUD only redraws when one of vm's own
    // @Published fields (e.g. profileShiftCount) ticks, leaving live
    // counters like buffer / offset / bitrates frozen between events.
    @ObservedObject var diagnostics: PlaybackDiagnostics

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            row("STATE", stateText)
            // NET / AVG NET / VIDEO source fields match what we PATCH:
            //   NET     → player_metrics_network_bitrate_mbps     (LocalHTTPProxy per-chunk wire rate; nil during idle)
            //   AVG NET → player_metrics_avg_network_bitrate_mbps (AVPlayer session-wide observed throughput)
            //   VIDEO   → player_metrics_video_bitrate_mbps       (variant's indicated bitrate)
            row("NET", mbpsText(diagnostics.networkBitrate))
            row("AVG NET", mbpsText(diagnostics.observedBitrate))
            row("VIDEO", mbpsText(diagnostics.indicatedBitrate))
            row("RES", resolutionText)
            row("BUFFER", secondsText(diagnostics.bufferDepth))
            row("OFFSET", offsetText)
            row("SHIFTS", String(vm.profileShiftCount))
            row("STALLS", stallText)
            row("DROPPED", framesText(diagnostics.droppedVideoFrames))
        }
        .padding(.horizontal, Space.s3)
        .padding(.vertical, Space.s2)
        .frame(width: 240, alignment: .leading)
        .background(
            RoundedRectangle(cornerRadius: Radius.row, style: .continuous)
                .fill(Color.black.opacity(0.45))
        )
    }

    private func row(_ label: String, _ value: String) -> some View {
        HStack(spacing: Space.s2) {
            Text(label)
                .font(AppType.monoSm())
                .foregroundColor(Tokens.diag)
                .frame(width: 72, alignment: .leading)
            Text(value)
                .font(AppType.monoSm())
                .foregroundColor(Tokens.fg)
                .lineLimit(1)
                .truncationMode(.tail)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    // MARK: - Field formatters

    private var stateText: String {
        let s = diagnostics.state.uppercased()
        let reason = diagnostics.waitingReason
        return reason.isEmpty ? s : "\(s) (\(reason))"
    }

    private var resolutionText: String {
        sizeText(diagnostics.videoWidth, diagnostics.videoHeight)
    }

    private var offsetText: String {
        if let wallClock = diagnostics.playheadWallClock {
            let trueOffset = Date().timeIntervalSince(wallClock)
            return String(format: "%.1fs", trueOffset)
        }
        return secondsText(diagnostics.liveOffset)
    }

    private var stallText: String {
        let n = diagnostics.stallCount
        if n == 0 { return "0" }
        return String(format: "%d (last %.1fs)", n, diagnostics.lastStallDurationSeconds)
    }

    private func mbpsText(_ bps: Double?) -> String {
        guard let bps, bps > 0 else { return "—" }
        return String(format: "%.2f Mbps", bps / 1_000_000)
    }

    private func secondsText(_ s: Double?) -> String {
        guard let s else { return "—" }
        return String(format: "%.1fs", s)
    }

    private func framesText(_ f: Double?) -> String {
        guard let f else { return "—" }
        return String(format: "%.0f", f)
    }

    private func sizeText(_ w: Double?, _ h: Double?) -> String {
        guard let w, let h, w > 0, h > 0 else { return "—" }
        return "\(Int(w))×\(Int(h))"
    }
}
