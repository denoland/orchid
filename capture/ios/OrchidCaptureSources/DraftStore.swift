import Foundation

enum SubmitOutcome: Equatable {
    case sent(issueURL: String?)
    case queuedLocally(reason: String)
}

@MainActor
final class DraftStore: ObservableObject {
    @Published private(set) var pending: Int = 0
    @Published private(set) var lastOutcome: SubmitOutcome?

    private let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.withoutEscapingSlashes]
        return e
    }()

    private var endpoint: URL? {
        // Read from UserDefaults — set by Settings screen. Falls back to env
        // (works when launching from Xcode with a scheme env var) and finally
        // an Info.plist key for TestFlight/AdHoc builds.
        if let s = UserDefaults.standard.string(forKey: "orchid.endpoint"),
           let url = URL(string: s), !s.isEmpty {
            return url
        }
        if let s = ProcessInfo.processInfo.environment["ORCHID_CAPTURE_ENDPOINT"],
           let url = URL(string: s) {
            return url
        }
        if let s = Bundle.main.object(forInfoDictionaryKey: "OrchidCaptureEndpoint") as? String,
           let url = URL(string: s) {
            return url
        }
        return nil
    }

    private var token: String {
        UserDefaults.standard.string(forKey: "orchid.token")
            ?? ProcessInfo.processInfo.environment["ORCHID_CAPTURE_TOKEN"]
            ?? (Bundle.main.object(forInfoDictionaryKey: "OrchidCaptureToken") as? String)
            ?? ""
    }

    init() { recountPending() }

    func submit(_ draft: Draft) async {
        enqueue(draft)
        guard let endpoint else {
            lastOutcome = .queuedLocally(reason: "no endpoint configured")
            return
        }
        do {
            var req = URLRequest(url: endpoint)
            req.httpMethod = "POST"
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            if !token.isEmpty {
                req.setValue(token, forHTTPHeaderField: "X-Capture-Token")
            }
            req.httpBody = try encoder.encode(draft)
            req.timeoutInterval = 30
            let (data, resp) = try await URLSession.shared.data(for: req)
            guard let http = resp as? HTTPURLResponse,
                  (200..<300).contains(http.statusCode) else
            {
                let snippet = String(data: data, encoding: .utf8)?.prefix(120) ?? ""
                lastOutcome = .queuedLocally(reason: "HTTP error: \(snippet)")
                return
            }
            struct Ack: Decodable { var issue_url: String? }
            let ack = try? JSONDecoder().decode(Ack.self, from: data)
            lastOutcome = .sent(issueURL: ack?.issue_url)
        } catch {
            lastOutcome = .queuedLocally(reason: error.localizedDescription)
        }
    }

    private func enqueue(_ draft: Draft) {
        do {
            let line = try encoder.encode(draft)
            try appendLine(line, to: queueURL())
            recountPending()
        } catch {
            lastOutcome = .queuedLocally(reason: "queue write failed: \(error.localizedDescription)")
        }
    }

    private func queueURL() throws -> URL {
        let fm = FileManager.default
        let base = try fm.url(
            for: .documentDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        return base.appendingPathComponent("orchid-capture-queue.jsonl")
    }

    private func appendLine(_ data: Data, to url: URL) throws {
        if !FileManager.default.fileExists(atPath: url.path) {
            try Data().write(to: url)
        }
        let handle = try FileHandle(forWritingTo: url)
        defer { try? handle.close() }
        try handle.seekToEnd()
        try handle.write(contentsOf: data)
        try handle.write(contentsOf: Data([0x0A]))
    }

    private func recountPending() {
        guard
            let url = try? queueURL(),
            let data = try? Data(contentsOf: url),
            let text = String(data: data, encoding: .utf8)
        else {
            pending = 0
            return
        }
        pending = text.split(whereSeparator: \.isNewline).count
    }
}
