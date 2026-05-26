import { useEffect, useRef, useState } from 'react'
import type { Node, NodeProps } from '@xyflow/react'
import type { LinkVariant } from './types'

// ─── note ──────────────────────────────────────────────────────────────

export const NOTE_W = 220
export const NOTE_H = 140

export type NoteData = {
  text: string
  onChange: (t: string) => void
  onDelete: () => void
}

export function NoteNode({ data }: NodeProps<Node<NoteData, 'note'>>) {
  const [editing, setEditing] = useState(false)
  return (
    <div
      className="bg-amber-100/95 dark:bg-amber-300/15 ring-1 ring-amber-200/80 dark:ring-amber-500/30 rounded-md shadow-sm hover:shadow-md p-2 group"
      style={{ width: NOTE_W, height: NOTE_H }}
      onDoubleClick={(e) => { e.stopPropagation(); setEditing(true) }}
    >
      <button
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => { e.stopPropagation(); data.onDelete() }}
        className="absolute -top-2 -right-2 w-5 h-5 rounded-full bg-white ring-1 ring-zinc-300 text-zinc-500 hover:text-rose-600 hover:ring-rose-300 text-[10px] opacity-0 group-hover:opacity-100 transition-opacity"
        title="delete note"
      >×</button>
      {editing ? (
        <textarea
          autoFocus
          value={data.text}
          onChange={(e) => data.onChange(e.target.value)}
          onBlur={() => setEditing(false)}
          onPointerDown={(e) => e.stopPropagation()}
          className="w-full h-full bg-transparent resize-none outline-none text-[13px] text-amber-900 dark:text-amber-100"
        />
      ) : (
        <div className="w-full h-full text-[13px] text-amber-900 dark:text-amber-100 whitespace-pre-wrap overflow-hidden">
          {data.text || <span className="text-amber-500 dark:text-amber-400 italic">double-click to edit</span>}
        </div>
      )}
    </div>
  )
}

// ─── text ──────────────────────────────────────────────────────────────

export type TextData = {
  text: string
  onChange: (t: string) => void
  onDelete: () => void
  startEditing?: boolean
}

export function TextNode({ data }: NodeProps<Node<TextData, 'text'>>) {
  const [editing, setEditing] = useState(!!data.startEditing)
  const ref = useRef<HTMLTextAreaElement | null>(null)
  useEffect(() => { if (editing) requestAnimationFrame(() => ref.current?.focus()) }, [editing])
  return (
    <div className="relative group min-w-[60px]" onDoubleClick={(e) => { e.stopPropagation(); setEditing(true) }}>
      <button
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => { e.stopPropagation(); data.onDelete() }}
        className="absolute -top-2 -right-2 w-5 h-5 rounded-full bg-white dark:bg-zinc-900 ring-1 ring-zinc-300 dark:ring-zinc-700 text-zinc-500 hover:text-rose-600 hover:ring-rose-300 text-[10px] opacity-0 group-hover:opacity-100 transition-opacity"
        title="delete"
      >×</button>
      {editing ? (
        <textarea
          ref={ref}
          value={data.text}
          onChange={(e) => data.onChange(e.target.value)}
          onBlur={() => setEditing(false)}
          onPointerDown={(e) => e.stopPropagation()}
          rows={Math.max(1, data.text.split('\n').length)}
          className="bg-transparent resize-none outline-none text-[15px] text-zinc-900 dark:text-zinc-100 leading-tight min-w-[160px]"
          autoFocus
          placeholder="text"
        />
      ) : (
        <div className="text-[15px] text-zinc-900 dark:text-zinc-100 leading-tight whitespace-pre-wrap select-none">
          {data.text || <span className="text-zinc-400 dark:text-zinc-500 italic">text</span>}
        </div>
      )}
    </div>
  )
}

// ─── link ──────────────────────────────────────────────────────────────

export type LinkData = {
  url: string
  title: string
  variant: LinkVariant
  image?: string
  description?: string
  site?: string
  snippet?: string
  onDelete: () => void
}

export const LINK_W = 280
export const LINK_H = 230

