# Orchid Capture — iOS Share Extension

This directory holds a working **iOS Share Extension** that lets the system
share sheet hand any image, URL, or selected text directly to Orchid Capture.

## Why this is a separate directory

The main iOS app under `../OrchidCaptureIOS.swiftpm/` is a Swift Playgrounds
App package (`.swiftpm`). That format supports **exactly one application
target** — it can't host an app extension alongside the app.

The share extension code below is ready to compile. To run it, you migrate
the prototype from `.swiftpm` to a regular Xcode project that has both:

- an iOS App target (uses the source files under `../OrchidCaptureIOS.swiftpm/`)
- an iOS Share Extension target (uses the source files in this directory)

## Files

| File | Role |
|---|---|
| `ShareViewController.swift`  | Entry point; reads `NSExtensionContext`, builds the `Draft`. |
| `ShareComposer.swift`        | SwiftUI mini composer rendered inside the share sheet. |
| `ShareDraftSubmitter.swift`  | POSTs the draft to the orch `/api/drafts` endpoint. |
| `Info.plist`                 | Activation rule (images, URLs, text). |

`Draft.swift`, `DraftStore.swift` (or specifically the `Draft` types) from
the main app need to be added to **both** target memberships in Xcode so the
share extension can construct the same JSON shape.

## Migration: `.swiftpm` → Xcode project with share extension

Two paths, pick one.

### Path A (recommended) — manual conversion in Xcode

1. **Create a new Xcode project**
   - File > New > Project > iOS > App
   - Product Name: `OrchidCapture`
   - Bundle Identifier: `land.deno.orchid.capture`
   - Language: Swift, Interface: SwiftUI
   - Use Storyboard: **No**, Include Tests: optional

2. **Copy main app sources**
   Drag all `*.swift` files from
   `capture/ios/OrchidCaptureIOS.swiftpm/` (except `Package.swift`) into the
   new project's `OrchidCapture/` group. When prompted, choose **Copy items
   if needed**, target membership = `OrchidCapture` only.

3. **Add the share extension target**
   - File > New > Target > Share Extension
   - Product Name: `OrchidCaptureShareExtension`
   - Bundle Identifier: `land.deno.orchid.capture.shareextension`

4. **Replace the generated share extension sources**
   Delete the boilerplate `ShareViewController.swift` Xcode generated, then
   drag the four files in this directory into the
   `OrchidCaptureShareExtension/` group, target membership =
   `OrchidCaptureShareExtension` only. When asked about the existing
   `Info.plist`, replace it with the one in this directory.

5. **Share the `Draft` type with the extension**
   Select `Draft.swift` in the project navigator, open File Inspector
   (⌥⌘1), and check both `OrchidCapture` and
   `OrchidCaptureShareExtension` under **Target Membership**.

6. **Enable the App Group on both targets**
   On each target, Signing & Capabilities > + Capability > **App Groups** >
   add `group.land.deno.orchid.capture`. The main app's Endpoint settings
   write the endpoint+token here; the share extension reads them back.

   You'll also need to update the main app's `DraftStore.swift` to write to
   the app-group `UserDefaults` rather than `.standard` — that's a one-line
   change documented in `../OrchidCaptureIOS.swiftpm/DraftStore.swift`.

7. **Microphone + Speech usage strings**
   On the main app target's Info, add:
   - `NSMicrophoneUsageDescription` — same string as the .swiftpm capability.
   - `NSSpeechRecognitionUsageDescription` — same.

8. **Permissions on the share extension**
   The share extension needs network access (default on) and doesn't need
   mic/speech.

9. Build and run. Pick the share extension scheme in Xcode and run; iOS
   launches a host app (Safari/Photos) you can share from.

### Path B — XcodeGen one-liner

If you have [XcodeGen](https://github.com/yonaskolb/XcodeGen) installed
(`brew install xcodegen`), you can use the `project.yml` checked in next to
this README to generate everything in one command:

```sh
cd capture/ios
xcodegen generate
open OrchidCapture.xcodeproj
```

The `project.yml` declares both targets, the App Group entitlement, target
memberships for shared sources, and the Info.plist usage strings.

## Testing the share extension

1. Run the share extension scheme in Xcode. Pick **Safari** as the host app.
2. Open any web page in the launched Safari, tap the share icon, scroll
   until you see **Orchid Capture**, tap it.
3. The share sheet should slide up with the page URL pre-filled and a note
   field. Type a one-line note and tap **Capture**.
4. If the App Group is correctly configured, the request goes to your local
   orch endpoint and the share sheet dismisses on success.

## Known limitations

- **No background submit retry.** If the network fails inside the share
  extension, the draft is dropped. The right fix is to write to an App
  Group JSONL queue and have the main app drain it on next launch — left
  for a follow-up.
- **No voice in the share extension.** iOS share extensions can't record
  audio; voice capture stays in the main app.
- **Photo Library activation needs `NSExtensionActivationSupportsAttachmentsWithMaxCount`**
  if you also want to share from Photos directly. The current Info.plist
  is tuned for Safari (URL) and Notes (text/image).
