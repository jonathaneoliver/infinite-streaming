import SwiftUI

/// Cinematic server picker — replaces the old `ContentView` server row +
/// `AddServerSheet`. Three primary actions:
///
/// - Pick an existing saved server (cards in a grid).
/// - Pair via 6-character code (PairingView sheet).
/// - Add manually by URL (sheet form).
struct ServerPickerScreen: View {
    @ObservedObject var vm: PlayerViewModel
    let onServerChosen: () -> Void

    @State private var showPairing = false
    @State private var showManualAdd = false
    @State private var showQRScanner = false
    @State private var discovered: [RendezvousService.DiscoveredServer] = []
    @State private var discoveryState: DiscoveryState = .idle

    private enum DiscoveryState { case idle, scanning, done }

    var body: some View {
        GeometryReader { geo in
            ScrollView {
                VStack(alignment: .leading, spacing: Space.s7) {
                    header
                    if !undiscoveredVisible.isEmpty || discoveryState == .scanning {
                        discoveredSection(width: geo.size.width)
                    }
                    if vm.servers.isEmpty {
                        emptyHint
                    } else {
                        serverGrid(width: geo.size.width)
                    }
                    actions
                }
                .padding(.horizontal, Space.s7)
                .padding(.vertical, Space.s7)
            }
            .background(Tokens.bg.ignoresSafeArea())
        }
        .task { await runDiscovery() }
        .sheet(isPresented: $showPairing) {
            PairingSheet { urlString in
                showPairing = false
                if let profile = ServerProfile.fromDashboardURL(urlString) {
                    vm.addServer(profile)
                    onServerChosen()
                }
            }
        }
        .sheet(isPresented: $showManualAdd) {
            ManualAddSheet { profile in
                showManualAdd = false
                vm.addServer(profile)
                onServerChosen()
            }
        }
        #if os(iOS)
        .sheet(isPresented: $showQRScanner) {
            QRScanSheet { code in
                showQRScanner = false
                if let profile = ServerProfile.fromDashboardURL(code) {
                    vm.addServer(profile)
                    onServerChosen()
                }
            }
        }
        #endif
    }

    /// Servers visible on the same public IP that the user hasn't
    /// already saved — anything in `vm.servers` is filtered out so we
    /// don't duplicate the saved-grid below.
    private var undiscoveredVisible: [RendezvousService.DiscoveredServer] {
        let savedURLs = Set(vm.servers.map { $0.contentURL.lowercased() })
        return discovered.filter { d in
            // Compare the dashboard URL we'd build from the rendezvous
            // entry — that's what addServer dedupes on.
            guard let profile = ServerProfile.fromDashboardURL(d.url) else { return false }
            return !savedURLs.contains(profile.contentURL.lowercased())
        }
    }

    @ViewBuilder
    private func discoveredSection(width: CGFloat) -> some View {
        VStack(alignment: .leading, spacing: Space.s3) {
            HStack(spacing: Space.s2) {
                Text("DISCOVERED ON YOUR NETWORK")
                    .font(AppType.label())
                    .foregroundColor(Tokens.fgDim)
                if discoveryState == .scanning {
                    ProgressView().scaleEffect(0.7).tint(Tokens.fgDim)
                }
                Spacer()
                Button("Refresh") {
                    Task { await runDiscovery() }
                }
                .font(AppType.monoSm())
                .foregroundColor(Tokens.fgDim)
                .buttonStyle(.plain)
            }
            if undiscoveredVisible.isEmpty && discoveryState == .done {
                Text("No nearby servers announced.")
                    .font(AppType.body(size: 14))
                    .foregroundColor(Tokens.fgFaint)
            } else {
                LazyVGrid(columns: gridColumns(for: width), spacing: Space.s4) {
                    ForEach(undiscoveredVisible) { entry in
                        DiscoveredCard(entry: entry) {
                            guard let profile = ServerProfile.fromDashboardURL(
                                entry.url,
                                label: entry.label.isEmpty ? nil : entry.label
                            ) else { return }
                            vm.addServer(profile)
                            onServerChosen()
                        }
                    }
                }
            }
        }
    }

