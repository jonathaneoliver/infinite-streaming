import SwiftUI
import Charts

struct TestingSessionView: View {
    @StateObject var viewModel: TestingSessionViewModel
    @ObservedObject var diagnostics: PlaybackDiagnostics
    var appLogs: [String]

    @State private var shapeRate: Double = 0
    @State private var shapeDelay: Double = 0
    @State private var shapeLoss: Double = 0

    @State private var faultTab: FaultTab = .segment
    @State private var templateMode: String = "sliders"
    @State private var templateMargin: Double = 0
    @State private var defaultStepSeconds: Double = 12
    @State private var patternSteps: [PatternStep] = []
    @State private var selectedGroupSessionId: String = ""
    @AppStorage("testing_session_show_session_details") private var showSessionDetails: Bool = true
    @AppStorage("testing_session_show_failure_controls") private var showFailureControls: Bool = true
    @AppStorage("testing_session_show_network_shaping") private var showNetworkShaping: Bool = true
    @AppStorage("testing_session_show_bitrate_chart") private var showBitrateChart: Bool = false
    @AppStorage("testing_session_show_group_controls") private var showGroupControls: Bool = true
    @AppStorage("testing_session_show_debug_log") private var showDebugLog: Bool = false
    @State private var templateModeOverride: String? = nil
    @State private var templateMarginOverride: Double? = nil
    @State private var defaultStepOverride: Double? = nil
    @State private var shapeRateOverride: Double? = nil
    @State private var shapeDelayOverride: Double? = nil
    @State private var shapeLossOverride: Double? = nil
    @State private var sliderOverrides: [String: Double] = [:]
    @State private var isHydrating: Bool = false
    @State private var shapeApplyTask: Task<Void, Never>? = nil
    @State private var bitrateAxisMaxMode: String = "auto"

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            groupControls
            sessionStats
            failureControls
            networkShaping
            bitrateChart
            debugLog
        }
        .onAppear {
            hydrateFromSession()
            viewModel.start()
        }
        .onDisappear {
            viewModel.stop()
        }
        .onChange(of: viewModel.session) { _ in
            hydrateFromSession()
        }
        .onChange(of: templateMode) { _ in
            regeneratePatternSteps(reset: true)
        }
        .onChange(of: templateMargin) { _ in
            regeneratePatternSteps(reset: false)
        }
        .onChange(of: defaultStepSeconds) { _ in
            regeneratePatternSteps(reset: false)
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Testing Session")
                .font(.title2)
            if let session = viewModel.session {
                Text("Session \(session.sessionId) · \(portDisplay(session)) · \(session.playerId)")
                    .font(.subheadline)
                    .foregroundColor(.secondary)
            } else {
                Text("No session yet")
                    .font(.subheadline)
                    .foregroundColor(.secondary)
            }
            if !viewModel.nftablesMessage.isEmpty {
                Text(viewModel.nftablesMessage)
                    .font(.caption)
                    .foregroundColor(.orange)
            }
        }
    }

    private var sessionStats: some View {
        cardHeader(isExpanded: $showSessionDetails) {
            HStack(alignment: .center) {
                Text("Session Details")
                Spacer()
                if let session = viewModel.session {
                    Text("M: \(session.masterManifestCount ?? 0) / Man: \(session.manifestCount ?? 0) / Seg: \(session.segmentCount ?? 0)")
                        .font(.caption)
                        .foregroundColor(.secondary)
                        .padding(.horizontal, 10)
                        .padding(.vertical, 4)
                        .background(Color(.secondarySystemBackground))
                        .clipShape(Capsule())
                }
            }
        } content: {
            if let session = viewModel.session {
                LazyVGrid(columns: [
                    GridItem(.adaptive(minimum: 180), spacing: 16)
                ], spacing: 14) {
                    self.statsCell("User Agent", session.userAgent)
                    self.statsCell("Player IP", session.playerIP)
                    self.statsCell("Port", portDisplay(session))
                    self.statsCell("Last Request", session.lastRequest)
                    self.statsCell("First Request", session.firstRequest)
                    self.statsCell("Session Duration", formatDuration(session.sessionDuration))
                    self.statsCell("Manifest URL", session.manifestURL)
                    self.statsCell("Master Manifest URL", session.masterManifestURL)
                    self.statsCell("Last Request URL", session.lastRequestURL)
                    self.statsCell("Measured Mbps", formatMbps(session))
                }
                let metrics = playerMetricsRows(session)
                if !metrics.isEmpty {
                    Divider().padding(.vertical, 4)
                    Text("Player Metrics")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    LazyVGrid(columns: [
                        GridItem(.adaptive(minimum: 180), spacing: 16)
                    ], spacing: 14) {
                        ForEach(metrics, id: \.0) { row in
                            self.statsCell(row.0, row.1)
                        }
                    }
                }
            } else {
                Text("No session yet").font(.caption).foregroundColor(.secondary)
            }
        }
    }

    private var failureControls: some View {
        card(title: "Fault Injection", isExpanded: $showFailureControls) {
            if let session = viewModel.session {
                Picker("Failure Type", selection: $faultTab) {
                    ForEach(FaultTab.allCases, id: \.self) { tab in
                        Text(tab.label).tag(tab)
                    }
                }
                .pickerStyle(.segmented)
                .hidden()
                .frame(height: 0)

                faultTabBar()

                VStack(alignment: .leading, spacing: 12) {
                    switch faultTab {
                    case .segment:
                        sectionHeader("Failure Type")
                        failureTypeChips(key: "segment_failure_type", options: FailureOptions.segmentTypes, session: session)
                        sectionHeader("Scope")
                        failureScopeChips(key: "segment_failure_urls", options: segmentScopeOptions(session), session: session)
                        sectionHeader("Mode")
                        modeMenuRow(key: "segment_failure_mode", options: FailureOptions.modeOptions, session: session)
                        rangeSlider("Consecutive", key: "segment_consecutive_failures", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                        rangeSlider("Frequency", key: "segment_failure_frequency", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                    case .manifest:
                        sectionHeader("Failure Type")
                        failureTypeChips(key: "manifest_failure_type", options: FailureOptions.baseTypes, session: session)
                        sectionHeader("Scope")
                        failureScopeChips(key: "manifest_failure_urls", options: manifestScopeOptions(session), session: session)
                        sectionHeader("Mode")
                        modeMenuRow(key: "manifest_failure_mode", options: FailureOptions.modeOptions, session: session)
                        rangeSlider("Consecutive", key: "manifest_consecutive_failures", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                        rangeSlider("Frequency", key: "manifest_failure_frequency", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                    case .master:
                        sectionHeader("Failure Type")
                        failureTypeChips(key: "master_manifest_failure_type", options: FailureOptions.baseTypes, session: session)
                        sectionHeader("Mode")
                        modeMenuRow(key: "master_manifest_failure_mode", options: FailureOptions.modeOptions, session: session)
                        rangeSlider("Consecutive", key: "master_manifest_consecutive_failures", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                        rangeSlider("Frequency", key: "master_manifest_failure_frequency", session: session, min: 0, max: 10, step: 1, format: "%.0f")
                case .transport:
                        sectionHeader("Fault Type")
                        transportRadioGroup(key: "transport_failure_type", options: FailureOptions.transportTypes, session: session)
                        sectionHeader("Mode")
                        transportRadioGroup(key: "transport_failure_mode", options: FailureOptions.transportModeOptions, session: session)
                        let transportRange = transportConsecutiveRange(session: session)
                        transportRangeRow(transportRange.label, key: "transport_consecutive_failures", session: session, min: transportRange.min, max: transportRange.max, step: transportRange.step, format: "%.0f")
                        transportRangeRow("Frequency (secs)", key: "transport_failure_frequency", session: session, min: 0, max: 60, step: 1, format: "%.0f")
                        sectionHeader("State")
                        infoRow("State", session.transportFaultActive ? "Active" : "Idle")
                        sectionHeader("Fault Counters")
                        infoRow("Fault Counters", "Drop \(session.transportFaultDropPackets) pkts · Reject \(session.transportFaultRejectPackets) pkts")
                    }
                }
            } else {
                Text("No session yet").font(.caption).foregroundColor(.secondary)
            }
        }
    }

    private var networkShaping: some View {
        card(title: "Network Shaping", isExpanded: $showNetworkShaping) {
            if let session = viewModel.session {
                let usePattern = templateMode != "sliders"
                compactSlider("Delay (ms)", value: $shapeDelay, range: 0...250, step: 5, format: "%.0f", onChange: { newValue in
                    if !isHydrating {
                        shapeDelayOverride = newValue
                    }
                }, onCommit: { _ in
                    if !isHydrating {
                        scheduleShapeApply()
                    }
                })
                compactSlider("Loss (%)", value: $shapeLoss, range: 0...10, step: 0.5, format: "%.1f", onChange: { newValue in
                    if !isHydrating {
                        shapeLossOverride = newValue
                    }
                }, onCommit: { _ in
                    if !isHydrating {
                        scheduleShapeApply()
                    }
                })
                compactSlider("Throughput (Mbps)", value: $shapeRate, range: 0...30, step: 0.1, format: "%.1f", onChange: { newValue in
                    if !isHydrating {
                        shapeRateOverride = newValue
                    }
                }, onCommit: { _ in
                    if !isHydrating {
                        scheduleShapeApply()
                    }
                })
                    .disabled(usePattern)
                    .opacity(usePattern ? 0.5 : 1.0)

                Divider().padding(.vertical, 6)

                patternHeaderRow()

                if usePattern {
                    let presets = shapingPresets(session)
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(patternSteps) { step in
                            patternStepRow(step, presets: presets)
                        }
                        HStack(spacing: 8) {
                            Button("Add Step") { addStep() }
                                .buttonStyle(.bordered)
                            Button("Clear") { patternSteps.removeAll() }
                                .buttonStyle(.bordered)
                        }
                        Button("Apply Pattern") {
                            Task { await applyPattern() }
                        }
                        .buttonStyle(.borderedProminent)
                    }
                }

            } else {
                Text("No session yet").font(.caption).foregroundColor(.secondary)
            }
        }
    }

    private var bitrateChart: some View {
        card(title: "Bitrate Chart", isExpanded: $showBitrateChart) {
            if let session = viewModel.session {
                TimelineView(.periodic(from: Date(), by: 1)) { context in
                    let now = context.date
                    let windowSeconds: TimeInterval = 300
                    let cutoff = now.addingTimeInterval(-windowSeconds)
                    let state = buildBitrateChartState(session: session, cutoff: cutoff, now: now)

                    BitrateChartPanel(state: state, cutoff: cutoff, now: now, axisMode: $bitrateAxisMaxMode)
                }
            } else {
                Text("No session yet").font(.caption).foregroundColor(.secondary)
            }
        }
    }

    private func buildBitrateChartState(session: SessionData, cutoff: Date, now: Date) -> BitrateChartState {
        let samples = viewModel.bandwidthSamples.filter { $0.timestamp >= cutoff }
        let actualSeries = samples.map {
            MetricSample(timestamp: $0.timestamp, value: $0.mbpsOutAvg > 0 ? $0.mbpsOutAvg : $0.mbpsOut)
        }
        let actual1sSeries = samples.compactMap { sample -> MetricSample? in
            guard sample.mbpsOut1s > 0 else { return nil }
            return MetricSample(timestamp: sample.timestamp, value: sample.mbpsOut1s)
        }
        let activeSeries = samples.compactMap { sample -> MetricSample? in
            guard sample.mbpsOutActive > 0 else { return nil }
            return MetricSample(timestamp: sample.timestamp, value: sample.mbpsOutActive)
        }
        let playerEstimate = diagnostics.playerEstimateSamples.filter { $0.timestamp >= cutoff }
        let renditionSeries = diagnostics.variantBitrateSamples.filter { $0.timestamp >= cutoff }
        let observedSeries = diagnostics.observedBitrateSamples.filter { $0.timestamp >= cutoff }
        let indicatedSeries = diagnostics.indicatedBitrateSamples.filter { $0.timestamp >= cutoff }
        let averageVideoSeries = diagnostics.averageVideoBitrateSamples.filter { $0.timestamp >= cutoff }
        let targetSeries = viewModel.limitSamples.filter { $0.timestamp >= cutoff }
        let targetRate = targetSeries.last?.value
        var maxCandidates: [Double] = []
        maxCandidates.append(contentsOf: actualSeries.map { $0.value })
        maxCandidates.append(contentsOf: actual1sSeries.map { $0.value })
        maxCandidates.append(contentsOf: activeSeries.map { $0.value })
        maxCandidates.append(contentsOf: playerEstimate.map { $0.value })
        maxCandidates.append(contentsOf: renditionSeries.map { $0.value })
        maxCandidates.append(contentsOf: observedSeries.map { $0.value })
        maxCandidates.append(contentsOf: indicatedSeries.map { $0.value })
        maxCandidates.append(contentsOf: averageVideoSeries.map { $0.value })
        maxCandidates.append(contentsOf: targetSeries.map { $0.value })
        let maxAutoRaw = maxCandidates.max() ?? 1
        let maxAuto = min(100, max(1, maxAutoRaw))
        let maxY: Double
        if bitrateAxisMaxMode == "auto" {
            maxY = maxAuto
        } else if let fixed = Double(bitrateAxisMaxMode) {
            maxY = fixed
        } else {
            maxY = maxAuto
        }
        let bufferSamples = diagnostics.bufferDepthSamples.filter { $0.timestamp >= cutoff }
        let liveOffsetSamples = diagnostics.liveOffsetSamples.filter { $0.timestamp >= cutoff }
        let bufferMax = min(60, max(5, max(bufferSamples.map { $0.value }.max() ?? 0, liveOffsetSamples.map { $0.value }.max() ?? 0)))
        return BitrateChartState(
            actualSeries: actualSeries,
            actual1sSeries: actual1sSeries,
            activeSeries: activeSeries,
            playerEstimate: playerEstimate,
            renditionSeries: renditionSeries,
            observedSeries: observedSeries,
            indicatedSeries: indicatedSeries,
            averageVideoSeries: averageVideoSeries,
            targetSeries: targetSeries,
            bufferSamples: bufferSamples,
            liveOffsetSamples: liveOffsetSamples,
            maxY: maxY,
            bufferMax: bufferMax,
            targetRate: targetRate
        )
    }

    private var groupControls: some View {
        card(title: "Group Controls", isExpanded: $showGroupControls) {
            if let session = viewModel.session {
                let groupId = session.groupId
                let groupSessions = groupId.isEmpty
                    ? [SessionData]()
                    : viewModel.allSessions.filter { $0.groupId == groupId }
                let otherSessions = groupSessions.filter { $0.sessionId != session.sessionId }
                let summary = otherSessions
                    .map { "Session \($0.sessionId) (Port \(portDisplay($0))) · \($0.playerId)" }
                    .sorted()
                    .joined(separator: " · ")
                HStack(spacing: 10) {
                    Image(systemName: "link")
                        .foregroundColor(.green)
                    Text(groupId.isEmpty || summary.isEmpty ? "Grouped with: No other sessions yet" : "Grouped with: \(summary)")
                        .font(.subheadline)
                        .foregroundColor(.primary)
                    if !groupId.isEmpty {
                        Text(groupId)
                            .font(.caption.bold())
                            .foregroundColor(Color(.systemBrown))
                            .padding(.horizontal, 8)
                            .padding(.vertical, 2)
                            .background(Color(.systemYellow).opacity(0.6))
                            .clipShape(Capsule())
                    }
                    Spacer()
                    Button("Ungroup") {
                        Task { await viewModel.unlinkSession() }
                    }
                    .buttonStyle(.bordered)
                    .disabled(groupId.isEmpty)
                }
                .padding(10)
                .background(Color.green.opacity(0.12))
                .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))

                let candidates = viewModel.allSessions.filter { $0.sessionId != session.sessionId }
                HStack(spacing: 12) {
                    Text(groupId.isEmpty ? "Add to group" : "Add to group")
                        .font(.subheadline)
                    Picker("Select session", selection: $selectedGroupSessionId) {
                        Text("Select session…").tag("")
                        ForEach(candidates) { item in
                            Text("Session \(item.sessionId)").tag(item.sessionId)
                        }
                    }
                    .disabled(candidates.isEmpty)
                    Button("Group") {
                        let targetId = selectedGroupSessionId
                        if !targetId.isEmpty {
                            Task { await viewModel.linkSessions(targetId: targetId) }
                        }
                    }
                    .buttonStyle(.bordered)
                }
            } else {
                Text("No session yet").font(.caption).foregroundColor(.secondary)
            }
        }
    }


    private var debugLog: some View {
        VStack(alignment: .leading, spacing: 6) {
            DisclosureGroup("Debug Log", isExpanded: $showDebugLog) {
                if !appLogs.isEmpty {
                    Text("App Logs")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    ForEach(Array(appLogs.suffix(8).enumerated()), id: \.offset) { _, line in
                        Text(line)
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                    Divider().padding(.vertical, 4)
                }
                if !viewModel.logs.isEmpty {
                    Text("Session Logs")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    ForEach(Array(viewModel.logs.suffix(8).enumerated()), id: \.offset) { _, line in
                        Text(line)
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                }
            }
            .font(.headline)
        }
    }

    private func statsRow(_ label: String, _ value: String) -> some View {
        HStack {
            Text(label).font(.caption).foregroundColor(.secondary)
            Spacer()
            Text(value).font(.caption)
        }
    }

    private func formatDuration(_ seconds: Double?) -> String {
        guard let seconds else { return "—" }
        let hrs = Int(seconds) / 3600
        let mins = (Int(seconds) % 3600) / 60
        let secs = Int(seconds) % 60
        return String(format: "%02d:%02d:%02d", hrs, mins, secs)
    }

    private func formatMbps(_ session: SessionData) -> String {
        let value = session.mbpsOutAvg ?? session.mbpsOut ?? 0
        return String(format: "%.2f Mbps", value)
    }

    private func formatSeconds3(_ value: Double?) -> String {
        guard let value else { return "—" }
        return String(format: "%.3fs", value)
    }

    private func formatMbpsValue(_ value: Double?) -> String {
        guard let value else { return "—" }
        return String(format: "%.2f Mbps", value)
    }

    private func formatPercentValue(_ value: Double?) -> String {
        guard let value else { return "—" }
        return String(format: "%.2f%%", value)
    }

    private func formatRateValue(_ value: Double?) -> String {
        guard let value else { return "—" }
        return String(format: "%.2fx", value)
    }

    private func metricString(_ session: SessionData, _ key: String) -> String {
        session[key]?.stringValue ?? "—"
    }

    private func metricDouble(_ session: SessionData, _ key: String) -> Double? {
        session[key]?.doubleValue
    }

    private func metricInt(_ session: SessionData, _ key: String) -> Int? {
        session[key]?.intValue
    }

    private func playerMetricsRows(_ session: SessionData) -> [(String, String)] {
        let rows: [(String, String)] = [
            ("Last Event", metricString(session, "player_metrics_last_event")),
            ("Trigger Type", metricString(session, "player_metrics_trigger_type")),
            ("Event Time", metricString(session, "player_metrics_event_time")),
            ("State", metricString(session, "player_metrics_state")),
            ("Position", formatSeconds3(metricDouble(session, "player_metrics_position_s"))),
            ("Playback Rate", formatRateValue(metricDouble(session, "player_metrics_playback_rate"))),
            ("Buffer Depth", formatSeconds3(metricDouble(session, "player_metrics_buffer_depth_s"))),
            ("Buffer End", formatSeconds3(metricDouble(session, "player_metrics_buffer_end_s"))),
            ("Seekable End", formatSeconds3(metricDouble(session, "player_metrics_seekable_end_s"))),
            ("Live Edge", formatSeconds3(metricDouble(session, "player_metrics_live_edge_s"))),
            ("Live Offset", formatSeconds3(metricDouble(session, "player_metrics_live_offset_s"))),
            ("Display Resolution", metricString(session, "player_metrics_display_resolution")),
            ("Video Resolution", metricString(session, "player_metrics_video_resolution")),
            ("First Frame Time", formatSeconds3(metricDouble(session, "player_metrics_video_first_frame_time_s"))),
            ("Video Start Time", formatSeconds3(metricDouble(session, "player_metrics_video_start_time_s"))),
            ("Video Bitrate", formatMbpsValue(metricDouble(session, "player_metrics_video_bitrate_mbps"))),
            ("Network Bitrate", formatMbpsValue(metricDouble(session, "player_metrics_network_bitrate_mbps"))),
            ("Video Quality", formatPercentValue(metricDouble(session, "player_metrics_video_quality_pct"))),
            ("Stalls", metricInt(session, "player_metrics_stall_count").map(String.init) ?? "—"),
            ("Stall Time", formatSeconds3(metricDouble(session, "player_metrics_stall_time_s"))),
            ("Last Stall Time", formatSeconds3(metricDouble(session, "player_metrics_last_stall_time_s"))),
            ("Last Error", metricString(session, "player_metrics_error")),
            ("Source", metricString(session, "player_metrics_source"))
        ]
        return rows.filter { $0.1 != "—" && !$0.1.isEmpty }
    }


    private func card<Content: View>(title: String, isExpanded: Binding<Bool>, @ViewBuilder content: @escaping () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            DisclosureGroup(title, isExpanded: isExpanded) {
                content()
            }
            .font(.headline)
        }
        .padding(12)
        .background(Color(.systemBackground))
        .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .stroke(Color(.separator), lineWidth: 1)
        )
    }

    private func cardHeader<Header: View, Content: View>(isExpanded: Binding<Bool>, @ViewBuilder header: @escaping () -> Header, @ViewBuilder content: @escaping () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            DisclosureGroup(isExpanded: isExpanded) {
                content()
            } label: {
                header()
            }
            .font(.headline)
        }
        .padding(12)
        .background(Color(.systemBackground))
        .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .stroke(Color(.separator), lineWidth: 1)
        )
    }

    private func statsCell(_ label: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label.uppercased())
                .font(.caption2)
                .foregroundColor(.secondary)
            Text(value.isEmpty ? "—" : value)
                .font(.callout)
                .lineLimit(3)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func compactSlider(_ title: String, value: Binding<Double>, range: ClosedRange<Double>, step: Double, format: String, onChange: ((Double) -> Void)? = nil, onCommit: ((Double) -> Void)? = nil) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(title).font(.caption)
                Spacer()
                Text(String(format: format, value.wrappedValue))
                    .font(.caption)
                    .foregroundColor(.secondary)
            }
            Slider(value: value, in: range, step: step, onEditingChanged: { editing in
                if !editing {
                    onCommit?(value.wrappedValue)
                }
            })
            .onChange(of: value.wrappedValue) { newValue in
                onChange?(newValue)
            }
        }
    }

    private func templateLabel(_ mode: String) -> String {
        switch mode {
        case "square_wave": return "▁▔ Square"
        case "ramp_up": return "↗ Ramp Up"
        case "ramp_down": return "↘ Ramp Down"
        case "pyramid": return "⛰ Pyramid"
        default: return "🎚 Sliders"
        }
    }

    private func portDisplay(_ session: SessionData) -> String {
        let port = session.xForwardedPortExternal.isEmpty ? session.xForwardedPort : session.xForwardedPortExternal
        return port.isEmpty ? "—" : port
    }

    private func hydrateFromSession() {
        guard let session = viewModel.session else { return }
        isHydrating = true
        defer { DispatchQueue.main.async { isHydrating = false } }

        let serverRate = session.raw["nftables_bandwidth_mbps"]?.doubleValue ?? 0
        let patternEnabled = isPatternEnabled(session)
        let targetRate = patternEnabled ? (chartTargetRate(session) ?? serverRate) : serverRate
        if let override = shapeRateOverride, !patternEnabled {
            if abs(override - serverRate) < 0.0001 {
                shapeRateOverride = nil
                shapeRate = serverRate
            } else {
                shapeRate = override
            }
        } else {
            if patternEnabled {
                shapeRateOverride = nil
            }
            shapeRate = targetRate
        }
        let serverDelay = session.raw["nftables_delay_ms"]?.doubleValue ?? 0
        if let override = shapeDelayOverride {
            if abs(override - serverDelay) < 0.0001 {
                shapeDelayOverride = nil
                shapeDelay = serverDelay
            } else {
                shapeDelay = override
            }
        } else {
            shapeDelay = serverDelay
        }
        let serverLoss = session.raw["nftables_packet_loss"]?.doubleValue ?? 0
        if let override = shapeLossOverride {
            if abs(override - serverLoss) < 0.0001 {
                shapeLossOverride = nil
                shapeLoss = serverLoss
            } else {
                shapeLoss = override
            }
        } else {
            shapeLoss = serverLoss
        }
        let rawMode = session.raw["nftables_pattern_template_mode"]?.stringValue ?? "sliders"
        let serverMode = FailureOptions.templateModes.contains(rawMode) ? rawMode : "sliders"
        templateMode = templateModeOverride ?? serverMode

        let rawMargin = session.raw["nftables_pattern_margin_pct"]?.doubleValue ?? 0
        let serverMargin = coerceValue(rawMargin, allowed: FailureOptions.templateMargins, fallback: 0)
        templateMargin = templateMarginOverride ?? serverMargin

        let rawStep = session.raw["nftables_pattern_default_step_seconds"]?.doubleValue ?? 12
        let serverStep = coerceValue(rawStep, allowed: FailureOptions.defaultStepSeconds, fallback: 12)
        defaultStepSeconds = defaultStepOverride ?? serverStep
        for key in rangeSliderKeys {
            if let override = sliderOverrides[key] {
                let serverValue = session.raw[key]?.doubleValue ?? 0
                if abs(override - serverValue) < 0.0001 {
                    sliderOverrides[key] = nil
                }
            }
        }
        let hasPatternOverride = templateModeOverride != nil || templateMarginOverride != nil || defaultStepOverride != nil
        if !(hasPatternOverride && templateMode != "sliders") {
            if let steps = session.raw["nftables_pattern_steps"]?.arrayValue {
                patternSteps = steps.compactMap { step in
                    guard let obj = step.objectValue else { return nil }
                    let rate = obj["rate_mbps"]?.doubleValue ?? 0
                    let duration = obj["duration_seconds"]?.doubleValue ?? 12
                    let enabled = obj["enabled"]?.boolValue ?? true
                    return PatternStep(rate: rate, duration: duration, enabled: enabled)
                }
            }
        }
        if patternSteps.isEmpty {
            if templateMode != "sliders" {
                let generated = buildTemplateSteps(session: session)
                if !generated.isEmpty {
                    patternSteps = generated
                } else {
                    patternSteps = [PatternStep(rate: shapeRate, duration: defaultStepSeconds, enabled: true)]
                }
            } else {
                patternSteps = [PatternStep(rate: shapeRate, duration: defaultStepSeconds, enabled: true)]
            }
        }
    }

    private var rangeSliderKeys: [String] {
        [
            "segment_consecutive_failures",
            "segment_failure_frequency",
            "manifest_consecutive_failures",
            "manifest_failure_frequency",
            "master_manifest_consecutive_failures",
            "master_manifest_failure_frequency",
            "transport_consecutive_failures",
            "transport_failure_frequency"
        ]
    }

    private func failurePicker(_ title: String, key: String, options: [FailureOption], session: SessionData) -> some View {
        let current = session.raw[key]?.stringValue ?? "none"
        return Picker(title, selection: Binding<String>(
            get: { current },
            set: { value in
                Task { await viewModel.applyPatch(set: [key: .string(value)], fields: [key]) }
            })) {
                ForEach(options, id: \.value) { option in
                    Text(option.text).tag(option.value)
                }
            }
    }

    private func sectionHeader(_ title: String) -> some View {
        Text(title)
            .font(.subheadline.weight(.semibold))
            .foregroundColor(.secondary)
    }

    private func faultTabBar() -> some View {
        HStack(spacing: 0) {
            ForEach(FaultTab.allCases, id: \.self) { tab in
                Button {
                    faultTab = tab
                } label: {
                    VStack(spacing: 6) {
                        Text(tab.label)
                            .font(.subheadline.weight(faultTab == tab ? .semibold : .regular))
                            .foregroundColor(faultTab == tab ? .accentColor : .secondary)
                            .frame(maxWidth: .infinity)
                        Rectangle()
                            .fill(faultTab == tab ? Color.accentColor : Color.clear)
                            .frame(height: 2)
                    }
                    .padding(.vertical, 8)
                }
                .buttonStyle(.plain)
            }
        }
        .background(
            Rectangle()
                .fill(Color(.separator))
                .frame(height: 1)
                .offset(y: 18)
        )
    }

    private func failureTypeChips(key: String, options: [FailureOption], session: SessionData) -> some View {
        let current = session.raw[key]?.stringValue ?? "none"
        return FlowLayout(spacing: 8) {
            ForEach(options, id: \.value) { option in
                chipButton(title: option.text, selected: current == option.value) {
                    Task { await viewModel.applyPatch(set: [key: .string(option.value)], fields: [key]) }
                }
            }
        }
    }

    private func failureScopeChips(key: String, options: [FailureScopeOption], session: SessionData) -> some View {
        let selected = Set((session.raw[key]?.arrayValue ?? []).compactMap { $0.stringValue })
        return FlowLayout(spacing: 8) {
            ForEach(options) { option in
                chipToggleButton(title: option.label, selected: selected.contains(option.value) || selected.isEmpty) {
                    var values = selected
                    if option.value == "All" {
                        values = ["All"]
                    } else {
                        if values.contains(option.value) {
                            values.remove(option.value)
                        } else {
                            values.insert(option.value)
                        }
                    }
                    let payload = values.map { JSONValue.string($0) }
                    Task { await viewModel.applyPatch(set: [key: .array(payload)], fields: [key]) }
                }
            }
        }
    }

    private func modeMenuRow(key: String, options: [FailureOption], session: SessionData) -> some View {
        let current = session.raw[key]?.stringValue ?? options.first?.value ?? ""
        return Picker("Mode", selection: Binding<String>(
            get: { current },
            set: { value in
                Task { await viewModel.applyPatch(set: [key: .string(value)], fields: [key]) }
            })) {
                ForEach(options, id: \.value) { option in
                    Text(option.text).tag(option.value)
                }
            }
            .labelsHidden()
            .pickerStyle(.menu)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func transportRadioGroup(key: String, options: [FailureOption], session: SessionData) -> some View {
        let current = session.raw[key]?.stringValue ?? options.first?.value ?? ""
        return FlowLayout(spacing: 12) {
            ForEach(options, id: \.value) { option in
                radioButton(title: option.text, selected: current == option.value) {
                    Task { await viewModel.applyPatch(set: [key: .string(option.value)], fields: [key]) }
                }
            }
        }
    }

    private func transportRangeRow(_ title: String, key: String, session: SessionData, min: Double, max: Double, step: Double, format: String) -> some View {
        let serverValue = session.raw[key]?.doubleValue ?? 0
        let value = sliderOverrides[key] ?? serverValue
        return HStack(spacing: 12) {
            Text(title)
                .font(.subheadline)
                .foregroundColor(.secondary)
                .frame(width: 140, alignment: .leading)
            Slider(value: Binding<Double>(
                get: { value },
                set: { newValue in
                    sliderOverrides[key] = newValue
                }), in: min...max, step: step, onEditingChanged: { editing in
                    if !editing {
                        let finalValue = sliderOverrides[key] ?? serverValue
                        if !isHydrating {
                            Task { await viewModel.applyPatch(set: [key: .number(finalValue)], fields: [key]) }
                        }
                    }
                })
            Text(String(format: format, value))
                .font(.subheadline)
                .foregroundColor(.secondary)
                .frame(width: 40, alignment: .trailing)
        }
    }

    private func infoRow(_ label: String, _ value: String) -> some View {
        HStack {
            Text(label.uppercased())
                .font(.caption2)
                .foregroundColor(.secondary)
            Spacer()
            Text(value.isEmpty ? "—" : value)
                .font(.caption)
                .foregroundColor(.secondary)
        }
        .padding(.vertical, 2)
    }

    private func radioButton(title: String, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 6) {
                ZStack {
                    Circle()
                        .stroke(selected ? Color.accentColor : Color(.separator), lineWidth: 1.5)
                        .frame(width: 16, height: 16)
                    if selected {
                        Circle()
                            .fill(Color.accentColor)
                            .frame(width: 8, height: 8)
                    }
                }
                Text(title)
                    .font(.subheadline)
                    .foregroundColor(.primary)
            }
            .padding(.vertical, 4)
        }
        .buttonStyle(.plain)
    }

    private func chipWrap<Item, Content: View>(options: [Item], minWidth: CGFloat, @ViewBuilder content: @escaping (Item) -> Content) -> some View {
        LazyVGrid(
            columns: [GridItem(.adaptive(minimum: minWidth), spacing: 8)],
            alignment: .leading,
            spacing: 8
        ) {
            ForEach(Array(options.enumerated()), id: \.offset) { _, option in
                content(option)
            }
        }
    }

    private func chipToggleButton(title: String, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 6) {
                if selected {
                    Image(systemName: "checkmark")
                        .font(.caption2)
                        .foregroundColor(.white)
                }
                Text(title)
                    .font(.caption)
                    .lineLimit(2)
                    .multilineTextAlignment(.center)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .foregroundColor(selected ? .white : .primary)
            .background(selected ? Color.accentColor : Color(.systemGray6))
            .clipShape(Capsule())
            .overlay(
                Capsule()
                    .stroke(selected ? Color.accentColor.opacity(0.7) : Color(.separator), lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
    }

    private func modePicker(_ title: String, key: String, session: SessionData) -> some View {
        let current = session.raw[key]?.stringValue ?? "failures_per_seconds"
        return Picker(title, selection: Binding<String>(
            get: { current },
            set: { value in
                Task { await viewModel.applyPatch(set: [key: .string(value)], fields: [key]) }
            })) {
                ForEach(FailureOptions.modeOptions, id: \.value) { option in
                    Text(option.text).tag(option.value)
                }
            }
    }

    private func transportModePicker(session: SessionData) -> some View {
        let current = session.raw["transport_failure_mode"]?.stringValue ?? "failures_per_seconds"
        return Picker("Transport Mode", selection: Binding<String>(
            get: { current },
            set: { value in
                Task { await viewModel.applyPatch(set: ["transport_failure_mode": .string(value)], fields: ["transport_failure_mode"]) }
            })) {
                ForEach(FailureOptions.transportModeOptions, id: \.value) { option in
                    Text(option.text).tag(option.value)
                }
            }
    }

    private func transportConsecutiveRange(session: SessionData) -> (label: String, min: Double, max: Double, step: Double) {
        let mode = (session.raw["transport_failure_mode"]?.stringValue ?? "failures_per_seconds").lowercased()
        if mode == "failures_per_packets" {
            return ("Consecutive (pkts)", 0, 500, 1)
        }
        return ("Consecutive (secs)", 0, 30, 1)
    }

    private func rangeSlider(_ title: String, key: String, session: SessionData, min: Double, max: Double, step: Double, format: String) -> some View {
        let serverValue = session.raw[key]?.doubleValue ?? 0
        let value = sliderOverrides[key] ?? serverValue
        return VStack(alignment: .leading) {
            HStack {
                Text(title)
                Spacer()
                Text(String(format: format, value))
            }
            Slider(value: Binding<Double>(
                get: { value },
                set: { newValue in
                    sliderOverrides[key] = newValue
                }), in: min...max, step: step, onEditingChanged: { editing in
                    if !editing {
                        let finalValue = sliderOverrides[key] ?? serverValue
                        if !isHydrating {
                            Task { await viewModel.applyPatch(set: [key: .number(finalValue)], fields: [key]) }
                        }
                    }
                })
        }
    }

    private func failureScope(_ title: String, key: String, options: [FailureScopeOption], session: SessionData) -> some View {
        let selected = Set((session.raw[key]?.arrayValue ?? []).compactMap { $0.stringValue })
        return VStack(alignment: .leading) {
            Text(title)
            ForEach(options) { option in
                Toggle(option.label, isOn: Binding<Bool>(
                    get: { selected.contains(option.value) || selected.isEmpty },
                    set: { _ in
                        var values = selected
                        if option.value == "All" {
                            values = ["All"]
                        } else {
                            if values.contains(option.value) {
                                values.remove(option.value)
                            } else {
                                values.insert(option.value)
                            }
                        }
                        let payload = values.map { JSONValue.string($0) }
                        Task { await viewModel.applyPatch(set: [key: .array(payload)], fields: [key]) }
                    }))
            }
        }
    }

    private func manifestScopeOptions(_ session: SessionData) -> [FailureScopeOption] {
        var items = [FailureScopeOption(value: "All", label: "All"), FailureScopeOption(value: "audio", label: "Audio")]
        session.manifestVariants.forEach { variant in
            let label = variantLabel(variant)
            items.append(FailureScopeOption(value: variant.url, label: label))
        }
        return items
    }

    private func segmentScopeOptions(_ session: SessionData) -> [FailureScopeOption] {
        var items = [FailureScopeOption(value: "All", label: "All"), FailureScopeOption(value: "audio", label: "Audio")]
        session.manifestVariants.forEach { variant in
            let label = variantLabel(variant)
            items.append(FailureScopeOption(value: variant.url, label: label))
        }
        return items
    }

    private func variantLabel(_ variant: ManifestVariant) -> String {
        let resolution = variant.resolution
        let height = resolution.split(separator: "x").last.map(String.init) ?? "unknown"
        let kbps = Int(Double(variant.bandwidth) / 1000.0)
        return "\(height)p/\(kbps)kbps"
    }

    private func addStep() {
        patternSteps.append(PatternStep(rate: shapeRate, duration: defaultStepSeconds, enabled: true))
    }

    private func removeStep(_ step: PatternStep) {
        patternSteps.removeAll { $0.id == step.id }
    }

    private func bindingForStep(_ step: PatternStep) -> PatternStepBinding {
        PatternStepBinding(
            rate: Binding(
                get: { step.rate },
                set: { newValue in
                    if let index = patternSteps.firstIndex(where: { $0.id == step.id }) {
                        patternSteps[index].rate = newValue
                    }
                }
            ),
            duration: Binding(
                get: { step.duration },
                set: { newValue in
                    if let index = patternSteps.firstIndex(where: { $0.id == step.id }) {
                        patternSteps[index].duration = newValue
                    }
                }
            ),
            enabled: Binding(
                get: { step.enabled },
                set: { newValue in
                    if let index = patternSteps.firstIndex(where: { $0.id == step.id }) {
                        patternSteps[index].enabled = newValue
                    }
                }
            )
        )
    }

    private func patternHeaderRow() -> some View {
        return VStack(alignment: .leading, spacing: 6) {
            ScrollView(.horizontal, showsIndicators: false) {
                patternChipGroup(title: "Pattern", options: FailureOptions.templateModes, selected: templateMode) { mode in
                    templateMode = mode
                    templateModeOverride = mode
                    if mode == "sliders" {
                        templateMarginOverride = nil
                        defaultStepOverride = nil
                    }
                }
                .padding(.vertical, 2)
            }
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    Text("Step Duration")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    ForEach(FailureOptions.defaultStepSeconds, id: \.self) { value in
                        chipButton(
                            title: "\(Int(value))s",
                            selected: defaultStepSeconds == value
                        ) {
                            defaultStepSeconds = value
                            defaultStepOverride = value
                        }
                    }
                }
                .padding(.vertical, 2)
            }
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    Text("Margin")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    ForEach(FailureOptions.templateMargins, id: \.self) { value in
                        chipButton(
                            title: value == 0 ? "Exact" : "+\(Int(value))%",
                            selected: templateMargin == value
                        ) {
                            templateMargin = value
                            templateMarginOverride = value
                        }
                    }
                }
                .padding(.vertical, 2)
            }
        }
    }

    private func patternChipGroup(title: String, options: [String], selected: String, onSelect: @escaping (String) -> Void) -> some View {
        HStack(spacing: 8) {
            Text(title)
                .font(.caption)
                .foregroundColor(.secondary)
            ForEach(options, id: \.self) { mode in
                chipButton(title: templateLabel(mode), selected: selected == mode) {
                    onSelect(mode)
                }
            }
        }
    }

    private func chipButton(title: String, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(title)
                .font(.caption)
                .padding(.horizontal, 12)
                .padding(.vertical, 6)
                .foregroundColor(selected ? .white : .primary)
                .background(selected ? Color.accentColor : Color(.systemGray6))
                .clipShape(Capsule())
                .overlay(
                    Capsule()
                        .stroke(selected ? Color.accentColor.opacity(0.7) : Color(.separator), lineWidth: 1)
                )
        }
        .buttonStyle(.plain)
    }

    private func shapingPresets(_ session: SessionData) -> [ShapingPreset] {
        let videoPresets = collectVideoShapingPresets(session.manifestVariants)
        let stallRiskThreshold = computeStallRiskThreshold(videoPresets)
        let presets = collectShapingBandwidthPresets(session.manifestVariants).map { preset in
            let isRisk = stallRiskThreshold != nil && (preset.rateMbps ?? 0) < (stallRiskThreshold ?? 0)
            return ShapingPreset(id: preset.id, label: preset.label, rateMbps: preset.rateMbps, risk: isRisk)
        }
        return [ShapingPreset(id: "custom", label: "Custom", rateMbps: nil, risk: false)] + presets
    }

    private func presetId(for rate: Double, presets: [ShapingPreset]) -> String {
        let match = presets.first { preset in
            guard let value = preset.rateMbps else { return false }
            return abs(value - rate) < 0.001
        }
        return match?.id ?? "custom"
    }

    private func patternStepRow(_ step: PatternStep, presets: [ShapingPreset]) -> some View {
        let binding = bindingForStep(step)
        let presetId = presetId(for: step.rate, presets: presets)
        let riskThreshold = computeStallRiskThreshold(collectVideoShapingPresets(viewModel.session?.manifestVariants ?? []))
        let isRisk = binding.enabled.wrappedValue && isRateRisk(step.rate, threshold: riskThreshold)
        return HStack(spacing: 8) {
            Text("Preset")
                .font(.caption)
                .foregroundColor(.secondary)
            Picker("Preset", selection: Binding<String>(
                get: { presetId },
                set: { newValue in
                    if let preset = presets.first(where: { $0.id == newValue }),
                       let rate = preset.rateMbps {
                        binding.rate.wrappedValue = rate
                    }
                })) {
                    ForEach(presets) { preset in
                        let label = preset.risk && preset.id != "custom" ? "⚠ \(preset.label)" : preset.label
                        Text(label).tag(preset.id)
                    }
                }
                .pickerStyle(.menu)
                .disabled(!binding.enabled.wrappedValue)
            Text("Mbps")
                .font(.caption)
                .foregroundColor(.secondary)
            TextField("Mbps", value: binding.rate, format: .number)
                .textFieldStyle(.roundedBorder)
                .frame(width: 80)
                .disabled(!binding.enabled.wrappedValue)
            Text("Time (s)")
                .font(.caption)
                .foregroundColor(.secondary)
            TextField("Time", value: binding.duration, format: .number)
                .textFieldStyle(.roundedBorder)
                .frame(width: 70)
                .disabled(!binding.enabled.wrappedValue)
            Toggle("Enabled", isOn: binding.enabled)
                .labelsHidden()
            Spacer()
            Button(action: { removeStep(step) }) {
                Image(systemName: "xmark.circle.fill")
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.plain)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 8)
        .background(isRisk ? Color.red.opacity(0.08) : Color(.systemGray6))
        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 8, style: .continuous)
                .stroke(isRisk ? Color.red.opacity(0.6) : Color(.separator), lineWidth: 1)
        )
    }

    private func collectShapingBandwidthPresets(_ playlists: [ManifestVariant]) -> [ShapingPreset] {
        let sorted = playlists.sorted { $0.bandwidth > $1.bandwidth }
        var seen = Set<String>()
        var presets: [ShapingPreset] = []
        for playlist in sorted {
            let bandwidth = Double(playlist.bandwidth)
            if bandwidth <= 0 { continue }
            let mbps = (bandwidth / 1_000_000.0)
            let rounded = (mbps * 1000).rounded() / 1000
            let key = String(format: "%.3f", rounded)
            if seen.contains(key) { continue }
            seen.insert(key)
            let resolution = playlist.resolution
            let height = resolution.split(separator: "x").last.map(String.init) ?? "unknown"
            let isAudio = playlist.url.lowercased().contains("audio")
            let heightLabel = isAudio ? "audio" : (height == "unknown" ? "unknown" : "\(height)p")
            let kbps = Int((bandwidth / 1000).rounded())
            let label = "\(heightLabel)/\(kbps)kbps"
            presets.append(ShapingPreset(id: key, label: label, rateMbps: rounded, risk: false))
        }
        return presets
    }

    private func collectVideoShapingPresets(_ playlists: [ManifestVariant]) -> [ShapingPreset] {
        let filtered = playlists.filter { !$0.url.lowercased().contains("audio") }
        let sorted = filtered.sorted { $0.bandwidth > $1.bandwidth }
        var seen = Set<String>()
        var presets: [ShapingPreset] = []
        for playlist in sorted {
            let bandwidth = Double(playlist.bandwidth)
            if bandwidth <= 0 { continue }
            let mbps = (bandwidth / 1_000_000.0)
            let rounded = (mbps * 1000).rounded() / 1000
            let key = String(format: "%.3f", rounded)
            if seen.contains(key) { continue }
            seen.insert(key)
            let resolution = playlist.resolution
            let height = resolution.split(separator: "x").last.map(String.init) ?? "unknown"
            let heightLabel = height == "unknown" ? "unknown" : "\(height)p"
            let kbps = Int((bandwidth / 1000).rounded())
            let label = "\(heightLabel)/\(kbps)kbps"
            presets.append(ShapingPreset(id: key, label: label, rateMbps: rounded, risk: false))
        }
        return presets.sorted { ($0.rateMbps ?? 0) < ($1.rateMbps ?? 0) }
    }

    private func computeStallRiskThreshold(_ presets: [ShapingPreset]) -> Double? {
        let values = presets.compactMap { $0.rateMbps }.filter { $0 > 0 }
        guard let minVideo = values.min() else { return nil }
        let threshold = minVideo * 1.1
        return (threshold * 1000).rounded() / 1000
    }

    private func chartTargetRate(_ session: SessionData) -> Double? {
        let patternEnabled = isPatternEnabled(session)
        let runtimeTarget = session.raw["nftables_pattern_rate_runtime_mbps"]?.doubleValue
        if patternEnabled, let runtimeTarget, runtimeTarget.isFinite, runtimeTarget >= 0 {
            return runtimeTarget
        }
        let stepIndexRaw = session.raw["nftables_pattern_step_runtime"]?.doubleValue
            ?? session.raw["nftables_pattern_step"]?.doubleValue
            ?? 0
        let stepIndex = Int(stepIndexRaw)
        if stepIndex > 0, let steps = session.raw["nftables_pattern_steps"]?.arrayValue, stepIndex <= steps.count {
            if let obj = steps[stepIndex - 1].objectValue,
               let rate = obj["rate_mbps"]?.doubleValue,
               rate.isFinite {
                return rate
            }
        }
        if let base = session.raw["nftables_bandwidth_mbps"]?.doubleValue {
            return base
        }
        return nil
    }

    private func isPatternEnabled(_ session: SessionData) -> Bool {
        if let boolValue = session.raw["nftables_pattern_enabled"]?.boolValue {
            return boolValue
        }
        if let intValue = session.raw["nftables_pattern_enabled"]?.intValue {
            return intValue == 1
        }
        let stringValue = session.raw["nftables_pattern_enabled"]?.stringValue ?? ""
        return stringValue.lowercased() == "true"
    }

    private func isCustomRate(_ rate: Double, presets: [ShapingPreset]) -> Bool {
        guard rate > 0 else { return false }
        let match = presets.first { preset in
            guard let value = preset.rateMbps else { return false }
            return abs(value - rate) < 0.001
        }
        return match == nil
    }

    private func isRateRisk(_ rate: Double, threshold: Double?) -> Bool {
        guard let threshold, rate > 0 else { return false }
        return rate < threshold
    }

    private func estimateAudioOverheadMbps(_ playlists: [ManifestVariant]) -> Double {
        var audioMbps = 0.0
        for playlist in playlists {
            if !playlist.url.lowercased().contains("audio") { continue }
            let bandwidth = Double(playlist.bandwidth)
            if bandwidth <= 0 { continue }
            audioMbps = max(audioMbps, bandwidth / 1_000_000.0)
        }
        let overhead = 0.05
        let total = audioMbps + overhead
        return (total * 1000).rounded() / 1000
    }

    private func regeneratePatternSteps(reset: Bool) {
        guard templateMode != "sliders" else {
            if patternSteps.isEmpty {
                patternSteps = [PatternStep(rate: shapeRate, duration: defaultStepSeconds, enabled: true)]
            }
            return
        }
        guard let session = viewModel.session else { return }
        let steps = buildTemplateSteps(session: session)
        guard !steps.isEmpty else { return }
        var merged = steps
        if !reset, !patternSteps.isEmpty {
            let presets = collectShapingBandwidthPresets(session.manifestVariants)
            let count = min(patternSteps.count, merged.count)
            for idx in 0..<count {
                let existing = patternSteps[idx]
                if existing.duration > 0 {
                    merged[idx].duration = existing.duration
                }
                merged[idx].enabled = existing.enabled
                if isCustomRate(existing.rate, presets: presets) {
                    merged[idx].rate = existing.rate
                }
            }
        }
        patternSteps = merged
        if let first = merged.first {
            shapeRate = first.rate
        }
    }

    private func buildTemplateSteps(session: SessionData) -> [PatternStep] {
        let videoPresets = collectVideoShapingPresets(session.manifestVariants)
        let baseRates = videoPresets.compactMap { $0.rateMbps }.filter { $0 >= 0 }
        guard !baseRates.isEmpty else { return [] }
        let minRate = baseRates.first ?? 0
        let maxRate = baseRates.last ?? 0
        let maxPlus50 = (maxRate * 1.5)

        var rates: [Double] = []
        switch templateMode {
        case "square_wave":
            rates = maxPlus50 == minRate ? [maxPlus50] : [maxPlus50, minRate]
        case "ramp_up":
            rates = baseRates
            if maxPlus50 > maxRate { rates.append(maxPlus50) }
        case "ramp_down":
            rates = baseRates.reversed()
            if maxPlus50 > maxRate { rates.insert(maxPlus50, at: 0) }
        case "pyramid":
            var ascending = baseRates
            if maxPlus50 > maxRate { ascending.append(maxPlus50) }
            rates = ascending + ascending.dropLast().reversed()
        default:
            rates = []
        }

        let marginPct = templateMargin
        let overhead = estimateAudioOverheadMbps(session.manifestVariants)
        let adjustRate: (Double) -> Double = { value in
            var adjusted = value * (1 + (marginPct / 100.0))
            adjusted += overhead
            if adjusted < 0 { adjusted = 0 }
            return (adjusted * 1000).rounded() / 1000
        }

        return rates
            .filter { $0 >= 0 }
            .map { PatternStep(rate: adjustRate($0), duration: defaultStepSeconds, enabled: true) }
    }

    private func applyPattern() async {
        let steps: [JSONValue] = patternSteps.map {
            .object([
                "rate_mbps": .number($0.rate),
                "duration_seconds": .number($0.duration),
                "enabled": .bool($0.enabled)
            ])
        }
        let segmentSeconds = inferSegmentDurationSeconds(viewModel.session)
        let pattern = PatternRequest(
            steps: steps,
            segment_duration_seconds: segmentSeconds,
            default_segments: max(1, defaultStepSeconds / max(1, segmentSeconds)),
            default_step_seconds: defaultStepSeconds,
            template_mode: templateMode,
            template_margin_pct: templateMargin,
            delay_ms: shapeDelay,
            loss_pct: shapeLoss
        )
        let port = viewModel.session?.xForwardedPortExternal.isEmpty == false ? (viewModel.session?.xForwardedPortExternal ?? "") : (viewModel.session?.xForwardedPort ?? "")
        guard !port.isEmpty else {
            viewModel.logAction("Pattern apply skipped: missing port")
            return
        }
        viewModel.logAction("Pattern apply: port=\(port) mode=\(templateMode) steps=\(patternSteps.count)")
        await viewModel.applyPattern(port: port, pattern: pattern)
    }

    private func scheduleShapeApply() {
        shapeApplyTask?.cancel()
        let rate = shapeRate
        let delay = shapeDelay
        let loss = shapeLoss
        shapeApplyTask = Task {
            try? await Task.sleep(nanoseconds: 250_000_000)
            await viewModel.applyShape(rate: rate, delay: delay, loss: loss)
        }
    }

    private func inferSegmentDurationSeconds(_ session: SessionData?) -> Double {
        let explicit = session?.raw["nftables_pattern_segment_duration_seconds"]?.doubleValue ?? 0
        if explicit > 0 { return explicit }
        let candidates = [session?.manifestURL ?? "", session?.masterManifestURL ?? "", session?.lastRequestURL ?? ""]
        for value in candidates {
            if let match = value.range(of: "(?:_|/)(\\d+)s(?:[._/?]|$)", options: .regularExpression) {
                let substring = value[match]
                let digits = substring.filter { $0.isNumber }
                if let number = Double(digits), number > 0 {
                    return number
                }
            }
        }
        return 1
    }

    private func coerceValue(_ value: Double, allowed: [Double], fallback: Double) -> Double {
        if allowed.contains(value) { return value }
        let nearest = allowed.min(by: { abs($0 - value) < abs($1 - value) })
        return nearest ?? fallback
    }
}

