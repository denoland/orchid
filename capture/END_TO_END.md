# Orchid Capture — end-to-end on your Mac

This is the run-it-yourself guide. The flow at the end of these steps:

1. You take a screenshot, type one sentence, press ⌘↩.
2. The macOS Capture app POSTs the draft to a locally running `orch` binary.
3. `orch` writes the image to disk, calls `gh issue create`, and returns
   the new issue URL.
4. The composer shows "captured → orchid#NNN" with a clickable link.

Total moving parts: **two terminal tabs and one menu bar app**.

## Prereqs

| Need | Why |
|---|---|
| macOS 13 Ventura or newer | `MenuBarExtra` requires it. |
| Xcode 15+ or Swift 5.9 toolchain (`xcode-select --install`) | Builds the capture app. |
| Go 1.22+ (`brew install go`) | Builds the `orch` binary. |
| `gh` CLI authed (`gh auth login`) | `orch` calls it to file issues. |
| Write access to whatever `default_repo` you configure | Or it 403s. |

The defaults in `capture/local.hcl` file issues against `denoland/orchid`.
If that's not where you want test issues, change `default_repo` first.

## 1. Start the capture server (terminal tab 1)

From the orchid repo root:

```sh
go build -o orch .
./orch -config capture/local.hcl -capture-only
```

Expected log line:

```
orchid capture: listening on http://127.0.0.1:8787/, assets under /tmp/orchid-capture-assets
```

The `-capture-only` flag tells `orch` to skip the swarm logic and VM
bootstrap — only the `/api/drafts` and `/captures/*` HTTP routes go live.
Issues are created in your default repo via the `gh` CLI in your shell.

> **Production note:** the same endpoints are wired into the regular
> `./orch -config swarm.hcl` flow when the config has a `capture { ... }`
> block. `-capture-only` is just a faster local loop.

### Smoke-test with curl

```sh
curl -s -X POST http://127.0.0.1:8787/api/drafts \
  -H 'X-Capture-Token: local-dev-token' \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "test-1",
    "createdAt": "2026-05-23T10:00:00Z",
    "source": "manual",
    "kind": "text",
    "note": "hello from curl",
    "text": { "body": "this is a test capture" }
  }' | jq
```

You should see:

```json
{ "ok": true, "id": "test-1", "issue_url": "https://github.com/.../issues/123" }
```

If you got the issue URL, the capture server is wired up correctly. Close
the test issue on GitHub.

## 2. Run the macOS capture app (terminal tab 2)

```sh
cd capture/macos
swift run
```

First run resolves the `KeyboardShortcuts` dependency from the Swift Package
Index (~10 seconds), builds, then launches. A small circle should appear in
the menu bar; no dock icon.

### Tell it where to POST

Click the menu bar circle → `…` → **Settings...** → **Endpoint**.

- **Endpoint URL:** `http://127.0.0.1:8787/api/drafts`
- **X-Capture-Token:** `local-dev-token`

(Both match `capture/local.hcl`. Adjust if you changed them.)

You can also bootstrap via env vars instead of the UI:

```sh
ORCHID_CAPTURE_ENDPOINT=http://127.0.0.1:8787/api/drafts \
ORCHID_CAPTURE_TOKEN=local-dev-token \
swift run
```

## 3. Capture something for real

Three flows to try:

### Screenshot to clipboard

1. Press `⌃⇧⌘4`, drag-select an area. The image lands on your clipboard.
2. Click the menu bar circle (or hit ⌃⌥⌘O for the floating panel).
3. The composer's top row should say "screenshot" with a thumbnail.
4. Type a sentence ("the inbox table jitters when a row updates"), press
   ⌘↩.
5. Below the field: "captured → orchid#NNN" with a clickable link. Click
   it to see the GitHub issue with your note as the title and the image
   embedded inline (when `public_url` is configured) or a path note (when
   running locally).

### Saved screenshot

1. Press `⌘⇧3` (full-screen) or `⌘⇧4` (region). The screenshot is saved
   to `~/Desktop` instead of the clipboard.
2. Open the composer. The artifact row should say **saved screenshot** with
   the filename.
3. Same flow as above. Submit.

### Link

1. Copy any URL.
2. Open the composer. Artifact row should say "link" with the host.
3. Type a one-line note, submit.

## 4. Watch the orch logs

In terminal tab 1, every accepted draft logs nothing by default — it's
boring HTTP. If `gh issue create` fails (label not present, auth scope
wrong, repo doesn't exist) you'll see the error in the macOS composer
under the field, **and** the draft will still be on disk at
`~/Library/Application Support/OrchidCapture/queue.jsonl` so you can
inspect / replay it.

To watch the queue grow:

```sh
tail -f ~/Library/Application\ Support/OrchidCapture/queue.jsonl | jq
```

## 5. (Optional) iOS app

```sh
open capture/ios/OrchidCaptureIOS.swiftpm
```

Xcode opens the Swift Playgrounds App project. Pick an iOS Simulator,
⌘R. On first run iOS asks for **microphone** and **speech recognition**
permission — allow both.

Voice flow:

1. Big black circle in the middle. Tap it.
2. The ring of black spokes pulses as you speak; the on-device transcript
   streams below the timer.
3. Tap to stop. A sheet shows the transcript and an optional text field.
4. Tap **Capture**.

The simulator can reach `http://127.0.0.1:8787` from your Mac, but
`http://localhost` resolves inside the simulator's container — use
`http://host.docker.internal` from a real iPhone on the same Wi-Fi, or
expose via `ssh -R` / `cloudflared` / `ngrok` for device testing.

> The iOS `.swiftpm` does not include the **share extension** target —
> that requires a regular Xcode project with two targets. See
> `capture/ios/ShareExtension/README.md` for the migration path.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| "queued — no endpoint configured" | URL or token blank | Set them in Settings → Endpoint. |
| "queued — HTTP 401: bad token" | Token mismatch | Settings token must match `auth_token` in `capture/local.hcl`. |
| "queued — could not connect to server" | orch not running | Start it in terminal tab 1. |
| "could not add label: '...' not found" | The configured label doesn't exist on the target repo | Remove it from `default_labels` in the HCL or create the label. |
| Composer never shows screenshot | macOS Spotlight indexing on `~/Desktop` is off | `mdutil -i on ~/Desktop` |
| Window title context is empty | App not granted Accessibility | System Settings > Privacy & Security > Accessibility > add the running OrchidCapture binary. |
| ⌃⌥⌘O does nothing | The default conflicts with another app's shortcut | Settings > Hotkey > rebind. |
| iOS Simulator can't reach `127.0.0.1:8787` | Network from simulator | Use the Mac's LAN IP, not localhost. |

## What still requires manual setup

- **Real public_url for asset embedding in GitHub.** Locally, the image
  URL would be `http://127.0.0.1:8787/captures/<id>.png` — unreachable
  from GitHub's renderer. Either deploy `orch` to a public host (set
  `public_url` to it), or accept that local images stay as path notes
  inside the issue body until the production endpoint is wired up.
- **Code signing.** `swift run` produces an unsigned binary that's fine
  for personal use but Gatekeeper will refuse a downloaded copy. See
  `capture/macos/Makefile` for the sign + notarize flow.
- **iOS distribution.** TestFlight or Ad-Hoc; not in scope for this PR.
