import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  BackgroundVariant,
  Handle,
  Position,
  MarkerType,
  NodeResizer,
  useNodesState,
  useEdgesState,
  applyNodeChanges,
  applyEdgeChanges,
  addEdge,
  useViewport,
  useReactFlow,
  type Node,
  type Edge,
  type Connection,
  type NodeTypes,
  type NodeChange,
  type EdgeChange,
  type NodeProps,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import type { Job, State } from './types'
import { attention, ciStatus, LEVEL_COLOR, type AttentionLevel } from './attention'
import { Pane } from './Pane'
import { Composer } from './Composer'
import { useCollabSocket, type Cursor } from './collab'

interface Props { state: State }

const CARD_W = 220
const CARD_H = 96
const NOTE_W = 220
const NOTE_H = 140
const COLS = 4
const GAP = 18
const HEADER_OFFSET = 220
const STORAGE_KEY = 'orchid.canvas.v12'

type Tool = 'select' | 'box' | 'pen' | 'eraser' | 'note' | 'text'

/// A persisted ink stroke. The path is stored in local coords (its bbox
/// starts at 0,0); the node's `x,y` is the absolute canvas position.
interface Stroke {
  id: string
  x: number
  y: number
  w: number
  h: number
  d: string
  width: number
}

interface UserNode { type: 'note' | 'link' | 'text'; id: string; x: number; y: number; data: Record<string, unknown> }

interface Snap {
  cards: Record<string, { x: number; y: number }>
  user: UserNode[]
  strokes: Stroke[]
  edges: Edge[]
  viewport?: { x: number; y: number; zoom: number }
  panes?: Record<string, { x: number; y: number; w: number; h: number }>
  view?: 'canvas' | 'list'
}

function emptySnap(): Snap {
  return { cards: {}, user: [], strokes: [], edges: [], panes: {} }
}
function normalizeSnap(raw: any): Snap {
  if (!raw || typeof raw !== 'object') return emptySnap()
  return {
    cards: raw.cards ?? {},
    user: raw.user ?? [],
    strokes: raw.strokes ?? [],
    edges: raw.edges ?? [],
    viewport: raw.viewport,
    panes: raw.panes ?? {},
    view: raw.view === 'list' ? 'list' : 'canvas',
  }
}
async function fetchSnap(): Promise<Snap> {
  try {
    const r = await fetch('/api/snap', { credentials: 'include', cache: 'no-store' })
    if (!r.ok) return emptySnap()
    return normalizeSnap(await r.json())
  } catch { return emptySnap() }
}
// Debounced PUT so rapid drags don't flood the server. Flushed on
// pagehide/beforeunload via sendBeacon so a refresh mid-debounce never
// loses the latest layout.
let saveTimer: ReturnType<typeof setTimeout> | null = null
let savePending: Snap | null = null
function doPut(body: Snap) {
  return fetch('/api/snap', {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    credentials: 'include',
    keepalive: true,
    body: JSON.stringify(body),
  }).catch(() => {})
}
function saveSnap(s: Snap) {
  savePending = s
  if (saveTimer) return
  saveTimer = setTimeout(() => {
    const body = savePending
    saveTimer = null
    savePending = null
    if (body) doPut(body)
  }, 300)
}
function flushSnap() {
  if (saveTimer) {
    clearTimeout(saveTimer)
    saveTimer = null
  }
  const body = savePending
  savePending = null
  if (!body) return
  // sendBeacon survives navigation; falls back to fetch keepalive.
  const json = JSON.stringify(body)
  const sent = typeof navigator !== 'undefined' && navigator.sendBeacon &&
    navigator.sendBeacon('/api/snap', new Blob([json], { type: 'application/json' }))
  if (!sent) doPut(body)
}
if (typeof window !== 'undefined') {
  window.addEventListener('pagehide', flushSnap)
  window.addEventListener('beforeunload', flushSnap)
  window.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') flushSnap()
  })
}

const newId = () => Math.random().toString(36).slice(2, 9)

// ─── card node ────────────────────────────────────────────────────────

type CardData = {
  job: Job
  setExpanded: (tmux: string) => void
}

// Realtime activity bus. The relay's canvas WS pushes {type:'activity',
// tmux, ts} messages whenever a session's pane changes — these arrive
// faster than the /api/state poll. CardNode subscribes and overrides
// `attention()` to 'working' for the brief window after a push so the
// dashboard reacts immediately instead of waiting for the next poll.
const ActivityContext = React.createContext<{ at: Map<string, number>; tick: number }>({
  at: new Map(), tick: 0,
})
const ACTIVITY_HOLD_MS = 4000

function CardNode({ data, dragging }: NodeProps<Node<CardData, 'card'>>) {
  const { job, setExpanded } = data
  const activity = React.useContext(ActivityContext)
  let attn = attention(job)
  // Push-driven override: if the relay pinged us recently for this tmux,
  // treat the card as working for the next few seconds regardless of
  // what the slower polled activity array says.
  const lastPing = activity.at.get(job.tmux)
  if (lastPing && Date.now() - lastPing < ACTIVITY_HOLD_MS) {
    attn = { ...attn, level: 'working', reason: 'active' }
  }
  const c = LEVEL_COLOR[attn.level]
  const ci = ciStatus(job.last_check_conclusions ?? {})
  const ringClass = 'ring-zinc-200/80 dark:ring-zinc-700/70 ' + (
    attn.level === 'needs-you' ? 'card-status-needs '
    : attn.level === 'working' ? 'card-status-working '
    : attn.level === 'watching' ? 'card-status-watching '
    : ''
  )
  return (
    <div
      onClick={(e) => {
        if (dragging) return
        e.stopPropagation()
        setExpanded(job.tmux)
      }}
      className={
        'bg-white/95 dark:bg-zinc-900/95 backdrop-blur rounded-xl ring-1 shadow-sm hover:shadow-md cursor-pointer ' +
        ringClass
      }
      style={{ width: CARD_W, height: CARD_H }}
    >
      <Handle
        type="target"
        position={Position.Left}
        style={{ background: '#a78bfa', width: 8, height: 8, border: '1.5px solid white' }}
      />
      <Handle
        type="source"
        position={Position.Right}
        style={{ background: '#a78bfa', width: 8, height: 8, border: '1.5px solid white' }}
      />
      <CardCompact job={job} fact={factFor(job, attn, ci)} dot={c.bar} factColor={c.text} />
    </div>
  )
}

// ─── note node ────────────────────────────────────────────────────────

type NoteData = { text: string; onChange: (t: string) => void; onDelete: () => void }

