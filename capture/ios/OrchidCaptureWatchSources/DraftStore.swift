import Foundation

/// Mirrors the macOS DraftStore lifecycle: enqueue to a local JSONL
/// file first (so a flaky network never loses a capture), then attempt
/// HTTP submit. The watch is offline more often than not, so the queue
/// path is the common case, not the exception.

enum SubmitOutcome: Equatable {
    case sent(issueURL: String?)
    case queuedLocally(reason: String)
}

@MainActor
final class DraftStore: ObservableObject {
    @Published private(set) var pending: Int = 0
    @Published private(set) var lastOutcome: SubmitOutcome?
    @Published private(set) var inFlight: Bool = false

    private let settings: WatchSettings

    private let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.withoutEscapingSlashes]
        return e
    }()

    init(settings: WatchSettings) {
        self.settings = settings
        recountPending()
    }

    func submit(_ draft: Draft) async {
        enqueue(draft)
        guard let endpoint = settings.endpoint else {
            lastOutcome = .queuedLocally(reason: "no endpoint — open the iPhone app to sync")
            return
        }
        inFlight = true
        defer { inFlight = false }

        do {
            var req = URLRequest(url: endpoint)
            req.httpMethod = "POST"
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            if !settings.token.isEmpty {
                req.setValue(settings.token, forHTTPHeaderField: "X-Capture-Token")
            }
            req.timeoutInterval = 30
            req.httpBody = try encoder.encode(draft)

            let (data, resp) = try await URLSession.shared.data(for: req)
            guard let http = resp as? HTTPURLResponse else {
                lastOutcome = .queuedLocally(reason: "no http response")
                return
            }
            guard (200..<300).contains(http.statusCode) else {
                let snippet = String(data: data, encoding: .utf8)?.prefix(100) ?? ""
                lastOutcome = .queuedLocally(reason: "HTTP \(http.statusCode) · \(snippet)")
                return
            }
            struct Ack: Decodable { var issue_url: String? }
            let ack = try? JSONDecoder().decode(Ack.self, from: data)
            lastOutcome = .sent(issueURL: ack?.issue_url)
            // Once at least one draft has gone out cleanly we no longer
            // need it in the on-disk queue. The simplest correctness
            // story is: drain the queue after each successful submit.
            drainQueueAfterSuccess(matching: draft.id)
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
        let base = try fm.url(for: .documentDirectory,
                              in: .userDomainMask,
                              appropriateFor: nil,
                              create: true)
        return base.appendingPathComponent("orchid-watch-queue.jsonl")
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

    /// After a successful submit, remove the draft (by id) from the
    /// queue so it doesn't replay on the next drain pass.
    private func drainQueueAfterSuccess(matching id: String) {
        guard
            let url = try? queueURL(),
            let data = try? Data(contentsOf: url),
            let text = String(data: data, encoding: .utf8)
        else { return }
        let remaining = text
            .split(separator: "\n", omittingEmptySubsequences: true)
            .filter { line in
                // Cheap substring filter is enough — ids are 18+ chars
                // and won't collide with json field text in practice.
                !line.contains("\"\(id)\"")
            }
        let joined = remaining.joined(separator: "\n")
        try? (joined.isEmpty ? "" : joined + "\n").data(using: .utf8)?
            .write(to: url, options: .atomic)
        recountPending()
    }

    /// Re-submit anything still sitting in the local queue. Called by
    /// Settings → "Retry queued". Walks the file once; each row is
    /// resubmitted independently so a single failure doesn't block the rest.
    func drainQueue() async {
        guard
            let url = try? queueURL(),
            let data = try? Data(contentsOf: url),
            let text = String(data: data, encoding: .utf8)
        else { return }
        let lines = text.split(separator: "\n", omittingEmptySubsequences: true)
        let decoder: JSONDecoder = {
            let d = JSONDecoder()
            d.dateDecodingStrategy = .iso8601
            return d
        }()
        for line in lines {
            guard let bytes = line.data(using: .utf8),
                  let draft = try? decoder.decode(Draft.self, from: bytes)
            else { continue }
            await submit(draft)
        }
    }

    func clearQueue() {
        if let url = try? queueURL() {
            try? FileManager.default.removeItem(at: url)
        }
        recountPending()
    }
}
