import SwiftUI
import UIKit
import Compression
import SwiftTerm

/// Live tmux pane viewer powered by SwiftTerm. The orch SSE stream
/// delivers gzipped tmux capture-pane frames; we gunzip and feed the
/// raw bytes (with ANSI escapes intact) to a SwiftTerm view so the
/// TUI renders properly. Keystrokes from the on-screen keyboard +
/// quick-key bar POST back through /api/pane.
struct PaneView: View {
    let tmux: String
    let title: String

    @AppStorage("orchid.endpoint")    private var endpoint: String = ""
    @AppStorage("orchid.http_secret") private var httpSecret: String = ""

    @State private var streamTask: Task<Void, Never>?
    @State private var errorMsg: String?

    // The TerminalView is created once and shared with the bridge so we
    // can `.feed()` it from the network task.
    @StateObject private var bridge = TermBridge()

    var body: some View {
        VStack(spacing: 0) {
            TerminalRepresentable(bridge: bridge, onSend: { data in
                Task { await self.send(data: data) }
            })
            .background(Color.black)

            if let err = errorMsg {
                Text(err)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal, 12).padding(.vertical, 6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(white: 0.08))
            }

            keyBar
        }
        .background(Color.black)
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackground(.black, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
        .preferredColorScheme(.dark)
        .onAppear { start() }
        .onDisappear { stop() }
    }

    // ─── key bar (Esc / Tab / arrows / Ctrl-C / Enter) ───────────────
    private var keyBar: some View {
        HStack(spacing: 6) {
            keyButton("Esc")  { Task { await send("\u{1B}") } }
            keyButton("Tab")  { Task { await send("\t") } }
            keyButton("↑")    { Task { await send("\u{1B}[A") } }
            keyButton("↓")    { Task { await send("\u{1B}[B") } }
            keyButton("⌃C")   { Task { await send("\u{03}") } }
            Spacer()
            keyButton("⏎", primary: true) { Task { await send("\r") } }
        }
        .padding(.horizontal, 8).padding(.vertical, 6)
        .background(Color(white: 0.12))
    }

    private func keyButton(_ label: String, primary: Bool = false, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(label)
                .font(.system(size: 12, weight: .medium, design: .monospaced))
                .foregroundStyle(primary ? .black : .white)
                .padding(.horizontal, 10).padding(.vertical, 5)
                .background(primary ? Color.white : Color(white: 0.22))
                .cornerRadius(6)
        }
    }

    // ─── SSE stream lifecycle ───────────────────────────────────────
    private func start() {
        guard let base = baseURL(),
              let url = URL(string: "\(base.absoluteString)/api/pane/stream?s=\(tmux)&cols=80&rows=30")
        else { errorMsg = "bad endpoint"; return }
        streamTask?.cancel()
        streamTask = Task {
            var req = URLRequest(url: url)
            req.timeoutInterval = 0
            req.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
            if !httpSecret.isEmpty {
                req.setValue("Bearer \(httpSecret)", forHTTPHeaderField: "Authorization")
            }
            req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
            let session = makeSSESession()
            let stream = AsyncThrowingStream<Data, Error> { continuation in
                let delegate = SSEDelegate(continuation: continuation)
                let task = session.dataTask(with: req)
                task.delegate = delegate
                continuation.onTermination = { _ in task.cancel() }
                task.resume()
            }
            do {
                var buffer = Data()
                for try await chunk in stream {
                    guard !Task.isCancelled else { return }
                    buffer.append(chunk)
                    while let nl = buffer.firstIndex(of: 0x0A) {
                        let line = buffer.prefix(upTo: nl)
                        buffer.removeSubrange(0...nl)
                        guard let s = String(data: line, encoding: .utf8) else { continue }
                        let trimmed = s.hasSuffix("\r") ? String(s.dropLast()) : s
                        if trimmed.hasPrefix("data: z:") {
                            let b64 = String(trimmed.dropFirst("data: z:".count))
                            if let raw = decodeFrame(b64) {
                                let clear: [UInt8] = [0x1B, 0x5B, 0x48, 0x1B, 0x5B, 0x32, 0x4A]
                                await bridge.feed(clear + Array(raw))
                            }
                        }
                    }
                }
            } catch {
                if !Task.isCancelled {
                    await MainActor.run { errorMsg = error.localizedDescription }
                }
            }
        }
    }

    private func makeSSESession() -> URLSession {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 0
        cfg.timeoutIntervalForResource = 0
        cfg.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        cfg.waitsForConnectivity = false
        return URLSession(configuration: cfg)
    }

    private func stop() {
        streamTask?.cancel()
        streamTask = nil
    }

    // ─── send keystrokes ────────────────────────────────────────────
    private func send(_ s: String) async {
        await send(data: Array(s.utf8))
    }
    private func send(data bytes: [UInt8]) async {
        guard !bytes.isEmpty,
              let base = baseURL(),
              let url = URL(string: "\(base.absoluteString)/api/pane?s=\(tmux)")
        else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.httpBody = Data(bytes)
        if !httpSecret.isEmpty {
            req.setValue("Bearer \(httpSecret)", forHTTPHeaderField: "Authorization")
        }
        do { _ = try await URLSession.shared.data(for: req) }
        catch {
            await MainActor.run { errorMsg = error.localizedDescription }
        }
    }

    private func baseURL() -> URL? {
        guard let u = URL(string: endpoint) else { return nil }
        var s = u.absoluteString
        for suffix in ["/api/drafts", "/api/state", "/"] {
            if s.hasSuffix(suffix) { s.removeLast(suffix.count); break }
        }
        return URL(string: s)
    }
}

