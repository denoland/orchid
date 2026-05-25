import { useEffect, useState } from 'react'
import { Dashboard } from './Dashboard'
import { InstallModal } from './InstallModal'
import type { State } from './types'

export interface RelayInfo {
  connected: boolean
  root: string | null
  login: string | null
  token: string | null
}

export default function App() {
  const [state, setState] = useState<State>({ jobs: [], vms: [], inbox: '', operator: '' })
  const [relay, setRelay] = useState<RelayInfo | null>(null)

  useEffect(() => {
    let cancelled = false
    let ws: WebSocket | null = null
    let pollTimer: ReturnType<typeof setInterval> | undefined
    let reopenTimer: ReturnType<typeof setTimeout> | undefined
    let reopenDelay = 1000

    // 401/403 means the session cookie expired or never existed — kick
    // to OAuth via the apex /login. Same flow regardless of which
    // transport (WS or fetch fallback) noticed first.
    const bounceToLogin = () => {
      const apex = location.host.split('.').slice(1).join('.')
      const next = encodeURIComponent(location.href)
      location.href = `https://${apex}/login?next=${next}`
    }

    // HTTP fallback used when the events WS can't be opened — e.g. the
    // user is running orch locally without the relay, or a corporate
    // proxy strips Upgrade. Behaves like the old polling loop but at a
    // gentler cadence.
    const fetchOnce = async () => {
      if (document.hidden) return
      try {
        const res = await fetch('/api/state')
        if (res.status === 401 || res.status === 403) { bounceToLogin(); return }
        if (!res.ok) return
        const data: State = await res.json()
        if (!cancelled) setState(data)
      } catch { /* swallow */ }
    }

    const startFallback = () => {
      if (pollTimer) return
      fetchOnce()
      pollTimer = setInterval(fetchOnce, 5000)
    }

    const stopFallback = () => {
      if (pollTimer) { clearInterval(pollTimer); pollTimer = undefined }
    }

    const openWS = () => {
      if (cancelled) return
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
      try {
        ws = new WebSocket(`${proto}//${location.host}/api/events/ws`)
      } catch {
        startFallback()
        return
      }
      let firstMsgTimer: ReturnType<typeof setTimeout> | undefined
      ws.onopen = () => {
        // Reset reconnect backoff. Relay caches the latest state and
        // pushes it on accept; if orch hasn't pushed yet (fresh boot),
        // arm a one-shot fetch so the dashboard doesn't sit blank.
        reopenDelay = 1000
        firstMsgTimer = setTimeout(() => { fetchOnce() }, 2000)
      }
      ws.onmessage = (ev) => {
        if (cancelled) return
        try {
          const f = JSON.parse(ev.data) as
            | { t: 'state'; state: State }
            | { t: 'relay-info'; connected: boolean; root: string | null; login: string | null; token: string | null }
          if (f.t === 'state') {
            // Real state arrived — drop the safety fetch timer and any
            // active fallback poll. relay-info on its own doesn't count,
            // because a fresh DO sends relay-info before orch has had
            // a chance to push state, and we don't want that to mask
            // a missing state push.
            if (firstMsgTimer) { clearTimeout(firstMsgTimer); firstMsgTimer = undefined }
            stopFallback()
            setState(f.state)
          } else if (f.t === 'relay-info') {
            setRelay({
              connected: f.connected,
              root: f.root,
              login: f.login,
              token: f.token,
            })
          }
        } catch { /* ignore */ }
      }
      ws.onclose = () => {
        if (cancelled) return
        if (firstMsgTimer) { clearTimeout(firstMsgTimer); firstMsgTimer = undefined }
        ws = null
        // Reconnect with capped exponential backoff. Falls back to
        // polling in the meantime so the dashboard never goes blank.
        startFallback()
        reopenTimer = setTimeout(openWS, reopenDelay)
        reopenDelay = Math.min(reopenDelay * 2, 30000)
      }
      ws.onerror = () => {
        // Errors arrive just before close — close handler does the
        // reconnect, so this is intentionally a no-op.
      }
    }

    openWS()

    return () => {
      cancelled = true
      if (ws) { try { ws.close() } catch {} }
      stopFallback()
      if (reopenTimer) clearTimeout(reopenTimer)
    }
  }, [])

  return (
    <>
      <Dashboard state={state} relay={relay} />
      <InstallModal relay={relay} />
    </>
  )
}
