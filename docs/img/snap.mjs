// Headless screenshot pipeline for docs.
// Run: cd www && npm run dev   (in another shell)
// Then: node docs/img/snap.mjs

import { chromium } from 'playwright'
import { mkdir } from 'node:fs/promises'

const BASE = process.env.SNAP_BASE ?? 'http://localhost:5174'
const OUT  = new URL('./', import.meta.url).pathname

const shots = [
  // [filename, path, options]
  { file: 'dashboard-list.png', path: '/', after: async (page) => {
      const list = await page.$('button[title="Switch to list view"]')
      if (list) await list.click()
      await page.waitForTimeout(600)
    },
  },
  { file: 'dashboard-list-dark.png', path: '/', after: async (page) => {
      const list = await page.$('button[title="Switch to list view"]')
      if (list) await list.click()
      const theme = await page.$('button[title="Theme"], button[aria-label="Theme"]')
      if (theme) await theme.click()
      await page.waitForTimeout(600)
    },
  },
  { file: 'docs.png',           path: '/docs' },
  { file: 'docs-page.png',      path: '/docs/getting-started' },
  { file: 'settings.png',       path: '/', after: async (page) => {
      const s = await page.$('button[title="Settings"]')
      if (s) { await s.click(); await page.waitForTimeout(700) }
    },
  },
  { file: 'landing.png',        url: 'https://orchid.littledivy.com/' },
]

async function main() {
  await mkdir(OUT, { recursive: true })
  const browser = await chromium.launch()
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 })
  // Block the events WebSocket so the dashboard falls back to /api/state.
  // Vite's HMR socket "succeeds" on upgrade and the SPA waits forever for
  // a state frame that never comes. Override WebSocket so any open() to
  // /api/events/ws immediately closes; the SPA falls back to fetchOnce.
  await ctx.addInitScript(() => {
    const _WS = window.WebSocket
    window.WebSocket = function (url, ...rest) {
      if (typeof url === 'string' && url.includes('/api/events/ws')) {
        // Fake a closed socket so onclose fires once handlers are attached.
        const fake = new EventTarget()
        Object.assign(fake, {
          url, readyState: 3,
          close() {}, send() {},
          onopen: null, onmessage: null, onclose: null, onerror: null,
        })
        setTimeout(() => {
          const ev = new Event('close')
          fake.onclose && fake.onclose(ev)
          fake.dispatchEvent(ev)
        }, 0)
        return fake
      }
      return new _WS(url, ...rest)
    }
    window.WebSocket.OPEN = 1
    window.WebSocket.CLOSED = 3
  })
  for (const s of shots) {
    const page = await ctx.newPage()
    const url = s.url ?? (BASE + s.path)
    console.log(`→ ${url}`)
    await page.goto(url, { waitUntil: 'networkidle' })
    await page.waitForTimeout(800)
    if (s.after) await s.after(page)
    await page.waitForTimeout(400)
    await page.screenshot({ path: OUT + s.file, fullPage: false })
    await page.close()
  }
  await browser.close()
  console.log('done.')
}

main().catch((e) => { console.error(e); process.exit(1) })
