import { useState } from 'react'

const ROUTES = ['clawpatrol', 'orchid', 'deno'] as const
type Route = typeof ROUTES[number]

// Orch's /api/drafts demands X-Capture-Token (the same secret macOS/iOS
// apps use). The dashboard is already auth'd via session cookie, but the
// capture endpoint deliberately ignores cookies — it's the one path that
// has to work for native clients without a relay session. So fetch the
// token from /api/config once and reuse it for every Capture click.
let captureTokenPromise: Promise<string | null> | null = null
function getCaptureToken(): Promise<string | null> {
  if (captureTokenPromise) return captureTokenPromise
  captureTokenPromise = fetch('/api/config', { credentials: 'include', cache: 'no-store' })
    .then((r) => r.ok ? r.json() : null)
    .then((j: any) => j?.orchestrator?.capture?.auth_token ?? null)
    .catch(() => null)
  return captureTokenPromise
}

type Outcome =
  | { kind: 'sent'; url: string | null }
  | { kind: 'error'; reason: string }

/// Sleek freeform input pinned over the canvas. Mirrors the menu-bar
/// composer's vibe — translucent capsule, route picker, capture button.
/// Hitting Capture POSTs a draft to /api/drafts which orch turns into an
/// inbox issue.
interface ComposerProps {
  autoFocus?: boolean
  onSent?: (issueUrl: string | null) => void
  onCancel?: () => void
}

export function Composer({ autoFocus, onSent, onCancel }: ComposerProps = {}) {
  const [note, setNote] = useState('')
  const [route, setRoute] = useState<Route>('clawpatrol')
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<Outcome | null>(null)

  const send = async () => {
    if (!note.trim() || busy) return
    setBusy(true)
    setOutcome(null)
    try {
      const body = {
        id: ulidLike(),
        createdAt: new Date().toISOString(),
        source: 'web',
        kind: 'text',
        note: note.trim(),
        text: { body: note.trim(), originURL: window.location.href },
        target: { repo: 'denoland/orchid', labels: [route] },
      }
      const tok = await getCaptureToken()
      if (!tok) {
        setOutcome({ kind: 'error', reason: 'capture token unavailable — configure capture block in swarm.hcl' })
        return
      }
      const res = await fetch('/api/drafts', {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'x-capture-token': tok },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const t = await res.text().catch(() => '')
        setOutcome({ kind: 'error', reason: t || `HTTP ${res.status}` })
        return
      }
      const j: { issue_url?: string } = await res.json().catch(() => ({}))
      setOutcome({ kind: 'sent', url: j.issue_url ?? null })
      setNote('')
      onSent?.(j.issue_url ?? null)
    } catch (e: any) {
      setOutcome({ kind: 'error', reason: e?.message ?? 'fetch failed' })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="bg-white/75 dark:bg-zinc-900/75 backdrop-blur ring-1 ring-zinc-200/80 dark:ring-zinc-700/80 rounded-2xl shadow-lg shadow-zinc-200/50 dark:shadow-black/40 p-3">
      <textarea
        autoFocus={autoFocus}
        value={note}
        onChange={(e) => setNote(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            send()
          }
          if (e.key === 'Escape') {
            e.preventDefault()
            onCancel?.()
          }
        }}
        rows={2}
        placeholder="spawn an idea, bug, or thought… (⌘↩)"
        className="w-full resize-none bg-transparent outline-none text-[14.5px] text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
      />
      <div className="flex items-center gap-2 mt-2">
        <RouteMenu value={route} onChange={setRoute} />
        <div className="flex-1" />
        {outcome?.kind === 'sent' && (
          outcome.url ? (
            <a
              href={outcome.url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[11px] mono text-emerald-600 hover:underline"
            >
              ✓ #{outcome.url.split('/').pop()}
            </a>
          ) : (
            <span className="text-[11px] mono text-emerald-600">✓ filed</span>
          )
        )}
        {outcome?.kind === 'error' && (
          <span className="text-[11px] mono text-rose-600">× {outcome.reason}</span>
        )}
        <button
          onClick={send}
          disabled={!note.trim() || busy}
          className={
            'text-[12.5px] px-3 py-1.5 rounded-full mono font-medium transition-colors ' +
            (note.trim() && !busy
              ? 'bg-violet-600 text-white hover:bg-violet-700'
              : 'bg-zinc-200 dark:bg-zinc-800 text-zinc-400 dark:text-zinc-500 cursor-not-allowed')
          }
        >
          {busy ? 'sending…' : 'spawn ↵'}
        </button>
      </div>
    </div>
  )
}

function RouteMenu({ value, onChange }: { value: Route; onChange: (r: Route) => void }) {
  return (
    <div className="relative">
      <select
        value={value}
        onChange={(e) => onChange(e.target.value as Route)}
        className="appearance-none text-[11.5px] mono pl-2.5 pr-6 py-1 rounded-full bg-zinc-100 dark:bg-zinc-800 hover:bg-zinc-200/80 dark:hover:bg-zinc-700 text-zinc-700 dark:text-zinc-200"
      >
        {ROUTES.map((r) => <option key={r} value={r}>{r}</option>)}
      </select>
      <span className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-[8px] text-zinc-500">▼</span>
    </div>
  )
}

function ulidLike(): string {
  // Same shape as the macOS app's generator — not a real ULID, but
  // time-sortable + unique enough for client-generated IDs.
  const t = Date.now().toString(36).toUpperCase().padStart(10, '0')
  const r = Array.from({ length: 16 }, () =>
    'ABCDEFGHJKMNPQRSTVWXYZ0123456789'[Math.floor(Math.random() * 32)]
  ).join('')
  return `${t}-${r}`
}
