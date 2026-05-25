
import React, { useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
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
import {
  NoteNode, LinkNode, TextNode, StrokeNode,
  PenLayer, CollabLayer, useCollabSocket,
  detectVariant, fetchGitHubSnippet, fetchOG,
  type Stroke, type UserNode,
  type NoteData, type TextData, type LinkData, type StrokeData, type LinkVariant,
  NOTE_W, NOTE_H, LINK_W, LINK_H,
} from '@orchid/whiteboard'

import type { RelayInfo } from './App'
import { WSBusContext } from './App'

interface Props { state: State; relay: RelayInfo | null }

const CARD_W = 220
const CARD_H = 96
const COLS = 4
const GAP = 18
const HEADER_OFFSET = 220

type Tool = 'select' | 'box' | 'pen' | 'eraser' | 'note' | 'text'

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
/// Fetches the layout snap from orch. Returns `null` on any failure
/// (network, auth, parse). The caller must NOT treat null as "empty
/// snap" — that would defeat the persisted layout the first time a
/// request blips. Use `null` to keep the rebuild effect parked.
async function fetchSnap(): Promise<Snap | null> {
  try {
    const r = await fetch('/api/snap', { credentials: 'include', cache: 'no-store' })
    if (!r.ok) return null
    return normalizeSnap(await r.json())
  } catch { return null }
}
// Bus-aware snap save path. When the events WS is up, layout writes
// piggyback that connection (saveBusSender is wired by App.tsx on
// mount) so card drags don't hit the /api/snap HTTP route — every
// avoided round-trip is one fewer DO request on Cloudflare. The HTTP
// PUT remains as a fallback for local-mode operators with no relay
// agent connected, and for pagehide when the bus isn't initialised.
let saveBusSender: ((msg: any) => void) | null = null
export function setSnapBusSender(fn: ((msg: any) => void) | null) { saveBusSender = fn }
let saveTimer: ReturnType<typeof setTimeout> | null = null
let savePending: Snap | null = null
function doPut(body: Snap) {
  if (saveBusSender) {
    try { saveBusSender({ t: 'snap-put', snap: body }) } catch {}
    return Promise.resolve()
  }
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
  if (saveBusSender) {
    try { saveBusSender({ t: 'snap-put', snap: body }) } catch {}
    return
  }
  // sendBeacon survives navigation when the bus isn't available.
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
  // what the slower polled activity array says. A pending prompt outranks
  // it though — the activity ping fires once on the busy→prompted screen
  // redraw, but the dialog is sitting there waiting on a human and the
  // card should stay red until the prompt clears.
  const lastPing = activity.at.get(job.tmux)
  if (lastPing && Date.now() - lastPing < ACTIVITY_HOLD_MS && !job.needs_input) {
    attn = { ...attn, level: 'working', reason: 'active' }
  }
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
      <CardCompact job={job} />
    </div>
  )
}

// Whiteboard primitives (note / text / link / stroke nodes, pen overlay,
// collab WS) live in @orchid/whiteboard. Orchid-only nodes (CardNode
// above, PaneWindowNode below) stay here.

// ─── pane window node ───

type PaneNodeData = {
  tmux: string
  jobRef: { current: Map<string, Job> }
  onClose: (tmux: string) => void
}