function NoteNode({ data }: NodeProps<Node<NoteData, 'note'>>) {
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

// ─── text node ───

type TextData = {
  text: string
  onChange: (t: string) => void
  onDelete: () => void
  startEditing?: boolean
}

function TextNode({ data }: NodeProps<Node<TextData, 'text'>>) {
  const [editing, setEditing] = useState(!!data.startEditing)
  const ref = useRef<HTMLTextAreaElement | null>(null)
  useEffect(() => {
    if (editing) requestAnimationFrame(() => ref.current?.focus())
  }, [editing])
  return (
    <div
      className="relative group min-w-[60px]"
      onDoubleClick={(e) => { e.stopPropagation(); setEditing(true) }}
    >
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

// ─── link node ────────────────────────────────────────────────────────

type LinkVariant = 'youtube' | 'github-code' | 'gist' | 'docs' | 'meet' | 'pr' | 'issue' | 'generic'
type LinkData = {
  url: string
  title: string
  variant: LinkVariant
  image?: string
  description?: string
  site?: string
  snippet?: string
  onDelete: () => void
}

function detectVariant(url: string): { variant: LinkVariant; title: string; image?: string } {
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
      return {
        variant: 'youtube',
        title: 'youtube · ' + id,
        image: id ? `https://i.ytimg.com/vi/${id}/hqdefault.jpg` : undefined,
      }
    }
    if (host === 'docs.google.com') return { variant: 'docs', title: 'google docs' }
    if (host === 'meet.google.com') return { variant: 'meet', title: 'google meet ' + u.pathname.replace(/^\//, '') }
    return { variant: 'generic', title: host + u.pathname }
  } catch { return { variant: 'generic', title: url } }
}

async function fetchGitHubSnippet(url: string): Promise<string | undefined> {
  // github.com/{owner}/{repo}/blob/{ref}/{path} → raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}
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

async function fetchOG(url: string): Promise<Partial<LinkData>> {
  try {
    const res = await fetch(`/api/og?url=${encodeURIComponent(url)}`)
    if (!res.ok) return {}
    const j = await res.json() as Record<string, string>
    return {
      image: j['image'] || undefined,
      title: j['title'] || undefined,
      description: j['description'] || undefined,
      site: j['site'] || undefined,
    }
  } catch { return {} }
}

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

const LINK_W = 280
const LINK_H = 230

function LinkNode({ data }: NodeProps<Node<LinkData, 'link'>>) {
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
          <img
            src={data.image}
            alt=""
            className="w-full h-full object-cover"
            onPointerDown={(e) => e.stopPropagation()}
          />
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

// ─── stroke node ───

type StrokeData = { d: string; w: number; h: number; sw: number; onErase?: (id: string) => void; erasing?: boolean }

function StrokeNode({ id, data, selected }: NodeProps<Node<StrokeData, 'stroke'>>) {
  return (
    <svg
      width={data.w + data.sw * 2}
      height={data.h + data.sw * 2}
      viewBox={`${-data.sw} ${-data.sw} ${data.w + data.sw * 2} ${data.h + data.sw * 2}`}
      style={{ overflow: 'visible', cursor: data.erasing ? 'pointer' : 'move' }}
      onClick={(e) => {
        if (!data.erasing) return
        e.stopPropagation()
        data.onErase?.(id)
      }}
    >
      <path
        d={data.d}
        fill="none"
        strokeWidth={data.sw}
        strokeLinecap="round"
        strokeLinejoin="round"
        style={{ stroke: 'var(--ink)' }}
      />
      {selected && (
        <rect
          x={-2}
          y={-2}
          width={data.w + 4}
          height={data.h + 4}
          fill="none"
          stroke="#a78bfa"
          strokeDasharray="3 2"
          strokeWidth={0.8}
        />
      )}
    </svg>
  )
}

// ─── pane window node ───

type PaneNodeData = {
  tmux: string
  jobRef: { current: Map<string, Job> }
  onClose: (tmux: string) => void
}

function PaneWindowNode({ data, id, selected }: NodeProps<Node<PaneNodeData, 'pane'>>) {
  const job = data.jobRef.current.get(data.tmux)
  const ci = job ? ciStatus(job.last_check_conclusions ?? {}) : 'pending'
  const title = job?.issue_title || data.tmux
  return (
    <div className="rounded-lg overflow-hidden shadow-2xl ring-1 ring-black/40 flex flex-col bg-[#0b0b0e] w-full h-full">
      <NodeResizer
        minWidth={480}
        minHeight={320}
        isVisible={selected}
        lineClassName="!border-violet-400"
        handleClassName="!bg-violet-400"
      />
      <div className="h-8 bg-zinc-800/95 flex items-center px-3 gap-3 select-none flex-shrink-0 cursor-grab active:cursor-grabbing">
        <div className="flex gap-1.5 flex-shrink-0">
          <button
            onPointerDown={(e) => e.stopPropagation()}
            onClick={(e) => { e.stopPropagation(); data.onClose(data.tmux) }}
            className="w-3 h-3 rounded-full bg-rose-500 hover:bg-rose-400 transition-colors"
            title="close"
          />
          <span className="w-3 h-3 rounded-full bg-amber-400" />
          <span className="w-3 h-3 rounded-full bg-emerald-500" />
        </div>
        <div className="flex-1 min-w-0 text-center text-[12px] text-zinc-300 truncate">
          {title}
        </div>
        <div className="flex items-center gap-2 flex-shrink-0">
          {job?.pr && job?.target_repo && (
            <PRBadge repo={job.target_repo} pr={job.pr} ci={ci} />
          )}
        </div>
      </div>
      <div
        className="flex-1 min-h-0 nodrag nowheel"
        onPointerDown={(e) => e.stopPropagation()}
      >
        <Pane session={data.tmux} />
      </div>
    </div>
  )
  void id
}

function PRBadge({ repo, pr, ci }: { repo: string; pr: number; ci: 'fail' | 'pass' | 'pending' }) {
  const variant: 'open' | 'closed' | 'pending' =
    ci === 'fail' ? 'closed' : ci === 'pass' ? 'open' : 'pending'
  const color =
    variant === 'closed' ? 'text-rose-400 bg-rose-500/15 ring-rose-500/30'
    : variant === 'open' ? 'text-emerald-400 bg-emerald-500/15 ring-emerald-500/30'
    : 'text-amber-400 bg-amber-500/15 ring-amber-500/30'
  return (
    <a
      href={`https://github.com/${repo}/pull/${pr}`}
      target="_blank"
      rel="noopener noreferrer"
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      className={`mono inline-flex items-center gap-1 text-[11px] px-1.5 py-0.5 rounded ring-1 ring-inset ${color}`}
      title={`PR #${pr} · ${variant}`}
    >
      <PRIcon variant={variant} />
      #{pr}
    </a>
  )
}

function PRIcon({ variant }: { variant: 'open' | 'closed' | 'pending' }) {
  if (variant === 'closed') {
    return (
      <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
        <path d="M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 14a2.25 2.25 0 1 1 0-4.5 2.25 2.25 0 0 1 0 4.5ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm-2.03-9.53a.75.75 0 0 1 1.06 0L13 3.69l1.22-1.22a.75.75 0 1 1 1.06 1.06L14.06 4.75l1.22 1.22a.75.75 0 1 1-1.06 1.06L13 5.81l-1.22 1.22a.75.75 0 1 1-1.06-1.06l1.22-1.22-1.22-1.22a.75.75 0 0 1 0-1.06Z"/>
      </svg>
    )
  }
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
      <path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z"/>
    </svg>
  )
}

const nodeTypes: NodeTypes = { card: CardNode, note: NoteNode, link: LinkNode, text: TextNode, stroke: StrokeNode, pane: PaneWindowNode }

// ─── pen overlay ──────────────────────────────────────────────────────

function PenLayer({
  active,
  containerRef,
  onStroke,
}: {
  active: boolean
  containerRef: React.RefObject<HTMLDivElement>
  onStroke: (s: Stroke) => void
}) {
  const viewport = useViewport()
  const [current, setCurrent] = useState<{ x: number; y: number }[]>([])
  const drawingRef = useRef(false)

  const toWorld = (clientX: number, clientY: number) => {
    const r = containerRef.current?.getBoundingClientRect()
    if (!r) return { x: 0, y: 0 }
    return {
      x: (clientX - r.left - viewport.x) / viewport.zoom,
      y: (clientY - r.top - viewport.y) / viewport.zoom,
    }
  }

  const onPointerDown = (e: React.PointerEvent) => {
    if (!active) return
    e.preventDefault()
    drawingRef.current = true
    setCurrent([toWorld(e.clientX, e.clientY)])
    ;(e.target as Element).setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    if (!drawingRef.current) return
    setCurrent((c) => [...c, toWorld(e.clientX, e.clientY)])
  }
  const onPointerUp = (e: React.PointerEvent) => {
    if (!drawingRef.current) return
    drawingRef.current = false
    ;(e.target as Element).releasePointerCapture(e.pointerId)
    if (current.length > 1) {
      const xs = current.map((p) => p.x)
      const ys = current.map((p) => p.y)
      const minX = Math.min(...xs), minY = Math.min(...ys)
      const maxX = Math.max(...xs), maxY = Math.max(...ys)
      const local = current.map((p) => ({ x: p.x - minX, y: p.y - minY }))
      onStroke({
        id: newId(),
        x: minX,
        y: minY,
        w: maxX - minX,
        h: maxY - minY,
        d: pointsToPath(local),
        width: 1.8,
      })
    }
    setCurrent([])
  }

  return (
    <svg
      className="absolute inset-0"
      style={{
        width: '100%', height: '100%',
        pointerEvents: active ? 'auto' : 'none',
        zIndex: active ? 30 : 1,
        cursor: active ? 'crosshair' : 'default',
      }}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
    >
      <g transform={`translate(${viewport.x},${viewport.y}) scale(${viewport.zoom})`}>
        {current.length > 1 && (
          <path
            d={pointsToPath(current)}
            strokeWidth={1.8}
            fill="none"
            strokeLinejoin="round"
            strokeLinecap="round"
            style={{ stroke: 'var(--ink)' }}
          />
        )}
      </g>
    </svg>
  )
}

function pointsToPath(points: { x: number; y: number }[]): string {
  if (points.length === 0) return ''
  let d = `M ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)}`
  for (let i = 1; i < points.length; i++) {
    d += ` L ${points[i].x.toFixed(1)} ${points[i].y.toFixed(1)}`
  }
  return d
}

function makeCardNode(
  job: Job,
  pos: { x: number; y: number },
  setExpanded: (tmux: string) => void,
): Node<CardData, 'card'> {
  return {
    id: job.tmux,
    type: 'card',
    position: { x: pos.x, y: pos.y },
    data: { job, setExpanded },
    style: { width: CARD_W, height: CARD_H },
    draggable: true,
    zIndex: 2,
  }
}

// ─── dashboard ────────────────────────────────────────────────────────

export function Dashboard({ state }: Props) {
  return (
    <ReactFlowProvider>
      <DashboardInner state={state} />
    </ReactFlowProvider>
  )
}

function DashboardInner({ state }: Props) {
  const { jobs = [], inbox = '' } = state
  const snapRef = useRef<Snap>(emptySnap())
  const [snapLoaded, setSnapLoaded] = useState(false)
  const jobsByTmuxRef = useRef<Map<string, Job>>(new Map())
  const [nodes, setNodes] = useNodesState<Node>([])
  const [edges, setEdges] = useEdgesState<Edge>(snapRef.current.edges)
  const [tool, setTool] = useState<Tool>('select')
  const [strokes, setStrokes] = useState<Stroke[]>(snapRef.current.strokes)
  const [composerAt, setComposerAt] = useState<{ x: number; y: number } | null>(null)
  // Only auto-show the header composer on the empty-canvas first run.
  // Once there are any cards, dismiss permanently — clicking/dragging/moving
  // also dismisses.
  const [headerComposerDismissed, setHeaderComposerDismissed] = useState(false)
  const lastPaneClickRef = useRef(0)
  const containerRef = useRef<HTMLDivElement>(null)
  const sendRef = useRef<(msg: any) => void>(() => {})

  const persist = useCallback(() => { saveSnap(snapRef.current) }, [])

  // Pull canvas layout from server on first mount. Until it arrives,
  // render an empty canvas (cards still appear from /api/state polling).
  useEffect(() => {
    let alive = true
    fetchSnap().then((s) => {
      if (!alive) return
      snapRef.current = s
      setEdges(s.edges)
      setStrokes(s.strokes)
      if (s.view) setView(s.view)
      setSnapLoaded(true)
    })
    return () => { alive = false }
  }, [])

  const makePaneNode = useCallback((tmux: string, x: number, y: number, w = 720, h = 480): Node<PaneNodeData, 'pane'> => ({
    id: 'pane:' + tmux,
    type: 'pane',
    position: { x, y },
    data: {
      tmux,
      jobRef: jobsByTmuxRef,
      onClose: (t: string) => closePane(t),
    },
    style: { width: w, height: h },
    zIndex: 50,
    draggable: true,
    selectable: true,
  }), [])

  const openPane = useCallback((tmux: string) => {
    const paneId = 'pane:' + tmux
    // Default position: a step away from the card so it doesn't fully cover it.
    const card = snapRef.current.cards[tmux]
    const stored = snapRef.current.panes?.[tmux]
    const x = stored?.x ?? (card ? card.x + CARD_W + 40 : 200)
    const y = stored?.y ?? (card ? card.y : 240)
    const w = stored?.w ?? 720
    const h = stored?.h ?? 480
    if (!snapRef.current.panes) snapRef.current.panes = {}
    snapRef.current.panes[tmux] = { x, y, w, h }
    persist()
    setNodes((nds) => nds.some((n) => n.id === paneId)
      ? nds
      : [...nds, makePaneNode(tmux, x, y, w, h)])
    sendRef.current({ type: 'pane:open', tmux, x, y, w, h })
  }, [persist, setNodes, makePaneNode])

  const closePane = useCallback((tmux: string) => {
    const paneId = 'pane:' + tmux
    setNodes((nds) => nds.filter((n) => n.id !== paneId))
    if (snapRef.current.panes) {
      delete snapRef.current.panes[tmux]
      persist()
    }
    sendRef.current({ type: 'pane:close', tmux })
  }, [persist, setNodes])

  const setExpanded = openPane

  // ─── user node bindings (notes + links) ───
  const updateNote = useCallback((id: string, text: string) => {
    const u = snapRef.current.user.find((n) => n.id === id)
    if (u) { u.data.text = text; persist() }
    setNodes((nds) => nds.map((n) => n.id === id ? { ...n, data: { ...(n.data as any), text } } : n))
    sendRef.current({ type: 'note:upsert', id, text })
  }, [setNodes, persist])
  const deleteUserNode = useCallback((id: string) => {
    const u = snapRef.current.user.find((n) => n.id === id)
    const kind = u?.type
    snapRef.current.user = snapRef.current.user.filter((u) => u.id !== id)
    persist()
    setNodes((nds) => nds.filter((n) => n.id !== id))
    sendRef.current({ type: (kind === 'link' ? 'link:delete' : 'note:delete'), id })
  }, [setNodes, persist])

  const makeNoteNode = useCallback((u: UserNode): Node<NoteData, 'note'> => ({
    id: u.id,
    type: 'note',
    position: { x: u.x, y: u.y },
    data: {
      text: (u.data.text as string) ?? '',
      onChange: (t: string) => updateNote(u.id, t),
      onDelete: () => deleteUserNode(u.id),
    },
    style: { width: NOTE_W, height: NOTE_H },
  }), [updateNote, deleteUserNode])

  const makeTextNode = useCallback((u: UserNode, startEditing = false): Node<TextData, 'text'> => ({
    id: u.id,
    type: 'text',
    position: { x: u.x, y: u.y },
    data: {
      text: (u.data.text as string) ?? '',
      onChange: (t: string) => updateNote(u.id, t),
      onDelete: () => deleteUserNode(u.id),
      startEditing,
    },
  }), [updateNote, deleteUserNode])

  const makeLinkNode = useCallback((u: UserNode): Node<LinkData, 'link'> => ({
    id: u.id,
    type: 'link',
    position: { x: u.x, y: u.y },
    data: {
      url: u.data.url as string,
      title: u.data.title as string,
      variant: u.data.variant as LinkVariant,
      image: u.data.image as string | undefined,
      description: u.data.description as string | undefined,
      site: u.data.site as string | undefined,
      snippet: u.data.snippet as string | undefined,
      onDelete: () => deleteUserNode(u.id),
    },
    style: { width: LINK_W, height: LINK_H },
  }), [deleteUserNode])

  const enrichLink = useCallback(async (u: UserNode) => {
    const url = u.data.url as string
    const variant = u.data.variant as LinkVariant
    const updates: Partial<LinkData> = {}
    if (variant === 'github-code' && !u.data.snippet) {
      const snippet = await fetchGitHubSnippet(url)
      if (snippet) updates.snippet = snippet
    }
    if (variant !== 'youtube' && !u.data.image) {
      const og = await fetchOG(url)
      if (og.image) updates.image = og.image
      if (og.title && !u.data.title) updates.title = og.title
      if (og.description) updates.description = og.description
      if (og.site) updates.site = og.site
    }
    if (Object.keys(updates).length === 0) return
    Object.assign(u.data, updates)
    persist()
    setNodes((nds) => nds.map((n) => n.id === u.id ? makeLinkNode(u) : n))
    sendRef.current({ type: 'link:upsert', id: u.id, x: u.x, y: u.y, data: u.data })
  }, [persist, setNodes, makeLinkNode])

  // ─── card sync ───
  // Keep a jobs lookup synced for pane nodes that read it lazily.
  useEffect(() => {
    jobsByTmuxRef.current = new Map(jobs.filter((j) => j.tmux).map((j) => [j.tmux, j]))
  }, [jobs])

  // Skip rebuilds while a drag is in flight. React Flow loses its internal
  // drag tracking if the nodes prop is replaced mid-drag, which previously
  // reverted card positions to their pre-drag values.
  const draggingRef = useRef(false)
  const [view, setView] = useState<'canvas' | 'list'>('canvas')
  const [settingsOpen, setSettingsOpen] = useState(false)
  // In list view, clicking a row opens this pane modal instead of a
  // canvas node (which doesn't render in list mode).
  const [listExpanded, setListExpanded] = useState<string | null>(null)
  // Last-seen activity timestamp per tmux. Updated from the canvas WS;
  // CardNode reads via ActivityContext for sub-poll-latency indicators.
  const activityAtRef = useRef<Map<string, number>>(new Map())
  const [activityTick, setActivityTick] = useState(0)
  const activityCtx = useMemo(
    () => ({ at: activityAtRef.current, tick: activityTick }),
    [activityTick],
  )

  useEffect(() => {
    // Defer until the server snap arrives. Otherwise the first state poll
    // races ahead with an empty snap, lays out cards on the default grid,
    // and the saveSnap below clobbers the persisted positions.
    if (!snapLoaded) return
    if (draggingRef.current) return // see draggingRef comment above
    setNodes((current) => {
      const snap = snapRef.current
      const haveCard = new Map<string, Node>()
      for (const n of current) if (n.type === 'card') haveCard.set(n.id, n)
      const live = new Set(jobs.filter((j) => j.tmux).map((j) => j.tmux))

      const result: Node[] = []
      // Keep non-card nodes as-is.
      for (const n of current) if (n.type !== 'card') result.push(n)
      // Keep live card nodes — preserve React Flow's position (the user's
      // last drag) verbatim. The rebuild effect MUST NOT touch snap.cards
      // for live cards; that's the job of onNodeDragStop / onNodesChange.
      for (const n of current) {
        if (n.type !== 'card') continue
        if (!live.has(n.id)) continue
        const job = jobs.find((j) => j.tmux === n.id)!
        result.push({
          ...n,
          data: { ...(n.data as CardData), job, setExpanded },
        })
      }
      // Restore / place new card nodes.
      const used = new Set<string>()
      for (const c of Object.values(snap.cards)) {
        const col = Math.round(c.x / (CARD_W + GAP))
        const row = Math.round((c.y - HEADER_OFFSET) / (CARD_H + GAP))
        used.add(`${col},${row}`)
      }
      const newCards: Node[] = []
      let col = 0, row = 0
      for (const j of jobs) {
        if (!j.tmux || haveCard.has(j.tmux)) continue
        const persisted = snap.cards[j.tmux]
        if (persisted) {
          newCards.push(makeCardNode(j, persisted, setExpanded))
          continue
        }
        while (used.has(`${col},${row}`)) {
          col++
          if (col >= COLS) { col = 0; row++ }
        }
        used.add(`${col},${row}`)
        const pos = { x: col * (CARD_W + GAP), y: HEADER_OFFSET + row * (CARD_H + GAP) }
        snap.cards[j.tmux] = pos
        newCards.push(makeCardNode(j, pos, setExpanded))
        col++
        if (col >= COLS) { col = 0; row++ }
      }
      // Prune cards whose sessions are gone — but only when we actually
      // have a live jobs list. An empty jobs prop is the normal initial
      // state (before /api/state has returned), and wiping snap.cards in
      // that window caused the next poll to lay everything out on the
      // default grid and overwrite the persisted layout.
      if (jobs.length > 0) {
        for (const id of Object.keys(snap.cards)) {
          if (!live.has(id)) delete snap.cards[id]
        }
      }
      // Drop edges that reference sessions that are gone.
      const validIds = new Set([...live, ...snap.user.map((u) => u.id)])
      const prunedEdges = snap.edges.filter((e) => validIds.has(e.source) && validIds.has(e.target))
      if (prunedEdges.length !== snap.edges.length) {
        snap.edges = prunedEdges
        setEdges(prunedEdges)
      }
      // User nodes (notes/links/text) live in current already; if none, rehydrate.
      if (!current.some((n) => n.type === 'note' || n.type === 'link' || n.type === 'text')) {
        for (const u of snap.user) {
          if (u.type === 'note') result.push(makeNoteNode(u))
          else if (u.type === 'link') result.push(makeLinkNode(u))
          else if (u.type === 'text') result.push(makeTextNode(u))
        }
      }
      // Strokes — restore as nodes if they're not present yet.
      if (!current.some((n) => n.type === 'stroke')) {
        for (const s of snap.strokes) result.push(makeStrokeNode(s))
      }
      // Pane windows — restore from snap if none present.
      if (!current.some((n) => n.type === 'pane')) {
        for (const [tmux, p] of Object.entries(snap.panes ?? {})) {
          if (!live.has(tmux)) continue
          result.push(makePaneNode(tmux, p.x, p.y, p.w, p.h))
        }
      }
      result.push(...newCards)
      // Persist only when the rebuild actually mutated snap.cards (new
      // cards placed, dead ones pruned). Saving on every poll spams PUTs
      // and was racing user drags.
      if (newCards.length > 0) saveSnap(snap)
      return result
    })
  }, [jobs, snapLoaded, setNodes, setExpanded, makeNoteNode, makeLinkNode, makeTextNode, makePaneNode])

  const onEdgesChange = useCallback((changes: EdgeChange[]) => {
    setEdges((eds) => {
      const next = applyEdgeChanges(changes, eds)
      snapRef.current.edges = next
      persist()
      return next
    })
    for (const ch of changes) {
      if (ch.type === 'remove') sendRef.current({ type: 'edge:remove', id: ch.id })
    }
  }, [setEdges, persist])

  const buildEdge = (conn: Connection): Edge => ({
    ...conn,
    id: `e_${conn.source}_${conn.target}_${newId()}`,
    type: 'smoothstep',
    animated: false,
    selectable: true,
    focusable: true,
    deletable: true,
    interactionWidth: 24,
    markerEnd: { type: MarkerType.ArrowClosed, color: '#a78bfa', width: 18, height: 18 },
    style: { stroke: '#a78bfa', strokeWidth: 1.6 },
    label: 'then',
    labelStyle: { fontSize: 10, fontFamily: 'ui-monospace, monospace', fill: '#7c3aed' },
    labelBgStyle: { fill: '#faf5ff' },
    labelBgPadding: [4, 2] as [number, number],
    labelBgBorderRadius: 4,
  })

  const applyEdgeAdd = useCallback((edge: Edge) => {
    setEdges((eds) => {
      if (eds.some((e) => e.id === edge.id)) return eds
      const next = [...eds, edge]
      snapRef.current.edges = next
      persist()
      return next
    })
  }, [setEdges, persist])

  const onConnect = useCallback((conn: Connection) => {
    if (!conn.source || !conn.target || conn.source === conn.target) return
    const edge = buildEdge(conn)
    applyEdgeAdd(edge)
    sendRef.current({ type: 'edge:add', edge })
  }, [applyEdgeAdd])

  const onNodesChange = useCallback((changes: NodeChange[]) => {
    setNodes((nds) => applyNodeChanges(changes, nds))
    for (const ch of changes) {
      if (ch.type === 'position' && ch.position) {
        const id = ch.id
        const pos = ch.position
        if (snapRef.current.cards[id]) {
          snapRef.current.cards[id] = pos
        } else if (id.startsWith('pane:')) {
          const tmux = id.slice(5)
          if (!snapRef.current.panes) snapRef.current.panes = {}
          const prev = snapRef.current.panes[tmux] ?? { w: 720, h: 480, x: 0, y: 0 }
          snapRef.current.panes[tmux] = { ...prev, x: pos.x, y: pos.y }
        } else {
          const u = snapRef.current.user.find((n) => n.id === id)
          if (u) { u.x = pos.x; u.y = pos.y }
          const s = snapRef.current.strokes.find((s) => s.id === id)
          if (s) { s.x = pos.x; s.y = pos.y }
        }
        persist()
        sendRef.current({ type: 'node:move', id, x: pos.x, y: pos.y })
      }
      if (ch.type === 'dimensions' && ch.dimensions && (ch.id.startsWith('pane:'))) {
        const tmux = ch.id.slice(5)
        if (!snapRef.current.panes) snapRef.current.panes = {}
        const prev = snapRef.current.panes[tmux] ?? { x: 0, y: 0, w: 720, h: 480 }
        snapRef.current.panes[tmux] = { ...prev, w: ch.dimensions.width, h: ch.dimensions.height }
        persist()
        sendRef.current({ type: 'pane:resize', tmux, w: ch.dimensions.width, h: ch.dimensions.height })
      }
      if (ch.type === 'remove') {
        const id = ch.id
        // Strokes: also drop from snap so the rehydrate effect doesn't
        // resurrect them on the next state poll.
        const strokeIdx = snapRef.current.strokes.findIndex((s) => s.id === id)
        if (strokeIdx >= 0) {
          snapRef.current.strokes.splice(strokeIdx, 1)
          persist()
          sendRef.current({ type: 'stroke:remove', id })
          continue
        }
        const userIdx = snapRef.current.user.findIndex((u) => u.id === id)
        if (userIdx >= 0) {
          const kind = snapRef.current.user[userIdx].type
          snapRef.current.user.splice(userIdx, 1)
          persist()
          sendRef.current({ type: kind === 'link' ? 'link:delete' : 'note:delete', id })
        }
      }
    }
  }, [setNodes, persist])

  // ─── pen / eraser ───
  const toolRef = useRef<Tool>(tool)
  toolRef.current = tool

  // Forward-declared so makeStrokeNode below can reference it; assigned via
  // ref after applyStrokeRemove is in scope.
  const removeStrokeRef = useRef<(id: string) => void>(() => {})

  const makeStrokeNode = useCallback((s: Stroke): Node<StrokeData, 'stroke'> => ({
    id: s.id,
    type: 'stroke',
    position: { x: s.x, y: s.y },
    data: {
      d: s.d, w: s.w, h: s.h, sw: s.width,
      erasing: toolRef.current === 'eraser',
      onErase: (id: string) => {
        removeStrokeRef.current(id)
        sendRef.current({ type: 'stroke:remove', id })
      },
    },
    draggable: true,
    selectable: true,
    zIndex: 1,
  }), [])

  const applyStrokeAdd = useCallback((s: Stroke) => {
    if (snapRef.current.strokes.some((x) => x.id === s.id)) return
    snapRef.current.strokes = [...snapRef.current.strokes, s]
    persist()
    setStrokes(snapRef.current.strokes)
    setNodes((nds) => nds.some((n) => n.id === s.id) ? nds : [...nds, makeStrokeNode(s)])
  }, [persist, setNodes, makeStrokeNode])
  const applyStrokeRemove = useCallback((id: string) => {
    const next = snapRef.current.strokes.filter((s) => s.id !== id)
    if (next.length === snapRef.current.strokes.length) return
    snapRef.current.strokes = next
    persist()
    setStrokes(next)
    setNodes((nds) => nds.filter((n) => n.id !== id))
  }, [persist, setNodes])
  useEffect(() => { removeStrokeRef.current = applyStrokeRemove }, [applyStrokeRemove])
  const addStroke = useCallback((s: Stroke) => {
    applyStrokeAdd(s)
    sendRef.current({ type: 'stroke:add', stroke: s })
  }, [applyStrokeAdd])
  // Keep stroke nodes' `erasing` data in sync when the tool changes so the
  // cursor/click behaviour flips without a remount.
  useEffect(() => {
    setNodes((nds) => nds.map((n) =>
      n.type === 'stroke'
        ? { ...n, data: { ...(n.data as StrokeData), erasing: tool === 'eraser' } }
        : n,
    ))
  }, [tool, setNodes])

  // ─── add text ───
  const rf = useReactFlow()
  const addTextAt = useCallback((screenX: number, screenY: number) => {
    const pos = rf.screenToFlowPosition({ x: screenX, y: screenY })
    const id = newId()
    const u: UserNode = { type: 'text', id, x: pos.x, y: pos.y, data: { text: '' } }
    snapRef.current.user.push(u)
    persist()
    setNodes((nds) => [...nds, makeTextNode(u, true)])
    setTool('select')
    sendRef.current({ type: 'note:upsert', kind: 'text', id, x: pos.x, y: pos.y, text: '' })
  }, [rf, persist, setNodes, makeTextNode, setTool])

  // ─── add note ───
  const addNote = useCallback(() => {
    const id = newId()
    // Place near current viewport center.
    const r = containerRef.current?.getBoundingClientRect()
    const x = ((r?.width ?? 800) / 2 - 110) - 0
    const y = HEADER_OFFSET + 40
    const u: UserNode = { type: 'note', id, x, y, data: { text: '' } }
    snapRef.current.user.push(u)
    persist()
    setNodes((nds) => [...nds, makeNoteNode(u)])
    setTool('select')
    sendRef.current({ type: 'note:upsert', id, x, y, text: '' })
  }, [persist, setNodes, makeNoteNode, setTool])

  // ─── paste handler ───
  useEffect(() => {
    const onPaste = (e: ClipboardEvent) => {
      // Ignore pastes targeting form fields.
      const tag = (e.target as HTMLElement)?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA') return
      const text = e.clipboardData?.getData('text/plain')?.trim()
      if (!text) return
      if (!/^https?:\/\//i.test(text)) return
      e.preventDefault()
      const { variant, title, image } = detectVariant(text)
      const id = newId()
      const r = containerRef.current?.getBoundingClientRect()
      const x = ((r?.width ?? 800) / 2 - 140)
      const y = HEADER_OFFSET + 60
      const u: UserNode = {
        type: 'link', id, x, y,
        data: { url: text, variant, title, ...(image ? { image } : {}) },
      }
      snapRef.current.user.push(u)
      persist()
      setNodes((nds) => [...nds, makeLinkNode(u)])
      enrichLink(u)
      sendRef.current({ type: 'link:upsert', id, x, y, data: u.data })
    }
    document.addEventListener('paste', onPaste)
    return () => document.removeEventListener('paste', onPaste)
  }, [persist, setNodes, makeLinkNode, enrichLink])

  // Enrich any restored link nodes that don't yet have og/snippet data.
  useEffect(() => {
    for (const u of snapRef.current.user) {
      if (u.type !== 'link') continue
      const variant = u.data.variant as LinkVariant
      if ((variant === 'github-code' && !u.data.snippet) || (variant !== 'youtube' && !u.data.image)) {
        enrichLink(u)
      }
    }
    // intentionally only run on mount
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // ─── collab receive — apply remote ops to local state ───
  const handleRemote = useCallback((msg: any) => {
    switch (msg.type) {
      case 'activity': {
        const tmux = msg.tmux as string
        if (!tmux) break
        activityAtRef.current.set(tmux, Date.now())
        setActivityTick((t) => t + 1)
        break
      }
      case 'stroke:add':
        if (msg.stroke) applyStrokeAdd(msg.stroke as Stroke)
        break
      case 'stroke:remove':
        if (msg.id) applyStrokeRemove(msg.id as string)
        break
      case 'node:move': {
        const id = msg.id as string
        const x = msg.x as number
        const y = msg.y as number
        if (typeof x !== 'number' || typeof y !== 'number') break
        if (snapRef.current.cards[id]) {
          snapRef.current.cards[id] = { x, y }
        } else {
          const u = snapRef.current.user.find((n) => n.id === id)
          if (u) { u.x = x; u.y = y }
          const s = snapRef.current.strokes.find((s) => s.id === id)
          if (s) { s.x = x; s.y = y }
        }
        persist()
        setNodes((nds) => nds.map((n) => n.id === id ? { ...n, position: { x, y } } : n))
        break
      }
      case 'edge:add':
        if (msg.edge) applyEdgeAdd(msg.edge as Edge)
        break
      case 'edge:remove':
        if (msg.id) {
          setEdges((eds) => {
            const next = eds.filter((e) => e.id !== msg.id)
            if (next.length === eds.length) return eds
            snapRef.current.edges = next
            persist()
            return next
          })
        }
        break
      case 'pane:open': {
        const tmux = msg.tmux as string
        if (!tmux) break
        const x = (msg.x as number) ?? 200
        const y = (msg.y as number) ?? 240
        const w = (msg.w as number) ?? 720
        const h = (msg.h as number) ?? 480
        if (!snapRef.current.panes) snapRef.current.panes = {}
        snapRef.current.panes[tmux] = { x, y, w, h }
        persist()
        const paneId = 'pane:' + tmux
        setNodes((nds) => nds.some((n) => n.id === paneId)
          ? nds
          : [...nds, makePaneNode(tmux, x, y, w, h)])
        break
      }
      case 'pane:close': {
        const tmux = msg.tmux as string
        if (!tmux) break
        if (snapRef.current.panes) {
          delete snapRef.current.panes[tmux]
          persist()
        }
        const paneId = 'pane:' + tmux
        setNodes((nds) => nds.filter((n) => n.id !== paneId))
        break
      }
      case 'pane:resize': {
        const tmux = msg.tmux as string
        const w = msg.w as number, h = msg.h as number
        if (!tmux || typeof w !== 'number' || typeof h !== 'number') break
        if (!snapRef.current.panes) snapRef.current.panes = {}
        const prev = snapRef.current.panes[tmux] ?? { x: 0, y: 0, w: 720, h: 480 }
        snapRef.current.panes[tmux] = { ...prev, w, h }
        persist()
        const paneId = 'pane:' + tmux
        setNodes((nds) => nds.map((n) => n.id === paneId
          ? { ...n, style: { ...n.style, width: w, height: h } } : n))
        break
      }
      case 'note:upsert': {
        const id = msg.id as string
        const text = (msg.text ?? '') as string
        const kind = (msg.kind === 'text' ? 'text' : 'note') as 'text' | 'note'
        let u = snapRef.current.user.find((n) => n.id === id)
        if (!u) {
          u = { type: kind, id, x: msg.x as number, y: msg.y as number, data: { text } }
          snapRef.current.user.push(u)
          setNodes((nds) => nds.some((n) => n.id === id) ? nds : [
            ...nds,
            kind === 'text' ? makeTextNode(u!) : makeNoteNode(u!),
          ])
        } else {
          u.data.text = text
          setNodes((nds) => nds.map((n) => n.id === id
            ? { ...n, data: { ...(n.data as any), text } } : n))
        }
        persist()
        break
      }
      case 'note:delete':
      case 'link:delete': {
        const id = msg.id as string
        snapRef.current.user = snapRef.current.user.filter((u) => u.id !== id)
        persist()
        setNodes((nds) => nds.filter((n) => n.id !== id))
        break
      }
      case 'link:upsert': {
        const id = msg.id as string
        const data = (msg.data ?? {}) as Record<string, unknown>
        let u = snapRef.current.user.find((n) => n.id === id)
        if (!u) {
          u = { type: 'link', id, x: msg.x as number, y: msg.y as number, data }
          snapRef.current.user.push(u)
          setNodes((nds) => nds.some((n) => n.id === id) ? nds : [...nds, makeLinkNode(u!)])
        } else {
          Object.assign(u.data, data)
          setNodes((nds) => nds.map((n) => n.id === id ? makeLinkNode(u!) : n))
        }
        persist()
        break
      }
    }
  }, [applyStrokeAdd, applyStrokeRemove, applyEdgeAdd, setNodes, setEdges, persist, makeNoteNode, makeLinkNode, makeTextNode, makePaneNode])

  const { cursors, sendCursor, send } = useCollabSocket({ onMessage: handleRemote })
  useEffect(() => { sendRef.current = send }, [send])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (tool !== 'select') { setTool('select'); return }
        setNodes((nds) => {
          if (!nds.some((n) => n.type === 'pane')) return nds
          // Close all open pane windows on Escape.
          return nds.filter((n) => n.type !== 'pane')
        })
      }
      if ((e.target as HTMLElement).tagName === 'INPUT' || (e.target as HTMLElement).tagName === 'TEXTAREA') return
      if (e.key === 'v') setTool('select')
      if (e.key === 'r') setTool('box')
      if (e.key === 'p') setTool('pen')
      if (e.key === 'e') setTool('eraser')
      if (e.key === 't') setTool('text')
      if (e.key === 'n') addNote()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [tool, setExpanded, addNote])

  return (
    <ActivityContext.Provider value={activityCtx}>
    <div ref={containerRef} className="relative h-screen w-screen">
      <Header
        inbox={inbox}
        count={jobs.length}
        showComposer={!headerComposerDismissed && jobs.length === 0}
        view={view}
        setView={(v) => {
          setView(v)
          snapRef.current.view = v
          persist()
        }}
        onOpenSettings={() => setSettingsOpen(true)}
      />
      {settingsOpen && <SettingsPanel onClose={() => setSettingsOpen(false)} />}
      {view === 'canvas' && (
        <FloatingToolbar tool={tool} setTool={setTool} addNote={addNote} />
      )}
      {view === 'canvas' && (
        <CollabLayer cursors={cursors} sendCursor={sendCursor} containerRef={containerRef} />
      )}
      {view === 'list' && (
        <ListView jobs={jobs} onOpen={(t) => setListExpanded(t)} />
      )}
      {view === 'list' && listExpanded && (
        <PaneModal tmux={listExpanded} jobsByTmuxRef={jobsByTmuxRef} onClose={() => setListExpanded(null)} />
      )}
      {view === 'canvas' && <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onConnect={onConnect}
        onPaneClick={(e) => {
          setHeaderComposerDismissed(true)
          if (tool === 'text') {
            addTextAt(e.clientX, e.clientY)
            return
          }
          if (tool !== 'select') return
          const now = Date.now()
          const isDouble = now - lastPaneClickRef.current < 400
          lastPaneClickRef.current = now
          if (!isDouble) return
          setComposerAt({ x: e.clientX, y: e.clientY })
        }}
        onNodeDragStart={() => {
          draggingRef.current = true
          setHeaderComposerDismissed(true)
        }}
        onNodeDragStop={(_e, node) => {
          draggingRef.current = false
          const id = node.id
          const pos = node.position
          if (snapRef.current.cards[id]) {
            snapRef.current.cards[id] = { x: pos.x, y: pos.y }
          } else if (id.startsWith('pane:')) {
            const tmux = id.slice(5)
            if (!snapRef.current.panes) snapRef.current.panes = {}
            const prev = snapRef.current.panes[tmux] ?? { w: 720, h: 480, x: 0, y: 0 }
            snapRef.current.panes[tmux] = { ...prev, x: pos.x, y: pos.y }
          } else {
            const u = snapRef.current.user.find((n) => n.id === id)
            if (u) { u.x = pos.x; u.y = pos.y }
            const s = snapRef.current.strokes.find((s) => s.id === id)
            if (s) { s.x = pos.x; s.y = pos.y }
          }
          persist()
          sendRef.current({ type: 'node:move', id, x: pos.x, y: pos.y })
        }}
        onMove={() => setHeaderComposerDismissed(true)}
        selectionOnDrag={tool === 'box'}
        selectionMode={'partial' as any}
        onMoveEnd={(_e, viewport) => {
          snapRef.current.viewport = viewport
          persist()
        }}
        proOptions={{ hideAttribution: true }}
        panOnDrag={tool === 'select' ? true : (tool === 'box' ? [1, 2] : false)}
        nodesDraggable={tool === 'select' || tool === 'box'}
        panOnScroll={false}
        zoomOnScroll
        zoomOnPinch
        zoomOnDoubleClick={false}
        minZoom={0.25}
        maxZoom={2}
        nodesConnectable={tool === 'select'}
        nodesFocusable
        edgesFocusable
        elementsSelectable
        deleteKeyCode={['Backspace', 'Delete']}
        defaultViewport={snapRef.current.viewport ?? { x: 0, y: 0, zoom: 1 }}
        fitView={false}
      >
        <Background variant={BackgroundVariant.Dots} gap={22} size={1.4} color="#d4d4d8" />
      </ReactFlow>}
      {view === 'canvas' && (
        <PenLayer
          active={tool === 'pen'}
          containerRef={containerRef}
          onStroke={addStroke}
        />
      )}
      {composerAt && (
        <FloatingComposer
          at={composerAt}
          onDismiss={() => setComposerAt(null)}
        />
      )}
    </div>
    </ActivityContext.Provider>
  )
}

function Header({
  count, showComposer, view, setView, onOpenSettings,
}: {
  inbox: string; count: number; showComposer: boolean
  view: 'canvas' | 'list'; setView: (v: 'canvas' | 'list') => void
  onOpenSettings: () => void
}) {
  // Stop pointer events on the entire header row so clicks on the title /
  // toggle / link don't bubble through to the ReactFlow pane behind it.
  return (
    <div className="fixed top-0 inset-x-0 z-40 pointer-events-none">
      <div className="px-8 pt-6 flex flex-col gap-3">
        <div
          className="pointer-events-auto flex items-baseline gap-3"
          onPointerDown={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >
          <h1 className="serif text-[44px] font-medium leading-none text-zinc-900 dark:text-zinc-100 italic">
            Orchid
          </h1>
          <span className="mono text-[12px] text-zinc-400 dark:text-zinc-500">{count}</span>
          <div className="flex-1" />
          <HeaderBtnBar>
            <ViewToggle view={view} setView={setView} />
            <SettingsButton onClick={onOpenSettings} />
            <ThemeToggle />
            <LogoutButton />
          </HeaderBtnBar>
        </div>
        {showComposer && (
          <div
            className="pointer-events-auto max-w-[820px] mx-auto w-full"
            onPointerDown={(e) => e.stopPropagation()}
            onClick={(e) => e.stopPropagation()}
          >
            <Composer />
          </div>
        )}
      </div>
    </div>
  )
}

/// Pill container around the top-right action buttons. Matches the
/// FloatingToolbar's chrome (white/zinc-900 backdrop, ring, shadow).
function HeaderBtnBar({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-1 bg-white/95 dark:bg-zinc-900/95 backdrop-blur ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-xl px-1.5 py-1.5 shadow-lg shadow-zinc-300/40 dark:shadow-black/40">
      {children}
    </div>
  )
}

function SettingsButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title="Settings"
      className="w-9 h-9 rounded-lg flex items-center justify-center transition-colors text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1 1.55V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 9 19.4a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.6 15a1.7 1.7 0 0 0-1.55-1H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.6 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1-1.55V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1 1.55 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87V9c.21.51.7.92 1.55 1H21a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.55 1Z" />
      </svg>
    </button>
  )
}

interface OrchestratorCfg {
  poll_interval?: string
  state_file?: string
  branch_prefix?: string
  workdir_root?: string
  http_addr?: string
  http_secret?: string
  bot_login?: string
  bot_email?: string
  ntfy_topic?: string
  allowed_logins?: string[]
}
interface ConfigShape {
  orchestrator?: OrchestratorCfg
  [k: string]: any
}

function SettingsPanel({ onClose }: { onClose: () => void }) {
  const [cfg, setCfg] = useState<OrchestratorCfg | null>(null)
  const [original, setOriginal] = useState<OrchestratorCfg | null>(null)
  const [status, setStatus] = useState<string>('')

  useEffect(() => {
    let alive = true
    fetch('/api/config', { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : Promise.reject(r.statusText))
      .then((j: ConfigShape) => {
        if (!alive) return
        const o = (j.orchestrator ?? {}) as OrchestratorCfg
        setCfg({ ...o })
        setOriginal({ ...o })
      })
      .catch((e) => setStatus('load failed: ' + String(e)))
    return () => { alive = false }
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const dirty = useMemo(() => {
    if (!cfg || !original) return false
    return JSON.stringify(cfg) !== JSON.stringify(original)
  }, [cfg, original])

  const save = async () => {
    if (!cfg) return
    setStatus('saving')
    const patch = { orchestrator: cfg }
    const r = await fetch('/api/config', {
      method: 'PUT',
      credentials: 'include',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(patch),
    })
    if (!r.ok) {
      setStatus('error: ' + (await r.text()))
      return
    }
    setOriginal({ ...cfg })
    setStatus('saved — restart orchid to apply')
    setTimeout(() => setStatus(''), 4000)
  }

  const setField = <K extends keyof OrchestratorCfg>(k: K, v: OrchestratorCfg[K]) => {
    setCfg((c) => c ? { ...c, [k]: v } : c)
  }

  return (
    <div className="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm flex items-center justify-center p-6" onClick={onClose}>
      <div
        className="relative w-full max-w-[760px] max-h-[88vh] bg-white dark:bg-zinc-900 rounded-2xl ring-1 ring-zinc-200 dark:ring-zinc-800 shadow-2xl overflow-hidden flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 h-14 flex items-center gap-3 border-b border-zinc-200 dark:border-zinc-800 flex-shrink-0">
          <span className="serif italic text-[22px] text-zinc-900 dark:text-zinc-100">Settings</span>
          <div className="flex-1" />
          {status && <span className="text-[12px] text-zinc-500 dark:text-zinc-400">{status}</span>}
          <button
            onClick={save}
            disabled={!dirty || status === 'saving'}
            className="text-[12px] px-3 py-1.5 rounded-md bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 disabled:opacity-40 disabled:cursor-not-allowed hover:opacity-90"
          >Save</button>
          <button
            onClick={onClose}
            className="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200 rounded p-1"
            title="Close (esc)"
          >
            <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
              <line x1="18" y1="6" x2="6" y2="18" />
              <line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </div>

        <div className="flex-1 min-h-0 overflow-auto px-6 py-6 space-y-8">
          <Section title="Orchestrator" subtitle="Core swarm settings — applied on next restart.">
            <Field label="Poll interval" hint="How often to scan the inbox repo (e.g. 20s).">
              <Input value={cfg?.poll_interval ?? ''} onChange={(v) => setField('poll_interval', v)} placeholder="20s" />
            </Field>
            <Field label="Branch prefix" hint="Prefix used for orchid-created branches.">
              <Input value={cfg?.branch_prefix ?? ''} onChange={(v) => setField('branch_prefix', v)} placeholder="orch/divybot-" />
            </Field>
            <Field label="HTTP address" hint="Where orch's dashboard server listens. Leave blank to disable.">
              <Input value={cfg?.http_addr ?? ''} onChange={(v) => setField('http_addr', v)} placeholder=":8000" />
            </Field>
            <Field label="HTTP secret" hint="Bearer token gating the local dashboard. Required for relay tunneling.">
              <Input value={cfg?.http_secret ?? ''} onChange={(v) => setField('http_secret', v)} placeholder="…" secret />
            </Field>
            <Field label="Bot login" hint="Git author for orchid-created commits.">
              <Input value={cfg?.bot_login ?? ''} onChange={(v) => setField('bot_login', v)} placeholder="divybot" />
            </Field>
            <Field label="Bot email" hint="Author email. Falls back to <login>@users.noreply.github.com.">
              <Input value={cfg?.bot_email ?? ''} onChange={(v) => setField('bot_email', v)} placeholder="divybot@users.noreply.github.com" />
            </Field>
            <Field label="ntfy topic" hint="Push notifications for orchid events.">
              <Input value={cfg?.ntfy_topic ?? ''} onChange={(v) => setField('ntfy_topic', v)} placeholder="orchid-divy-…" />
            </Field>
          </Section>

          <Section
            title="Allowed GitHub logins"
            subtitle="Besides you, these GitHub users can sign in via OAuth and view this dashboard."
          >
            <ChipList
              values={cfg?.allowed_logins ?? []}
              onChange={(v) => setField('allowed_logins', v)}
              placeholder="github-login"
            />
          </Section>

          <div className="text-[11.5px] text-zinc-400 dark:text-zinc-500">
            Other blocks (targets, vms, capture) are still HCL-only for now — edit
            <code className="mono mx-1">{cfg ? 'swarm.hcl' : ''}</code> on the box directly.
          </div>
        </div>
      </div>
    </div>
  )
}

function Section({ title, subtitle, children }: { title: string; subtitle?: string; children: React.ReactNode }) {
  return (
    <section>
      <div className="mb-4">
        <h3 className="serif italic text-[18px] text-zinc-900 dark:text-zinc-100">{title}</h3>
        {subtitle && <p className="text-[12px] text-zinc-500 dark:text-zinc-400 mt-0.5">{subtitle}</p>}
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  )
}
function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[180px_1fr] gap-4 items-start">
      <div>
        <div className="text-[13px] text-zinc-700 dark:text-zinc-300">{label}</div>
        {hint && <div className="text-[11px] text-zinc-400 dark:text-zinc-500 mt-0.5 leading-snug">{hint}</div>}
      </div>
      <div>{children}</div>
    </div>
  )
}
function Input({ value, onChange, placeholder, secret }: { value: string; onChange: (v: string) => void; placeholder?: string; secret?: boolean }) {
  return (
    <input
      type={secret ? 'password' : 'text'}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      spellCheck={false}
      autoComplete="off"
      className="mono w-full text-[12.5px] bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 focus:ring-zinc-400 dark:focus:ring-zinc-600 rounded-md px-3 py-2 outline-none text-zinc-900 dark:text-zinc-100"
    />
  )
}
function ChipList({ values, onChange, placeholder }: { values: string[]; onChange: (v: string[]) => void; placeholder?: string }) {
  const [draft, setDraft] = useState('')
  const add = () => {
    const v = draft.trim()
    if (!v) return
    if (values.includes(v)) { setDraft(''); return }
    onChange([...values, v])
    setDraft('')
  }
  return (
    <div className="flex flex-wrap gap-2 items-center bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-md px-3 py-2">
      {values.map((v) => (
        <span key={v} className="mono inline-flex items-center gap-1.5 text-[12px] bg-zinc-200/80 dark:bg-zinc-800 text-zinc-800 dark:text-zinc-200 rounded px-2 py-0.5">
          {v}
          <button
            onClick={() => onChange(values.filter((x) => x !== v))}
            className="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200"
            title="Remove"
          >×</button>
        </span>
      ))}
      <input
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add() }
          else if (e.key === 'Backspace' && draft === '' && values.length > 0) {
            onChange(values.slice(0, -1))
          }
        }}
        onBlur={add}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
        className="mono flex-1 min-w-[120px] bg-transparent outline-none text-[12.5px] text-zinc-900 dark:text-zinc-100"
      />
    </div>
  )
}

function ViewToggle({ view, setView }: { view: 'canvas' | 'list'; setView: (v: 'canvas' | 'list') => void }) {
  const next = view === 'canvas' ? 'list' : 'canvas'
  const title = `Switch to ${next} view`
  return (
    <button
      onClick={() => setView(next)}
      title={title}
      className="w-9 h-9 rounded-lg flex items-center justify-center transition-colors text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
    >
      {view === 'canvas' ? (
        // canvas → currently canvas; show "list" icon to indicate switch
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
          <line x1="8" y1="6" x2="21" y2="6" />
          <line x1="8" y1="12" x2="21" y2="12" />
          <line x1="8" y1="18" x2="21" y2="18" />
          <circle cx="4" cy="6" r="1" />
          <circle cx="4" cy="12" r="1" />
          <circle cx="4" cy="18" r="1" />
        </svg>
      ) : (
        // list → currently list; show "canvas" (grid) icon
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
          <rect x="3" y="3" width="7" height="7" />
          <rect x="14" y="3" width="7" height="7" />
          <rect x="3" y="14" width="7" height="7" />
          <rect x="14" y="14" width="7" height="7" />
        </svg>
      )}
    </button>
  )
}

function ListView({ jobs, onOpen }: { jobs: Job[]; onOpen: (tmux: string) => void }) {
  const activity = React.useContext(ActivityContext)
  // Group by attention level so the highest-signal cards rise to the top
  // without losing their visual category. Inside a group, sort by score
  // then alphabetically for stable rendering.
  const groups = useMemo(() => {
    const order: AttentionLevel[] = ['needs-you', 'working', 'watching', 'quiet']
    const buckets: Record<AttentionLevel, Job[]> = {
      'needs-you': [], 'working': [], 'watching': [], 'quiet': [],
    }
    for (const j of jobs) buckets[attention(j).level].push(j)
    return order
      .map((lvl) => ({
        lvl,
        jobs: buckets[lvl].sort((a, b) => {
          const sa = attention(a).score, sb = attention(b).score
          if (sa !== sb) return sb - sa
          return (a.issue_title || '').localeCompare(b.issue_title || '')
        }),
      }))
      .filter((g) => g.jobs.length > 0)
  }, [jobs])

  return (
    <div className="absolute inset-0 top-[96px] overflow-auto">
      <div className="max-w-[1100px] mx-auto px-10 pb-16 space-y-8">
        {jobs.length === 0 && (
          <div className="py-24 text-center text-zinc-400 dark:text-zinc-500">
            <div className="serif italic text-[28px] mb-2">Empty</div>
            <div className="text-[13px]">Open an issue in your inbox repo to spawn a session.</div>
          </div>
        )}
        {groups.map((g) => (
          <section key={g.lvl}>
            <div className="flex items-baseline gap-3 mb-3 px-2">
              <span className="serif italic text-[20px] text-zinc-900 dark:text-zinc-100">
                {GROUP_LABEL[g.lvl]}
              </span>
              <span className="mono text-[11px] text-zinc-400 dark:text-zinc-500">{g.jobs.length}</span>
            </div>
            <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
              {g.jobs.map((job) => (
                <ListRow
                  key={job.tmux || String(job.issue)}
                  job={job}
                  onOpen={onOpen}
                  activityAt={activity.at.get(job.tmux ?? '')}
                />
              ))}
            </div>
          </section>
        ))}
      </div>
    </div>
  )
}

const GROUP_LABEL: Record<AttentionLevel, string> = {
  'needs-you': 'Needs you',
  'working':   'Working',
  'watching':  'Awaiting review',
  'quiet':     'Quiet',
}

function ListRow({ job, onOpen, activityAt }: { job: Job; onOpen: (tmux: string) => void; activityAt?: number }) {
  let attn = attention(job)
  if (activityAt && Date.now() - activityAt < ACTIVITY_HOLD_MS) {
    attn = { ...attn, level: 'working', reason: 'active' }
  }
  const color = LEVEL_COLOR[attn.level]
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const agent = job.tmux?.toLowerCase().startsWith('codex') ? 'codex' :
    job.tmux?.toLowerCase().startsWith('claude') ? 'claude' : 'unknown'
  const issueNum = String(job.issue ?? '').replace(/^0+/, '')
  const isActive = attn.level === 'working'
  return (
    <button
      onClick={() => job.tmux && onOpen(job.tmux)}
      className="group w-full text-left py-4 px-2 hover:bg-zinc-50/80 dark:hover:bg-zinc-900/40 transition-colors flex items-center gap-5"
    >
      <span className="relative flex w-2.5 h-2.5 flex-shrink-0">
        {isActive && (
          <span className={`absolute inline-flex h-full w-full rounded-full ${color.bar} opacity-60 animate-ping`} />
        )}
        <span className={`relative w-2.5 h-2.5 rounded-full ${color.bar}`} />
      </span>
      <div className="flex-1 min-w-0">
        <div className="text-[15px] leading-snug text-zinc-900 dark:text-zinc-100 truncate font-medium">
          {job.issue_title || job.tmux || '—'}
        </div>
        <div className="mt-0.5 mono text-[11px] text-zinc-400 dark:text-zinc-500 flex items-center gap-2 truncate">
          <AgentMark agent={agent as Agent} />
          <span>{repo}</span>
          {issueNum && <span className="text-zinc-300 dark:text-zinc-600">·</span>}
          {issueNum && <span>#{issueNum}</span>}
          {job.pr ? (
            <>
              <span className="text-zinc-300 dark:text-zinc-600">·</span>
              <span>PR #{job.pr}</span>
            </>
          ) : null}
        </div>
      </div>
      <span className="text-zinc-300 dark:text-zinc-600 opacity-0 group-hover:opacity-100 transition-opacity">
        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
          <polyline points="9 18 15 12 9 6" />
        </svg>
      </span>
    </button>
  )
}

function PaneModal({ tmux, jobsByTmuxRef, onClose }: {
  tmux: string
  jobsByTmuxRef: React.MutableRefObject<Map<string, Job>>
  onClose: () => void
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])
  const job = jobsByTmuxRef.current.get(tmux)
  const ci = job ? ciStatus(job.last_check_conclusions ?? {}) : 'pending'
  const title = job?.issue_title || tmux
  return (
    <div className="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm flex items-center justify-center p-6" onClick={onClose}>
      <div
        className="relative w-full max-w-[1200px] h-[80vh] rounded-lg overflow-hidden shadow-2xl ring-1 ring-black/40 flex flex-col bg-[#0b0b0e]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="h-8 bg-zinc-800/95 flex items-center px-3 gap-3 select-none flex-shrink-0">
          <div className="flex gap-1.5 flex-shrink-0">
            <button
              onClick={onClose}
              className="w-3 h-3 rounded-full bg-rose-500 hover:bg-rose-400 transition-colors"
              title="close (esc)"
            />
            <span className="w-3 h-3 rounded-full bg-amber-400" />
            <span className="w-3 h-3 rounded-full bg-emerald-500" />
          </div>
          <div className="flex-1 min-w-0 text-center text-[12px] text-zinc-300 truncate">
            {title}
          </div>
          <div className="flex items-center gap-2 flex-shrink-0">
            {job?.pr && job?.target_repo && (
              <ModalPRBadge repo={job.target_repo} pr={job.pr} ci={ci} />
            )}
          </div>
        </div>
        <div className="flex-1 min-h-0">
          <Pane session={tmux} />
        </div>
      </div>
    </div>
  )
}

function ModalPRBadge({ repo, pr, ci }: { repo: string; pr: number; ci: 'fail' | 'pass' | 'pending' }) {
  const variant: 'open' | 'closed' | 'pending' =
    ci === 'fail' ? 'closed' : ci === 'pass' ? 'open' : 'pending'
  const color =
    variant === 'closed' ? 'text-rose-400 bg-rose-500/15 ring-rose-500/30'
    : variant === 'open' ? 'text-emerald-400 bg-emerald-500/15 ring-emerald-500/30'
    : 'text-amber-400 bg-amber-500/15 ring-amber-500/30'
  return (
    <a
      href={`https://github.com/${repo}/pull/${pr}`}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(e) => e.stopPropagation()}
      className={`mono inline-flex items-center gap-1 text-[11px] px-1.5 py-0.5 rounded ring-1 ring-inset ${color}`}
      title={`PR #${pr} · ${variant}`}
    >
      #{pr}
    </a>
  )
}

function LogoutButton() {
  return (
    <a
      href="/logout"
      title="Log out"
      className="w-9 h-9 rounded-lg flex items-center justify-center transition-colors text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
        <polyline points="16 17 21 12 16 7" />
        <line x1="21" y1="12" x2="9" y2="12" />
      </svg>
    </a>
  )
}

function FloatingComposer({ at, onDismiss }: { at: { x: number; y: number }; onDismiss: () => void }) {
  // Clamp so the 460px composer never overflows the viewport.
  const W = 460
  const H = 180
  const margin = 12
  const vw = window.innerWidth
  const vh = window.innerHeight
  const left = Math.min(Math.max(margin, at.x - W / 2), vw - W - margin)
  const top  = Math.min(Math.max(margin, at.y - 16), vh - H - margin)

  // Dismiss on outside click.
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onDown = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as globalThis.Node)) onDismiss()
    }
    // Defer so the pane-click event that opened us doesn't immediately close.
    const id = setTimeout(() => window.addEventListener('pointerdown', onDown), 0)
    return () => { clearTimeout(id); window.removeEventListener('pointerdown', onDown) }
  }, [onDismiss])

  return (
    <div
      ref={ref}
      className="fixed z-50"
      style={{ left, top, width: W }}
      onPointerDown={(e) => e.stopPropagation()}
    >
      <Composer autoFocus onSent={() => onDismiss()} onCancel={onDismiss} />
    </div>
  )
}

