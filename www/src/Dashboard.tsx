
import React, { useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { marked } from 'marked'
import type { Job, State, AgentMeter, VM } from './types'
import { attention, ciStatus, LEVEL_COLOR, type AttentionLevel } from './attention'
import { Pane } from './Pane'
import { Composer } from './Composer'
import { mockJobs, mockVMs } from './mock'
import { OrchidArt } from './OrchidArt'
import { AgentLogo } from './AgentLogo'
import { OSIcon } from './OSIcon'

import type { RelayInfo } from './App'
import { WSBusContext } from './App'

interface Props { state: State; relay: RelayInfo | null }

// Realtime activity bus. The relay's events WS pushes {type:'activity',
// tmux, ts} messages whenever a session's pane changes — these arrive
// faster than the /api/state poll. List rows subscribe via ActivityContext
// and override `attention()` to 'working' for the brief window after a
// push so the dashboard reacts immediately instead of waiting for the
// next poll.
const ActivityContext = React.createContext<{ at: Map<string, number>; tick: number }>({
  at: new Map(), tick: 0,
})
const ACTIVITY_HOLD_MS = 4000

function PRBadge({ repo, pr, ci }: { repo: string; pr: number; ci: 'fail' | 'pass' | 'pending' }) {
  const variant: 'open' | 'closed' | 'pending' =
    ci === 'fail' ? 'closed' : ci === 'pass' ? 'open' : 'pending'
  const color =
    variant === 'closed' ? 'text-rose-400 bg-rose-500/15 ring-rose-500/30'
    : variant === 'open' ? 'text-emerald-400 bg-emerald-500/15 ring-emerald-500/30'
    : 'text-amber-400 bg-amber-500/15 ring-amber-500/30'
  return (
    <a
      href={`https://github.com/${repo}/pull/${pr}`}
      target="_blank"
      rel="noopener noreferrer"
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      className={`mono inline-flex items-center gap-1 text-[11px] px-1.5 py-0.5 rounded ring-1 ring-inset ${color}`}
      title={`PR #${pr} · ${variant}`}
    >
      <PRIcon variant={variant} />
      #{pr}
    </a>
  )
}

function PRIcon({ variant }: { variant: 'open' | 'closed' | 'pending' }) {
  if (variant === 'closed') {
    return (
      <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
        <path d="M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 14a2.25 2.25 0 1 1 0-4.5 2.25 2.25 0 0 1 0 4.5ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm-2.03-9.53a.75.75 0 0 1 1.06 0L13 3.69l1.22-1.22a.75.75 0 1 1 1.06 1.06L14.06 4.75l1.22 1.22a.75.75 0 1 1-1.06 1.06L13 5.81l-1.22 1.22a.75.75 0 1 1-1.06-1.06l1.22-1.22-1.22-1.22a.75.75 0 0 1 0-1.06Z"/>
      </svg>
    )
  }
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
      <path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z"/>
    </svg>
  )
}

// ─── dashboard ────────────────────────────────────────────────────────

export function Dashboard({ state, relay }: Props) {
  return <DashboardInner state={state} relay={relay} />
}

// ?mock=<n> in the URL prepends N fake sessions to the live jobs list
// so the views can be previewed at scale without spawning real work.
const MOCK_COUNT = (() => {
  try {
    const n = parseInt(new URLSearchParams(location.search).get('mock') ?? '', 10)
    return Number.isFinite(n) && n > 0 ? Math.min(500, n) : 0
  } catch { return 0 }
})()

function DashboardInner({ state: rawState, relay }: Props) {
  const liveJobs = rawState.jobs ?? []
  const jobs = useMemo(
    () => MOCK_COUNT > 0 ? [...mockJobs(MOCK_COUNT), ...liveJobs] : liveJobs,
    [liveJobs],
  )
  const state = useMemo(() => {
    if (MOCK_COUNT === 0) return rawState
    return { ...rawState, jobs, vms: [...mockVMs(), ...(rawState.vms ?? [])] }
  }, [rawState, jobs])
  const inbox = state.inbox ?? ''
  const jobsByTmuxRef = useRef<Map<string, Job>>(new Map())
  // Shared events-WS bus from App.tsx — see WSBusContext. Used here for
  // inbound `activity` pushes that beat the slower /api/state poll.
  const bus = useContext(WSBusContext)

  const [tab, setTab] = useState<Tab>('sessions')
  const [q, setQ] = useState('') // search, lives in the nav (GitHub-style)
  const openSettings = useCallback((s: SectionId) => setTab(sectionToTab(s)), [])
  const [showCapture, setShowCapture] = useState(false)
  const [showStats, setShowStats] = useState(false) // mobile: usage sidebar drawer
  // Clicking a list row opens this session in a modal pane.
  const [listExpanded, setListExpanded] = useState<string | null>(null)
  // Last-seen activity timestamp per tmux. Updated from the events WS;
  // ListRow reads via ActivityContext for sub-poll-latency indicators.
  const activityAtRef = useRef<Map<string, number>>(new Map())
  const [activityTick, setActivityTick] = useState(0)
  const activityCtx = useMemo(
    () => ({ at: activityAtRef.current, tick: activityTick }),
    [activityTick],
  )

  // Keep a jobs lookup synced for the pane modal that reads it lazily.
  useEffect(() => {
    jobsByTmuxRef.current = new Map(jobs.filter((j) => j.tmux).map((j) => [j.tmux, j]))
  }, [jobs])

  // Subscribe to the events WS for realtime `activity` pings so a session
  // flips to "working" immediately instead of waiting for the next poll.
  useEffect(() => {
    if (!bus) return
    return bus.subscribe((msg: any) => {
      if (msg?.type !== 'activity') return
      const tmux = msg.tmux as string
      if (!tmux) return
      activityAtRef.current.set(tmux, Date.now())
      setActivityTick((t) => t + 1)
    })
  }, [bus])

  return (
    <ActivityContext.Provider value={activityCtx}>
    <div className="min-h-screen w-full overflow-x-hidden flex flex-col bg-zinc-50 dark:bg-zinc-950">
      <TopBar
        count={jobs.filter((j) => !j.closed_state).length}
        vmCount={new Set((state.vms ?? []).map((v) => v.host)).size}
        tab={tab}
        setTab={setTab}
        q={q}
        setQ={setQ}
        onOpenCapture={() => setShowCapture(true)}
        onToggleStats={() => setShowStats((v) => !v)}
      />
      <div className="flex items-start">
        {tab === 'memory' ? (
          <div className="relative flex-1 min-w-0 min-h-[calc(100vh-93px)] bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur">
            <MemoryPage />
          </div>
        ) : tab === 'analytics' ? (
          <div className="relative flex-1 min-w-0 min-h-[calc(100vh-93px)] bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur">
            <AnalyticsPage state={state} />
          </div>
        ) : tab === 'machines' ? (
          <div className="relative flex-1 min-w-0 min-h-[calc(100vh-93px)] bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur">
            <MachinesPage state={state} />
          </div>
        ) : tab !== 'sessions' ? (
          <div className="relative flex-1 min-w-0 min-h-[calc(100vh-93px)]">
            <SettingsPage key={tab} jobs={jobs} state={state} relay={relay} initialSection={TAB_SECTION[tab as Exclude<Tab, 'sessions' | 'memory' | 'analytics' | 'machines'>]} onClose={() => setTab('sessions')} />
          </div>
        ) : relay && !relay.connected && relay.login && relay.token ? (
          // No machine has dialed in yet → the whole Sessions view is the setup
          // page (GitHub new-repo style), not a banner over an empty list.
          <div className="relative flex-1 min-w-0 min-h-[calc(100vh-93px)] bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur">
            <FirstJoinSetup relay={relay} />
          </div>
        ) : (
          <>
            <div className="flex-1 min-w-0 flex flex-col">
              <WarningStack
                stateLoaded={state.inbox !== undefined}
                inbox={state.inbox}
                openSettings={openSettings}
              />
              {/* full-bleed: no padding, no radius; bordered container, sidebar bg. */}
              <div className="border border-zinc-200 dark:border-zinc-800 bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur">
                <ListView jobs={jobs} q={q} onOpen={(t) => setListExpanded(t)} />
              </div>
            </div>
            <Sidebar state={state} open={showStats} onClose={() => setShowStats(false)} />
          </>
        )}
      </div>
      {showCapture && <CapturePage jobs={jobs} inbox={inbox} onClose={() => setShowCapture(false)} />}
      {listExpanded && (
        <PaneModal tmux={listExpanded} jobsByTmuxRef={jobsByTmuxRef} onClose={() => setListExpanded(null)} />
      )}
    </div>
    </ActivityContext.Provider>
  )
}

/// Right-hand telemetry sidebar: the per-agent usage + governor strips (claude,
/// codex, …) given room to breathe, a live VM list, and the orchid line-art
/// anchored at the bottom edge. The strips are the same QuotaStrip the navbar
/// used — just stacked vertically as the page's persistent ops column. Hidden
/// below lg so the canvas keeps the full width on narrow screens.
function Sidebar({ state, open, onClose }: { state: State; open: boolean; onClose: () => void }) {
  const agents = state.agents ?? {}
  // claude first, then any other agents (codex…), only those with a reading.
  const order = ['claude', ...Object.keys(agents).filter((a) => a !== 'claude')]
  const present = order.filter((a) => agents[a]?.quota || agents[a]?.governor)
  return (
    <>
      {/* mobile backdrop when the drawer is open */}
      <div
        className={'fixed inset-0 z-40 bg-black/40 lg:hidden transition-opacity ' + (open ? 'opacity-100' : 'opacity-0 pointer-events-none')}
        onClick={onClose}
        aria-hidden
      />
      <aside
        className={
          'flex flex-col w-[86vw] max-w-sm h-full overflow-hidden border-l border-zinc-200 dark:border-zinc-800 bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur ' +
          // mobile: off-canvas drawer (fixed, full height). desktop: a full-height
          // column (bg + border run all the way down via self-stretch); a sticky
          // inner keeps the telemetry + art in view while the page scrolls.
          'fixed inset-y-0 right-0 z-50 shadow-2xl transition-transform lg:static lg:self-stretch lg:overflow-visible lg:z-0 lg:w-80 lg:max-w-none lg:shadow-none lg:translate-x-0 lg:flex-shrink-0 ' +
          (open ? 'translate-x-0' : 'translate-x-full lg:translate-x-0')
        }
      >
      <div className="relative flex flex-col h-full overflow-hidden lg:sticky lg:top-[93px] lg:h-[calc(100vh-93px)]">
      <button
        className="lg:hidden absolute top-3 right-3 z-20 w-7 h-7 flex items-center justify-center rounded-md text-zinc-500 hover:bg-zinc-200/60 dark:hover:bg-zinc-700/60"
        onClick={onClose}
        aria-label="Close usage panel"
      >
        ✕
      </button>
      <div className="flex-1 overflow-y-auto px-4 py-5 flex flex-col gap-5 z-10">
        <h2 className="mono text-[10px] uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">
          Usage &amp; pacing
        </h2>
        {present.length === 0 && (
          <div className="text-[12px] text-zinc-500 dark:text-zinc-400">No quota reading yet…</div>
        )}
        {present.map((a) => {
          const m = agents[a]!
          return m.quota ? (
            <QuotaStrip key={a} quota={m.quota} governor={m.governor} label={a} stacked />
          ) : null
        })}

        <div className="border-t border-zinc-200 dark:border-zinc-800 pt-4 flex flex-col gap-2">
          <h2 className="mono text-[10px] uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">
            Machines
          </h2>
          {[...new Set(state.vms.map((v) => v.host))].map((host) => {
            const slots = state.vms.filter((v) => v.host === host)
            const off = slots.every((v) => v.online === false)
            const u = slots.reduce((a, v) => a + (v.used ?? 0), 0)
            const c = slots.reduce((a, v) => a + (v.capacity ?? 0), 0)
            return (
              <div key={host} className="flex items-center gap-2 text-[12px]">
                <span
                  className={'w-1.5 h-1.5 rounded-full flex-shrink-0 ' + (off ? 'bg-zinc-400 dark:bg-zinc-600' : 'bg-emerald-500')}
                  title={off ? 'offline' : 'online'}
                />
                <span className="mono text-zinc-700 dark:text-zinc-300 flex-1 truncate">{host}</span>
                <span className="mono text-[10px] text-zinc-400">{slots.length} slots</span>
                <span className="mono text-zinc-500 dark:text-zinc-400 tabular-nums">{u}/{c || '∞'}</span>
              </div>
            )
          })}
        </div>
      </div>
      <OrchidArt
        posClassName="absolute right-0 bottom-0 z-0"
        width="460px"
        height="460px"
        opacity={0.14}
        bleed={0}
        maskStart={30}
        maskEnd={92}
        transform="translate(22%, 22%)"
      />
      </div>
      </aside>
    </>
  )
}

function TopBar({
  count, vmCount, tab, setTab, q, setQ, onOpenCapture, onToggleStats,
}: {
  count: number
  vmCount: number
  tab: Tab
  setTab: (t: Tab) => void
  q: string
  setQ: (s: string) => void
  onOpenCapture: () => void
  onToggleStats: () => void
}) {
  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: 'sessions', label: 'Sessions', count },
    { id: 'machines', label: 'Machines', count: vmCount },
    { id: 'analytics', label: 'Analytics' },
    { id: 'memory', label: 'Memory' },
    { id: 'settings', label: 'Settings' },
  ]
  return (
    <div className="flex-shrink-0 sticky top-0 z-40 border-b border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-950">
      {/* Row 1: logo · search (GitHub-style) · actions */}
      <div className="flex items-center gap-4 h-12 px-4 sm:px-6">
        <div className="flex items-center gap-1.5 flex-shrink-0">
          <img src="/favicon.svg" alt="Orchid" className="w-6 h-6 flex-shrink-0" />
          <span className="text-[18px] font-semibold tracking-tight text-zinc-800 dark:text-zinc-100 hidden sm:block">Orchid</span>
        </div>
        <div className="hidden sm:block flex-1" />
        <div className="relative flex-1 max-w-none sm:max-w-md">
          <svg className="absolute left-2.5 top-1/2 -translate-y-1/2 text-zinc-400" width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round">
            <circle cx="11" cy="11" r="7" /><line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search title, repo, #issue…"
            className="w-full pl-9 sm:pl-8 pr-2 py-2.5 sm:py-1.5 text-[16px] sm:text-[12.5px] rounded-lg sm:rounded-md bg-zinc-100/70 dark:bg-zinc-800/60 border border-transparent focus:border-zinc-300 dark:focus:border-zinc-600 focus:bg-white dark:focus:bg-zinc-900 outline-none text-zinc-800 dark:text-zinc-200 placeholder:text-zinc-400"
          />
        </div>
        <button
          className="lg:hidden w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
          onClick={onToggleStats}
          aria-label="Usage & pacing"
          title="Usage & pacing"
        >
          <svg width={15} height={15} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.9} strokeLinecap="round" strokeLinejoin="round">
            <line x1="6" y1="20" x2="6" y2="12" /><line x1="12" y1="20" x2="12" y2="4" /><line x1="18" y1="20" x2="18" y2="14" />
          </svg>
        </button>
        <HeaderBtnBar>
          <CaptureButton onClick={onOpenCapture} />
          <ThemeToggle />
          <LogoutButton />
        </HeaderBtnBar>
      </div>
      {/* Row 2: tabs (underline, GitHub-style) */}
      <nav className="flex items-stretch h-11 px-2 sm:px-4 -mb-px">
        {tabs.map((t) => {
          const on = tab === t.id
          return (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={
                'relative h-full px-3.5 flex items-center gap-2 text-[14px] transition-colors ' +
                (on ? 'text-zinc-900 dark:text-zinc-50 font-semibold' : 'text-zinc-600 dark:text-zinc-300 hover:text-zinc-900 dark:hover:text-zinc-50')
              }
            >
              {t.label}
              {t.count != null && <span className="mono text-[11px] px-1.5 py-0.5 rounded-full bg-zinc-200/80 dark:bg-zinc-700/70 text-zinc-600 dark:text-zinc-300 tabular-nums">{t.count}</span>}
              {on && <span className="absolute -bottom-px inset-x-1.5 h-0.5 rounded-full bg-zinc-900 dark:bg-zinc-100" />}
            </button>
          )
        })}
      </nav>
    </div>
  )
}