function PaneWindowNode({ data, selected }: NodeProps<Node<PaneNodeData, 'pane'>>) {
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

export function Dashboard({ state, relay }: Props) {
  return (
    <ReactFlowProvider>
      <DashboardInner state={state} relay={relay} />
    </ReactFlowProvider>
  )
}

function DashboardInner({ state, relay }: Props) {
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
  // Shared events-WS bus from App.tsx — see WSBusContext. Used for
  // inbound snap pushes and to skip the /api/canvas/ws side-channel.
  const bus = useContext(WSBusContext)

  const persist = useCallback(() => { saveSnap(snapRef.current) }, [])

  // Snap (canvas layout) arrives via two channels, whichever wins:
  //  1. Relay's events WS — primed on accept with the cached layout,
  //     no HTTP round-trip. Skipped when there's no bus (local mode).
  //  2. Fallback fetch of /api/snap. Retries on transient failure so a
  //     single 503 right after OAuth doesn't let the rebuild effect
  //     default-grid the layout and PUT that clobbering snap back.
  const snapLoadedRef = useRef(false)
  useEffect(() => { snapLoadedRef.current = snapLoaded }, [snapLoaded])
  useEffect(() => {
    if (!bus) return
    return bus.subscribe((msg: any) => {
      if (msg?.t !== 'snap') return
      try {
        const s = normalizeSnap(msg.snap ?? {})
        snapRef.current = s
        setEdges(s.edges)
        setStrokes(s.strokes)
        if (s.view) setView(s.view)
        setSnapLoaded(true)
      } catch {}
    })
  }, [bus])
  useEffect(() => {
    let alive = true
    let attempt = 0
    const load = async () => {
      while (alive) {
        if (snapLoadedRef.current) return
        const s = await fetchSnap()
        if (!alive) return
        if (s) {
          snapRef.current = s
          setEdges(s.edges)
          setStrokes(s.strokes)
          if (s.view) setView(s.view)
          setSnapLoaded(true)
          return
        }
        attempt++
        await new Promise((r) => setTimeout(r, Math.min(8000, 500 * 2 ** attempt)))
      }
    }
    load()
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
  const [showSettings, setShowSettings] = useState(false)
  const [showCapture, setShowCapture] = useState(false)
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

  const { cursors, sendCursor, send } = useCollabSocket({ onMessage: handleRemote, transport: bus })
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
      {!showSettings && !showCapture && (
        <Header
          inbox={inbox}
          count={jobs.length}
          quota={state.quota}
          showComposer={!headerComposerDismissed && jobs.length === 0}
          view={view}
          setView={(v) => {
            setView(v)
            snapRef.current.view = v
            persist()
          }}
          onOpenSettings={() => setShowSettings(true)}
          onOpenCapture={() => setShowCapture(true)}
        />
      )}
      {showSettings && <SettingsPage jobs={jobs} state={state} relay={relay} onClose={() => setShowSettings(false)} />}
      {showCapture && <CapturePage jobs={jobs} inbox={inbox} onClose={() => setShowCapture(false)} />}
      {view === 'canvas' && !showSettings && !showCapture && (
        <FloatingToolbar tool={tool} setTool={setTool} addNote={addNote} />
      )}
      {view === 'canvas' && !showSettings && !showCapture && (
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
          newId={newId}
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
  count, quota, showComposer, view, setView, onOpenSettings, onOpenCapture,
}: {
  inbox: string; count: number; showComposer: boolean
  quota?: State['quota']
  view: 'canvas' | 'list'; setView: (v: 'canvas' | 'list') => void
  onOpenSettings: () => void
  onOpenCapture: () => void
}) {
  // Stop pointer events on the entire header row so clicks on the title /
  // toggle / link don't bubble through to the ReactFlow pane behind it.
  return (
    <div className="fixed top-0 inset-x-0 z-40 pointer-events-none">
      <div className="px-4 sm:px-8 pt-4 sm:pt-6 flex flex-col gap-3">
        <div
          className="pointer-events-auto flex flex-wrap items-baseline gap-x-3 gap-y-2"
          onPointerDown={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >
          <h1 className="serif text-[32px] sm:text-[44px] font-medium leading-none text-zinc-900 dark:text-zinc-100 italic">
            Orchid
          </h1>
          <span className="mono text-[12px] text-zinc-400 dark:text-zinc-500">{count}</span>
          {quota && (
            <div className="hidden sm:block">
              <QuotaStrip quota={quota} />
            </div>
          )}
          <div className="flex-1" />
          <HeaderBtnBar>
            <CaptureButton onClick={onOpenCapture} />
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

/// Compact two-bar quota readout sourced from Claude Code's
/// statusline.jsonl feed. Bar widths track used_percentage; the
/// trailing label is the time to reset (4h12m / 2d3h). When usage
/// outpaces elapsed-time we tint amber to flag burn faster than
/// sustainable. Hidden entirely until the agent has reported once.
function QuotaStrip({ quota }: { quota: NonNullable<State['quota']> }) {
  const now = Math.floor(Date.now() / 1000)
  const fmt = (secs: number) => {
    if (secs <= 0) return 'now'
    const h = Math.floor(secs / 3600)
    const m = Math.floor((secs % 3600) / 60)
    const d = Math.floor(h / 24)
    if (d > 0) return `${d}d${h % 24}h`
    if (h > 0) return `${h}h${m}m`
    return `${m}m`
  }
  const bar = (label: string, pct: number, resets: number, window: number) => {
    const elapsedPct = Math.min(100, Math.max(0, (1 - Math.max(0, resets - now) / window) * 100))
    const hot = pct > elapsedPct + 5
    const trackColor = hot ? 'bg-amber-200/60 dark:bg-amber-900/40' : 'bg-zinc-200 dark:bg-zinc-800'
    const fillColor = hot ? 'bg-amber-500' : 'bg-emerald-500/80 dark:bg-emerald-400/80'
    return (
      <div className="flex items-center gap-1.5">
        <span className="mono text-[10px] text-zinc-400 dark:text-zinc-500 w-[18px]">{label}</span>
        <div className={'relative h-1.5 w-20 rounded-full overflow-hidden ' + trackColor}>
          <div className={'absolute inset-y-0 left-0 ' + fillColor} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
        </div>
        <span className="mono text-[10px] text-zinc-500 dark:text-zinc-400 tabular-nums">{Math.round(pct)}%</span>
        <span className="mono text-[10px] text-zinc-400 dark:text-zinc-500">{fmt(resets - now)}</span>
      </div>
    )
  }
  return (
    <div
      className="pointer-events-auto ml-3 flex items-center gap-3 bg-white/80 dark:bg-zinc-900/80 backdrop-blur ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-md px-2.5 py-1"
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      title="Claude subscription usage: 5-hour session window and 7-day cap. Amber = burning faster than elapsed time would sustain."
    >
      {bar('5h', quota.five_hour_pct, quota.five_hour_resets_at, 5 * 3600)}
      {bar('7d', quota.seven_day_pct, quota.seven_day_resets_at, 7 * 24 * 3600)}
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

function CaptureButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title="Capture (file a new issue)"
      className="w-9 h-9 rounded-lg flex items-center justify-center transition-colors text-zinc-700 dark:text-zinc-300 hover:bg-zinc-100 dark:hover:bg-zinc-800"
    >
      <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
        <line x1="12" y1="5" x2="12" y2="19" />
        <line x1="5" y1="12" x2="19" y2="12" />
      </svg>
    </button>
  )
}

function CapturePage({ jobs, inbox, onClose }: { jobs: Job[]; inbox: string; onClose: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  // Most-recent inbox-driven jobs. Captures land here once orch picks
  // them up and labels them; until then the user sees their own
  // composed item bubble to the top on the next /api/state poll.
  const recent = useMemo(
    () => [...jobs].sort((a, b) => b.issue - a.issue).slice(0, 12),
    [jobs],
  )

  return (
    <div className="absolute inset-0 z-30 bg-white dark:bg-zinc-950 flex flex-col">
      <div className="px-8 h-14 flex items-center gap-3 border-b border-zinc-200 dark:border-zinc-800 flex-shrink-0">
        <button
          onClick={onClose}
          className="text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 flex items-center gap-1 text-[13px]"
          title="Back (esc)"
        >
          <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
            <polyline points="15 18 9 12 15 6" />
          </svg>
        </button>
        <span className="serif italic text-[24px] text-zinc-900 dark:text-zinc-100 ml-2">Capture</span>
        <span className="mono text-[12px] text-zinc-400 dark:text-zinc-500">spawn an idea</span>
      </div>

      <div className="flex-1 min-h-0 overflow-auto">
        <div className="max-w-[720px] mx-auto px-4 sm:px-8 py-8 sm:py-12 space-y-10">
          <Composer autoFocus />

          <div>
            <div className="serif italic text-[18px] text-zinc-900 dark:text-zinc-100 mb-3 px-1">Recent</div>
            {recent.length === 0 && (
              <p className="text-[13px] text-zinc-500 dark:text-zinc-400 px-1">
                Nothing here yet. Type above to file your first capture.
              </p>
            )}
            <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
              {recent.map((j) => (
                <RecentCaptureRow key={j.issue} job={j} inbox={inbox} />
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function RecentCaptureRow({ job, inbox }: { job: Job; inbox: string }) {
  const attn = attention(job)
  const color = LEVEL_COLOR[attn.level]
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const issueURL = inbox ? `https://github.com/${inbox}/issues/${job.issue}` : `#${job.issue}`
  return (
    <a
      href={issueURL}
      target="_blank"
      rel="noopener noreferrer"
      className="group flex items-center gap-4 px-1 py-3 hover:bg-zinc-50/80 dark:hover:bg-zinc-900/40 transition-colors"
    >
      <span className={`w-2 h-2 rounded-full ${color.bar} flex-shrink-0`} />
      <div className="flex-1 min-w-0">
        <div className="text-[14px] text-zinc-900 dark:text-zinc-100 truncate">
          {job.issue_title || job.tmux || `#${job.issue}`}
        </div>
        <div className="mt-0.5 mono text-[11px] text-zinc-400 dark:text-zinc-500 truncate">
          #{job.issue} · {repo}{job.pr ? ` · PR #${job.pr}` : ''}
        </div>
      </div>
      <span className="text-zinc-300 dark:text-zinc-600 opacity-0 group-hover:opacity-100 transition-opacity">
        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
          <line x1="7" y1="17" x2="17" y2="7" />
          <polyline points="7 7 17 7 17 17" />
        </svg>
      </span>
    </a>
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

interface CaptureCfg {
  auth_token?: string
  assets_dir?: string
  public_url?: string
}
interface OrchestratorCfg {
  poll_interval?: string
  state_db?: string
  branch_prefix?: string
  workdir_root?: string
  http_addr?: string
  http_secret?: string
  bot_login?: string
  bot_email?: string
  ntfy_topic?: string
  allowed_logins?: string[]
  capture?: CaptureCfg
}
type GhCfg = { inbox_repo?: string }
interface VMCfg {
  name: string
  host?: string
  user?: string
  key?: string
  capacity?: number
  bot_login?: string
  agent?: string
}
interface TargetCfg {
  name: string
  repo?: string
}
interface ConfigShape {
  orchestrator?: OrchestratorCfg
  github?: GhCfg
  vms?: VMCfg[]
  targets?: TargetCfg[]
  [k: string]: any
}

interface RepoOption {
  full_name: string
  private: boolean
  description?: string | null
  pushed_at?: string | null
  avatar?: string
}

function cryptoToken(): string {
  // 16 random bytes as hex — same shape as the install.sh-generated
  // capture token, suitable for X-Capture-Token.
  const buf = new Uint8Array(16)
  crypto.getRandomValues(buf)
  return Array.from(buf, (b) => b.toString(16).padStart(2, '0')).join('')
}

function useRepos(enabled: boolean) {
  const [repos, setRepos] = useState<RepoOption[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!enabled || repos !== null) return
    let alive = true
    fetch('/api/_relay/repos', { credentials: 'include' })
      .then(async (r) => {
        if (r.status === 412) {
          // No GH access_token on file — user signed in before we
          // started capturing it. Surface a clear reconnect prompt.
          throw new Error('reauth')
        }
        if (!r.ok) throw new Error(r.statusText || String(r.status))
        return r.json()
      })
      .then((j: { repos?: RepoOption[]; error?: string }) => {
        if (!alive) return
        if (j.error) setError(j.error)
        setRepos(j.repos ?? [])
      })
      .catch((e) => { if (alive) setError(String(e.message ?? e)) })
    return () => { alive = false }
  }, [enabled, repos])
  return { repos, error }
}

function RepoPicker({ value, onChange, placeholder, repos, error }: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  repos: RepoOption[] | null
  error?: string | null
}) {
  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState('')
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as globalThis.Node)) setOpen(false)
    }
    window.addEventListener('pointerdown', onDown)
    return () => window.removeEventListener('pointerdown', onDown)
  }, [])
  const filtered = useMemo(() => {
    if (!repos) return []
    const f = filter.toLowerCase()
    return f ? repos.filter((r) => r.full_name.toLowerCase().includes(f)).slice(0, 50) : repos.slice(0, 50)
  }, [repos, filter])
  const selected = repos?.find((r) => r.full_name === value)
  const [owner, name] = (value || '').split('/')
  const avatar = selected?.avatar ?? (owner ? `https://github.com/${owner}.png?size=80` : undefined)
  return (
    <div ref={ref} className="relative">
      <div
        onClick={() => setOpen((o) => !o)}
        className="flex items-center gap-3 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-lg px-3 py-2.5 cursor-pointer hover:ring-zinc-400 dark:hover:ring-zinc-600 transition-colors"
      >
        {value ? (
          <>
            {avatar && <img src={avatar} alt="" className="w-7 h-7 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />}
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-1.5 text-[13px] text-zinc-900 dark:text-zinc-100 truncate">
                <span className="text-zinc-500 dark:text-zinc-400">{owner}</span>
                <span className="text-zinc-300 dark:text-zinc-600">/</span>
                <span className="font-medium mono">{name}</span>
                {selected?.private && <span className="text-[10.5px] text-zinc-400 dark:text-zinc-500">private</span>}
              </div>
              {selected?.description && (
                <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 truncate">
                  {selected.description}
                </div>
              )}
            </div>
          </>
        ) : (
          <span className="flex-1 text-[12.5px] text-zinc-400 dark:text-zinc-500">
            {placeholder || 'pick a repo or type owner/repo'}
          </span>
        )}
        <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} className={`text-zinc-400 transition-transform ${open ? 'rotate-180' : ''}`}>
          <polyline points="6 9 12 15 18 9" />
        </svg>
      </div>
      {open && (
        <div className="absolute z-20 mt-1.5 w-full bg-white dark:bg-zinc-900 ring-1 ring-zinc-200 dark:ring-zinc-700 rounded-lg shadow-xl shadow-zinc-300/40 dark:shadow-black/40 overflow-hidden">
          <div className="flex items-center gap-2 px-3 py-2.5 border-b border-zinc-200 dark:border-zinc-800">
            <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} className="text-zinc-400">
              <circle cx="11" cy="11" r="7" />
              <line x1="20" y1="20" x2="16.5" y2="16.5" />
            </svg>
            <input
              autoFocus
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="search your repos…"
              className="w-full text-[13px] outline-none bg-transparent text-zinc-900 dark:text-zinc-100"
            />
          </div>
          <div className="max-h-[320px] overflow-auto">
            {error === 'reauth' && (
              <div className="px-3 py-3 flex items-start gap-3 bg-amber-50/60 dark:bg-amber-900/20 border-b border-amber-200 dark:border-amber-900/50">
                <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} className="text-amber-600 mt-0.5 flex-shrink-0">
                  <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
                </svg>
                <div className="flex-1">
                  <div className="text-[12.5px] text-amber-900 dark:text-amber-200">Reconnect GitHub to load your repos</div>
                  <div className="text-[11px] text-amber-700 dark:text-amber-300/80 mt-0.5">
                    Sign in again so orchid can read your repo list — old sessions don't carry the token.
                  </div>
                  <a
                    href="/login"
                    className="mono inline-block mt-1.5 text-[11px] px-2 py-0.5 rounded bg-amber-900 text-amber-50 dark:bg-amber-100 dark:text-amber-900 hover:opacity-90"
                  >Reconnect</a>
                </div>
              </div>
            )}
            {!repos && !error && (
              <div className="px-3 py-4 text-[12.5px] text-zinc-400 dark:text-zinc-500 italic">loading repos…</div>
            )}
            {repos && filtered.length === 0 && (
              <div className="px-3 py-4 text-[12.5px] text-zinc-400 dark:text-zinc-500">
                no matches — paste owner/repo below
              </div>
            )}
            {filtered.map((r) => (
              <button
                key={r.full_name}
                onClick={() => { onChange(r.full_name); setOpen(false); setFilter('') }}
                className="w-full text-left px-3 py-2 hover:bg-zinc-50 dark:hover:bg-zinc-800/60 flex items-center gap-3 transition-colors group"
              >
                <img src={r.avatar} alt="" className="w-7 h-7 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-1.5 text-[13px]">
                    <span className="text-zinc-500 dark:text-zinc-400 truncate">{r.full_name.split('/')[0]}</span>
                    <span className="text-zinc-300 dark:text-zinc-600">/</span>
                    <span className="font-medium text-zinc-900 dark:text-zinc-100 mono truncate">{r.full_name.split('/')[1]}</span>
                    {r.private && <span className="text-[10.5px] text-zinc-400 dark:text-zinc-500">private</span>}
                  </div>
                  {r.description && (
                    <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 truncate">{r.description}</div>
                  )}
                </div>
                <span className="mono text-[10px] text-zinc-400 dark:text-zinc-500 opacity-0 group-hover:opacity-100 transition-opacity flex-shrink-0">
                  {timeAgo(r.pushed_at)}
                </span>
              </button>
            ))}
          </div>
          <div className="border-t border-zinc-200 dark:border-zinc-800 px-3 py-2 bg-zinc-50 dark:bg-zinc-950">
            <input
              value={value}
              onChange={(e) => onChange(e.target.value)}
              placeholder="…or paste owner/repo"
              className="mono w-full text-[11.5px] outline-none bg-transparent text-zinc-700 dark:text-zinc-300 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
            />
          </div>
        </div>
      )}
    </div>
  )
}

function timeAgo(iso?: string | null): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'now'
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d`
  const mo = Math.floor(d / 30)
  if (mo < 12) return `${mo}mo`
  return `${Math.floor(mo / 12)}y`
}

type SectionId = 'orch' | 'access' | 'capture' | 'vms' | 'targets' | 'usage' | 'danger'

function SettingsPage({ jobs, state, relay, onClose }: {
  jobs: Job[]
  state: State
  relay: RelayInfo | null
  onClose: () => void
}) {
  const [cfg, setCfg] = useState<OrchestratorCfg | null>(null)
  const [gh, setGh] = useState<GhCfg | null>(null)
  const [vms, setVms] = useState<VMCfg[]>([])
  const [targets, setTargets] = useState<TargetCfg[]>([])
  const [original, setOriginal] = useState<{
    cfg: OrchestratorCfg; gh: GhCfg; targets: TargetCfg[]
  } | null>(null)
  const [status, setStatus] = useState<string>('')
  const [section, setSection] = useState<SectionId>('orch')
  const { repos, error: reposError } = useRepos(true)

  useEffect(() => {
    let alive = true
    fetch('/api/config', { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : Promise.reject(r.statusText))
      .then((j: ConfigShape) => {
        if (!alive) return
        const o = (j.orchestrator ?? {}) as OrchestratorCfg
        const g = (j.github ?? {}) as GhCfg
        const v = (j.vms ?? []) as VMCfg[]
        const t = (j.targets ?? []) as TargetCfg[]
        setCfg({ ...o }); setGh({ ...g }); setVms([...v]); setTargets([...t])
        setOriginal({ cfg: { ...o }, gh: { ...g }, targets: [...t] })
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
    if (!cfg || !gh || !original) return false
    return JSON.stringify(cfg) !== JSON.stringify(original.cfg) ||
      JSON.stringify(gh) !== JSON.stringify(original.gh) ||
      JSON.stringify(targets) !== JSON.stringify(original.targets)
  }, [cfg, gh, targets, original])

  const save = async () => {
    if (!cfg || !gh || !original) return
    setStatus('saving')
    const patch: Record<string, any> = {}

    // Singletons. Strip the nested `capture` so it doesn't accidentally
    // try to serialise as an attribute — capture lives under its own
    // `orchestrator.capture` patch key.
    const orchTop: any = { ...cfg }
    const capture = orchTop.capture
    delete orchTop.capture
    patch.orchestrator = orchTop
    patch.github = gh
    if (capture) patch['orchestrator.capture'] = capture

    // Targets — keyed-block patches. Diff against original. VMs aren't
    // patched from the dashboard: the VMs section is read-only and
    // surfaces a join command instead of an editable form.
    const byNameOrig = (arr: { name: string }[]) => Object.fromEntries(arr.map((x) => [x.name, x]))
    const tgOrig = byNameOrig(original.targets), tgCur = byNameOrig(targets)
    for (const name of new Set([...Object.keys(tgOrig), ...Object.keys(tgCur)])) {
      if (!tgCur[name]) { patch[`target.${name}`] = { __delete: true }; continue }
      const { name: _n, ...body } = tgCur[name] as any
      patch[`target.${name}`] = body
    }

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
    setOriginal({ cfg: { ...cfg }, gh: { ...gh }, targets: [...targets] })
    // Access (allowed_logins) hot-applies via the live relay agent —
    // no restart needed. Everything else (poll interval, http_addr,
    // bot identity, target/VM blocks) is still read once at orch
    // startup, so a bounce is required for those.
    setStatus('saved · Access applies now, other fields on next orch restart')
    setTimeout(() => setStatus(''), 6000)
  }

  const setField = <K extends keyof OrchestratorCfg>(k: K, v: OrchestratorCfg[K]) => {
    setCfg((c) => c ? { ...c, [k]: v } : c)
  }
  const setGhField = <K extends keyof GhCfg>(k: K, v: GhCfg[K]) => {
    setGh((g) => g ? { ...g, [k]: v } : g)
  }
  const setCaptureField = <K extends keyof CaptureCfg>(k: K, v: CaptureCfg[K]) => {
    setCfg((c) => c ? { ...c, capture: { ...(c.capture ?? {}), [k]: v } } : c)
  }

  // Aggregate "live" tmux sessions per VM from the polled job list.
  const sessionsByVM = useMemo(() => {
    const m = new Map<string, Job[]>()
    for (const j of jobs) {
      if (!j.tmux) continue
      const arr = m.get(j.vm) ?? []
      arr.push(j)
      m.set(j.vm, arr)
    }
    return m
  }, [jobs])

  const navItems: { id: SectionId; label: string }[] = [
    { id: 'orch',    label: 'Orchestrator' },
    { id: 'access',  label: 'Access' },
    { id: 'capture', label: 'Capture' },
    { id: 'vms',     label: 'VMs' },
    { id: 'targets', label: 'Targets' },
    { id: 'usage',   label: 'Usage' },
    { id: 'danger',  label: 'Danger zone' },
  ]

  return (
    <div className="absolute inset-0 z-30 bg-white dark:bg-zinc-950 flex flex-col">
      <div className="px-8 h-14 flex items-center gap-3 border-b border-zinc-200 dark:border-zinc-800 flex-shrink-0">
        <button
          onClick={onClose}
          className="text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 flex items-center gap-1 text-[13px]"
          title="Back (esc)"
        >
          <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
            <polyline points="15 18 9 12 15 6" />
          </svg>
        </button>
        <span className="serif italic text-[24px] text-zinc-900 dark:text-zinc-100 ml-2">Settings</span>
        <div className="flex-1" />
        {status && <span className="text-[12px] text-zinc-500 dark:text-zinc-400">{status}</span>}
        <button
          onClick={save}
          disabled={!dirty || status === 'saving'}
          className="text-[12px] px-3 py-1.5 rounded-md bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 disabled:opacity-40 disabled:cursor-not-allowed hover:opacity-90"
        >Save</button>
      </div>

      <div className="flex-1 min-h-0 flex flex-col md:flex-row">
        {/* mobile: horizontal scroll chip rail; desktop: left aside */}
        <aside className="md:w-48 flex-shrink-0 border-b md:border-b-0 md:border-r border-zinc-200 dark:border-zinc-800 px-3 py-3 md:py-6 overflow-x-auto md:overflow-y-auto">
          <nav className="flex md:flex-col gap-0.5 whitespace-nowrap">
            {navItems.map((it) => (
              <button
                key={it.id}
                onClick={() => setSection(it.id)}
                className={
                  'text-left px-3 py-1.5 rounded-md text-[13px] transition-colors flex-shrink-0 ' +
                  (section === it.id
                    ? 'bg-zinc-100 dark:bg-zinc-800 text-zinc-900 dark:text-zinc-100'
                    : 'text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100')
                }
              >{it.label}</button>
            ))}
          </nav>
        </aside>

        <main className="flex-1 min-w-0 overflow-auto">
          <div className="max-w-[820px] mx-auto px-4 sm:px-8 md:px-10 py-6 md:py-10 space-y-8">
            {section === 'orch' && (
              <>
                <Section title="GitHub">
                  <Field label="Inbox repo" hint="Issues filed here drive orchid. Labels map to targets.">
                    <RepoPicker
                      value={gh?.inbox_repo ?? ''}
                      onChange={(v) => setGhField('inbox_repo', v)}
                      repos={repos}
                      error={reposError}
                      placeholder="pick or type owner/repo"
                    />
                  </Field>
                </Section>
                <Section title="Orchestrator" subtitle="Core swarm settings — applied on next restart.">
                  <Field label="Poll interval" hint="How often to scan the inbox (e.g. 20s).">
                    <Input value={cfg?.poll_interval ?? ''} onChange={(v) => setField('poll_interval', v)} placeholder="20s" />
                  </Field>
                  <Field label="Branch prefix">
                    <Input value={cfg?.branch_prefix ?? ''} onChange={(v) => setField('branch_prefix', v)} placeholder="orch/" />
                  </Field>
                  <Field label="HTTP address" hint="Where orch's dashboard server listens.">
                    <Input value={cfg?.http_addr ?? ''} onChange={(v) => setField('http_addr', v)} placeholder=":8000" />
                  </Field>
                  <Field label="HTTP secret" hint="Bearer token gating the local dashboard.">
                    <Input value={cfg?.http_secret ?? ''} onChange={(v) => setField('http_secret', v)} placeholder="…" secret />
                  </Field>
                  <Field label="Bot login">
                    <Input value={cfg?.bot_login ?? ''} onChange={(v) => setField('bot_login', v)} placeholder="yourbot" />
                  </Field>
                  <Field label="Bot email">
                    <Input value={cfg?.bot_email ?? ''} onChange={(v) => setField('bot_email', v)} placeholder="yourbot@users.noreply.github.com" />
                  </Field>
                  <Field label="ntfy topic">
                    <Input value={cfg?.ntfy_topic ?? ''} onChange={(v) => setField('ntfy_topic', v)} placeholder="orchid-…" />
                  </Field>
                </Section>
              </>
            )}

            {section === 'access' && (
              <Section
                title="Access"
                subtitle="You always have access. Add GitHub users you want to share this dashboard with — they sign in via OAuth and only see your subdomain."
              >
                <AllowedUsers
                  values={cfg?.allowed_logins ?? []}
                  onChange={(v) => setField('allowed_logins', v)}
                />
              </Section>
            )}

            {section === 'capture' && (
              <>
                <Section
                  title="Connect Orchid Capture"
                  subtitle="One-click handoff to the macOS app — or copy the values into the iOS app's Settings."
                >
                  {cfg?.capture?.auth_token && (
                    <div className="mb-4">
                      <a
                        href={`orchid://configure?endpoint=${encodeURIComponent(`https://${location.host}/api/drafts`)}&token=${encodeURIComponent(cfg.capture.auth_token)}`}
                        className="inline-flex items-center gap-2 text-[12.5px] mono px-3 py-2 rounded-md bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 hover:opacity-90"
                      >
                        <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
                          <polyline points="15 3 21 3 21 9" />
                          <line x1="10" y1="14" x2="21" y2="3" />
                          <path d="M21 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5" />
                        </svg>
                        Open in macOS app
                      </a>
                    </div>
                  )}
                  <Field label="Endpoint">
                    <CopyValue value={`https://${location.host}/api/drafts`} />
                  </Field>
                  <Field label="Auth token" hint="X-Capture-Token. Rotate any time — clients pick up the new value on next request.">
                    <div className="flex items-center gap-2">
                      <div className="flex-1">
                        <Input
                          value={cfg?.capture?.auth_token ?? ''}
                          onChange={(v) => setCaptureField('auth_token', v)}
                          placeholder="…"
                          secret
                        />
                      </div>
                      <button
                        onClick={() => setCaptureField('auth_token', cryptoToken())}
                        className="mono text-[11px] px-3 py-2 rounded-md ring-1 ring-zinc-300 dark:ring-zinc-700 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-100 dark:hover:bg-zinc-800"
                      >regenerate</button>
                    </div>
                  </Field>
                  <Field label="Assets dir" hint="Where uploaded screenshots / clips are stored.">
                    <Input
                      value={cfg?.capture?.assets_dir ?? ''}
                      onChange={(v) => setCaptureField('assets_dir', v)}
                      placeholder="/root/orch/captures"
                    />
                  </Field>
                  <Field label="Public URL" hint="Base URL used to embed images in issue bodies. Leave blank if you don't have one.">
                    <Input
                      value={cfg?.capture?.public_url ?? ''}
                      onChange={(v) => setCaptureField('public_url', v)}
                      placeholder={`https://${location.host}`}
                    />
                  </Field>
                </Section>
              </>
            )}

            {section === 'vms' && (
              <Section
                title="VMs"
                subtitle="Worker sessions run on boxes that have joined this orch. Bring a new one online by running the join command on it — no SSH config to fill in here."
              >
                <VMJoinGuide vms={vms} sessionsByVM={sessionsByVM} relay={relay} />
              </Section>
            )}

            {section === 'targets' && (
              <Section
                title="Targets"
                subtitle="Inbox labels → work repos. Add a repo and the label defaults to its name (override if you want)."
              >
                <TargetsList targets={targets} setTargets={setTargets} repos={repos} reposError={reposError} />
              </Section>
            )}

            {section === 'usage' && (
              <Section
                title="Usage"
                subtitle="Per-session Claude spend + context, pulled from each pane's statusline feed. Updates in near-real-time."
              >
                <UsageTable jobs={jobs} quota={state.quota} />
              </Section>
            )}

            {section === 'danger' && (
              <Section
                title="Danger zone"
                subtitle="These actions can't be undone from the dashboard."
              >
                <div className="rounded-xl ring-1 ring-rose-200 dark:ring-rose-900/50 p-5 flex items-start gap-5">
                  <div className="flex-1">
                    <div className="text-[14px] text-zinc-900 dark:text-zinc-100 font-medium">Revoke agent token</div>
                    <div className="text-[12px] text-zinc-500 dark:text-zinc-400 mt-1">
                      Disconnects the current orch instance. Sign in again to mint a fresh token,
                      then run <code className="mono">orch join</code> with the new credentials.
                    </div>
                  </div>
                  <button
                    onClick={async () => {
                      if (!confirm('Revoke the current agent token? Your orch will disconnect.')) return
                      const r = await fetch('/api/_relay/revoke', { method: 'POST', credentials: 'include' })
                      if (r.ok) alert('Token revoked. Sign in again to mint a new one.')
                      else alert('Revoke failed: ' + (await r.text()))
                    }}
                    className="mono text-[12px] px-3 py-1.5 rounded-md ring-1 ring-rose-300 dark:ring-rose-700 text-rose-700 dark:text-rose-300 hover:bg-rose-50 dark:hover:bg-rose-950"
                  >
                    Revoke
                  </button>
                </div>
              </Section>
            )}
          </div>
        </main>
      </div>
    </div>
  )
}

