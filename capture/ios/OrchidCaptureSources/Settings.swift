import SwiftUI

enum CaptureRoute: String, CaseIterable, Identifiable {
    case clawpatrol
    case orchid
    case deno

    var id: String { rawValue }
    var displayName: String { rawValue }
}

/// Settings sheet — endpoint, token, route. Persisted via @AppStorage so
/// DraftStore can read the same keys.
struct SettingsView: View {
    @Environment(\.dismiss) private var dismiss
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.token") private var token: String = ""
    @AppStorage("orchid.http_secret") private var httpSecret: String = ""
    @AppStorage("orchid.route") private var route: String = CaptureRoute.clawpatrol.rawValue

    @State private var probing: ProbeState = .idle
    enum ProbeState: Equatable {
        case idle
        case probing
        case ok
        case fail(String)
    }

    var body: some View {
        NavigationStack {
            Form {
                Section("Endpoint") {
                    TextField(
                        "https://orchid.example.com/api/drafts",
                        text: $endpoint
                    )
                    .keyboardType(.URL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .font(.system(.body, design: .monospaced))

                    SecureField("X-Capture-Token", text: $token)
                        .font(.system(.body, design: .monospaced))

                    SecureField("Dashboard token (http_secret)", text: $httpSecret)
                        .font(.system(.body, design: .monospaced))

                    HStack {
                        Button {
                            Task { await probe() }
                        } label: {
                            Label("Test connection", systemImage: "wifi")
                        }
                        Spacer()
                        switch probing {
                        case .idle:
                            EmptyView()
                        case .probing:
                            ProgressView()
                        case .ok:
                            Label("reachable", systemImage: "checkmark.circle.fill")
                                .foregroundStyle(.green)
                                .font(.caption)
                        case .fail(let r):
                            Label(r, systemImage: "xmark.circle.fill")
                                .foregroundStyle(.red)
                                .font(.caption)
                                .lineLimit(2)
                        }
                    }
                }

                Section("Route") {
                    Picker("Label", selection: $route) {
                        ForEach(CaptureRoute.allCases) { r in
                            Text(r.displayName).tag(r.rawValue)
                        }
                    }
                    .pickerStyle(.segmented)
                    Text("Captures are filed under denoland/orchid with this label.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }

                Section {
                    Text("Drafts not delivered stay queued on-device until the next successful submit.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            .navigationTitle("Settings")
            .navigationBarTitleDisplayMode(.large)
        }
    }

    private func probe() async {
        probing = .probing
        guard let url = URL(string: endpoint), let base = url.host else {
            probing = .fail("bad URL")
            return
        }
        // HEAD to the endpoint URL. We don't expect a 200 (the handler is
        // POST-only) — we just want to see that the host answers and that
        // the token would be accepted on a real POST.
        do {
            var req = URLRequest(url: url)
            req.httpMethod = "HEAD"
            req.setValue(token, forHTTPHeaderField: "X-Capture-Token")
            req.timeoutInterval = 6
            let (_, resp) = try await URLSession.shared.data(for: req)
            if let http = resp as? HTTPURLResponse {
                // 405/404 still means we reached orch. 401/403 means the token
                // is rejected. 5xx means orch is alive but broke.
                switch http.statusCode {
                case 401, 403:
                    probing = .fail("auth rejected (\(http.statusCode))")
                default:
                    probing = .ok
                    _ = base
                }
            } else {
                probing = .fail("no response")
            }
        } catch {
            probing = .fail(error.localizedDescription)
        }
    }
}
