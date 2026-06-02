import SwiftUI

@main
struct OrchidCaptureApp: App {
    @StateObject private var drafts = DraftStore()
    @StateObject private var store = StateStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(drafts)
                .environmentObject(store)
                .tint(Theme.orchid)
        }
    }
}

enum Tab: String, CaseIterable { case sessions, machines, settings
    var label: String {
        switch self {
        case .sessions: return "Sessions"
        case .machines: return "Machines"
        case .settings: return "Settings"
        }
    }
}

/// Root shell — mirrors the web Dashboard: a sticky TopBar (logo · search ·
/// Capture) over a GitHub-style tab strip, with the selected tab's content
/// below. Sessions rows push the live pane.
struct RootView: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @State private var tab: Tab = .sessions
    @State private var q = ""
    @State private var showCapture = false

    private var liveCount: Int { (store.state.jobs ?? []).filter { !$0.isClosed }.count }
    private var vmCount: Int { (store.state.vms ?? []).count }

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                TopBar(tab: $tab, q: $q, sessionCount: liveCount, vmCount: vmCount,
                       onCapture: { showCapture = true })
                content
            }
            .background(Theme.surface)
            .navigationBarHidden(true)
        }
        .sheet(isPresented: $showCapture) { CaptureSheet() }
        .onAppear { store.start() }
    }

    @ViewBuilder private var content: some View {
        switch tab {
        case .sessions:  SessionsList(q: q)
        case .machines:  MachinesContent()
        case .settings:  SettingsForm()
        }
    }
}

// ─── TopBar (two rows: brand+search, then tabs) ────────────────────────────

struct TopBar: View {
    @Binding var tab: Tab
    @Binding var q: String
    let sessionCount: Int
    let vmCount: Int
    let onCapture: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Image("orchid-logo").resizable().scaledToFit().frame(width: 24, height: 24)
                Text("Orchid").font(.system(size: 18, weight: .semibold)).foregroundStyle(Theme.ink)
                Spacer(minLength: 12)
                searchField
                Button(action: onCapture) {
                    Image(systemName: "plus").font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(Theme.ink)
                        .frame(width: 30, height: 30)
                        .background(Theme.searchBg, in: RoundedRectangle(cornerRadius: 8))
                }
            }
            .padding(.horizontal, 16).frame(height: 48)

            tabStrip
        }
        .background(Theme.navBg)
        .overlay(alignment: .bottom) { Rectangle().fill(Theme.line).frame(height: 1) }
    }

    private var searchField: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass").font(.system(size: 12)).foregroundStyle(Theme.faint)
            TextField("Search", text: $q)
                .font(.system(size: 13))
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            if !q.isEmpty {
                Button { q = "" } label: {
                    Image(systemName: "xmark.circle.fill").font(.system(size: 13)).foregroundStyle(Theme.faint)
                }
            }
        }
        .padding(.horizontal, 9).frame(height: 32).frame(maxWidth: 220)
        .background(Theme.searchBg, in: RoundedRectangle(cornerRadius: 8))
    }

    private var tabStrip: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 2) {
                ForEach(Tab.allCases, id: \.self) { t in
                    let on = t == tab
                    Button { tab = t } label: {
                        VStack(spacing: 0) {
                            HStack(spacing: 6) {
                                Text(t.label)
                                    .font(.system(size: 14, weight: on ? .semibold : .regular))
                                    .foregroundStyle(on ? Theme.ink : Theme.muted)
                                if let c = count(t) {
                                    Text("\(c)").font(Theme.mono(11))
                                        .foregroundStyle(Theme.muted)
                                        .padding(.horizontal, 6).padding(.vertical, 1)
                                        .background(Theme.chipBg, in: Capsule())
                                }
                            }
                            .padding(.horizontal, 12).frame(height: 42)
                            Rectangle().fill(on ? Theme.ink : Color.clear)
                                .frame(height: 2).padding(.horizontal, 6)
                        }
                    }
                }
            }
            .padding(.horizontal, 8)
        }
        .frame(height: 44)
    }

    private func count(_ t: Tab) -> Int? {
        switch t {
        case .sessions: return sessionCount
        case .machines: return vmCount
        default: return nil
        }
    }
}

/// Capture as a presented sheet (the dashboard's header Capture button).
struct CaptureSheet: View {
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        NavigationStack {
            ContentView()
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button { dismiss() } label: { Image(systemName: "xmark") }
                            .tint(Theme.muted)
                    }
                }
        }
    }
}
