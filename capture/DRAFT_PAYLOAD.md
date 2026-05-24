# Orchid Capture — Draft Payload

Captured ideas flow through a single JSON shape on their way to Orchid. The macOS
and iOS prototypes both produce this payload; the eventual server-side
"draft → issue" step consumes it.

Keep it boring: a draft is just the raw material a user collected plus a short
human note. The Orchid backend (or a future LLM step) is responsible for turning
this into a polished issue body, picking labels, etc.

## Schema (v0)

```jsonc
{
  "id":         "01HRZK8X9PY...",        // ULID generated client-side
  "createdAt":  "2026-05-23T14:21:09Z",  // ISO-8601 UTC
  "source":     "macos" | "ios",
  "kind":       "screenshot" | "link" | "text" | "voice",

  // Free-form note the human typed/spoke describing the capture.
  "note":       "the inbox table jumps when a row updates",

  // Type-specific payload. Exactly one of these is set.
  "image":      { "mime": "image/png", "bytes_base64": "..." },
  "link":       { "url": "https://...", "title": "optional page title" },
  "text":       { "body": "...selected text...", "originURL": "https://..." },
  "voice":      { "mime": "audio/m4a", "bytes_base64": "...", "durationSec": 9.4 },

  // Optional hints the capture layer can attach.
  "context": {
    "appName":   "Safari",
    "windowTitle": "Orchid · denoland/orchid",
    "selection":   "..."   // text selection if available
  },

  // Where this draft should land. The prototype leaves this empty;
  // the future server step will route to denoland/orchid by default.
  "target": {
    "repo":   "denoland/orchid",
    "labels": ["clawpatrol"]
  }
}
```

## Lifecycle

```
[capture surface] -> [local JSONL queue] -> [submit] -> [Orchid draft endpoint]
                          ^                                       |
                          |                                       v
                          +----- retry on failure ---- [GitHub issue or draft]
```

The prototypes implement the first two boxes only. `submit` is a stub that
prints the payload and writes it to a local file — wiring it to a real Orchid
endpoint is a follow-up (see `capture/README.md`).

## Why JSONL on disk?

- Survives the app being killed mid-capture.
- Trivial to inspect with `tail -f` or `jq`.
- The future submit worker is just "drain the file, retry on failure".

## Open questions

- Whether the LLM normalization step lives in the capture app, on the Orchid
  server, or as an Orchid worker target itself. The prototypes assume server.
- How to deduplicate near-identical screenshots taken seconds apart.
- Whether voice should be transcribed on-device (Speech.framework) before submit
  or server-side. On-device is faster for the user; server-side gives one model
  consistency.