function Th({ children, align }: { children: React.ReactNode; align?: 'right' | 'left' }) {
  return <th className={`px-4 py-2 font-medium ${align === 'right' ? 'text-right' : 'text-left'}`}>{children}</th>
}
function Td({ children, align }: { children: React.ReactNode; align?: 'right' | 'left' }) {
  return <td className={`px-4 py-2.5 ${align === 'right' ? 'text-right' : 'text-left'} text-zinc-900 dark:text-zinc-100`}>{children}</td>
}

function Section({ title, subtitle, children }: { title: string; subtitle?: string; children: React.ReactNode }) {
  return (
    <section>
      <div className="mb-5">
        <h3 className="serif italic text-[28px] leading-none text-zinc-900 dark:text-zinc-100">{title}</h3>
        {subtitle && <p className="text-[13px] text-zinc-500 dark:text-zinc-400 mt-2 max-w-[640px]">{subtitle}</p>}
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
function CopyValue({ value, secret }: { value: string; secret?: boolean }) {
  const [revealed, setRevealed] = useState(!secret)
  const [copied, setCopied] = useState(false)
  const display = revealed ? value : value.replace(/./g, '•')
  return (
    <div className="flex items-center gap-2 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-md px-3 py-2">
      <code className="mono flex-1 text-[12px] text-zinc-900 dark:text-zinc-100 truncate select-all">{display}</code>
      {secret && (
        <button
          onClick={() => setRevealed((v) => !v)}
          className="text-[11px] mono text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200"
        >{revealed ? 'hide' : 'show'}</button>
      )}
      <button
        onClick={() => {
          navigator.clipboard.writeText(value).catch(() => {})
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        }}
        className="text-[11px] mono px-2 py-0.5 rounded bg-zinc-200 dark:bg-zinc-800 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-300 dark:hover:bg-zinc-700"
      >{copied ? 'copied' : 'copy'}</button>
    </div>
  )
}

interface GhProfile { login: string; name?: string; bio?: string; avatar_url?: string }

function useGhProfiles(logins: string[]): Map<string, GhProfile | 'loading' | 'missing'> {
  // Public unauth lookup of /users/<login>. Cached in a module-level
  // map so flipping sections doesn't refetch. Errors swallowed — we
  // fall back to a generic avatar placeholder.
  const [, force] = useState(0)
  useEffect(() => {
    for (const login of logins) {
      const key = login.toLowerCase()
      if (!key || profileCache.has(key)) continue
      profileCache.set(key, 'loading')
      fetch(`https://api.github.com/users/${encodeURIComponent(login)}`)
        .then((r) => r.ok ? r.json() : Promise.reject(r.statusText))
        .then((j: any) => {
          profileCache.set(key, { login: j.login, name: j.name ?? undefined, bio: j.bio ?? undefined, avatar_url: j.avatar_url })
          force((n) => n + 1)
        })
        .catch(() => {
          profileCache.set(key, 'missing')
          force((n) => n + 1)
        })
    }
  }, [logins.join(',')])
  const m = new Map<string, GhProfile | 'loading' | 'missing'>()
  for (const login of logins) m.set(login.toLowerCase(), profileCache.get(login.toLowerCase()) ?? 'loading')
  return m
}
const profileCache = new Map<string, GhProfile | 'loading' | 'missing'>()

// VMJoinGuide replaces the old form-based VM CRUD. It surfaces the
// install + join command operators run on a new box to bring it
// online, plus a read-only roster of VMs the orch already knows about
// (with their live session counts).
//
// The command needs the relay subdomain + agent token to embed in the
// `orch join` URL — both come from /api/_relay/info, the same endpoint
// the first-run InstallModal uses. Local-only orchs (no relay) get a
// fallback that points at swarm.hcl, since there's no relay endpoint
// for a fresh box to dial into.
function VMJoinGuide({ vms, sessionsByVM, relay }: {
  vms: VMCfg[]
  sessionsByVM: Map<string, Job[]>
  relay: RelayInfo | null
}) {
  // relay-info now arrives via the App-level events WS. Treat null as
  // "loading"; once the WS lands its first frame, the JoinCommandCard
  // can render. Local-only orchs (no relay agent token) fall into the
  // unavailable branch automatically because relay.token stays null.
  const info: RelayInfo | null | 'unavailable' = relay ?? null

  const isLocal = (vm: VMCfg) =>
    vm.host === 'localhost' || vm.host === '127.0.0.1' || vm.host === '::1'

  return (
    <div className="space-y-6">
      <JoinCommandCard info={info} />
      <div>
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500 mb-2 px-1">Connected</div>
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
          {vms.length === 0 && (
            <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
              No VMs yet — run the command above on a box to bring it online.
            </div>
          )}
          {vms.map((vm, i) => {
            const live = sessionsByVM.get(vm.name)?.length ?? 0
            const local = isLocal(vm)
            return (
              <div key={vm.name + i} className="flex items-center gap-3 px-4 py-3">
                <span className="w-8 h-8 rounded-md flex items-center justify-center flex-shrink-0 bg-zinc-100 dark:bg-zinc-800 text-zinc-500 dark:text-zinc-400">
                  {local ? (
                    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
                      <rect x="3" y="4" width="18" height="12" rx="2" />
                      <line x1="8" y1="20" x2="16" y2="20" />
                      <line x1="12" y1="16" x2="12" y2="20" />
                    </svg>
                  ) : (
                    <svg width={16} height={16} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
                      <circle cx="12" cy="12" r="9" />
                      <line x1="3" y1="12" x2="21" y2="12" />
                      <path d="M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18" />
                    </svg>
                  )}
                </span>
                <div className="flex-1 min-w-0">
                  <div className="mono text-[13.5px] font-medium text-zinc-900 dark:text-zinc-100 truncate">
                    {vm.name || '(unnamed)'}
                  </div>
                  <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 mono truncate">
                    {local ? 'localhost' : `${vm.user ?? 'root'}@${vm.host ?? '?'}`} · {vm.agent || 'claude'}
                  </div>
                </div>
                <span className="mono text-[11px] text-zinc-400 dark:text-zinc-500 flex-shrink-0">
                  {live} / {vm.capacity ?? '∞'}
                </span>
              </div>
            )
          })}
        </div>
        <div className="mt-2 px-1 text-[11.5px] text-zinc-400 dark:text-zinc-500">
          Per-VM SSH settings, capacity, agent, and bot overrides live in <code className="mono">swarm.hcl</code>.
        </div>
      </div>
    </div>
  )
}

import type { UsageHistoryRow } from './types'

/// Stacked-bar chart of daily Claude spend over a rolling window.
/// SVG, no chart library — every dep we keep out is one less hit on
/// the bundle size. X axis = day, Y axis = USD. Bars are stacked by
/// model family so a glance shows where the budget went (opus vs
/// sonnet vs haiku splits).
function UsageChart({ rows, days }: { rows: UsageHistoryRow[]; days: number }) {
  type DayBar = { date: string; opus: number; sonnet: number; haiku: number; other: number; total: number }
  const grid = useMemo<DayBar[]>(() => {
    // Build the contiguous date axis even when a day has zero spend.
    // Otherwise an idle weekend leaves a hole and the bars shift.
    const by = new Map<string, DayBar>()
    const today = new Date()
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(today.getTime() - i * 86400_000)
      const k = d.toISOString().slice(0, 10)
      by.set(k, { date: k, opus: 0, sonnet: 0, haiku: 0, other: 0, total: 0 })
    }
    for (const r of rows) {
      const bar = by.get(r.date)
      if (!bar) continue
      const m = r.model.toLowerCase()
      const fam = m.includes('opus') ? 'opus'
        : m.includes('sonnet') ? 'sonnet'
        : m.includes('haiku') ? 'haiku' : 'other'
      bar[fam] += r.cost_usd
      bar.total += r.cost_usd
    }
    return Array.from(by.values())
  }, [rows, days])

  const max = Math.max(0.01, ...grid.map((g) => g.total))
  const total = grid.reduce((acc, g) => acc + g.total, 0)
  const W = 760, H = 200, pad = { l: 36, r: 12, t: 12, b: 22 }
  const innerW = W - pad.l - pad.r
  const innerH = H - pad.t - pad.b
  const barW = innerW / grid.length

  const fam = {
    opus:   { fill: '#a78bfa', label: 'opus' },
    sonnet: { fill: '#34d399', label: 'sonnet' },
    haiku:  { fill: '#60a5fa', label: 'haiku' },
    other:  { fill: '#71717a', label: 'other' },
  } as const

  const ticks = [0, 0.25, 0.5, 0.75, 1].map((p) => ({ y: pad.t + innerH - p * innerH, v: p * max }))

  const [hover, setHover] = useState<{ i: number; x: number; y: number } | null>(null)
  const svgRef = useRef<SVGSVGElement | null>(null)
  const hovered = hover ? grid[hover.i] : null

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5 relative">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
          Daily spend · last {days}d
        </div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          ${total.toFixed(2)} window total
        </div>
      </div>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        className="w-full"
        onPointerLeave={() => setHover(null)}
      >
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={pad.l} x2={W - pad.r} y1={t.y} y2={t.y} stroke="currentColor" className="text-zinc-200 dark:text-zinc-800" strokeDasharray={i === 0 ? '' : '2 3'} />
            <text x={pad.l - 6} y={t.y + 3} textAnchor="end" className="fill-zinc-400 dark:fill-zinc-500" fontSize="9">
              ${t.v.toFixed(t.v < 1 ? 2 : 1)}
            </text>
          </g>
        ))}
        {grid.map((g, i) => {
          const x = pad.l + i * barW
          const stacks: { fill: string; v: number }[] = [
            { fill: fam.opus.fill,   v: g.opus },
            { fill: fam.sonnet.fill, v: g.sonnet },
            { fill: fam.haiku.fill,  v: g.haiku },
            { fill: fam.other.fill,  v: g.other },
          ].filter((s) => s.v > 0)
          let yCursor = pad.t + innerH
          const hot = hover?.i === i
          return (
            <g
              key={g.date}
              onPointerEnter={(e) => {
                const r = svgRef.current?.getBoundingClientRect()
                if (!r) return
                setHover({ i, x: e.clientX - r.left, y: e.clientY - r.top })
              }}
              onPointerMove={(e) => {
                const r = svgRef.current?.getBoundingClientRect()
                if (!r) return
                setHover({ i, x: e.clientX - r.left, y: e.clientY - r.top })
              }}
              style={{ cursor: 'pointer' }}
            >
              {/* invisible full-height hit target so even a $0 day is hoverable */}
              <rect x={x} y={pad.t} width={barW} height={innerH} fill="transparent" />
              {stacks.map((s, j) => {
                const h = (s.v / max) * innerH
                yCursor -= h
                return <rect key={j} x={x + 1} y={yCursor} width={Math.max(0, barW - 2)} height={Math.max(0, h)} fill={s.fill} opacity={hover && !hot ? 0.5 : 1} />
              })}
              {i % Math.ceil(days / 8) === 0 && (
                <text x={x + barW / 2} y={H - 6} textAnchor="middle" className="fill-zinc-400 dark:fill-zinc-500" fontSize="9">
                  {g.date.slice(5)}
                </text>
              )}
            </g>
          )
        })}
      </svg>
      {hovered && hover && (
        <div
          className="pointer-events-none absolute bg-zinc-900/95 dark:bg-zinc-100/95 text-zinc-50 dark:text-zinc-900 mono text-[11px] rounded px-2 py-1 shadow-lg z-10"
          style={{ left: Math.min(hover.x + 14, 600), top: Math.max(40, hover.y - 8) }}
        >
          <div className="text-[10.5px] opacity-80">{hovered.date}</div>
          <div className="tabular-nums font-medium">${hovered.total.toFixed(2)}</div>
          {hovered.opus   > 0 && <div className="tabular-nums">opus ${hovered.opus.toFixed(2)}</div>}
          {hovered.sonnet > 0 && <div className="tabular-nums">sonnet ${hovered.sonnet.toFixed(2)}</div>}
          {hovered.haiku  > 0 && <div className="tabular-nums">haiku ${hovered.haiku.toFixed(2)}</div>}
          {hovered.other  > 0 && <div className="tabular-nums">other ${hovered.other.toFixed(2)}</div>}
        </div>
      )}
      <div className="flex items-center gap-3 mt-2 mono text-[10.5px] text-zinc-500 dark:text-zinc-400">
        {(['opus', 'sonnet', 'haiku', 'other'] as const).map((k) => (
          <span key={k} className="inline-flex items-center gap-1">
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: fam[k].fill }} />
            {fam[k].label}
          </span>
        ))}
      </div>
    </div>
  )
}

