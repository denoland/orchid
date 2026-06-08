# Orchid Capture — macOS menu bar app

A SwiftUI Swift Package that puts a small circle in the menu bar. Click it →
black-and-white composer pops down. The composer notices what's on your
clipboard (screenshot, link, text) and lets you describe it in one sentence,
then queues a draft and POSTs it to a configured Orchid endpoint.

## What it does end-to-end

- **Clipboard** — polls `NSPasteboard.changeCount` and shows you whatever
  is on the clipboard (PNG, URL, text).
- **Saved screenshots** — `NSMetadataQuery` watches your screenshot save
  location (default `~/Desktop`) and surfaces new ones to the composer.
- **Global hotkey** — ⌃⌥⌘O (rebindable in Settings) summons a floating
  panel anywhere on macOS.
- **App context** — captures the frontmost app's name; if you grant the app
  Accessibility access in System Settings, also the focused window's title.
- **Submit** — POSTs the draft to `orch /api/drafts` and shows the resulting
  GitHub issue URL right in the composer.
- **Queue** — every draft is also written to a local JSONL queue so the
  capture never silently vanishes if the endpoint is down.

## Run it

```sh
cd capture/macos
swift run
```

On first launch `swift run` resolves the `KeyboardShortcuts` dependency from
the Swift Package Index, then builds and launches. The dock will not show an
icon — the app sets `NSApp.setActivationPolicy(.accessory)` at startup.

Quit: click the menu bar circle, open the `…` menu, choose **Quit**.

For a richer dev loop, open `Package.swift` in Xcode 15+ and ⌘R. Xcode
bundles it as a proper `.app` and you get the SwiftUI canvas.

### Requirements

- macOS 13 Ventura or newer (uses `MenuBarExtra`).
- Xcode 15 or Swift 5.9 toolchain.

### Wire it up to a running orch

Open the popover → `…` menu → **Settings...** → **Endpoint** tab. Paste:

- **Endpoint URL** — wherever your orch is reachable, e.g.
  `http://127.0.0.1:8000/api/drafts` (local) or
  `http://<host>:8000/api/drafts` (LAN / Tailscale).
- **X-Capture-Token** — the same token you set in the orch `capture.auth_token`
  config.

Both values are persisted in `UserDefaults` and read on the next submit. You
can also bootstrap from the environment:

```sh
ORCHID_CAPTURE_ENDPOINT=http://127.0.0.1:8787/api/drafts \
ORCHID_CAPTURE_TOKEN=local-dev-token \
swift run
```

## Manual test plan

End-to-end run guide: see `../END_TO_END.md`. The bullets below are smoke
tests for the app in isolation.

1. `swift run` — confirm a circle appears in the menu bar and no dock icon.
2. Take a screenshot to clipboard with `⌃⇧⌘4` (drag selection). Click the
   menu bar icon — the composer should show "screenshot" with a thumbnail.
3. Take a screenshot to file with `⌘⇧3` — the composer should switch to
   "saved screenshot" with the file name.
4. Hit ⌃⌥⌘O from anywhere — a floating panel should appear centered on
   screen with the same composer.
5. Type "the inbox table jitters on update", press ⌘↩. The composer should
   clear; below the field you should see either "captured → <issue link>"
   (if an endpoint is configured and reachable) or "queued — ..." with a
   reason.
6. Open Settings, rebind the hotkey to ⌃⇧Space, close Settings, try the new
   binding.
7. From the popover's `…` menu, click **Reveal queue in Finder** — Finder
   should open with the JSONL queue file selected.

## What's there

| File | Role |
|---|---|
| `OrchidCaptureApp.swift`      | App entry; declares the menu bar + Settings scenes. |
| `ComposerView.swift`          | The popover UI — preview + note field + Capture. |
| `ClipboardWatcher.swift`      | Polls `NSPasteboard` and classifies the artifact. |
| `ScreenshotWatcher.swift`     | Watches the screenshot save location for new files. |
| `AppContext.swift`            | Frontmost app name + AX window title. |
| `Hotkey.swift`                | `KeyboardShortcuts` wiring + Settings recorder. |
| `FloatingComposerPanel.swift` | The hotkey-summoned `NSPanel`. |
| `DraftStore.swift`            | JSONL queue + HTTP submit + outcome state. |
| `Draft.swift`                 | The shared `Draft` Codable shape. |
| `Info.plist`                  | `LSUIElement=true`, bundle metadata. |
| `Entitlements.plist`          | Sandbox off (for AX), network client. |
| `Makefile`                    | Build / sign / notarize / staple. |

The queue lives at:

```
~/Library/Application Support/OrchidCapture/queue.jsonl
```

Use **Reveal queue in Finder** from the popover's `…` menu, or:

```sh
tail -f ~/Library/Application\ Support/OrchidCapture/queue.jsonl | jq
```

## Signed distribution build

```sh
make app                     # build a real .app bundle under .build/OrchidCapture.app
make sign TEAM_ID=ABCDE12345  # codesign with Developer ID Application
make notarize NOTARY_PROFILE=orchid-notarize
make staple
```

Run `xcrun notarytool store-credentials orchid-notarize` once per machine
to bake the Apple ID, app-specific password, and team ID into the keychain
under that profile name. `make help` lists the rest.

## Follow-ups (out of scope for the prototype)

- **Sandboxed build that still reads AX window titles.** Probably means
  splitting the AX read into a non-sandboxed helper tool.
- **TextField global-shortcut listener** to capture the current selection
  from any text editor (needs Accessibility + a CGEvent global monitor).
- **Drag-and-drop input** on the popover — drop a file/url and have it
  become the artifact without going through the clipboard.
