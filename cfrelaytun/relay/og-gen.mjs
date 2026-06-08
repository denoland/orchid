// Generate per-page OpenGraph cards (1200×630) for the docs site.
//   node cfrelaytun/relay/og-gen.mjs   →   public/og/<slug>.png
import { chromium } from 'playwright'
import { readFileSync, mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const HERE = dirname(fileURLToPath(import.meta.url)) // cfrelaytun/relay
const ROOT = resolve(HERE, '../..')                  // repo root
const OUT = `${HERE}/public/og`
mkdirSync(OUT, { recursive: true })

const favicon = readFileSync(`${HERE}/public/favicon.svg`, 'utf8')
const spray = readFileSync(`${ROOT}/.claude/skills/orchid-art/orchid-spray.svg`, 'utf8')

// slug, title (on card), lede, footer path
const CARDS = [
  { slug: 'default', title: 'High velocity coding<br>agent orchestration', lede: 'A self-hosted swarm of coding agents that ship pull requests.', path: '' },
  { slug: 'docs', title: 'Documentation', lede: 'Everything orchid knows how to do — written down.', path: '/docs' },
  { slug: 'getting-started', title: 'Getting started', lede: 'Install on a machine, connect GitHub, file your first inbox issue.', path: '/docs/getting-started' },
  { slug: 'dashboard', title: 'Dashboard', lede: 'Tour the session list, PR/CI status, panes, and settings.', path: '/docs/dashboard' },
  { slug: 'configuration', title: 'Configuration', lede: 'Every field in swarm.hcl, what it does, when to change it.', path: '/docs/configuration' },
  { slug: 'throttling', title: 'Throttling & pacing', lede: 'Spend the weekly quota evenly — hard gate, adaptive governor, duty-cycling.', path: '/docs/throttling' },
  { slug: 'targets', title: 'Targets', lede: 'Route labels in the inbox to different work repos.', path: '/docs/targets' },
  { slug: 'vms', title: 'VMs', lede: 'Scale the swarm across multiple machines.', path: '/docs/vms' },
  { slug: 'security', title: 'Security', lede: 'Sandbox sessions with clawpatrol; rotate tokens.', path: '/docs/security' },
  { slug: 'tailscale', title: 'Tailscale', lede: 'Run orch without public IPs using your own tailnet.', path: '/docs/tailscale' },
  { slug: 'memory', title: 'Memory', lede: 'Shared, git-backed knowledge base the swarm accumulates across sessions.', path: '/docs/memory' },
  { slug: 'supervision', title: 'Supervision', lede: 'Chat with your orchid on Telegram, Slack, Discord via OpenClaw or Hermes.', path: '/docs/supervision' },
  { slug: 'architecture', title: 'Architecture', lede: 'How orch, the relay, and Claude sessions fit together.', path: '/docs/architecture' },
]

const html = (c) => `<!doctype html><html><head><meta charset="utf-8"><style>
  * { margin: 0; box-sizing: border-box; }
  html, body { width: 1200px; height: 630px; }
  body {
    font-family: -apple-system, 'Helvetica Neue', Arial, sans-serif;
    background: linear-gradient(135deg, #ffffff 0%, #faf5ff 70%, #f3e8ff 100%);
    position: relative; overflow: hidden;
  }
  .art {
    position: absolute; top: -60px; right: -150px; width: 760px; height: 760px;
    color: #7c3aed; opacity: 0.10;
  }
  .art svg { width: 100%; height: 100%; }
  .wrap { position: absolute; inset: 0; padding: 84px 90px; display: flex; flex-direction: column; }
  .brand { display: flex; align-items: center; gap: 16px; }
  .brand svg { width: 46px; height: 46px; }
  .brand span { font-size: 30px; font-weight: 700; letter-spacing: -0.5px; color: #18181b; }
  .title { margin-top: auto; font-size: 76px; font-weight: 700; line-height: 1.04; letter-spacing: -2px; color: #18181b; max-width: 820px; }
  .lede { margin-top: 26px; font-family: Georgia, 'Times New Roman', serif; font-style: italic; font-size: 31px; line-height: 1.4; color: #52525b; max-width: 700px; }
  .foot { margin-top: auto; font-family: ui-monospace, 'SF Mono', Menlo, monospace; font-size: 22px; color: #a78bfa; letter-spacing: 0.3px; }
</style></head><body>
  <div class="art">${spray}</div>
  <div class="wrap">
    <div class="brand">${favicon}<span>Orchid</span></div>
    <div class="title">${c.title}</div>
    <div class="lede">${c.lede}</div>
    <div class="foot">orchid.littledivy.com${c.path}</div>
  </div>
</body></html>`

const browser = await chromium.launch()
const ctx = await browser.newContext({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 2 })
for (const c of CARDS) {
  const page = await ctx.newPage()
  await page.setContent(html(c), { waitUntil: 'networkidle' })
  await page.waitForTimeout(150)
  await page.screenshot({ path: `${OUT}/${c.slug}.png` })
  console.log(`→ og/${c.slug}.png`)
  await page.close()
}
await browser.close()
console.log('done.')