interface DonutSlice {
  key: string
  label: string
  value: number
  color: string
  meta?: string
}

/// Reusable donut chart. Hovering a slice highlights it (outer-ring
/// pop + dimmed siblings) and surfaces a tooltip with the label /
/// value / share, anchored at the cursor. Pure SVG + a thin
/// React-state hover model so we keep zero chart-lib deps.
function Donut({ slices, title, units = '$', subtitle }: {
  slices: DonutSlice[]
  title: string
  units?: string
  subtitle?: string
}) {
  const [hover, setHover] = useState<{ key: string; x: number; y: number } | null>(null)
  const ref = useRef<SVGSVGElement | null>(null)
  const total = useMemo(() => slices.reduce((acc, s) => acc + s.value, 0), [slices])
  // Build arcs. Skip zero slices so the legend isn't littered with
  // "0.0%" entries from sessions that ran but consumed nothing.
  const arcs = useMemo(() => {
    const r = 56, R = 92
    const cx = 110, cy = 110
    let a = -Math.PI / 2 // 12-o'clock start
    return slices.filter((s) => s.value > 0).map((s) => {
      const frac = s.value / Math.max(1e-9, total)
      const a2 = a + frac * Math.PI * 2
      const large = a2 - a > Math.PI ? 1 : 0
      const sx = cx + R * Math.cos(a),  sy = cy + R * Math.sin(a)
      const ex = cx + R * Math.cos(a2), ey = cy + R * Math.sin(a2)
      const sx2 = cx + r * Math.cos(a2), sy2 = cy + r * Math.sin(a2)
      const ex2 = cx + r * Math.cos(a),  ey2 = cy + r * Math.sin(a)
      const d = `M ${sx} ${sy} A ${R} ${R} 0 ${large} 1 ${ex} ${ey} L ${sx2} ${sy2} A ${r} ${r} 0 ${large} 0 ${ex2} ${ey2} Z`
      a = a2
      return { slice: s, d, frac }
    })
  }, [slices, total])

  const hoveredSlice = hover ? slices.find((s) => s.key === hover.key) : null
  const hoveredFrac = hoveredSlice ? hoveredSlice.value / Math.max(1e-9, total) : 0

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const r = ref.current?.getBoundingClientRect()
    if (!r) return
    setHover((h) => h ? { ...h, x: e.clientX - r.left, y: e.clientY - r.top } : h)
  }

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5 relative">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">{title}</div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          {units}{total.toFixed(2)}{subtitle ? ' · ' + subtitle : ''}
        </div>
      </div>
      <div className="flex items-center gap-6">
        <svg
          ref={ref}
          viewBox="0 0 220 220"
          className="flex-shrink-0"
          style={{ width: 220, height: 220 }}
          onPointerMove={onMove}
          onPointerLeave={() => setHover(null)}
        >
          {arcs.map(({ slice, d }) => {
            const dim = hover && hover.key !== slice.key ? 0.35 : 1
            const pop = hover && hover.key === slice.key
            return (
              <path
                key={slice.key}
                d={d}
                fill={slice.color}
                opacity={dim}
                style={{
                  transformOrigin: '110px 110px',
                  transform: pop ? 'scale(1.04)' : 'scale(1)',
                  transition: 'opacity 80ms linear, transform 80ms ease-out',
                  cursor: 'pointer',
                }}
                onPointerEnter={(e) => {
                  const r = ref.current?.getBoundingClientRect()
                  if (!r) return
                  setHover({ key: slice.key, x: e.clientX - r.left, y: e.clientY - r.top })
                }}
              />
            )
          })}
          {/* center label */}
          <text x={110} y={106} textAnchor="middle" className="fill-zinc-900 dark:fill-zinc-100 mono" fontSize="18">
            {hoveredSlice ? `${units}${hoveredSlice.value.toFixed(2)}` : `${units}${total.toFixed(2)}`}
          </text>
          <text x={110} y={124} textAnchor="middle" className="fill-zinc-500 dark:fill-zinc-400 mono" fontSize="10">
            {hoveredSlice ? `${(hoveredFrac * 100).toFixed(1)}%` : 'total'}
          </text>
        </svg>
        <div className="flex-1 min-w-0">
          <div className="grid grid-cols-1 gap-1.5 max-h-[200px] overflow-y-auto pr-2">
            {slices.filter((s) => s.value > 0).map((s) => {
              const frac = s.value / Math.max(1e-9, total)
              const dim = hover && hover.key !== s.key ? 'opacity-50' : ''
              return (
                <div
                  key={s.key}
                  className={'flex items-center gap-2 text-[11.5px] ' + dim}
                  onPointerEnter={() => setHover({ key: s.key, x: 0, y: 0 })}
                  onPointerLeave={() => setHover(null)}
                >
                  <span className="inline-block w-2 h-2 rounded-sm flex-shrink-0" style={{ background: s.color }} />
                  <span className="flex-1 min-w-0 truncate text-zinc-700 dark:text-zinc-300" title={s.meta || s.label}>
                    {s.label}
                  </span>
                  <span className="mono tabular-nums text-zinc-600 dark:text-zinc-300 w-14 text-right">{units}{s.value.toFixed(2)}</span>
                  <span className="mono tabular-nums text-zinc-400 dark:text-zinc-500 w-10 text-right">{(frac * 100).toFixed(1)}%</span>
                </div>
              )
            })}
          </div>
        </div>
      </div>
      {hoveredSlice && hover && hover.x > 0 && (
        <div
          className="pointer-events-none absolute bg-zinc-900/95 dark:bg-zinc-100/95 text-zinc-50 dark:text-zinc-900 mono text-[11px] rounded px-2 py-1 shadow-lg z-10"
          style={{ left: hover.x + 14, top: hover.y - 8 }}
        >
          <div>{hoveredSlice.label}</div>
          <div className="tabular-nums">{units}{hoveredSlice.value.toFixed(2)} · {(hoveredFrac * 100).toFixed(1)}%</div>
          {hoveredSlice.meta && <div className="text-[10px] opacity-70">{hoveredSlice.meta}</div>}
        </div>
      )}
    </div>
  )
}

