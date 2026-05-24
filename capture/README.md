# Orchid Capture

> _The bottleneck isn't writing the issue. It's getting the thought into
> Orchid before it evaporates._

Ambient idea intake for Orchid. Two apps + one backend route, all sharing a
single JSON draft format.

## End-to-end at a glance

```
┌──────────────────────┐    ┌──────────────────────┐
│  macOS menu bar app  │    │     iOS voice app    │
│  (capture/macos/)    │    │     + share ext      │
│                      │    │     (capture/ios/)   │
└──────────┬───────────┘    └──────────┬───────────┘
           │ POST /api/drafts          │
           │ X-Capture-Token: ...      │
           ▼                           ▼
   ┌───────────────────────────────────────────┐
   │      orch  -capture-only  (or full)       │
   │                                           │
   │   /api/drafts  → gh issue create          │
   │   /captures/*  → serve image + voice      │
   └───────────────────┬───────────────────────┘
                       │
                       ▼
              GitHub issue in
              denoland/orchid
              (or any target repo)
```

**To run the whole thing locally on your Mac**, see
[`END_TO_END.md`](./END_TO_END.md). Three commands, two terminal tabs.

## Layout

```
capture/
├── README.md                       (this file)
├── END_TO_END.md                   step-by-step local run guide
├── DRAFT_PAYLOAD.md                shared payload schema
├── local.hcl                       capture-only orch config for local dev
├── macos/                          SwiftPM executable, MenuBarExtra app
│   ├── Package.swift
│   ├── Makefile                    build / sign / notarize / staple
│   ├── Info.plist
│   ├── Entitlements.plist
│   ├── README.md
│   └── Sources/OrchidCapture/
│       ├── OrchidCaptureApp.swift
│       ├── ComposerView.swift
│       ├── ClipboardWatcher.swift
│       ├── ScreenshotWatcher.swift     ~/Desktop screenshot detection
│       ├── AppContext.swift            frontmost app + AX window title
│       ├── Hotkey.swift                KeyboardShortcuts wiring
│       ├── FloatingComposerPanel.swift hotkey-summoned NSPanel
│       ├── DraftStore.swift            queue + HTTP submit
│       └── Draft.swift
└── ios/                            Swift Playgrounds App + Share Extension
    ├── README.md
    ├── project.yml                 XcodeGen spec (app + extension)
    ├── OrchidCaptureIOS.swiftpm/   main app sources (.swiftpm format)
    │   ├── Package.swift
    │   ├── MyApp.swift
    │   ├── ContentView.swift
    │   ├── WaveformRing.swift
    │   ├── Recorder.swift              AVAudioRecorder + AVAudioEngine tap
    │   ├── Transcriber.swift           SFSpeechRecognizer (on-device)
    │   ├── Draft.swift
    │   └── DraftStore.swift
    ├── ShareExtension/
    │   ├── README.md                   migration guide
    │   ├── Info.plist
    │   ├── ShareViewController.swift
    │   ├── ShareComposer.swift
    │   └── ShareDraftSubmitter.swift
    └── SupportingFiles/
        ├── AppInfo.plist
        ├── OrchidCapture.entitlements
        └── ShareExtension.entitlements
```

And in the orch source tree (the Go binary), the matching server-side
pieces:

```
capture_api.go    /api/drafts handler + /captures/* asset server
orch.go           CaptureBlock config + -capture-only flag
```

## Design intent

Five rules the prototypes try to follow, from the gist:

1. **Faster than opening GitHub.** From idea to draft should be one keypress
   on macOS, one tap on iOS.
2. **Use what's already there.** The user just took a screenshot / copied a
   link / spoke a thought — don't make them paste it again.
3. **Ask only what we can't infer.** One note field, that's it.
4. **Drafts, not issues.** The capture layer talks to `orch`, which talks
   to GitHub. The capture layer never writes to GitHub directly.
5. **Black and white only.** No accent colors, no status badges, no icons
   except a few utilitarian glyphs.

## Wire format

[`DRAFT_PAYLOAD.md`](./DRAFT_PAYLOAD.md). Both apps produce the same JSON
shape; the macOS app writes them to
`~/Library/Application Support/OrchidCapture/queue.jsonl` and the iOS app
to `Documents/orchid-capture-queue.jsonl`, then POSTs to `/api/drafts`.

## Research notes

Comparable apps, libraries, and platform patterns I looked at before
writing any code:

### macOS menu bar patterns
- **`MenuBarExtra`** (SwiftUI, macOS 13+). [Apple docs][menubar-apple],
  [nilcoalescing tutorial][menubar-nilcoalescing],
  [sarunw tutorial][menubar-sarunw].
- **Maccy** — open-source clipboard manager, the reference for
  `NSPasteboard.changeCount` polling. https://github.com/p0deje/Maccy.
- **Paste / CleanShot X / Raycast** — commercial inspirations for "ambient
  capture surface that doesn't get in the way." Raycast Notes is the
  closest UX analogue.
