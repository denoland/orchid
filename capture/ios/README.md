# Orchid Capture — iOS

A SwiftUI iOS app with two targets and a paired watchOS companion:

- **OrchidCapture** — the main single-screen voice-capture app. One big
  central record circle, ring waveform around it, live on-device
  transcription, review sheet, submit. Lives under
  `OrchidCaptureIOS.swiftpm/`.

- **OrchidCaptureShareExtension** — a system Share Extension that accepts
  images, URLs, and selected text from anywhere on iOS. Lives under
  `ShareExtension/`. Requires migrating from the `.swiftpm` package format
  to a regular Xcode project; see `ShareExtension/README.md` and the
  `project.yml` XcodeGen spec in this directory.

- **OrchidCaptureWatch** — the watchOS companion (the wrist drawn on the
  landing page). Hold the dot, speak, lift to submit a voice draft.
  Lives under `OrchidCaptureWatchSources/`. Reads its endpoint + token
  from the paired iPhone via WatchConnectivity, so first-time setup is
  one tap on the phone (see `PhoneSyncForWatch.swift.example`).

## Quick run (main app only)

```sh
cd capture/ios
open OrchidCaptureIOS.swiftpm
```

Xcode opens the package as an iOS app. Pick a Simulator or your device,
⌘R. Grant **microphone** and **speech recognition** when prompted.

To point it at a local orch capture server, set scheme env vars before
launch (Xcode > Product > Scheme > Edit Scheme > Run > Arguments):

| Name | Value |
|---|---|
| `ORCHID_CAPTURE_ENDPOINT` | `http://<your-mac-ip>:8787/api/drafts` |
| `ORCHID_CAPTURE_TOKEN` | `local-dev-token` |

A real iPhone reaches a Mac over the LAN at the Mac's IP, not `localhost`.

## Full run (main app + share extension)

The `.swiftpm` format only supports one target. To get the share extension
running, generate a regular Xcode project:

```sh
brew install xcodegen          # one-time
cd capture/ios
xcodegen generate
open OrchidCapture.xcodeproj
```

`project.yml` declares both targets, the App Group entitlement, the shared
`Draft.swift`, and the Info.plist usage strings. Set your team ID, build,
run the OrchidCaptureShareExtension scheme (Safari is a good host app to
test from), share any web page → "Orchid Capture" → describe → Capture.

Manual conversion path (no XcodeGen) is in `ShareExtension/README.md`.

## Watch app (paired companion)

The watchOS target builds alongside the iOS app and gets embedded into
its bundle (see `project.yml` → `OrchidCaptureWatch`). To run it:

```sh
cd capture/ios
xcodegen generate
open OrchidCapture.xcodeproj
# Pick the OrchidCaptureWatch scheme + a paired iPhone+Watch simulator
# (e.g. "iPhone 15 + Apple Watch Series 9"). ⌘R.
```

Grant **mic** and **speech recognition** when prompted. Hold the dot,
speak, lift. The watch posts a `voice` draft straight to
`/api/drafts` on the orch capture endpoint it received from the phone.

### Pairing flow

On first launch the watch shows *"Open iPhone app to pair"* until the
phone pushes endpoint + token via WatchConnectivity. Wire that on the
phone side with the snippet in
[`PhoneSyncForWatch.swift.example`](PhoneSyncForWatch.swift.example):

```swift
PhoneSyncForWatch.shared.pushToWatch(
    endpoint: URL(string: settings.endpoint),
    token:    settings.captureToken
)
```

Call it once on app launch (after settings load) and again whenever
the user edits the endpoint or rotates the capture token.

### Dev fallback (no phone)

For simulator / device builds without a paired iPhone, set scheme env
vars on the OrchidCaptureWatch scheme — same keys the iOS and macOS
apps use:

| Name | Value |
|---|---|
| `ORCHID_CAPTURE_ENDPOINT` | `http://<your-mac-ip>:8787/api/drafts` |
| `ORCHID_CAPTURE_TOKEN` | `local-dev-token` |

### Preview without building

Each watch view has a `#Preview` so you can render the capture screen
or settings sheet in Xcode's canvas (`OrchidCaptureWatchSources/*.swift`,
⌥⌘↩) without launching a watch simulator. Useful for tweaking the
ring animation.

## What's there

