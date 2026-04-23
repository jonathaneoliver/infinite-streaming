import SwiftUI

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

    var shortLabel: String {
        switch self {
        case .dev: return "Dev"
        case .release: return "Release"
        case .ubuntu: return "Ubuntu"
        }
    }

    var host: String {
        switch self {
        case .dev: return "100.111.190.54"
        case .release: return "infinitestreaming.jeoliver.com"
        case .ubuntu: return "192.168.0.106"
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
    @Environment(\.openURL) private var openURL
    @State private var baseURLText: String = ""
    @State private var protocolSelection: ProtocolOption = .hls
    @State private var segmentSelection: SegmentOption = .s6
    @State private var codecSelection: CodecOption = .h264
    @AppStorage("server_environment") private var serverEnvironmentRaw: String = ServerEnvironment.dev.rawValue
    @State private var showContentPicker = false
    @State private var isTVFullscreen = false
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    #if os(tvOS)
    private enum TVFocus: Hashable { case retryFetch, fullscreen, serverDev, serverUbuntu, contentButton, allow4k }
    @FocusState private var tvFocus: TVFocus?
    #endif

    init() {
        _viewModel = StateObject(wrappedValue: PlaybackViewModel())
    }

    var body: some View {
        mainContent
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
            .onChange(of: viewModel.selectedContent) {
                applyEnvironment()
                viewModel.logAction("Selected content: \(viewModel.selectedContent)")
                viewModel.applySelection()
                viewModel.play()
            }
            .onChange(of: protocolSelection) { oldValue, newValue in
                if newValue == .dash {
                    protocolSelection = .hls
                    viewModel.protocolOption = .hls
                    viewModel.logAction("DASH is not supported. Forcing HLS.")
                } else {
                    viewModel.protocolOption = newValue
                }
            }
            .onChange(of: segmentSelection) { oldValue, newValue in
                viewModel.segmentOption = newValue
            }
            .onChange(of: codecSelection) { oldValue, newValue in
                viewModel.codecOption = newValue
            }
            .onChange(of: viewModel.goLiveMode) {
                applyEnvironment()
                if !viewModel.selectedContent.isEmpty {
                    viewModel.logAction("Go Live mode \(viewModel.goLiveMode ? "ON" : "OFF") — restarting playback")
                    viewModel.applySelection()
                    viewModel.play()
                }
            }
            .sheet(isPresented: $showContentPicker) {
                contentPickerSheet
            }
    }

    @ViewBuilder
    private var mainContent: some View {
        #if os(tvOS)
        tvOSBody
        #else
        iOSBody
        #endif
    }

    // MARK: - iOS body

    #if os(iOS)
    private var iOSBody: some View {
        ScrollView(.vertical, showsIndicators: true) {
            VStack(alignment: .leading, spacing: 12) {
                Text("InfiniteStream Player")
                    .font(.title)

                let compact = horizontalSizeClass == .compact
                if compact {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack(spacing: 8) {
                            Button("Retry") {
                                viewModel.logAction("Retry Fetch")
                                applyEnvironment()
                                viewModel.retryFetch()
                            }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                            Button("Restart") {
                                viewModel.logAction("Restart Playback")
                                applyEnvironment()
                                viewModel.restartPlayback()
                            }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                            Button("Reload") {
                                viewModel.logAction("Reload Page")
                                Task {
                                    applyEnvironment()
                                    await viewModel.reloadPage()
                                }
                            }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                        }
                        HStack(spacing: 12) {
                            Toggle("4K", isOn: $viewModel.prefer4kNative)
                                .toggleStyle(.switch)
                                .fixedSize()
                            Toggle("Local Proxy", isOn: $viewModel.localProxyEnabled)
                                .toggleStyle(.switch)
                                .fixedSize()
                            Toggle("Auto-Recovery", isOn: $viewModel.autoRecoveryEnabled)
                                .toggleStyle(.switch)
                                .fixedSize()
                            Toggle("Go Live", isOn: $viewModel.goLiveMode)
                                .toggleStyle(.switch)
                                .fixedSize()
                        }
                        .font(.caption)
                    }
                } else {
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
                            Toggle("", isOn: $viewModel.localProxyEnabled)
                                .labelsHidden()
                                .toggleStyle(.switch)
                            Text("Local Proxy")
                        }
                        HStack(spacing: 6) {
                            Toggle("", isOn: $viewModel.autoRecoveryEnabled)
                                .labelsHidden()
                                .toggleStyle(.switch)
                            Text("Auto-Recovery")
                        }
                        HStack(spacing: 6) {
                            Toggle("", isOn: $viewModel.goLiveMode)
                                .labelsHidden()
                                .toggleStyle(.switch)
                            Text("Go Live")
                        }
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
                                .onChange(of: geo.size) { oldSize, newSize in
                                    viewModel.diagnostics.updateDisplaySize(newSize)
                                }
                        }
                    )
                    .background(Color.black)
                    .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
                    .padding(.horizontal, -16)

                VStack(alignment: .leading, spacing: 8) {
                    Text("Content Control")
                        .font(.headline)

                    Picker("Server", selection: $serverEnvironmentRaw) {
                        ForEach(ServerEnvironment.allCases) { env in
                            Text(env.label).tag(env.rawValue)
                        }
                    }
                    .pickerStyle(.segmented)
                    .onChange(of: serverEnvironmentRaw) {
                        applyEnvironment()
                        baseURLText = viewModel.baseURLString
                    }

                    TextField("Base URL", text: $baseURLText)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled(true)
                        .onSubmit {
                            viewModel.baseURLString = baseURLText
                        }

                    if compact {
                        VStack(alignment: .leading, spacing: 6) {
                            HStack(spacing: 8) {
                                Picker("Protocol", selection: $protocolSelection) {
                                    ForEach(ProtocolOption.allCases) { option in
                                        Text(option.label).tag(option)
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
                            }
                            .controlSize(.small)
                            Button {
                                showContentPicker = true
                            } label: {
                                let display = viewModel.selectedContent.isEmpty ? "Select content" : viewModel.selectedContent
                                Text(display)
                                    .lineLimit(1)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                            }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                        }
                    } else {
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
                    }

                    if !viewModel.lastMasterRequestLine.isEmpty {
                        Text(viewModel.lastMasterRequestLine)
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                }
                .font(.caption)

            }
            .padding()
        }
    }

    #endif

    // MARK: - tvOS body

    #if os(tvOS)
    private var tvOSBody: some View {
        GeometryReader { geo in
            HStack(alignment: .top, spacing: 0) {
                // Left panel: action buttons + player
                VStack(alignment: .leading, spacing: 8) {
                    HStack(spacing: 8) {
                        Button("Retry Fetch") {
                            viewModel.logAction("Retry Fetch")
                            applyEnvironment()
                            viewModel.retryFetch()
                        }
                        .buttonStyle(.bordered)
                        .tint(Color(white: 0.4))
                        .foregroundStyle(Color.white)
                        .focused($tvFocus, equals: .retryFetch)
                        Button("Restart Playback") {
                            viewModel.logAction("Restart Playback")
                            applyEnvironment()
                            viewModel.restartPlayback()
                        }
                        .buttonStyle(.bordered)
                        .tint(Color(white: 0.4))
                        .foregroundStyle(Color.white)
                        Button("Reload Page") {
                            viewModel.logAction("Reload Page")
                            Task {
                                applyEnvironment()
                                await viewModel.reloadPage()
                            }
                        }
                        .buttonStyle(.bordered)
                        .tint(Color(white: 0.4))
                        .foregroundStyle(Color.white)
                        Button(isTVFullscreen ? "Exit Fullscreen" : "Fullscreen") {
                            isTVFullscreen.toggle()
                        }
                        .buttonStyle(.bordered)
                        .tint(Color(white: 0.4))
                        .foregroundStyle(Color.white)
                        .focused($tvFocus, equals: .fullscreen)
                        Spacer()
                    }
                    .font(.caption)

                    HStack(spacing: 8) {
                        tvOptionButton(label: viewModel.prefer4kNative ? "Allow 4K: ON" : "Allow 4K: OFF",
                                       selected: viewModel.prefer4kNative,
                                       action: { viewModel.prefer4kNative.toggle() },
                                       onMove: { if $0 == .down { tvFocus = .serverDev } })
                        .focused($tvFocus, equals: .allow4k)
                        tvOptionButton(label: viewModel.localProxyEnabled ? "Local Proxy: ON" : "Local Proxy: OFF",
                                       selected: viewModel.localProxyEnabled,
                                       action: { viewModel.localProxyEnabled.toggle() },
                                       onMove: { if $0 == .down { tvFocus = .serverDev } })
                        tvOptionButton(label: viewModel.autoRecoveryEnabled ? "Auto-Recovery: ON" : "Auto-Recovery: OFF",
                                       selected: viewModel.autoRecoveryEnabled,
                                       action: { viewModel.autoRecoveryEnabled.toggle() },
                                       onMove: { if $0 == .down { tvFocus = .serverDev } })
                        tvOptionButton(label: viewModel.goLiveMode ? "Go Live: ON" : "Go Live: OFF",
                                       selected: viewModel.goLiveMode,
                                       action: { viewModel.goLiveMode.toggle() },
                                       onMove: { if $0 == .down { tvFocus = .serverDev } })
                        Spacer()
                    }
                    .font(.caption)

                    PlayerView(player: viewModel.player)
                        .aspectRatio(16.0 / 9.0, contentMode: .fit)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .background(Color.black)
                        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
                }
                .padding(8)
                .frame(width: isTVFullscreen ? geo.size.width : geo.size.width * 0.65)
                .focusSection()

                if !isTVFullscreen {
                    Rectangle()
                        .fill(Color(white: 0.2))
                        .frame(width: 1)

                    tvRightPanel
                        .frame(width: geo.size.width * 0.35 - 1)
                        .focusSection()
                }
            }
        }
        .background(Color.black)
        .onExitCommand {
            if isTVFullscreen { isTVFullscreen = false }
        }
    }

    private var tvRightPanel: some View {
        ScrollView(.vertical, showsIndicators: false) {
            VStack(alignment: .leading, spacing: 10) {
                tvRightPanelHeader
                tvRightPanelToggles
                tvRightPanelContent
            }
            .padding(16)
        }
    }

    @ViewBuilder
    private var tvRightPanelHeader: some View {
        Text("InfiniteStream Player")
            .font(.title3)
            .bold()
        Text("Content Control")
            .font(.headline)
            .foregroundColor(.secondary)
            .padding(.top, 4)
    }

    @ViewBuilder
    private var tvRightPanelToggles: some View {
        Text("Server").font(.caption).foregroundColor(.secondary).padding(.top, 2)
        tvServerRow
        Text("Protocol").font(.caption).foregroundColor(.secondary).padding(.top, 2)
        tvProtocolRow
        Text("Segment").font(.caption).foregroundColor(.secondary).padding(.top, 2)
        tvSegmentRow
        Text("Codec").font(.caption).foregroundColor(.secondary).padding(.top, 2)
        tvCodecRow
    }

    @ViewBuilder
    private var tvRightPanelContent: some View {
        Text("Content").font(.caption).foregroundColor(.secondary).padding(.top, 2)
        Button {
            showContentPicker = true
        } label: {
            Text(viewModel.selectedContent.isEmpty ? "Select content" : viewModel.selectedContent)
                .lineLimit(1)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .buttonStyle(.bordered)
        .tint(Color.gray)
        .foregroundStyle(Color.black)
        .background(tvFocus == .contentButton ? tvSelectedTint : Color.clear, in: RoundedRectangle(cornerRadius: 10))
        .focused($tvFocus, equals: .contentButton)
        if !viewModel.lastMasterRequestLine.isEmpty {
            Text(viewModel.lastMasterRequestLine)
                .font(.caption2)
                .foregroundColor(.secondary)
                .padding(.top, 4)
        }
    }

    private let tvSelectedTint = Color(red: 0, green: 0.706, blue: 0.847)

    private var tvServerRow: some View {
        HStack(spacing: 4) {
            tvOptionButton(label: ServerEnvironment.dev.shortLabel,
                           selected: serverEnvironmentRaw == ServerEnvironment.dev.rawValue,
                           action: { serverEnvironmentRaw = ServerEnvironment.dev.rawValue; applyEnvironment(); baseURLText = viewModel.baseURLString },
                           onMove: { if $0 == .up { tvFocus = .allow4k } })
            .focused($tvFocus, equals: .serverDev)
            tvOptionButton(label: ServerEnvironment.release.shortLabel,
                           selected: serverEnvironmentRaw == ServerEnvironment.release.rawValue,
                           action: { serverEnvironmentRaw = ServerEnvironment.release.rawValue; applyEnvironment(); baseURLText = viewModel.baseURLString },
                           onMove: { if $0 == .up { tvFocus = .allow4k } })
            tvOptionButton(label: ServerEnvironment.ubuntu.shortLabel,
                           selected: serverEnvironmentRaw == ServerEnvironment.ubuntu.rawValue,
                           action: { serverEnvironmentRaw = ServerEnvironment.ubuntu.rawValue; applyEnvironment(); baseURLText = viewModel.baseURLString },
                           onMove: { if $0 == .up { tvFocus = .allow4k } })
            .focused($tvFocus, equals: .serverUbuntu)
        }
    }

    private var tvProtocolRow: some View {
        HStack(spacing: 4) {
            ForEach(ProtocolOption.allCases) { option in
                tvOptionButton(label: option.label, selected: protocolSelection == option) {
                    protocolSelection = option
                }
            }
        }
    }

    private var tvSegmentRow: some View {
        HStack(spacing: 4) {
            ForEach(SegmentOption.allCases) { option in
                tvOptionButton(label: option.label, selected: segmentSelection == option) {
                    segmentSelection = option
                }
            }
        }
    }

    private var tvCodecRow: some View {
        HStack(spacing: 4) {
            ForEach(CodecOption.allCases) { option in
                tvOptionButton(label: option.label, selected: codecSelection == option) {
                    codecSelection = option
                }
            }
        }
    }

    private func tvOptionButton(label: String, selected: Bool, action: @escaping () -> Void, onMove: ((MoveCommandDirection) -> Void)? = nil) -> some View {
        Button(label, action: action)
            .buttonStyle(.bordered)
            .tint(Color.gray)
            .foregroundStyle(Color.black)
            .background(selected ? tvSelectedTint : Color.clear, in: RoundedRectangle(cornerRadius: 10))
            .onMoveCommand { direction in
                onMove?(direction)
            }
    }
    #endif

    // MARK: - Shared content picker sheet

    @ViewBuilder
    private var contentPickerSheet: some View {
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
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { showContentPicker = false }
                }
            }
            #endif
        }
    }

    private func applyEnvironment() {
        let env = ServerEnvironment(rawValue: serverEnvironmentRaw) ?? .dev
        let host = env.host
        let contentPort = env.contentPort
        let playbackPort = env.playbackPort
        viewModel.baseURLString = "http://\(host):\(contentPort)"
        if viewModel.goLiveMode {
            viewModel.playbackBaseURLString = "http://\(host):\(contentPort)"
            viewModel.includePlayerIdInURL = false
        } else {
            viewModel.playbackBaseURLString = "http://\(host):\(playbackPort)"
            viewModel.includePlayerIdInURL = true
        }
        viewModel.primePlayback = false
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
