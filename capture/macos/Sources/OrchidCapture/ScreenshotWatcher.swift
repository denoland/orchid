import Foundation
import AppKit

/// Watches the user's screenshot save location for new image files and
/// republishes the latest one as a `ClipboardArtifact.image` so the composer
/// can pick it up exactly like a clipboard screenshot.
///
/// Reads the screenshot location from `com.apple.screencapture/location` —
/// the same defaults key `screencapture -P show` exposes — falling back to
/// `~/Desktop` when the key isn't set.
@MainActor
final class ScreenshotWatcher: ObservableObject {
    @Published var latest: ClipboardArtifact = .none
    @Published var latestPath: URL?

    private var query: NSMetadataQuery?
    private var seenIDs = Set<String>()
    private var startupAt = Date()

    init() {
        start()
    }

    deinit {
        // Property access from deinit is sync OK because NSMetadataQuery's
        // stop() is thread-safe and we hold the only reference.
        query?.stop()
    }

    func start() {
        let dir = Self.screenshotDirectory()
        let q = NSMetadataQuery()
        q.searchScopes = [dir]
        // kMDItemIsScreenCapture is set on screenshots taken via ⌘⇧3/4/5.
        q.predicate = NSPredicate(format: "kMDItemIsScreenCapture = 1")
        q.notificationBatchingInterval = 0.3

        NotificationCenter.default.addObserver(
            self, selector: #selector(handleResults(_:)),
            name: .NSMetadataQueryDidFinishGathering, object: q)
        NotificationCenter.default.addObserver(
            self, selector: #selector(handleResults(_:)),
            name: .NSMetadataQueryDidUpdate, object: q)

        q.start()
        self.query = q
    }

    @objc private func handleResults(_ note: Notification) {
        guard let q = query else { return }
        q.disableUpdates()
        defer { q.enableUpdates() }

        // Newest first. Spotlight gives kMDItemContentCreationDate; we sort
        // by that and take the most recent one we haven't seen this session.
        let items: [NSMetadataItem] = (0..<q.resultCount).compactMap { idx in
            q.result(at: idx) as? NSMetadataItem
        }
        let sorted = items
            .compactMap { item -> (NSMetadataItem, Date, String)? in
                guard
                    let date = item.value(forAttribute: NSMetadataItemContentCreationDateKey) as? Date,
                    let path = item.value(forAttribute: NSMetadataItemPathKey) as? String
                else { return nil }
                return (item, date, path)
            }
            .sorted { $0.1 > $1.1 }
        guard let pick = sorted.first else { return }
        // Don't re-surface screenshots that pre-dated app launch — gathering
        // returns the entire historical match set on first fire.
        if pick.1 < startupAt { return }
        if seenIDs.contains(pick.2) { return }
        seenIDs.insert(pick.2)

        let url = URL(fileURLWithPath: pick.2)
        guard let data = try? Data(contentsOf: url) else { return }
        latestPath = url
        latest = .image(data, mime: "image/png")

        // Also surface it via NSWorkspace so the composer can show "from
        // <filename>" in the preview.
        NotificationCenter.default.post(name: .orchidScreenshotDetected, object: url)
    }

    static func screenshotDirectory() -> URL {
        // `defaults read com.apple.screencapture location` — same fetch path
        // the system uses, so a custom save location is respected without
        // any extra config.
        if let loc = UserDefaults(suiteName: "com.apple.screencapture")?
            .string(forKey: "location"), !loc.isEmpty
        {
            let expanded = (loc as NSString).expandingTildeInPath
            return URL(fileURLWithPath: expanded, isDirectory: true)
        }
        let fm = FileManager.default
        return (try? fm.url(for: .desktopDirectory, in: .userDomainMask,
                            appropriateFor: nil, create: false))
            ?? URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Desktop")
    }
}

extension Notification.Name {
    static let orchidScreenshotDetected = Notification.Name("OrchidCapture.ScreenshotDetected")
}