const VARIANT_STYLE: Record<LinkVariant, { icon: string; bg: string; ring: string; text: string }> = {
  youtube:       { icon: '▶',  bg: 'bg-red-50 dark:bg-red-900/20',           ring: 'ring-red-200 dark:ring-red-700/40',         text: 'text-red-700 dark:text-red-300' },
  'github-code': { icon: '<>', bg: 'bg-zinc-100 dark:bg-zinc-800',           ring: 'ring-zinc-300 dark:ring-zinc-700',          text: 'text-zinc-700 dark:text-zinc-200' },
  gist:          { icon: '✎',  bg: 'bg-zinc-100 dark:bg-zinc-800',           ring: 'ring-zinc-300 dark:ring-zinc-700',          text: 'text-zinc-700 dark:text-zinc-200' },
  docs:          { icon: '📄', bg: 'bg-blue-50 dark:bg-blue-900/20',         ring: 'ring-blue-200 dark:ring-blue-700/40',       text: 'text-blue-700 dark:text-blue-300' },
  meet:          { icon: '📞', bg: 'bg-emerald-50 dark:bg-emerald-900/20',   ring: 'ring-emerald-200 dark:ring-emerald-700/40', text: 'text-emerald-700 dark:text-emerald-300' },
  pr:            { icon: '⤴',  bg: 'bg-violet-50 dark:bg-violet-900/20',     ring: 'ring-violet-200 dark:ring-violet-700/40',   text: 'text-violet-700 dark:text-violet-300' },
  issue:         { icon: '○',  bg: 'bg-amber-50 dark:bg-amber-900/20',       ring: 'ring-amber-200 dark:ring-amber-700/40',     text: 'text-amber-700 dark:text-amber-300' },
  generic:       { icon: '🔗', bg: 'bg-white dark:bg-zinc-900',              ring: 'ring-zinc-200 dark:ring-zinc-700',          text: 'text-zinc-700 dark:text-zinc-200' },
}

export function LinkNode({ data }: NodeProps<Node<LinkData, 'link'>>) {
  const s = VARIANT_STYLE[data.variant]
  const host = (() => { try { return new URL(data.url).hostname.replace(/^www\./, '') } catch { return data.url } })()
  return (
    <div
      className={`${s.bg} ring-1 ${s.ring} rounded-xl shadow-sm hover:shadow-md overflow-hidden flex flex-col group`}
      style={{ width: LINK_W, height: LINK_H }}
    >
      <button
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => { e.stopPropagation(); data.onDelete() }}
        className="absolute -top-2 -right-2 w-5 h-5 rounded-full bg-white ring-1 ring-zinc-300 text-zinc-500 hover:text-rose-600 hover:ring-rose-300 text-[10px] opacity-0 group-hover:opacity-100 transition-opacity z-10"
      >×</button>
      {data.snippet ? (
        <div className="h-[110px] bg-zinc-900 dark:bg-zinc-950 text-zinc-100 p-2 overflow-hidden">
          <pre className="mono text-[9px] leading-tight whitespace-pre overflow-hidden">{data.snippet}</pre>
        </div>
      ) : data.image ? (
        <div className="h-[110px] bg-zinc-100 dark:bg-zinc-800 overflow-hidden">
          <img src={data.image} alt="" className="w-full h-full object-cover" onPointerDown={(e) => e.stopPropagation()} />
        </div>
      ) : (
        <div className="h-[110px] bg-gradient-to-br from-zinc-100 to-zinc-200 dark:from-zinc-800 dark:to-zinc-900 flex items-center justify-center">
          <span className={`text-3xl ${s.text}`}>{s.icon}</span>
        </div>
      )}
      <div className="p-3 flex flex-col gap-1 flex-1 min-h-0">
        <div className={`mono text-[10px] ${s.text} flex items-center gap-1.5`}>
          <span>{s.icon}</span>
          <span className="uppercase tracking-wide">{data.variant}</span>
          <span className="text-zinc-400 dark:text-zinc-500">·</span>
          <span className="text-zinc-500 dark:text-zinc-400 truncate">{data.site || host}</span>
        </div>
        <div className="text-[13px] font-medium text-zinc-900 dark:text-zinc-100 line-clamp-2 leading-snug">
          {data.title}
        </div>
        {data.description && (
          <div className="text-[11px] text-zinc-500 dark:text-zinc-400 line-clamp-2 leading-snug">
            {data.description}
          </div>
        )}
        <div className="flex-1" />
        <a
          href={data.url}
          target="_blank"
          rel="noopener noreferrer"
          onClick={(e) => e.stopPropagation()}
          onPointerDown={(e) => e.stopPropagation()}
          className="mono text-[10px] text-zinc-400 dark:text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 truncate"
        >
          {data.url}
        </a>
      </div>
    </div>
  )
}