struct FailureOption {
    let value: String
    let text: String
}

struct FailureScopeOption: Identifiable {
    let id = UUID()
    let value: String
    let label: String
}

struct PatternStep: Identifiable {
    let id = UUID()
    var rate: Double
    var duration: Double
    var enabled: Bool
}

struct PatternStepBinding {
    let rate: Binding<Double>
    let duration: Binding<Double>
    let enabled: Binding<Bool>
}

struct FlowLayout: Layout {
    var spacing: CGFloat = 8

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let maxWidth = proposal.width ?? .infinity
        var width: CGFloat = 0
        var height: CGFloat = 0
        var rowWidth: CGFloat = 0
        var rowHeight: CGFloat = 0

        for subview in subviews {
            let size = subview.sizeThatFits(.unspecified)
            if rowWidth + size.width > maxWidth {
                width = max(width, rowWidth)
                height += rowHeight + spacing
                rowWidth = size.width
                rowHeight = size.height
            } else {
                rowWidth = rowWidth == 0 ? size.width : rowWidth + spacing + size.width
                rowHeight = max(rowHeight, size.height)
            }
        }

        width = max(width, rowWidth)
        height += rowHeight
        return CGSize(width: width, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX
        var y = bounds.minY
        var rowHeight: CGFloat = 0

        for subview in subviews {
            let size = subview.sizeThatFits(.unspecified)
            if x + size.width > bounds.maxX {
                x = bounds.minX
                y += rowHeight + spacing
                rowHeight = 0
            }
            subview.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(size))
            x += size.width + spacing
            rowHeight = max(rowHeight, size.height)
        }
    }
}