    private func runDiscovery() async {
        discoveryState = .scanning
        let result = await RendezvousService.discoverServers()
        await MainActor.run {
            discovered = result
            discoveryState = .done
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: Space.s4) {
            // Back-to-Home affordance — only shown when there's already
            // an active server, so first-launch users with no saved
            // servers don't get a button that strands them on a blank Home.
            if vm.activeServer != nil {
                BackChevronButton { onServerChosen() }
            }
            VStack(alignment: .leading, spacing: Space.s2) {
                Text("INFINITE STREAM")
                    .font(AppType.label())
                    .foregroundColor(Tokens.fgDim)
                Text("Choose a server")
                    .font(AppType.title(size: 38))
                    .foregroundColor(Tokens.fg)
            }
            Spacer()
        }
    }

    private var emptyHint: some View {
        VStack(alignment: .leading, spacing: Space.s2) {
            Text("No saved servers yet.")
                .font(AppType.body())
                .foregroundColor(Tokens.fgDim)
            Text("Pair with a code or add a server URL to get started.")
                .font(AppType.body(size: 15))
                .foregroundColor(Tokens.fgFaint)
        }
    }

    private func serverGrid(width: CGFloat) -> some View {
        let columns = gridColumns(for: width)
        return LazyVGrid(columns: columns, spacing: Space.s4) {
            ForEach(vm.servers) { server in
                ServerCard(
                    server: server,
                    active: server.id == vm.activeServerID,
                    onSelect: {
                        vm.selectServer(server.id)
                        onServerChosen()
                    },
                    onForget: { vm.forgetServer(server.id) }
                )
            }
        }
        .tvFocusSection()
    }

    private func gridColumns(for screenWidth: CGFloat) -> [GridItem] {
        #if os(tvOS)
        return Array(repeating: GridItem(.flexible(), spacing: Space.s4), count: 3)
        #else
        // Reactive to the *current* window width, not the natural-
        // orientation bounds — `UIScreen.main.bounds.width` stays in
        // portrait dimensions even after the iPad rotates, leaving the
        // grid one column when it should be three.
        let cols = screenWidth < 500 ? 1 : (screenWidth < 900 ? 2 : 3)
        return Array(repeating: GridItem(.flexible(), spacing: Space.s4), count: cols)
        #endif
    }

    private var actions: some View {
        // Wrap in a wrapping HStack so the row collapses gracefully on
        // narrow iPhone widths — three actions side-by-side would
        // overflow on smaller screens.
        FlexibleActionsRow {
            ActionButton(
                title: "Pair with code",
                systemImage: "person.fill.checkmark",
                primary: true
            ) { showPairing = true }
            #if os(iOS)
            ActionButton(
                title: "Scan QR",
                systemImage: "qrcode.viewfinder",
                primary: false
            ) { showQRScanner = true }
            #endif
            ActionButton(
                title: "Add by URL",
                systemImage: "link",
                primary: false
            ) { showManualAdd = true }
        }
    }
}

// MARK: - Server card

private struct ServerCard: View {
    let server: ServerProfile
    let active: Bool
    let onSelect: () -> Void
    let onForget: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: Space.s2) {
            HStack {
                if active {
                    Circle().fill(Tokens.accent).frame(width: 8, height: 8)
                }
                Text(server.label)
                    .font(AppType.bodyEm())
                    .foregroundColor(Tokens.fg)
                    .lineLimit(1)
                Spacer()
            }
            Text(server.contentURL)
                .font(AppType.mono(size: 12))
                .foregroundColor(Tokens.fgDim)
                .lineLimit(1)
            Spacer().frame(height: Space.s3)
            HStack {
                Spacer()
                Button("Forget", action: onForget)
                    .font(AppType.monoSm())
                    .foregroundColor(Tokens.fgDim)
                    .buttonStyle(.plain)
            }
        }
        .padding(Space.s4)
        .frame(maxWidth: .infinity, minHeight: 120, alignment: .topLeading)
        .background(active ? Tokens.bgCard : Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.card)
        .contentShape(Rectangle())
        .onTapGesture(perform: onSelect)
    }
}

// MARK: - Discovered card