/// Cheap URL → variant + title classification. No network calls. Use
/// fetchGitHubSnippet from this module to enrich later.
export function detectVariant(url: string): { variant: LinkVariant; title: string; image?: string } {
  try {
    const u = new URL(url)
    const host = u.hostname.replace(/^www\./, '')
    if (host === 'github.com') {
      const m = u.pathname.match(/^\/([^/]+)\/([^/]+)\/(pull|issues)\/(\d+)/)
      if (m) return { variant: m[3] === 'pull' ? 'pr' : 'issue', title: `${m[1]}/${m[2]} #${m[4]}` }
      const f = u.pathname.match(/^\/([^/]+)\/([^/]+)\/(blob|tree)\/([^/]+)\/(.+)/)
      if (f) return { variant: 'github-code', title: `${f[1]}/${f[2]} · ${f[5]}` }
      return { variant: 'github-code', title: host + u.pathname }
    }
    if (host === 'gist.github.com') return { variant: 'gist', title: 'gist ' + u.pathname.replace(/^\//, '') }
    if (host.endsWith('youtube.com') || host === 'youtu.be') {
      const id = u.searchParams.get('v') ?? u.pathname.replace(/^\//, '').split('/').pop() ?? ''
      return { variant: 'youtube', title: 'youtube · ' + id, image: id ? `https://i.ytimg.com/vi/${id}/hqdefault.jpg` : undefined }
    }
    if (host === 'docs.google.com') return { variant: 'docs', title: 'google docs' }
    if (host === 'meet.google.com') return { variant: 'meet', title: 'google meet ' + u.pathname.replace(/^\//, '') }
    return { variant: 'generic', title: host + u.pathname }
  } catch { return { variant: 'generic', title: url } }
}

export async function fetchGitHubSnippet(url: string): Promise<string | undefined> {
  const m = url.match(/^https:\/\/github\.com\/([^/]+)\/([^/]+)\/blob\/([^/]+)\/(.+?)(?:#.*)?$/)
  if (!m) return undefined
  const raw = `https://raw.githubusercontent.com/${m[1]}/${m[2]}/${m[3]}/${m[4]}`
  try {
    const res = await fetch(raw, { mode: 'cors' })
    if (!res.ok) return undefined
    const text = await res.text()
    return text.split('\n').slice(0, 8).join('\n')
  } catch { return undefined }
}

// ─── stroke ────────────────────────────────────────────────────────────

export type StrokeData = {
  d: string
  w: number
  h: number
  sw: number
  onErase?: (id: string) => void
  erasing?: boolean
}

export function StrokeNode({ id, data, selected }: NodeProps<Node<StrokeData, 'stroke'>>) {
  return (
    <svg
      width={data.w + data.sw * 2}
      height={data.h + data.sw * 2}
      viewBox={`${-data.sw} ${-data.sw} ${data.w + data.sw * 2} ${data.h + data.sw * 2}`}
      style={{ overflow: 'visible', cursor: data.erasing ? 'pointer' : 'move' }}
      onClick={(e) => { if (!data.erasing) return; e.stopPropagation(); data.onErase?.(id) }}
    >
      <path d={data.d} fill="none" strokeWidth={data.sw} strokeLinecap="round" strokeLinejoin="round" style={{ stroke: 'var(--ink)' }} />
      {selected && (
        <rect x={-2} y={-2} width={data.w + 4} height={data.h + 4} fill="none" stroke="#a78bfa" strokeDasharray="3 2" strokeWidth={0.8} />
      )}
    </svg>
  )
}
