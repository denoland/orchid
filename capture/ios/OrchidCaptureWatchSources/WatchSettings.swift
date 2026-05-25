import Foundation
import Combine
import WatchConnectivity

/// Where the watch app reads/writes its capture endpoint + token, and
/// how those values get there from the paired iPhone.
///
/// Typing a URL or pasting a token on the Watch is brutal, so the
/// canonical setup flow is:
///
///   1. User configures the iOS app (or scans the dashboard pairing
///      QR / opens the `orchid://` URL on their phone).
///   2. The iOS app calls
///      `WCSession.default.updateApplicationContext([...])` with the
///      endpoint and token (see capture/ios/README.md for the snippet
///      to drop into the main iOS app once it ships).
///   3. The watch receives it here, persists it, and the capture
///      screen starts working.
///
/// Build-time env vars `ORCHID_CAPTURE_ENDPOINT` and `ORCHID_CAPTURE_TOKEN`
/// are honoured as a fallback for simulator / dev rigs that don't have a
/// paired phone — same convention the macOS app uses.
@MainActor
final class WatchSettings: NSObject, ObservableObject {
    @Published private(set) var endpoint: URL?
    @Published private(set) var token: String = ""
    /// Most recent sync from the iPhone, if any. Drives the
    /// "synced from iPhone · 2m ago" line in Settings.
    @Published private(set) var lastSyncFromPhone: Date?

    private let endpointKey = "orchid.endpoint"
    private let tokenKey    = "orchid.token"
    private let syncKey     = "orchid.lastSync"

    /// Application-context keys the iPhone sends. Keep these stable —
    /// they're the over-the-air schema.
    static let phoneEndpointKey = "endpoint"
    static let phoneTokenKey    = "token"

    override init() {
        super.init()
        loadFromDefaults()
        applyEnvFallback()
        activateWatchSession()
    }

    var isConfigured: Bool {
        endpoint != nil && !token.isEmpty
    }

    func setManual(endpoint url: URL?, token: String) {
        if let url {
            UserDefaults.standard.set(url.absoluteString, forKey: endpointKey)
        } else {
            UserDefaults.standard.removeObject(forKey: endpointKey)
        }
        UserDefaults.standard.set(token, forKey: tokenKey)
        self.endpoint = url
        self.token = token
    }

    private func loadFromDefaults() {
        if let s = UserDefaults.standard.string(forKey: endpointKey),
           let url = URL(string: s) {
            endpoint = url
        }
        if let t = UserDefaults.standard.string(forKey: tokenKey) {
            token = t
        }
        if let ts = UserDefaults.standard.object(forKey: syncKey) as? Date {
            lastSyncFromPhone = ts
        }
    }

    private func applyEnvFallback() {
        // Only apply env fallbacks if nothing is already configured,
        // so a previously synced phone value isn't clobbered by stale
        // scheme env vars on relaunch.
        if endpoint == nil,
           let s = ProcessInfo.processInfo.environment["ORCHID_CAPTURE_ENDPOINT"],
           let url = URL(string: s) {
            endpoint = url
        }
        if token.isEmpty,
           let t = ProcessInfo.processInfo.environment["ORCHID_CAPTURE_TOKEN"] {
            token = t
        }
    }

    private func activateWatchSession() {
        guard WCSession.isSupported() else { return }
        let s = WCSession.default
        s.delegate = self
        s.activate()
        // If the phone has already pushed an application context before
        // the watch app launched, pick it up on activation.
        applyPhoneContext(s.receivedApplicationContext)
    }

    fileprivate func applyPhoneContext(_ ctx: [String: Any]) {
        var changed = false
        if let s = ctx[Self.phoneEndpointKey] as? String,
           let url = URL(string: s),
           url != endpoint
        {
            endpoint = url
            UserDefaults.standard.set(s, forKey: endpointKey)
            changed = true
        }
        if let t = ctx[Self.phoneTokenKey] as? String, t != token {
            token = t
            UserDefaults.standard.set(t, forKey: tokenKey)
            changed = true
        }
        if changed {
            let now = Date()
            lastSyncFromPhone = now
            UserDefaults.standard.set(now, forKey: syncKey)
        }
    }
}

// MARK: WCSessionDelegate

extension WatchSettings: WCSessionDelegate {
    nonisolated func session(_ session: WCSession,
                             activationDidCompleteWith state: WCSessionActivationState,
                             error: Error?) {
        // Pull whatever the phone has on its side as soon as activation
        // completes — applicationContext sticks across reboots.
        let ctx = session.receivedApplicationContext
        Task { @MainActor in self.applyPhoneContext(ctx) }
    }

    nonisolated func session(_ session: WCSession,
                             didReceiveApplicationContext applicationContext: [String: Any]) {
        let copy = applicationContext
        Task { @MainActor in self.applyPhoneContext(copy) }
    }

    /// `userInfo` payloads are queued by the OS even if the watch is
    /// asleep — used for one-shot pushes from the phone (e.g. "I just
    /// regenerated my capture token, here's the new one").
    nonisolated func session(_ session: WCSession,
                             didReceiveUserInfo userInfo: [String: Any] = [:]) {
        let copy = userInfo
        Task { @MainActor in self.applyPhoneContext(copy) }
    }
}
