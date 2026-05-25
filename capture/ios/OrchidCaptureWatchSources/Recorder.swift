import Foundation
import AVFoundation

/// Lightweight m4a recorder for the watch. Writes to a temp file while
/// holding, returns the bytes + duration on stop. The recorded blob is
/// what gets base64-encoded into a `voice` Draft.
@MainActor
final class Recorder: NSObject, ObservableObject {
    /// 0…1 normalized peak level for the active recording, sampled by
    /// the ring animation. Polls the recorder's average power so the
    /// existing watch UI can pulse with audio amplitude instead of a
    /// fake heartbeat.
    @Published private(set) var level: Float = 0
    @Published private(set) var isRecording = false

    private var recorder: AVAudioRecorder?
    private var levelTimer: Timer?
    private var startedAt: Date?
    private var fileURL: URL?

    enum RecorderError: Error {
        case permissionDenied
        case sessionFailed(Error)
        case recorderFailed(Error)
    }

    /// Begin recording. Caller is responsible for awaiting permission
    /// once at app launch — see CaptureScreen which gates the press
    /// gesture on `Recorder.requestMicPermission()`.
    func start() async throws {
        let session = AVAudioSession.sharedInstance()
        do {
            try session.setCategory(.playAndRecord,
                                    mode: .measurement,
                                    options: [.duckOthers])
            try session.setActive(true, options: [])
        } catch {
            throw RecorderError.sessionFailed(error)
        }

        let url = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("orchid-\(UUID().uuidString).m4a")

        let settings: [String: Any] = [
            AVFormatIDKey:            kAudioFormatMPEG4AAC,
            AVSampleRateKey:          16_000,
            AVNumberOfChannelsKey:    1,
            AVEncoderAudioQualityKey: AVAudioQuality.medium.rawValue,
            AVEncoderBitRateKey:      32_000
        ]

        do {
            let r = try AVAudioRecorder(url: url, settings: settings)
            r.isMeteringEnabled = true
            r.delegate = self
            guard r.record() else {
                throw RecorderError.recorderFailed(
                    NSError(domain: "Recorder", code: -1)
                )
            }
            recorder = r
            fileURL = url
            startedAt = Date()
            isRecording = true
            startLevelTimer()
        } catch let e as RecorderError {
            throw e
        } catch {
            throw RecorderError.recorderFailed(error)
        }
    }

    /// Stop recording. Returns the captured audio bytes, the mime type,
    /// and the duration in seconds — already shaped for `Draft.voice(...)`.
    /// Returns nil if the recording was too short to be useful (<0.4s),
    /// so the watch never sends silent or accidental drafts.
    @discardableResult
    func stop() -> (data: Data, mime: String, duration: Double)? {
        defer { teardown() }
        guard let recorder, let fileURL else { return nil }

        recorder.stop()
        let duration = recorder.currentTime > 0
            ? recorder.currentTime
            : (startedAt.map { -$0.timeIntervalSinceNow } ?? 0)
        if duration < 0.4 { return nil }

        guard let data = try? Data(contentsOf: fileURL),
              !data.isEmpty
        else { return nil }
        return (data, "audio/m4a", duration)
    }

    func cancel() {
        recorder?.stop()
        teardown()
    }

    private func teardown() {
        levelTimer?.invalidate()
        levelTimer = nil
        if let fileURL {
            try? FileManager.default.removeItem(at: fileURL)
        }
        recorder = nil
        fileURL = nil
        startedAt = nil
        isRecording = false
        level = 0
        try? AVAudioSession.sharedInstance().setActive(false,
                                                       options: [.notifyOthersOnDeactivation])
    }

    private func startLevelTimer() {
        levelTimer?.invalidate()
        levelTimer = Timer.scheduledTimer(withTimeInterval: 0.08,
                                          repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self, let r = self.recorder else { return }
                r.updateMeters()
                let db = r.averagePower(forChannel: 0)
                // Map -60…0 dB → 0…1. Anything below -60 is silence.
                let clamped = max(-60, min(0, db))
                self.level = (clamped + 60) / 60
            }
        }
    }

    /// Request mic permission. Returns true if granted. Safe to call
    /// repeatedly — the system caches the answer. Uses the watchOS 10
    /// AVAudioApplication entry point (the older
    /// AVAudioSession.requestRecordPermission is deprecated on watchOS 10).
    static func requestMicPermission() async -> Bool {
        await withCheckedContinuation { cont in
            AVAudioApplication.requestRecordPermission { ok in
                cont.resume(returning: ok)
            }
        }
    }
}

extension Recorder: AVAudioRecorderDelegate {
    func audioRecorderEncodeErrorDidOccur(_ recorder: AVAudioRecorder,
                                          error: Error?) {
        teardown()
    }
}