function ThemeToggle() {
  const [dark, setDark] = useState(() => {
    if (typeof window === 'undefined') return false
    const saved = localStorage.getItem('orchid.theme')
    if (saved) return saved === 'dark'
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false
  })
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('orchid.theme', dark ? 'dark' : 'light')
  }, [dark])
  return (
    <button
      onClick={() => setDark(d => !d)}
      title={dark ? 'switch to light' : 'switch to dark'}
      className="w-9 h-9 rounded-lg flex items-center justify-center transition-colors text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
    >
      {dark ? (
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4 12H2M22 12h-2M5 5l1.5 1.5M17.5 17.5L19 19M5 19l1.5-1.5M17.5 6.5L19 5" strokeLinecap="round" />
        </svg>
      ) : (
        <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
          <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" strokeLinejoin="round" />
        </svg>
      )}
    </button>
  )
}

// Bottom-center floating toolbar in the tldraw style: a pill of icon
// buttons with the active tool highlighted.
// Renders other users' cursors at their world-coord positions. Cursors are
// driven by the parent (which owns the WS connection). Also forwards local
// pointer moves to the parent's sendCursor.
function CollabLayer({
  cursors,
  sendCursor,
  containerRef,
}: {
  cursors: Map<string, Cursor>
  sendCursor: (x: number, y: number) => void
  containerRef: React.RefObject<HTMLDivElement>
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
    <div
      className="absolute inset-0 pointer-events-none overflow-hidden"
      style={{ zIndex: 50 }}
    >
      {Array.from(cursors.entries()).map(([id, c]) => (
        <CursorView key={id} cursor={c} viewport={viewport} />
      ))}
    </div>
  )
}

