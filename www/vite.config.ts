import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Local-only preview data so `npm run dev` shows the UI without the
// production orchid backend. Remove or set USE_MOCK=0 once a real /api/state
// is reachable (via SSH tunnel or local orch).
const mockState = () => ({
  inbox: 'denoland/orchid',
  operator: 'operator',
  vms: [{ name: 'paris', host: 'paris.local', capacity: 30, used: 6 }],
  jobs: [
    {
      issue: 184, vm: 'paris', tmux: 'claude-1',
      target: 'clawpatrol', target_repo: 'denoland/clawpatrol',
      branch: 'orch/divybot-184', issue_title: 'Tray icon flickers when state changes',
      lifecycle: 'oneshot', schedule: '', pr: 211, next_fire_at: '',
      last_check_conclusions: { build: 'FAILURE', test: 'SUCCESS' },
      activity: [0,0,2,5,4,3,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
    },
    {
      issue: 178, vm: 'paris', tmux: 'claude-2',
      target: 'clawpatrol', target_repo: 'denoland/clawpatrol',
      branch: 'orch/divybot-177', issue_title: 'Wire end-to-end capture pipeline',
      lifecycle: 'oneshot', schedule: '', pr: 178, next_fire_at: '',
      last_check_conclusions: { build: 'SUCCESS', test: 'SUCCESS' },
      activity: Array(28).fill(0),
    },
    {
      issue: 159, vm: 'paris', tmux: 'claude-3',
      target: 'orchid', target_repo: 'denoland/orchid',
      branch: 'orch/divybot-159', issue_title: 'Avoid remote-control race during worker bootstrap',
      lifecycle: 'oneshot', schedule: '', pr: 0, next_fire_at: '',
      last_check_conclusions: {},
      activity: Array(28).fill(0).map((_, i) => i > 18 ? Math.floor(Math.random()*8) : 0),
    },
    {
      issue: 160, vm: 'paris', tmux: 'claude-4',
      target: 'deno', target_repo: 'denoland/deno',
      branch: 'orch/divybot-160', issue_title: 'Add Node lazy-init for child_process',
      lifecycle: 'oneshot', schedule: '', pr: 0, next_fire_at: '',
      last_check_conclusions: {},
      activity: Array(28).fill(0).map(() => Math.floor(Math.random()*10)),
    },
    {
      issue: 0, vm: 'paris', tmux: 'claude-5',
      target: 'orchid', target_repo: 'denoland/orchid',
      branch: '', issue_title: 'Hourly health check',
      lifecycle: 'cron', schedule: '0 * * * *', pr: 0,
      next_fire_at: new Date(Date.now() + 14*60*1000).toISOString(),
      last_check_conclusions: {},
      activity: Array(28).fill(1),
    },
    {
      issue: 170, vm: 'paris', tmux: 'claude-6',
      target: 'clawpatrol', target_repo: 'denoland/clawpatrol',
      branch: 'orch/divybot-170', issue_title: 'Tiny PR — refactor only one helper',
      lifecycle: 'oneshot', schedule: '', pr: 215, next_fire_at: '',
      last_check_conclusions: { build: 'IN_PROGRESS' },
      activity: Array(28).fill(0).map((_, i) => i > 24 ? 1 : 0),
    },
  ],
})

const USE_MOCK = process.env.USE_MOCK !== '0'

export default defineConfig({
  plugins: [
    react(),
    USE_MOCK && {
      name: 'orchid-mock-api',
      configureServer(server: any) {
        server.middlewares.use('/api/state', (_req: any, res: any) => {
          res.setHeader('content-type', 'application/json')
          res.end(JSON.stringify(mockState()))
        })
        server.middlewares.use('/api/pane/stream', (_req: any, res: any) => {
          res.setHeader('content-type', 'text/event-stream')
          res.setHeader('cache-control', 'no-store')
          res.flushHeaders?.()
          let tick = 0
          const id = setInterval(() => {
            tick++
            const snap =
              '\x1b[36m$ \x1b[0morchid worker pane (mock SSE)\n' +
              '\x1b[2mtick ' + tick + ' — ' + new Date().toLocaleTimeString() + '\x1b[0m\n\n' +
              '> what should I do about this?\n'
            const enc = Buffer.from(snap).toString('base64')
            res.write('data: ' + enc + '\n\n')
          }, 1000)
          _req.on('close', () => clearInterval(id))
        })
        server.middlewares.use('/api/pane', (_req: any, res: any) => {
          // POST-only mock — keystrokes are swallowed.
          res.statusCode = 204
          res.end()
        })
        server.middlewares.use('/api/drafts', (req: any, res: any) => {
          let body = ''
          req.on('data', (c: any) => { body += c })
          req.on('end', () => {
            const fake = 200 + Math.floor(Math.random() * 50)
            res.setHeader('content-type', 'application/json')
            res.end(JSON.stringify({
              ok: true,
              id: 'mock-' + Date.now(),
              issue_url: `https://github.com/denoland/orchid/issues/${fake}`,
              asset_url: '',
            }))
          })
        })
      },
    },
  ].filter(Boolean) as any,
  build: {
    outDir: '../internal/orch/embed-dist',
    emptyOutDir: true,
    assetsDir: '_a',
    chunkSizeWarningLimit: 800,
    rollupOptions: {
      output: {
        // Pull the two heaviest deps (react-flow, xterm) into their own
        // chunks so the initial paint doesn't have to parse them.
        manualChunks: {
          'reactflow': ['@xyflow/react'],
          'xterm': ['@xterm/xterm', '@xterm/addon-fit'],
        },
      },
    },
  },
  server: {
    proxy: USE_MOCK ? undefined : {
      '/api': 'http://localhost:8000',
      '/ws': { target: 'ws://localhost:8000', ws: true },
    },
  },
})