| File | Role |
|---|---|
| `OrchidCaptureIOS.swiftpm/MyApp.swift`         | App entry. |
| `OrchidCaptureIOS.swiftpm/ContentView.swift`   | Main screen — record + transcript footer + review sheet. |
| `OrchidCaptureIOS.swiftpm/WaveformRing.swift`  | The 60-spoke ring around the record button. |
| `OrchidCaptureIOS.swiftpm/Recorder.swift`      | `AVAudioRecorder` for the m4a + `AVAudioEngine` tap for transcription. |
| `OrchidCaptureIOS.swiftpm/Transcriber.swift`   | `SFSpeechRecognizer`, on-device when supported. |
| `OrchidCaptureIOS.swiftpm/Draft.swift`         | Shared draft model used by app + share extension. |
| `OrchidCaptureIOS.swiftpm/DraftStore.swift`    | JSONL queue + HTTP submit + outcome state. |
| `OrchidCaptureIOS.swiftpm/Package.swift`       | iOSApplication product, mic + speech capabilities. |
| `ShareExtension/ShareViewController.swift`     | Reads NSExtensionContext, builds Draft. |
| `ShareExtension/ShareComposer.swift`           | The mini SwiftUI composer the share sheet hosts. |
| `ShareExtension/ShareDraftSubmitter.swift`     | POSTs from inside the extension. |
| `ShareExtension/Info.plist`                    | Activation rule (image, URL, text). |
| `ShareExtension/README.md`                     | Migration guide. |
| `project.yml`                                  | XcodeGen spec for all three targets. |
| `SupportingFiles/AppInfo.plist`                | Main-app Info.plist for XcodeGen builds. |
| `SupportingFiles/WatchInfo.plist`              | Watch-app Info.plist (mic + speech usage strings). |
| `SupportingFiles/*.entitlements`               | App Group capability for app + extension. |
| `OrchidCaptureWatchSources/WatchApp.swift`     | Watch app entry. |
| `OrchidCaptureWatchSources/CaptureScreen.swift`| Hold-to-record screen (matches landing page mock). |
| `OrchidCaptureWatchSources/SettingsView.swift` | Watch settings sheet (endpoint, queue actions). |
| `OrchidCaptureWatchSources/Recorder.swift`     | `AVAudioRecorder` + level meter. |
| `OrchidCaptureWatchSources/Transcriber.swift`  | `SFSpeechRecognizer`, on-device when supported. |
| `OrchidCaptureWatchSources/Draft.swift`        | Wire-compatible Draft model. |
| `OrchidCaptureWatchSources/DraftStore.swift`   | Queue + HTTP submit + drain. |
| `OrchidCaptureWatchSources/WatchSettings.swift`| Endpoint/token storage + WCSession receiver. |
| `PhoneSyncForWatch.swift.example`              | iPhone-side WCSession push snippet (drop into the iOS app target). |

## Manual test plan

1. Run main app on Simulator. Grant mic + speech. Big black circle visible.
2. Tap. Spokes around the ring pulse. Live transcript appears below timer.
3. Tap to stop. Review sheet shows transcript + optional text field.
4. Add a sentence ("about the orchid Capture UI"), Capture.
5. If wired to a local orch with the env vars above, see "captured" in the
   top right; otherwise the queue grows.
6. Inspect the queue in Xcode > Devices and Simulators > Container > Files.

## Watch manual test plan

1. `xcodegen generate`, open the project, pick the OrchidCaptureWatch
   scheme on a watch+phone Sim pair, ⌘R.
2. Grant mic + speech permission on the watch.
3. Status reads *"Open iPhone app to pair"*. Set the two scheme env
   vars (above) and relaunch — status flips to *"Hold to capture"*.
4. Touch and hold the ring. Haptic *start* tick, the ring scales with
   your voice, live transcript appears under the icon.
5. Lift. The icon swaps to the progress spinner, then ✓ (sent) or a
   tray icon (queued offline). The queued counter increments when
   offline; *Settings → Retry queued* drains it.
6. With `orch -capture-only` running on your Mac, the draft should
   show up as a new GitHub issue tagged `clawpatrol` in your inbox
   repo. Grep `orch.log` for `/api/drafts` if it doesn't.

## Known limitations

- The `.swiftpm` only supports the main app target — share extension
  requires the Xcode project path described above.
- Live transcription needs `requiresOnDeviceRecognition` support, which
  is locale- and device-dependent. When unavailable, audio still records;
  transcript stays empty.
- Background submit retry isn't implemented. A submission that fails
  remains in the on-device JSONL queue but nothing drains it. Drain
  worker is a follow-up. (The watch app has a manual *Retry queued*
  button in Settings as an interim measure.)
- The watch app needs `PhoneSyncForWatch.swift.example` dropped into
  the iOS main app target for over-the-air pairing to work. Until then,
  use the scheme env vars (see above).
