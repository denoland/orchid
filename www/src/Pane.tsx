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

    const doFit = () => { fitRef.current?.fit() }
    window.addEventListener('resize', doFit)
    // handle virtual keyboard on mobile
    window.visualViewport?.addEventListener('resize', doFit)

    return () => {
      window.removeEventListener('resize', doFit)
      window.visualViewport?.removeEventListener('resize', doFit)
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
    <div className="flex flex-col bg-white" style={{ height: '100dvh' }}>
      <div className="border-b border-[#ebebeb] px-4 h-9 flex items-center justify-between flex-shrink-0 bg-white">
        <a
          href="#/"
          className="text-[12px] text-[#525252] hover:text-[#171717] transition-colors"
        >
          ← orchid
        </a>
        <span className="text-[11px] text-[#a3a3a3] flex items-center gap-1.5 min-w-0 overflow-hidden">
          <code className="text-[#525252] font-mono truncate">{session}</code>
          {status === 'reconnecting' && <span className="text-[#dc2626] flex-shrink-0">reconnecting…</span>}
          {status === 'connecting' && <span className="text-[#d97706] flex-shrink-0">connecting…</span>}
        </span>
      </div>

      <div
        ref={containerRef}
        className="flex-1 overflow-hidden"
        style={{ padding: '8px 10px' }}
      />
    </div>
  )
}
