import SwiftUI
import AppKit

/// A small borderless panel that hosts the ComposerView. Summoned by the
/// global hotkey. Closes on Escape or after a successful submit.
@MainActor
final class FloatingComposerPanel {
    private var panel: NSPanel?
    private var escMonitor: Any?

    func toggle(host: some View) {
        if let panel, panel.isVisible {
            close()
        } else {
            show(host: host)
        }
    }

    func show(host: some View, at anchor: CGPoint? = nil) {
        if panel == nil {
            let p = KeyableBorderlessPanel(
                contentRect: NSRect(x: 0, y: 0, width: 460, height: 240),
                styleMask: [.borderless, .nonactivatingPanel, .fullSizeContentView],
                backing: .buffered, defer: false
            )
            p.isFloatingPanel = true
            p.level = .floating
            p.backgroundColor = .clear
            p.isOpaque = false
            p.hasShadow = true
            p.isMovableByWindowBackground = true
            p.hidesOnDeactivate = false
            p.contentView = NSHostingView(rootView: host)
            installEscapeMonitor(panel: p)
            panel = p
        } else {
            panel?.contentView = NSHostingView(rootView: host)
        }
        guard let panel else { return }
        if let anchor {
            positionNear(panel, anchor: anchor)
        } else {
            center(panel)
        }
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
    }

    var isVisible: Bool { panel?.isVisible == true }

    func close() {
        panel?.orderOut(nil)
    }

    private func center(_ panel: NSPanel) {
        guard let screen = NSScreen.main else { return }
        let size = panel.frame.size
        let frame = NSRect(
            x: screen.visibleFrame.midX - size.width / 2,
            y: screen.visibleFrame.midY - size.height / 2 + 80,
            width: size.width, height: size.height
        )
        panel.setFrame(frame, display: true)
    }

    /// Place the panel close to the cursor — by default below-right of the
    /// anchor, flipping to above or left when the screen edge would clip it.
    private func installEscapeMonitor(panel: NSPanel) {
        if escMonitor != nil { return }
        escMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self, weak panel] event in
            // 53 = ESC. Close the floating composer without intercepting
            // other keystrokes.
            if event.keyCode == 53, panel?.isKeyWindow == true {
                self?.close()
                return nil
            }
            return event
        }
    }

    private func positionNear(_ panel: NSPanel, anchor: CGPoint) {
        let size = panel.frame.size
        let screen = NSScreen.screens.first(where: { $0.frame.contains(anchor) })
            ?? NSScreen.main
        guard let screen else { return }
        let vis = screen.visibleFrame

        var x = anchor.x + 14
        // AppKit Y grows upward; "below the cursor" = smaller Y.
        var y = anchor.y - size.height - 14
        if x + size.width > vis.maxX {
            x = anchor.x - size.width - 14
        }
        if y < vis.minY {
            y = anchor.y + 14
        }
        x = max(vis.minX + 6, min(x, vis.maxX - size.width - 6))
        y = max(vis.minY + 6, min(y, vis.maxY - size.height - 6))
        panel.setFrame(
            NSRect(x: x, y: y, width: size.width, height: size.height),
            display: true
        )
    }
}

/// Borderless panels default to `canBecomeKey == false`, which blocks text
/// input. Allow it without making the panel "main" so the underlying app
/// stays the focused app conceptually.
private final class KeyableBorderlessPanel: NSPanel {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }
}
