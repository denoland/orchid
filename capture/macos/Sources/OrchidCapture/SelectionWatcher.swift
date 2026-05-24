import Foundation
import AppKit
import ApplicationServices
import Combine

/// Watches the frontmost foreign app for a non-empty selected-text range and
/// fires a callback shortly after the selection becomes stable. The composer
/// uses this to pop a floating note input next to the cursor without the user
/// reaching for the menu bar.
///
/// Strategy: poll the AX API every 300ms. Polling is the simplest reliable
/// path — AXObserver notifications are inconsistent across apps (Chrome,
/// Slack, etc. don't always bubble selection events), but every AX-cooperative
/// app exposes `kAXSelectedTextAttribute` on its focused element.
@MainActor
final class SelectionWatcher: ObservableObject {
    /// Most recent stable, non-empty selection. Cleared when the foreign
    /// app's selection clears.
    @Published var selectedText: String?

    /// Whether the macOS Accessibility permission has been granted. The
    /// composer surfaces this in Settings so users can fix it without
    /// guessing.
    @Published var permissionGranted: Bool = false

    /// Invoked once per new selection with the text and the cursor location
    /// at fire time (AppKit screen coords).
    var onSelection: ((String, CGPoint) -> Void)?

    /// Invoked when the foreign app's selection clears (deselect, focus
    /// switch, etc.). Lets transient UI like the selection pill hide itself
    /// instead of lingering.
    var onCleared: (() -> Void)?

    private var timer: Timer?
    private var mouseMonitor: Any?
    private var lastSeen: String = ""
    private var suppressed: String = ""
    /// Set by the global mouse monitor on mouse-up if the cursor moved
    /// meaningfully since mouse-down. The next tick uses it to decide
    /// whether the user just finished a drag-selection and we should fall
    /// back to a synthetic ⌘C if AX returned nothing.
    private var dragMayHaveEnded: Bool = false
    private var mouseDownAt: CGPoint?

    init() {
        refreshPermission(prompt: true)
        let t = Timer.scheduledTimer(withTimeInterval: 0.3, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.tick() }
        }
        RunLoop.main.add(t, forMode: .common)
        self.timer = t

