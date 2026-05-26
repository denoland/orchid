import SwiftUI
import Compression

/// Live tmux pane viewer.
///
/// Opens an SSE stream at /api/pane/stream?s=<tmux>. Each frame is
/// `data: z:<base64-gzip>` — we gunzip, strip ANSI escapes, and show
/// the latest snapshot. Send keystrokes back with POST /api/pane.
struct PaneView: View {
    let tmux: String
    let title: String

    @AppStorage("orchid.endpoint")    private var endpoint: String = ""
    @AppStorage("orchid.http_secret") private var httpSecret: String = ""

    @State private var screen: String = ""
    @State private var input: String = ""
    @State private var streamTask: Task<Void, Never>?
    @State private var connected = false
    @State private var errorMsg: String?
    @FocusState private var inputFocused: Bool

    var body: some View {
        VStack(spacing: 0) {
            ScrollView {
                Text(screen.isEmpty ? "connecting…" : screen)
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundStyle(.white)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(12)
                    .textSelection(.enabled)
            }
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
            inputBar
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

    // ─── key bar (Esc / Tab / Ctrl-C / Enter shortcuts) ─────────────
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

    // ─── input bar ──────────────────────────────────────────────────
    private var inputBar: some View {
        HStack(spacing: 8) {
            TextField("type, send with ⏎", text: $input, axis: .horizontal)
                .font(.system(size: 13, design: .monospaced))
                .foregroundStyle(.white)
                .padding(.horizontal, 10).padding(.vertical, 8)
                .background(Color(white: 0.18))
                .cornerRadius(6)
                .focused($inputFocused)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .submitLabel(.send)
                .onSubmit {
                    let payload = input + "\r"
                    input = ""
                    Task { await send(payload) }
                }
            Button {
                let payload = input
                input = ""
                Task { await send(payload) }
            } label: {
                Image(systemName: "paperplane.fill")
                    .foregroundStyle(.white)
                    .padding(10)
                    .background(Color.accentColor)
                    .clipShape(Circle())
            }
            .disabled(input.isEmpty)
        }
        .padding(8)
        .background(Color.black)
    }

    // ─── stream lifecycle ───────────────────────────────────────────
    private func start() {
        guard let base = baseURL(),
              let url = URL(string: "\(base.absoluteString)/api/pane/stream?s=\(tmux)&cols=80&rows=30")
        else { errorMsg = "bad endpoint"; return }
        streamTask?.cancel()
        streamTask = Task {
            var req = URLRequest(url: url)
            req.timeoutInterval = 0
            if !httpSecret.isEmpty {
                req.setValue("Bearer \(httpSecret)", forHTTPHeaderField: "Authorization")
            }
            req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
            do {
                let (bytes, resp) = try await URLSession.shared.bytes(for: req)
                if let http = resp as? HTTPURLResponse, http.statusCode >= 400 {
                    errorMsg = "stream http \(http.statusCode)"; return
                }
                errorMsg = nil
                connected = true
                for try await line in bytes.lines {
                    guard !Task.isCancelled else { return }
                    if line.hasPrefix("data: z:") {
                        let b64 = String(line.dropFirst("data: z:".count))
                        if let snap = decodeFrame(b64) {
                            await MainActor.run { self.screen = snap }
                        }
                    }
                }
            } catch {
                if !Task.isCancelled { errorMsg = error.localizedDescription }
            }
            connected = false
        }
    }

    private func stop() {
        streamTask?.cancel()
        streamTask = nil
    }

    // ─── send keystrokes ────────────────────────────────────────────
    private func send(_ s: String) async {
        guard !s.isEmpty,
              let base = baseURL(),
              let url = URL(string: "\(base.absoluteString)/api/pane?s=\(tmux)")
        else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.httpBody = s.data(using: .utf8)
        if !httpSecret.isEmpty {
            req.setValue("Bearer \(httpSecret)", forHTTPHeaderField: "Authorization")
        }
        do { _ = try await URLSession.shared.data(for: req) }
        catch { errorMsg = error.localizedDescription }
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

// ─── gunzip + ANSI strip ────────────────────────────────────────────

private func decodeFrame(_ b64: String) -> String? {
    guard let gz = Data(base64Encoded: b64) else { return nil }
    guard let raw = gunzip(gz) else { return nil }
    let s = String(decoding: raw, as: UTF8.self)
    return stripANSI(s)
}

/// In-place gunzip via Compression framework. Inflates a complete
/// gzip stream into one buffer — pane frames are small (a few KB).
private func gunzip(_ data: Data) -> Data? {
    let chunk = 64 * 1024
    var out = Data()
    return data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Data? in
        let src = raw.bindMemory(to: UInt8.self).baseAddress!
        let stream = UnsafeMutablePointer<compression_stream>.allocate(capacity: 1)
        defer { stream.deallocate() }
        var s = compression_stream(dst_ptr: UnsafeMutablePointer<UInt8>.allocate(capacity: chunk),
                                   dst_size: chunk,
                                   src_ptr: src,
                                   src_size: data.count,
                                   state: nil)
        defer { s.dst_ptr.advanced(by: -(chunk - s.dst_size)).deallocate() }
        let dstStart = s.dst_ptr
        guard compression_stream_init(&s, COMPRESSION_STREAM_DECODE, COMPRESSION_ZLIB) == COMPRESSION_STATUS_OK else { return nil }
        defer { compression_stream_destroy(&s) }
        // Skip gzip header (10 bytes minimum): magic 1f 8b, method 08, flags,
        // mtime(4), xfl, os. Trailing 8 bytes (crc32 + isize) ignored by raw zlib.
        let headerLen = 10
        if data.count <= headerLen { return nil }
        s.src_ptr  = src.advanced(by: headerLen)
        s.src_size = data.count - headerLen - 8
        while true {
            let status = compression_stream_process(&s, Int32(COMPRESSION_STREAM_FINALIZE.rawValue))
            let produced = chunk - s.dst_size
            if produced > 0 {
                out.append(Data(bytes: dstStart, count: produced))
                s.dst_ptr = dstStart
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

/// Drop ANSI CSI / OSC escape sequences and most control bytes so the
/// text is legible in SwiftUI without a full terminal renderer.
private func stripANSI(_ s: String) -> String {
    var out = String(); out.reserveCapacity(s.count)
    let chars = Array(s.unicodeScalars)
    var i = 0
    while i < chars.count {
        let c = chars[i]
        if c == "\u{001B}" {
            // ESC [...] sequence
            i += 1
            if i < chars.count && (chars[i] == "[" || chars[i] == "(" || chars[i] == ")") {
                i += 1
                while i < chars.count {
                    let x = chars[i]; i += 1
                    if (x.value >= 0x40 && x.value <= 0x7E) { break }
                }
            } else if i < chars.count && chars[i] == "]" {
                // OSC — terminated by BEL or ESC \
                i += 1
                while i < chars.count {
                    if chars[i] == "\u{0007}" { i += 1; break }
                    if chars[i] == "\u{001B}" && i + 1 < chars.count && chars[i+1] == "\\" { i += 2; break }
                    i += 1
                }
            } else {
                i += 1
            }
            continue
        }
        if c.value < 0x20 && c != "\n" && c != "\t" { i += 1; continue }
        out.unicodeScalars.append(c)
        i += 1
    }
    return out
}
