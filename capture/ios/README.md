# Orchid Capture — iOS

A SwiftUI iOS app with two targets:

- **OrchidCapture** — the main single-screen voice-capture app. One big
  central record circle, ring waveform around it, live on-device
  transcription, review sheet, submit. Lives under
  `OrchidCaptureIOS.swiftpm/`.

- **OrchidCaptureShareExtension** — a system Share Extension that accepts
  images, URLs, and selected text from anywhere on iOS. Lives under
  `ShareExtension/`. Requires migrating from the `.swiftpm` package format
  to a regular Xcode project; see `ShareExtension/README.md` and the
  `project.yml` XcodeGen spec in this directory.

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
| `project.yml`                                  | XcodeGen spec for both targets. |
| `SupportingFiles/AppInfo.plist`                | Main-app Info.plist for XcodeGen builds. |
| `SupportingFiles/*.entitlements`               | App Group capability for app + extension. |

## Manual test plan

1. Run main app on Simulator. Grant mic + speech. Big black circle visible.
2. Tap. Spokes around the ring pulse. Live transcript appears below timer.
3. Tap to stop. Review sheet shows transcript + optional text field.
4. Add a sentence ("about the orchid Capture UI"), Capture.
5. If wired to a local orch with the env vars above, see "captured" in the
   top right; otherwise the queue grows.
6. Inspect the queue in Xcode > Devices and Simulators > Container > Files.

## Known limitations

- The `.swiftpm` only supports the main app target — share extension
  requires the Xcode project path described above.
- Live transcription needs `requiresOnDeviceRecognition` support, which
  is locale- and device-dependent. When unavailable, audio still records;
  transcript stays empty.
- Background submit retry isn't implemented. A submission that fails
  remains in the on-device JSONL queue but nothing drains it. Drain
  worker is a follow-up.
