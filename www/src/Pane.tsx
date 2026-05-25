import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

interface Props {
  session: string
}

const XTERM_THEME = {
  background: '#0b0b0e',
  foreground: '#e4e4e7',
  cursor: '#a78bfa',
  cursorAccent: '#0b0b0e',
  selectionBackground: 'rgba(167,139,250,0.25)',
  black: '#0b0b0e',
  red: '#f87171',
  green: '#34d399',
  yellow: '#fbbf24',
  blue: '#60a5fa',
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#a1a1aa',
  brightBlack: '#52525b',
  brightRed: '#fca5a5',
  brightGreen: '#6ee7b7',
  brightYellow: '#fcd34d',
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#fafafa',
}

export function Pane({ session }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const [status, setStatus] = useState<'connecting' | 'live' | 'error'>('connecting')

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      theme: XTERM_THEME,
      fontFamily: 'ui-monospace, SF Mono, Menlo, monospace',
      fontSize: 12,
      lineHeight: 1.35,
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
    // Without this, the xterm canvas is rendered but never picks up
    // keyboard focus until the user clicks inside it — typing into the
    // expanded card looks broken.
    term.focus()

    let lastDims = `${term.cols}x${term.rows}`
    const pushResize = () => {
      const dims = `${term.cols}x${term.rows}`
      if (dims === lastDims) return
      lastDims = dims
      // Best-effort — server clamps and silently drops if the session
      // disappeared mid-resize.
      fetch(
        `/api/pane/resize?s=${encodeURIComponent(session)}&cols=${term.cols}&rows=${term.rows}`,
        { method: 'POST' }
      ).catch(() => {})
    }
    pushResize()

    // Keystrokes → POST to /api/pane (unchanged from polling era).
    term.onData((data) => {
      fetch(`/api/pane?s=${encodeURIComponent(session)}`, { method: 'POST', body: data })
        .catch(() => {})
    })

    // Server-Sent Events stream of base64-encoded `tmux capture-pane`
    // snapshots. Held open only while the card is on-screen and the
    // tab is foregrounded — every second of an idle stream burns DO
    // duration and ~40KB/s of egress for content nobody's reading.
    let es: EventSource | null = null
    let cancelled = false
    let isVisible = true       // intersection observer says it's on screen
    let isForeground = !document.hidden
    let retryTimer: ReturnType<typeof setTimeout> | undefined

    const shouldStream = () => isVisible && isForeground

    function connect() {
      if (cancelled || !shouldStream() || es) return
      setStatus('connecting')
      const cols = term.cols
      const rows = term.rows
      es = new EventSource(
        `/api/pane/stream?s=${encodeURIComponent(session)}&cols=${cols}&rows=${rows}`,
      )
      es.onopen = () => setStatus('live')
      const decoder = new TextDecoder('utf-8')
      const decodeBytes = (data: string) => {
        const bin = atob(data)
        const out = new Uint8Array(bin.length)
        for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
        return out
      }
      es.onmessage = async (ev) => {
        try {
          let raw: string = ev.data
          let bytes: Uint8Array
          if (raw.startsWith('z:')) {
            // Per-frame gzip: orch packs each tmux snapshot through
            // gzip, base64-encodes the bytes, and tags them `z:` so we
            // know to inflate via the platform DecompressionStream API.
            // Done at the app layer because Content-Encoding doesn't
            // pass cleanly through the relay tunnel.
            const gz = decodeBytes(raw.slice(2))
            const stream = new Blob([gz as BlobPart]).stream()
              .pipeThrough(new DecompressionStream('gzip'))
            const buf = await new Response(stream).arrayBuffer()
            bytes = new Uint8Array(buf)
          } else {
            bytes = decodeBytes(raw)
          }
          term.clear()
          term.write(decoder.decode(bytes))
        } catch { /* malformed frame, ignore */ }
      }
      es.onerror = () => {
        setStatus('error')
        if (es?.readyState === EventSource.CLOSED) {
          es = null
          if (shouldStream()) retryTimer = setTimeout(connect, 2000)
        }
      }
    }
    function disconnect() {
      if (retryTimer) { clearTimeout(retryTimer); retryTimer = undefined }
      if (es) { es.close(); es = null }
      setStatus('connecting')
    }

    // Pause the stream when the card scrolls / flows off-screen.
    // 0% threshold = react as soon as it leaves the viewport.
    const io = new IntersectionObserver((entries) => {
      const v = entries[0]?.isIntersecting ?? true
      if (v === isVisible) return
      isVisible = v
      if (shouldStream()) connect()
      else disconnect()
    }, { threshold: 0 })
    io.observe(containerRef.current)

    // Tab visibility — backgrounded tabs would otherwise keep the
    // stream open + burn DO duration for content they can't render.
    const onVis = () => {
      isForeground = !document.hidden
      if (shouldStream()) connect()
      else disconnect()
    }
    document.addEventListener('visibilitychange', onVis)

    connect()

    const doFit = () => {
      fitRef.current?.fit()
      pushResize()
    }
    window.addEventListener('resize', doFit)
    // The card grows from 220×92 to 880×560 when expanded — observe the
    // container directly so we fit when the wrapper layout changes too.
    const ro = new ResizeObserver(() => doFit())
    ro.observe(containerRef.current)

    return () => {
      cancelled = true
      if (retryTimer) clearTimeout(retryTimer)
      es?.close()
      io.disconnect()
      document.removeEventListener('visibilitychange', onVis)
      window.removeEventListener('resize', doFit)
      ro.disconnect()
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, [session])

  return (
    <div className="flex flex-col h-full bg-[#0b0b0e] rounded-lg overflow-hidden">
      <div className="px-3 h-7 flex items-center justify-between flex-shrink-0 border-b border-zinc-800/70">
        <code className="text-[11px] text-zinc-400 mono truncate">{session}</code>
        <span className="text-[10px] mono text-zinc-500 flex items-center gap-1.5">
          <span className={
            'w-1.5 h-1.5 rounded-full ' + (
              status === 'live' ? 'bg-emerald-400 animate-pulse' :
              status === 'error' ? 'bg-rose-400' : 'bg-amber-400'
            )
          } />
          {status}
        </span>
      </div>
      <div ref={containerRef} className="flex-1 overflow-hidden p-2" />
    </div>
  )
}
