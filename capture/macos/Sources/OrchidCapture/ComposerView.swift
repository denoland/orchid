import SwiftUI
import AppKit

/// Sleek, borderless composer. Two side-by-side chips at the top let the
/// user see *and* toggle which artifact (clipboard / screenshot / foreign
/// selection) will ride along with the note. Single multi-line input below.
/// Bottom rail has the route picker, screenshot toggle, and Capture button.
struct ComposerView: View {
    /// True when hosted in a borderless floating panel (we add our own
    /// rounded translucent container). The menu-bar popover keeps its own
    /// chrome and sets this to false.
    var floatingStyle: Bool = false
    /// Show the inline overflow (⋯) menu. Menu-bar popover wants it.
    var showsMenu: Bool = true

    @EnvironmentObject private var clipboard: ClipboardWatcher
    @EnvironmentObject private var screenshots: ScreenshotWatcher
    @EnvironmentObject private var drafts: DraftStore
    @EnvironmentObject private var selection: SelectionWatcher

    @AppStorage("orchid.route") private var routeRaw: String = CaptureRoute.clawpatrol.rawValue
    @AppStorage("orchid.includeScreenshot") private var includeScreenshot: Bool = false

    @State private var note: String = ""
    @State private var manualSlot: ArtifactSlot? = nil
    @State private var preferDesktopScreenshot = false
    @FocusState private var noteFocused: Bool

    private var route: CaptureRoute {
        CaptureRoute(rawValue: routeRaw) ?? .clawpatrol
    }

    // MARK: - Slot model

    enum ArtifactSlot: Hashable { case selection, screenshot, clipboard }

    private var screenshotArtifact: ClipboardArtifact? {
        if preferDesktopScreenshot, case .image = screenshots.latest { return screenshots.latest }
        if includeScreenshot, case .image = clipboard.artifact { return clipboard.artifact }
        if case .image = clipboard.artifact { return clipboard.artifact }
        return nil
    }

    private var clipboardArtifact: ClipboardArtifact? {
        switch clipboard.artifact {
        case .none, .image: return nil
        case .text, .link: return clipboard.artifact
        }
    }

    private var selectionArtifact: ClipboardArtifact? {
        guard let s = selection.selectedText, !s.isEmpty else { return nil }
        return .text(s)
    }

    private func artifactFor(_ slot: ArtifactSlot) -> ClipboardArtifact? {
        switch slot {
        case .selection:  return selectionArtifact
        case .screenshot: return screenshotArtifact
        case .clipboard:  return clipboardArtifact
        }
    }

    private var resolvedSlot: ArtifactSlot? {
        if let m = manualSlot, artifactFor(m) != nil { return m }
        if selectionArtifact != nil { return .selection }
        if includeScreenshot, screenshotArtifact != nil { return .screenshot }
        if screenshotArtifact != nil { return .screenshot }
        if clipboardArtifact != nil { return .clipboard }
        return nil
    }

    private var activeArtifact: ClipboardArtifact {
        guard let slot = resolvedSlot, let art = artifactFor(slot) else { return .none }
        return art
    }

