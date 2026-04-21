import SafariServices
import SwiftUI
import Charts

enum ServerEnvironment: String, CaseIterable, Identifiable {
    case dev
    case release
    case ubuntu

    var id: String { rawValue }

    var label: String {
        switch self {
        case .dev: return "Dev (40000)"
        case .release: return "Release (30000)"
        case .ubuntu: return "Ubuntu (21000)"
        }
    }

    var host: String {
        switch self {
        case .dev: return "100.111.190.54"
        case .release: return "infinitestreaming.jeoliver.com"
        case .ubuntu: return "jonathanoliver-ubuntu.local"
        }
    }

    var contentPort: String {
        switch self {
        case .dev: return "40000"
        case .release: return "30000"
        case .ubuntu: return "21000"
        }
    }

    var playbackPort: String {
        switch self {
        case .dev: return "40081"
        case .release: return "30081"
        case .ubuntu: return "21081"
        }
    }
}

@MainActor
struct ContentView: View {
    @StateObject private var viewModel: PlaybackViewModel
    @StateObject private var testingViewModel: TestingSessionViewModel
    @Environment(\.openURL) private var openURL
    @State private var safariURL: URL?
    @State private var showSafariView = false
    @State private var baseURLText: String = ""
    @State private var protocolSelection: ProtocolOption = .hls
    @State private var segmentSelection: SegmentOption = .s6
    @State private var codecSelection: CodecOption = .h264
    @AppStorage("server_environment") private var serverEnvironmentRaw: String = ServerEnvironment.dev.rawValue
    @State private var showContentPicker = false

    init() {
        let playback = PlaybackViewModel()
        _viewModel = StateObject(wrappedValue: playback)
        let saved = UserDefaults.standard.string(forKey: "server_environment") ?? ServerEnvironment.dev.rawValue
        let env = ServerEnvironment(rawValue: saved) ?? .dev
        let controlURL = URL(string: "http://\(env.host):\(env.contentPort)") ?? URL(string: "http://localhost:40000")!
        _testingViewModel = StateObject(wrappedValue: TestingSessionViewModel(
            playerId: playback.playerId,
            controlBaseURL: controlURL,
            onRemoteRestartRequested: { reason in
                playback.restartPlayback(reason: reason)
            }
        ))
    }

