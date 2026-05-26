import { chromium } from 'playwright'
import { mkdir } from 'node:fs/promises'

const BASE = process.env.SNAP_BASE ?? 'http://localhost:5173'
const OUT  = new URL('./', import.meta.url).pathname

const shots = [
  { file: 'diagram-architecture.png', path: '/docs/architecture' },
  { file: 'diagram-issue.png',        path: '/docs/architecture', which: 1 },
  { file: 'diagram-vms.png',          path: '/docs/vms' },
  { file: 'diagram-supervision.png',  path: '/docs/supervision' },
  { file: 'diagram-capture.png',      path: '/docs/capture' },
  { file: 'diagram-journey.png',      path: '/docs/getting-started' },
  { file: 'diagram-card-states.png',  path: '/docs/dashboard' },
]

async function main() {
  await mkdir(OUT, { recursive: true })
  const browser = await chromium.launch()
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 })
  for (const s of shots) {
    const page = await ctx.newPage()
    console.log(`→ ${BASE + s.path}`)
    await page.goto(BASE + s.path, { waitUntil: 'networkidle' })
    await page.waitForTimeout(1200)
    const all = await page.$$('.docs-diagram')
    const target = all[s.which ?? 0]
    if (target) await target.screenshot({ path: OUT + s.file })
    await page.close()
  }
  await browser.close()
}
main().catch((e) => { console.error(e); process.exit(1) })