/// Compact two-bar quota readout sourced from Claude Code's
/// statusline.jsonl feed. Bar widths track used_percentage; the
/// trailing label is the time to reset (4h12m / 2d3h). When usage
/// outpaces elapsed-time we tint amber to flag burn faster than
/// sustainable. Hidden entirely until the agent has reported once.
///
/// When the daemon's weekly throttle is active (quota.throttle), the 7d
/// bar gets a pace-target marker (where usage *should* be by now) and
/// the strip drives its coloring + a status chip off the authoritative
/// server mode rather than the local elapsed-time heuristic.
function QuotaStrip({ quota, governor, label, stacked }: { quota: NonNullable<State['quota']>; governor?: State['governor']; label?: string; stacked?: boolean }) {
  const now = Math.floor(Date.now() / 1000)
  const thr = quota.throttle
  const fmt = (secs: number) => {
    if (secs <= 0) return 'now'
    const h = Math.floor(secs / 3600)
    const m = Math.floor((secs % 3600) / 60)
    const d = Math.floor(h / 24)
    if (d > 0) return `${d}d${h % 24}h`
    if (h > 0) return `${h}h${m}m`
    return `${m}m`
  }
  // `hot` accent: amber = brake engaged / over pace, red = hard pause.
  type Hot = false | 'amber' | 'red'
  const bar = (label: string, pct: number, resets: number, window: number, opts?: { hot?: Hot; targetPct?: number }) => {
    const elapsedPct = Math.min(100, Math.max(0, (1 - Math.max(0, resets - now) / window) * 100))
    // Authoritative server signal wins; fall back to the local elapsed
    // heuristic for older daemons that don't report a throttle decision.
    const hot: Hot = opts?.hot !== undefined ? opts.hot : pct > elapsedPct + 5 ? 'amber' : false
    // Prefer the server-computed pace target; fall back to the local guess.
    const targetPct = opts?.targetPct !== undefined ? opts.targetPct : undefined
    const trackColor =
      hot === 'red'
        ? 'bg-rose-200/60 dark:bg-rose-900/40'
        : hot === 'amber'
          ? 'bg-amber-200/60 dark:bg-amber-900/40'
          : 'bg-zinc-200 dark:bg-zinc-800'
    const fillColor =
      hot === 'red' ? 'bg-rose-500' : hot === 'amber' ? 'bg-amber-500' : 'bg-emerald-500/80 dark:bg-emerald-400/80'
    return (
      <div className="flex items-center gap-1.5">
        <span className="mono text-[10px] text-zinc-500 dark:text-zinc-400 w-[18px]">{label}</span>
        <div className={'relative h-1.5 w-20 rounded-full overflow-hidden ' + trackColor}>
          <div className={'absolute inset-y-0 left-0 ' + fillColor} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
          {targetPct !== undefined && (
            <div
              className="absolute inset-y-0 w-px bg-zinc-500/70 dark:bg-zinc-300/70"
              style={{ left: `${Math.min(100, Math.max(0, targetPct))}%` }}
              title={`pace target ${Math.round(targetPct)}%`}
            />
          )}
        </div>
        <span className="mono text-[10px] text-zinc-500 dark:text-zinc-400 tabular-nums">{Math.round(pct)}%</span>
        <span className="mono text-[10px] text-zinc-500 dark:text-zinc-400">{fmt(resets - now)}</span>
      </div>
    )
  }
  // Map the server throttle mode onto the 7d bar accent + status chip.
  const sevenHot: Hot | undefined = thr
    ? thr.mode === 'pause_5h' || thr.mode === 'pause_week'
      ? 'red'
      : thr.mode === 'throttle'
        ? 'amber'
        : false
    : undefined
  let chip: { text: string; cls: string } | null = null
  if (thr && thr.mode !== 'allow') {
    if (thr.mode === 'throttle') {
      chip = { text: 'pacing', cls: 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300' }
    } else if (thr.mode === 'pause_5h') {
      const u = thr.until ? ` · ${fmt(thr.until - now)}` : ''
      chip = { text: `paused 5h${u}`, cls: 'bg-rose-100 text-rose-700 dark:bg-rose-900/40 dark:text-rose-300' }
    } else if (thr.mode === 'pause_week') {
      const u = thr.until ? ` · resets ${fmt(thr.until - now)}` : ''
      chip = { text: `paused${u}`, cls: 'bg-rose-100 text-rose-700 dark:bg-rose-900/40 dark:text-rose-300' }
    }
  }
  let title =
    (label ? `${label} usage` : 'Claude subscription usage') +
    ': 5-hour session window and 7-day cap. Amber = burning faster than elapsed time would sustain.'
  if (quota.plan_type) title += `\nplan: ${quota.plan_type}`
  if (quota.credits != null) title += `\ncredits: $${quota.credits.toFixed(2)}`
  if (thr) {
    if (thr.reason) title += `\n${thr.reason}`
    if (thr.projected_exhaust_at) title += `\nexhausts in ${fmt(thr.projected_exhaust_at - now)} at current burn`
  }
  const bars = (
    <>
      {bar('5h', quota.five_hour_pct, quota.five_hour_resets_at, 5 * 3600)}
      {bar('7d', quota.seven_day_pct, quota.seven_day_resets_at, 7 * 24 * 3600, {
        hot: sevenHot,
        targetPct: thr?.target_pct,
      })}
    </>
  )
  const chipEl = chip && (
    <span className={'mono text-[10px] px-1.5 py-0.5 rounded-full whitespace-nowrap ' + chip.cls}>{chip.text}</span>
  )
  const govEl = governor?.enabled && <GovernorStrip gov={governor} />

  if (stacked) {
    // Two columns: logo + label + bars on the left, the weekly % as a big
    // glance number filling the right-center free area (amber when over-pace).
    return (
      <div
        className="pointer-events-auto w-full flex items-center gap-3 bg-white/70 dark:bg-zinc-900/70 ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-lg px-3 py-3"
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => e.stopPropagation()}
        title={title}
      >
        <div className="flex-1 min-w-0 flex flex-col gap-2">
          <div className="flex items-center gap-1.5">
            <AgentLogo account={label ?? 'claude'} size={15} className="text-zinc-700 dark:text-zinc-200 flex-shrink-0" />
            <span className="mono text-[12px] text-zinc-600 dark:text-zinc-300 truncate">{label}</span>
            {quota.plan_type && (
              <span className="mono text-[9px] px-1 rounded bg-zinc-100 dark:bg-zinc-800 text-zinc-500 dark:text-zinc-400 whitespace-nowrap">{quota.plan_type}</span>
            )}
          </div>
          <div className="flex flex-col gap-1.5">{bars}</div>
          {(chipEl || govEl) && <div className="flex flex-wrap items-center gap-2">{chipEl}{govEl}</div>}
        </div>
        <div className="flex flex-col items-center justify-center flex-shrink-0 leading-none px-1">
          <span className={'mono font-semibold tabular-nums tracking-tight text-[32px] ' + (sevenHot === 'red' ? 'text-rose-500' : sevenHot ? 'text-amber-500' : 'text-zinc-900 dark:text-zinc-100')}>
            {Math.round(quota.seven_day_pct)}
            <span className="text-[14px] font-normal text-zinc-500 dark:text-zinc-400">%</span>
          </span>
          <span className="mono text-[9px] uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400 mt-1">7d</span>
        </div>
      </div>
    )
  }

  return (
    <div
      className="pointer-events-auto ml-3 flex items-center gap-3 bg-white/80 dark:bg-zinc-900/80 backdrop-blur ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-md px-2.5 py-1"
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      title={title}
    >
      {label && (
        <span className="mono text-[10px] px-1.5 py-0.5 rounded-full bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300 whitespace-nowrap">
          {label}
        </span>
      )}
      {bars}
      {chipEl}
      {govEl}
    </div>
  )
}

/// Proactive pacing governor telemetry: the adaptive admission cap with live
/// active/paused counts, the measured-vs-target burn rate of the binding bucket,
/// and the projected end-of-week usage against the 92% ceiling. Amber when over
/// pace (burn > target). Only rendered when the daemon reports governor.enabled.
function GovernorStrip({ gov }: { gov: NonNullable<State['governor']> }) {
  // The binding bucket drives the burn-vs-target readout; default to weekly.
  const onFive = gov.binding === '5h'
  const burn = onFive ? gov.burn_five : gov.burn_weekly
  const target = onFive ? gov.target_five : gov.target_weekly
  const over = burn > target + 0.05 // small deadband to avoid flicker at parity
  const capLabel = gov.effective_cap < 0 ? '∞' : String(gov.effective_cap)
  const burnCls = over
    ? 'text-amber-700 dark:text-amber-300'
    : 'text-zinc-500 dark:text-zinc-400'
  const projOver = gov.projected_end_pct > 92
  const title =
    `Pacing governor: adaptive concurrency cap + duty-cycle to land at ~92% by reset.\n` +
    `cap ${capLabel}, active ${gov.active}, paused ${gov.paused}\n` +
    `burn ${burn.toFixed(1)}%/h ${over ? '>' : '≤'} target ${target.toFixed(1)}%/h (binding ${gov.binding || 'weekly'})\n` +
    `projected end-of-week ${gov.projected_end_pct.toFixed(0)}% vs 92% ceiling`
  return (
    <div className="flex items-center gap-1.5" title={title}>
      <span className="mono text-[10px] text-zinc-600 dark:text-zinc-300 tabular-nums">
        cap {capLabel}
        <span className="text-zinc-500 dark:text-zinc-400">
          {' '}({gov.active}
          {gov.paused > 0 && <span className="text-sky-600 dark:text-sky-400">/{gov.paused}❄</span>})
        </span>
      </span>
      <span className={'mono text-[10px] tabular-nums ' + burnCls}>
        {burn.toFixed(1)}{over ? '>' : '≤'}{target.toFixed(1)}%/h
      </span>
      <span
        className={
          'mono text-[10px] tabular-nums ' +
          (projOver ? 'text-amber-700 dark:text-amber-300' : 'text-zinc-500 dark:text-zinc-400')
        }
      >
        →{gov.projected_end_pct.toFixed(0)}%
      </span>
    </div>
  )
}

/// Pill container around the top-right action buttons. Matches the
/// FloatingToolbar's chrome (white/zinc-900 backdrop, ring, shadow).
function HeaderBtnBar({ children }: { children: React.ReactNode }) {
  // Plain inline group — the buttons live directly in the navbar, no pill chrome.
  return <div className="flex items-center gap-0.5">{children}</div>
}

function CaptureButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title="Capture — file a new issue"
      className="w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
    >
      {/* feather-style paper-plane: "send a thought into the inbox" */}
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <line x1="22" y1="2" x2="11" y2="13" />
        <polygon points="22 2 15 22 11 13 2 9 22 2" />
      </svg>
    </button>
  )
}

function CapturePage({ jobs, inbox, onClose }: { jobs: Job[]; inbox: string; onClose: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  // Most-recent inbox-driven jobs. Captures land here once orch picks
  // them up and labels them; until then the user sees their own
  // composed item bubble to the top on the next /api/state poll.
  const recent = useMemo(
    () => [...jobs].sort((a, b) => b.issue - a.issue).slice(0, 12),
    [jobs],
  )

  return (
    <div
      className="fixed inset-0 z-40 grid place-items-center bg-zinc-900/40 dark:bg-black/60 backdrop-blur-sm p-4 sm:p-8"
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="bg-white dark:bg-zinc-950 rounded-2xl ring-1 ring-zinc-200 dark:ring-zinc-700 shadow-2xl w-full max-w-[680px] max-h-[80vh] flex flex-col"
      >
        <div className="px-6 h-12 flex items-center gap-3 border-b border-zinc-200 dark:border-zinc-800 flex-shrink-0">
          <span className="serif italic text-[20px] text-zinc-900 dark:text-zinc-100">Capture</span>
          <span className="mono text-[12px] text-zinc-500 dark:text-zinc-400">spawn an idea</span>
          <div className="flex-1" />
          <button
            onClick={onClose}
            className="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200 rounded p-1"
            title="Close (esc)"
          >
            <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2}>
              <line x1="18" y1="6" x2="6" y2="18" />
              <line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </div>

        <div className="flex-1 min-h-0 overflow-auto">
          <div className="px-6 py-6 space-y-8">
            <Composer autoFocus onSent={() => onClose()} onCancel={onClose} />

            <div>
              <div className="serif italic text-[16px] text-zinc-900 dark:text-zinc-100 mb-2">Recent</div>
              {recent.length === 0 && (
                <p className="text-[13px] text-zinc-500 dark:text-zinc-400">
                  Nothing here yet. Type above to file your first capture.
                </p>
              )}
              <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
                {recent.map((j) => (
                  <RecentCaptureRow key={j.issue} job={j} inbox={inbox} />
                ))}
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function RecentCaptureRow({ job, inbox }: { job: Job; inbox: string }) {
  const attn = attention(job)
  const color = LEVEL_COLOR[attn.level]
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const issueURL = inbox ? `https://github.com/${inbox}/issues/${job.issue}` : `#${job.issue}`
  return (
    <a
      href={issueURL}
      target="_blank"
      rel="noopener noreferrer"
      className="group flex items-center gap-4 px-1 py-3 hover:bg-zinc-50/80 dark:hover:bg-zinc-900/40 transition-colors"
    >
      <span className={`w-2 h-2 rounded-full ${color.bar} flex-shrink-0`} />
      <div className="flex-1 min-w-0">
        <div className="text-[14px] text-zinc-900 dark:text-zinc-100 truncate">
          {job.issue_title || job.tmux || `#${job.issue}`}
        </div>
        <div className="mt-0.5 mono text-[11px] text-zinc-500 dark:text-zinc-400 truncate">
          #{job.issue} · {repo}{job.pr ? ` · PR #${job.pr}` : ''}
        </div>
      </div>
      <span className="text-zinc-300 dark:text-zinc-600 opacity-0 group-hover:opacity-100 transition-opacity">
        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
          <line x1="7" y1="17" x2="17" y2="7" />
          <polyline points="7 7 17 7 17 17" />
        </svg>
      </span>
    </a>
  )
}

function DocsButton() {
  return (
    <a
      href="/docs"
      title="Docs"
      className="w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20" />
        <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z" />
      </svg>
    </a>
  )
}

// Stacked top-center warnings. Lives above the header (z-50) so the
// capture composer doesn't sit on top of it. Each row is dismissed by
// fixing the underlying config (GitHub auth, target repos, etc), not
// by a close button — keeps the dashboard honest about whether orchid
// can actually do anything.
function WarningStack({
  stateLoaded, inbox, openSettings,
}: {
  stateLoaded: boolean
  inbox?: string
  openSettings: (section: SectionId) => void
}) {
  const [targetsOK, setTargetsOK] = useState<boolean | null>(null)
  const [dismissed, setDismissed] = useState(false)
  useEffect(() => {
    let alive = true
    fetch('/api/config', { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : null)
      .then((j) => { if (alive) setTargetsOK(Array.isArray(j?.targets) && j.targets.length > 0) })
      .catch(() => { if (alive) setTargetsOK(true) /* don't nag if probe fails */ })
    return () => { alive = false }
  }, [])
  useEffect(() => {
    if (dismissed) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setDismissed(true) }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [dismissed])

  const rows: { label: string; cta: string; onClick: () => void }[] = []
  if (targetsOK === false) {
    rows.push({
      label: 'No targets configured — issues won’t match any repo.',
      cta: 'Add target',
      onClick: () => openSettings('targets'),
    })
  }
  if (!inbox) {
    rows.push({
      label: 'Inbox repo not set — orchid has nothing to poll.',
      cta: 'Set inbox',
      onClick: () => openSettings('orch'),
    })
  }
  // Wait until both /api/state and /api/config have come back at least
  // once. Otherwise the initial empty-stub state flashes a "GitHub not
  // connected" warning before the real data lands.
  if (!stateLoaded || targetsOK === null) return null
  if (rows.length === 0 || dismissed) return null

  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-zinc-900/40 dark:bg-black/60 backdrop-blur-sm p-6"
         onClick={() => setDismissed(true)}>
      <div
        onClick={(e) => e.stopPropagation()}
        className="bg-white dark:bg-zinc-900 rounded-2xl ring-1 ring-zinc-200 dark:ring-zinc-700 shadow-2xl p-7 max-w-md w-full"
      >
        <div className="flex items-center gap-2 mb-4">
          <span className="w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
          <h2 className="serif italic text-[20px] text-zinc-900 dark:text-zinc-50">Heads up</h2>
          <div className="flex-1" />
          <button
            onClick={() => setDismissed(true)}
            className="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200 rounded p-1"
            title="Close (esc)"
          >
            <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2}>
              <line x1="18" y1="6" x2="6" y2="18" />
              <line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </div>
        <div className="space-y-2.5">
          {rows.map((r) => (
            <div key={r.label} className="flex items-center gap-3 rounded-lg ring-1 ring-amber-200 dark:ring-amber-800/60 bg-amber-50/70 dark:bg-amber-950/30 px-3 py-2">
              <span className="text-[13px] text-amber-900 dark:text-amber-100 flex-1">{r.label}</span>
              <button
                onClick={() => { r.onClick(); setDismissed(true) }}
                className="mono text-[11px] px-2 py-0.5 rounded ring-1 ring-amber-300 dark:ring-amber-700 text-amber-800 dark:text-amber-200 hover:bg-amber-100 dark:hover:bg-amber-900/50 flex-shrink-0"
              >{r.cta}</button>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

function SettingsButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title="Settings"
      className="w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1 1.55V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 9 19.4a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.6 15a1.7 1.7 0 0 0-1.55-1H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.6 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1-1.55V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1 1.55 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87V9c.21.51.7.92 1.55 1H21a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.55 1Z" />
      </svg>
    </button>
  )
}

interface CaptureCfg {
  auth_token?: string
  assets_dir?: string
  public_url?: string
}
interface OrchestratorCfg {
  poll_interval?: string
  state_db?: string
  branch_prefix?: string
  workdir_root?: string
  http_addr?: string
  http_secret?: string
  bot_login?: string
  bot_email?: string
  ntfy_topic?: string
  allowed_logins?: string[]
  capture?: CaptureCfg
}
type GhCfg = { inbox_repo?: string }
interface VMCfg {
  name: string
  host?: string
  user?: string
  key?: string
  capacity?: number
  bot_login?: string
  agent?: string
}
interface TargetCfg {
  name: string
  repo?: string
}
interface ConfigShape {
  orchestrator?: OrchestratorCfg
  github?: GhCfg
  vms?: VMCfg[]
  targets?: TargetCfg[]
  [k: string]: any
}

interface RepoOption {
  full_name: string
  private: boolean
  description?: string | null
  pushed_at?: string | null
  avatar?: string
}

function cryptoToken(): string {
  // 16 random bytes as hex — same shape as the install.sh-generated
  // capture token, suitable for X-Capture-Token.
  const buf = new Uint8Array(16)
  crypto.getRandomValues(buf)
  return Array.from(buf, (b) => b.toString(16).padStart(2, '0')).join('')
}

function useRepos(enabled: boolean) {
  const [repos, setRepos] = useState<RepoOption[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!enabled || repos !== null) return
    let alive = true
    fetch('/api/_relay/repos', { credentials: 'include' })
      .then(async (r) => {
        if (r.status === 412) {
          // No GH access_token on file — user signed in before we
          // started capturing it. Surface a clear reconnect prompt.
          throw new Error('reauth')
        }
        if (!r.ok) throw new Error(r.statusText || String(r.status))
        return r.json()
      })
      .then((j: { repos?: RepoOption[]; error?: string }) => {
        if (!alive) return
        if (j.error) setError(j.error)
        setRepos(j.repos ?? [])
      })
      .catch((e) => { if (alive) setError(String(e.message ?? e)) })
    return () => { alive = false }
  }, [enabled, repos])
  return { repos, error }
}

function RepoPicker({ value, onChange, placeholder, repos, error }: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  repos: RepoOption[] | null
  error?: string | null
}) {
  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState('')
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as globalThis.Node)) setOpen(false)
    }
    window.addEventListener('pointerdown', onDown)
    return () => window.removeEventListener('pointerdown', onDown)
  }, [])
  const filtered = useMemo(() => {
    if (!repos) return []
    const f = filter.toLowerCase()
    return f ? repos.filter((r) => r.full_name.toLowerCase().includes(f)).slice(0, 50) : repos.slice(0, 50)
  }, [repos, filter])
  const selected = repos?.find((r) => r.full_name === value)
  const [owner, name] = (value || '').split('/')
  const avatar = selected?.avatar ?? (owner ? `https://github.com/${owner}.png?size=80` : undefined)
  return (
    <div ref={ref} className="relative">
      <div
        onClick={() => setOpen((o) => !o)}
        className="flex items-center gap-3 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-lg px-3 py-2.5 cursor-pointer hover:ring-zinc-400 dark:hover:ring-zinc-600 transition-colors"
      >
        {value ? (
          <>
            {avatar && <img src={avatar} alt="" className="w-7 h-7 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />}
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-1.5 text-[13px] text-zinc-900 dark:text-zinc-100 truncate">
                <span className="text-zinc-500 dark:text-zinc-400">{owner}</span>
                <span className="text-zinc-300 dark:text-zinc-600">/</span>
                <span className="font-medium mono">{name}</span>
                {selected?.private && <span className="text-[10.5px] text-zinc-500 dark:text-zinc-400">private</span>}
              </div>
              {selected?.description && (
                <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 truncate">
                  {selected.description}
                </div>
              )}
            </div>
          </>
        ) : (
          <span className="flex-1 text-[12.5px] text-zinc-500 dark:text-zinc-400">
            {placeholder || 'pick a repo or type owner/repo'}
          </span>
        )}
        <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} className={`text-zinc-400 transition-transform ${open ? 'rotate-180' : ''}`}>
          <polyline points="6 9 12 15 18 9" />
        </svg>
      </div>
      {open && (
        <div className="absolute z-20 mt-1.5 w-full bg-white dark:bg-zinc-900 ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-lg shadow-xl shadow-zinc-300/40 dark:shadow-black/40 overflow-hidden">
          <div className="flex items-center gap-2 px-3 py-2.5 border-b border-zinc-200 dark:border-zinc-800">
            <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} className="text-zinc-400">
              <circle cx="11" cy="11" r="7" />
              <line x1="20" y1="20" x2="16.5" y2="16.5" />
            </svg>
            <input
              autoFocus
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="search your repos…"
              className="w-full text-[13px] outline-none bg-transparent text-zinc-900 dark:text-zinc-100"
            />
          </div>
          <div className="max-h-[320px] overflow-auto">
            {error === 'reauth' && (
              <div className="px-3 py-3 flex items-start gap-3 bg-amber-50/60 dark:bg-amber-900/20 border-b border-amber-200 dark:border-amber-900/50">
                <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} className="text-amber-600 mt-0.5 flex-shrink-0">
                  <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
                </svg>
                <div className="flex-1">
                  <div className="text-[12.5px] text-amber-900 dark:text-amber-200">Reconnect GitHub to load your repos</div>
                  <div className="text-[11px] text-amber-700 dark:text-amber-300/80 mt-0.5">
                    Sign in again so orchid can read your repo list — old sessions don't carry the token.
                  </div>
                  <a
                    href="/login"
                    className="mono inline-block mt-1.5 text-[11px] px-2 py-0.5 rounded bg-amber-900 text-amber-50 dark:bg-amber-100 dark:text-amber-900 hover:opacity-90"
                  >Reconnect</a>
                </div>
              </div>
            )}
            {!repos && !error && (
              <div className="px-3 py-4 text-[12.5px] text-zinc-500 dark:text-zinc-400 italic">loading repos…</div>
            )}
            {repos && filtered.length === 0 && (
              <div className="px-3 py-4 text-[12.5px] text-zinc-500 dark:text-zinc-400">
                no matches — paste owner/repo below
              </div>
            )}
            {filtered.map((r) => (
              <button
                key={r.full_name}
                onClick={() => { onChange(r.full_name); setOpen(false); setFilter('') }}
                className="w-full text-left px-3 py-2 hover:bg-zinc-50 dark:hover:bg-zinc-800/60 flex items-center gap-3 transition-colors group"
              >
                <img src={r.avatar} alt="" className="w-7 h-7 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-1.5 text-[13px]">
                    <span className="text-zinc-500 dark:text-zinc-400 truncate">{r.full_name.split('/')[0]}</span>
                    <span className="text-zinc-300 dark:text-zinc-600">/</span>
                    <span className="font-medium text-zinc-900 dark:text-zinc-100 mono truncate">{r.full_name.split('/')[1]}</span>
                    {r.private && <span className="text-[10.5px] text-zinc-500 dark:text-zinc-400">private</span>}
                  </div>
                  {r.description && (
                    <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 truncate">{r.description}</div>
                  )}
                </div>
                <span className="mono text-[10px] text-zinc-500 dark:text-zinc-400 opacity-0 group-hover:opacity-100 transition-opacity flex-shrink-0">
                  {timeAgo(r.pushed_at)}
                </span>
              </button>
            ))}
          </div>
          <div className="border-t border-zinc-200 dark:border-zinc-800 px-3 py-2 bg-zinc-50 dark:bg-zinc-950">
            <input
              value={value}
              onChange={(e) => onChange(e.target.value)}
              placeholder="…or paste owner/repo"
              className="mono w-full text-[11.5px] outline-none bg-transparent text-zinc-700 dark:text-zinc-300 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
            />
          </div>
        </div>
      )}
    </div>
  )
}

