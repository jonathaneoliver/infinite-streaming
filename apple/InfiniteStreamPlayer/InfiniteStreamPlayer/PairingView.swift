import SwiftUI

/// Shows a pairing code and waits for the user to publish a server URL
/// from the dashboard. Used primarily on tvOS (no QR scanner) but also
/// available on iOS as an alternative to scanning.
///
/// Lifecycle:
///  - On appear: generate a code, render it, start a polling Task.
///  - On rendezvous match: hand the URL to `onPaired`, dismiss.
///  - On cancel/dismiss: cancel the Task, DELETE the rendezvous entry.
struct PairingView: View {
    let onPaired: (String) -> Void
    let onCancel: () -> Void

    @State private var code: String = ""
    @State private var statusMessage: String = "Waiting for the dashboard…"
    @State private var errorMessage: String? = nil
    @State private var pollingTask: Task<Void, Never>? = nil
    @State private var rendezvousURL: String = RendezvousService.url
    @State private var discovered: [RendezvousService.DiscoveredServer] = []
    @State private var discoveryTask: Task<Void, Never>? = nil

    var body: some View {
        ScrollView {
            VStack(spacing: 28) {
                Text("Pair this device")
                    .font(.largeTitle)
                    .bold()

                if !discovered.isEmpty {
                    VStack(spacing: 12) {
                        Text("Servers on your network")
                            .font(.title3)
                            .foregroundColor(.secondary)
                        VStack(spacing: 8) {
                            ForEach(discovered) { server in
                                Button {
                                    onPaired(server.url)
                                } label: {
                                    HStack(spacing: 12) {
                                        Image(systemName: "server.rack")
                                        VStack(alignment: .leading, spacing: 2) {
                                            Text(server.label).font(.body).bold()
                                            Text(server.url)
                                                .font(.caption)
                                                .foregroundColor(.secondary)
                                                .lineLimit(1)
                                                .truncationMode(.middle)
                                        }
                                        Spacer()
                                    }
                                    .padding(.horizontal, 16).padding(.vertical, 12)
                                    .background(Color.gray.opacity(0.15))
                                    .cornerRadius(10)
                                }
                                #if !os(tvOS)
                                .buttonStyle(.plain)
                                #endif
                            }
                        }
                        Text("…or pair manually with a code:")
                            .font(.caption)
                            .foregroundColor(.secondary)
                            .padding(.top, 4)
                    }
                }

                VStack(spacing: 12) {
                    Text("On your phone or laptop, open:")
                        .foregroundColor(.secondary)
                    Text(displayHost(rendezvousURL))
                        .font(hostFont)
                        .monospaced()
                        .multilineTextAlignment(.center)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.horizontal, 16).padding(.vertical, 8)
                        .background(Color.gray.opacity(0.15))
                        .cornerRadius(8)
                    Text("then enter this code:")
                        .foregroundColor(.secondary)
                }

                Text(code)
                    .font(.system(size: codeFontSize, weight: .heavy, design: .monospaced))
                    .tracking(8)
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
                    .padding(.horizontal, 24).padding(.vertical, 16)
                    .background(Color.accentColor.opacity(0.18))
                    .cornerRadius(18)

                VStack(spacing: 8) {
                    if let err = errorMessage {
                        Text(err)
                            .foregroundColor(.red)
                            .multilineTextAlignment(.center)
                            .padding(.horizontal, 40)
                    } else {
                        HStack(spacing: 10) {
                            ProgressView()
                            Text(statusMessage).foregroundColor(.secondary)
                        }
                    }
                }
                .frame(minHeight: 60)

                HStack(spacing: 16) {
                    Button("Generate New Code") { restart() }
                    Button("Cancel") {
                        cancelAndDismiss()
                    }
                }
                .padding(.top, 8)
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 32)
            .frame(maxWidth: .infinity)
        }
        .onAppear {
            start()
            startDiscovery()
        }
        .onDisappear {
            pollingTask?.cancel()
            discoveryTask?.cancel()
        }
    }

    private func startDiscovery() {
        discoveryTask?.cancel()
        discoveryTask = Task {
            // Refresh every 15s while the screen is open. The server's
            // announce TTL is 90s, so this is responsive without being
            // chatty.
            while !Task.isCancelled {
                let servers = await RendezvousService.discoverServers()
                await MainActor.run { discovered = servers }
                try? await Task.sleep(nanoseconds: 15 * 1_000_000_000)
            }
        }
    }

    private var codeFontSize: CGFloat {
        #if os(tvOS)
        88
        #else
        56
        #endif
    }

    private var hostFont: Font {
        #if os(tvOS)
        .title2
        #else
        .callout
        #endif
    }

    private func start() {
        // If rendezvous isn't configured, fail fast and show a helpful message.
        if RendezvousService.url.isEmpty {
            errorMessage = "Pairing rendezvous is not configured. Set InfiniteStreamRendezvousURL in app settings."
            return
        }
        if code.isEmpty {
            code = RendezvousService.generateCode()
        }
        pollingTask?.cancel()
        let myCode = code
        pollingTask = Task {
            do {
                let serverURL = try await RendezvousService.pollForServerURL(code: myCode)
                await RendezvousService.releaseCode(myCode)
                await MainActor.run { onPaired(serverURL) }
            } catch is CancellationError {
                // Cancelled by user — silent.
            } catch {
                await MainActor.run {
                    errorMessage = error.localizedDescription
                    statusMessage = ""
                }
            }
        }
    }

    private func restart() {
        errorMessage = nil
        statusMessage = "Waiting for the dashboard…"
        code = RendezvousService.generateCode()
        start()
    }

    private func cancelAndDismiss() {
        pollingTask?.cancel()
        let toRelease = code
        Task { await RendezvousService.releaseCode(toRelease) }
        onCancel()
    }

    private func displayHost(_ urlString: String) -> String {
        guard let u = URL(string: urlString), let host = u.host else { return urlString }
        return host
    }
}
