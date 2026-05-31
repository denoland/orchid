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

// PR-status octicons used in the session rows.
function PrOpen() {
  return <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z" /></svg>
}
function PrMerged() {
  return <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor"><path d="M5.45 5.154A4.25 4.25 0 0 0 9.25 7.5h1.378a2.251 2.251 0 1 1 0 1.5H9.25A5.734 5.734 0 0 1 5 7.123v3.505a2.25 2.25 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.95-.218ZM4.25 13.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Zm8.5-4.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5ZM5 3.25a.75.75 0 1 0-1.5 0 .75.75 0 0 0 1.5 0Z" /></svg>
}

// The new dashboard: tabbed nav, GitHub-style session rows with PR/CI
// status, and the Usage & pacing sidebar. Shared by the dashboard +
// targets mockups.
function DashFrame() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dmn-nav">
        <ClaudeMark />
        <span className="dmn-tab on">Sessions<i className="dmn-pill">24</i></span>
        <span className="dmn-tab">Machines<i className="dmn-pill">5</i></span>
        <span className="dmn-tab">Analytics</span>
        <span className="dmn-tab">Integrations<i className="dmn-pill">3</i></span>
        <span className="dmn-tab">Settings</span>
        <span className="dmn-search">Search…</span>
      </div>
      <div className="dmn-body">
        <div className="dmn-list">
          <div className="dmn-row needs">
            <span className="dmn-pr open"><PrOpen /></span>
            <div className="dmn-main">
              <div className="dmn-title">Tray icon flickers on wake from sleep</div>
              <div className="dmn-meta">codex · clawpatrol · #184 · 4m</div>
            </div>
            <span className="dmn-flag">needs you</span>
          </div>
          <div className="dmn-row">
            <span className="dmn-pr open"><PrOpen /></span>
            <div className="dmn-main">
              <div className="dmn-title">npm: cli-table push() is not a function</div>
              <div className="dmn-meta">claude · deno · #24986 · 11m</div>
            </div>
            <span className="dmn-ci pass">✓ CI</span>
          </div>
          <div className="dmn-row">
            <span className="dmn-pr open"><PrOpen /></span>
            <div className="dmn-main">
              <div className="dmn-title">LSP: autocomplete in deno.json not working</div>
              <div className="dmn-meta">claude · deno · #23898 · 27m</div>
            </div>
            <span className="dmn-ci fail">✕ CI</span>
          </div>
          <div className="dmn-row">
            <span className="dmn-pr work"><span className="dmn-spin" /></span>
            <div className="dmn-main">
              <div className="dmn-title">Support https deps in package.json</div>
              <div className="dmn-meta">codex · deno · #27542 · 2m</div>
            </div>
            <span className="dmn-ci run">working</span>
          </div>
          <div className="dmn-row dim">
            <span className="dmn-pr merged"><PrMerged /></span>
            <div className="dmn-main">
              <div className="dmn-title">Refactor lsp to separate service struct</div>
              <div className="dmn-meta">claude · deno · #26847 · 1h</div>
            </div>
            <span className="dmn-tag">merged</span>
          </div>
        </div>
        <aside className="dmn-aside">
          <div className="dmn-ah">Usage &amp; pacing</div>
          <div className="dmn-q">
            <div className="dmn-qtop"><span>claude</span><span>58%</span></div>
            <div className="dmn-bar"><i style={{ width: '58%' }} /></div>
            <div className="dmn-bar sm"><i style={{ width: '15%' }} /></div>
          </div>
          <div className="dmn-q">
            <div className="dmn-qtop"><span>codex</span><span>19%</span></div>
            <div className="dmn-bar"><i className="g" style={{ width: '19%' }} /></div>
            <div className="dmn-bar sm"><i className="g" style={{ width: '7%' }} /></div>
          </div>
          <div className="dmn-ah sep">VMs</div>
          <div className="dmn-vm"><i className="vd on" />local<b>12/12</b></div>
          <div className="dmn-vm"><i className="vd on" />mac-mini<b>7/7</b></div>
          <div className="dmn-vm"><i className="vd on" />local-codex<b>6/6</b></div>
        </aside>
      </div>
    </Frame>
  )
}

// ─── dashboard: the tabbed list UI + telemetry sidebar ──────────────
export function DashboardMockup() {
  return <DashFrame />
}

// ─── settings: tabbed nav + flattened, auto-saving form ─────────────
export function SettingsMockup() {
  return (
    <Frame url="divy.orchid.littledivy.com">
      <div className="dmn-nav">
        <ClaudeMark />
        <span className="dmn-tab">Sessions<i className="dmn-pill">24</i></span>
        <span className="dmn-tab">Machines<i className="dmn-pill">5</i></span>
        <span className="dmn-tab">Analytics</span>
        <span className="dmn-tab">Integrations<i className="dmn-pill">3</i></span>
        <span className="dmn-tab on">Settings</span>
      </div>
      <div className="dm-form solo">
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
        <div className="dm-saved">✓ saved · changes auto-apply</div>
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
        <div className="dmn-nav">
          <ClaudeMark />
          <span className="dmn-tab on">Sessions<i className="dmn-pill">3</i></span>
          <span className="dmn-tab">Machines</span>
          <span className="dmn-tab">Analytics</span>
          <span className="dmn-search">Search…</span>
        </div>
        <div className="dmn-list pad">
          <div className="dmn-row">
            <span className="dmn-pr work"><span className="dmn-spin" /></span>
            <div className="dmn-main">
              <div className="dmn-title">tray icon flickers when state changes</div>
              <div className="dmn-meta">claude · clawpatrol · #184 · 3m</div>
            </div>
            <span className="dmn-ci run">working</span>
          </div>
          <div className="dmn-row">
            <span className="dmn-pr open"><PrOpen /></span>
            <div className="dmn-main">
              <div className="dmn-title">add Node lazy-init for child_process</div>
              <div className="dmn-meta">claude · deno · #160 · 12m</div>
            </div>
            <span className="dmn-ci pass">✓ CI</span>
          </div>
          <div className="dmn-row">
            <span className="dmn-pr work"><span className="dmn-spin" /></span>
            <div className="dmn-main">
              <div className="dmn-title">avoid remote-control race during bootstrap</div>
              <div className="dmn-meta">codex · orchid · #159 · 1m</div>
            </div>
            <span className="dmn-ci run">working</span>
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
{A('ntfy_topic')}    = {S('orchid-divy-7f3k9')}{'\n}\n\n'}
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