function timeAgo(iso?: string | null): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'now'
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d`
  const mo = Math.floor(d / 30)
  if (mo < 12) return `${mo}mo`
  return `${Math.floor(mo / 12)}y`
}

type SectionId = 'orch' | 'access' | 'capture' | 'vms' | 'targets' | 'usage' | 'danger'

interface MemNote { name: string; file: string; target: string; summary: string; links: string[]; backlinks: string[] }
interface MemTreeNode { name: string; path: string; note?: MemNote; children: MemTreeNode[] }

// buildMemTree turns note paths (memory/<owner>/<repo>/note.md) into a real
// nested directory tree — a cgit-style file browser. Dirs sort before files,
// alphabetically.
function buildMemTree(notes: MemNote[]): MemTreeNode {
  const root: MemTreeNode = { name: '', path: '', children: [] }
  const dirs = new Map<string, MemTreeNode>([['', root]])
  const ensureDir = (path: string): MemTreeNode => {
    let node = dirs.get(path)
    if (node) return node
    const slash = path.lastIndexOf('/')
    const parent = ensureDir(slash < 0 ? '' : path.slice(0, slash))
    node = { name: path.slice(slash + 1), path, children: [] }
    dirs.set(path, node)
    parent.children.push(node)
    return node
  }
  for (const n of notes) {
    const slash = n.file.lastIndexOf('/')
    const parent = ensureDir(slash < 0 ? '' : n.file.slice(0, slash))
    parent.children.push({ name: n.file.slice(slash + 1), path: n.file, note: n, children: [] })
  }
  const sort = (node: MemTreeNode) => {
    node.children.sort((a, b) => {
      const ad = a.note === undefined, bd = b.note === undefined
      if (ad !== bd) return ad ? -1 : 1
      return a.name.localeCompare(b.name)
    })
    node.children.forEach(sort)
  }
  sort(root)
  return root
}

// parseFm splits a note's YAML-ish frontmatter from its body and flattens it to
// key/value pairs (picks up nested keys like metadata.type by their indented
// `key: value` line). Block scalars (`desc: |`) and value-less keys are skipped.
function parseFm(text: string): { meta: [string, string][]; body: string } {
  if (!text.startsWith('---')) return { meta: [], body: text }
  const end = text.indexOf('\n---', 3)
  if (end < 0) return { meta: [], body: text }
  const meta: [string, string][] = []
  for (const line of text.slice(3, end).split('\n')) {
    const m = line.match(/^\s*([A-Za-z0-9_-]+):\s*(.*)$/)
    if (m) { const v = m[2].trim(); if (v && v !== '|') meta.push([m[1], v]) }
  }
  return { meta, body: text.slice(end + 4).replace(/^\n+/, '') }
}

// MemoryPage is a compact directory-tree browser over the swarm's git-backed
// memory store: nested folders (owner/repo/...), markdown render on click,
// search across notes, and a small backlinks/links footer. No graph — a tree.
function MemoryPage() {
  const [notes, setNotes] = useState<MemNote[]>([])
  const [gh, setGh] = useState<{ repo: string; branch: string; subdir: string } | null>(null)
  const [sel, setSel] = useState<MemNote | null>(null)
  const [dirSel, setDirSel] = useState('') // selected folder ('' = root); shown when no file selected
  const [raw, setRaw] = useState('')
  const [loading, setLoading] = useState(true)
  const [q, setQ] = useState('')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  useEffect(() => {
    fetch('/api/memory', { credentials: 'include', cache: 'no-store' })
      .then((r) => r.json())
      .then((d) => { setNotes(d.notes || []); setGh({ repo: d.repo, branch: d.branch, subdir: d.subdir }) })
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [])

  // Fetch the open file, or the selected folder's README (its MEMORY.md index).
  // A folder without a MEMORY.md leaves raw empty → an auto-generated TOC shows.
  useEffect(() => {
    const path = sel ? sel.file : (dirSel ? dirSel + '/' : '') + 'MEMORY.md'
    let live = true
    fetch('/api/memory?note=' + encodeURIComponent(path), { credentials: 'include', cache: 'no-store' })
      .then((r) => (r.ok ? r.text() : Promise.reject()))
      .then((t) => { if (live) setRaw(t) })
      .catch(() => { if (live) setRaw('') })
    return () => { live = false }
  }, [sel, dirSel])

  const parsed = useMemo(() => parseFm(raw), [raw])
  const html = useMemo(() => (parsed.body ? (marked.parse(parsed.body) as string) : ''), [parsed])
  const ghBlob = (file: string) => gh ? `https://github.com/${gh.repo}/blob/${gh.branch}/${gh.subdir}/${file}` : ''
  const ghTree = gh ? `https://github.com/${gh.repo}/tree/${gh.branch}/${gh.subdir}` : ''

  const byFile = useMemo(() => new Map(notes.map((n) => [n.file, n])), [notes])
  const searching = q.trim() !== ''
  // Client-side filter is fine at current scale; if the store grows to many
  // thousands, move search server-side (the API already reads every note).
  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase()
    if (!needle) return notes
    return notes.filter((n) =>
      n.file.toLowerCase().includes(needle) ||
      n.name.toLowerCase().includes(needle) ||
      n.summary.toLowerCase().includes(needle))
  }, [notes, q])
  const tree = useMemo(() => buildMemTree(filtered), [filtered])

  const toggle = (path: string) =>
    setCollapsed((s) => { const n = new Set(s); n.has(path) ? n.delete(path) : n.add(path); return n })

  // Recursive cgit-style rows. Dirs collapse/expand (force-open while searching);
  // files select + render. Indent by depth.
  const renderNode = (node: MemTreeNode, depth: number): React.ReactNode => {
    const pad = { paddingLeft: depth * 13 + 4 }
    if (node.note === undefined) {
      const open = searching || !collapsed.has(node.path)
      const onDir = !sel && dirSel === node.path
      return (
        <div key={node.path || '/'}>
          {node.name && (
            <div style={pad}
              className={'w-full flex items-center gap-1 py-[3px] pr-1 text-[12.5px] rounded ' + (onDir
                ? 'bg-violet-100 text-violet-800 dark:bg-violet-500/15 dark:text-violet-200'
                : 'text-zinc-600 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900')}>
              <span onClick={() => toggle(node.path)} className={'text-zinc-400 w-3 inline-block cursor-pointer transition-transform ' + (open ? 'rotate-90' : '')}>›</span>
              <span onClick={() => { setSel(null); setDirSel(node.path); if (collapsed.has(node.path)) toggle(node.path) }} className="truncate font-medium cursor-pointer flex-1">{node.name}</span>
            </div>
          )}
          {open && node.children.map((c) => renderNode(c, node.name ? depth + 1 : depth))}
        </div>
      )
    }
    const on = sel?.file === node.note.file
    return (
      <button key={node.path} onClick={() => setSel(node.note!)} style={pad}
        className={'w-full flex items-center gap-1 py-[3px] pr-1 text-left text-[12.5px] rounded ' + (on
          ? 'bg-violet-100 text-violet-800 dark:bg-violet-500/15 dark:text-violet-200'
          : 'text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-900')}>
        <span className="w-3 inline-block text-zinc-300 dark:text-zinc-600">·</span>
        <span className="truncate">{node.name.replace(/\.md$/, '')}</span>
      </button>
    )
  }

  const linkChips = (files: string[]) => (
    <div className="flex flex-wrap gap-1.5">
      {files.map((f) => {
        const t = byFile.get(f)
        const cross = t && sel && t.target && t.target !== sel.target
        return (
          <button key={f} onClick={() => t && setSel(t)} disabled={!t}
            className="mono text-[11px] px-2 py-0.5 rounded border border-zinc-200 dark:border-zinc-700 hover:bg-zinc-100 dark:hover:bg-zinc-800 text-zinc-600 dark:text-zinc-300 disabled:opacity-50">
            {cross && <span className="text-zinc-400">{t!.target}/</span>}{t ? t.name : f}
          </button>
        )
      })}
    </div>
  )

  return (
    <div className="flex-1 min-w-0 w-full max-w-screen-xl mx-auto p-4 sm:p-6">
      <div className="flex items-center gap-3 mb-3">
        <h1 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Memory</h1>
        <span className="mono text-[11px] px-1.5 py-0.5 rounded-full bg-zinc-200/80 dark:bg-zinc-700/70 text-zinc-600 dark:text-zinc-300 tabular-nums">{notes.length}</span>
        {ghTree && <a href={ghTree} target="_blank" rel="noreferrer" className="mono text-[11px] text-zinc-400 hover:text-violet-500 truncate">{gh!.repo}/{gh!.subdir} ↗</a>}
      </div>
      {loading ? (
        <div className="text-sm text-zinc-400">Loading…</div>
      ) : notes.length === 0 ? (
        <div className="text-sm text-zinc-400">No memories yet — the store is empty. Notes appear here as sessions learn.</div>
      ) : (
        <div className="flex flex-col md:flex-row gap-5 items-start">
          {/* Left: search + directory tree */}
          <div className="w-full md:w-72 flex-shrink-0">
            <input
              value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search…"
              className="w-full mb-2 rounded-md border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-950 px-3 py-1.5 text-sm text-zinc-800 dark:text-zinc-100 placeholder-zinc-400 focus:outline-none focus:ring-1 focus:ring-violet-400"
            />
            {filtered.length === 0
              ? <div className="text-[13px] text-zinc-400 px-1">No matches.</div>
              : <div className="flex flex-col">{tree.children.map((c) => renderNode(c, 0))}</div>}
          </div>

          {/* Right: rendered note + backlinks/links */}
          <div className="flex-1 min-w-0 w-full">
            {sel ? (
              <div className="flex flex-col gap-4">
                {/* Frontmatter, rendered as a metadata card */}
                <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-zinc-50 dark:bg-zinc-900/50 px-4 py-3">
                  <div className="flex items-start justify-between gap-3">
                    <div className="text-base font-semibold text-zinc-900 dark:text-zinc-100 truncate">
                      {parsed.meta.find(([k]) => k === 'name')?.[1] || sel.name}
                    </div>
                    <a href={ghBlob(sel.file)} target="_blank" rel="noreferrer"
                      className="mono text-[11px] text-zinc-400 hover:text-violet-500 flex-shrink-0 mt-1">{sel.file} ↗</a>
                  </div>
                  {parsed.meta.find(([k]) => k === 'description') && (
                    <p className="text-[13px] text-zinc-500 dark:text-zinc-400 mt-1">{parsed.meta.find(([k]) => k === 'description')![1]}</p>
                  )}
                  {parsed.meta.filter(([k]) => k !== 'name' && k !== 'description').length > 0 && (
                    <div className="flex flex-wrap gap-x-4 gap-y-1 mt-2">
                      {parsed.meta.filter(([k]) => k !== 'name' && k !== 'description').map(([k, v]) => (
                        <span key={k} className="text-[12px]"><span className="text-zinc-400">{k}:</span> <span className="mono text-zinc-600 dark:text-zinc-300">{v}</span></span>
                      ))}
                    </div>
                  )}
                </div>
                <article className="docs-prose rounded-lg border border-zinc-200 dark:border-zinc-800 p-4 sm:p-6 bg-white dark:bg-zinc-950" dangerouslySetInnerHTML={{ __html: html }} />
                {(sel.backlinks?.length > 0 || sel.links?.length > 0) && (
                  <div className="flex flex-col gap-3 rounded-lg border border-zinc-200 dark:border-zinc-800 p-4">
                    {sel.backlinks?.length > 0 && (
                      <div>
                        <div className="text-[11px] font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400 mb-1.5">← Backlinks ({sel.backlinks.length})</div>
                        {linkChips(sel.backlinks)}
                      </div>
                    )}
                    {sel.links?.length > 0 && (
                      <div>
                        <div className="text-[11px] font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400 mb-1.5">→ Links ({sel.links.length})</div>
                        {linkChips(sel.links)}
                      </div>
                    )}
                  </div>
                )}
              </div>
            ) : (() => {
              // Folder view: breadcrumb + README (the dir's MEMORY.md, if any) +
              // an auto-generated table of contents of its direct children.
              const prefix = dirSel ? dirSel + '/' : ''
              const childDirs = new Set<string>()
              const childFiles: MemNote[] = []
              for (const n of notes) {
                if (!n.file.startsWith(prefix)) continue
                const rest = n.file.slice(prefix.length)
                const slash = rest.indexOf('/')
                if (slash >= 0) childDirs.add(rest.slice(0, slash))
                else childFiles.push(n)
              }
              const segs = dirSel ? dirSel.split('/') : []
              return (
                <div className="flex flex-col gap-4">
                  <div className="flex items-center gap-1.5 text-[13px] flex-wrap">
                    <button onClick={() => setDirSel('')} className={!dirSel ? 'font-semibold text-zinc-800 dark:text-zinc-100' : 'text-zinc-500 hover:text-violet-500'}>{gh?.subdir || 'memory'}</button>
                    {segs.map((s, i) => (
                      <span key={i} className="flex items-center gap-1.5">
                        <span className="text-zinc-300 dark:text-zinc-600">/</span>
                        <button onClick={() => setDirSel(segs.slice(0, i + 1).join('/'))} className={i === segs.length - 1 ? 'font-semibold text-zinc-800 dark:text-zinc-100' : 'text-zinc-500 hover:text-violet-500'}>{s}</button>
                      </span>
                    ))}
                    {gh && <a href={ghTree + (dirSel ? '/' + dirSel : '')} target="_blank" rel="noreferrer" className="mono text-[11px] text-zinc-400 hover:text-violet-500 ml-1">↗</a>}
                  </div>
                  {html && <article className="docs-prose rounded-lg border border-zinc-200 dark:border-zinc-800 p-4 sm:p-6 bg-white dark:bg-zinc-950" dangerouslySetInnerHTML={{ __html: html }} />}
                  {(childDirs.size > 0 || childFiles.length > 0) && (
                    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/60">
                      {[...childDirs].sort().map((d) => (
                        <button key={d} onClick={() => setDirSel(prefix + d)} className="w-full flex items-center gap-2 px-4 py-2 text-left text-[13px] hover:bg-zinc-50 dark:hover:bg-zinc-900/60">
                          <span className="text-zinc-400">▸</span><span className="font-medium text-zinc-700 dark:text-zinc-200">{d}/</span>
                        </button>
                      ))}
                      {childFiles.sort((a, b) => a.name.localeCompare(b.name)).map((n) => (
                        <button key={n.file} onClick={() => setSel(n)} className="w-full flex flex-col items-start px-4 py-2 text-left hover:bg-zinc-50 dark:hover:bg-zinc-900/60">
                          <span className="text-[13px] font-medium text-zinc-800 dark:text-zinc-100">{n.name}</span>
                          {n.summary && <span className="text-[12px] text-zinc-500 dark:text-zinc-400 line-clamp-1">{n.summary}</span>}
                        </button>
                      ))}
                    </div>
                  )}
                  {!html && childDirs.size === 0 && childFiles.length === 0 && (
                    <div className="text-sm text-zinc-400 px-1 pt-2">Empty.</div>
                  )}
                </div>
              )
            })()}
          </div>
        </div>
      )}
    </div>
  )
}

// Top-level tabs. Sessions is the list; the rest open SettingsPage focused on a
// section — Machines (VMs), Analytics (usage), Integrations get their own tab;
// Settings holds the rest (orch/access/capture/targets/danger) via its own nav.
type Tab = 'sessions' | 'machines' | 'analytics' | 'memory' | 'settings'
const TAB_SECTION: Record<Exclude<Tab, 'sessions' | 'memory' | 'analytics' | 'machines'>, SectionId> = {
  settings: 'orch',
}
function sectionToTab(s: SectionId): Tab {
  return s === 'vms' ? 'machines' : s === 'usage' ? 'analytics' : 'settings'
}