function CursorView({
  cursor,
  viewport,
}: {
  cursor: Cursor
  viewport: { x: number; y: number; zoom: number }
}) {
  const screenX = cursor.x * viewport.zoom + viewport.x
  const screenY = cursor.y * viewport.zoom + viewport.y
  return (
    <div
      className="absolute"
      style={{
        transform: `translate3d(${screenX}px, ${screenY}px, 0)`,
        transition: 'transform 90ms linear',
      }}
    >
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

function FloatingToolbar({
  tool, setTool, addNote,
}: { tool: Tool; setTool: (t: Tool) => void; addNote: () => void }) {
  return (
    <div className="fixed bottom-5 left-1/2 -translate-x-1/2 z-40 pointer-events-auto">
      <div className="flex items-center gap-1 bg-white/95 dark:bg-zinc-900/95 backdrop-blur ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-xl px-1.5 py-1.5 shadow-lg shadow-zinc-300/40 dark:shadow-black/40">
        <ToolBtn active={tool === 'select'} onClick={() => setTool('select')} title="Select (V)" hint="V">
          <IconArrow />
        </ToolBtn>
        <ToolBtn active={tool === 'box'} onClick={() => setTool('box')} title="Box select (R)" hint="R">
          <IconBox />
        </ToolBtn>
        <ToolBtn active={tool === 'pen'} onClick={() => setTool('pen')} title="Draw (P)" hint="P">
          <IconPen />
        </ToolBtn>
        <ToolBtn active={tool === 'eraser'} onClick={() => setTool('eraser')} title="Eraser (E)" hint="E">
          <IconEraser />
        </ToolBtn>
        <div className="w-px h-5 bg-zinc-200 dark:bg-zinc-700 mx-1" />
        <ToolBtn active={tool === 'text'} onClick={() => setTool('text')} title="Text (T)" hint="T">
          <IconText />
        </ToolBtn>
        <ToolBtn active={false} onClick={addNote} title="Note (N)" hint="N">
          <IconNote />
        </ToolBtn>
      </div>
    </div>
  )
}

function ToolBtn({
  active, onClick, title, children, hint,
}: {
  active: boolean; onClick: () => void; title: string; hint?: string
  children: React.ReactNode
}) {
  return (
    <button
      onClick={onClick}
      title={title}
      className={
        'w-9 h-9 rounded-lg flex items-center justify-center transition-colors relative group ' +
        (active
          ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900'
          : 'text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800')
      }
    >
      {children}
      {hint && (
        <span
          className={
            'absolute -bottom-1 right-1 mono text-[8px] ' +
            (active ? 'text-zinc-400' : 'text-zinc-400')
          }
        >
          {hint}
        </span>
      )}
    </button>
  )
}

function IconArrow() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
      <path d="M5 3l5 18 3-8 8-3z" strokeLinejoin="round" />
    </svg>
  )
}
function IconPen() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
      <path d="M16 3l5 5L8 21H3v-5z" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}
