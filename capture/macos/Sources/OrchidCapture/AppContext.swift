import Foundation
import AppKit
import ApplicationServices

/// Snapshots the frontmost app and (if Accessibility is granted) its window
/// title. The window title path uses the AX API, which requires the user to
/// approve OrchidCapture under
///   System Settings > Privacy & Security > Accessibility.
/// If that approval hasn't happened yet, the window title is omitted and the
/// app name is still captured (NSWorkspace is unprivileged).
enum AppContextSnapshot {
    static func current() -> DraftContext {
        var snapshot = DraftContext()

        if let front = NSWorkspace.shared.frontmostApplication {
            snapshot.appName = front.localizedName
            if AXIsProcessTrusted() {
                snapshot.windowTitle = focusedWindowTitle(pid: front.processIdentifier)
            }
        }
        return snapshot
    }

    private static func focusedWindowTitle(pid: pid_t) -> String? {
        let app = AXUIElementCreateApplication(pid)
        var rawWindow: CFTypeRef?
        let err = AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &rawWindow)
        guard err == .success, let window = rawWindow else { return nil }
        // window is AXUIElement-typed at the AX layer.
        var rawTitle: CFTypeRef?
        let titleErr = AXUIElementCopyAttributeValue(
            window as! AXUIElement,
            kAXTitleAttribute as CFString,
            &rawTitle
        )
        if titleErr == .success, let s = rawTitle as? String, !s.isEmpty {
            return s
        }
        return nil
    }
}