    // MARK: - Body

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if showsMenu || !floatingStyle { headerRow }
            inputArea
            controlBar
        }
        .padding(floatingStyle ? 16 : 14)
        .background(background)
        .clipShape(RoundedRectangle(cornerRadius: floatingStyle ? 14 : 0, style: .continuous))
        .overlay {
            if floatingStyle {
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .strokeBorder(Color.primary.opacity(0.08), lineWidth: 0.5)
            }
        }
        .onAppear { noteFocused = true }
        .onReceive(NotificationCenter.default.publisher(for: .orchidScreenshotDetected)) { _ in
            preferDesktopScreenshot = true
        }
        .onChange(of: clipboard.artifact) { _, _ in
            preferDesktopScreenshot = false
        }
    }

    @ViewBuilder
    private var background: some View {
        if floatingStyle {
            ZStack { Color.clear.background(.ultraThinMaterial) }
        } else {
            Color(nsColor: .windowBackgroundColor)
        }
    }

    private var headerRow: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(Color.accentColor)
                .frame(width: 8, height: 8)
            Text("orchid")
                .font(.system(.subheadline, design: .monospaced).weight(.semibold))
                .foregroundStyle(.secondary)

            Spacer()

            if showsMenu {
                Menu {
                    Button("Reveal queue in Finder") { drafts.revealQueueInFinder() }
                    Button("Clear queue", role: .destructive) { drafts.clear() }
                    Divider()
                    Button("Settings…") {
                        NSApp.activate(ignoringOtherApps: true)
                        NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
                    }
                    Divider()
                    Button("Quit") { NSApp.terminate(nil) }
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .frame(width: 18)
            }
        }
    }

    private func clipboardIconFor(_ art: ClipboardArtifact) -> String {
        switch art {
        case .link: return "link"
        case .text: return "doc.text"
        default:    return "doc.on.clipboard"
        }
    }

    private func indicatorState(_ slot: ArtifactSlot, active: ArtifactSlot?) -> SlotIndicator.State {
        if active == slot { return .active }
        if artifactFor(slot) != nil { return .available }
        return .empty
    }

    private func clipboardCallToActionForHelp() -> String {
        guard let art = clipboardArtifact else { return "Clipboard empty" }
        return clipboardCallToAction(art)
    }

    private func clipboardCallToAction(_ art: ClipboardArtifact) -> String {
        switch art {
        case .text(let s): return "Select \(s.count) chars from clipboard"
        case .link:        return "Select link from clipboard"
        default:           return "Select from clipboard"
        }
    }

    private func artifactThumbnail(_ art: ClipboardArtifact) -> NSImage? {
        if case .image(let data, _) = art { return NSImage(data: data) }
        return nil
    }

    // MARK: - Input

    private var inputArea: some View {
        TextField(placeholderForCurrentArtifact(), text: $note, axis: .vertical)
            .textFieldStyle(.plain)
            .font(.system(size: 16))
            .lineLimit(6...16)
            .focused($noteFocused)
            .padding(.vertical, 8)
            .padding(.horizontal, 2)
            .frame(minHeight: 140, alignment: .top)
    }

    // MARK: - Control bar

    private var controlBar: some View {
        let active = resolvedSlot
        return HStack(spacing: 6) {
            SlotIndicator(
                icon: "camera",
                state: indicatorState(.screenshot, active: active),
                help: screenshotArtifact == nil ? "Capture screenshot" : "Attach screenshot",
                onTap: {
                    if screenshotArtifact == nil {
                        includeScreenshot = true
                        runFullScreenScreencapture()
                        manualSlot = .screenshot
                    } else {
                        manualSlot = (active == .screenshot) ? nil : .screenshot
                    }
                }
            )
            SlotIndicator(
                icon: "doc.on.clipboard",
                state: indicatorState(.clipboard, active: active),
                help: clipboardCallToActionForHelp(),
                onTap: {
                    guard clipboardArtifact != nil else { return }
                    manualSlot = (active == .clipboard) ? nil : .clipboard
                }
            )
            SlotIndicator(
                icon: "text.quote",
                state: indicatorState(.selection, active: active),
                help: selectionArtifact != nil ? "Attach selection" : "No selection",
                onTap: {
                    guard selectionArtifact != nil else { return }
                    manualSlot = (active == .selection) ? nil : .selection
                }
            )

            Menu {
                ForEach(CaptureRoute.allCases) { r in
                    Button(r.displayName) { routeRaw = r.rawValue }
                }
            } label: {
                HStack(spacing: 4) {
                    Image(systemName: "tag")
                        .font(.system(size: 10))
                    Text(route.displayName)
                        .font(.caption.weight(.medium))
                }
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(Capsule().fill(Color.primary.opacity(0.05)))
                .foregroundStyle(.primary)
            }
            .menuStyle(.borderlessButton)
            .menuIndicator(.hidden)
            .fixedSize()

            Spacer()

            outcomeLine

            Button {
                capture()
            } label: {
                HStack(spacing: 4) {
                    Text("Capture")
                    Image(systemName: "return")
                        .font(.system(size: 10, weight: .semibold))
                        .opacity(0.7)
                }
                .font(.callout.weight(.medium))
                .padding(.horizontal, 10)
                .padding(.vertical, 5)
                .foregroundStyle(.white)
                .background(
                    Capsule().fill(
                        (note.isEmpty && activeArtifact == .none)
                            ? Color.gray
                            : Color.accentColor
                    )
                )
            }
            .buttonStyle(.plain)
            .keyboardShortcut(.return, modifiers: [.command])
            .disabled(note.isEmpty && activeArtifact == .none)
        }
    }

    @ViewBuilder
    private var outcomeLine: some View {
        if let outcome = drafts.lastOutcome {
            switch outcome {
            case .sent(let url):
                HStack(spacing: 3) {
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                    if let url, let u = URL(string: url) {
                        Link(u.lastPathComponent, destination: u)
                    } else {
                        Text("captured")
                    }
                }
                .font(.caption2)
                .foregroundStyle(.secondary)
            case .queuedLocally:
                HStack(spacing: 3) {
                    Image(systemName: "tray.fill")
                    Text(drafts.pending > 0 ? "\(drafts.pending) queued" : "queued")
                }
                .font(.caption2)
                .foregroundStyle(.secondary)
            }
        } else if drafts.pending > 0 {
            Text("\(drafts.pending) queued").font(.caption2).foregroundStyle(.secondary)
        }
    }

    private func placeholderForCurrentArtifact() -> String {
        switch activeArtifact {
        case .image: return "what's the problem with this screenshot?"
        case .link:  return "why did you save this link?"
        case .text:  return "what's the thought here?"
        case .none:  return "describe an idea, bug, or thought…"
        }
    }

    /// Shell out to `screencapture -x -c` for a silent full-screen grab to
    /// the pasteboard. ClipboardWatcher picks up the image on its next tick
    /// (poked here so the preview feels immediate).
    private func runFullScreenScreencapture() {
        Task.detached {
            let task = Process()
            task.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
            task.arguments = ["-x", "-c"]
            do {
                try task.run()
                task.waitUntilExit()
            } catch {
                await MainActor.run { self.includeScreenshot = false }
            }
            await MainActor.run { self.clipboard.poke() }
        }
    }

    private func capture() {
        var draft = Draft.make(
            kind: activeArtifact.kind,
            note: note,
            target: route.draftTarget
        )

        let shouldAttachImage: Bool = {
            if case .image = activeArtifact, includeScreenshot { return true }
            return false
        }()

        switch activeArtifact {
        case .image(let data, let mime):
            if shouldAttachImage {
                draft.image = DraftImage(mime: mime, bytesBase64: data.base64EncodedString())
            } else {
                draft.kind = .text
                draft.text = DraftText(body: note, originURL: nil)
            }
        case .link(let url):
            draft.link = DraftLink(url: url.absoluteString, title: nil)
        case .text(let body):
            draft.text = DraftText(body: body, originURL: nil)
        case .none:
            draft.kind = .text
            draft.text = DraftText(body: note, originURL: nil)
        }
        draft.context = AppContextSnapshot.current()
        let toSubmit = draft
        Task { await drafts.submit(toSubmit) }
        note = ""
        manualSlot = nil
        preferDesktopScreenshot = false
        selection.dismissCurrent()
    }
}

// MARK: - Slot indicator

struct SlotIndicator: View {
    enum State { case empty, available, active }

    let icon: String
    let state: State
    let help: String
    let onTap: () -> Void

    var body: some View {
        Button(action: onTap) {
            Image(systemName: icon)
                .font(.system(size: 11, weight: .semibold))
                .frame(width: 24, height: 22)
                .foregroundStyle(foreground)
                .background(Capsule().fill(background))
                .overlay(Capsule().strokeBorder(border, lineWidth: 0.6))
        }
        .buttonStyle(.plain)
        .help(help)
        .opacity(state == .empty ? 0.5 : 1)
    }

    private var foreground: Color {
        switch state {
        case .active:    return .accentColor
        case .available: return .primary
        case .empty:     return .secondary
        }
    }
    private var background: Color {
        switch state {
        case .active:    return Color.accentColor.opacity(0.16)
        case .available: return Color.primary.opacity(0.06)
        case .empty:     return Color.primary.opacity(0.03)
        }
    }
    private var border: Color {
        state == .active ? Color.accentColor.opacity(0.4) : .clear
    }
}
