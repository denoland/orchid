// Minimal config for running orch in -capture-only mode locally on your Mac.
// Pairs with the Orchid Capture macOS / iOS apps under capture/.
//
//   go build -o orch . && ./orch -config capture/local.hcl -capture-only
//
// Then point the macOS app at the local endpoint:
//
//   export ORCHID_CAPTURE_ENDPOINT=http://127.0.0.1:8787/api/drafts
//   export ORCHID_CAPTURE_TOKEN=local-dev-token
//   cd capture/macos && swift run
//
// You'll need `gh auth login` already done in this shell so the capture
// handler can call `gh issue create` against denoland/orchid (or whatever
// default_repo you set).
//
// The fields below are intentionally minimal — the swarm-side targets, VMs,
// and bootstrap_prompt are not consulted in capture-only mode.

github {
  inbox_repo = "denoland/orchid"
}

orchestrator {
  poll_interval = "30s"
  state_db      = "/tmp/orchid-capture-state.db"
  branch_prefix = "capture/"
  workdir_root  = "/tmp/orchid-capture-work"
  http_addr     = "127.0.0.1:8787"
  bot_login     = "divybot"

  capture {
    auth_token = "local-dev-token"
    // Drop image/voice blobs here. Defaults to <state_db dir>/captures.
    assets_dir = "/tmp/orchid-capture-assets"
    // Leave public_url empty when running locally — issues will link to the
    // on-disk path with a note instead of trying to embed an unreachable URL.
    public_url = ""
    // Falls back to github.inbox_repo when empty.
    default_repo   = "denoland/orchid"
    // Leave empty for local testing — adding `clawpatrol` (or any other
    // target label) here would auto-route captured issues to the swarm.
    default_labels = []
    max_body_mb    = 16
  }
}

bootstrap_prompt = "unused in capture-only mode"
