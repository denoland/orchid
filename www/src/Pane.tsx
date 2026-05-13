import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

interface Props {
  session: string
}

const XTERM_THEME = {
  background: '#ffffff',
  foreground: '#1a1a1a',
  cursor: '#555555',
  cursorAccent: '#ffffff',
  selectionBackground: 'rgba(0,0,0,0.12)',
  black: '#000000',
  red: '#c0392b',
  green: '#27ae60',
  yellow: '#d4a017',
  blue: '#2980b9',
  magenta: '#8e44ad',
  cyan: '#16a085',
  white: '#808080',
  brightBlack: '#404040',
  brightRed: '#e74c3c',
  brightGreen: '#2ecc71',
  brightYellow: '#f1c40f',
  brightBlue: '#3498db',
  brightMagenta: '#9b59b6',
  brightCyan: '#1abc9c',
  brightWhite: '#1a1a1a',
}

export function Pane({ session }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const retryRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const [status, setStatus] = useState<'connecting' | 'connected' | 'reconnecting'>('connecting')

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      theme: XTERM_THEME,
      fontFamily: 'ui-monospace, SF Mono, Menlo, monospace',
      fontSize: 13,
      lineHeight: 1.4,
      cursorBlink: false,
      scrollback: 5000,
      convertEol: true,
    })

    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()

    termRef.current = term
    fitRef.current = fit

    function connect() {
      if (wsRef.current) {
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
      }

      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const host = window.location.host
      const ws = new WebSocket(`${proto}//${host}/ws?s=${encodeURIComponent(session)}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => setStatus('connected')

      ws.onmessage = (ev) => {
        const data =
          ev.data instanceof ArrayBuffer
            ? new Uint8Array(ev.data)
            : ev.data
        term.write(data)
      }

      ws.onclose = () => {
        setStatus('reconnecting')
        retryRef.current = setTimeout(connect, 1500)
      }

      ws.onerror = () => ws.close()
    }

    term.onData((data) => {
      if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
        wsRef.current.send(data)
      }
    })

    connect()

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    return () => {
      window.removeEventListener('resize', onResize)
      if (retryRef.current) clearTimeout(retryRef.current)
      if (wsRef.current) {
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
      }
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, [session])

  return (
    <div className="flex flex-col min-h-screen bg-white">
      <div className="border-b border-[#ebebeb] px-6 h-10 flex items-center justify-between flex-shrink-0 bg-white">
        <a
          href="#/"
          className="text-[13px] text-[#525252] hover:text-[#171717] transition-colors"
        >
          ← orchid
        </a>
        <span className="text-[12px] text-[#a3a3a3] flex items-center gap-2">
          <code className="text-[#525252] font-mono">{session}</code>
          {status === 'reconnecting' && (
            <span className="text-[#dc2626]">reconnecting…</span>
          )}
          {status === 'connecting' && (
            <span className="text-[#d97706]">connecting…</span>
          )}
        </span>
      </div>

      <div className="flex-1 p-4 overflow-hidden">
        <div
          className="h-full border border-[#ebebeb] rounded-lg overflow-hidden"
          style={{ background: '#ffffff' }}
        >
          <div
            ref={containerRef}
            className="h-full"
            style={{ padding: '10px 14px' }}
          />
        </div>
      </div>
    </div>
  )
}