struct ShapingPreset: Identifiable, Hashable {
    let id: String
    let label: String
    let rateMbps: Double?
    let risk: Bool
}

struct BitrateChartState {
    let actualSeries: [MetricSample]
    let actual1sSeries: [MetricSample]
    let activeSeries: [MetricSample]
    let playerEstimate: [MetricSample]
    let renditionSeries: [MetricSample]
    let observedSeries: [MetricSample]
    let indicatedSeries: [MetricSample]
    let averageVideoSeries: [MetricSample]
    let targetSeries: [MetricSample]
    let bufferSamples: [MetricSample]
    let liveOffsetSamples: [MetricSample]
    let maxY: Double
    let bufferMax: Double
    let targetRate: Double?
}

struct BitrateChartPanel: View {
    let state: BitrateChartState
    let cutoff: Date
    let now: Date
    @Binding var axisMode: String
    private let colorActual = Color(red: 56.0 / 255.0, green: 189.0 / 255.0, blue: 248.0 / 255.0)
    private let colorActual1s = Color(red: 168.0 / 255.0, green: 85.0 / 255.0, blue: 247.0 / 255.0)
    private let colorActive = Color(red: 16.0 / 255.0, green: 185.0 / 255.0, blue: 129.0 / 255.0)
    private let colorPlayer = Color(red: 99.0 / 255.0, green: 102.0 / 255.0, blue: 241.0 / 255.0)
    private let colorRendition = Color(red: 239.0 / 255.0, green: 68.0 / 255.0, blue: 68.0 / 255.0)
    private let colorLimit = Color(red: 245.0 / 255.0, green: 158.0 / 255.0, blue: 11.0 / 255.0)
    private let colorBuffer = Color(red: 37.0 / 255.0, green: 99.0 / 255.0, blue: 235.0 / 255.0)
    private let colorObserved = Color(red: 20.0 / 255.0, green: 184.0 / 255.0, blue: 166.0 / 255.0)
    private let colorIndicated = Color(red: 232.0 / 255.0, green: 121.0 / 255.0, blue: 249.0 / 255.0)
    private let colorAvgVideo = Color(red: 100.0 / 255.0, green: 116.0 / 255.0, blue: 139.0 / 255.0)
    private let colorLiveOffset = Color(red: 245.0 / 255.0, green: 158.0 / 255.0, blue: 11.0 / 255.0)
    @State private var hiddenSeries: Set<String> = []
    @State private var hiddenBufferSeries: Set<String> = []

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Text("Bitrate Y Max")
                    .font(.caption)
                    .foregroundColor(.secondary)
                ForEach(["auto", "5", "10", "20", "40"], id: \.self) { option in
                    chipButton(title: option == "auto" ? "Auto" : "\(option) Mbps", selected: axisMode == option) {
                        axisMode = option
                    }
                }
            }
            Chart {
                ForEach(bitrateSeries) { series in
                    if !hiddenSeries.contains(series.label) {
                        ForEach(series.samples) { sample in
                            LineMark(
                                x: .value("Time", sample.timestamp),
                                y: .value(series.label, sample.value)
                            )
                            .foregroundStyle(by: .value("Series", series.label))
                            .lineStyle(StrokeStyle(lineWidth: series.lineWidth, dash: series.dash))
                            .interpolationMethod(series.interpolation)
                        }
                    }
                }
            }
            .chartLegend(.hidden)
            .chartForegroundStyleScale([
                "Actual (18s)": colorActual,
                "Actual (1s)": colorActual1s,
                "Active (18s)": colorActive,
                "Player est.": colorPlayer,
                "Rendition": colorRendition,
                "Observed": colorObserved,
                "Indicated": colorIndicated,
                "Avg Video": colorAvgVideo,
                "Limit": colorLimit
            ])
            .chartXScale(domain: cutoff...now)
            .chartYAxisLabel("Mbps", alignment: .leading)
            .chartYScale(domain: 0...state.maxY)
            .frame(height: 200)
            legendGrid(series: bitrateSeries, hiddenSet: $hiddenSeries)

            Chart {
                ForEach(bufferSeries) { series in
                    if !hiddenBufferSeries.contains(series.label) {
                        ForEach(series.samples) { sample in
                            LineMark(
                                x: .value("Time", sample.timestamp),
                                y: .value(series.label, sample.value)
                            )
                            .foregroundStyle(by: .value("Series", series.label))
                            .lineStyle(StrokeStyle(lineWidth: series.lineWidth, dash: series.dash))
                            .interpolationMethod(series.interpolation)
                        }
                    }
                }
            }
            .chartLegend(.hidden)
            .chartForegroundStyleScale([
                "Buffer Depth": colorBuffer,
                "Live Offset": colorLiveOffset
            ])
            .chartXScale(domain: cutoff...now)
            .chartYAxisLabel("Buffer Depth (s)", alignment: .leading)
            .chartYScale(domain: 0...state.bufferMax)
            .frame(height: 160)
            legendGrid(series: bufferSeries, hiddenSet: $hiddenBufferSeries)
        }
    }

    private func chipButton(title: String, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(title)
                .font(.caption)
                .padding(.horizontal, 12)
                .padding(.vertical, 6)
                .foregroundColor(selected ? .white : .primary)
                .background(selected ? Color.accentColor : Color(.systemGray6))
                .clipShape(Capsule())
                .overlay(
                    Capsule()
                        .stroke(selected ? Color.accentColor.opacity(0.7) : Color(.separator), lineWidth: 1)
                )
        }
        .buttonStyle(.plain)
    }

    private var bitrateSeries: [ChartSeries] {
        [
            ChartSeries(label: "Actual (18s)", samples: state.actualSeries, color: colorActual, lineWidth: 2, dash: [], interpolation: .linear),
            ChartSeries(label: "Actual (1s)", samples: state.actual1sSeries, color: colorActual1s, lineWidth: 1.5, dash: [], interpolation: .linear),
            ChartSeries(label: "Active (18s)", samples: state.activeSeries, color: colorActive, lineWidth: 1.5, dash: [], interpolation: .linear),
            ChartSeries(label: "Player est.", samples: state.playerEstimate, color: colorPlayer, lineWidth: 1.5, dash: [6, 4], interpolation: .linear),
            ChartSeries(label: "Rendition", samples: state.renditionSeries, color: colorRendition, lineWidth: 1.5, dash: [3, 3], interpolation: .linear),
            ChartSeries(label: "Observed", samples: state.observedSeries, color: colorObserved, lineWidth: 1.5, dash: [], interpolation: .linear),
            ChartSeries(label: "Indicated", samples: state.indicatedSeries, color: colorIndicated, lineWidth: 1.5, dash: [], interpolation: .linear),
            ChartSeries(label: "Avg Video", samples: state.averageVideoSeries, color: colorAvgVideo, lineWidth: 1.5, dash: [], interpolation: .linear),
            ChartSeries(label: "Limit", samples: state.targetSeries, color: colorLimit, lineWidth: 1.5, dash: [6, 4], interpolation: .stepStart)
        ].filter { series in
            if series.label == "Limit" {
                return state.targetRate != nil
            }
            return !series.samples.isEmpty
        }
    }

    private var bufferSeries: [ChartSeries] {
        [
            ChartSeries(label: "Buffer Depth", samples: state.bufferSamples, color: colorBuffer, lineWidth: 2, dash: [], interpolation: .linear),
            ChartSeries(label: "Live Offset", samples: state.liveOffsetSamples, color: colorLiveOffset, lineWidth: 1.5, dash: [6, 4], interpolation: .linear)
        ].filter { !$0.samples.isEmpty }
    }

    private func legendGrid(series: [ChartSeries], hiddenSet: Binding<Set<String>>) -> some View {
        let columns = [GridItem(.adaptive(minimum: 120), spacing: 8, alignment: .leading)]
        return LazyVGrid(columns: columns, alignment: .leading, spacing: 6) {
            ForEach(series) { item in
                let isHidden = hiddenSet.wrappedValue.contains(item.label)
                Button {
                    if isHidden {
                        hiddenSet.wrappedValue.remove(item.label)
                    } else {
                        hiddenSet.wrappedValue.insert(item.label)
                    }
                } label: {
                    HStack(spacing: 6) {
                        Circle()
                            .fill(item.color)
                            .frame(width: 8, height: 8)
                        Text(item.label)
                            .font(.caption)
                            .foregroundColor(.primary)
                            .strikethrough(isHidden, color: .secondary)
                            .opacity(isHidden ? 0.4 : 1.0)
                    }
                }
                .buttonStyle(.plain)
            }
        }
    }
}