function SettingsPage({ jobs, state, relay, initialSection, onClose }: {
  jobs: Job[]
  state: State
  relay: RelayInfo | null
  initialSection?: SectionId
  onClose: () => void
}) {
  const [cfg, setCfg] = useState<OrchestratorCfg | null>(null)
  const [gh, setGh] = useState<GhCfg | null>(null)
  const [vms, setVms] = useState<VMCfg[]>([])
  const [targets, setTargets] = useState<TargetCfg[]>([])
  const [original, setOriginal] = useState<{
    cfg: OrchestratorCfg; gh: GhCfg; targets: TargetCfg[]
  } | null>(null)
  const [status, setStatus] = useState<string>('')
  const { repos, error: reposError } = useRepos(true)

  // Machines / Analytics / Integrations each own a top-level tab now, so
  // here they render as a single bare section. The "Settings" tab (orch)
  // flattens everything that's left into one scroll page — no inner nav.
  const flat = !initialSection || initialSection === 'orch'
  const FLAT_IDS: SectionId[] = ['orch', 'access', 'capture', 'targets', 'danger']
  const vis = (id: SectionId) => flat ? FLAT_IDS.includes(id) : initialSection === id
  const pageTitle = initialSection === 'vms' ? 'Machines'
    : initialSection === 'usage' ? 'Analytics'
    : 'Settings'

  useEffect(() => {
    let alive = true
    fetch('/api/config', { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : Promise.reject(r.statusText))
      .then((j: ConfigShape) => {
        if (!alive) return
        const o = (j.orchestrator ?? {}) as OrchestratorCfg
        const g = (j.github ?? {}) as GhCfg
        const v = (j.vms ?? []) as VMCfg[]
        const t = (j.targets ?? []) as TargetCfg[]
        setCfg({ ...o }); setGh({ ...g }); setVms([...v]); setTargets([...t])
        setOriginal({ cfg: { ...o }, gh: { ...g }, targets: [...t] })
      })
      .catch((e) => setStatus('load failed: ' + String(e)))
    return () => { alive = false }
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const dirty = useMemo(() => {
    if (!cfg || !gh || !original) return false
    return JSON.stringify(cfg) !== JSON.stringify(original.cfg) ||
      JSON.stringify(gh) !== JSON.stringify(original.gh) ||
      JSON.stringify(targets) !== JSON.stringify(original.targets)
  }, [cfg, gh, targets, original])

  const save = async () => {
    if (!cfg || !gh || !original) return
    setStatus('saving')
    const patch: Record<string, any> = {}

    // Singletons. Strip the nested `capture` so it doesn't accidentally
    // try to serialise as an attribute — capture lives under its own
    // `orchestrator.capture` patch key.
    const orchTop: any = { ...cfg }
    const capture = orchTop.capture
    delete orchTop.capture
    patch.orchestrator = orchTop
    patch.github = gh
    if (capture) patch['orchestrator.capture'] = capture

    // Targets — keyed-block patches. Diff against original. VMs aren't
    // patched from the dashboard: the VMs section is read-only and
    // surfaces a join command instead of an editable form.
    const byNameOrig = (arr: { name: string }[]) => Object.fromEntries(arr.map((x) => [x.name, x]))
    const tgOrig = byNameOrig(original.targets), tgCur = byNameOrig(targets)
    for (const name of new Set([...Object.keys(tgOrig), ...Object.keys(tgCur)])) {
      if (!tgCur[name]) { patch[`target.${name}`] = { __delete: true }; continue }
      const { name: _n, ...body } = tgCur[name] as any
      patch[`target.${name}`] = body
    }

    const r = await fetch('/api/config', {
      method: 'PUT',
      credentials: 'include',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(patch),
    })
    if (!r.ok) {
      setStatus('error: ' + (await r.text()))
      return
    }
    setOriginal({ cfg: { ...cfg }, gh: { ...gh }, targets: [...targets] })
    // Access (allowed_logins) hot-applies via the live relay agent —
    // no restart needed. Everything else (poll interval, http_addr,
    // bot identity, target/VM blocks) is still read once at orch
    // startup, so a bounce is required for those.
    setStatus('saved · Access applies now, other fields on next orch restart')
    setTimeout(() => setStatus(''), 6000)
  }

  // Auto-save: debounce 800ms after the last edit. No Save button — the
  // form persists itself. saveRef keeps the effect off the save closure
  // so it always runs the latest one without re-arming on every render.
  const saveRef = useRef(save)
  saveRef.current = save
  useEffect(() => {
    if (!dirty) return
    const id = setTimeout(() => { saveRef.current() }, 800)
    return () => clearTimeout(id)
  }, [dirty, cfg, gh, targets])

  const setField = <K extends keyof OrchestratorCfg>(k: K, v: OrchestratorCfg[K]) => {
    setCfg((c) => c ? { ...c, [k]: v } : c)
  }
  const setGhField = <K extends keyof GhCfg>(k: K, v: GhCfg[K]) => {
    setGh((g) => g ? { ...g, [k]: v } : g)
  }
  const setCaptureField = <K extends keyof CaptureCfg>(k: K, v: CaptureCfg[K]) => {
    setCfg((c) => c ? { ...c, capture: { ...(c.capture ?? {}), [k]: v } } : c)
  }

  // Aggregate "live" tmux sessions per VM from the polled job list.
  const sessionsByVM = useMemo(() => {
    const m = new Map<string, Job[]>()
    for (const j of jobs) {
      if (!j.tmux || j.closed_state) continue
      const arr = m.get(j.vm) ?? []
      arr.push(j)
      m.set(j.vm, arr)
    }
    return m
  }, [jobs])

  const navItems: { id: SectionId; label: string }[] = [
    { id: 'orch',         label: 'Orchestrator' },
    { id: 'access',       label: 'Access' },
    { id: 'capture',      label: 'Capture' },
    { id: 'vms',          label: 'VMs' },
    { id: 'targets',      label: 'Targets' },
    { id: 'usage',        label: 'Usage' },
    { id: 'danger',       label: 'Danger zone' },
  ]

  return (
    <div className="absolute inset-0 z-30 bg-zinc-50/95 dark:bg-zinc-900/95 backdrop-blur flex flex-col">
      {/* Auto-save status — subtle floating pill, no header bar / Save button. */}
      {status && (
        <div className="pointer-events-none fixed bottom-4 right-4 z-50 mono text-[11px] px-3 py-1.5 rounded-md shadow-lg bg-zinc-900/95 dark:bg-zinc-100/95 text-zinc-50 dark:text-zinc-900">
          {status === 'saving' ? 'saving…' : status}
        </div>
      )}

      <div className="flex-1 min-h-0 flex flex-col">
        <main className="flex-1 min-w-0 overflow-auto">
          <div className="w-full max-w-screen-xl mx-auto p-4 sm:p-6 space-y-6">
            {vis('orch') && (
              <>
                <Section title="GitHub">
                  <Field label="Inbox repo" hint="Issues filed here drive orchid. Labels map to targets.">
                    <RepoPicker
                      value={gh?.inbox_repo ?? ''}
                      onChange={(v) => setGhField('inbox_repo', v)}
                      repos={repos}
                      error={reposError}
                      placeholder="pick or type owner/repo"
                    />
                  </Field>
                </Section>
                <Section title="Orchestrator" subtitle="Core swarm settings — applied on next restart.">
                  <Field label="Poll interval" hint="How often to scan the inbox (e.g. 20s).">
                    <Input value={cfg?.poll_interval ?? ''} onChange={(v) => setField('poll_interval', v)} placeholder="20s" />
                  </Field>
                  <Field label="Branch prefix">
                    <Input value={cfg?.branch_prefix ?? ''} onChange={(v) => setField('branch_prefix', v)} placeholder="orch/" />
                  </Field>
                  <Field label="HTTP address" hint="Where orch's dashboard server listens.">
                    <Input value={cfg?.http_addr ?? ''} onChange={(v) => setField('http_addr', v)} placeholder=":8000" />
                  </Field>
                  <Field label="HTTP secret" hint="Bearer token gating the local dashboard.">
                    <Input value={cfg?.http_secret ?? ''} onChange={(v) => setField('http_secret', v)} placeholder="…" secret />
                  </Field>
                  <Field label="Bot login">
                    <Input value={cfg?.bot_login ?? ''} onChange={(v) => setField('bot_login', v)} placeholder="yourbot" />
                  </Field>
                  <Field label="Bot email">
                    <Input value={cfg?.bot_email ?? ''} onChange={(v) => setField('bot_email', v)} placeholder="yourbot@users.noreply.github.com" />
                  </Field>
                  <Field label="ntfy topic">
                    <Input value={cfg?.ntfy_topic ?? ''} onChange={(v) => setField('ntfy_topic', v)} placeholder="orchid-…" />
                  </Field>
                </Section>
              </>
            )}

            {vis('access') && (
              <Section
                title="Access"
                subtitle="You always have access. Add GitHub users you want to share this dashboard with — they sign in via OAuth and only see your subdomain."
              >
                <AllowedUsers
                  values={cfg?.allowed_logins ?? []}
                  onChange={(v) => setField('allowed_logins', v)}
                />
              </Section>
            )}

            {vis('capture') && (
              <>
                <Section
                  title="Connect Orchid Capture"
                  subtitle="One-click handoff to the macOS app — or copy the values into the iOS app's Settings."
                >
                  {cfg?.capture?.auth_token && (
                    <div className="mb-4">
                      <a
                        href={`orchid://configure?endpoint=${encodeURIComponent(`https://${location.host}/api/drafts`)}&token=${encodeURIComponent(cfg.capture.auth_token)}`}
                        className="inline-flex items-center gap-2 text-[12.5px] mono px-3 py-2 rounded-md bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 hover:opacity-90"
                      >
                        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
                          <polyline points="15 3 21 3 21 9" />
                          <line x1="10" y1="14" x2="21" y2="3" />
                          <path d="M21 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5" />
                        </svg>
                        Open in macOS app
                      </a>
                    </div>
                  )}
                  <Field label="Endpoint">
                    <CopyValue value={`https://${location.host}/api/drafts`} />
                  </Field>
                  <Field label="Auth token" hint="X-Capture-Token. Rotate any time — clients pick up the new value on next request.">
                    <div className="flex items-center gap-2">
                      <div className="flex-1">
                        <Input
                          value={cfg?.capture?.auth_token ?? ''}
                          onChange={(v) => setCaptureField('auth_token', v)}
                          placeholder="…"
                          secret
                        />
                      </div>
                      <button
                        onClick={() => setCaptureField('auth_token', cryptoToken())}
                        className="mono text-[11px] px-3 py-2 rounded-md ring-1 ring-zinc-300 dark:ring-zinc-700 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-100 dark:hover:bg-zinc-800"
                      >regenerate</button>
                    </div>
                  </Field>
                  <Field label="Assets dir" hint="Where uploaded screenshots / clips are stored.">
                    <Input
                      value={cfg?.capture?.assets_dir ?? ''}
                      onChange={(v) => setCaptureField('assets_dir', v)}
                      placeholder="/root/orch/captures"
                    />
                  </Field>
                  <Field label="Public URL" hint="Base URL used to embed images in issue bodies. Leave blank if you don't have one.">
                    <Input
                      value={cfg?.capture?.public_url ?? ''}
                      onChange={(v) => setCaptureField('public_url', v)}
                      placeholder={`https://${location.host}`}
                    />
                  </Field>
                </Section>
              </>
            )}

            {vis('vms') && (
              <Section
                title="Machines"
                subtitle="Worker sessions run on boxes that have joined this orch. Bring a new one online by running the join command on it — no SSH config to fill in here."
              >
                <VMJoinGuide vms={vms} sessionsByVM={sessionsByVM} relay={relay} />
              </Section>
            )}

            {vis('targets') && (
              <Section
                title="Targets"
                subtitle="Inbox labels → work repos. Add a repo and the label defaults to its name (override if you want)."
              >
                <TargetsList targets={targets} setTargets={setTargets} repos={repos} reposError={reposError} />
              </Section>
            )}

            {vis('usage') && (
              <Section
                title="Usage"
                subtitle="Token throughput (input + output + cache writes — cache reads excluded) and live context, pulled from each pane's statusline feed. Updates in near-real-time."
              >
                <UsageTable jobs={jobs} quota={state.quota} governor={state.governor} />
              </Section>
            )}

            {vis('danger') && (
              <Section
                title="Danger zone"
                subtitle="These actions can't be undone from the dashboard."
              >
                <div className="rounded-xl ring-1 ring-rose-200 dark:ring-rose-900/50 p-5 flex items-start gap-5">
                  <div className="flex-1">
                    <div className="text-[14px] text-zinc-900 dark:text-zinc-100 font-medium">Revoke agent token</div>
                    <div className="text-[12px] text-zinc-500 dark:text-zinc-400 mt-1">
                      Disconnects the current orch instance. Sign in again to mint a fresh token,
                      then run <code className="mono">orch join</code> with the new credentials.
                    </div>
                  </div>
                  <button
                    onClick={async () => {
                      if (!confirm('Revoke the current agent token? Your orch will disconnect.')) return
                      const r = await fetch('/api/_relay/revoke', { method: 'POST', credentials: 'include' })
                      if (r.ok) alert('Token revoked. Sign in again to mint a new one.')
                      else alert('Revoke failed: ' + (await r.text()))
                    }}
                    className="mono text-[12px] px-3 py-1.5 rounded-md ring-1 ring-rose-300 dark:ring-rose-700 text-rose-700 dark:text-rose-300 hover:bg-rose-50 dark:hover:bg-rose-950"
                  >
                    Revoke
                  </button>
                </div>
              </Section>
            )}
          </div>
        </main>
      </div>
    </div>
  )
}

function Th({ children, align }: { children: React.ReactNode; align?: 'right' | 'left' }) {
  return <th className={`px-4 py-2 font-medium ${align === 'right' ? 'text-right' : 'text-left'}`}>{children}</th>
}
function Td({ children, align }: { children: React.ReactNode; align?: 'right' | 'left' }) {
  return <td className={`px-4 py-2.5 ${align === 'right' ? 'text-right' : 'text-left'} text-zinc-900 dark:text-zinc-100`}>{children}</td>
}

function Section({ title, subtitle, children }: { title: string; subtitle?: string; children: React.ReactNode }) {
  return (
    <section className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-950 p-4">
      <div className="mb-3">
        <h3 className="text-[13px] font-semibold text-zinc-900 dark:text-zinc-100">{title}</h3>
        {subtitle && <p className="text-[12px] text-zinc-500 dark:text-zinc-400 mt-0.5 max-w-[640px]">{subtitle}</p>}
      </div>
      <div className="space-y-3">{children}</div>
    </section>
  )
}
function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[180px_1fr] gap-4 items-start">
      <div>
        <div className="text-[13px] text-zinc-700 dark:text-zinc-300">{label}</div>
        {hint && <div className="text-[11px] text-zinc-500 dark:text-zinc-400 mt-0.5 leading-snug">{hint}</div>}
      </div>
      <div>{children}</div>
    </div>
  )
}
function Input({ value, onChange, placeholder, secret }: { value: string; onChange: (v: string) => void; placeholder?: string; secret?: boolean }) {
  return (
    <input
      type={secret ? 'password' : 'text'}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      spellCheck={false}
      autoComplete="off"
      className="mono w-full text-[12.5px] bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 focus:ring-zinc-400 dark:focus:ring-zinc-600 rounded-md px-3 py-2 outline-none text-zinc-900 dark:text-zinc-100"
    />
  )
}
function CopyValue({ value, secret }: { value: string; secret?: boolean }) {
  const [revealed, setRevealed] = useState(!secret)
  const [copied, setCopied] = useState(false)
  const display = revealed ? value : value.replace(/./g, '•')
  return (
    <div className="flex items-center gap-2 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-md px-3 py-2">
      <code className="mono flex-1 text-[12px] text-zinc-900 dark:text-zinc-100 truncate select-all">{display}</code>
      {secret && (
        <button
          onClick={() => setRevealed((v) => !v)}
          className="text-[11px] mono text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200"
        >{revealed ? 'hide' : 'show'}</button>
      )}
      <button
        onClick={() => {
          navigator.clipboard.writeText(value).catch(() => {})
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        }}
        className="text-[11px] mono px-2 py-0.5 rounded bg-zinc-200 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-300 dark:hover:bg-zinc-700"
      >{copied ? 'copied' : 'copy'}</button>
    </div>
  )
}

interface GhProfile { login: string; name?: string; bio?: string; avatar_url?: string }

function useGhProfiles(logins: string[]): Map<string, GhProfile | 'loading' | 'missing'> {
  // Public unauth lookup of /users/<login>. Cached in a module-level
  // map so flipping sections doesn't refetch. Errors swallowed — we
  // fall back to a generic avatar placeholder.
  const [, force] = useState(0)
  useEffect(() => {
    for (const login of logins) {
      const key = login.toLowerCase()
      if (!key || profileCache.has(key)) continue
      profileCache.set(key, 'loading')
      fetch(`https://api.github.com/users/${encodeURIComponent(login)}`)
        .then((r) => r.ok ? r.json() : Promise.reject(r.statusText))
        .then((j: any) => {
          profileCache.set(key, { login: j.login, name: j.name ?? undefined, bio: j.bio ?? undefined, avatar_url: j.avatar_url })
          force((n) => n + 1)
        })
        .catch(() => {
          profileCache.set(key, 'missing')
          force((n) => n + 1)
        })
    }
  }, [logins.join(',')])
  const m = new Map<string, GhProfile | 'loading' | 'missing'>()
  for (const login of logins) m.set(login.toLowerCase(), profileCache.get(login.toLowerCase()) ?? 'loading')
  return m
}
const profileCache = new Map<string, GhProfile | 'loading' | 'missing'>()

// VMJoinGuide replaces the old form-based VM CRUD. It surfaces the
// install + join command operators run on a new box to bring it
// online, plus a read-only roster of VMs the orch already knows about
// (with their live session counts).
//
// The command needs the relay subdomain + agent token to embed in the
// `orch join` URL — both come from /api/_relay/info, the same endpoint
// the first-run InstallModal uses. Local-only orchs (no relay) get a
// fallback that points at swarm.hcl, since there's no relay endpoint
// for a fresh box to dial into.
function VMJoinGuide({ vms, sessionsByVM, relay }: {
  vms: VMCfg[]
  sessionsByVM: Map<string, Job[]>
  relay: RelayInfo | null
}) {
  // relay-info now arrives via the App-level events WS. Treat null as
  // "loading"; once the WS lands its first frame, the JoinCommandCard
  // can render. Local-only orchs (no relay agent token) fall into the
  // unavailable branch automatically because relay.token stays null.
  const info: RelayInfo | null | 'unavailable' = relay ?? null

  const isLocal = (vm: VMCfg) =>
    vm.host === 'localhost' || vm.host === '127.0.0.1' || vm.host === '::1'

  return (
    <div className="space-y-6">
      <div>
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-2 px-1">Connected</div>
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
          {vms.length === 0 && (
            <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
              No VMs yet — run the command above on a box to bring it online.
            </div>
          )}
          {vms.map((vm, i) => {
            const live = sessionsByVM.get(vm.name)?.length ?? 0
            const local = isLocal(vm)
            return (
              <div key={vm.name + i} className="flex items-center gap-3 px-4 py-3">
                <span className="w-8 h-8 rounded-md flex items-center justify-center flex-shrink-0 bg-zinc-100 dark:bg-zinc-800 text-zinc-500 dark:text-zinc-400">
                  {local ? (
                    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
                      <rect x="3" y="4" width="18" height="12" rx="2" />
                      <line x1="8" y1="20" x2="16" y2="20" />
                      <line x1="12" y1="16" x2="12" y2="20" />
                    </svg>
                  ) : (
                    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
                      <circle cx="12" cy="12" r="9" />
                      <line x1="3" y1="12" x2="21" y2="12" />
                      <path d="M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18" />
                    </svg>
                  )}
                </span>
                <div className="flex-1 min-w-0">
                  <div className="mono text-[13.5px] font-medium text-zinc-900 dark:text-zinc-100 truncate">
                    {vm.name || '(unnamed)'}
                  </div>
                  <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 mono truncate">
                    {local ? 'localhost' : `${vm.user ?? 'root'}@${vm.host ?? '?'}`} · {vm.agent || 'claude'}
                  </div>
                </div>
                <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400 flex-shrink-0">
                  {live} / {vm.capacity ?? '∞'}
                </span>
              </div>
            )
          })}
        </div>
        <div className="mt-2 px-1 text-[11.5px] text-zinc-500 dark:text-zinc-400">
          Per-VM SSH settings, capacity, agent, and bot overrides live in <code className="mono">swarm.hcl</code>.
        </div>
      </div>
      <JoinCommandCard info={info} />
    </div>
  )
}

import type { UsageHistoryRow } from './types'

/// Stacked-bar chart of daily Claude spend over a rolling window.
/// SVG, no chart library — every dep we keep out is one less hit on
/// the bundle size. X axis = day, Y axis = USD. Bars are stacked by
/// model family so a glance shows where the budget went (opus vs
/// sonnet vs haiku splits).
// Subscriptions don't bill per-token, so dollar figures are noise here.
// What matters is token throughput. tok() = real NEW tokens: input +
// output + cache WRITES. We deliberately EXCLUDE cache_read — every turn
// re-reads the full cached prefix, so it re-counts the same tokens
// thousands of times (billed ~0.1x, not real throughput). Including it
// inflated a day to ~20B ("read Wikipedia several times"); this is the
// honest consumption number. fmtTok renders it compact (1.2M / 340K).
function tok(r: UsageHistoryRow): number {
  return (r.input_tokens ?? 0) + (r.output_tokens ?? 0) + (r.cache_creation ?? 0)
}
function fmtTok(n: number): string {
  if (n >= 1e9) return (n / 1e9).toFixed(n < 1e10 ? 1 : 0) + 'B'
  if (n >= 1e6) return (n / 1e6).toFixed(n < 1e7 ? 1 : 0) + 'M'
  if (n >= 1e3) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0) + 'K'
  return String(Math.round(n))
}

function UsageChart({ rows, days }: { rows: UsageHistoryRow[]; days: number }) {
  type DayBar = { date: string; opus: number; sonnet: number; haiku: number; other: number; total: number }
  const grid = useMemo<DayBar[]>(() => {
    // Build the contiguous date axis even when a day has zero spend.
    // Otherwise an idle weekend leaves a hole and the bars shift.
    const by = new Map<string, DayBar>()
    const today = new Date()
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(today.getTime() - i * 86400_000)
      const k = d.toISOString().slice(0, 10)
      by.set(k, { date: k, opus: 0, sonnet: 0, haiku: 0, other: 0, total: 0 })
    }
    for (const r of rows) {
      const bar = by.get(r.date)
      if (!bar) continue
      const m = r.model.toLowerCase()
      const fam = m.includes('opus') ? 'opus'
        : m.includes('sonnet') ? 'sonnet'
        : m.includes('haiku') ? 'haiku' : 'other'
      bar[fam] += tok(r)
      bar.total += tok(r)
    }
    return Array.from(by.values())
  }, [rows, days])

  const max = Math.max(1, ...grid.map((g) => g.total))
  const total = grid.reduce((acc, g) => acc + g.total, 0)
  const W = 760, H = 200, pad = { l: 36, r: 12, t: 12, b: 22 }
  const innerW = W - pad.l - pad.r
  const innerH = H - pad.t - pad.b
  const barW = innerW / grid.length

  const fam = {
    opus:   { fill: '#a78bfa', label: 'opus' },
    sonnet: { fill: '#34d399', label: 'sonnet' },
    haiku:  { fill: '#60a5fa', label: 'haiku' },
    other:  { fill: '#71717a', label: 'other' },
  } as const

  const ticks = [0, 0.25, 0.5, 0.75, 1].map((p) => ({ y: pad.t + innerH - p * innerH, v: p * max }))

  const [hover, setHover] = useState<{ i: number; x: number; y: number } | null>(null)
  const svgRef = useRef<SVGSVGElement | null>(null)
  const hovered = hover ? grid[hover.i] : null

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5 relative">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
          Daily tokens · last {days}d
        </div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          {fmtTok(total)} tok window
        </div>
      </div>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        className="w-full"
        onPointerLeave={() => setHover(null)}
      >
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={pad.l} x2={W - pad.r} y1={t.y} y2={t.y} stroke="currentColor" className="text-zinc-200 dark:text-zinc-800" strokeDasharray={i === 0 ? '' : '2 3'} />
            <text x={pad.l - 6} y={t.y + 3} textAnchor="end" className="fill-zinc-500 dark:fill-zinc-400" fontSize="9">
              {fmtTok(t.v)}
            </text>
          </g>
        ))}
        {grid.map((g, i) => {
          const x = pad.l + i * barW
          const stacks: { fill: string; v: number }[] = [
            { fill: fam.opus.fill,   v: g.opus },
            { fill: fam.sonnet.fill, v: g.sonnet },
            { fill: fam.haiku.fill,  v: g.haiku },
            { fill: fam.other.fill,  v: g.other },
          ].filter((s) => s.v > 0)
          let yCursor = pad.t + innerH
          const hot = hover?.i === i
          return (
            <g
              key={g.date}
              onPointerEnter={(e) => {
                const r = svgRef.current?.getBoundingClientRect()
                if (!r) return
                setHover({ i, x: e.clientX - r.left, y: e.clientY - r.top })
              }}
              onPointerMove={(e) => {
                const r = svgRef.current?.getBoundingClientRect()
                if (!r) return
                setHover({ i, x: e.clientX - r.left, y: e.clientY - r.top })
              }}
              style={{ cursor: 'pointer' }}
            >
              {/* invisible full-height hit target so even a $0 day is hoverable */}
              <rect x={x} y={pad.t} width={barW} height={innerH} fill="transparent" />
              {stacks.map((s, j) => {
                const h = (s.v / max) * innerH
                yCursor -= h
                return <rect key={j} x={x + 1} y={yCursor} width={Math.max(0, barW - 2)} height={Math.max(0, h)} fill={s.fill} opacity={hover && !hot ? 0.5 : 1} />
              })}
              {i % Math.ceil(days / 8) === 0 && (
                <text x={x + barW / 2} y={H - 6} textAnchor="middle" className="fill-zinc-500 dark:fill-zinc-400" fontSize="9">
                  {g.date.slice(5)}
                </text>
              )}
            </g>
          )
        })}
      </svg>
      {hovered && hover && (
        <div
          className="pointer-events-none absolute bg-zinc-900/95 dark:bg-zinc-100/95 text-zinc-50 dark:text-zinc-900 mono text-[11px] rounded px-2 py-1 shadow-lg z-10"
          style={{ left: Math.min(hover.x + 14, 600), top: Math.max(40, hover.y - 8) }}
        >
          <div className="text-[10.5px] opacity-80">{hovered.date}</div>
          <div className="tabular-nums font-medium">{fmtTok(hovered.total)} tok</div>
          {hovered.opus   > 0 && <div className="tabular-nums">opus {fmtTok(hovered.opus)}</div>}
          {hovered.sonnet > 0 && <div className="tabular-nums">sonnet {fmtTok(hovered.sonnet)}</div>}
          {hovered.haiku  > 0 && <div className="tabular-nums">haiku {fmtTok(hovered.haiku)}</div>}
          {hovered.other  > 0 && <div className="tabular-nums">other {fmtTok(hovered.other)}</div>}
        </div>
      )}
      <div className="flex items-center gap-3 mt-2 mono text-[10.5px] text-zinc-500 dark:text-zinc-400">
        {(['opus', 'sonnet', 'haiku', 'other'] as const).map((k) => (
          <span key={k} className="inline-flex items-center gap-1">
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: fam[k].fill }} />
            {fam[k].label}
          </span>
        ))}
      </div>
    </div>
  )
}

