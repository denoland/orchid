// Browser-window mockups that show "here's what it looks like" for each
// docs concept. Same aesthetic as landing's .frame: traffic-light dots,
// pill URL bar, dot-grid body. Each mockup is hand-built HTML — no real
// data fetched, just an honest still-life of the dashboard region.

import './DocsMockups.css'

function Frame({ url, children }: { url: string; children: React.ReactNode }) {
  return (
    <div className="docs-frame">
      <div className="docs-frame-bar">
        <span className="docs-dot r" />
        <span className="docs-dot a" />
        <span className="docs-dot g" />
        <span className="docs-addr">
          <svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor"><path d="M8 1a3 3 0 0 0-3 3v3H4a2 2 0 0 0-2 2v5a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2v-5a2 2 0 0 0-2-2h-1V4a3 3 0 0 0-3-3zm2 6V4a2 2 0 1 0-4 0v3z"/></svg>
          {url}
        </span>
      </div>
      <div className="docs-frame-body">{children}</div>
    </div>
  )
}

// ─── dashboard list view ────────────────────────────────────────────
export function DashboardMockup() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dm-head">
        <span className="dm-brand">Orchid <em className="dm-count">6</em></span>
        <span className="dm-icons">
          <span className="dm-pill">+</span>
          <span className="dm-pill">≡</span>
          <span className="dm-pill">⚙</span>
          <span className="dm-pill">☾</span>
        </span>
      </div>

      <div className="dm-group">Needs you <em>1</em></div>
      <div className="dm-row needs">
        <span className="dm-dot rose" />
        <div>
          <div className="dm-title">Tray icon flickers when state changes</div>
          <div className="dm-meta">clawpatrol · #184 · PR #211</div>
        </div>
      </div>

      <div className="dm-group">Working <em>2</em></div>
      <div className="dm-row">
        <span className="dm-dot emerald" />
        <div>
          <div className="dm-title">Add Node lazy-init for child_process</div>
          <div className="dm-meta">deno · #160</div>
        </div>
      </div>
      <div className="dm-row">
        <span className="dm-dot emerald" />
        <div>
          <div className="dm-title">Avoid remote-control race during worker bootstrap</div>
          <div className="dm-meta">orchid · #159</div>
        </div>
      </div>

      <div className="dm-group">Awaiting review <em>2</em></div>
      <div className="dm-row">
        <span className="dm-dot amber" />
        <div>
          <div className="dm-title">Wire end-to-end capture pipeline</div>
          <div className="dm-meta">clawpatrol · #178 · PR #178</div>
        </div>
      </div>
    </Frame>
  )
}

// ─── settings (orchestrator pane) ───────────────────────────────────
export function SettingsMockup() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dm-head">
        <span className="dm-brand">Settings</span>
      </div>
      <div className="dm-set">
        <aside className="dm-side">
          <div className="dm-side-item active">Orchestrator</div>
          <div className="dm-side-item">Access</div>
          <div className="dm-side-item">Capture</div>
          <div className="dm-side-item">VMs</div>
          <div className="dm-side-item">Targets</div>
          <div className="dm-side-item">Usage</div>
        </aside>
        <div className="dm-form">
          <div className="dm-h">GitHub</div>
          <label className="dm-field">
            <span>Inbox repo</span>
            <input value="denoland/orchid" readOnly />
          </label>
          <div className="dm-h">Orchestrator</div>
          <label className="dm-field">
            <span>Poll interval</span>
            <input value="30s" readOnly />
          </label>
          <label className="dm-field">
            <span>Branch prefix</span>
            <input value="orch/issue-" readOnly />
          </label>
          <label className="dm-field">
            <span>Bot login</span>
            <input value="divybot" readOnly />
          </label>
        </div>
      </div>
    </Frame>
  )
}

// ─── capture composer ───────────────────────────────────────────────
export function CaptureMockup() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dm-head">
        <span className="dm-brand">Capture <em className="dm-faint">spawn an idea</em></span>
      </div>
      <div className="dm-composer">
        <div className="dm-input">Tray icon flickers when state changes…</div>
        <div className="dm-input-bar">
          <span className="dm-chip">clawpatrol ▾</span>
          <span className="dm-go">spawn ↵</span>
        </div>
      </div>
      <div className="dm-recent">Recent</div>
      <div className="dm-row"><span className="dm-dot rose" />
        <div><div className="dm-title">Tray icon flickers when state changes</div><div className="dm-meta">#184 · clawpatrol</div></div>
      </div>
      <div className="dm-row"><span className="dm-dot amber" />
        <div><div className="dm-title">Wire end-to-end capture pipeline</div><div className="dm-meta">#178 · clawpatrol</div></div>
      </div>
      <div className="dm-row"><span className="dm-dot emerald" />
        <div><div className="dm-title">Add Node lazy-init for child_process</div><div className="dm-meta">#160 · deno</div></div>
      </div>
    </Frame>
  )
}

