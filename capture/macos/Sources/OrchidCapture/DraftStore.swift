import Foundation
import AppKit

/// Submission result surfaced back to the UI so the composer can show
/// "captured → <issue url>" or "queued locally — endpoint not reachable".
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

    /// Live endpoint, set by Settings; falls back to ORCHID_CAPTURE_ENDPOINT.
    private var endpoint: URL?
    /// X-Capture-Token, set by Settings; falls back to ORCHID_CAPTURE_TOKEN.
    private var token: String = ""

    init() {
        if let s = ProcessInfo.processInfo.environment["ORCHID_CAPTURE_ENDPOINT"],
           let url = URL(string: s) {
            endpoint = url
        }
        if let t = ProcessInfo.processInfo.environment["ORCHID_CAPTURE_TOKEN"] {
            token = t
        }
        recountPending()
    }

    func updateEndpoint(_ url: URL?) {
        if let url, !url.absoluteString.isEmpty {
            endpoint = url
        } else if ProcessInfo.processInfo.environment["ORCHID_CAPTURE_ENDPOINT"] == nil {
            endpoint = nil
        }
    }

    func updateToken(_ t: String) {
        if !t.isEmpty {
            token = t
        }
    }

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
            guard let http = resp as? HTTPURLResponse else {
                lastOutcome = .queuedLocally(reason: "no http response")
                return
            }
            guard (200..<300).contains(http.statusCode) else {
                let snippet = String(data: data, encoding: .utf8)?.prefix(120) ?? ""
                lastOutcome = .queuedLocally(reason: "HTTP \(http.statusCode): \(snippet)")
                return
            }
            // Server returns { ok, id, issue_url, asset_url }
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
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        ).appendingPathComponent("OrchidCapture", isDirectory: true)
        try fm.createDirectory(at: base, withIntermediateDirectories: true)
        return base.appendingPathComponent("queue.jsonl")
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

    func clear() {
        if let url = try? queueURL() {
            try? FileManager.default.removeItem(at: url)
            recountPending()
        }
    }

    func revealQueueInFinder() {
        if let url = try? queueURL() {
            NSWorkspace.shared.activateFileViewerSelecting([url])
        }
    }
}