        // Clicks outside our app are the strongest signal that the source
        // app's selection just changed — short-circuit the poll so the pill
        // doesn't linger up to 300ms after a deselect.
        // Watch both ends of a drag. Mouse-down short-circuits a deselect
        // (so the pill disappears immediately when the user clicks
        // elsewhere). Mouse-up flags that a fresh selection *might* exist —
        // we'll try AX first, then fall back to a synthetic copy.
        mouseMonitor = NSEvent.addGlobalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown, .leftMouseUp]
        ) { [weak self] event in
            let location = NSEvent.mouseLocation
            Task { @MainActor in
                guard let self else { return }
                switch event.type {
                case .leftMouseDown:
                    self.mouseDownAt = location
                case .leftMouseUp:
                    if let down = self.mouseDownAt {
                        let moved = hypot(location.x - down.x, location.y - down.y)
                        // 5px threshold — a click that doesn't move shouldn't
                        // trigger the synthetic-copy fallback (otherwise we
                        // clobber the clipboard on every UI click).
                        self.dragMayHaveEnded = moved > 5
                    }
                    self.mouseDownAt = nil
                default:
                    break
                }
                self.tick()
            }
        }
    }

    deinit {
        timer?.invalidate()
        if let mouseMonitor { NSEvent.removeMonitor(mouseMonitor) }
    }

    func refreshPermission(prompt: Bool) {
        if prompt {
            let key = kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String
            let opts: NSDictionary = [key: true]
            permissionGranted = AXIsProcessTrustedWithOptions(opts)
        } else {
            permissionGranted = AXIsProcessTrusted()
        }
    }

    /// Mark the current selection as dismissed (e.g. user closed the panel
    /// without submitting). We won't fire again until the selection actually
    /// changes.
    func dismissCurrent() {
        suppressed = lastSeen
        selectedText = nil
    }

    private func clearIfNeeded() {
        if lastSeen.isEmpty { return }
        lastSeen = ""
        selectedText = nil
        onCleared?()
    }

    /// Last-resort selection read for multi-element selections that AX
    /// can't describe (Slack threads, mixed web content). Sends ⌘C to the
    /// frontmost app via CGEvent, waits a beat, snapshots the pasteboard,
    /// then restores the previous pasteboard contents. Returns the
    /// captured string if the pasteboard changed.
    private func readSelectionViaSyntheticCopy(targetPID: pid_t) -> String? {
        let pb = NSPasteboard.general
        let preChange = pb.changeCount
        // Snapshot every available representation so we can restore even
        // images/RTF — `string(forType:.string)` alone would lose fidelity.
        let saved: [(NSPasteboard.PasteboardType, Data)] = (pb.types ?? []).compactMap { t in
            guard let d = pb.data(forType: t) else { return nil }
            return (t, d)
        }

        guard let src = CGEventSource(stateID: .combinedSessionState) else { return nil }
        let cKey: CGKeyCode = 8 // virtual keycode for 'c'
        let down = CGEvent(keyboardEventSource: src, virtualKey: cKey, keyDown: true)
        let up   = CGEvent(keyboardEventSource: src, virtualKey: cKey, keyDown: false)
        down?.flags = .maskCommand
        up?.flags = .maskCommand
        down?.postToPid(targetPID)
        up?.postToPid(targetPID)

        // Apps need a beat to service the cmd-c. 80ms is the sweet spot —
        // shorter misses Slack/Chrome, longer feels laggy.
        Thread.sleep(forTimeInterval: 0.08)

        let captured: String? = (pb.changeCount != preChange)
            ? pb.string(forType: .string)
            : nil

        // Restore whatever was there before so we don't clobber the user's
        // clipboard.
        pb.clearContents()
        for (t, d) in saved {
            pb.setData(d, forType: t)
        }
        return captured
    }

    private func tick() {
        if !permissionGranted {
            // Quietly re-check — user may have flipped the toggle in System
            // Settings.
            refreshPermission(prompt: false)
            return
        }
        guard let app = NSWorkspace.shared.frontmostApplication else { return }
        let ownPID = ProcessInfo.processInfo.processIdentifier
        // User switched to our own composer — keep current pill state alone
        // (the composer logic decides whether to hide it).
        guard app.processIdentifier != ownPID else { return }

        let axApp = AXUIElementCreateApplication(app.processIdentifier)
        var focusedRef: CFTypeRef?
        guard AXUIElementCopyAttributeValue(
            axApp,
            kAXFocusedUIElementAttribute as CFString,
            &focusedRef
        ) == .success, let focusedRef else {
            clearIfNeeded()
            return
        }
        let focused = focusedRef as! AXUIElement

        var selRef: CFTypeRef?
        let result = AXUIElementCopyAttributeValue(
            focused,
            kAXSelectedTextAttribute as CFString,
            &selRef
        )
        let axText = (result == .success ? selRef as? String : nil)?
            .trimmingCharacters(in: .whitespacesAndNewlines)

        // AX gives us nothing on multi-element selections (Slack threads,
        // mixed Chrome content, web pages with embedded images). If the
        // user *just* released a drag, try a synthetic ⌘C as a fallback —
        // we save and restore the pasteboard so the user's clipboard
        // doesn't get clobbered.
        let trimmed: String = {
            if let t = axText, !t.isEmpty { return t }
            if dragMayHaveEnded, let copied = readSelectionViaSyntheticCopy(targetPID: app.processIdentifier) {
                return copied.trimmingCharacters(in: .whitespacesAndNewlines)
            }
            return ""
        }()
        dragMayHaveEnded = false

        if trimmed.isEmpty {
            // Clearing the selection resets suppression so the *next*
            // selection — even of the same text — fires.
            suppressed = ""
            clearIfNeeded()
            return
        }
        guard trimmed != lastSeen else { return }
        lastSeen = trimmed
        if trimmed == suppressed { return }
        selectedText = trimmed
        onSelection?(trimmed, NSEvent.mouseLocation)
    }
}