const PALETTE_8 = ['#a78bfa','#34d399','#60a5fa','#f59e0b','#ec4899','#22d3ee','#f87171','#84cc16']

/// Donut: spend per session in the window. Top 8 by spend + "other".
/// Hover surfaces the session id and (if we can match it to a current
/// job) the issue title + repo.
function UsageBySessionDonut({
  rows, days, jobs,
}: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  const jobByIssue = useMemo(() => {
    const m = new Map<number, Job>()
    for (const j of jobs) m.set(j.issue, j)
    return m
  }, [jobs])
  const slices = useMemo<DonutSlice[]>(() => {
    const by = new Map<string, { total: number; issue: number }>()
    for (const r of rows) {
      const cur = by.get(r.session_id) ?? { total: 0, issue: 0 }
      cur.total += r.cost_usd
      if (r.issue) cur.issue = r.issue
      by.set(r.session_id, cur)
    }
    const sorted = Array.from(by.entries()).sort((a, b) => b[1].total - a[1].total)
    const top = sorted.slice(0, 8)
    const rest = sorted.slice(8).reduce((acc, [_, v]) => acc + v.total, 0)
    const out: DonutSlice[] = top.map(([sid, v], i) => {
      const job = v.issue ? jobByIssue.get(v.issue) : undefined
      const title = job?.issue_title ?? (v.issue ? `issue #${v.issue}` : sid.slice(0, 8))
      return {
        key: sid,
        label: title,
        value: v.total,
        color: PALETTE_8[i],
        meta: v.issue ? `#${v.issue} · ${job?.target_repo ?? 'closed session'}` : `session ${sid.slice(0, 12)}`,
      }
    })
    if (rest > 0) out.push({ key: '__other__', label: 'other sessions', value: rest, color: '#71717a' })
    return out
  }, [rows, jobByIssue])
  const sessionCount = useMemo(() => new Set(rows.map((r) => r.session_id)).size, [rows])
  return (
    <Donut
      slices={slices}
      title={`Spend by session · ${days}d`}
      subtitle={`${sessionCount} sessions`}
    />
  )
}