private struct DiscoveredCard: View {
    let entry: RendezvousService.DiscoveredServer
    let onAdd: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: Space.s3) {
            Image(systemName: "wifi")
                .font(.system(size: 18))
                .foregroundColor(Tokens.accent)
                .frame(width: 28, height: 28)
            VStack(alignment: .leading, spacing: 2) {
                Text(entry.label)
                    .font(AppType.bodyEm())
                    .foregroundColor(Tokens.fg)
                    .lineLimit(1)
                Text(entry.url)
                    .font(AppType.mono(size: 12))
                    .foregroundColor(Tokens.fgDim)
                    .lineLimit(1)
            }
            Spacer()
            Image(systemName: "plus.circle.fill")
                .font(.system(size: 22))
                .foregroundColor(Tokens.accent)
        }
        .padding(Space.s4)
        .frame(maxWidth: .infinity, minHeight: 80, alignment: .topLeading)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.card)
        .contentShape(Rectangle())
        .onTapGesture(perform: onAdd)
    }
}

// MARK: - Action button

private struct ActionButton: View {
    let title: String
    let systemImage: String
    let primary: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: Space.s2) {
                Image(systemName: systemImage)
                Text(title).font(AppType.bodyEm())
            }
            .foregroundColor(primary ? .black : Tokens.fg)
            .padding(.horizontal, Space.s5)
            .padding(.vertical, 14)
            .background(primary ? Tokens.accent : Tokens.bgSoft)
            .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
            .cinematicFocusFollower(cornerRadius: Radius.row)
        }
        .buttonStyle(.plain)
        #if os(tvOS)
        .focusEffectDisabled()
        #endif
    }
}

// MARK: - Flexible actions row

/// Lays children out left-to-right with equal-flex sizing. Falls back to
/// a vertical stack on narrow widths so the iPhone portrait layout
/// doesn't truncate button labels.
private struct FlexibleActionsRow<Content: View>: View {
    @ViewBuilder let content: Content

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: Space.s4) { content }
            VStack(alignment: .leading, spacing: Space.s3) { content }
        }
    }
}

// MARK: - QR scan sheet (iOS only)

#if os(iOS)
private struct QRScanSheet: View {
    let onScanned: (String) -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ZStack {
                Color.black.ignoresSafeArea()
                QRScannerView { code in onScanned(code) }
                VStack {
                    Spacer()
                    Text("Point camera at the dashboard's Server QR code")
                        .font(AppType.body(size: 14))
                        .foregroundColor(.white)
                        .padding(.horizontal, Space.s4)
                        .padding(.vertical, Space.s2)
                        .background(Color.black.opacity(0.6))
                        .clipShape(Capsule())
                        .padding(.bottom, Space.s7)
                }
            }
            .navigationTitle("Scan QR")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .foregroundColor(.white)
                }
            }
            .toolbarBackground(.black, for: .navigationBar)
            .toolbarBackground(.visible, for: .navigationBar)
            .toolbarColorScheme(.dark, for: .navigationBar)
        }
    }
}
#endif

// MARK: - Pairing sheet (wraps PairingView)

private struct PairingSheet: View {
    let onPaired: (String) -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            PairingView(
                onPaired: { onPaired($0) },
                onCancel: { dismiss() }
            )
            .navigationTitle("Pair")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close") { dismiss() }
                }
            }
            #endif
        }
    }
}

// MARK: - Manual add sheet

private struct ManualAddSheet: View {
    let onAdded: (ServerProfile) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var label = ""
    @State private var urlText = ""
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section("Server") {
                    TextField("Label (optional)", text: $label)
                    TextField("URL (e.g. http://192.168.1.10:30000)", text: $urlText)
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        .keyboardType(.URL)
                        .autocorrectionDisabled()
                        #endif
                }
                if let error {
                    Section { Text(error).foregroundColor(.red) }
                }
            }
            .navigationTitle("Add server")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Add") { tryAdd() }
                        .disabled(urlText.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
        }
    }

    private func tryAdd() {
        let cleanLabel = label.trimmingCharacters(in: .whitespaces)
        if let p = ServerProfile.fromDashboardURL(urlText, label: cleanLabel.isEmpty ? nil : cleanLabel) {
            onAdded(p)
        } else {
            error = "That doesn't look like a valid http(s) URL."
        }
    }
}