    var body: some View {
        ScrollView(.vertical, showsIndicators: true) {
            VStack(alignment: .leading, spacing: 12) {
                Text("InfiniteStream Player")
                    .font(.title)

                HStack(spacing: 12) {
                    Button("Retry Fetch") {
                        viewModel.logAction("Retry Fetch")
                        applyEnvironment()
                        viewModel.retryFetch()
                    }
                    .buttonStyle(.bordered)
                    Button("Restart Playback") {
                        viewModel.logAction("Restart Playback")
                        applyEnvironment()
                        viewModel.restartPlayback()
                    }
                    .buttonStyle(.bordered)
                    Button("Reload Page") {
                        viewModel.logAction("Reload Page")
                        Task {
                            applyEnvironment()
                            await viewModel.reloadPage()
                        }
                    }
                    .buttonStyle(.bordered)
                    Spacer()
                    HStack(spacing: 6) {
                        Toggle("", isOn: $viewModel.prefer4kNative)
                            .labelsHidden()
                            .toggleStyle(.switch)
                        Text("Allow 4K")
                    }
                    HStack(spacing: 6) {
                        Toggle("", isOn: $viewModel.autoRecoveryEnabled)
                            .labelsHidden()
                            .toggleStyle(.switch)
                        Text("Auto-Recovery")
                    }
                    HStack(spacing: 6) {
                        Toggle("", isOn: $viewModel.localProxyEnabled)
                            .labelsHidden()
                            .toggleStyle(.switch)
                        Text("Local Proxy")
                    }
                }

                PlayerView(player: viewModel.player)
                    .aspectRatio(16.0 / 9.0, contentMode: .fit)
                    .frame(maxWidth: .infinity)
                    .background(
                        GeometryReader { geo in
                            Color.clear
                                .onAppear {
                                    viewModel.diagnostics.updateDisplaySize(geo.size)
                                    DispatchQueue.main.async {
                                        viewModel.diagnostics.updateDisplaySize(geo.size)
                                    }
                                }
                                .onChange(of: geo.size) { newSize in
                                    viewModel.diagnostics.updateDisplaySize(newSize)
                                }
                        }
                    )
                    .background(Color.black)
                    .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
                    .padding(.horizontal, -16)

                playbackDiagnosticsGrid

                VStack(alignment: .leading, spacing: 8) {
                    Text("Content Control")
                        .font(.headline)

                    Picker("Server", selection: $serverEnvironmentRaw) {
                        ForEach(ServerEnvironment.allCases) { env in
                            Text(env.label).tag(env.rawValue)
                        }
                    }
                    .pickerStyle(.segmented)
                    .onChange(of: serverEnvironmentRaw) { _ in
                        applyEnvironment()
                        baseURLText = viewModel.baseURLString
                    }

                    TextField("Base URL", text: $baseURLText)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled(true)
                        .onSubmit {
                            viewModel.baseURLString = baseURLText
                        }

                    HStack(spacing: 8) {
                        Picker("Protocol", selection: $protocolSelection) {
                            ForEach(ProtocolOption.allCases) { option in
                                Text(option.label)
                                    .tag(option)
                                    .foregroundColor(option == .dash ? .secondary : .primary)
                                    .opacity(option == .dash ? 0.4 : 1.0)
                                    .disabled(option == .dash)
                            }
                        }
                        Picker("Segment", selection: $segmentSelection) {
                            ForEach(SegmentOption.allCases) { option in
                                Text(option.label).tag(option)
                            }
                        }
                        Picker("Codec", selection: $codecSelection) {
                            ForEach(CodecOption.allCases) { option in
                                Text(option.label).tag(option)
                            }
                        }
                        Button {
                            showContentPicker = true
                        } label: {
                            let display = viewModel.selectedContent.isEmpty ? "Select content" : viewModel.selectedContent
                            Text(display)
                                .lineLimit(1)
                        }
                        .buttonStyle(.bordered)
                    }
                    .controlSize(.small)

                    if !viewModel.lastMasterRequestLine.isEmpty {
                        Text(viewModel.lastMasterRequestLine)
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                }
                .font(.caption)

                if URL(string: viewModel.baseURLString) != nil {
                    TestingSessionView(viewModel: testingViewModel, diagnostics: viewModel.diagnostics, appLogs: viewModel.logLines)
                }

            }
            .padding()
        }
        .task {
            applyEnvironment()
            await viewModel.refreshContentList()
            baseURLText = viewModel.baseURLString
            protocolSelection = viewModel.protocolOption == .dash ? .hls : viewModel.protocolOption
            segmentSelection = viewModel.segmentOption
            codecSelection = viewModel.codecOption
            if let first = viewModel.availableContent.first?.name {
                if viewModel.selectedContent != first {
                    viewModel.selectedContent = first
                } else {
                    viewModel.logAction("Auto-play default content: \(first)")
                    viewModel.applySelection()
                    viewModel.play()
                }
            }
        }
        .onChange(of: viewModel.selectedContent) { _ in
            applyEnvironment()
            viewModel.logAction("Selected content: \(viewModel.selectedContent)")
            viewModel.applySelection()
            viewModel.play()
        }
        .onChange(of: protocolSelection) { value in
            if value == .dash {
                protocolSelection = .hls
                viewModel.protocolOption = .hls
                viewModel.logAction("DASH is not supported on iOS. Forcing HLS.")
            } else {
                viewModel.protocolOption = value
            }
        }
        .onChange(of: segmentSelection) { value in
            viewModel.segmentOption = value
        }
        .onChange(of: codecSelection) { value in
            viewModel.codecOption = value
        }
        .sheet(isPresented: $showContentPicker) {
            NavigationView {
                List {
                    ForEach(viewModel.availableContent) { item in
                        Button {
                            viewModel.selectedContent = item.name
                            showContentPicker = false
                        } label: {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(item.name)
                                    .foregroundColor(isPlayable(item) ? .primary : .secondary)
                                    .opacity(isPlayable(item) ? 1.0 : 0.5)
                            }
                        }
                        .disabled(!isPlayable(item))
                    }
                }
                .navigationTitle("Select Content (\(viewModel.availableContent.count))")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") { showContentPicker = false }
                    }
                }
            }
        }
    }

    private var playbackDiagnosticsGrid: some View {
        let rows = playbackDiagnosticsRows()
        return VStack(alignment: .leading, spacing: 8) {
            Text("Playback Diagnostics")
                .font(.headline)
            LazyVGrid(columns: [
                GridItem(.adaptive(minimum: 170), spacing: 16)
            ], spacing: 12) {
                ForEach(rows, id: \.label) { row in
                    diagnosticsCell(row.label, row.value)
                }
            }
        }
    }

    private func playbackDiagnosticsRows() -> [(label: String, value: String)] {
        let diagnostics = viewModel.diagnostics
        var rows: [(String, String)] = [
            ("State", diagnostics.state),
            ("Current Time", formatSeconds(diagnostics.currentTime)),
            ("Buffered End", formatSeconds(diagnostics.bufferedEnd)),
            ("Buffer Depth", formatSeconds(diagnostics.bufferDepth)),
            ("Live Offset", formatSeconds(diagnostics.liveOffset)),
            ("Playback Rate", String(format: "%.2fx", diagnostics.playbackRate)),
            ("Likely To Keep Up", diagnostics.likelyToKeepUp ? "Yes" : "No"),
            ("Buffer Empty", diagnostics.bufferEmpty ? "Yes" : "No"),
            ("Stalls", "\(diagnostics.stallCount)"),
            ("Observed Bitrate", formatBitrate(diagnostics.observedBitrate)),
            ("Indicated Bitrate", formatBitrate(diagnostics.indicatedBitrate)),
            ("Average Video Bitrate", formatBitrate(diagnostics.averageVideoBitrate)),
            ("Variant (URI)", trimURI(diagnostics.lastSegmentURI)),
            ("Item Status", diagnostics.itemStatus)
        ]
        if !diagnostics.waitingReason.isEmpty {
            rows.append(("Waiting Reason", diagnostics.waitingReason))
        }
        if !diagnostics.itemError.isEmpty {
            rows.append(("Item Error", diagnostics.itemError))
        }
        if !diagnostics.lastFailure.isEmpty {
            rows.append(("Playback Failure", diagnostics.lastFailure))
        }
        if !diagnostics.lastError.isEmpty {
            rows.append(("Last Error", diagnostics.lastError))
        }
        if !diagnostics.lastErrorLog.isEmpty {
            rows.append(("Error Log", diagnostics.lastErrorLog))
        }
        return rows
    }

    private func diagnosticsCell(_ label: String, _ value: String) -> some View {
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

    private func formatSeconds(_ value: Double?) -> String {
        guard let value else { return "--" }
        return String(format: "%.2fs", value)
    }

    private func formatBitrate(_ value: Double?) -> String {
        guard let value, value > 0 else { return "--" }
        let mbps = value / 1_000_000
        return String(format: "%.2f Mbps", mbps)
    }

    private func trimURI(_ uri: String) -> String {
        guard !uri.isEmpty else { return "--" }
        if let last = uri.split(separator: "/").last {
            return String(last)
        }
        return uri
    }

    private func applyEnvironment() {
        let env = ServerEnvironment(rawValue: serverEnvironmentRaw) ?? .dev
        let host = env.host
        let contentPort = env.contentPort
        let playbackPort = env.playbackPort
        viewModel.baseURLString = "http://\(host):\(contentPort)"
        viewModel.playbackBaseURLString = "http://\(host):\(playbackPort)"
        if let controlURL = URL(string: viewModel.baseURLString) {
            testingViewModel.updateControlBaseURL(controlURL)
        }
        viewModel.primePlayback = false
        viewModel.includePlayerIdInURL = true
        viewModel.forcePlayerIdOnPlayback = false
        viewModel.allowPlayerIdOnContentPort = false
    }

    private func isPlayable(_ item: ContentItem) -> Bool {
        let supportsProtocol = (protocolSelection == .hls) ? item.hasHls : item.hasDash
        guard supportsProtocol else { return false }
        let inferred = inferCodec(from: item.name)
        switch codecSelection {
        case .h264:
            return inferred == .h264
        case .hevc:
            return inferred == .hevc
        case .av1:
            return false
        case .all:
            return inferred != .av1
        }
    }

}