interface DonutSlice {
  key: string
  label: string
  value: number
  color: string
  meta?: string
}

/// Reusable donut chart. Hovering a slice highlights it (outer-ring
/// pop + dimmed siblings) and surfaces a tooltip with the label /
/// value / share, anchored at the cursor. Pure SVG + a thin
/// React-state hover model so we keep zero chart-lib deps.
function Donut({ slices, title, units = '$', subtitle, fmt }: {
  slices: DonutSlice[]
  title: string
  units?: string
  subtitle?: string
  fmt?: (n: number) => string
}) {
  const fv = fmt ?? ((n: number) => units + n.toFixed(2))
  const [hover, setHover] = useState<{ key: string; x: number; y: number } | null>(null)
  const ref = useRef<SVGSVGElement | null>(null)
  const total = useMemo(() => slices.reduce((acc, s) => acc + s.value, 0), [slices])
  // Build arcs. Skip zero slices so the legend isn't littered with
  // "0.0%" entries from sessions that ran but consumed nothing.
  const arcs = useMemo(() => {
    const r = 56, R = 92
    const cx = 110, cy = 110
    let a = -Math.PI / 2 // 12-o'clock start
    return slices.filter((s) => s.value > 0).map((s) => {
      const frac = s.value / Math.max(1e-9, total)
      const a2 = a + frac * Math.PI * 2
      const large = a2 - a > Math.PI ? 1 : 0
      const sx = cx + R * Math.cos(a),  sy = cy + R * Math.sin(a)
      const ex = cx + R * Math.cos(a2), ey = cy + R * Math.sin(a2)
      const sx2 = cx + r * Math.cos(a2), sy2 = cy + r * Math.sin(a2)
      const ex2 = cx + r * Math.cos(a),  ey2 = cy + r * Math.sin(a)
      const d = `M ${sx} ${sy} A ${R} ${R} 0 ${large} 1 ${ex} ${ey} L ${sx2} ${sy2} A ${r} ${r} 0 ${large} 0 ${ex2} ${ey2} Z`
      a = a2
      return { slice: s, d, frac }
    })
  }, [slices, total])

  const hoveredSlice = hover ? slices.find((s) => s.key === hover.key) : null
  const hoveredFrac = hoveredSlice ? hoveredSlice.value / Math.max(1e-9, total) : 0

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const r = ref.current?.getBoundingClientRect()
    if (!r) return
    setHover((h) => h ? { ...h, x: e.clientX - r.left, y: e.clientY - r.top } : h)
  }

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5 relative">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">{title}</div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          {fv(total)}{subtitle ? ' · ' + subtitle : ''}
        </div>
      </div>
      <div className="flex items-center gap-6">
        <svg
          ref={ref}
          viewBox="0 0 220 220"
          className="flex-shrink-0"
          style={{ width: 220, height: 220 }}
          onPointerMove={onMove}
          onPointerLeave={() => setHover(null)}
        >
          {arcs.map(({ slice, d }) => {
            const dim = hover && hover.key !== slice.key ? 0.35 : 1
            const pop = hover && hover.key === slice.key
            return (
              <path
                key={slice.key}
                d={d}
                fill={slice.color}
                opacity={dim}
                style={{
                  transformOrigin: '110px 110px',
                  transform: pop ? 'scale(1.04)' : 'scale(1)',
                  transition: 'opacity 80ms linear, transform 80ms ease-out',
                  cursor: 'pointer',
                }}
                onPointerEnter={(e) => {
                  const r = ref.current?.getBoundingClientRect()
                  if (!r) return
                  setHover({ key: slice.key, x: e.clientX - r.left, y: e.clientY - r.top })
                }}
              />
            )
          })}
          {/* center label */}
          <text x={110} y={106} textAnchor="middle" className="fill-zinc-900 dark:fill-zinc-100 mono" fontSize="18">
            {hoveredSlice ? fv(hoveredSlice.value) : fv(total)}
          </text>
          <text x={110} y={124} textAnchor="middle" className="fill-zinc-500 dark:fill-zinc-400 mono" fontSize="10">
            {hoveredSlice ? `${(hoveredFrac * 100).toFixed(1)}%` : 'total'}
          </text>
        </svg>
        <div className="flex-1 min-w-0">
          <div className="grid grid-cols-1 gap-1.5 max-h-[200px] overflow-y-auto pr-2">
            {slices.filter((s) => s.value > 0).map((s) => {
              const frac = s.value / Math.max(1e-9, total)
              const dim = hover && hover.key !== s.key ? 'opacity-50' : ''
              return (
                <div
                  key={s.key}
                  className={'flex items-center gap-2 text-[11.5px] ' + dim}
                  onPointerEnter={() => setHover({ key: s.key, x: 0, y: 0 })}
                  onPointerLeave={() => setHover(null)}
                >
                  <span className="inline-block w-2 h-2 rounded-sm flex-shrink-0" style={{ background: s.color }} />
                  <span className="flex-1 min-w-0 truncate text-zinc-700 dark:text-zinc-300" title={s.meta || s.label}>
                    {s.label}
                  </span>
                  <span className="mono tabular-nums text-zinc-600 dark:text-zinc-300 w-14 text-right">{fv(s.value)}</span>
                  <span className="mono tabular-nums text-zinc-500 dark:text-zinc-400 w-10 text-right">{(frac * 100).toFixed(1)}%</span>
                </div>
              )
            })}
          </div>
        </div>
      </div>
      {hoveredSlice && hover && hover.x > 0 && (
        <div
          className="pointer-events-none absolute bg-zinc-900/95 dark:bg-zinc-100/95 text-zinc-50 dark:text-zinc-900 mono text-[11px] rounded px-2 py-1 shadow-lg z-10"
          style={{ left: hover.x + 14, top: hover.y - 8 }}
        >
          <div>{hoveredSlice.label}</div>
          <div className="tabular-nums">{fv(hoveredSlice.value)} · {(hoveredFrac * 100).toFixed(1)}%</div>
          {hoveredSlice.meta && <div className="text-[10px] opacity-70">{hoveredSlice.meta}</div>}
        </div>
      )}
    </div>
  )
}

const PALETTE_8 = ['#a78bfa','#34d399','#60a5fa','#f59e0b','#ec4899','#22d3ee','#f87171','#84cc16']

/// Donut: spend per session in the window. Top 8 by spend + "other".
/// Hover surfaces the session id and (if we can match it to a current
/// job) the issue title + repo.
function UsageBySessionDonut({
  rows, days, jobs,
}: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  const jobByIssue = useMemo(() => {
    const m = new Map<number, Job>()
    for (const j of jobs) m.set(j.issue, j)
    return m
  }, [jobs])
  const slices = useMemo<DonutSlice[]>(() => {
    const by = new Map<string, { total: number; issue: number }>()
    for (const r of rows) {
      const cur = by.get(r.session_id) ?? { total: 0, issue: 0 }
      cur.total += tok(r)
      if (r.issue) cur.issue = r.issue
      by.set(r.session_id, cur)
    }
    const sorted = Array.from(by.entries()).sort((a, b) => b[1].total - a[1].total)
    const top = sorted.slice(0, 8)
    const rest = sorted.slice(8).reduce((acc, [_, v]) => acc + v.total, 0)
    const out: DonutSlice[] = top.map(([sid, v], i) => {
      const job = v.issue ? jobByIssue.get(v.issue) : undefined
      const title = job?.issue_title ?? (v.issue ? `issue #${v.issue}` : sid.slice(0, 8))
      return {
        key: sid,
        label: title,
        value: v.total,
        color: PALETTE_8[i],
        meta: v.issue ? `#${v.issue} · ${job?.target_repo ?? 'closed session'}` : `session ${sid.slice(0, 12)}`,
      }
    })
    if (rest > 0) out.push({ key: '__other__', label: 'other sessions', value: rest, color: '#71717a' })
    return out
  }, [rows, jobByIssue])
  const sessionCount = useMemo(() => new Set(rows.map((r) => r.session_id)).size, [rows])
  return (
    <Donut
      slices={slices}
      title={`Tokens by session · ${days}d`}
      subtitle={`${sessionCount} sessions`}
      fmt={fmtTok}
    />
  )
}

/// Donut: spend per upstream target repo. Session → issue → repo via
/// the same job map. Sessions whose issue was already torn down show
/// as "unknown" so the chart still totals correctly.
function UsageByRepoDonut({
  rows, days, jobs,
}: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  const jobByIssue = useMemo(() => {
    const m = new Map<number, Job>()
    for (const j of jobs) m.set(j.issue, j)
    return m
  }, [jobs])
  const slices = useMemo<DonutSlice[]>(() => {
    const by = new Map<string, number>()
    for (const r of rows) {
      const job = r.issue ? jobByIssue.get(r.issue) : undefined
      const repo = job?.target_repo ?? 'unknown'
      by.set(repo, (by.get(repo) ?? 0) + tok(r))
    }
    const sorted = Array.from(by.entries()).sort((a, b) => b[1] - a[1])
    return sorted.map(([repo, v], i) => ({
      key: repo,
      label: repo,
      value: v,
      color: repo === 'unknown' ? '#71717a' : PALETTE_8[i % PALETTE_8.length],
    }))
  }, [rows, jobByIssue])
  return (
    <Donut
      slices={slices}
      title={`Tokens by repo · ${days}d`}
      subtitle={`${slices.length} repos`}
      fmt={fmtTok}
    />
  )
}

// Legacy stub kept off to avoid dragging the prior bar implementation
// along. The donuts above replaced it entirely; callers shouldn't
// reach this.
function _legacyUsageBySessionChart({ rows, days, jobs }: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  type DayBar = { date: string; parts: Record<string, number>; total: number }
  // Resolve session_id → human label from active jobs first, then
  // fall back to a short prefix so closed sessions still render.
  const labelFor = useMemo(() => {
    const m = new Map<string, string>()
    for (const j of jobs) {
      const tmux = j.tmux || ''
      // statusline session_id ≠ tmux id; we don't have a direct map
      // here. Best-effort: use the issue title indexed by tmux. UI
      // shows the short hash if the session is no longer tracked.
      m.set(tmux, j.issue_title || tmux)
    }
    return (sid: string) => sid.slice(0, 6)
  }, [jobs])

  const { grid, topSessions, colors } = useMemo(() => {
    const totals = new Map<string, number>()
    for (const r of rows) {
      totals.set(r.session_id, (totals.get(r.session_id) ?? 0) + r.cost_usd)
    }
    const sorted = Array.from(totals.entries()).sort((a, b) => b[1] - a[1])
    const top = sorted.slice(0, 8).map((e) => e[0])
    const topSet = new Set(top)

    const today = new Date()
    const by = new Map<string, DayBar>()
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(today.getTime() - i * 86400_000)
      const k = d.toISOString().slice(0, 10)
      by.set(k, { date: k, parts: {}, total: 0 })
    }
    for (const r of rows) {
      const bar = by.get(r.date); if (!bar) continue
      const key = topSet.has(r.session_id) ? r.session_id : '__other__'
      bar.parts[key] = (bar.parts[key] ?? 0) + r.cost_usd
      bar.total += r.cost_usd
    }
    const palette = ['#a78bfa','#34d399','#60a5fa','#f59e0b','#ec4899','#22d3ee','#f87171','#84cc16','#71717a']
    const colors: Record<string, string> = { __other__: '#71717a' }
    top.forEach((sid, i) => { colors[sid] = palette[i] ?? '#a1a1aa' })
    return { grid: Array.from(by.values()), topSessions: top, colors }
  }, [rows, days])

  const max = Math.max(0.01, ...grid.map((g) => g.total))
  const total = grid.reduce((acc, g) => acc + g.total, 0)
  const W = 760, H = 200, pad = { l: 36, r: 12, t: 12, b: 22 }
  const innerW = W - pad.l - pad.r
  const innerH = H - pad.t - pad.b
  const barW = innerW / grid.length
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((p) => ({ y: pad.t + innerH - p * innerH, v: p * max }))

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
          Spend by session · last {days}d
        </div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          ${total.toFixed(2)} · top {topSessions.length} of {new Set(rows.map((r) => r.session_id)).size} sessions
        </div>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full">
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={pad.l} x2={W - pad.r} y1={t.y} y2={t.y} stroke="currentColor" className="text-zinc-200 dark:text-zinc-800" strokeDasharray={i === 0 ? '' : '2 3'} />
            <text x={pad.l - 6} y={t.y + 3} textAnchor="end" className="fill-zinc-500 dark:fill-zinc-400" fontSize="9">${t.v.toFixed(t.v < 1 ? 2 : 1)}</text>
          </g>
        ))}
        {grid.map((g, i) => {
          const x = pad.l + i * barW
          // Stack top sessions in their fixed order so colours stay
          // consistent across days, then append "other" on top.
          const order = [...topSessions, '__other__']
          let yCursor = pad.t + innerH
          return (
            <g key={g.date}>
              {order.map((sid) => {
                const v = g.parts[sid] ?? 0
                if (v <= 0) return null
                const h = (v / max) * innerH
                yCursor -= h
                return <rect key={sid} x={x + 1} y={yCursor} width={Math.max(0, barW - 2)} height={Math.max(0, h)} fill={colors[sid]} />
              })}
              {i % Math.ceil(days / 8) === 0 && (
                <text x={x + barW / 2} y={H - 6} textAnchor="middle" className="fill-zinc-500 dark:fill-zinc-400" fontSize="9">{g.date.slice(5)}</text>
              )}
              <title>{`${g.date}\n$${g.total.toFixed(2)}`}</title>
            </g>
          )
        })}
      </svg>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 mt-2 mono text-[10.5px] text-zinc-500 dark:text-zinc-400">
        {topSessions.map((sid) => (
          <span key={sid} className="inline-flex items-center gap-1" title={sid}>
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: colors[sid] }} />
            {labelFor(sid)}
          </span>
        ))}
        {Object.keys(colors).includes('__other__') && (
          <span className="inline-flex items-center gap-1">
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: colors['__other__'] }} />
            other
          </span>
        )}
      </div>
    </div>
  )
}

