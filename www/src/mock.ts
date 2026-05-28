// Fake sessions + VMs for previewing the dashboard at scale. Enabled
// by appending ?mock=<n> to the dashboard URL. Live state is still
// merged in — these are prepended so real jobs stay visible.
import type { Job, VM } from './types'

const REPOS = [
  'denoland/deno', 'denoland/std', 'denoland/deploy', 'denoland/fresh',
  'littledivy/littledivy', 'littledivy/orchid', 'littledivy/sashiko',
  'littledivy/clawpatrol', 'cloudflare/workers-sdk', 'oven-sh/bun',
]

const TITLES = [
  'Refactor stream backpressure handler',
  'Fix race in event loop on Windows',
  'Add JSR auto-publish on tag push',
  'TypeScript 5.6 perf regression',
  'Migrate to napi v3',
  'Fix flaky test on macOS arm64',
  'Bump deno_ast to 0.42',
  'Permissions prompt UX cleanup',
  'WASI ext_socket sockaddr_in6 panic',
  'http/2 server push regression',
  'Sourcemap mapping off-by-one',
  'Telemetry sampling oversample',
  'Disable v8 stack trace limit',
  'New `deno doc --json` flag',
  'Remove dead retry path in fetch',
  'Lockfile v5 migration',
  'Top-level await deadlock repro',
  'Worker termination memory leak',
  'Cargo workspace cleanup',
  'Add CI matrix for openbsd',
]

const BRANCHES = ['orch/101','orch/102','orch/103','orch/104','orch/105','orch/106','orch/107','orch/108']
const VMS = ['local', 'vm-1', 'vm-2', 'vm-3', 'vm-4', 'vm-5', 'vm-6']

function pick<T>(a: T[], i: number): T { return a[i % a.length] }
function rnd(seed: number): number {
  let t = seed + 0x6D2B79F5
  t = Math.imul(t ^ t >>> 15, t | 1)
  t ^= t + Math.imul(t ^ t >>> 7, t | 61)
  return ((t ^ t >>> 14) >>> 0) / 4294967296
}

export function mockJobs(n: number): Job[] {
  const out: Job[] = []
  for (let i = 0; i < n; i++) {
    const repo = pick(REPOS, i * 7)
    const title = pick(TITLES, i * 11)
    const branch = pick(BRANCHES, i)
    const vm = pick(VMS, i * 3)
    const r = rnd(i + 1)
    let needs_input = false
    let conclusions: Record<string, string> = {}
    let pr = 0
    let activity: number[] = []
    if (r < 0.12) {
      needs_input = true
      pr = i + 9000
      activity = bump(i, 30, false)
    } else if (r < 0.22) {
      pr = i + 9000
      conclusions = { build: 'failure', test: 'failure' }
      activity = bump(i, 30, true)
    } else if (r < 0.37) {
      pr = i + 9000
      conclusions = { build: 'success', test: 'success' }
      activity = bump(i, 30, true)
    } else if (r < 0.67) {
      activity = bump(i, 30, false)
    } else {
      activity = bump(i, 30, false).map((v, k) => (k < 18 ? v : 0))
    }
    const isCodex = rnd(i * 41) < 0.2
    const tmux = `${isCodex ? 'codex' : 'claude'}-mock-${i}-${vm}`
    const cost = 0.05 + rnd(i * 17) * 4.5
    out.push({
      issue: 80000 + i,
      vm,
      tmux,
      target: repo.split('/')[1],
      target_repo: repo,
      branch,
      issue_title: title,
      lifecycle: i % 11 === 0 ? 'cron' : 'oneshot',
      schedule: '',
      pr,
      next_fire_at: '',
      last_check_conclusions: conclusions,
      activity,
      needs_input,
      vm_online: true,
      usage: {
        model: isCodex ? 'gpt-5-codex' : 'claude-sonnet-4',
        cost_usd: Math.round(cost * 100) / 100,
        context_pct: Math.min(95, Math.round(rnd(i * 23) * 100)),
      },
    } as Job)
  }
  return out
}

function bump(seed: number, len: number, busy: boolean): number[] {
  const a = new Array(len).fill(0)
  for (let k = 0; k < len; k++) {
    const x = rnd(seed * 31 + k)
    a[k] = busy
      ? (x < 0.7 ? Math.floor(x * 12) : 0)
      : (x < 0.35 ? Math.floor(x * 6) : 0)
  }
  return a
}

export function mockVMs(): VM[] {
  return VMS.map((name, i) => ({
    name,
    host: i === 0 ? 'localhost' : `10.42.3.${100 + i}`,
    capacity: 16,
    used: 0,
    bot: 'littledivy',
    agent: i % 5 === 0 ? 'codex' : 'claude',
    online: i !== 5,
    last_err: i === 5 ? 'ssh: connect to host: connection refused' : undefined,
  }))
}
