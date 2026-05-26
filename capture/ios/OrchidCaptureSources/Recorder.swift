import Foundation
import AVFoundation
import Speech

/// Records to an `.m4a` file via `AVAudioRecorder` and (optionally) feeds the
/// same input to an `AVAudioEngine` tap so a `Transcriber` can run live
/// on-device speech recognition in parallel.
@MainActor
final class Recorder: NSObject, ObservableObject {
    @Published private(set) var isRecording = false
    @Published private(set) var level: Double = 0
    @Published private(set) var duration: TimeInterval = 0
    @Published private(set) var lastError: String?

    private var recorder: AVAudioRecorder?
    private var meterTimer: Timer?
    private var startedAt: Date?
    private(set) var lastFileURL: URL?

    // Engine + transcription
    private let engine = AVAudioEngine()
    private weak var transcriber: Transcriber?
    private var transcriptionRequest: SFSpeechAudioBufferRecognitionRequest?

    func toggle(transcriber: Transcriber? = nil) async {
        if isRecording {
            await stop()
        } else {
            await start(transcriber: transcriber)
        }
    }

    func start(transcriber: Transcriber? = nil) async {
        do {
            try await ensurePermission()

            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.record, mode: .measurement, options: [])
            try session.setActive(true, options: .notifyOthersOnDeactivation)

            let url = try makeFileURL()
            lastFileURL = url

            let settings: [String: Any] = [
                AVFormatIDKey: kAudioFormatMPEG4AAC,
                AVSampleRateKey: 44_100.0,
                AVNumberOfChannelsKey: 1,
                AVEncoderAudioQualityKey: AVAudioQuality.medium.rawValue
            ]
            let r = try AVAudioRecorder(url: url, settings: settings)
            r.isMeteringEnabled = true
            r.delegate = self
            guard r.record() else {
                throw NSError(domain: "OrchidCapture", code: 1,
                              userInfo: [NSLocalizedDescriptionKey: "record() returned false"])
            }
            recorder = r
            startedAt = Date()
            isRecording = true
            startMeterTimer()

            // Wire up live transcription if a transcriber was provided. We
            // tap the input node and pipe buffers into the recognition
            // request; AVAudioRecorder writes the m4a in parallel.
            if let transcriber, let req = await transcriber.startSession() {
                self.transcriber = transcriber
                self.transcriptionRequest = req
                let input = engine.inputNode
                let format = input.outputFormat(forBus: 0)
                input.removeTap(onBus: 0)
                input.installTap(onBus: 0, bufferSize: 1024, format: format) { buffer, _ in
                    req.append(buffer)
                }
                engine.prepare()
                try engine.start()
            }
        } catch {
            lastError = error.localizedDescription
            isRecording = false
        }
    }

    func stop() async {
        meterTimer?.invalidate()
        meterTimer = nil
        recorder?.stop()
        recorder = nil
        isRecording = false

        engine.inputNode.removeTap(onBus: 0)
        if engine.isRunning { engine.stop() }
        transcriber?.endSession()

        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }

    func discard() {
        if let url = lastFileURL { try? FileManager.default.removeItem(at: url) }
        lastFileURL = nil
        duration = 0
        level = 0
    }

    /// Consume the file: returns its bytes and removes it from disk.
    func takeAudio() -> (data: Data, duration: TimeInterval)? {
        guard let url = lastFileURL else { return nil }
        defer {
            try? FileManager.default.removeItem(at: url)
            lastFileURL = nil
        }
        let data = (try? Data(contentsOf: url)) ?? Data()
        return (data, duration)
    }

    private func ensurePermission() async throws {
        let session = AVAudioSession.sharedInstance()
        if session.recordPermission == .granted { return }
        let granted: Bool = await withCheckedContinuation { cont in
            session.requestRecordPermission { cont.resume(returning: $0) }
        }
        guard granted else {
            throw NSError(domain: "OrchidCapture", code: 2,
                          userInfo: [NSLocalizedDescriptionKey: "microphone permission denied"])
        }
    }

    private func makeFileURL() throws -> URL {
        let fm = FileManager.default
        let dir = try fm.url(for: .cachesDirectory, in: .userDomainMask,
                             appropriateFor: nil, create: true)
        return dir.appendingPathComponent("capture-\(ulidLike()).m4a")
    }

    private func startMeterTimer() {
        let t = Timer.scheduledTimer(withTimeInterval: 0.05, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.tick() }
        }
        RunLoop.main.add(t, forMode: .common)
        meterTimer = t
    }

    private func tick() {
        guard let r = recorder, let startedAt else { return }
        r.updateMeters()
        let db = Double(r.averagePower(forChannel: 0))
        let normalized = max(0, min(1, 1 - pow(10, db / 20)))
        level = normalized
        duration = Date().timeIntervalSince(startedAt)
    }
}

extension Recorder: AVAudioRecorderDelegate {
    nonisolated func audioRecorderDidFinishRecording(
        _ recorder: AVAudioRecorder,
        successfully flag: Bool
    ) {
        Task { @MainActor in
            self.isRecording = false
        }
    }
}