/// Rolled-up totals across a configurable window. Pulls the same
/// /api/usage_history payload and re-aggregates so the operator can
/// switch between day / week / month without a round trip.
function UsageRollups({ rows }: { rows: UsageHistoryRow[] }) {
  const today = new Date().toISOString().slice(0, 10)
  const windowSum = (days: number) => {
    const since = new Date(Date.now() - days * 86400_000).toISOString().slice(0, 10)
    return rows.filter((r) => r.date >= since && r.date <= today)
      .reduce((acc, r) => acc + tok(r), 0)
  }
  const card = (label: string, days: number) => {
    const v = windowSum(days)
    return (
      <div className="rounded-lg ring-1 ring-zinc-200 dark:ring-zinc-800 px-4 py-3 flex-1">
        <div className="text-[10.5px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-1">{label}</div>
        <div className="mono text-[20px] text-zinc-900 dark:text-zinc-100 tabular-nums">{fmtTok(v)} <span className="text-[12px] text-zinc-500 dark:text-zinc-400">tok</span></div>
      </div>
    )
  }
  return (
    <div className="flex items-stretch gap-3">
      {card('Today',   1)}
      {card('7 days',  7)}
      {card('30 days', 30)}
    </div>
  )
}

// AnalyticsPage — full-bleed top-level tab (like Sessions/Memory), compact and
// honest. Leads with per-account QUOTA (the real constraint on a subscription),
// then throughput in tok() (input+output+cache-writes; cache_read excluded — it
// re-counts the same prefix every turn and would read as billions). No dollar
// figures: subscriptions aren't billed per-token, so $ is noise here.
interface ActivityResp {
  days: number
  rows: { date: string; sessions: number; prs: number }[]
  by_repo: { repo: string; sessions: number; prs: number }[]
  total_sessions: number; total_prs: number
  today_sessions: number; today_prs: number
}