function IconEraser() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
      <path d="M3 16l7 7h7l4-4-11-11-7 8z" strokeLinejoin="round" />
      <path d="M14 6l4 4" strokeLinejoin="round" />
    </svg>
  )
}
function IconBox() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeDasharray="3 2">
      <rect x="3" y="3" width="18" height="18" rx="2" />
    </svg>
  )
}
function IconText() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
      <path d="M5 5h14M12 5v14" strokeLinecap="round" />
    </svg>
  )
}
function IconNote() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}>
      <path d="M4 4h12l4 4v12H4z" strokeLinejoin="round" />
      <path d="M16 4v4h4" strokeLinejoin="round" />
    </svg>
  )
}

function CardCompact({ job }: { job: Job; fact: string; dot: string; factColor: string }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const agent = detectAgent(job)
  return (
    <div className="p-3 h-full flex flex-col gap-1.5">
      <div className="flex items-center gap-1.5 min-w-0">
        <AgentMark agent={agent} />
        <span className="mono text-[10.5px] text-zinc-400 dark:text-zinc-500 truncate">{repo}</span>
        <div className="flex-1" />
        {job.lifecycle === 'cron' && (
          <span className="mono text-[10px] text-violet-500">cron</span>
        )}
      </div>
      <div className="text-[13px] text-zinc-900 dark:text-zinc-100 leading-snug line-clamp-4 flex-1">
        {job.issue_title || '—'}
      </div>
    </div>
  )
}

