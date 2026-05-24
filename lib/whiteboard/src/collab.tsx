import { useEffect, useRef, useState } from 'react'
import { useViewport } from '@xyflow/react'

export interface Cursor {
  x: number
  y: number
  color: string
  name: string
  updatedAt: number
}

export interface CollabMsg {
  type: string
  userId?: string
  peers?: string[]
  [k: string]: unknown
}

const COLORS = [
  '#ef4444', '#f97316', '#eab308', '#22c55e',
  '#06b6d4', '#3b82f6', '#a855f7', '#ec4899',
]

function colorFor(id: string) {
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) | 0
  return COLORS[Math.abs(h) % COLORS.length]
}
function shortName(id: string): string { return id.slice(-4) }

/// Realtime collab over WebSocket. The server is expected to be a dumb
/// relay — every client receives every other client's messages.
/// `cursor` / `hello` / `leave` are handled internally; anything else
/// is forwarded to the caller for state reconciliation.
export function useCollabSocket(opts?: {
  url?: string                          // default `${proto}://${host}/api/canvas/ws`
  onMessage?: (msg: CollabMsg) => void
}): {
  cursors: Map<string, Cursor>
  myId: string | null
  sendCursor: (worldX: number, worldY: number) => void
  send: (msg: CollabMsg) => void
} {
  const [cursors, setCursors] = useState<Map<string, Cursor>>(new Map())
  const myIdRef = useRef<string | null>(null)
  const [myId, setMyId] = useState<string | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const lastSentRef = useRef(0)
  const sendImplRef = useRef<(m: CollabMsg) => void>(() => {})
  const onMessageRef = useRef(opts?.onMessage)
  onMessageRef.current = opts?.onMessage

  useEffect(() => {
    let alive = true
    let retry = 0
    function connect() {
      const url = opts?.url ?? `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/canvas/ws`
      const ws = new WebSocket(url)
      wsRef.current = ws
      sendImplRef.current = (m: CollabMsg) => {
        if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m))
      }
      ws.onopen = () => { retry = 0 }
      ws.onmessage = (ev) => {
        let msg: CollabMsg
        try { msg = JSON.parse(ev.data) } catch { return }
        switch (msg.type) {
          case 'hello':
            if (msg.userId) { myIdRef.current = msg.userId; setMyId(msg.userId) }
            break
          case 'cursor': {
            const id = msg.userId
            if (!id || id === myIdRef.current) break
            const x = msg['x'] as number, y = msg['y'] as number
            setCursors((prev) => {
              const next = new Map(prev)
              next.set(id, { x, y, color: colorFor(id), name: shortName(id), updatedAt: Date.now() })
              return next
            })
            break
          }
          case 'leave':
            if (msg.userId) {
              setCursors((prev) => {
                if (!prev.has(msg.userId!)) return prev
                const next = new Map(prev); next.delete(msg.userId!); return next
              })
            }
            break
          default:
            onMessageRef.current?.(msg)
            break
        }
      }
      ws.onclose = () => {
        if (!alive) return
        retry = Math.min(retry + 1, 6)
        setTimeout(connect, 500 * (1 << (retry - 1)))
      }
      ws.onerror = () => {}
    }
    connect()
    return () => { alive = false; wsRef.current?.close() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // GC stale cursors (no update in 5s = disconnected).
  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now()
      setCursors((prev) => {
        let changed = false
        const next = new Map(prev)
        for (const [k, v] of next) {
          if (now - v.updatedAt > 5000) { next.delete(k); changed = true }
        }
        return changed ? next : prev
      })
    }, 1500)
    return () => clearInterval(id)
  }, [])

  const sendCursor = (worldX: number, worldY: number) => {
    const now = performance.now()
    if (now - lastSentRef.current < 33) return
    lastSentRef.current = now
    sendImplRef.current({ type: 'cursor', x: worldX, y: worldY })
  }
  const send = (msg: CollabMsg) => sendImplRef.current(msg)

  return { cursors, myId, sendCursor, send }
}

/// Captures pointer moves on the React Flow surface and broadcasts them
/// as world-space cursors. Renders every remote cursor on top.
export function CollabLayer({
  cursors, sendCursor, containerRef,
}: {
  cursors: Map<string, Cursor>
  sendCursor: (x: number, y: number) => void
  containerRef: React.RefObject<HTMLDivElement | null>
}) {
  const viewport = useViewport()

  useEffect(() => {
    const onMove = (e: PointerEvent) => {
      const r = containerRef.current?.getBoundingClientRect()
      if (!r) return
      const worldX = (e.clientX - r.left - viewport.x) / viewport.zoom
      const worldY = (e.clientY - r.top - viewport.y) / viewport.zoom
      sendCursor(worldX, worldY)
    }
    window.addEventListener('pointermove', onMove)
    return () => window.removeEventListener('pointermove', onMove)
  }, [containerRef, viewport.x, viewport.y, viewport.zoom, sendCursor])

  return (
    <div className="absolute inset-0 pointer-events-none overflow-hidden" style={{ zIndex: 50 }}>
      {Array.from(cursors.entries()).map(([id, c]) => (
        <CursorView key={id} cursor={c} viewport={viewport} />
      ))}
    </div>
  )
}

function CursorView({ cursor, viewport }: { cursor: Cursor; viewport: { x: number; y: number; zoom: number } }) {
  const screenX = cursor.x * viewport.zoom + viewport.x
  const screenY = cursor.y * viewport.zoom + viewport.y
  return (
    <div className="absolute" style={{ transform: `translate3d(${screenX}px, ${screenY}px, 0)`, transition: 'transform 90ms linear' }}>
      <svg width={18} height={20} viewBox="0 0 18 20" fill="none" style={{ filter: 'drop-shadow(0 1px 2px rgba(0,0,0,.2))' }}>
        <path d="M3 2l13 8-5 1-1 6-7-15z" fill={cursor.color} stroke="white" strokeWidth={1.2} strokeLinejoin="round" />
      </svg>
      <span
        className="mono text-[10px] px-1.5 py-0.5 rounded text-white shadow"
        style={{ background: cursor.color, transform: 'translate(14px, -4px)', display: 'inline-block' }}
      >
        {cursor.name}
      </span>
    </div>
  )
}
