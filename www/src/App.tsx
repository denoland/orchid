import { createContext, useEffect, useRef, useState } from 'react'
import { Dashboard } from './Dashboard'
import { Docs } from './Docs'
import type { State } from './types'

export interface RelayInfo {
  connected: boolean
  root: string | null
  login: string | null
  token: string | null
}

// Shared events-WS bus. App.tsx owns exactly one WS connection per tab
// and routes frames to interested subscribers via this context, so we
// don't pay for a second WS handshake + DO accept for every consumer
// (collab cursors, future pane streams, etc.).
export interface WSBus {
  subscribe: (handler: (msg: any) => void) => () => void
  send: (msg: any) => void
  // Stable peer id assigned by the relay on accept. Set once a `hello`
  // frame lands; null before that.
  peerId: () => string | null
}
export const WSBusContext = createContext<WSBus | null>(null)

export default function App() {
  if (location.pathname.startsWith('/docs')) return <Docs />
  return <RootGate />
}

// "/" has no marketing landing: signed-in users get the dashboard, everyone
// else gets the docs (with a Sign in button). We probe /api/state once to
// decide — ok ⇒ dashboard, 401/403/unreachable ⇒ docs.
function RootGate() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  useEffect(() => {
    let alive = true
    fetch('/api/state', { credentials: 'include' })
      .then((r) => {
        // A real signed-in response is JSON; vite dev / SPA fallback returns
        // HTML with 200, which must NOT count as authed.
        const json = r.headers.get('content-type')?.includes('application/json')
        if (alive) setAuthed(r.ok && !!json)
      })
      .catch(() => { if (alive) setAuthed(false) })
    return () => { alive = false }
  }, [])
  if (authed === null) return null
  return authed ? <DashboardApp /> : <Docs />
}

function DashboardApp() {
  const [state, setState] = useState<State>({ jobs: [], vms: [], inbox: '' })
  const [relay, setRelay] = useState<RelayInfo | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const peerIdRef = useRef<string | null>(null)
  const subsRef = useRef<Set<(msg: any) => void>>(new Set())

  useEffect(() => {
    // Polling cadence when the events WS is down. Kept tight because
    // the UI feels stale fast — the WS path is the steady state.
    const FALLBACK_POLL_MS = 5000
    // Cap on the exponential WS-reopen backoff.
    const RECONNECT_MAX_MS = 30_000
    let cancelled = false
    let pollTimer: ReturnType<typeof setInterval> | undefined
    let reopenTimer: ReturnType<typeof setTimeout> | undefined
    let reopenDelay = 1000

    const bounceToLogin = () => {
      const apex = location.host.split('.').slice(1).join('.')
      const next = encodeURIComponent(location.href)
      location.href = `https://${apex}/login?next=${next}`
    }

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
      pollTimer = setInterval(fetchOnce, FALLBACK_POLL_MS)
    }

    const stopFallback = () => {
      if (pollTimer) { clearInterval(pollTimer); pollTimer = undefined }
    }

    const openWS = () => {
      if (cancelled) return
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
      let ws: WebSocket
      try {
        ws = new WebSocket(`${proto}//${location.host}/api/events/ws`)
      } catch {
        startFallback()
        return
      }
      wsRef.current = ws
      let firstMsgTimer: ReturnType<typeof setTimeout> | undefined
      ws.onopen = () => {
        reopenDelay = 1000
        firstMsgTimer = setTimeout(() => { fetchOnce() }, 2000)
      }
      ws.onmessage = (ev) => {
        if (cancelled) return
        let f: any
        try { f = JSON.parse(ev.data) } catch { return }
        if (f.t === 'state') {
          // Real state arrived — drop the safety fetch + fallback poll
          // permanently for this connection.
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
        } else if (f.t === 'hello') {
          peerIdRef.current = f.userId ?? null
        }
        // Fan out every frame to bus subscribers (collab hooks etc.).
        // They filter by `t` themselves. A throwing subscriber is a bug,
        // not a benign race — log it so it gets noticed.
        for (const fn of subsRef.current) {
          try { fn(f) } catch (e) { console.error('bus subscriber:', e) }
        }
      }
      ws.onclose = () => {
        if (cancelled) return
        if (firstMsgTimer) { clearTimeout(firstMsgTimer); firstMsgTimer = undefined }
        wsRef.current = null
        peerIdRef.current = null
        startFallback()
        reopenTimer = setTimeout(openWS, reopenDelay)
        reopenDelay = Math.min(reopenDelay * 2, RECONNECT_MAX_MS)
      }
      ws.onerror = () => {}
    }

    openWS()

    return () => {
      cancelled = true
      if (wsRef.current) { try { wsRef.current.close() } catch {} }
      stopFallback()
      if (reopenTimer) clearTimeout(reopenTimer)
    }
  }, [])

  const bus: WSBus = {
    subscribe: (handler) => { subsRef.current.add(handler); return () => { subsRef.current.delete(handler) } },
    send: (msg) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        try { ws.send(JSON.stringify(msg)) } catch {}
      }
    },
    peerId: () => peerIdRef.current,
  }

  return (
    <WSBusContext.Provider value={bus}>
      <Dashboard state={state} relay={relay} />
    </WSBusContext.Provider>
  )
}
