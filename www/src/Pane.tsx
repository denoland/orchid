import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

interface Props {
  session: string
}

const XTERM_THEME = {
  background: '#0f0f1a',
  foreground: '#dcdcf0',
  cursor: '#dcdcf0',
  cursorAccent: '#0f0f1a',
  selectionBackground: 'rgba(120,120,220,0.28)',
  black: '#1a1a2c',
  red: '#f07070',
  green: '#78d080',
  yellow: '#e8c870',
  blue: '#7090e8',
  magenta: '#c070c8',
  cyan: '#70c8d8',
  white: '#dcdcf0',
  brightBlack: '#404060',
  brightRed: '#ff8888',
  brightGreen: '#90e898',
  brightYellow: '#f0d888',
  brightBlue: '#88a8f8',
  brightMagenta: '#d888e0',
  brightCyan: '#88d8e8',
  brightWhite: '#f0f0ff',
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

      ws.onopen = () => {
        setStatus('connected')
      }

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

      ws.onerror = () => {
        ws.close()
      }
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
    <div className="flex flex-col h-screen bg-[#0f0f1a]">
      {/* header */}
      <div
        className="flex items-center justify-between px-4 flex-shrink-0 border-b"
        style={{
          height: 40,
          borderColor: '#2a2a3c',
          background: '#13131f',
        }}
      >
        <a
          href="#/"
          className="text-[12px] text-[#7070a0] hover:text-[#dcdcf0] transition-colors"
        >
          ← dashboard
        </a>
        <span className="text-[12px] text-[#7070a0]">
          <code className="text-[#aaaacc]">{session}</code>
          {status === 'reconnecting' && (
            <span className="ml-3 text-[#f07070]">reconnecting…</span>
          )}
          {status === 'connecting' && (
            <span className="ml-3 text-[#e8c870]">connecting…</span>
          )}
        </span>
      </div>

      {/* terminal */}
      <div
        ref={containerRef}
        className="flex-1 overflow-hidden"
        style={{ padding: '8px 12px' }}
      />
    </div>
  )
}
