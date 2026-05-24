import SwiftUI
import KeyboardShortcuts

extension KeyboardShortcuts.Name {
    /// The summon hotkey. Default: ⌃⌥⌘O (Control-Option-Command-O). The user
    /// can rebind it from the Settings scene; KeyboardShortcuts persists the
    /// chosen binding under `KeyboardShortcuts_<name>` in UserDefaults.
    static let summonComposer = Self("summonComposer", default: .init(.o, modifiers: [.control, .option, .command]))
}

/// Wires the global hotkey to the floating composer panel. Held in
/// `@StateObject` from the App so it's alive for the process lifetime.
@MainActor
final class HotkeyController: ObservableObject {
    var onTrigger: (() -> Void)?

    init() {
        KeyboardShortcuts.onKeyUp(for: .summonComposer) { [weak self] in
            self?.onTrigger?()
        }
    }
}

/// Settings tab where the user picks the global hotkey.
struct HotkeySettingsView: View {
    var body: some View {
        Form {
            KeyboardShortcuts.Recorder("Summon composer", name: .summonComposer)
            Text("Default: ⌃⌥⌘O")
                .font(.caption).foregroundStyle(.secondary)
        }
        .padding(20)
        .frame(width: 360, height: 120)
    }
}