type Agent = 'claude' | 'codex' | 'unknown'

function detectAgent(job: Job): Agent {
  const t = (job.tmux || '').toLowerCase()
  if (t.startsWith('codex')) return 'codex'
  if (t.startsWith('claude')) return 'claude'
  return 'unknown'
}

function AgentMark({ agent }: { agent: Agent }) {
  if (agent === 'claude') {
    // Official Claude mark from Bootstrap Icons (icons.getbootstrap.com/icons/claude).
    return (
      <svg
        width={13} height={13} viewBox="0 0 16 16"
        fill="currentColor"
        className="text-[#cc7c5a] flex-shrink-0"
        aria-label="Claude"
      >
        <path d="m3.127 10.604 3.135-1.76.053-.153-.053-.085H6.11l-.525-.032-1.791-.048-1.554-.065-1.505-.08-.38-.081L0 7.832l.036-.234.32-.214.455.04 1.009.069 1.513.105 1.097.064 1.626.17h.259l.036-.105-.089-.065-.068-.064-1.566-1.062-1.695-1.121-.887-.646-.48-.327-.243-.306-.104-.67.435-.48.585.04.15.04.593.456 1.267.981 1.654 1.218.242.202.097-.068.012-.049-.109-.181-.9-1.626-.96-1.655-.428-.686-.113-.411a2 2 0 0 1-.068-.484l.496-.674L4.446 0l.662.089.279.242.411.94.666 1.48 1.033 2.014.302.597.162.553.06.17h.105v-.097l.085-1.134.157-1.392.154-1.792.052-.504.25-.605.497-.327.387.186.319.456-.045.294-.19 1.23-.37 1.93-.243 1.29h.142l.161-.16.654-.868 1.097-1.372.484-.545.565-.601.363-.287h.686l.505.751-.226.775-.707.895-.585.759-.839 1.13-.524.904.048.072.125-.012 1.897-.403 1.024-.186 1.223-.21.553.258.06.263-.218.536-1.307.323-1.533.307-2.284.54-.028.02.032.04 1.029.098.44.024h1.077l2.005.15.525.346.315.424-.053.323-.807.411-3.631-.863-.872-.218h-.12v.073l.726.71 1.331 1.202 1.667 1.55.084.383-.214.302-.226-.032-1.464-1.101-.565-.497-1.28-1.077h-.084v.113l.295.432 1.557 2.34.08.718-.112.234-.404.141-.444-.08-.911-1.28-.94-1.44-.759-1.291-.093.053-.448 4.821-.21.246-.484.186-.403-.307-.214-.496.214-.98.258-1.28.21-1.016.19-1.263.112-.42-.008-.028-.092.012-.953 1.307-1.448 1.957-1.146 1.227-.274.109-.477-.247.045-.44.266-.39 1.586-2.018.956-1.25.617-.723-.004-.105h-.036l-4.212 2.736-.75.096-.324-.302.04-.496.154-.162 1.267-.871z"/>
      </svg>
    )
  }
  if (agent === 'codex') {
    // Official OpenAI mark from Bootstrap Icons (icons.getbootstrap.com/icons/openai).
    return (
      <svg
        width={13} height={13} viewBox="0 0 16 16"
        fill="currentColor"
        className="text-zinc-900 dark:text-white flex-shrink-0"
        aria-label="Codex"
      >
        <path d="M14.949 6.547a3.94 3.94 0 0 0-.348-3.273 4.11 4.11 0 0 0-4.4-1.934 4.1 4.1 0 0 0-1.126-.613 4.15 4.15 0 0 0-2.118-.086 4.1 4.1 0 0 0-1.891.948 4.04 4.04 0 0 0-1.158 1.753 4.1 4.1 0 0 0-1.563.679 4 4 0 0 0-1.14 1.254 3.99 3.99 0 0 0 .502 4.731 3.94 3.94 0 0 0 .346 3.274 4.11 4.11 0 0 0 4.402 1.933c.382.425.852.764 1.377.995.526.231 1.095.35 1.67.346 1.78.002 3.358-1.132 3.901-2.804a4.1 4.1 0 0 0 1.563-.68 4 4 0 0 0 1.14-1.253 3.99 3.99 0 0 0-.506-4.716m-6.097 8.406a3.05 3.05 0 0 1-1.945-.694l.096-.054 3.23-1.838a.53.53 0 0 0 .265-.455v-4.49l1.366.778q.02.011.025.035v3.722c-.003 1.653-1.361 2.992-3.037 2.996m-6.53-2.75a2.95 2.95 0 0 1-.36-2.01l.095.057L5.29 12.09a.53.53 0 0 0 .527 0l3.949-2.246v1.555a.05.05 0 0 1-.022.041L6.473 13.3c-1.454.826-3.311.335-4.15-1.098m-.85-6.94A3.02 3.02 0 0 1 3.07 3.949v3.785a.51.51 0 0 0 .262.451l3.93 2.237-1.366.779a.05.05 0 0 1-.048 0L2.585 9.342a2.98 2.98 0 0 1-1.113-4.094zm11.216 2.571L8.747 5.576l1.362-.776a.05.05 0 0 1 .048 0l3.265 1.86a3 3 0 0 1 1.173 1.207 2.96 2.96 0 0 1-.27 3.2 3.05 3.05 0 0 1-1.36.997V8.279a.52.52 0 0 0-.276-.445m1.36-2.015-.097-.057-3.226-1.855a.53.53 0 0 0-.53 0L6.249 6.153V4.598a.04.04 0 0 1 .019-.04L9.533 2.7a3.07 3.07 0 0 1 3.257.139c.474.325.843.778 1.066 1.303.223.526.289 1.103.191 1.664zM5.503 8.575 4.139 7.8a.05.05 0 0 1-.026-.037V4.049c0-.57.166-1.127.476-1.607s.752-.864 1.275-1.105a3.08 3.08 0 0 1 3.234.41l-.096.054-3.23 1.838a.53.53 0 0 0-.265.455zm.742-1.577 1.758-1 1.762 1v2l-1.755 1-1.762-1z"/>
      </svg>
    )
  }
  return (
    <span className="w-3 h-3 rounded-full bg-zinc-300 dark:bg-zinc-600 flex-shrink-0" />
  )
}

