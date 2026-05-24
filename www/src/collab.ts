import { useEffect, useRef, useState } from 'react'

export interface Cursor {
  x: number
  y: number
  color: string
  name: string
  updatedAt: number
}

interface CollabMsg {
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

function shortName(id: string): string {
  return id.slice(-4)
}

/// Realtime collab over websocket. The server is a dumb relay — every
/// connected client receives every other client's messages. Cursors are
/// handled internally; anything else goes through `onMessage`.
export function useCollabSocket(opts?: {
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
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      const ws = new WebSocket(`${proto}://${location.host}/api/canvas/ws`)
      wsRef.current = ws

      sendImplRef.current = (m: CollabMsg) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify(m))
        }
      }

      ws.onopen = () => { retry = 0 }
      ws.onmessage = (ev) => {
        let msg: CollabMsg
        try { msg = JSON.parse(ev.data) } catch { return }
        switch (msg.type) {
          case 'hello':
            if (msg.userId) {
              myIdRef.current = msg.userId
              setMyId(msg.userId)
            }
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
                const next = new Map(prev)
                next.delete(msg.userId!)
                return next
              })
            }
            break
          default:
            // Everything else (strokes, node moves, edges, …) is forwarded
            // to the caller for app-level state reconciliation.
            onMessageRef.current?.(msg)
            break
        }
      }
      ws.onclose = () => {
        if (!alive) return
        retry = Math.min(retry + 1, 6)
        setTimeout(connect, 500 * (1 << (retry - 1)))
      }
      ws.onerror = () => { /* will trigger onclose */ }
    }
    connect()
    return () => {
      alive = false
      wsRef.current?.close()
    }
  }, [])

  // GC stale cursors (no update in 5s = disconnected).
  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now()
      setCursors((prev) => {
        let changed = false
        const next = new Map(prev)
        for (const [k, v] of next) {
          if (now - v.updatedAt > 5000) {
            next.delete(k)
            changed = true
          }
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