private struct ChartSeries: Identifiable {
    let id: String
    let label: String
    let samples: [MetricSample]
    let color: Color
    let lineWidth: CGFloat
    let dash: [CGFloat]
    let interpolation: InterpolationMethod

    init(label: String, samples: [MetricSample], color: Color, lineWidth: CGFloat, dash: [CGFloat], interpolation: InterpolationMethod) {
        self.id = label
        self.label = label
        self.samples = samples
        self.color = color
        self.lineWidth = lineWidth
        self.dash = dash
        self.interpolation = interpolation
    }
}

enum FailureOptions {
    static let baseTypes: [FailureOption] = [
        FailureOption(value: "none", text: "None"),
        FailureOption(value: "404", text: "404"),
        FailureOption(value: "500", text: "500"),
        FailureOption(value: "403", text: "403"),
        FailureOption(value: "timeout", text: "Timeout"),
        FailureOption(value: "connection_refused", text: "Conn Refused"),
        FailureOption(value: "dns_failure", text: "DNS Failure"),
        FailureOption(value: "rate_limiting", text: "Rate Limit"),
        FailureOption(value: "request_connect_reset", text: "Request Connect Reset"),
        FailureOption(value: "request_connect_delayed", text: "Request Connect Delay"),
        FailureOption(value: "request_connect_hang", text: "Request Connect Hang"),
        FailureOption(value: "request_first_byte_reset", text: "Request Header Reset"),
        FailureOption(value: "request_first_byte_delayed", text: "Request Header Delay"),
        FailureOption(value: "request_first_byte_hang", text: "Request Header Hang"),
        FailureOption(value: "request_body_reset", text: "Request Body Reset"),
        FailureOption(value: "request_body_delayed", text: "Request Body Delay"),
        FailureOption(value: "request_body_hang", text: "Request Body Hang"),
    ]

