import { useContext, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { WSBusContext } from './App'

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
  const bus = useContext(WSBusContext)

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

    // Pane bytes ride the events WS as {t:'pane', paneId, data} frames.
    // No HTTP SSE, no long-open response — DO stays hibernated between
    // tmux ticks, then wakes briefly to forward each frame. Card off-
    // screen or tab backgrounded → pane-unsub → 0 traffic.
    //
    // Falls back to the legacy /api/pane/stream EventSource when bus
    // is unavailable (local-mode orch without relay).
    let cancelled = false
    let isVisible = true
    let isForeground = !document.hidden
    let subbed = false
    let retryTimer: ReturnType<typeof setTimeout> | undefined
    let es: EventSource | null = null
    const shouldStream = () => isVisible && isForeground

    const decoder = new TextDecoder('utf-8')
    const decodeBytes = (data: string) => {
      const bin = atob(data)
      const out = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
      return out
    }
    const applyFrame = async (raw: string) => {
      try {
        let bytes: Uint8Array
        if (raw.startsWith('z:')) {
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

    let busUnsub: (() => void) | undefined
    function subscribe() {
      if (cancelled || !shouldStream()) return
      if (bus) {
        if (subbed) return
        subbed = true
        setStatus('connecting')
        bus.send({ t: 'pane-sub', paneId: session, cols: term.cols, rows: term.rows })
        if (!busUnsub) {
          busUnsub = bus.subscribe((msg: any) => {
            if (msg?.t !== 'pane' || msg.paneId !== session) return
            setStatus('live')
            void applyFrame(msg.data)
          })
        }
        return
      }
      // Legacy SSE fallback when there's no shared bus.
      if (es) return
      setStatus('connecting')
      es = new EventSource(
        `/api/pane/stream?s=${encodeURIComponent(session)}&cols=${term.cols}&rows=${term.rows}`,
      )
      es.onopen = () => setStatus('live')
      es.onmessage = (ev) => { void applyFrame(ev.data) }
      es.onerror = () => {
        setStatus('error')
        if (es?.readyState === EventSource.CLOSED) {
          es = null
          if (shouldStream()) retryTimer = setTimeout(subscribe, 2000)
        }
      }
    }
    function unsubscribe() {
      if (retryTimer) { clearTimeout(retryTimer); retryTimer = undefined }
      if (bus) {
        if (subbed) {
          subbed = false
          bus.send({ t: 'pane-unsub', paneId: session })
        }
      }
      if (es) { es.close(); es = null }
      setStatus('connecting')
    }

    const io = new IntersectionObserver((entries) => {
      const v = entries[0]?.isIntersecting ?? true
      if (v === isVisible) return
      isVisible = v
      if (shouldStream()) subscribe()
      else unsubscribe()
    }, { threshold: 0 })
    io.observe(containerRef.current)

    const onVis = () => {
      isForeground = !document.hidden
      if (shouldStream()) subscribe()
      else unsubscribe()
    }
    document.addEventListener('visibilitychange', onVis)

    subscribe()

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
      unsubscribe()
      if (busUnsub) busUnsub()
      io.disconnect()
      document.removeEventListener('visibilitychange', onVis)
      window.removeEventListener('resize', doFit)
      ro.disconnect()
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, [session, bus])

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