// ─── install terminal ────────────────────────────────────────────────
// A terminal-only mockup (no browser frame) showing the curl install +
// orch join one-liners.
export function InstallMockup() {
  return (
    <div className="docs-term">
      <div className="docs-term-bar">
        <span className="docs-dot r" />
        <span className="docs-dot a" />
        <span className="docs-dot g" />
        <span className="docs-term-title">~ — bash</span>
      </div>
      <pre className="docs-term-body">
        <span className="t-prompt">$</span> <span className="t-cmd">curl -fsSL https://orchid.littledivy.com/install.sh | bash</span>{'\n'}
        <span className="t-dim">  ▶ installing Go 1.25.0…</span>{'\n'}
        <span className="t-dim">  ▶ cloning denoland/orchid…</span>{'\n'}
        <span className="t-dim">  ▶ building orch → ~/.local/bin/orch</span>{'\n'}
        <span className="t-ok">  ✓ user service enabled</span>{'\n'}
        {'\n'}
        <span className="t-prompt">$</span> <span className="t-cmd">orch join wss://divy.orchid.littledivy.com/agent ab38…91f2</span>{'\n'}
        <span className="t-ok">  ✓ connected to relay as divy</span>{'\n'}
      </pre>
    </div>
  )
}

// ─── targets: an inbox issue card with labels routing to repos ──────
export function TargetsMockup() {
  return (
    <Frame url="github.com/denoland/orchid/issues">
      <div className="dm-issue">
        <div className="dm-issue-head">
          <span className="dm-issue-state">● Open</span>
          <span className="dm-issue-title">fix: panic on empty input</span>
        </div>
        <div className="dm-issue-labels">
          <span className="dm-lbl clawpatrol">clawpatrol</span>
          <span className="dm-lbl bug">bug</span>
        </div>
        <div className="dm-issue-body">
          Reproduces on macOS 15.2. See attached pane capture…
        </div>
      </div>
      <div className="dm-routing">
        <div className="dm-arrow">↳ routes to <code>denoland/clawpatrol</code> via <code>target "clawpatrol"</code></div>
      </div>
    </Frame>
  )
}

// ─── swarm.hcl snippet ──────────────────────────────────────────────
export function ConfigMockup() {
  return (
    <div className="docs-term">
      <div className="docs-term-bar">
        <span className="docs-dot r" />
        <span className="docs-dot a" />
        <span className="docs-dot g" />
        <span className="docs-term-title">~/.orch/swarm.hcl</span>
      </div>
      <pre className="docs-term-body hcl">
{`github {
  inbox_repo = "denoland/orchid"
}

orchestrator {
  poll_interval = "30s"
  branch_prefix = "orch/issue-"
  bot_login     = "divybot"
  ntfy_topic    = "orchid-divy-7f3k9"
}

target "clawpatrol" {
  label = "clawpatrol"
  repo  = "denoland/clawpatrol"
}

vm "fra1" {
  host     = "orchid@worker.fra1.example.com"
  capacity = 10
}`}
      </pre>
    </div>
  )
}

// ─── telegram chat ───────────────────────────────────────────────────
export function TelegramMockup() {
  return (
    <div className="docs-phone">
      <div className="docs-phone-bar">
        <span className="docs-phone-time">9:41</span>
        <span className="docs-phone-name">@orchid_bot</span>
      </div>
      <div className="docs-phone-body">
        <div className="dm-bub me">what's happening on orchid?</div>
        <div className="dm-bub bot">
          6 sessions live on <code>paris</code> · 1 needs you (#184 perm dialog)<br/>
          2 PRs awaiting review · CI green on both
        </div>
        <div className="dm-bub me">restart claude-3</div>
        <div className="dm-bub bot">done. session respawned, prompt re-pasted.</div>
      </div>
    </div>
  )
}

export const MOCKUPS: Record<string, React.FC> = {
  dashboard: DashboardMockup,
  settings:  SettingsMockup,
  capture:   CaptureMockup,
  install:   InstallMockup,
  targets:   TargetsMockup,
  config:    ConfigMockup,
  telegram:  TelegramMockup,
}
