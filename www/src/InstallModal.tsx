import { useEffect, useState } from 'react'
import { OrchidArt } from './OrchidArt'

interface RelayInfo {
  connected: boolean
  token: string | null
  login: string | null
  root: string
}

/// When the dashboard is hosted via the orchid.com relay and the user's
/// orch instance hasn't dialed in yet, the relay returns `connected:
/// false` and the agent token. We show this modal with the one-liner
/// install command so the user can wire it up.
export function InstallModal() {
  const [info, setInfo] = useState<RelayInfo | null>(null)
  // Once dismissed, stay dismissed — a flaky DO sometimes reports
  // connected:false even when the agent's WS is alive, and we don't
  // want the modal to keep popping back over the dashboard.
  const [hidden, setHidden] = useState<boolean>(() => localStorage.getItem('orchid.installSeen') === '1')

  useEffect(() => {
    let cancelled = false
    let id: ReturnType<typeof setInterval> | undefined
    async function poll() {
      if (document.hidden) return
      try {
        const res = await fetch('/api/_relay/info', { credentials: 'same-origin' })
        if (!res.ok) { if (id) clearInterval(id); return }
        const j = (await res.json()) as RelayInfo
        if (cancelled) return
        setInfo(j)
        // Once the agent is connected we don't need to keep polling —
        // the install modal only exists to surface the join command
        // while orch is offline. If it later goes down, /api/state's
        // 503 + browser refresh will bring this back.
        if (j.connected && id) { clearInterval(id); id = undefined }
      } catch { /* swallow */ }
    }
    poll()
    id = setInterval(poll, 10000)
    return () => { cancelled = true; if (id) clearInterval(id) }
  }, [])

  if (!info || info.connected || hidden) return null

  const sub = info.login?.toLowerCase().replace(/[^a-z0-9-]/g, '') ?? 'me'
  // Authoritative root from the relay's ROOT_DOMAIN — beats parsing
  // location.hostname which gets confused by 3+ label setups.
  const root = info.root
  const relayURL = `wss://${sub}.${root}/agent`
  const install = `curl -fsSL https://${root}/install.sh | sh`
  const join = `orch join ${relayURL} ${info.token}`

  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-zinc-900/40 backdrop-blur-sm p-6">
      <OrchidArt opacity={0.28} />
      <div className="relative bg-white dark:bg-zinc-900 rounded-2xl ring-1 ring-zinc-200 dark:ring-zinc-700 shadow-2xl p-10 max-w-2xl w-full">
        <button
          onClick={() => { setHidden(true); localStorage.setItem('orchid.installSeen', '1') }}
          title="Close"
          className="absolute top-3 right-3 text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200 rounded p-1"
        >
          <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
            <line x1="18" y1="6" x2="6" y2="18" />
            <line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
        <div className="flex items-center gap-3 mb-6">
          <span className="w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
          <h2 className="serif text-[26px] italic font-medium text-zinc-900 dark:text-zinc-100">
            Connect your orchid
          </h2>
        </div>
        <Step n={1} label="Install">
          <Cmd value={install} />
        </Step>
        <Step n={2} label="Join">
          <Cmd value={join} />
        </Step>
      </div>
    </div>
  )
}

function Step({ n, label, children }: { n: number; label: string; children: React.ReactNode }) {
  return (
    <div className="mb-4 last:mb-0">
      <div className="flex items-center gap-2 mb-1.5">
        <span className="mono text-[10.5px] text-zinc-400 dark:text-zinc-500">{n}.</span>
        <span className="text-[12px] text-zinc-600 dark:text-zinc-400">{label}</span>
      </div>
      {children}
    </div>
  )
}

function Cmd({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="relative group">
      <pre className="bg-zinc-950 text-zinc-100 mono text-[12px] p-3 pr-16 rounded-lg overflow-x-auto whitespace-pre">{value}</pre>
      <button
        onClick={() => {
          navigator.clipboard.writeText(value).catch(() => {})
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        }}
        className="absolute top-1/2 right-2 -translate-y-1/2 mono text-[10.5px] px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-300 opacity-70 group-hover:opacity-100 transition-opacity"
      >
        {copied ? 'copied' : 'copy'}
      </button>
    </div>
  )
}
