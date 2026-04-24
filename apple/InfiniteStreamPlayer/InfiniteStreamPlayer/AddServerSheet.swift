import SwiftUI

#if os(iOS)
/// Sheet for adding a new server profile via manual host/port entry,
/// with an optional QR-scan shortcut. Closes via `onDone(profile)` —
/// passes the added profile or nil if cancelled.
struct AddServerSheet: View {
    @ObservedObject var store: ServerProfileStore
    let onDone: (ServerProfile?) -> Void

    @State private var host: String = ""
    @State private var contentPortText: String = "30000"
    @State private var playbackPortText: String = "30081"
    @State private var label: String = ""
    @State private var useHTTPS: Bool = false
    @State private var showQR: Bool = false
    @State private var showPairing: Bool = false
    @State private var errorMessage: String? = nil
    @State private var discovered: [RendezvousService.DiscoveredServer] = []
    @State private var discoveryLoaded: Bool = false
    @State private var discoveryTask: Task<Void, Never>? = nil

    var body: some View {
        NavigationView {
            Form {
                Section(header: Text("Discovered on your network"),
                        footer: Text("Servers visible from your public IP. If your phone is on cellular or a different network, use manual entry below.").font(.caption).foregroundColor(.secondary)) {
                    if !discoveryLoaded {
                        HStack(spacing: 10) {
                            ProgressView()
                            Text("Looking…").foregroundColor(.secondary)
                        }
                    } else if discovered.isEmpty {
                        Text("No servers detected.")
                            .foregroundColor(.secondary)
                            .font(.callout)
                    } else {
                        ForEach(discovered) { server in
                            Button {
                                addDiscovered(server)
                            } label: {
                                HStack {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(server.label)
                                            .font(.body)
                                            .foregroundColor(.primary)
                                        Text(server.url)
                                            .font(.caption)
                                            .foregroundColor(.secondary)
                                            .lineLimit(1)
                                            .truncationMode(.middle)
                                    }
                                    Spacer()
                                    Image(systemName: "plus.circle.fill")
                                        .foregroundColor(.accentColor)
                                }
                            }
                        }
                        Button {
                            startDiscovery()
                        } label: {
                            Label("Refresh", systemImage: "arrow.clockwise")
                        }
                        .font(.callout)
                    }
                }
                Section(header: Text("Host")) {
                    TextField("hostname or IP", text: $host)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled(true)
                        .keyboardType(.URL)
                }
                Section(header: Text("Ports")) {
                    HStack {
                        Text("Content").frame(width: 80, alignment: .leading)
                        TextField("30000", text: $contentPortText)
                            .keyboardType(.numberPad)
                            .onChange(of: contentPortText) { _, new in
                                if let port = Int(new) { playbackPortText = String(port + 81) }
                            }
                    }
                    HStack {
                        Text("Playback").frame(width: 80, alignment: .leading)
                        TextField("30081", text: $playbackPortText)
                            .keyboardType(.numberPad)
                    }
                    Toggle("Use HTTPS", isOn: $useHTTPS)
                }
                Section(header: Text("Label (optional)")) {
                    TextField("auto: host:port", text: $label)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled(true)
                }
                if let err = errorMessage {
                    Section { Text(err).foregroundColor(.red).font(.callout) }
                }
                Section {
                    Button {
                        showQR = true
                    } label: {
                        Label("Scan QR from dashboard", systemImage: "qrcode.viewfinder")
                    }
                    Button {
                        showPairing = true
                    } label: {
                        Label("Pair with code", systemImage: "key.horizontal")
                    }
                }
            }
            .navigationTitle("Add Server")
            .navigationBarTitleDisplayMode(.inline)
            .onAppear { startDiscovery() }
            .onDisappear { discoveryTask?.cancel() }
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { onDone(nil) }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { save() }
                        .disabled(host.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
            .sheet(isPresented: $showQR) {
                QRScannerView { scanned in
                    if let p = ServerProfile.fromDashboardURL(scanned) {
                        store.add(p, makeActive: true)
                        showQR = false
                        onDone(p)
                    } else {
                        errorMessage = "Scanned QR is not a valid http(s) URL."
                        showQR = false
                    }
                }
            }
            .sheet(isPresented: $showPairing) {
                PairingView(
                    onPaired: { serverURL in
                        if let p = ServerProfile.fromDashboardURL(serverURL) {
                            store.add(p, makeActive: true)
                            showPairing = false
                            onDone(p)
                        } else {
                            errorMessage = "Paired URL is not a valid http(s) URL."
                            showPairing = false
                        }
                    },
                    onCancel: { showPairing = false }
                )
            }
        }
    }

    private func save() {
        errorMessage = nil
        guard let cport = Int(contentPortText) else {
            errorMessage = "Content port must be a number."
            return
        }
        let pport = Int(playbackPortText) ?? (cport + 81)
        let scheme = useHTTPS ? "https" : "http"
        guard let profile = ServerProfile.fromHostPort(host: host, contentPort: cport, playbackPort: pport, scheme: scheme, label: label.isEmpty ? nil : label) else {
            errorMessage = "Invalid host or port."
            return
        }
        store.add(profile, makeActive: true)
        onDone(profile)
    }

    private func startDiscovery() {
        discoveryTask?.cancel()
        discoveryTask = Task {
            let servers = await RendezvousService.discoverServers()
            await MainActor.run {
                discovered = servers
                discoveryLoaded = true
            }
        }
    }

    private func addDiscovered(_ server: RendezvousService.DiscoveredServer) {
        // Use the host:port from the URL as the button label so several
        // discovered servers from the same announce label still distinguish.
        guard let profile = ServerProfile.fromDashboardURL(server.url) else {
            errorMessage = "Discovered URL is not valid: \(server.url)"
            return
        }
        store.add(profile, makeActive: true)
        onDone(profile)
    }
}
#endif
