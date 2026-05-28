import { useEffect, useMemo, useState } from 'react'

// /api/drafts demands X-Capture-Token (same secret macOS/iOS apps use).
// The dashboard is already auth'd via session cookie, but the capture
// endpoint deliberately ignores cookies — it's the one path that has
// to work for native clients without a relay session. We fetch the
// config once to learn (a) the capture token, (b) the configured
// targets so the user picks among real labels/repos instead of
// hardcoded ones, and (c) the inbox repo (capture's default).
let configPromise: Promise<CaptureConfig | null> | null = null
interface ConfigTarget { name: string; label?: string; repo?: string }
interface CaptureConfig {
  token: string | null
  inboxRepo: string | null
  targets: ConfigTarget[]
}
function getCaptureConfig(force = false): Promise<CaptureConfig | null> {
  if (configPromise && !force) return configPromise
  configPromise = fetch('/api/config', { credentials: 'include', cache: 'no-store' })
    .then((r) => r.ok ? r.json() : null)
    .then((j: any) => ({
      token: j?.orchestrator?.capture?.auth_token ?? null,
      inboxRepo: j?.github?.inbox_repo ?? null,
      targets: Array.isArray(j?.targets) ? j.targets : [],
    } as CaptureConfig))
    .catch(() => null)
  return configPromise
}

type Outcome =
  | { kind: 'sent'; url: string | null }
  | { kind: 'error'; reason: string }

interface ComposerProps {
  autoFocus?: boolean
  onSent?: (issueUrl: string | null) => void
  onCancel?: () => void
}

export function Composer({ autoFocus, onSent, onCancel }: ComposerProps = {}) {
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<Outcome | null>(null)
  const [cfg, setCfg] = useState<CaptureConfig | null>(null)
  const [targetName, setTargetName] = useState<string>('')

  useEffect(() => {
    let alive = true
    getCaptureConfig().then((c) => { if (alive && c) setCfg(c) })
    return () => { alive = false }
  }, [])

  // Pick a default target once the config arrives.
  useEffect(() => {
    if (cfg && !targetName && cfg.targets.length > 0) {
      setTargetName(cfg.targets[0].name)
    }
  }, [cfg, targetName])

  const selected = useMemo(
    () => cfg?.targets.find((t) => t.name === targetName) ?? null,
    [cfg, targetName],
  )

  const send = async () => {
    if (!note.trim() || busy) return
    setBusy(true)
    setOutcome(null)
    try {
      // Default to the inbox repo if no targets are configured. Use the
      // target's label as the GitHub label (falls back to target name).
      const labels: string[] = []
      if (selected?.label) labels.push(selected.label)
      else if (selected?.name) labels.push(selected.name)
      const repo = selected?.repo ?? cfg?.inboxRepo ?? ''
      if (!repo) {
        setOutcome({ kind: 'error', reason: 'no inbox repo or target configured' })
        return
      }
      const body = {
        id: ulidLike(),
        createdAt: new Date().toISOString(),
        source: 'web',
        kind: 'text',
        note: note.trim(),
        text: { body: note.trim(), originURL: window.location.href },
        target: { repo, labels },
      }
      if (!cfg?.token) {
        setOutcome({ kind: 'error', reason: 'capture token unavailable — configure capture block in swarm.hcl' })
        return
      }
      const res = await fetch('/api/drafts', {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'x-capture-token': cfg.token },
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
        <TargetMenu
          targets={cfg?.targets ?? []}
          value={targetName}
          onChange={setTargetName}
          inboxRepo={cfg?.inboxRepo ?? null}
        />
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

function TargetMenu({
  targets, value, onChange, inboxRepo,
}: {
  targets: ConfigTarget[]
  value: string
  onChange: (name: string) => void
  inboxRepo: string | null
}) {
  if (targets.length === 0) {
    return (
      <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400">
        inbox · {inboxRepo ?? '—'}
      </span>
    )
  }
  return (
    <div className="relative">
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="appearance-none text-[11.5px] mono pl-2.5 pr-6 py-1 rounded-full bg-zinc-100 dark:bg-zinc-800 hover:bg-zinc-200/80 dark:hover:bg-zinc-700 text-zinc-700 dark:text-zinc-200"
      >
        {targets.map((t) => (
          <option key={t.name} value={t.name}>
            {t.label ?? t.name}{t.repo ? ` → ${t.repo.split('/')[1] ?? t.repo}` : ''}
          </option>
        ))}
      </select>
      <span className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-[8px] text-zinc-500">▼</span>
    </div>
  )
}

function ulidLike(): string {
  const t = Date.now().toString(36).toUpperCase().padStart(10, '0')
  const r = Array.from({ length: 16 }, () =>
    'ABCDEFGHJKMNPQRSTVWXYZ0123456789'[Math.floor(Math.random() * 32)]
  ).join('')
  return `${t}-${r}`
}