function AnalyticsPage({ state }: { state: State }) {
  const [days, setDays] = useState(30)
  const [act, setAct] = useState<ActivityResp | null>(null)
  useEffect(() => {
    let alive = true
    const load = () => fetch(`/api/activity?days=${days}`, { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : null).then((j) => { if (alive) setAct(j) }).catch(() => { if (alive) setAct(null) })
    load(); const id = setInterval(load, 60_000)
    return () => { alive = false; clearInterval(id) }
  }, [days])

  // per-account quota (the real constraint). Fall back to top-level quota as "claude".
  const accounts: [string, AgentMeter][] = state.agents && Object.keys(state.agents).length
    ? Object.entries(state.agents)
    : (state.quota ? [['claude', { quota: state.quota, governor: state.governor }]] : [])

  const rows = act?.rows ?? []
  const dayMax = Math.max(1, ...rows.map((r) => r.sessions))
  const repos = act?.by_repo ?? []
  const repoMax = Math.max(1, ...repos.map((r) => r.sessions))
  const today = new Date().toISOString().slice(0, 10)

  const section = (title: string, right?: React.ReactNode) => (
    <div className="flex items-center gap-2 mb-2">
      <span className="text-[11px] font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400">{title}</span>
      <div className="flex-1" />{right}
    </div>
  )
  const card = 'rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-950'

  const stat = (label: string, value: number, sub?: string) => (
    <div className={card + ' px-3 py-2.5'}>
      <div className="text-[10px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-0.5">{label}</div>
      <div className="mono text-[22px] text-zinc-900 dark:text-zinc-100 tabular-nums leading-tight">{value}</div>
      {sub && <div className="text-[11px] text-zinc-400 dark:text-zinc-500 mt-0.5">{sub}</div>}
    </div>
  )

  return (
    <div className="flex-1 min-w-0 w-full max-w-screen-xl mx-auto p-4 sm:p-6 flex flex-col gap-6">
      <div className="flex items-center gap-3">
        <h1 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Analytics</h1>
        <span className="mono text-[11px] px-1.5 py-0.5 rounded-full bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300 tabular-nums">{(state.jobs ?? []).filter((j) => !j.closed_state).length} active</span>
        <div className="flex-1" />
        <div className="flex items-center gap-1">
          {[7, 30, 90].map((d) => (
            <button key={d} onClick={() => setDays(d)}
              className={'mono text-[11px] px-2 py-1 rounded ' + (days === d
                ? 'bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-900'
                : 'text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800')}>{d}d</button>
          ))}
        </div>
      </div>

      {/* Accounts — quota is the real constraint */}
      <div>
        {section('Accounts')}
        {accounts.length === 0
          ? <div className={card + ' px-3 py-2.5 text-[12.5px] text-zinc-400'}>No quota reported by this plan.</div>
          : <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              {accounts.filter(([, m]) => m.quota).map(([name, m]) => (
                <QuotaStrip key={name} quota={m.quota!} governor={m.governor} label={name} stacked />
              ))}
            </div>}
      </div>

      {/* Activity — sessions started + PRs opened (one row per issue) */}
      <div>
        {section('Activity', <span className="mono text-[10px] text-zinc-400">one row per issue · resumes counted once</span>)}
        <div className="grid grid-cols-3 gap-3 mb-3">
          {stat('Today', act?.today_sessions ?? 0, `${act?.today_prs ?? 0} PRs opened`)}
          {stat(`Sessions · ${days}d`, act?.total_sessions ?? 0, 'issues worked')}
          {stat(`PRs · ${days}d`, act?.total_prs ?? 0, 'pull requests opened')}
        </div>
        <div className={card + ' px-3 py-3'}>
          <div className="flex items-end gap-[2px] h-20">
            {rows.map((d) => (
              <div key={d.date} title={`${d.date}: ${d.sessions} sessions, ${d.prs} PRs`}
                className="flex-1 min-w-0 bg-violet-500/60 hover:bg-violet-500 rounded-sm transition-colors"
                style={{ height: `${Math.max(2, (d.sessions / dayMax) * 100)}%` }} />
            ))}
          </div>
          <div className="flex justify-between mono text-[10px] text-zinc-400 mt-1.5">
            <span>{rows[0]?.date.slice(5)}</span><span>peak {dayMax}/d</span><span>{today.slice(5)}</span>
          </div>
        </div>
      </div>

      {/* By repo — sessions per work repo */}
      <div>
        {section('By repo', <span className="mono text-[10px] text-zinc-400">sessions · {days}d</span>)}
        <div className={card + ' divide-y divide-zinc-100 dark:divide-zinc-800/70'}>
          {repos.length === 0 && <div className="px-3 py-2.5 text-[12.5px] text-zinc-400">No activity in this window.</div>}
          {repos.slice(0, 8).map((r) => (
            <div key={r.repo} className="flex items-center gap-3 px-3 py-1.5">
              <span className="text-[12.5px] text-zinc-700 dark:text-zinc-300 truncate w-40 flex-shrink-0">{r.repo}</span>
              <div className="relative h-1.5 flex-1 rounded-full overflow-hidden bg-zinc-100 dark:bg-zinc-800">
                <div className="absolute inset-y-0 left-0 bg-violet-500/70" style={{ width: `${(r.sessions / repoMax) * 100}%` }} />
              </div>
              <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400 tabular-nums w-16 text-right">{r.sessions} · {r.prs} PR</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

// MachinesPage — full-bleed top-level tab (like Sessions/Memory/Analytics). A
// roster of worker VMs: health, agent, host, and a load bar (running/capacity).
// A host runs several worker slots (one VM block per agent/account), named
// like "mac-mini" / "mac-codex" / "mac-codex-mini". Derive a friendly machine
// name from their longest common prefix → "mac"; fall back to the host.
function machineName(slots: VM[]): string {
  const names = slots.map((s) => s.name)
  if (names.length === 1) return names[0]
  let p = names[0] ?? ''
  for (const n of names.slice(1)) {
    let i = 0
    while (i < p.length && i < n.length && p[i] === n[i]) i++
    p = p.slice(0, i)
  }
  p = p.replace(/[-_.\s]+$/, '')
  return p || slots[0]?.host || '—'
}

function MachineRowMenu({ address, name }: { address: string; name: string }) {
  const [open, setOpen] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)
  const copy = (text: string, what: string) => {
    navigator.clipboard.writeText(text).catch(() => {})
    setCopied(what)
    setTimeout(() => { setCopied(null); setOpen(false) }, 900)
  }
  return (
    <div className="relative">
      <button
        onClick={() => setOpen((o) => !o)}
        title="Machine actions"
        className="w-7 h-7 rounded-md flex items-center justify-center text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-700 dark:hover:text-zinc-200 transition-colors"
      >
        <svg width={16} height={16} viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="1.6" /><circle cx="12" cy="12" r="1.6" /><circle cx="19" cy="12" r="1.6" /></svg>
      </button>
      {open && (
        <>
          <div className="fixed inset-0 z-10" onClick={() => setOpen(false)} />
          <div className="absolute right-0 top-8 z-20 w-44 rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 shadow-xl py-1 text-[13px]">
            <button onClick={() => copy(address, 'addr')} className="w-full text-left px-3 py-1.5 hover:bg-zinc-100 dark:hover:bg-zinc-800 text-zinc-700 dark:text-zinc-200">{copied === 'addr' ? 'Copied' : 'Copy address'}</button>
            <button onClick={() => copy(name, 'name')} className="w-full text-left px-3 py-1.5 hover:bg-zinc-100 dark:hover:bg-zinc-800 text-zinc-700 dark:text-zinc-200">{copied === 'name' ? 'Copied' : 'Copy name'}</button>
          </div>
        </>
      )}
    </div>
  )
}

function MachinesPage({ state }: { state: State }) {
  const vms = state.vms ?? []
  // Group the config's VM blocks by physical host — each host runs several
  // worker "slots" (one block per agent/account). Operators think in machines.
  const hosts = useMemo(() => {
    const m = new Map<string, VM[]>()
    for (const v of vms) (m.get(v.host) ?? m.set(v.host, []).get(v.host)!).push(v)
    return [...m.entries()]
      .map(([host, slots]) => ({ host, slots: slots.slice().sort((a, b) => a.name.localeCompare(b.name)) }))
      .sort((a, b) => {
        const ao = a.slots.every((v) => v.online === false), bo = b.slots.every((v) => v.online === false)
        return (ao ? 1 : 0) - (bo ? 1 : 0) || a.host.localeCompare(b.host)
      })
  }, [vms])
  const onlineHosts = hosts.filter((h) => h.slots.some((v) => v.online !== false)).length
  const used = vms.reduce((a, v) => a + (v.used ?? 0), 0)
  const cap = vms.reduce((a, v) => a + (v.capacity ?? 0), 0)
  return (
    <div className="flex-1 min-w-0 w-full max-w-screen-xl mx-auto p-4 sm:p-6 flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <h1 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Machines</h1>
        <span className="mono text-[11px] px-1.5 py-0.5 rounded-full bg-zinc-200/80 dark:bg-zinc-700/70 text-zinc-600 dark:text-zinc-300 tabular-nums">{onlineHosts}/{hosts.length} online</span>
        <span className="mono text-[11px] text-zinc-400 tabular-nums">{vms.length} slots · {used}/{cap || '∞'} sessions</span>
      </div>
      {hosts.length === 0 ? (
        <div className="text-sm text-zinc-400">No machines connected yet.</div>
      ) : (
        <div className="rounded-xl border border-zinc-200 dark:border-zinc-800 overflow-hidden bg-white dark:bg-zinc-950">
          {/* column header (Tailscale-style) */}
          <div className="hidden md:flex items-center gap-4 px-5 py-2.5 bg-zinc-50 dark:bg-zinc-900/60 border-b border-zinc-200 dark:border-zinc-800 text-[10px] font-medium uppercase tracking-[0.12em] text-zinc-500 dark:text-zinc-400">
            <span className="flex-1 min-w-0">Machine</span>
            <span className="w-40 flex-shrink-0">Address</span>
            <span className="w-60 flex-shrink-0">Agents</span>
            <span className="w-32 flex-shrink-0">Sessions</span>
            <span className="w-8 flex-shrink-0" />
          </div>
          <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
            {hosts.map(({ host, slots }) => {
              const off = slots.every((v) => v.online === false)
              const u = slots.reduce((a, v) => a + (v.used ?? 0), 0)
              const c = slots.reduce((a, v) => a + (v.capacity ?? 0), 0)
              const load = c ? Math.min(100, (u / c) * 100) : 0
              const hot = load >= 90
              const err = slots.map((v) => v.last_err).find(Boolean)
              const owner = slots.map((v) => v.bot).find(Boolean)
              const name = machineName(slots)
              const os = slots.map((v) => v.os).find(Boolean) ?? (/mac|darwin|mini|book/i.test(name) ? 'Darwin' : 'Linux')
              return (
                <div key={host} className="flex flex-col md:flex-row md:items-center gap-3 md:gap-4 px-4 sm:px-5 py-4 hover:bg-zinc-50 dark:hover:bg-zinc-900/40 transition-colors">
                  {/* Machine: OS icon + name + owner + status pill */}
                  <div className="flex items-center gap-3 flex-1 min-w-0">
                    <OSIcon os={os} size={20} className={'flex-shrink-0 ' + (off ? 'text-zinc-300 dark:text-zinc-600' : 'text-zinc-700 dark:text-zinc-200')} />
                    <div className="min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className={'text-[14px] font-semibold truncate ' + (off ? 'text-zinc-400 dark:text-zinc-500' : 'text-zinc-900 dark:text-zinc-100')}>{name}</span>
                        <span className={'inline-flex items-center gap-1 text-[11px] font-medium ' + (off ? 'text-zinc-400 dark:text-zinc-500' : 'text-emerald-600 dark:text-emerald-400')}>
                          <span className={'w-1.5 h-1.5 rounded-full ' + (off ? 'bg-zinc-300 dark:bg-zinc-600' : 'bg-emerald-500')} />
                          {off ? 'Offline' : 'Connected'}
                        </span>
                        {err && <span className="mono text-[10px] px-1.5 py-0.5 rounded bg-rose-50 text-rose-600 dark:bg-rose-500/15 dark:text-rose-300 truncate max-w-[220px]" title={err}>{err}</span>}
                      </div>
                      <div className="text-[12px] text-zinc-500 dark:text-zinc-400 truncate mt-0.5">
                        {owner ? <>{owner}<span className="text-zinc-300 dark:text-zinc-600"> · </span></> : null}
                        {slots.length} slot{slots.length === 1 ? '' : 's'}
                      </div>
                    </div>
                  </div>
                  {/* Address */}
                  <div className="w-40 flex-shrink-0 mono text-[12px] text-zinc-600 dark:text-zinc-300 truncate pl-5 md:pl-0" title={host}>{host}</div>
                  {/* Agents: one clean chip per slot — provider mark · name · count */}
                  <div className="w-60 flex-shrink-0 flex flex-wrap items-center gap-1.5 pl-5 md:pl-0">
                    {slots.map((v) => (
                      <span key={v.name} title={`${v.name} · capacity ${v.capacity || '∞'}`} className="inline-flex items-center gap-1.5 text-[11.5px] pl-1.5 pr-1 py-0.5 rounded-full border border-zinc-200 dark:border-zinc-700/80 bg-white dark:bg-zinc-900">
                        <AgentLogo account={v.agent ?? 'claude'} size={12} className="text-zinc-700 dark:text-zinc-200" />
                        <span className="text-zinc-700 dark:text-zinc-200">{v.agent ?? 'claude'}</span>
                        <span className="mono text-[10px] tabular-nums text-zinc-500 dark:text-zinc-400 bg-zinc-100 dark:bg-zinc-800 rounded-full px-1.5 min-w-[18px] text-center">{v.capacity || '∞'}</span>
                      </span>
                    ))}
                  </div>
                  {/* Sessions: used/cap + load bar */}
                  <div className="w-32 flex-shrink-0 flex items-center gap-2 pl-5 md:pl-0">
                    <div className="relative h-1.5 flex-1 rounded-full overflow-hidden bg-zinc-200 dark:bg-zinc-800">
                      <div className={'absolute inset-y-0 left-0 ' + (hot ? 'bg-rose-500/80' : 'bg-emerald-500/80')} style={{ width: `${load}%` }} />
                    </div>
                    <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400 tabular-nums">{u}/{c || '∞'}</span>
                  </div>
                  {/* Action menu */}
                  <div className="w-8 flex-shrink-0 hidden md:flex justify-end">
                    <MachineRowMenu address={host} name={name} />
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}

// Settings → Usage tab. Sorted by spend desc so the most expensive
// sessions float to the top. Quota strip up top mirrors the
// (hidden-on-some-accounts) header chip so operators can see the
// 5h / 7d numbers when their plan exposes them.
function UsageTable({ jobs, quota, governor }: { jobs: Job[]; quota?: State['quota']; governor?: State['governor'] }) {
  const rows = useMemo(() => {
    return jobs
      .filter((j) => j.usage)
      .slice()
      .sort((a, b) => (b.usage?.context_pct ?? 0) - (a.usage?.context_pct ?? 0))
  }, [jobs])
  const [history, setHistory] = useState<UsageHistoryRow[] | null>(null)
  const [days, setDays] = useState(30)
  useEffect(() => {
    let alive = true
    fetch(`/api/usage_history?days=${days}`, { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : null)
      .then((j) => { if (alive) setHistory(j?.rows ?? []) })
      .catch(() => { if (alive) setHistory([]) })
    const id = setInterval(() => {
      fetch(`/api/usage_history?days=${days}`, { credentials: 'include', cache: 'no-store' })
        .then((r) => r.ok ? r.json() : null)
        .then((j) => { if (alive) setHistory(j?.rows ?? []) })
        .catch(() => {})
    }, 60_000)
    return () => { alive = false; clearInterval(id) }
  }, [days])
  return (
    <div className="space-y-6">
      {history && history.length > 0 && (
        <>
          <UsageRollups rows={history} />
          <div className="flex items-center justify-end gap-1">
            {[7, 30, 90].map((d) => (
              <button
                key={d}
                onClick={() => setDays(d)}
                className={
                  'mono text-[11px] px-2 py-1 rounded ' +
                  (days === d
                    ? 'bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-900'
                    : 'text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800')
                }
              >
                {d}d
              </button>
            ))}
          </div>
          <UsageChart rows={history} days={days} />
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
            <UsageBySessionDonut rows={history} days={days} jobs={jobs} />
            <UsageByRepoDonut    rows={history} days={days} jobs={jobs} />
          </div>
        </>
      )}
      {quota ? (
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
          <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-3">
            Subscription quota
          </div>
          <QuotaStrip quota={quota} governor={governor} />
        </div>
      ) : (
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
          <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-2">
            Subscription quota
          </div>
          <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
            Not reported by Claude on this account / plan. Token
            throughput and live context still update below — they're
            parsed from the same statusline feed but don't depend on the
            optional <code className="mono">rate_limits</code> field.
          </div>
        </div>
      )}
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 overflow-hidden">
        <div className="flex items-center px-4 py-3 bg-zinc-50 dark:bg-zinc-900/60 border-b border-zinc-200 dark:border-zinc-800">
          <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
            Live context window
          </div>
          <div className="flex-1" />
          <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
            {rows.length} active
          </div>
        </div>
        {rows.length === 0 && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No statusline samples yet. Sessions report as soon as their next render tick lands.
          </div>
        )}
        <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
          {rows.map((j) => {
            const ctx = j.usage?.context_pct
            const repo = j.target_repo ? j.target_repo.split('/')[1] : j.target || '—'
            return (
              <div key={j.tmux} className="flex items-center gap-3 px-4 py-3">
                <div className="flex-1 min-w-0">
                  <div className="text-[13px] text-zinc-900 dark:text-zinc-100 truncate">
                    {j.issue_title || j.tmux}
                  </div>
                  <div className="mono text-[11px] text-zinc-500 dark:text-zinc-400 truncate">
                    {repo} · {j.tmux}
                    {j.usage?.model ? ' · ' + j.usage.model : ''}
                  </div>
                </div>
                {typeof ctx === 'number' && (
                  <div className="flex items-center gap-1.5 flex-shrink-0">
                    <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400">ctx</span>
                    <div className="relative h-1.5 w-16 rounded-full overflow-hidden bg-zinc-200 dark:bg-zinc-800">
                      <div
                        className="absolute inset-y-0 left-0 bg-violet-500/80"
                        style={{ width: `${Math.min(100, Math.max(0, ctx))}%` }}
                      />
                    </div>
                    <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400 tabular-nums w-8">
                      {Math.round(ctx)}%
                    </span>
                  </div>
                )}
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

// The Sessions tab's empty state before any machine has joined — like GitHub's
// "Quick setup" screen on a fresh repo: the setup instructions ARE the page
// (no list, no sidebar, no modal). Replaces the old full-screen InstallModal.
function FirstJoinSetup({ relay }: { relay: RelayInfo | null }) {
  if (!relay || relay.connected || !relay.login || !relay.token) return null
  return (
    <div className="w-full max-w-2xl mx-auto px-4 sm:px-6 py-10 sm:py-14">
      <div className="flex items-center gap-2 mb-2">
        <span className="w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
        <span className="mono text-[10.5px] uppercase tracking-[0.18em] text-zinc-500 dark:text-zinc-400">Waiting for your first machine</span>
      </div>
      <h1 className="text-[22px] sm:text-[26px] font-semibold tracking-tight text-zinc-900 dark:text-zinc-100">Connect your machine</h1>
      <p className="mt-2.5 text-[13.5px] leading-relaxed text-zinc-500 dark:text-zinc-400 max-w-xl">
        Orchid runs the swarm on machines you own. Run these two commands on the
        box that should host it — a Linux server or your Mac. The moment it dials
        in, your sessions show up right here.
      </p>
      <div className="mt-6">
        <JoinCommandCard info={relay} />
      </div>
      <p className="mt-5 text-[12.5px] text-zinc-400 dark:text-zinc-500">
        First time? Read the{' '}
        <a href="/docs/getting-started" className="text-violet-600 dark:text-violet-400 hover:underline">getting started guide</a>.
      </p>
    </div>
  )
}

function JoinCommandCard({ info }: { info: RelayInfo | null | 'unavailable' }) {
  // Two branches:
  //   - Relay-managed orch: build the install + join commands from the
  //     subdomain + agent token in /api/_relay/info. Same shape as the
  //     first-run InstallModal so the muscle memory carries over.
  //   - Local orch (no relay, or relay endpoint missing): there's no
  //     network identity for a fresh VM to dial into, so we point
  //     the operator at swarm.hcl instead.
  if (info === null) {
    return (
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400 mb-3">Connect a machine</div>
        <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400">Loading…</div>
      </div>
    )
  }
  if (info === 'unavailable' || !info.login || !info.token) {
    return (
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6 space-y-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">Connect a machine</div>
        <div className="text-[13px] text-zinc-700 dark:text-zinc-300 leading-relaxed">
          This orch is running standalone — there's no relay endpoint for a
          new VM to join through.
        </div>
        <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
          Add a <code className="mono">vm "&lt;name&gt;" {`{ … }`}</code> block to{' '}
          <code className="mono">swarm.hcl</code> and restart orchid. To switch to
          a relay-managed orch, sign in at{' '}
          <code className="mono">orchid.littledivy.com</code> and run{' '}
          <code className="mono">orch join</code> with the issued token.
        </div>
      </div>
    )
  }

  const sub = info.login.toLowerCase().replace(/[^a-z0-9-]/g, '')
  // ROOT_DOMAIN comes from the relay so multi-label roots like
  // orchid.littledivy.com don't get truncated to littledivy.com by a
  // naive slice(-2). Falls back to hostname for older relays missing
  // the field.
  const root = info.root ?? location.hostname.split('.').slice(-2).join('.')
  const install = `curl -fsSL https://${root}/install.sh | bash`
  const join = `orch join wss://${sub}.${root}/agent ${info.token}`
  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6 space-y-5">
      <div className="flex items-center justify-between">
        <div className="text-[12px] uppercase tracking-wider text-zinc-500 dark:text-zinc-400">Connect a machine</div>
        <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400">SSH into the new box as root</div>
      </div>
      <JoinStep n={1} label="Install orch">
        <JoinCmd value={install} />
      </JoinStep>
      <JoinStep n={2} label="Join this orch">
        <JoinCmd value={join} secret />
      </JoinStep>
      <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
        The join token grants this orch's worker pool — treat it like a password.
        Rotate it from the <span className="italic">Danger zone</span> tab if it leaks.
      </div>
    </div>
  )
}

function JoinStep({ n, label, children }: { n: number; label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="flex items-center gap-2 mb-1.5">
        <span className="mono text-[10.5px] text-zinc-500 dark:text-zinc-400">{n}.</span>
        <span className="text-[12px] text-zinc-600 dark:text-zinc-300">{label}</span>
      </div>
      {children}
    </div>
  )
}

function JoinCmd({ value, secret }: { value: string; secret?: boolean }) {
  const [copied, setCopied] = useState(false)
  const [revealed, setRevealed] = useState(!secret)
  // Mask the trailing token. The command structure stays visible so the
  // operator can sanity-check the URL before pasting.
  const display = revealed ? value : value.replace(/(\S+)$/, (m) => m.replace(/./g, '•'))
  return (
    <div className="relative group">
      <pre className="bg-zinc-950 text-zinc-100 mono text-[12px] p-3 pr-24 rounded-lg whitespace-pre-wrap break-all">{display}</pre>
      <div className="absolute top-2 right-2 flex items-center gap-1">
        {secret && (
          <button
            onClick={() => setRevealed((v) => !v)}
            className="mono text-[10.5px] px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-300"
          >{revealed ? 'hide' : 'show'}</button>
        )}
        <button
          onClick={() => {
            navigator.clipboard.writeText(value).catch(() => {})
            setCopied(true)
            setTimeout(() => setCopied(false), 1200)
          }}
          className="mono text-[10.5px] px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-300 opacity-80 group-hover:opacity-100 transition-opacity"
        >{copied ? 'copied' : 'copy'}</button>
      </div>
    </div>
  )
}

function TargetsList({ targets, setTargets, repos, reposError }: {
  targets: TargetCfg[]
  setTargets: React.Dispatch<React.SetStateAction<TargetCfg[]>>
  repos: RepoOption[] | null
  reposError?: string | null
}) {
  const [adding, setAdding] = useState(false)
  const repoBy = useMemo(() => {
    const m = new Map<string, RepoOption>()
    for (const r of repos ?? []) m.set(r.full_name, r)
    return m
  }, [repos])

  return (
    <div>
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
        {targets.length === 0 && !adding && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No targets yet. Add a repo below to wire one up.
          </div>
        )}
        {targets.map((t, i) => {
          const repo = t.repo ? repoBy.get(t.repo) : undefined
          const [owner, name] = (t.repo ?? '').split('/')
          const avatar = repo?.avatar ?? (owner ? `https://github.com/${owner}.png?size=80` : undefined)
          return (
            <div key={t.name + i} className="flex items-center gap-3 px-4 py-3 group">
              {avatar ? (
                <img src={avatar} alt="" className="w-8 h-8 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />
              ) : (
                <div className="w-8 h-8 rounded-md bg-zinc-100 dark:bg-zinc-800 flex-shrink-0" />
              )}
              <div className="text-[13.5px] truncate flex-1 min-w-0">
                <span className="text-zinc-500 dark:text-zinc-400">{owner || '—'}</span>
                <span className="text-zinc-300 dark:text-zinc-600 mx-0.5">/</span>
                <span className="mono text-zinc-900 dark:text-zinc-100">{name || '—'}</span>
              </div>
              <input
                value={t.name}
                onChange={(e) => setTargets((arr) => arr.map((x, j) => j === i ? { ...x, name: e.target.value } : x))}
                placeholder="label"
                className="mono text-[12px] w-24 px-2 py-1 rounded bg-transparent outline-none text-zinc-500 dark:text-zinc-400 focus:bg-zinc-50 dark:focus:bg-zinc-900 focus:text-zinc-900 dark:focus:text-zinc-100 text-right"
                title="Label used in the inbox to route to this target"
              />
              <button
                onClick={() => setTargets((arr) => arr.filter((_, j) => j !== i))}
                className="text-[14px] text-zinc-400 hover:text-rose-600 opacity-0 group-hover:opacity-100 transition-opacity"
                title="remove"
              >×</button>
            </div>
          )
        })}
        {adding && (
          <div className="px-4 py-3 bg-zinc-50 dark:bg-zinc-950">
            <RepoPicker
              value=""
              onChange={(repo) => {
                if (!repo) return
                const label = (repo.split('/').pop() ?? '').toLowerCase().replace(/[^a-z0-9-_]/g, '-')
                setTargets((arr) => [...arr, { name: label, repo }])
                setAdding(false)
              }}
              repos={repos}
              error={reposError}
              placeholder="pick a repo to add as a target"
            />
          </div>
        )}
      </div>
      <button
        onClick={() => setAdding((a) => !a)}
        className="mt-3 mono text-[12px] px-3 py-1.5 rounded-md ring-1 ring-zinc-300 dark:ring-zinc-700 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-100 dark:hover:bg-zinc-800"
      >{adding ? 'cancel' : '+ add target'}</button>
    </div>
  )
}

function AllowedUsers({ values, onChange }: { values: string[]; onChange: (v: string[]) => void }) {
  const [owner, setOwner] = useState<string | null>(null)
  useEffect(() => {
    let alive = true
    fetch('/api/_relay/info', { credentials: 'include' })
      .then((r) => r.ok ? r.json() : null)
      .then((j) => { if (alive && j?.login) setOwner(j.login) })
      .catch(() => {})
    return () => { alive = false }
  }, [])
  // De-dupe: if the owner was also added to allowed_logins by hand,
  // don't render them twice. Owner always pinned at the top.
  const collaborators = useMemo(() => {
    if (!owner) return values
    return values.filter((v) => v.toLowerCase() !== owner.toLowerCase())
  }, [values, owner])
  const profiles = useGhProfiles([owner, ...collaborators].filter(Boolean) as string[])
  const [draft, setDraft] = useState('')
  const add = () => {
    const v = draft.trim().replace(/^@/, '')
    if (!v || values.includes(v)) { setDraft(''); return }
    onChange([...values, v])
    setDraft('')
  }
  return (
    <div className="space-y-3">
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
        {owner && (
          <UserRow login={owner} profile={profiles.get(owner.toLowerCase())} owner />
        )}
        {collaborators.length === 0 && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No collaborators yet. Add a GitHub login below to share access.
          </div>
        )}
        {collaborators.map((login) => (
          <UserRow
            key={login}
            login={login}
            profile={profiles.get(login.toLowerCase())}
            onRemove={() => onChange(values.filter((v) => v !== login))}
          />
        ))}
      </div>
      <div className="flex items-center gap-2 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-lg px-3 py-2">
        <span className="text-zinc-500 dark:text-zinc-400 text-[14px]">@</span>
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add() }
          }}
          onBlur={add}
          placeholder="github-login"
          spellCheck={false}
          autoComplete="off"
          className="mono flex-1 bg-transparent outline-none text-[13px] text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
        />
        <button
          onClick={add}
          disabled={!draft.trim()}
          className="mono text-[11.5px] px-3 py-1 rounded bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 disabled:opacity-30 disabled:cursor-not-allowed hover:opacity-90"
        >add</button>
      </div>
    </div>
  )
}

function UserRow({ login, profile, owner, onRemove }: {
  login: string
  profile?: GhProfile | 'loading' | 'missing'
  owner?: boolean
  onRemove?: () => void
}) {
  const p: GhProfile | null = profile && typeof profile === 'object' ? profile : null
  return (
    <div className="flex items-center gap-3 px-4 py-3 group">
      <img
        src={p?.avatar_url ?? `https://github.com/${login}.png?size=80`}
        alt=""
        className="w-8 h-8 rounded-full ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0"
        onError={(e) => { (e.currentTarget as HTMLImageElement).src = `https://github.com/identicons/${encodeURIComponent(login)}.png` }}
      />
      <a
        href={`https://github.com/${login}`}
        target="_blank"
        rel="noopener noreferrer"
        className="mono text-[13.5px] text-zinc-900 dark:text-zinc-100 hover:underline truncate flex-1"
      >@{login}</a>
      {p?.name && (
        <span className="text-[12px] text-zinc-500 dark:text-zinc-400 truncate hidden sm:inline">{p.name}</span>
      )}
      {owner && (
        <span className="text-[11px] text-zinc-500 dark:text-zinc-400">you</span>
      )}
      {profile === 'missing' && (
        <span className="text-[11px] text-rose-500 dark:text-rose-400">not found</span>
      )}
      {onRemove && (
        <button
          onClick={onRemove}
          className="text-[14px] text-zinc-400 hover:text-rose-600 opacity-0 group-hover:opacity-100 transition-opacity"
          title="remove"
        >×</button>
      )}
    </div>
  )
}

function ChipList({ values, onChange, placeholder }: { values: string[]; onChange: (v: string[]) => void; placeholder?: string }) {
  const [draft, setDraft] = useState('')
  const add = () => {
    const v = draft.trim()
    if (!v) return
    if (values.includes(v)) { setDraft(''); return }
    onChange([...values, v])
    setDraft('')
  }
  return (
    <div className="flex flex-wrap gap-2 items-center bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-md px-3 py-2">
      {values.map((v) => (
        <span key={v} className="mono inline-flex items-center gap-1.5 text-[12px] bg-zinc-200/80 dark:bg-zinc-800 text-zinc-800 dark:text-zinc-200 rounded px-2 py-0.5">
          {v}
          <button
            onClick={() => onChange(values.filter((x) => x !== v))}
            className="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200"
            title="Remove"
          >×</button>
        </span>
      ))}
      <input
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add() }
          else if (e.key === 'Backspace' && draft === '' && values.length > 0) {
            onChange(values.slice(0, -1))
          }
        }}
        onBlur={add}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
        className="mono flex-1 min-w-[120px] bg-transparent outline-none text-[12.5px] text-zinc-900 dark:text-zinc-100"
      />
    </div>
  )
}

type SortKey = 'newest' | 'oldest' | 'active' | 'attention'
const SORT_LABEL: Record<SortKey, string> = {
  newest: 'Newest', oldest: 'Oldest', active: 'Recently active', attention: 'Needs attention',
}
const PER_PAGE = 25

function ListView({ jobs, q, onOpen }: {
  jobs: Job[]
  q: string
  onOpen: (tmux: string) => void
}) {
  const activity = React.useContext(ActivityContext)
  const [sort, setSort] = useState<SortKey>('newest')
  const [page, setPage] = useState(0)

  const rows = useMemo(() => {
    const ts = (j: Job) => (j.spawned_at ? Date.parse(j.spawned_at) || 0 : 0)
    // Live sessions + deduped ghosts (one per issue) in one set.
    const byIssue = new Map<number, Job>()
    const live: Job[] = []
    for (const j of jobs) {
      if (j.closed_state) {
        const p = byIssue.get(j.issue)
        if (!p || (j.closed_at ?? 0) > (p.closed_at ?? 0)) byIssue.set(j.issue, j)
      } else if (j.tmux) {
        live.push(j)
      }
    }
    let all = [...live, ...byIssue.values()]
    const term = q.trim().toLowerCase()
    if (term) {
      all = all.filter((j) =>
        (j.issue_title || '').toLowerCase().includes(term) ||
        (j.target_repo || '').toLowerCase().includes(term) ||
        String(j.issue).includes(term) ||
        (j.tmux || '').toLowerCase().includes(term),
      )
    }
    const rank = (j: Job) => (j.closed_state ? 4 : { 'needs-you': 0, 'working': 1, 'watching': 2, 'quiet': 3 }[attention(j).level])
    all.sort((a, b) => {
      if (sort === 'oldest') return ts(a) - ts(b)
      if (sort === 'active') return (activity.at.get(b.tmux ?? '') ?? 0) - (activity.at.get(a.tmux ?? '') ?? 0) || ts(b) - ts(a)
      if (sort === 'attention') return rank(a) - rank(b) || ts(b) - ts(a)
      return ts(b) - ts(a) // newest
    })
    return all
  }, [jobs, q, sort, activity])

  const pages = Math.max(1, Math.ceil(rows.length / PER_PAGE))
  const cur = Math.min(page, pages - 1)
  const slice = rows.slice(cur * PER_PAGE, cur * PER_PAGE + PER_PAGE)

  return (
    <div className="flex flex-col">
      {/* List — flush to the container edges (rows carry their own padding) */}
      <div>
        {rows.length === 0 ? (
          <div className="py-24 text-center text-zinc-500 dark:text-zinc-400">
            <div className="serif italic text-[28px] mb-2">{q ? 'No matches' : 'Empty'}</div>
            <div className="text-[13px]">{q ? 'Try a different search.' : 'Open an issue in your inbox repo to spawn a session.'}</div>
          </div>
        ) : (
          <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
            {slice.map((job) => (
              <SessionRow
                key={(job.closed_state ? 'done-' : '') + (job.tmux || String(job.issue))}
                job={job}
                onOpen={onOpen}
                activityAt={activity.at.get(job.tmux ?? '')}
              />
            ))}
          </div>
        )}
      </div>

      {/* Pagination */}
      {pages > 1 && (
        <div className="flex items-center justify-between px-3 py-2 border-t border-zinc-200 dark:border-zinc-800 flex-shrink-0 mono text-[11px] text-zinc-500 dark:text-zinc-400">
          <span className="tabular-nums">{cur * PER_PAGE + 1}–{Math.min((cur + 1) * PER_PAGE, rows.length)} of {rows.length}</span>
          <div className="flex items-center gap-3">
            <button disabled={cur === 0} onClick={() => setPage(cur - 1)} className="disabled:opacity-30 hover:text-zinc-800 dark:hover:text-zinc-200">‹ Prev</button>
            <span className="tabular-nums">{cur + 1} / {pages}</span>
            <button disabled={cur >= pages - 1} onClick={() => setPage(cur + 1)} className="disabled:opacity-30 hover:text-zinc-800 dark:hover:text-zinc-200">Next ›</button>
          </div>
        </div>
      )}
    </div>
  )
}

// Octicon-style git glyphs (GitHub's own paths, 16-grid, currentColor).
const OCTICON_PR =
  'M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z'
const OCTICON_MERGE =
  'M5.45 5.154A4.25 4.25 0 0 0 9.25 7.5h1.378a2.251 2.251 0 1 1 0 1.5H9.25A5.734 5.734 0 0 1 5 7.123v3.505a2.25 2.25 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.95-.218ZM4.25 13.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Zm8.5-4.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5ZM5 3.25a.75.75 0 1 0 0 .005V3.25Z'

function Octicon({ d, className = '' }: { d: string; className?: string }) {
  return (
    <svg width={15} height={15} viewBox="0 0 16 16" fill="currentColor" className={className}>
      <path d={d} />
    </svg>
  )
}

// PrStatusIcon: a GitHub-familiar glyph for the session's PR/CI state, and a
// link to the PR itself. merged → purple merge; PR + CI fail → red ✗ ; PR + CI
// pass → green ✓ ; PR open (CI pending) → green PR glyph; no PR yet → a small
// "building" dot (pulses while active).
function PrStatusIcon({ job, active }: { job: Job; active: boolean }) {
  const ci = ciStatus(job.last_check_conclusions ?? {})
  let glyph: React.ReactNode
  let label = ''
  if (job.closed_state === 'merged') {
    glyph = <Octicon d={OCTICON_MERGE} className="text-violet-500" />
    label = 'merged'
  } else if (job.closed_state === 'closed') {
    glyph = (
      <svg width={15} height={15} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} className="text-zinc-400" strokeLinecap="round">
        <circle cx="12" cy="12" r="9" /><line x1="9" y1="9" x2="15" y2="15" /><line x1="15" y1="9" x2="9" y2="15" />
      </svg>
    )
    label = 'closed'
  } else if (job.pr && ci === 'fail') {
    glyph = (
      <svg width={15} height={15} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} className="text-rose-500" strokeLinecap="round">
        <circle cx="12" cy="12" r="9" /><line x1="9" y1="9" x2="15" y2="15" /><line x1="15" y1="9" x2="9" y2="15" />
      </svg>
    )
    label = 'CI failing'
  } else if (job.pr && ci === 'pass') {
    glyph = (
      <svg width={15} height={15} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} className="text-emerald-500" strokeLinecap="round" strokeLinejoin="round">
        <circle cx="12" cy="12" r="9" /><polyline points="8.5 12.5 11 15 15.5 9.5" />
      </svg>
    )
    label = 'CI passing'
  } else if (job.pr) {
    glyph = <Octicon d={OCTICON_PR} className="text-emerald-500" />
    label = 'PR open'
  } else {
    glyph = <span className={'block w-2.5 h-2.5 rounded-full bg-sky-500 ' + (active ? 'animate-pulse' : '')} />
    label = 'building — no PR yet'
  }
  const cls = 'flex-shrink-0 flex items-center justify-center w-5 h-5'
  if (job.pr && job.target_repo) {
    return (
      <a
        href={`https://github.com/${job.target_repo}/pull/${job.pr}`}
        target="_blank"
        rel="noreferrer"
        onClick={(e) => e.stopPropagation()}
        className={cls + ' hover:opacity-70'}
        title={`${label} — open PR #${job.pr}`}
      >
        {glyph}
      </a>
    )
  }
  return <span className={cls} title={label}>{glyph}</span>
}

// SessionRow: one compact row for the unified list. Live sessions are buttons
// (open the pane); merged/closed ghosts are dimmed static rows. The PR-status
// glyph links to the PR. needs-action rows get a rose left-accent + tag.
function SessionRow({ job, onOpen, activityAt }: {
  job: Job
  onOpen: (tmux: string) => void
  activityAt?: number
}) {
  const ghost = !!job.closed_state
  let attn = attention(job)
  if (!ghost && activityAt && Date.now() - activityAt < ACTIVITY_HOLD_MS && !job.needs_input) {
    attn = { ...attn, level: 'working', reason: 'active' }
  }
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const agent = job.tmux?.toLowerCase().startsWith('codex') ? 'codex' :
    job.tmux?.toLowerCase().startsWith('claude') ? 'claude' : 'unknown'
  const isActive = !ghost && attn.level === 'working'
  const needsAction = !ghost && attn.level === 'needs-you'
  const age = fmtAgo(job.spawned_at)

  const body = (
    <>
      <PrStatusIcon job={job} active={isActive} />
      <div className="flex-1 min-w-0">
        <div
          className={
            'truncate text-[14px] leading-snug ' +
            (ghost
              ? 'text-zinc-500 dark:text-zinc-400'
              : 'text-zinc-900 dark:text-zinc-100 ' + (needsAction ? 'font-semibold' : 'font-medium'))
          }
        >
          {job.issue_title || job.tmux || '—'}
        </div>
        <div className="mt-0.5 mono text-[11.5px] text-zinc-500 dark:text-zinc-400 flex items-center gap-1.5 truncate">
          <AgentMark agent={agent as Agent} />
          <span>{repo}</span>
          {ghost ? (
            <span className={job.closed_state === 'merged' ? 'text-violet-500' : 'text-zinc-500 dark:text-zinc-400'}>{job.closed_state}</span>
          ) : (
            <>
              {age && <><span className="text-zinc-300 dark:text-zinc-600">·</span><span>{age}</span></>}
              {needsAction && (
                <span className="mono text-[9px] uppercase tracking-wide px-1.5 py-px rounded-full bg-rose-100 text-rose-600 dark:bg-rose-900/40 dark:text-rose-300">
                  {job.needs_input ? 'needs input' : attn.reason}
                </span>
              )}
            </>
          )}
        </div>
      </div>
    </>
  )

  if (ghost) {
    return <div className="flex items-center gap-3 py-2.5 px-3 opacity-55 hover:opacity-90 transition-opacity">{body}</div>
  }
  return (
    <button
      onClick={() => job.tmux && onOpen(job.tmux)}
      className={
        'group w-full text-left flex items-center gap-3 py-2.5 px-3 border-l-2 transition-colors ' +
        (needsAction
          ? 'border-rose-400 bg-rose-50/40 dark:bg-rose-950/15 hover:bg-rose-50 dark:hover:bg-rose-950/25'
          : 'border-transparent hover:bg-zinc-50/80 dark:hover:bg-zinc-900/40')
      }
    >
      {body}
    </button>
  )
}

// fmtAgo renders a compact relative duration since an RFC3339 timestamp.
function fmtAgo(iso?: string): string | null {
  if (!iso) return null
  const t = Date.parse(iso)
  if (!t || Number.isNaN(t)) return null
  const s = Math.max(0, (Date.now() - t) / 1000)
  if (s < 90) return `${Math.round(s)}s`
  const m = s / 60
  if (m < 90) return `${Math.round(m)}m`
  const h = m / 60
  if (h < 36) return `${Math.round(h)}h`
  return `${Math.round(h / 24)}d`
}

function PaneModal({ tmux, jobsByTmuxRef, onClose }: {
  tmux: string
  jobsByTmuxRef: React.MutableRefObject<Map<string, Job>>
  onClose: () => void
}) {
  const [zoom, setZoom] = useState(false)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { if (zoom) setZoom(false); else onClose() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, zoom])
  const job = jobsByTmuxRef.current.get(tmux)
  const ci = job ? ciStatus(job.last_check_conclusions ?? {}) : 'pending'
  const title = job?.issue_title || tmux
  return (
    <div
      className={`fixed inset-0 z-50 bg-black/40 backdrop-blur-sm ${zoom ? '' : 'flex items-center justify-center p-2 sm:p-6'}`}
      onClick={zoom ? undefined : onClose}
    >
      <div
        className={
          zoom
            ? 'absolute inset-0 overflow-hidden shadow-2xl flex flex-col bg-[#0b0b0e]'
            : 'relative w-full max-w-[1200px] h-[92vh] sm:h-[80vh] rounded-lg overflow-hidden shadow-2xl ring-1 ring-black/40 flex flex-col bg-[#0b0b0e]'
        }
        onClick={(e) => e.stopPropagation()}
      >
        <div className="h-8 bg-zinc-800/95 flex items-center px-3 gap-3 select-none flex-shrink-0">
          <div className="flex gap-1.5 flex-shrink-0">
            <button
              onClick={onClose}
              className="w-3 h-3 rounded-full bg-rose-500 hover:bg-rose-400 transition-colors"
              title="close (esc)"
            />
            <span className="w-3 h-3 rounded-full bg-amber-400" />
            <button
              onClick={() => setZoom((z) => !z)}
              className="w-3 h-3 rounded-full bg-emerald-500 hover:bg-emerald-400 transition-colors"
              title={zoom ? 'restore' : 'fullscreen'}
            />
          </div>
          <div className="flex-1 min-w-0 text-center text-[12px] text-zinc-300 truncate">
            {title}
          </div>
          <div className="flex items-center gap-2 flex-shrink-0">
            {job?.pr && job?.target_repo && (
              <PRBadge repo={job.target_repo} pr={job.pr} ci={ci} />
            )}
          </div>
        </div>
        <div className="flex-1 min-h-0">
          <Pane session={tmux} />
        </div>
      </div>
    </div>
  )
}


function LogoutButton() {
  return (
    <a
      href="/logout"
      title="Log out"
      className="w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
        <polyline points="16 17 21 12 16 7" />
        <line x1="21" y1="12" x2="9" y2="12" />
      </svg>
    </a>
  )
}

function FloatingComposer({ at, onDismiss }: { at: { x: number; y: number }; onDismiss: () => void }) {
  // Clamp so the 460px composer never overflows the viewport.
  const W = 460
  const H = 180
  const margin = 12
  const vw = window.innerWidth
  const vh = window.innerHeight
  const left = Math.min(Math.max(margin, at.x - W / 2), vw - W - margin)
  const top  = Math.min(Math.max(margin, at.y - 16), vh - H - margin)

  // Dismiss on outside click.
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onDown = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as globalThis.Node)) onDismiss()
    }
    // Defer so the pane-click event that opened us doesn't immediately close.
    const id = setTimeout(() => window.addEventListener('pointerdown', onDown), 0)
    return () => { clearTimeout(id); window.removeEventListener('pointerdown', onDown) }
  }, [onDismiss])

  return (
    <div
      ref={ref}
      className="fixed z-50"
      style={{ left, top, width: W }}
      onPointerDown={(e) => e.stopPropagation()}
    >
      <Composer autoFocus onSent={() => onDismiss()} onCancel={onDismiss} />
    </div>
  )
}

