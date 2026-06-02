import SwiftUI

/// Capture — a single composer consistent with the rest of the app:
/// type a thought (or hold the mic to dictate), pick a label, spawn it as
/// an inbox issue via /api/drafts. Replaces the old full-screen voice ring.
struct ContentView: View {
    @EnvironmentObject private var drafts: DraftStore
    @StateObject private var recorder = Recorder()
    @StateObject private var transcriber = Transcriber()

    @State private var note = ""
    @State private var pendingAudio: (data: Data, duration: TimeInterval)?
    @FocusState private var focused: Bool

    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.route") private var routeRaw: String = CaptureRoute.clawpatrol.rawValue
    private var route: CaptureRoute { CaptureRoute(rawValue: routeRaw) ?? .clawpatrol }
    private var canSpawn: Bool { !endpoint.isEmpty && !note.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            composer
            statusLine
            Spacer()
        }
        .padding(16)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .background(Theme.surface)
        .navigationTitle("Capture")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar { ToolbarItem(placement: .topBarLeading) { routeMenu } }
        .onReceive(transcriber.$transcript) { t in
            if recorder.isRecording, !t.isEmpty { note = t }
        }
    }

    // ── composer card ─────────────────────────────────────────────────
    private var composer: some View {
        VStack(alignment: .leading, spacing: 12) {
            TextField("Capture a thought…", text: $note, axis: .vertical)
                .font(.system(size: 15))
                .lineLimit(3...8)
                .focused($focused)
                .tint(Theme.orchid)

            HStack(spacing: 10) {
                if recorder.isRecording {
                    HStack(spacing: 6) {
                        Circle().fill(Theme.rose).frame(width: 7, height: 7)
                        Text(time(recorder.duration)).font(Theme.mono(11)).foregroundStyle(Theme.muted)
                    }
                }
                Spacer()
                Button { Task { await toggleMic() } } label: {
                    Image(systemName: recorder.isRecording ? "stop.circle.fill" : "mic")
                        .font(.system(size: 18))
                        .foregroundStyle(recorder.isRecording ? Theme.rose : Theme.muted)
                }
                Button { Task { await spawn() } } label: {
                    Text("Spawn")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(canSpawn ? .white : Theme.faint)
                        .padding(.horizontal, 14).padding(.vertical, 7)
                        .background(canSpawn ? Theme.ink : Theme.line, in: Capsule())
                }
                .disabled(!canSpawn)
            }
        }
        .padding(14)
        .background(Theme.panel, in: RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).stroke(Theme.line, lineWidth: 1))
    }

    private var routeMenu: some View {
        Menu {
            ForEach(CaptureRoute.allCases) { r in
                Button { routeRaw = r.rawValue } label: {
                    if r.rawValue == routeRaw { Label(r.displayName, systemImage: "checkmark") }
                    else { Text(r.displayName) }
                }
            }
        } label: {
            HStack(spacing: 4) {
                Image(systemName: "tag").font(.system(size: 10))
                Text(route.displayName).font(Theme.mono(11, weight: .medium))
            }
            .foregroundStyle(Theme.muted)
        }
    }

    @ViewBuilder private var statusLine: some View {
        if endpoint.isEmpty {
            label("Set your endpoint in Settings", "exclamationmark.circle", Theme.amber)
        } else if recorder.isRecording {
            label("Listening…", "waveform", Theme.muted)
        } else if case .sent(let url) = drafts.lastOutcome {
            if let url, let u = URL(string: url) {
                Link(destination: u) { label("Captured — open issue", "checkmark.circle.fill", Theme.emerald) }
            } else {
                label("Captured", "checkmark.circle.fill", Theme.emerald)
            }
        } else if case .queuedLocally(let reason) = drafts.lastOutcome {
            label("Queued — \(reason)", "tray.full", Theme.amber)
        } else if drafts.pending > 0 {
            label("\(drafts.pending) queued", "tray.full", Theme.muted)
        } else if let err = recorder.lastError ?? transcriber.lastError {
            label(err, "exclamationmark.triangle", Theme.rose)
        }
    }

    private func label(_ text: String, _ icon: String, _ color: Color) -> some View {
        HStack(spacing: 6) {
            Image(systemName: icon).font(.system(size: 12))
            Text(text).font(Theme.mono(11)).lineLimit(2)
        }
        .foregroundStyle(color)
    }

    // ── actions ───────────────────────────────────────────────────────
    private func toggleMic() async {
        if recorder.isRecording {
            await recorder.stop()
            if let audio = recorder.takeAudio(), audio.duration > 0.4 { pendingAudio = audio }
        } else {
            focused = false
            await recorder.start(transcriber: transcriber)
        }
    }

    private func spawn() async {
        if recorder.isRecording { await recorder.stop() }
        let body = note.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty else { return }
        let target = DraftTarget(repo: "denoland/orchid", labels: [route.rawValue])
        let draft: Draft
        if let audio = pendingAudio {
            draft = Draft(id: ulidLike(), createdAt: Date(), source: "ios", kind: .voice,
                          note: body,
                          voice: DraftVoice(mime: "audio/m4a",
                                            bytesBase64: audio.data.base64EncodedString(),
                                            durationSec: audio.duration),
                          target: target)
        } else {
            draft = Draft(id: ulidLike(), createdAt: Date(), source: "ios", kind: .text,
                          note: body, text: DraftText(body: body), target: target)
        }
        await drafts.submit(draft)
        note = ""
        pendingAudio = nil
        focused = false
    }

    private func time(_ t: TimeInterval) -> String {
        String(format: "%01d:%02d", Int(t) / 60, Int(t) % 60)
    }
}
