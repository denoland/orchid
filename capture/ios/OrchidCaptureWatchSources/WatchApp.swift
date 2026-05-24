import SwiftUI

/// Orchid Capture for watchOS. Companion to the iPhone app — same
/// "hold to capture a voice note" affordance you see in the landing
/// page illustration. Drafts get queued locally and (in a follow-up)
/// will sync to the phone via WCSession for transcription + upload.
@main
struct OrchidCaptureWatchApp: App {
    var body: some Scene {
        WindowGroup {
            CaptureRing()
        }
    }
}

private struct CaptureRing: View {
    @State private var pressed = false
    @State private var status: String = "Hold to capture"

    var body: some View {
        ZStack {
            // Outer ring pulses while holding, matching the iPhone mock.
            Circle()
                .stroke(Color.purple.opacity(pressed ? 0.9 : 0.35),
                        lineWidth: pressed ? 6 : 3)
                .scaleEffect(pressed ? 1.05 : 1.0)
                .animation(.easeInOut(duration: 0.25), value: pressed)

            VStack(spacing: 6) {
                Image(systemName: pressed ? "waveform" : "mic.fill")
                    .font(.system(size: 28, weight: .medium))
                    .foregroundStyle(pressed ? .red : .primary)
                Text(status)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .contentShape(Circle())
        .onLongPressGesture(
            minimumDuration: 0.1,
            maximumDistance: .infinity,
            perform: { /* end handled by onLongPressGesture pressing */ },
            onPressingChanged: { isPressing in
                pressed = isPressing
                status = isPressing ? "Recording…" : "Hold to capture"
            }
        )
    }
}
