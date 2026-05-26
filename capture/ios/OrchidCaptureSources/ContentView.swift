import SwiftUI

struct ContentView: View {
    @EnvironmentObject private var drafts: DraftStore
    @StateObject private var recorder = Recorder()
    @StateObject private var transcriber = Transcriber()
    @State private var levels: [Double] = Array(repeating: 0, count: 60)
    @State private var showReview = false
    @State private var showSettings = false
    @State private var pendingAudio: (data: Data, duration: TimeInterval)?
    @State private var pendingTranscript: String = ""
    @State private var note: String = ""
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.route") private var routeRaw: String = CaptureRoute.clawpatrol.rawValue
    private var route: CaptureRoute { CaptureRoute(rawValue: routeRaw) ?? .clawpatrol }
    private var setupNeeded: Bool { endpoint.isEmpty }

    var body: some View {
        ZStack {
            Color.white.ignoresSafeArea()

            VStack {
                topBar
                Spacer()
                recordSurface
                Spacer()
                footer
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 16)
        }
        .sheet(isPresented: $showSettings) { SettingsView() }
        .sheet(isPresented: $showReview) {
            ReviewSheet(
                duration: pendingAudio?.duration ?? 0,
                transcript: pendingTranscript,
                note: $note,
                onDiscard: {
                    pendingAudio = nil
                    pendingTranscript = ""
                    note = ""
                    showReview = false
                },
                onCapture: {
                    Task {
                        await capture()
                        showReview = false
                    }
                }
            )
            .presentationDetents([.medium, .large])
        }
        .onReceive(recorder.$level) { level in
            levels.insert(level, at: 0)
            if levels.count > 60 { levels.removeLast() }
        }
    }

    private var topBar: some View {
        HStack(spacing: 8) {
            Circle().fill(Color.accentColor).frame(width: 8, height: 8)
            Text("orchid")
                .font(.system(.title3, design: .monospaced).weight(.semibold))
            routeChip
            Spacer()
            statusBadge
            Button {
                showSettings = true
            } label: {
                Image(systemName: "gearshape")
                    .font(.system(size: 17))
                    .foregroundStyle(.secondary)
            }
            .accessibilityLabel("Settings")
        }
    }

    private var routeChip: some View {
        Menu {
            ForEach(CaptureRoute.allCases) { r in
                Button(r.displayName) { routeRaw = r.rawValue }
            }
        } label: {
            HStack(spacing: 4) {
                Image(systemName: "tag")
                    .font(.system(size: 9))
                Text(route.displayName)
                    .font(.system(.caption2, design: .monospaced).weight(.medium))
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(Capsule().fill(Color.primary.opacity(0.06)))
        }
    }

    @ViewBuilder
    private var statusBadge: some View {
        if drafts.pending > 0 {
            Label("\(drafts.pending)", systemImage: "tray.fill")
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.secondary)
        } else if case .sent(let url) = drafts.lastOutcome {
            if let url, let u = URL(string: url) {
                Link(destination: u) {
                    Label("captured", systemImage: "checkmark.circle.fill")
                        .font(.system(.caption2, design: .monospaced))
                }
                .foregroundStyle(.green)
            } else {
                Label("captured", systemImage: "checkmark.circle.fill")
                    .font(.system(.caption2, design: .monospaced))
                    .foregroundStyle(.green)
            }
        } else if case .queuedLocally = drafts.lastOutcome {
            Label("queued", systemImage: "exclamationmark.triangle")
                .font(.system(.caption2, design: .monospaced))
                .foregroundStyle(.orange)
        } else if setupNeeded {
            Button("setup") { showSettings = true }
                .font(.system(.caption2, design: .monospaced))
                .buttonStyle(.bordered)
                .controlSize(.mini)
        }
    }

    private var recordSurface: some View {
        ZStack {
            WaveformRing(levels: recorder.isRecording ? levels : Array(repeating: 0, count: 60))
                .frame(width: 320, height: 320)
                .opacity(recorder.isRecording ? 1 : 0.25)

            Button {
                Task { await tapCenter() }
            } label: {
                ZStack {
                    Circle()
                        .fill(Color.black)
                    if recorder.isRecording {
                        RoundedRectangle(cornerRadius: 6)
                            .fill(Color.white)
                            .frame(width: 36, height: 36)
                    } else {
                        Image(systemName: "mic")
                            .font(.system(size: 44, weight: .light))
                            .foregroundStyle(.white)
                    }
                }
                .frame(width: 200, height: 200)
            }
            .buttonStyle(.plain)
            .animation(.easeInOut(duration: 0.18), value: recorder.isRecording)
        }
        .frame(maxWidth: .infinity)
    }