- **`LSUIElement` / `NSApp.setActivationPolicy(.accessory)`** — hides the
  dock icon. The prototype sets it dynamically at launch *and* declares it
  in the bundled `Info.plist` for the signed build.

### Global hotkeys on macOS
- **[sindresorhus/KeyboardShortcuts][ks]** — wraps Carbon
  `RegisterEventHotKey`, SwiftUI-native, lets users rebind. The prototype
  depends on it and ships a Settings recorder.
- **`RegisterEventHotKey`** (Carbon) is the underlying API; tied to the
  legacy Carbon Toolbox but still the only sanctioned path. [Discussion of
  the macOS 15 Option-only modifier regression][carbon-fb15168205].

### Clipboard / screenshot detection
- `NSPasteboard.changeCount` polled at 0.5 s — what the prototype uses,
  what Maccy uses, what every clipboard manager uses.
- `NSMetadataQuery` with `kMDItemIsScreenCapture = 1` — what the prototype
  uses to detect new `~/Desktop`-saved screenshots. The save location is
  read from `com.apple.screencapture/location` so a customised location
  works without extra config.
- **ScreenCaptureKit** (macOS 14+) lets the app initiate its own captures;
  needs screen recording permission. Not used.

### iOS voice / waveform / transcription
- **`AVAudioRecorder` + `isMeteringEnabled`** — the prototype's m4a writer
  and meter source. [createwithswift tutorial][wave-cws].
- **`AVAudioEngine` input tap** — the prototype taps the same input and
  feeds buffers into `SFSpeechRecognizer` for live on-device transcription.
- **`SFSpeechRecognizer` with `requiresOnDeviceRecognition`** — keeps
  audio on the device. Locale-dependent; the prototype falls back to
  cloud recognition gracefully when on-device isn't available.
- **[dmrschmidt/DSWaveformImage][dswave]** — production-quality SwiftUI
  waveform views, includes a `CircularWaveformRenderer`. The prototype
  hand-rolls a 60-spoke ring instead — fewer deps, less code, fine for v0.
- **Apple Voice Memos / Otter / Granola / Just Press Record** — UX
  references for review-screen patterns.

### iOS share extensions
- Apple's **Share Extension** target type is the right home for "select
  text → share → Orchid" and "share screenshot → Orchid" flows. The
  prototype includes a working share extension under `ios/ShareExtension/`
  plus an XcodeGen `project.yml` so you can generate the full app +
  extension Xcode project in one command. The `.swiftpm` format can't host
  the second target, hence the dual layout.

### Comparable open-source capture apps
- **Drafts** — the canonical "start with the text, decide where it goes
  later" app. Not open source, but the model is exactly what Orchid
  Capture aspires to.
- **Bear / Things / Reminders quick-add** — single-field capture surfaces.
- **Linear Quick Issue Capture** (`⌘K` then "new issue") — the most
  direct competitor in the dev-tools space.

## What was promised in the first scaffold PR, and where it landed

Each line item from the first PR's "intentionally not yet" list is now
wired up here:

| First PR follow-up | Where it landed |
|---|---|
| 1. Real submission endpoint in orch binary    | `capture_api.go` + `/api/drafts` route, `-capture-only` flag in `orch.go` |
| 2. Server-side draft → issue                  | `renderDraftIssue` + `gh issue create` in `capture_api.go` |
| 3. Global hotkey on macOS                     | `capture/macos/.../Hotkey.swift` + Settings recorder |
| 4. ~/Desktop screenshot watcher               | `capture/macos/.../ScreenshotWatcher.swift` (NSMetadataQuery) |
| 5. iOS share extension                        | `capture/ios/ShareExtension/` + XcodeGen `project.yml` |
| 6. On-device transcription                    | `capture/ios/.../Transcriber.swift` + AVAudioEngine tap in Recorder |
| 7. Code signing / notarization / distribution | `capture/macos/Makefile` + `Info.plist`, `Entitlements.plist` |
| 8. App / window context on macOS              | `capture/macos/.../AppContext.swift` (NSWorkspace + AX API) |

What's still genuinely deferred (and now lives in this PR's
[`END_TO_END.md`](./END_TO_END.md)#what-still-requires-manual-setup):

- **Public asset URL.** Until `orch` is deployed somewhere reachable by
  GitHub's renderer, image embeds in issues fall back to a local-path note.
- **iOS distribution** (TestFlight, code signing for App Store).
- **Drain worker** for queued drafts that failed to submit while the
  endpoint was down.

[menubar-apple]:        https://developer.apple.com/documentation/SwiftUI/MenuBarExtra
[menubar-nilcoalescing]: https://nilcoalescing.com/blog/BuildAMacOSMenuBarUtilityInSwiftUI/
[menubar-sarunw]:       https://sarunw.com/posts/swiftui-menu-bar-app/
[ks]:                   https://github.com/sindresorhus/KeyboardShortcuts
[carbon-fb15168205]:    https://github.com/feedback-assistant/reports/issues/552
[wave-cws]:             https://www.createwithswift.com/creating-a-live-audio-waveform-in-swiftui/
[dswave]:               https://github.com/dmrschmidt/DSWaveformImage
