// Browser-window mockups that show "here's what it looks like" for each
// docs concept. Same aesthetic as landing's .frame: traffic-light dots,
// pill URL bar, dot-grid body. Each mockup is hand-built HTML — no real
// data fetched, just an honest still-life of the dashboard region.

import './DocsMockups.css'
import { LockIcon, ClaudeMark, IssueOpenIcon } from './DocsIcons'

function Frame({ url, children }: { url: string; children: React.ReactNode }) {
  return (
    <div className="docs-frame">
      <div className="docs-frame-bar">
        <span className="docs-dot r" />
        <span className="docs-dot a" />
        <span className="docs-dot g" />
        <span className="docs-addr">
          <LockIcon />
          {url}
        </span>
      </div>
      <div className="docs-frame-body">{children}</div>
    </div>
  )
}

// ─── dashboard: list view (back) + canvas with cards+notes (front) ─
export function DashboardMockup() {
  return (
    <div className="dm-twin">
      <DashboardListFrame />
      <DashboardCanvasFrame />
    </div>
  )
}

function DashboardListFrame() {
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

function DashboardCanvasFrame() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dm-canvas-head">
        <span className="dm-canvas-brand">Orchid <em>3</em></span>
        <span className="dm-canvas-tools">
          <span className="dm-canvas-pill">+</span>
          <span className="dm-canvas-pill">≡</span>
          <span className="dm-canvas-pill">⚙</span>
        </span>
      </div>
      <div className="dm-canvas-grid">
        <div className="dm-rf-card pos-a working">
          <div className="dm-rf-top"><ClaudeMark /><span className="dm-rf-repo">clawpatrol</span></div>
          <div className="dm-rf-title">tray icon flickers when state changes</div>
          <div className="dm-rf-bar"><div className="dm-rf-bar-fill" /></div>
        </div>
        <div className="dm-rf-card pos-b">
          <div className="dm-rf-top"><ClaudeMark /><span className="dm-rf-repo">deno</span></div>
          <div className="dm-rf-title">add Node lazy-init for child_process</div>
          <div className="dm-rf-bar"><div className="dm-rf-bar-fill" /></div>
        </div>
        <div className="dm-rf-card pos-c needs">
          <div className="dm-rf-top"><ClaudeMark /><span className="dm-rf-repo">orchid</span></div>
          <div className="dm-rf-title">avoid remote-control race during worker bootstrap</div>
        </div>
        <div className="dm-note pos-note-a">
          ship before<br/>
          Friday demo
        </div>
        <div className="dm-note pos-note-b">
          revisit tmux<br/>
          paste race?
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

// ─── targets: github issues + orchid canvas, side by side ───────────
export function TargetsMockup() {
  return (
    <div className="dm-twin">
      <Frame url="github.com/denoland/orchid/issues">
        <div className="gh-mock">
          <div className="gh-tabs">
            <span className="gh-tab active">
              <IssueOpenIcon />
              Issues <em>34</em>
            </span>
            <span className="gh-tab">Pull requests <em>7</em></span>
          </div>
          <div className="gh-row">
            <span className="gh-state">○</span>
            <div className="gh-grow">
              <div className="gh-title">fix: tray icon flickers when state changes</div>
              <div className="gh-meta">#184 opened 2h ago by <strong>littledivy</strong></div>
            </div>
            <span className="dm-lbl clawpatrol">clawpatrol</span>
          </div>
          <div className="gh-row">
            <span className="gh-state">○</span>
            <div className="gh-grow">
              <div className="gh-title">add Node lazy-init for child_process</div>
              <div className="gh-meta">#160 opened 1d ago by <strong>littledivy</strong></div>
            </div>
            <span className="dm-lbl bug">deno</span>
          </div>
          <div className="gh-row">
            <span className="gh-state">○</span>
            <div className="gh-grow">
              <div className="gh-title">avoid remote-control race during worker bootstrap</div>
              <div className="gh-meta">#159 opened 1d ago by <strong>littledivy</strong></div>
            </div>
            <span className="dm-lbl orchid">orchid</span>
          </div>
        </div>
      </Frame>

      <Frame url="divy.orchid.littledivy.com">
        <div className="dm-canvas-head">
          <span className="dm-canvas-brand">Orchid <em>3</em></span>
          <span className="dm-canvas-tools">
            <span className="dm-canvas-pill">+</span>
            <span className="dm-canvas-pill">≡</span>
            <span className="dm-canvas-pill">⚙</span>
          </span>
        </div>
        <div className="dm-canvas-grid">
          <div className="dm-rf-card pos-a working">
            <div className="dm-rf-top">
              <ClaudeMark />
              <span className="dm-rf-repo">clawpatrol</span>
            </div>
            <div className="dm-rf-title">tray icon flickers when state changes</div>
          </div>
          <div className="dm-rf-card pos-b">
            <div className="dm-rf-top">
              <ClaudeMark />
              <span className="dm-rf-repo">deno</span>
            </div>
            <div className="dm-rf-title">add Node lazy-init for child_process</div>
          </div>
          <div className="dm-rf-pane pos-pane">
            <div className="dm-rf-pane-bar">
              <span className="docs-dot r" /><span className="docs-dot a" /><span className="docs-dot g" />
              <span className="dm-rf-pane-title">claude-3 · orchid</span>
            </div>
            <div className="dm-rf-pane-body">
              <span className="t-prompt">&gt;</span> <span className="t-dim">running cargo test</span><br/>
              <span className="t-ok">  ✓ 1842 passed</span><br/>
              <span className="t-prompt">&gt;</span> <span className="cursor-blink">▌</span>
            </div>
          </div>
        </div>
      </Frame>
    </div>
  )
}

// ─── swarm.hcl snippet ──────────────────────────────────────────────
export function ConfigMockup() {
  const K = (s: string) => <span className="hcl-k">{s}</span>   // block keyword
  const L = (s: string) => <span className="hcl-l">{s}</span>   // block label
  const A = (s: string) => <span className="hcl-a">{s}</span>   // attribute
  const S = (s: string) => <span className="hcl-s">"{s}"</span> // string
  const N = (s: string) => <span className="hcl-n">{s}</span>   // number
  return (
    <div className="docs-term">
      <div className="docs-term-bar">
        <span className="docs-dot r" />
        <span className="docs-dot a" />
        <span className="docs-dot g" />
        <span className="docs-term-title">~/.orch/swarm.hcl</span>
      </div>
      <pre className="docs-term-body hcl">
{K('github')} {'{\n  '}{A('inbox_repo')} = {S('denoland/orchid')}{'\n}\n\n'}
{K('orchestrator')} {'{\n  '}{A('poll_interval')} = {S('30s')}{'\n  '}
{A('branch_prefix')} = {S('orch/issue-')}{'\n  '}
{A('bot_login')}     = {S('divybot')}{'\n  '}
{A('ntfy_topic')}    = {S('REDACTED')}{'\n}\n\n'}
{K('target')} {L('"clawpatrol"')} {'{\n  '}{A('label')} = {S('clawpatrol')}{'\n  '}
{A('repo')}  = {S('denoland/clawpatrol')}{'\n}\n\n'}
{K('vm')} {L('"fra1"')} {'{\n  '}{A('host')}     = {S('orchid@worker.fra1.example.com')}{'\n  '}
{A('capacity')} = {N('10')}{'\n}'}
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