    private var footer: some View {
        VStack(spacing: 4) {
            if recorder.isRecording {
                Text(timeString(recorder.duration))
                    .font(.system(.callout, design: .monospaced))
                    .foregroundStyle(.secondary)
                if !transcriber.transcript.isEmpty {
                    Text(transcriber.transcript)
                        .font(.system(.footnote, design: .monospaced))
                        .foregroundStyle(.primary)
                        .multilineTextAlignment(.center)
                        .lineLimit(3)
                        .padding(.horizontal, 8)
                }
            } else if setupNeeded {
                Text("tap settings to add your orchid endpoint")
                    .font(.system(.callout, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            } else {
                Text("tap to capture a thought")
                    .font(.system(.callout, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
            if let err = recorder.lastError ?? transcriber.lastError {
                Text(err)
                    .font(.caption2)
                    .foregroundStyle(.red)
                    .lineLimit(2)
            }
        }
    }

    private func tapCenter() async {
        if setupNeeded {
            showSettings = true
            return
        }
        if recorder.isRecording {
            await recorder.stop()
            pendingTranscript = transcriber.transcript
            if let audio = recorder.takeAudio(), audio.duration > 0.4 {
                pendingAudio = audio
                showReview = true
            }
        } else {
            note = ""
            pendingTranscript = ""
            await recorder.start(transcriber: transcriber)
        }
    }

    private func capture() async {
        guard let audio = pendingAudio else { return }
        let voice = DraftVoice(
            mime: "audio/m4a",
            bytesBase64: audio.data.base64EncodedString(),
            durationSec: audio.duration
        )
        // Note carries the transcript when the user didn't type anything,
        // otherwise the typed note wins and transcript rides as extra text.
        let composedNote: String
        if note.isEmpty {
            composedNote = pendingTranscript
        } else if pendingTranscript.isEmpty {
            composedNote = note
        } else {
            composedNote = "\(note)\n\n\u{201c}\(pendingTranscript)\u{201d}"
        }
        let draft = Draft(
            id: ulidLike(),
            createdAt: Date(),
            source: "ios",
            kind: .voice,
            note: composedNote,
            voice: voice,
            target: DraftTarget(repo: "denoland/orchid", labels: [route.rawValue])
        )
        await drafts.submit(draft)
        pendingAudio = nil
        pendingTranscript = ""
        note = ""
    }

    private func timeString(_ t: TimeInterval) -> String {
        let m = Int(t) / 60
        let s = Int(t) % 60
        let cs = Int((t - floor(t)) * 10)
        return String(format: "%02d:%02d.%d", m, s, cs)
    }
}

private struct ReviewSheet: View {
    let duration: TimeInterval
    let transcript: String
    @Binding var note: String
    let onDiscard: () -> Void
    let onCapture: () -> Void
    @FocusState private var noteFocused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Image(systemName: "waveform")
                Text(String(format: "%.1fs voice note", duration))
                    .font(.system(.callout, design: .monospaced))
                Spacer()
            }
            .foregroundStyle(.secondary)

            if !transcript.isEmpty {
                Text(transcript)
                    .font(.system(.body, design: .monospaced))
                    .padding(10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.black.opacity(0.04))
                    .cornerRadius(6)
            }

            TextField(
                transcript.isEmpty
                    ? "what's this about?"
                    : "optional: add context to the transcript",
                text: $note,
                axis: .vertical
            )
            .lineLimit(3...6)
            .font(.system(.body, design: .monospaced))
            .padding(10)
            .background(Color.black.opacity(0.04))
            .cornerRadius(6)
            .focused($noteFocused)

            HStack {
                Button("Discard", role: .destructive, action: onDiscard)
                    .buttonStyle(.bordered)
                Spacer()
                Button("Capture", action: onCapture)
                    .buttonStyle(.borderedProminent)
                    .tint(.black)
            }
        }
        .padding(20)
    }
}