// ─── SSE delegate ────────────────────────────────────────────────────
// URLSession's bytes(for:) buffers SSE bodies before delivering lines,
// which made the pane appear to hang for ~30s before any frame showed.
// A plain delegate-driven data task pushes each `didReceive` straight
// into an AsyncThrowingStream so frames arrive as the server flushes.

final class SSEDelegate: NSObject, URLSessionDataDelegate {
    let continuation: AsyncThrowingStream<Data, Error>.Continuation
    init(continuation: AsyncThrowingStream<Data, Error>.Continuation) {
        self.continuation = continuation
    }
    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask,
                    didReceive data: Data) {
        continuation.yield(data)
    }
    func urlSession(_ session: URLSession, task: URLSessionTask,
                    didCompleteWithError error: Error?) {
        if let error { continuation.finish(throwing: error) }
        else { continuation.finish() }
    }
}

// ─── SwiftTerm bridge ────────────────────────────────────────────────

/// Shared handle to a TerminalView that SwiftUI lifecycle owns and the
/// SSE task feeds into. Lives in a StateObject so it survives view
/// re-renders without dropping scrollback.
final class TermBridge: ObservableObject {
    var terminal: TerminalView?

    @MainActor
    func feed(_ bytes: [UInt8]) {
        terminal?.feed(byteArray: bytes[...])
    }
}

private struct TerminalRepresentable: UIViewRepresentable {
    let bridge: TermBridge
    let onSend: ([UInt8]) -> Void

    func makeCoordinator() -> Coordinator { Coordinator(onSend: onSend) }

    func makeUIView(context: Context) -> TerminalView {
        let tv = TerminalView()
        tv.terminalDelegate = context.coordinator
        tv.backgroundColor = .black
        tv.nativeForegroundColor = .white
        tv.nativeBackgroundColor = .black
        // Match the cols/rows we request from the server.
        bridge.terminal = tv
        return tv
    }

    func updateUIView(_ uiView: TerminalView, context: Context) {}

    final class Coordinator: NSObject, TerminalViewDelegate {
        let onSend: ([UInt8]) -> Void
        init(onSend: @escaping ([UInt8]) -> Void) { self.onSend = onSend }
        func send(source: TerminalView, data: ArraySlice<UInt8>) { onSend(Array(data)) }
        func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {}
        func scrolled(source: TerminalView, position: Double) {}
        func setTerminalTitle(source: TerminalView, title: String) {}
        func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
        func requestOpenLink(source: TerminalView, link: String, params: [String : String]) {}
        func bell(source: TerminalView) {}
        func clipboardCopy(source: TerminalView, content: Data) {}
        func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}
    }
}

// ─── gunzip ─────────────────────────────────────────────────────────

private func decodeFrame(_ b64: String) -> Data? {
    guard let gz = Data(base64Encoded: b64) else { return nil }
    return gunzip(gz)
}

/// Gunzip via Apple's libcompression. orch frames are gzip — magic
/// 1f 8b 08 ... followed by raw DEFLATE then a CRC+ISIZE trailer.
/// COMPRESSION_ZLIB on Apple platforms decodes raw deflate (no zlib
/// header), so strip the 10-byte gzip header + 8-byte trailer and
/// hand the deflate body to the framework.
private func gunzip(_ data: Data) -> Data? {
    guard data.count > 18, data[0] == 0x1f, data[1] == 0x8b else { return nil }
    let chunk = 64 * 1024
    var out = Data()
    return data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Data? in
        let src = raw.bindMemory(to: UInt8.self).baseAddress!
        let dst = UnsafeMutablePointer<UInt8>.allocate(capacity: chunk)
        defer { dst.deallocate() }
        var s = compression_stream(dst_ptr: dst, dst_size: chunk,
                                   src_ptr: src.advanced(by: 10),
                                   src_size: data.count - 10 - 8,
                                   state: nil)
        guard compression_stream_init(&s, COMPRESSION_STREAM_DECODE, COMPRESSION_ZLIB)
              == COMPRESSION_STATUS_OK else { return nil }
        defer { compression_stream_destroy(&s) }
        while true {
            let status = compression_stream_process(&s, Int32(COMPRESSION_STREAM_FINALIZE.rawValue))
            let produced = chunk - s.dst_size
            if produced > 0 {
                out.append(Data(bytes: dst, count: produced))
                s.dst_ptr = dst
                s.dst_size = chunk
            }
            switch status {
            case COMPRESSION_STATUS_END: return out
            case COMPRESSION_STATUS_OK:  continue
            default: return nil
            }
        }
    }
}
