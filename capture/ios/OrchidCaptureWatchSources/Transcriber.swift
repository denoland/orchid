import Foundation
import Speech
import AVFoundation

/// Live on-device transcription. The watch app uses this purely as a
/// preview — the m4a is what gets uploaded as the voice draft, and the
/// orch backend is the source of truth for any future cloud transcription.
///
/// On-device recognition is requested unconditionally. Locale availability
/// varies; when on-device isn't supported we silently fall back to whatever
/// the system will do (typically nothing on watchOS), and `partialText`
/// stays empty. The audio recording always succeeds either way.
@MainActor
final class Transcriber: ObservableObject {
    @Published private(set) var partialText: String = ""
    @Published private(set) var isListening = false

    private let recognizer: SFSpeechRecognizer?
    private let engine = AVAudioEngine()
    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?

    init(locale: Locale = .current) {
        self.recognizer = SFSpeechRecognizer(locale: locale)
            ?? SFSpeechRecognizer(locale: Locale(identifier: "en-US"))
    }

    static func requestAuthorization() async -> SFSpeechRecognizerAuthorizationStatus {
        await withCheckedContinuation { cont in
            SFSpeechRecognizer.requestAuthorization { status in
                cont.resume(returning: status)
            }
        }
    }

    func start() {
        guard let recognizer, recognizer.isAvailable else { return }
        stop()
        partialText = ""

        let req = SFSpeechAudioBufferRecognitionRequest()
        req.shouldReportPartialResults = true
        if recognizer.supportsOnDeviceRecognition {
            req.requiresOnDeviceRecognition = true
        }
        self.request = req

        let input = engine.inputNode
        let format = input.outputFormat(forBus: 0)
        // Some watch input nodes report 0-channel formats during cold
        // start; bailing keeps us from crashing the audio engine.
        guard format.channelCount > 0 else { return }

        input.removeTap(onBus: 0)
        input.installTap(onBus: 0, bufferSize: 1024, format: format) { [weak self] buf, _ in
            self?.request?.append(buf)
        }

        do {
            engine.prepare()
            try engine.start()
            isListening = true
        } catch {
            // Engine refused to start — leave partialText empty; the
            // m4a recorder is the authoritative capture path.
            return
        }

        task = recognizer.recognitionTask(with: req) { [weak self] result, error in
            guard let self else { return }
            if let result {
                Task { @MainActor in
                    self.partialText = result.bestTranscription.formattedString
                }
            }
            if error != nil || (result?.isFinal ?? false) {
                Task { @MainActor in self.stop() }
            }
        }
    }

    func stop() {
        if engine.isRunning {
            engine.stop()
            engine.inputNode.removeTap(onBus: 0)
        }
        request?.endAudio()
        task?.cancel()
        request = nil
        task = nil
        isListening = false
    }
}