function ThemeToggle() {
  const [dark, setDark] = useState(() => {
    if (typeof window === 'undefined') return false
    const saved = localStorage.getItem('orchid.theme')
    if (saved) return saved === 'dark'
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false
  })
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('orchid.theme', dark ? 'dark' : 'light')
  }, [dark])
  return (
    <button
      onClick={() => setDark(d => !d)}
      title={dark ? 'switch to light' : 'switch to dark'}
      className="w-7 h-7 rounded-md flex items-center justify-center transition-colors text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800 hover:text-zinc-800 dark:hover:text-zinc-100"
    >
      {dark ? (
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4 12H2M22 12h-2M5 5l1.5 1.5M17.5 17.5L19 19M5 19l1.5-1.5M17.5 6.5L19 5" strokeLinecap="round" />
        </svg>
      ) : (
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
          <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" strokeLinejoin="round" />
        </svg>
      )}
    </button>
  )
}

function CardCompact({ job }: { job: Job }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const agent = detectAgent(job)
  return (
    <div className="p-3 h-full flex flex-col gap-1.5">
      <div className="flex items-center gap-1.5 min-w-0">
        <AgentMark agent={agent} />
        <span className="mono text-[10.5px] text-zinc-500 dark:text-zinc-400 truncate">{repo}</span>
        <div className="flex-1" />
        {job.lifecycle === 'cron' && (
          <span className="mono text-[10px] text-violet-500">cron</span>
        )}
      </div>
      <div className="text-[13px] text-zinc-900 dark:text-zinc-100 leading-snug line-clamp-4 flex-1">
        {job.issue_title || '—'}
      </div>
    </div>
  )
}

type Agent = 'claude' | 'codex' | 'unknown'

function detectAgent(job: Job): Agent {
  const t = (job.tmux || '').toLowerCase()
  if (t.startsWith('codex')) return 'codex'
  if (t.startsWith('claude')) return 'claude'
  return 'unknown'
}

function AgentMark({ agent }: { agent: Agent }) {
  if (agent === 'claude') {
    // Official Claude mark from Bootstrap Icons (icons.getbootstrap.com/icons/claude).
    return (
      <svg
        width={13} height={13} viewBox="0 0 16 16"
        fill="currentColor"
        className="text-[#cc7c5a] flex-shrink-0"
        aria-label="Claude"
      >
        <path d="m3.127 10.604 3.135-1.76.053-.153-.053-.085H6.11l-.525-.032-1.791-.048-1.554-.065-1.505-.08-.38-.081L0 7.832l.036-.234.32-.214.455.04 1.009.069 1.513.105 1.097.064 1.626.17h.259l.036-.105-.089-.065-.068-.064-1.566-1.062-1.695-1.121-.887-.646-.48-.327-.243-.306-.104-.67.435-.48.585.04.15.04.593.456 1.267.981 1.654 1.218.242.202.097-.068.012-.049-.109-.181-.9-1.626-.96-1.655-.428-.686-.113-.411a2 2 0 0 1-.068-.484l.496-.674L4.446 0l.662.089.279.242.411.94.666 1.48 1.033 2.014.302.597.162.553.06.17h.105v-.097l.085-1.134.157-1.392.154-1.792.052-.504.25-.605.497-.327.387.186.319.456-.045.294-.19 1.23-.37 1.93-.243 1.29h.142l.161-.16.654-.868 1.097-1.372.484-.545.565-.601.363-.287h.686l.505.751-.226.775-.707.895-.585.759-.839 1.13-.524.904.048.072.125-.012 1.897-.403 1.024-.186 1.223-.21.553.258.06.263-.218.536-1.307.323-1.533.307-2.284.54-.028.02.032.04 1.029.098.44.024h1.077l2.005.15.525.346.315.424-.053.323-.807.411-3.631-.863-.872-.218h-.12v.073l.726.71 1.331 1.202 1.667 1.55.084.383-.214.302-.226-.032-1.464-1.101-.565-.497-1.28-1.077h-.084v.113l.295.432 1.557 2.34.08.718-.112.234-.404.141-.444-.08-.911-1.28-.94-1.44-.759-1.291-.093.053-.448 4.821-.21.246-.484.186-.403-.307-.214-.496.214-.98.258-1.28.21-1.016.19-1.263.112-.42-.008-.028-.092.012-.953 1.307-1.448 1.957-1.146 1.227-.274.109-.477-.247.045-.44.266-.39 1.586-2.018.956-1.25.617-.723-.004-.105h-.036l-4.212 2.736-.75.096-.324-.302.04-.496.154-.162 1.267-.871z"/>
      </svg>
    )
  }
  if (agent === 'codex') {
    // Official OpenAI mark from Bootstrap Icons (icons.getbootstrap.com/icons/openai).
    return (
      <svg
        width={13} height={13} viewBox="0 0 16 16"
        fill="currentColor"
        className="text-zinc-900 dark:text-white flex-shrink-0"
        aria-label="Codex"
      >
        <path d="M14.949 6.547a3.94 3.94 0 0 0-.348-3.273 4.11 4.11 0 0 0-4.4-1.934 4.1 4.1 0 0 0-1.126-.613 4.15 4.15 0 0 0-2.118-.086 4.1 4.1 0 0 0-1.891.948 4.04 4.04 0 0 0-1.158 1.753 4.1 4.1 0 0 0-1.563.679 4 4 0 0 0-1.14 1.254 3.99 3.99 0 0 0 .502 4.731 3.94 3.94 0 0 0 .346 3.274 4.11 4.11 0 0 0 4.402 1.933c.382.425.852.764 1.377.995.526.231 1.095.35 1.67.346 1.78.002 3.358-1.132 3.901-2.804a4.1 4.1 0 0 0 1.563-.68 4 4 0 0 0 1.14-1.253 3.99 3.99 0 0 0-.506-4.716m-6.097 8.406a3.05 3.05 0 0 1-1.945-.694l.096-.054 3.23-1.838a.53.53 0 0 0 .265-.455v-4.49l1.366.778q.02.011.025.035v3.722c-.003 1.653-1.361 2.992-3.037 2.996m-6.53-2.75a2.95 2.95 0 0 1-.36-2.01l.095.057L5.29 12.09a.53.53 0 0 0 .527 0l3.949-2.246v1.555a.05.05 0 0 1-.022.041L6.473 13.3c-1.454.826-3.311.335-4.15-1.098m-.85-6.94A3.02 3.02 0 0 1 3.07 3.949v3.785a.51.51 0 0 0 .262.451l3.93 2.237-1.366.779a.05.05 0 0 1-.048 0L2.585 9.342a2.98 2.98 0 0 1-1.113-4.094zm11.216 2.571L8.747 5.576l1.362-.776a.05.05 0 0 1 .048 0l3.265 1.86a3 3 0 0 1 1.173 1.207 2.96 2.96 0 0 1-.27 3.2 3.05 3.05 0 0 1-1.36.997V8.279a.52.52 0 0 0-.276-.445m1.36-2.015-.097-.057-3.226-1.855a.53.53 0 0 0-.53 0L6.249 6.153V4.598a.04.04 0 0 1 .019-.04L9.533 2.7a3.07 3.07 0 0 1 3.257.139c.474.325.843.778 1.066 1.303.223.526.289 1.103.191 1.664zM5.503 8.575 4.139 7.8a.05.05 0 0 1-.026-.037V4.049c0-.57.166-1.127.476-1.607s.752-.864 1.275-1.105a3.08 3.08 0 0 1 3.234.41l-.096.054-3.23 1.838a.53.53 0 0 0-.265.455zm.742-1.577 1.758-1 1.762 1v2l-1.755 1-1.762-1z"/>
      </svg>
    )
  }
  return (
    <span className="w-3 h-3 rounded-full bg-zinc-300 dark:bg-zinc-600 flex-shrink-0" />
  )
}

