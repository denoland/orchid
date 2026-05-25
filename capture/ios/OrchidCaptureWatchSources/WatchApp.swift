import SwiftUI

/// Orchid Capture for watchOS. The wrist-side surface of the "On the
/// go" feature drawn on the landing page (www landing.html, "On the go"
/// section): hold the dot, speak a thought, lift to ship it as a draft
/// to your orch capture endpoint.
///
/// The hold-to-record affordance lives in `CaptureScreen`. Audio,
/// transcription, queue, and submit are split into their own files so
/// each piece is small enough to reason about on its own. The paired
/// iPhone provides endpoint + token over WatchConnectivity — see
/// `WatchSettings`.
@main
struct OrchidCaptureWatchApp: App {
    @StateObject private var settings: WatchSettings
    @StateObject private var store:    DraftStore

    init() {
        // DraftStore takes a reference to settings, so they have to be
        // wired together up-front rather than lazily inside the scene.
        let s = WatchSettings()
        _settings = StateObject(wrappedValue: s)
        _store    = StateObject(wrappedValue: DraftStore(settings: s))
    }

    var body: some Scene {
        WindowGroup {
            CaptureScreen()
                .environmentObject(settings)
                .environmentObject(store)
        }
    }
}