/// Donut: spend per upstream target repo. Session → issue → repo via
/// the same job map. Sessions whose issue was already torn down show
/// as "unknown" so the chart still totals correctly.
function UsageByRepoDonut({
  rows, days, jobs,
}: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  const jobByIssue = useMemo(() => {
    const m = new Map<number, Job>()
    for (const j of jobs) m.set(j.issue, j)
    return m
  }, [jobs])
  const slices = useMemo<DonutSlice[]>(() => {
    const by = new Map<string, number>()
    for (const r of rows) {
      const job = r.issue ? jobByIssue.get(r.issue) : undefined
      const repo = job?.target_repo ?? 'unknown'
      by.set(repo, (by.get(repo) ?? 0) + r.cost_usd)
    }
    const sorted = Array.from(by.entries()).sort((a, b) => b[1] - a[1])
    return sorted.map(([repo, v], i) => ({
      key: repo,
      label: repo,
      value: v,
      color: repo === 'unknown' ? '#71717a' : PALETTE_8[i % PALETTE_8.length],
    }))
  }, [rows, jobByIssue])
  return (
    <Donut
      slices={slices}
      title={`Spend by repo · ${days}d`}
      subtitle={`${slices.length} repos`}
    />
  )
}

// Legacy stub kept off to avoid dragging the prior bar implementation
// along. The donuts above replaced it entirely; callers shouldn't
// reach this.
function _legacyUsageBySessionChart({ rows, days, jobs }: { rows: UsageHistoryRow[]; days: number; jobs: Job[] }) {
  type DayBar = { date: string; parts: Record<string, number>; total: number }
  // Resolve session_id → human label from active jobs first, then
  // fall back to a short prefix so closed sessions still render.
  const labelFor = useMemo(() => {
    const m = new Map<string, string>()
    for (const j of jobs) {
      const tmux = j.tmux || ''
      // statusline session_id ≠ tmux id; we don't have a direct map
      // here. Best-effort: use the issue title indexed by tmux. UI
      // shows the short hash if the session is no longer tracked.
      m.set(tmux, j.issue_title || tmux)
    }
    return (sid: string) => sid.slice(0, 6)
  }, [jobs])

  const { grid, topSessions, colors } = useMemo(() => {
    const totals = new Map<string, number>()
    for (const r of rows) {
      totals.set(r.session_id, (totals.get(r.session_id) ?? 0) + r.cost_usd)
    }
    const sorted = Array.from(totals.entries()).sort((a, b) => b[1] - a[1])
    const top = sorted.slice(0, 8).map((e) => e[0])
    const topSet = new Set(top)

    const today = new Date()
    const by = new Map<string, DayBar>()
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(today.getTime() - i * 86400_000)
      const k = d.toISOString().slice(0, 10)
      by.set(k, { date: k, parts: {}, total: 0 })
    }
    for (const r of rows) {
      const bar = by.get(r.date); if (!bar) continue
      const key = topSet.has(r.session_id) ? r.session_id : '__other__'
      bar.parts[key] = (bar.parts[key] ?? 0) + r.cost_usd
      bar.total += r.cost_usd
    }
    const palette = ['#a78bfa','#34d399','#60a5fa','#f59e0b','#ec4899','#22d3ee','#f87171','#84cc16','#71717a']
    const colors: Record<string, string> = { __other__: '#71717a' }
    top.forEach((sid, i) => { colors[sid] = palette[i] ?? '#a1a1aa' })
    return { grid: Array.from(by.values()), topSessions: top, colors }
  }, [rows, days])

  const max = Math.max(0.01, ...grid.map((g) => g.total))
  const total = grid.reduce((acc, g) => acc + g.total, 0)
  const W = 760, H = 200, pad = { l: 36, r: 12, t: 12, b: 22 }
  const innerW = W - pad.l - pad.r
  const innerH = H - pad.t - pad.b
  const barW = innerW / grid.length
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((p) => ({ y: pad.t + innerH - p * innerH, v: p * max }))

  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
      <div className="flex items-center mb-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
          Spend by session · last {days}d
        </div>
        <div className="flex-1" />
        <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
          ${total.toFixed(2)} · top {topSessions.length} of {new Set(rows.map((r) => r.session_id)).size} sessions
        </div>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full">
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={pad.l} x2={W - pad.r} y1={t.y} y2={t.y} stroke="currentColor" className="text-zinc-200 dark:text-zinc-800" strokeDasharray={i === 0 ? '' : '2 3'} />
            <text x={pad.l - 6} y={t.y + 3} textAnchor="end" className="fill-zinc-400 dark:fill-zinc-500" fontSize="9">${t.v.toFixed(t.v < 1 ? 2 : 1)}</text>
          </g>
        ))}
        {grid.map((g, i) => {
          const x = pad.l + i * barW
          // Stack top sessions in their fixed order so colours stay
          // consistent across days, then append "other" on top.
          const order = [...topSessions, '__other__']
          let yCursor = pad.t + innerH
          return (
            <g key={g.date}>
              {order.map((sid) => {
                const v = g.parts[sid] ?? 0
                if (v <= 0) return null
                const h = (v / max) * innerH
                yCursor -= h
                return <rect key={sid} x={x + 1} y={yCursor} width={Math.max(0, barW - 2)} height={Math.max(0, h)} fill={colors[sid]} />
              })}
              {i % Math.ceil(days / 8) === 0 && (
                <text x={x + barW / 2} y={H - 6} textAnchor="middle" className="fill-zinc-400 dark:fill-zinc-500" fontSize="9">{g.date.slice(5)}</text>
              )}
              <title>{`${g.date}\n$${g.total.toFixed(2)}`}</title>
            </g>
          )
        })}
      </svg>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 mt-2 mono text-[10.5px] text-zinc-500 dark:text-zinc-400">
        {topSessions.map((sid) => (
          <span key={sid} className="inline-flex items-center gap-1" title={sid}>
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: colors[sid] }} />
            {labelFor(sid)}
          </span>
        ))}
        {Object.keys(colors).includes('__other__') && (
          <span className="inline-flex items-center gap-1">
            <span className="inline-block w-2 h-2 rounded-sm" style={{ background: colors['__other__'] }} />
            other
          </span>
        )}
      </div>
    </div>
  )
}

