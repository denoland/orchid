import SwiftUI
import WatchKit

/// Single-screen capture surface for the watch. Visual language matches
/// the watch device drawn in the landing page (www landing.html →
/// `.watch-screen` / `.watch-ripple` / `.watch-dot`): a single dot
/// surrounded by amber-purple ripples that pulse with audio amplitude.
///
/// One gesture: long-press the dot to record, lift to submit.
struct CaptureScreen: View {
    @EnvironmentObject var settings: WatchSettings
    @EnvironmentObject var store:    DraftStore
    @StateObject private var recorder    = Recorder()
    @StateObject private var transcriber = Transcriber()

    @State private var phase: Phase = .idle
    @State private var showSettings = false
    @State private var micGranted: Bool? = nil

    enum Phase: Equatable {
        case idle
        case recording(start: Date)
        case submitting
        case done(SubmitOutcome)
    }

    var body: some View {
        ZStack {
            // The pressable record surface — ring + icon + status. The
            // gear button below is layered on top so it stays tappable
            // (DragGesture(minimumDistance: 0) on the parent would
            // otherwise eat the tap).
            ZStack {
                ring
                VStack(spacing: 4) {
                    Spacer(minLength: 0)
                    icon
                    Text(statusLine)
                        .font(.system(size: 11, weight: .medium))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .lineLimit(2)
                        .padding(.horizontal, 6)
                    if !transcriber.partialText.isEmpty && phase != .idle {
                        Text(transcriber.partialText)
                            .font(.system(size: 10))
                            .foregroundStyle(.secondary.opacity(0.8))
                            .lineLimit(2)
                            .truncationMode(.head)
                            .padding(.horizontal, 6)
                    }
                    Spacer(minLength: 0)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .contentShape(Rectangle())
            .gesture(holdGesture)

            // Chrome overlay (gear + queue badge) — not part of the
            // press surface.
            VStack {
                HStack {
                    Spacer()
                    Button {
                        showSettings = true
                    } label: {
                        Image(systemName: "gearshape")
                            .font(.system(size: 13, weight: .semibold))
                            .foregroundStyle(.secondary)
                            .padding(6)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
                Spacer()
                if store.pending > 0 {
                    HStack(spacing: 4) {
                        Image(systemName: "tray.full")
                            .font(.system(size: 9))
                        Text("\(store.pending) queued")
                            .font(.system(size: 10, weight: .medium))
                    }
                    .foregroundStyle(.secondary)
                    .padding(.bottom, 2)
                }
            }
        }
        .task { await primePermissions() }
        .sheet(isPresented: $showSettings) {
            SettingsView()
                .environmentObject(settings)
                .environmentObject(store)
        }
        .onChange(of: store.lastOutcome) { _, outcome in
            if let outcome { phase = .done(outcome) }
        }
    }

    // MARK: pieces

    /// Two pulsing rings + a centre dot, exactly the affordance from the
    /// landing-page mock. The opacity / scale is driven by the live audio
    /// level so the watch matches the user's voice while recording.
    private var ring: some View {
        let amp = CGFloat(recorder.level)
        let recording = recorder.isRecording

        return ZStack {
            Circle()
                .stroke(recording ? Color.purple.opacity(0.45 + 0.4 * amp) : Color.purple.opacity(0.18),
                        lineWidth: recording ? 3 + 4 * amp : 2)
                .scaleEffect(recording ? 1.0 + 0.10 * amp : 0.9)
                .animation(.easeOut(duration: 0.12), value: amp)
            Circle()
                .stroke(recording ? Color.orange.opacity(0.35 + 0.3 * amp) : Color.orange.opacity(0.10),
                        lineWidth: recording ? 2 + 3 * amp : 1)
                .scaleEffect(recording ? 1.15 + 0.20 * amp : 1.0)
                .animation(.easeOut(duration: 0.14), value: amp)
        }
        .padding(8)
    }

    private var icon: some View {
        Group {
            switch phase {
            case .idle:
                Image(systemName: "mic.fill")
                    .font(.system(size: 30, weight: .semibold))
                    .foregroundStyle(.primary)
            case .recording:
                Image(systemName: "waveform")
                    .font(.system(size: 26, weight: .semibold))
                    .foregroundStyle(.red)
                    .symbolEffect(.variableColor.iterative, isActive: true)
            case .submitting:
                ProgressView()
                    .progressViewStyle(.circular)
                    .scaleEffect(0.9)
            case .done(.sent):
                Image(systemName: "checkmark.circle.fill")
                    .font(.system(size: 26, weight: .semibold))
                    .foregroundStyle(.green)
            case .done(.queuedLocally):
                Image(systemName: "tray.and.arrow.down.fill")
                    .font(.system(size: 24, weight: .semibold))
                    .foregroundStyle(.orange)
            }
        }
        .padding(.top, 4)
    }

    private var statusLine: String {
        switch phase {
        case .idle:
            if micGranted == false { return "Mic blocked — open Settings to grant" }
            if !settings.isConfigured { return "Open iPhone app to pair" }
            return "Hold to capture"
        case .recording(let start):
            let dur = max(0, -start.timeIntervalSinceNow)
            return String(format: "Recording… %0.1fs", dur)
        case .submitting:
            return "Sending…"
        case .done(.sent):
            return "Captured"
        case .done(.queuedLocally(let why)):
            return "Queued · " + why
        }
    }

    // MARK: gesture

    private var holdGesture: some Gesture {
        // DragGesture with min distance 0 == "press". Pairs with onEnded
        // so we can read the press duration; SwiftUI's onLongPressGesture
        // resets state when the press is cancelled, which makes
        // mid-recording cancellation harder than it should be.
        DragGesture(minimumDistance: 0)
            .onChanged { _ in
                if case .idle = phase { startRecording() }
            }
            .onEnded { _ in
                if case .recording = phase { stopAndSubmit() }
            }
    }

    private func startRecording() {
        guard micGranted ?? true else { return }
        phase = .recording(start: Date())
        WKInterfaceDevice.current().play(.start)
        Task {
            do {
                try await recorder.start()
                transcriber.start()
            } catch {
                phase = .done(.queuedLocally(reason: "mic failed"))
            }
        }
    }

    private func stopAndSubmit() {
        transcriber.stop()
        let snapshot = transcriber.partialText
        guard let blob = recorder.stop() else {
            phase = .idle
            return
        }
        WKInterfaceDevice.current().play(.stop)
        phase = .submitting
        Task {
            let draft = Draft.voice(
                note: snapshot,
                audio: blob.data,
                mime: blob.mime,
                durationSec: blob.duration
            )
            await store.submit(draft)
            // Brief dwell on the result so the user sees it, then reset.
            try? await Task.sleep(nanoseconds: 1_500_000_000)
            phase = .idle
        }
    }

    private func primePermissions() async {
        // Mic permission. The system caches the answer so re-asking is
        // free, but only the first call actually pops the modal.
        let ok = await Recorder.requestMicPermission()
        micGranted = ok
        // Speech permission. Best-effort: if denied we still record.
        _ = await Transcriber.requestAuthorization()
    }
}

#Preview {
    let settings = WatchSettings()
    let store = DraftStore(settings: settings)
    return CaptureScreen()
        .environmentObject(settings)
        .environmentObject(store)
}
