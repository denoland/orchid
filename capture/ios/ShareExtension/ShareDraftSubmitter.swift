import Foundation

/// Submits drafts from the share extension. Mirrors the main app's
/// `DraftStore.submit(...)` but doesn't keep a UI-facing `@Published` state.
/// Reads endpoint+token from the App Group (preferred) or UserDefaults.
@MainActor
struct ShareDraftSubmitter {
    /// Shared App Group identifier. Set this to the same group identifier you
    /// configure on both the app and share extension targets. The main app
    /// writes endpoint+token here when the user updates Settings.
    static let appGroup = "group.land.deno.orchid.capture"

    private var defaults: UserDefaults {
        UserDefaults(suiteName: Self.appGroup) ?? .standard
    }

    func send(_ draft: Draft) async {
        guard
            let s = defaults.string(forKey: "orchid.endpoint"),
            let url = URL(string: s)
        else { return }
        let token = defaults.string(forKey: "orchid.token") ?? ""

        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.withoutEscapingSlashes]

        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if !token.isEmpty {
            req.setValue(token, forHTTPHeaderField: "X-Capture-Token")
        }
        req.timeoutInterval = 30
        req.httpBody = try? encoder.encode(draft)
        _ = try? await URLSession.shared.data(for: req)
    }
}