/// Rolled-up totals across a configurable window. Pulls the same
/// /api/usage_history payload and re-aggregates so the operator can
/// switch between day / week / month without a round trip.
function UsageRollups({ rows }: { rows: UsageHistoryRow[] }) {
  const today = new Date().toISOString().slice(0, 10)
  const windowSum = (days: number) => {
    const since = new Date(Date.now() - days * 86400_000).toISOString().slice(0, 10)
    return rows.filter((r) => r.date >= since && r.date <= today)
      .reduce((acc, r) => acc + r.cost_usd, 0)
  }
  const card = (label: string, days: number) => {
    const v = windowSum(days)
    return (
      <div className="rounded-lg ring-1 ring-zinc-200 dark:ring-zinc-800 px-4 py-3 flex-1">
        <div className="text-[10.5px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500 mb-1">{label}</div>
        <div className="mono text-[20px] text-zinc-900 dark:text-zinc-100 tabular-nums">${v.toFixed(2)}</div>
      </div>
    )
  }
  return (
    <div className="flex items-stretch gap-3">
      {card('Today',   1)}
      {card('7 days',  7)}
      {card('30 days', 30)}
    </div>
  )
}

// Settings → Usage tab. Sorted by spend desc so the most expensive
// sessions float to the top. Quota strip up top mirrors the
// (hidden-on-some-accounts) header chip so operators can see the
// 5h / 7d numbers when their plan exposes them.
function UsageTable({ jobs, quota }: { jobs: Job[]; quota?: State['quota'] }) {
  const rows = useMemo(() => {
    return jobs
      .filter((j) => j.usage)
      .slice()
      .sort((a, b) => (b.usage?.cost_usd ?? 0) - (a.usage?.cost_usd ?? 0))
  }, [jobs])
  const totalSpend = useMemo(
    () => rows.reduce((acc, j) => acc + (j.usage?.cost_usd ?? 0), 0),
    [rows],
  )
  const [history, setHistory] = useState<UsageHistoryRow[] | null>(null)
  const [days, setDays] = useState(30)
  useEffect(() => {
    let alive = true
    fetch(`/api/usage_history?days=${days}`, { credentials: 'include', cache: 'no-store' })
      .then((r) => r.ok ? r.json() : null)
      .then((j) => { if (alive) setHistory(j?.rows ?? []) })
      .catch(() => { if (alive) setHistory([]) })
    const id = setInterval(() => {
      fetch(`/api/usage_history?days=${days}`, { credentials: 'include', cache: 'no-store' })
        .then((r) => r.ok ? r.json() : null)
        .then((j) => { if (alive) setHistory(j?.rows ?? []) })
        .catch(() => {})
    }, 60_000)
    return () => { alive = false; clearInterval(id) }
  }, [days])
  return (
    <div className="space-y-6">
      {history && history.length > 0 && (
        <>
          <UsageRollups rows={history} />
          <div className="flex items-center justify-end gap-1">
            {[7, 30, 90].map((d) => (
              <button
                key={d}
                onClick={() => setDays(d)}
                className={
                  'mono text-[11px] px-2 py-1 rounded ' +
                  (days === d
                    ? 'bg-zinc-900 dark:bg-zinc-100 text-zinc-50 dark:text-zinc-900'
                    : 'text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-800')
                }
              >
                {d}d
              </button>
            ))}
          </div>
          <UsageChart rows={history} days={days} />
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
            <UsageBySessionDonut rows={history} days={days} jobs={jobs} />
            <UsageByRepoDonut    rows={history} days={days} jobs={jobs} />
          </div>
        </>
      )}
      {quota ? (
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
          <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500 mb-3">
            Subscription quota
          </div>
          <QuotaStrip quota={quota} />
        </div>
      ) : (
        <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 p-5">
          <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500 mb-2">
            Subscription quota
          </div>
          <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
            Not reported by Claude on this account / plan. Per-session
            spend and context still update below — they're parsed from
            the same statusline feed but don't depend on the optional{' '}
            <code className="mono">rate_limits</code> field.
          </div>
        </div>
      )}
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 overflow-hidden">
        <div className="flex items-center px-4 py-3 bg-zinc-50 dark:bg-zinc-900/60 border-b border-zinc-200 dark:border-zinc-800">
          <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">
            Per-session spend
          </div>
          <div className="flex-1" />
          <div className="mono text-[12px] text-zinc-600 dark:text-zinc-300 tabular-nums">
            ${totalSpend.toFixed(2)} total · {rows.length} active
          </div>
        </div>
        {rows.length === 0 && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No statusline samples yet. Sessions report as soon as their next render tick lands.
          </div>
        )}
        <div className="divide-y divide-zinc-100 dark:divide-zinc-800/70">
          {rows.map((j) => {
            const cost = j.usage?.cost_usd ?? 0
            const ctx = j.usage?.context_pct
            const repo = j.target_repo ? j.target_repo.split('/')[1] : j.target || '—'
            return (
              <div key={j.tmux} className="flex items-center gap-3 px-4 py-3">
                <div className="flex-1 min-w-0">
                  <div className="text-[13px] text-zinc-900 dark:text-zinc-100 truncate">
                    {j.issue_title || j.tmux}
                  </div>
                  <div className="mono text-[11px] text-zinc-500 dark:text-zinc-400 truncate">
                    {repo} · {j.tmux}
                    {j.usage?.model ? ' · ' + j.usage.model : ''}
                  </div>
                </div>
                {typeof ctx === 'number' && (
                  <div className="flex items-center gap-1.5 flex-shrink-0">
                    <span className="mono text-[11px] text-zinc-400 dark:text-zinc-500">ctx</span>
                    <div className="relative h-1.5 w-16 rounded-full overflow-hidden bg-zinc-200 dark:bg-zinc-800">
                      <div
                        className="absolute inset-y-0 left-0 bg-violet-500/80"
                        style={{ width: `${Math.min(100, Math.max(0, ctx))}%` }}
                      />
                    </div>
                    <span className="mono text-[11px] text-zinc-500 dark:text-zinc-400 tabular-nums w-8">
                      {Math.round(ctx)}%
                    </span>
                  </div>
                )}
                <span className="mono text-[12.5px] text-zinc-900 dark:text-zinc-100 tabular-nums w-16 text-right flex-shrink-0">
                  ${cost.toFixed(2)}
                </span>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

function JoinCommandCard({ info }: { info: RelayInfo | null | 'unavailable' }) {
  // Two branches:
  //   - Relay-managed orch: build the install + join commands from the
  //     subdomain + agent token in /api/_relay/info. Same shape as the
  //     first-run InstallModal so the muscle memory carries over.
  //   - Local orch (no relay, or relay endpoint missing): there's no
  //     network identity for a fresh VM to dial into, so we point
  //     the operator at swarm.hcl instead.
  if (info === null) {
    return (
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500 mb-3">Add a VM</div>
        <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400">Loading…</div>
      </div>
    )
  }
  if (info === 'unavailable' || !info.login || !info.token) {
    return (
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6 space-y-3">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">Add a VM</div>
        <div className="text-[13px] text-zinc-700 dark:text-zinc-300 leading-relaxed">
          This orch is running standalone — there's no relay endpoint for a
          new VM to join through.
        </div>
        <div className="text-[12.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
          Add a <code className="mono">vm "&lt;name&gt;" {`{ … }`}</code> block to{' '}
          <code className="mono">swarm.hcl</code> and restart orchid. To switch to
          a relay-managed orch, sign in at{' '}
          <code className="mono">orchid.littledivy.com</code> and run{' '}
          <code className="mono">orch join</code> with the issued token.
        </div>
      </div>
    )
  }

  const sub = info.login.toLowerCase().replace(/[^a-z0-9-]/g, '')
  // ROOT_DOMAIN comes from the relay so multi-label roots like
  // orchid.littledivy.com don't get truncated to littledivy.com by a
  // naive slice(-2). Falls back to hostname for older relays missing
  // the field.
  const root = info.root ?? location.hostname.split('.').slice(-2).join('.')
  const install = `curl -fsSL https://${root}/install.sh | bash`
  const join = `orch join wss://${sub}.${root}/agent ${info.token}`
  return (
    <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 bg-zinc-50 dark:bg-zinc-900/40 p-6 space-y-5">
      <div className="flex items-center justify-between">
        <div className="text-[12px] uppercase tracking-wider text-zinc-400 dark:text-zinc-500">Add a VM</div>
        <div className="text-[11.5px] text-zinc-400 dark:text-zinc-500">SSH into the new box as root</div>
      </div>
      <JoinStep n={1} label="Install orch">
        <JoinCmd value={install} />
      </JoinStep>
      <JoinStep n={2} label="Join this orch">
        <JoinCmd value={join} secret />
      </JoinStep>
      <div className="text-[11.5px] text-zinc-500 dark:text-zinc-400 leading-relaxed">
        The join token grants this orch's worker pool — treat it like a password.
        Rotate it from the <span className="italic">Danger zone</span> tab if it leaks.
      </div>
    </div>
  )
}

function JoinStep({ n, label, children }: { n: number; label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="flex items-center gap-2 mb-1.5">
        <span className="mono text-[10.5px] text-zinc-400 dark:text-zinc-500">{n}.</span>
        <span className="text-[12px] text-zinc-600 dark:text-zinc-300">{label}</span>
      </div>
      {children}
    </div>
  )
}

function JoinCmd({ value, secret }: { value: string; secret?: boolean }) {
  const [copied, setCopied] = useState(false)
  const [revealed, setRevealed] = useState(!secret)
  // Mask the trailing token. The command structure stays visible so the
  // operator can sanity-check the URL before pasting.
  const display = revealed ? value : value.replace(/(\S+)$/, (m) => m.replace(/./g, '•'))
  return (
    <div className="relative group">
      <pre className="bg-zinc-950 text-zinc-100 mono text-[12px] p-3 pr-24 rounded-lg overflow-x-auto whitespace-pre">{display}</pre>
      <div className="absolute top-1/2 right-2 -translate-y-1/2 flex items-center gap-1">
        {secret && (
          <button
            onClick={() => setRevealed((v) => !v)}
            className="mono text-[10.5px] px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-300"
          >{revealed ? 'hide' : 'show'}</button>
        )}
        <button
          onClick={() => {
            navigator.clipboard.writeText(value).catch(() => {})
            setCopied(true)
            setTimeout(() => setCopied(false), 1200)
          }}
          className="mono text-[10.5px] px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-300 opacity-80 group-hover:opacity-100 transition-opacity"
        >{copied ? 'copied' : 'copy'}</button>
      </div>
    </div>
  )
}

function TargetsList({ targets, setTargets, repos, reposError }: {
  targets: TargetCfg[]
  setTargets: React.Dispatch<React.SetStateAction<TargetCfg[]>>
  repos: RepoOption[] | null
  reposError?: string | null
}) {
  const [adding, setAdding] = useState(false)
  const repoBy = useMemo(() => {
    const m = new Map<string, RepoOption>()
    for (const r of repos ?? []) m.set(r.full_name, r)
    return m
  }, [repos])

  return (
    <div>
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
        {targets.length === 0 && !adding && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No targets yet. Add a repo below to wire one up.
          </div>
        )}
        {targets.map((t, i) => {
          const repo = t.repo ? repoBy.get(t.repo) : undefined
          const [owner, name] = (t.repo ?? '').split('/')
          const avatar = repo?.avatar ?? (owner ? `https://github.com/${owner}.png?size=80` : undefined)
          return (
            <div key={t.name + i} className="flex items-center gap-3 px-4 py-3 group">
              {avatar ? (
                <img src={avatar} alt="" className="w-8 h-8 rounded-md ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0" />
              ) : (
                <div className="w-8 h-8 rounded-md bg-zinc-100 dark:bg-zinc-800 flex-shrink-0" />
              )}
              <div className="text-[13.5px] truncate flex-1 min-w-0">
                <span className="text-zinc-400 dark:text-zinc-500">{owner || '—'}</span>
                <span className="text-zinc-300 dark:text-zinc-600 mx-0.5">/</span>
                <span className="mono text-zinc-900 dark:text-zinc-100">{name || '—'}</span>
              </div>
              <input
                value={t.name}
                onChange={(e) => setTargets((arr) => arr.map((x, j) => j === i ? { ...x, name: e.target.value } : x))}
                placeholder="label"
                className="mono text-[12px] w-24 px-2 py-1 rounded bg-transparent outline-none text-zinc-500 dark:text-zinc-400 focus:bg-zinc-50 dark:focus:bg-zinc-900 focus:text-zinc-900 dark:focus:text-zinc-100 text-right"
                title="Label used in the inbox to route to this target"
              />
              <button
                onClick={() => setTargets((arr) => arr.filter((_, j) => j !== i))}
                className="text-[14px] text-zinc-400 hover:text-rose-600 opacity-0 group-hover:opacity-100 transition-opacity"
                title="remove"
              >×</button>
            </div>
          )
        })}
        {adding && (
          <div className="px-4 py-3 bg-zinc-50 dark:bg-zinc-950">
            <RepoPicker
              value=""
              onChange={(repo) => {
                if (!repo) return
                const label = (repo.split('/').pop() ?? '').toLowerCase().replace(/[^a-z0-9-_]/g, '-')
                setTargets((arr) => [...arr, { name: label, repo }])
                setAdding(false)
              }}
              repos={repos}
              error={reposError}
              placeholder="pick a repo to add as a target"
            />
          </div>
        )}
      </div>
      <button
        onClick={() => setAdding((a) => !a)}
        className="mt-3 mono text-[12px] px-3 py-1.5 rounded-md ring-1 ring-zinc-300 dark:ring-zinc-700 text-zinc-700 dark:text-zinc-200 hover:bg-zinc-100 dark:hover:bg-zinc-800"
      >{adding ? 'cancel' : '+ add target'}</button>
    </div>
  )
}

function AllowedUsers({ values, onChange }: { values: string[]; onChange: (v: string[]) => void }) {
  const [owner, setOwner] = useState<string | null>(null)
  useEffect(() => {
    let alive = true
    fetch('/api/_relay/info', { credentials: 'include' })
      .then((r) => r.ok ? r.json() : null)
      .then((j) => { if (alive && j?.login) setOwner(j.login) })
      .catch(() => {})
    return () => { alive = false }
  }, [])
  // De-dupe: if the owner was also added to allowed_logins by hand,
  // don't render them twice. Owner always pinned at the top.
  const collaborators = useMemo(() => {
    if (!owner) return values
    return values.filter((v) => v.toLowerCase() !== owner.toLowerCase())
  }, [values, owner])
  const profiles = useGhProfiles([owner, ...collaborators].filter(Boolean) as string[])
  const [draft, setDraft] = useState('')
  const add = () => {
    const v = draft.trim().replace(/^@/, '')
    if (!v || values.includes(v)) { setDraft(''); return }
    onChange([...values, v])
    setDraft('')
  }
  return (
    <div className="space-y-3">
      <div className="rounded-xl ring-1 ring-zinc-200 dark:ring-zinc-800 divide-y divide-zinc-100 dark:divide-zinc-800/70 overflow-hidden">
        {owner && (
          <UserRow login={owner} profile={profiles.get(owner.toLowerCase())} owner />
        )}
        {collaborators.length === 0 && (
          <div className="px-4 py-5 text-[13px] text-zinc-500 dark:text-zinc-400 text-center">
            No collaborators yet. Add a GitHub login below to share access.
          </div>
        )}
        {collaborators.map((login) => (
          <UserRow
            key={login}
            login={login}
            profile={profiles.get(login.toLowerCase())}
            onRemove={() => onChange(values.filter((v) => v !== login))}
          />
        ))}
      </div>
      <div className="flex items-center gap-2 bg-zinc-50 dark:bg-zinc-950 ring-1 ring-zinc-200 dark:ring-zinc-800 rounded-lg px-3 py-2">
        <span className="text-zinc-400 dark:text-zinc-500 text-[14px]">@</span>
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add() }
          }}
          onBlur={add}
          placeholder="github-login"
          spellCheck={false}
          autoComplete="off"
          className="mono flex-1 bg-transparent outline-none text-[13px] text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
        />
        <button
          onClick={add}
          disabled={!draft.trim()}
          className="mono text-[11.5px] px-3 py-1 rounded bg-zinc-900 text-zinc-50 dark:bg-zinc-100 dark:text-zinc-900 disabled:opacity-30 disabled:cursor-not-allowed hover:opacity-90"
        >add</button>
      </div>
    </div>
  )
}

function UserRow({ login, profile, owner, onRemove }: {
  login: string
  profile?: GhProfile | 'loading' | 'missing'
  owner?: boolean
  onRemove?: () => void
}) {
  const p: GhProfile | null = profile && typeof profile === 'object' ? profile : null
  return (
    <div className="flex items-center gap-3 px-4 py-3 group">
      <img
        src={p?.avatar_url ?? `https://github.com/${login}.png?size=80`}
        alt=""
        className="w-8 h-8 rounded-full ring-1 ring-zinc-200 dark:ring-zinc-800 flex-shrink-0"
        onError={(e) => { (e.currentTarget as HTMLImageElement).src = `https://github.com/identicons/${encodeURIComponent(login)}.png` }}
      />
      <a
        href={`https://github.com/${login}`}
        target="_blank"
        rel="noopener noreferrer"
        className="mono text-[13.5px] text-zinc-900 dark:text-zinc-100 hover:underline truncate flex-1"
      >@{login}</a>
      {p?.name && (
        <span className="text-[12px] text-zinc-400 dark:text-zinc-500 truncate hidden sm:inline">{p.name}</span>
      )}
      {owner && (
        <span className="text-[11px] text-zinc-400 dark:text-zinc-500">you</span>
      )}
      {profile === 'missing' && (
        <span className="text-[11px] text-rose-500 dark:text-rose-400">not found</span>
      )}
      {onRemove && (
        <button
          onClick={onRemove}
          className="text-[14px] text-zinc-400 hover:text-rose-600 opacity-0 group-hover:opacity-100 transition-opacity"
          title="remove"
        >×</button>
      )}
    </div>
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
    <div className="absolute inset-0 top-[84px] sm:top-[96px] overflow-auto">
      <div className="max-w-[1100px] mx-auto px-4 sm:px-8 md:px-10 pb-16 space-y-8">
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
  if (activityAt && Date.now() - activityAt < ACTIVITY_HOLD_MS && !job.needs_input) {
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
  const [zoom, setZoom] = useState(false)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { if (zoom) setZoom(false); else onClose() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, zoom])
  const job = jobsByTmuxRef.current.get(tmux)
  const ci = job ? ciStatus(job.last_check_conclusions ?? {}) : 'pending'
  const title = job?.issue_title || tmux
  return (
    <div
      className={`fixed inset-0 z-50 bg-black/40 backdrop-blur-sm ${zoom ? '' : 'flex items-center justify-center p-2 sm:p-6'}`}
      onClick={zoom ? undefined : onClose}
    >
      <div
        className={
          zoom
            ? 'absolute inset-0 overflow-hidden shadow-2xl flex flex-col bg-[#0b0b0e]'
            : 'relative w-full max-w-[1200px] h-[92vh] sm:h-[80vh] rounded-lg overflow-hidden shadow-2xl ring-1 ring-black/40 flex flex-col bg-[#0b0b0e]'
        }
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
            <button
              onClick={() => setZoom((z) => !z)}
              className="w-3 h-3 rounded-full bg-emerald-500 hover:bg-emerald-400 transition-colors"
              title={zoom ? 'restore' : 'fullscreen'}
            />
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
        <div className="flex-1 min-h-0">
          <Pane session={tmux} />
        </div>
      </div>
    </div>
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

function CardCompact({ job }: { job: Job }) {
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