function CardExpanded({ job, fact, attnText, onClose }: { job: Job; fact: string; attnText: string; onClose: () => void }) {
  const repo = job.target_repo
  const issueURL = job.issue ? `https://github.com/denoland/orchid/issues/${job.issue}` : null
  const prURL = job.pr ? `https://github.com/${repo}/pull/${job.pr}` : null
  return (
    <div className="h-full flex flex-col p-3 nodrag">
      <div className="flex items-start gap-3 mb-2">
        <div className="flex-1 min-w-0">
          <div className="text-[14px] text-zinc-900 dark:text-zinc-100 font-medium truncate">
            {job.issue_title || job.tmux || '—'}
          </div>
          <div className="mono text-[10.5px] text-zinc-400 dark:text-zinc-500 truncate">
            {repo} · {job.tmux} {job.branch && `· ${job.branch}`}
          </div>
        </div>
        <span className={`mono text-[11px] ${attnText}`}>{fact}</span>
        <button
          onClick={(e) => { e.stopPropagation(); onClose() }}
          className="text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100 rounded p-0.5"
          title="close (esc)"
        >
          <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2}>
            <path d="M18 6L6 18M6 6l12 12" />
          </svg>
        </button>
      </div>
      <div className="flex items-center gap-x-3 gap-y-1 flex-wrap mb-2">
        {issueURL && <Link href={issueURL}>issue #{job.issue}</Link>}
        {prURL && <Link href={prURL}>pr #{job.pr}</Link>}
        {repo && <Link href={`https://github.com/${repo}`}>{repo}</Link>}
      </div>
      <div className="flex-1 min-h-0 rounded-lg overflow-hidden ring-1 ring-zinc-200/80 dark:ring-zinc-700/80">
        {job.tmux && <Pane session={job.tmux} />}
      </div>
    </div>
  )
}

function Link({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(e) => e.stopPropagation()}
      className="mono text-[11px] text-zinc-500 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
    >
      {children}
    </a>
  )
}

function Dot({ dotBg, pulse }: { dotBg: string; pulse: boolean }) {
  return (
    <span className="relative inline-flex w-2 h-2 flex-shrink-0">
      {pulse && <span className={`absolute inline-flex h-full w-full rounded-full ${dotBg} opacity-50 animate-ping`} />}
      <span className={`relative w-2 h-2 rounded-full ${dotBg}`} />
    </span>
  )
}

function factFor(
  job: Job,
  attn: ReturnType<typeof attention>,
  ci: 'fail' | 'pass' | 'pending'
): string {
  if (attn.level === 'needs-you' && ci === 'fail') return 'CI fail'
  if (attn.level === 'needs-you') return 'idle'
  if (job.pr && ci === 'pass') return 'review'
  if (job.pr && ci === 'pending') return 'checking'
  if (job.pr) return 'PR'
  if (attn.level === 'working') return 'active'
  return ''
}
