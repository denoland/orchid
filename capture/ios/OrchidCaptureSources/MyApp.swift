import SwiftUI

@main
struct OrchidCaptureApp: App {
    @StateObject private var drafts = DraftStore()
    @StateObject private var store = StateStore()

    var body: some Scene {
        WindowGroup {
            RootTabs()
                .environmentObject(drafts)
                .environmentObject(store)
                .tint(Theme.orchid)
        }
    }
}

struct RootTabs: View {
    @EnvironmentObject private var store: StateStore

    var body: some View {
        TabView {
            JobsView()
                .tabItem { Label("Sessions", systemImage: "list.bullet.rectangle") }
            MachinesView()
                .tabItem { Label("Machines", systemImage: "server.rack") }
            ContentView()
                .tabItem { Label("Capture", systemImage: "mic.fill") }
            SettingsTab()
                .tabItem { Label("Settings", systemImage: "gearshape") }
        }
        .onAppear { store.start() }
    }
}

/// Settings as a top-level tab (SettingsView is also usable as a sheet).
/// Reconnects the live store when the endpoint/token change.
struct SettingsTab: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.http_secret") private var secret: String = ""

    var body: some View {
        SettingsView()
            .onChange(of: endpoint) { _ in store.reconnect() }
            .onChange(of: secret) { _ in store.reconnect() }
    }
}
