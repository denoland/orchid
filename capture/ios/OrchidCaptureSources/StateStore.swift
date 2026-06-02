import Foundation
import SwiftUI

/// Live swarm state. Mirrors www/src/App.tsx: one WebSocket to
/// /api/events/ws is the steady state (server pushes `{t:"state",state}`
/// frames); a /api/state poll is the fallback while the socket is down.
/// Auth is Bearer http_secret on both, read from the same @AppStorage keys
/// the Capture flow uses.
@MainActor
final class StateStore: ObservableObject {
    @Published var state = OrchState.empty
    @Published var connected = false
    @Published var lastError: String?

    enum Link { case live, polling, offline }
    @Published var link: Link = .offline

    private var task: Task<Void, Never>?

    private var endpoint: String { UserDefaults.standard.string(forKey: "orchid.endpoint") ?? "" }
    private var secret: String { UserDefaults.standard.string(forKey: "orchid.http_secret") ?? "" }

    var configured: Bool { !endpoint.isEmpty }

    private static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        return d
    }()

    // ── lifecycle ───────────────────────────────────────────────────
    func start() {
        guard task == nil, configured else { return }
        task = Task { await runLoop() }
    }

    func stop() {
        task?.cancel()
        task = nil
        connected = false
        link = .offline
    }

    /// Restart after settings change.
    func reconnect() {
        stop()
        start()
    }

    // ── reconnect loop ──────────────────────────────────────────────
    private func runLoop() async {
        var backoff: UInt64 = 1
        while !Task.isCancelled {
            // Seed/refresh immediately via the REST endpoint so the UI has
            // data even if the socket is slow to open.
            await pollOnce()
            do {
                try await streamWS()        // returns only on socket close/err
            } catch {
                lastError = error.localizedDescription
            }
            connected = false
            if Task.isCancelled { break }
            // While the socket is down, poll on a tight cadence.
            link = .polling
            for _ in 0..<Int(backoff) {
                if Task.isCancelled { break }
                try? await Task.sleep(nanoseconds: 5 * 1_000_000_000)
                await pollOnce()
            }
            backoff = min(backoff * 2, 6)   // ~ up to 30s between WS retries
        }
    }

    // ── REST poll ───────────────────────────────────────────────────
    private func pollOnce() async {
        guard let url = api("/api/state") else { return }
        var req = URLRequest(url: url, timeoutInterval: 8)
        authorize(&req)
        do {
            let (data, resp) = try await URLSession.shared.data(for: req)
            guard let http = resp as? HTTPURLResponse else { return }
            if http.statusCode == 401 || http.statusCode == 403 {
                lastError = "auth rejected — check Dashboard token in Settings"
                return
            }
            if http.statusCode >= 400 { lastError = "http \(http.statusCode)"; return }
            let s = try Self.decoder.decode(OrchState.self, from: data)
            state = s
            lastError = nil
            if link == .offline { link = .polling }
        } catch {
            lastError = error.localizedDescription
            link = .offline
        }
    }

    // ── WebSocket ───────────────────────────────────────────────────
    private func streamWS() async throws {
        guard let url = wsURL("/api/events/ws") else { return }
        var req = URLRequest(url: url)
        authorize(&req)
        let ws = URLSession.shared.webSocketTask(with: req)
        ws.resume()
        defer { ws.cancel(with: .goingAway, reason: nil) }

        while !Task.isCancelled {
            let msg = try await ws.receive()
            switch msg {
            case .string(let text):
                ingest(Data(text.utf8))
            case .data(let data):
                ingest(data)
            @unknown default:
                break
            }
            if !connected { connected = true; link = .live; lastError = nil }
        }
    }

    private struct Frame: Decodable { let t: String; let state: OrchState? }

    private func ingest(_ data: Data) {
        guard let frame = try? Self.decoder.decode(Frame.self, from: data) else { return }
        if frame.t == "state", let s = frame.state {
            state = s
            lastError = nil
        }
    }

    // ── url helpers ─────────────────────────────────────────────────
    private func authorize(_ req: inout URLRequest) {
        if !secret.isEmpty { req.setValue("Bearer \(secret)", forHTTPHeaderField: "Authorization") }
    }

    /// Strip a trailing /api/* path from the configured endpoint to get the base.
    private func base() -> URLComponents? {
        guard var c = URLComponents(string: endpoint) else { return nil }
        var path = c.path
        for suffix in ["/api/drafts", "/api/state", "/"] where path.hasSuffix(suffix) {
            path.removeLast(suffix.count); break
        }
        c.path = path
        c.query = nil
        return c
    }

    private func api(_ p: String) -> URL? {
        guard var c = base() else { return nil }
        c.path += p
        return c.url
    }

    private func wsURL(_ p: String) -> URL? {
        guard var c = base() else { return nil }
        c.scheme = (c.scheme == "https") ? "wss" : "ws"
        c.path += p
        return c.url
    }
}
