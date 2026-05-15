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

const POLL_MS = 1500

export function Pane({ session }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const [status, setStatus] = useState<'connecting' | 'connected' | 'error'>('connecting')

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

    let cancelled = false
    let last = ''

    async function poll() {
      try {
        const res = await fetch(`/api/pane?s=${encodeURIComponent(session)}`)
        if (!res.ok) { setStatus('error'); return }
        const body = await res.text()
        if (cancelled) return
        if (body !== last) {
          term.clear()
          term.write(body)
          last = body
        }
        setStatus('connected')
      } catch {
        if (!cancelled) setStatus('error')
      }
    }

    poll()
    const interval = setInterval(poll, POLL_MS)

    const doFit = () => { fitRef.current?.fit() }
    window.addEventListener('resize', doFit)
    window.visualViewport?.addEventListener('resize', doFit)

    return () => {
      cancelled = true
      clearInterval(interval)
      window.removeEventListener('resize', doFit)
      window.visualViewport?.removeEventListener('resize', doFit)
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
          {status === 'error' && <span className="text-[#dc2626] flex-shrink-0">disconnected</span>}
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
