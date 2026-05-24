import SwiftUI
import AppKit

/// Shared state container — instantiated once at app launch so the
/// selection watcher / hotkey / floating panel are wired up *before* the
/// user clicks the menu-bar icon. Previously this happened in the menu-bar
/// extra's `.onAppear`, which only fires when the user opens the popover —
/// the pill would never appear until they did.
@MainActor
final class AppEnvironment: ObservableObject {
    let clipboard = ClipboardWatcher()
    let screenshots = ScreenshotWatcher()
    let drafts = DraftStore()
    let hotkey = HotkeyController()
    let selection = SelectionWatcher()
    let floatingPanel = FloatingComposerPanel()
    let selectionPill = SelectionPillPanel()

    init() {
        wireTriggers()
    }

    @ViewBuilder
    func composerHost(floating: Bool, showsMenu: Bool) -> some View {
        ComposerView(floatingStyle: floating, showsMenu: showsMenu)
            .environmentObject(clipboard)
            .environmentObject(screenshots)
            .environmentObject(drafts)
            .environmentObject(selection)
            .frame(width: floating ? 460 : 360, alignment: .top)
            .fixedSize(horizontal: false, vertical: true)
    }

    private func wireTriggers() {
        hotkey.onTrigger = { [weak self] in
            guard let self else { return }
            self.selectionPill.dismiss()
            self.floatingPanel.toggle(host: self.composerHost(floating: true, showsMenu: false))
        }
        selection.onSelection = { [weak self] _, anchor in
            guard let self else { return }
            if self.floatingPanel.isVisible { return }
            self.selectionPill.onPick = { [weak self] in
                guard let self else { return }
                self.floatingPanel.show(
                    host: self.composerHost(floating: true, showsMenu: false),
                    at: anchor
                )
            }
            self.selectionPill.show(at: anchor)
        }
        selection.onCleared = { [weak self] in
            self?.selectionPill.dismiss()
        }
    }
}

@main
struct OrchidCaptureApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var env = AppEnvironment()

    var body: some Scene {
        MenuBarExtra {
            env.composerHost(floating: false, showsMenu: true)
        } label: {
            Image(systemName: env.drafts.pending > 0 ? "circle.fill" : "circle")
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
                .environmentObject(env.drafts)
                .environmentObject(env.selection)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }

    // Without this, closing the floating composer / settings drops the last
    // window and AppKit terminates the menu-bar app.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }
}

private struct SettingsView: View {
    @EnvironmentObject private var drafts: DraftStore

    var body: some View {
        TabView {
            HotkeySettingsView()
                .tabItem { Label("Hotkey", systemImage: "command") }

            EndpointSettingsView()
                .environmentObject(drafts)
                .tabItem { Label("Endpoint", systemImage: "network") }
        }
        .frame(minWidth: 420, minHeight: 220)
    }
}

private struct EndpointSettingsView: View {
    @EnvironmentObject private var drafts: DraftStore
    @AppStorage("orchid.endpoint") private var endpointString: String = ""
    @AppStorage("orchid.token") private var token: String = ""

    var body: some View {
        Form {
            TextField("Endpoint URL",
                      text: $endpointString,
                      prompt: Text("http://127.0.0.1:8787/api/drafts"))
                .textFieldStyle(.roundedBorder)
            SecureField("X-Capture-Token", text: $token)
                .textFieldStyle(.roundedBorder)
            Text("Drafts also stay queued on disk at\n~/Library/Application Support/OrchidCapture/queue.jsonl")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(20)
        .onChange(of: endpointString) { _, new in drafts.updateEndpoint(URL(string: new)) }
        .onChange(of: token) { _, new in drafts.updateToken(new) }
        .onAppear {
            drafts.updateEndpoint(URL(string: endpointString))
            drafts.updateToken(token)
        }
    }
}
