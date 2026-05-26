import SwiftUI

@main
struct OrchidCaptureApp: App {
    @StateObject private var drafts = DraftStore()

    var body: some Scene {
        WindowGroup {
            RootTabs()
                .environmentObject(drafts)
                .preferredColorScheme(.light)
                .tint(.black)
        }
    }
}

struct RootTabs: View {
    var body: some View {
        TabView {
            JobsView()
                .tabItem { Label("Jobs", systemImage: "list.bullet.rectangle") }
            ContentView()
                .tabItem { Label("Capture", systemImage: "mic.fill") }
        }
    }
}
