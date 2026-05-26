import Foundation
import Speech

/// On-device live transcription tied to the same recording session as
/// `Recorder`. Uses `SFSpeechRecognizer` with `requiresOnDeviceRecognition`
/// set so audio doesn't leave the device. The transcript is appended to in
/// realtime via `@Published transcript`.
@MainActor
final class Transcriber: NSObject, ObservableObject {
    @Published private(set) var transcript: String = ""
    @Published private(set) var authorized = false
    @Published private(set) var lastError: String?

    private let recognizer = SFSpeechRecognizer(locale: .current)
    private var recognitionRequest: SFSpeechAudioBufferRecognitionRequest?
    private var recognitionTask: SFSpeechRecognitionTask?

    /// Returns the buffer request the caller should feed audio into, plus a
    /// short error if transcription can't start (no recognizer, no
    /// permission). The caller is responsible for piping AVAudioEngine
    /// buffers in via `recognitionRequest?.append(_:)`.
    func startSession() async -> SFSpeechAudioBufferRecognitionRequest? {
        transcript = ""
        await requestAuthorization()
        guard authorized else { return nil }
        guard let recognizer, recognizer.isAvailable else {
            lastError = "speech recognizer unavailable for \(Locale.current.identifier)"
            return nil
        }

        let req = SFSpeechAudioBufferRecognitionRequest()
        req.shouldReportPartialResults = true
        if recognizer.supportsOnDeviceRecognition {
            req.requiresOnDeviceRecognition = true
        }

        recognitionRequest = req
        recognitionTask = recognizer.recognitionTask(with: req) { [weak self] result, error in
            Task { @MainActor in
                if let result {
                    self?.transcript = result.bestTranscription.formattedString
                }
                if let error {
                    self?.lastError = error.localizedDescription
                }
            }
        }
        return req
    }

    func endSession() {
        recognitionRequest?.endAudio()
        recognitionRequest = nil
        recognitionTask?.cancel()
        recognitionTask = nil
    }

    private func requestAuthorization() async {
        let status: SFSpeechRecognizerAuthorizationStatus = await withCheckedContinuation { cont in
            SFSpeechRecognizer.requestAuthorization { cont.resume(returning: $0) }
        }
        authorized = (status == .authorized)
        if !authorized {
            lastError = "speech recognition not authorized"
        }
    }
}
