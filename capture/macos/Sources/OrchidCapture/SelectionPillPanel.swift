import SwiftUI
import AppKit

/// A tiny floating button shown next to a fresh selection. Clicking it
/// expands into the full composer — PopClip-style. Stays non-activating so
/// the user keeps their place in the source app until they choose to
/// capture.
@MainActor
final class SelectionPillPanel {
    private var panel: NSPanel?
    private var dismissTimer: Timer?

    var onPick: (() -> Void)?

    var isVisible: Bool { panel?.isVisible == true }

    func show(at anchor: CGPoint) {
        if panel == nil {
            let p = NonactivatingPanel(
                contentRect: NSRect(x: 0, y: 0, width: 44, height: 28),
                styleMask: [.borderless, .nonactivatingPanel],
                backing: .buffered,
                defer: false
            )
            p.isFloatingPanel = true
            p.level = .floating
            p.hasShadow = true
            p.hidesOnDeactivate = false
            p.backgroundColor = .clear
            p.isOpaque = false
            p.contentView = NSHostingView(rootView: PillButton { [weak self] in
                guard let self else { return }
                self.onPick?()
                self.dismiss()
            })
            panel = p
        }
        guard let panel else { return }
        position(panel, at: anchor)
        panel.orderFrontRegardless()
        rearmTimer()
    }

    func dismiss() {
        panel?.orderOut(nil)
        dismissTimer?.invalidate()
        dismissTimer = nil
    }

    private func rearmTimer() {
        dismissTimer?.invalidate()
        // Pill is opt-in — if the user doesn't reach for it within a few
        // seconds, get out of the way.
        dismissTimer = Timer.scheduledTimer(withTimeInterval: 4.0, repeats: false) { [weak self] _ in
            Task { @MainActor in self?.dismiss() }
        }
    }

    private func position(_ panel: NSPanel, at anchor: CGPoint) {
        let size = panel.frame.size
        let screen = NSScreen.screens.first(where: { $0.frame.contains(anchor) })
            ?? NSScreen.main
        guard let screen else { return }
        let vis = screen.visibleFrame
        var x = anchor.x + 10
        var y = anchor.y - size.height - 10
        if x + size.width > vis.maxX { x = anchor.x - size.width - 10 }
        if y < vis.minY { y = anchor.y + 10 }
        x = max(vis.minX + 4, min(x, vis.maxX - size.width - 4))
        y = max(vis.minY + 4, min(y, vis.maxY - size.height - 4))
        panel.setFrame(
            NSRect(x: x, y: y, width: size.width, height: size.height),
            display: true
        )
    }
}

/// Borderless panels default to `canBecomeKey == false`, which blocks
/// SwiftUI button clicks. Force it on without `.activate`-ing the app so the
/// underlying selection stays put.
private final class NonactivatingPanel: NSPanel {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }
}

private struct PillButton: View {
    var action: () -> Void

    var body: some View {
        Button(action: action) {
            Image(systemName: "plus.circle.fill")
                .font(.system(size: 16, weight: .semibold))
                .foregroundStyle(.white)
                .frame(width: 44, height: 28)
                .background(Color.black.opacity(0.92))
                .clipShape(RoundedRectangle(cornerRadius: 6))
        }
        .buttonStyle(.plain)
    }
}