    static let segmentTypes: [FailureOption] = baseTypes + [
        FailureOption(value: "corrupted", text: "Corrupted")
    ]

    static let transportTypes: [FailureOption] = [
        FailureOption(value: "none", text: "None"),
        FailureOption(value: "drop", text: "Drop (Blackhole)"),
        FailureOption(value: "reject", text: "Reject (RST)")
    ]

    static let modeOptions: [FailureOption] = [
        FailureOption(value: "requests", text: "Requests"),
        FailureOption(value: "seconds", text: "Seconds"),
        FailureOption(value: "failures_per_seconds", text: "Failures / Seconds")
    ]

    static let transportModeOptions: [FailureOption] = [
        FailureOption(value: "failures_per_seconds", text: "Seconds"),
        FailureOption(value: "failures_per_packets", text: "Packets / Seconds")
    ]

    static let templateModes: [String] = ["sliders", "square_wave", "ramp_up", "ramp_down", "pyramid"]
    static let templateMargins: [Double] = [0.0, 10.0, 25.0, 50.0]
    static let defaultStepSeconds: [Double] = [6.0, 12.0, 18.0, 24.0]
}

enum FaultTab: CaseIterable {
    case segment
    case manifest
    case master
    case transport

    var label: String {
        switch self {
        case .segment: return "Segment"
        case .manifest: return "Manifest"
        case .master: return "Master"
        case .transport: return "Transport"
        }
    }
}
