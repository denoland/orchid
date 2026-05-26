import { chromium } from 'playwright'
import { mkdir } from 'node:fs/promises'

const BASE = process.env.SNAP_BASE ?? 'http://localhost:5173'
const OUT  = new URL('./', import.meta.url).pathname

const shots = [
  { file: 'diagram-architecture.png', path: '/docs/architecture' },
  { file: 'diagram-vms.png',          path: '/docs/vms' },
  { file: 'diagram-supervision.png',  path: '/docs/supervision' },
  { file: 'diagram-capture.png',      path: '/docs/capture' },
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
    const diagram = await page.$('.docs-diagram')
    if (diagram) {
      await diagram.screenshot({ path: OUT + s.file })
    }
    await page.close()
  }
  await browser.close()
}
main().catch((e) => { console.error(e); process.exit(1) })
