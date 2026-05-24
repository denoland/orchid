import Foundation
import AppKit
import Combine

/// What we infer is currently sitting on the user's clipboard.
enum ClipboardArtifact: Equatable {
    case none
    case image(Data, mime: String)
    case link(URL)
    case text(String)

    var humanLabel: String {
        switch self {
        case .none:       return "clipboard is empty"
        case .image:      return "image on clipboard"
        case .link(let u): return u.host ?? u.absoluteString
        case .text(let t): return t.count > 64 ? String(t.prefix(64)) + "..." : t
        }
    }

    var kind: DraftKind {
        switch self {
        case .image: return .screenshot
        case .link:  return .link
        case .text:  return .text
        case .none:  return .text
        }
    }
}

/// Polls NSPasteboard every 0.5s and republishes the latest artifact. Polling
/// `changeCount` is the standard way to detect clipboard changes on macOS —
/// there is no public push notification.
@MainActor
final class ClipboardWatcher: ObservableObject {
    @Published var artifact: ClipboardArtifact = .none

    private var lastChange: Int = -1
    private var timer: Timer?

    init() {
        // Tick at human speed — half a second is invisible and easy on battery.
        let t = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.tick() }
        }
        RunLoop.main.add(t, forMode: .common)
        self.timer = t
        tick()
    }

    deinit {
        timer?.invalidate()
    }

    private func tick() {
        let pb = NSPasteboard.general
        guard pb.changeCount != lastChange else { return }
        lastChange = pb.changeCount
        artifact = Self.classify(pasteboard: pb)
    }

    /// Force an immediate reclassification — used right after we synthesize
    /// a screencapture so the composer preview updates without waiting for
    /// the next 500ms tick.
    func poke() {
        tick()
    }

    static func classify(pasteboard pb: NSPasteboard) -> ClipboardArtifact {
        // Prefer image (Cmd+Shift+Ctrl+4 style screenshots land here as PNG).
        if let data = pb.data(forType: .png) {
            return .image(data, mime: "image/png")
        }
        if let data = pb.data(forType: .tiff) {
            return .image(data, mime: "image/tiff")
        }

        // Then URL.
        if let url = pb.string(forType: .URL).flatMap(URL.init(string:)) {
            return .link(url)
        }

        // Then text — but only call it a link if it parses as one and starts http.
        if let s = pb.string(forType: .string), !s.isEmpty {
            let trimmed = s.trimmingCharacters(in: .whitespacesAndNewlines)
            if let url = URL(string: trimmed),
               let scheme = url.scheme?.lowercased(),
               scheme == "http" || scheme == "https"
            {
                return .link(url)
            }
            return .text(s)
        }
        return .none
    }
}
