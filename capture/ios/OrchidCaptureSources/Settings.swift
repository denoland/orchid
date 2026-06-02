import SwiftUI

enum CaptureRoute: String, CaseIterable, Identifiable {
    case clawpatrol
    case orchid
    case deno

    var id: String { rawValue }
    var displayName: String { rawValue }
}

/// Settings form — endpoint, tokens, route. Rendered directly under the
/// app's TopBar (the shell owns navigation). Persisted via @AppStorage so
/// DraftStore + StateStore read the same keys; changing the endpoint or
/// dashboard token reconnects the live socket.
struct SettingsForm: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.token") private var token: String = ""
    @AppStorage("orchid.http_secret") private var httpSecret: String = ""
    @AppStorage("orchid.route") private var route: String = CaptureRoute.clawpatrol.rawValue

    @State private var probing: ProbeState = .idle
    enum ProbeState: Equatable { case idle, probing, ok, fail(String) }

    var body: some View {
        Form {
            Section("Endpoint") {
                TextField("https://orchid.example.com", text: $endpoint)
                    .keyboardType(.URL).textInputAutocapitalization(.never)
                    .autocorrectionDisabled().font(Theme.mono(13))
                SecureField("X-Capture-Token", text: $token).font(Theme.mono(13))
                SecureField("Dashboard token (http_secret)", text: $httpSecret).font(Theme.mono(13))
                HStack {
                    Button { Task { await probe() } } label: { Label("Test connection", systemImage: "wifi") }
                    Spacer()
                    switch probing {
                    case .idle: EmptyView()
                    case .probing: ProgressView()
                    case .ok: Label("reachable", systemImage: "checkmark.circle.fill")
                            .foregroundStyle(Theme.emerald).font(.caption)
                    case .fail(let r): Label(r, systemImage: "xmark.circle.fill")
                            .foregroundStyle(Theme.rose).font(.caption).lineLimit(2)
                    }
                }
            }
            Section("Capture label") {
                Picker("Label", selection: $route) {
                    ForEach(CaptureRoute.allCases) { Text($0.displayName).tag($0.rawValue) }
                }.pickerStyle(.segmented)
                Text("New captures are filed under the inbox repo with this label.")
                    .font(.caption).foregroundStyle(Theme.muted)
            }
            Section {
                Text("Undelivered drafts stay queued on-device until the next successful submit.")
                    .font(.caption).foregroundStyle(Theme.muted)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Theme.surface)
        .onChange(of: endpoint) { _ in store.reconnect() }
        .onChange(of: httpSecret) { _ in store.reconnect() }
    }

    private func probe() async {
        probing = .probing
        guard let url = URL(string: endpoint), url.host != nil else { probing = .fail("bad URL"); return }
        do {
            var req = URLRequest(url: url); req.httpMethod = "HEAD"
            req.setValue(token, forHTTPHeaderField: "X-Capture-Token"); req.timeoutInterval = 6
            let (_, resp) = try await URLSession.shared.data(for: req)
            if let http = resp as? HTTPURLResponse {
                switch http.statusCode {
                case 401, 403: probing = .fail("auth rejected (\(http.statusCode))")
                default: probing = .ok
                }
            } else { probing = .fail("no response") }
        } catch { probing = .fail(error.localizedDescription) }
    }
}
