import { useMemo } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  type Node,
  type Edge,
  Handle,
  Position,
  MarkerType,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'

// Hand-laid node graphs that ship as live components in the docs SPA and
// as PNGs (snap-diagrams.mjs) for raw-markdown viewers. Edges use simple
// straight or step lines — no smoothstep — so arrows don't wander.

type Tone = 'ink' | 'purple' | 'rose' | 'emerald' | 'amber' | 'muted'

interface BoxData {
  label: string
  sub?: string
  tone?: Tone
  shape?: 'rect' | 'pill'
}

const toneStyles: Record<Tone, { bg: string; fg: string; ring: string }> = {
  ink:     { bg: '#18181b', fg: '#fafaf9', ring: '#18181b' },
  purple:  { bg: '#7c3aed', fg: '#fafaf9', ring: '#7c3aed' },
  rose:    { bg: '#fff1f2', fg: '#9f1239', ring: '#fda4af' },
  emerald: { bg: '#ecfdf5', fg: '#065f46', ring: '#86efac' },
  amber:   { bg: '#fffbeb', fg: '#92400e', ring: '#fcd34d' },
  muted:   { bg: '#fff',    fg: '#27272a', ring: '#e4e4e7' },
}

function Box({ data }: { data: BoxData }) {
  const t = toneStyles[data.tone ?? 'muted']
  const rounded = data.shape === 'pill' ? '999px' : '10px'
  return (
    <div
      style={{
        background: t.bg, color: t.fg,
        border: `1px solid ${t.ring}`,
        borderRadius: rounded,
        padding: data.sub ? '10px 14px' : '8px 14px',
        fontSize: 13,
        fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
        minWidth: 130, textAlign: 'center',
        boxShadow: data.tone === 'muted' ? '0 1px 2px rgba(0,0,0,.04)' : 'none',
      }}
    >
      <div style={{ fontWeight: 600 }}>{data.label}</div>
      {data.sub && (
        <div style={{ fontSize: 11, opacity: .75, marginTop: 2 }}>{data.sub}</div>
      )}
      {/* Handles on all four sides so edges pick the shortest path. */}
      <Handle type="target" position={Position.Left}   id="l" style={{ opacity: 0, width: 1, height: 1 }} />
      <Handle type="source" position={Position.Right}  id="r" style={{ opacity: 0, width: 1, height: 1 }} />
      <Handle type="target" position={Position.Top}    id="t" style={{ opacity: 0, width: 1, height: 1 }} />
      <Handle type="source" position={Position.Bottom} id="b" style={{ opacity: 0, width: 1, height: 1 }} />
    </div>
  )
}

const nodeTypes = { box: Box }

interface DiagramProps {
  nodes: Node<BoxData>[]
  edges: Edge[]
  height?: number
}

function Diagram({ nodes, edges, height = 280 }: DiagramProps) {
  const ns = useMemo(() => nodes.map((n) => ({ ...n, type: 'box' as const })), [nodes])
  const es = useMemo(() => edges.map((e) => ({
    type: 'straight',
    animated: false,
    ...e,
    style: { stroke: '#7c3aed', strokeWidth: 1.6, ...(e.style || {}) },
    labelStyle: { fontSize: 11, fill: '#52525b', ...(e.labelStyle || {}) },
    labelBgStyle: { fill: '#fafaf9', ...(e.labelBgStyle || {}) },
    labelBgPadding: [6, 3],
    labelBgBorderRadius: 4,
    markerEnd: e.markerEnd ?? { type: MarkerType.ArrowClosed, color: '#7c3aed', width: 18, height: 18 },
  })), [edges])
  return (
    <div className="docs-diagram" style={{ height, margin: '20px 0', border: '1px solid #e4e4e7', borderRadius: 12, background: '#fafaf9' }}>
      <ReactFlowProvider>
        <ReactFlow
          nodes={ns}
          edges={es}
          nodeTypes={nodeTypes}
          fitView
          fitViewOptions={{ padding: 0.18 }}
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable={false}
          panOnDrag={false}
          zoomOnScroll={false}
          zoomOnPinch={false}
          zoomOnDoubleClick={false}
          preventScrolling={false}
          proOptions={{ hideAttribution: true }}
        >
          <Background gap={16} size={1} color="#e4e4e7" />
        </ReactFlow>
      </ReactFlowProvider>
    </div>
  )
}

// ─── architecture: user → relay → orch → VMs (horizontal chain) ───
const archNodes: Node<BoxData>[] = [
  { id: 'you',   position: { x:    0, y:  80 }, data: { label: 'You',        sub: 'phone · laptop', tone: 'muted' } },
  { id: 'relay', position: { x:  220, y:  80 }, data: { label: 'CF Worker',  sub: 'orchid.littledivy.com', tone: 'purple', shape: 'pill' } },
  { id: 'orch',  position: { x:  500, y:  80 }, data: { label: 'orch',       sub: 'your machine',   tone: 'ink' } },
  { id: 'vm1',   position: { x:  760, y:   0 }, data: { label: 'vm "local"', sub: 'tmux + claude×N', tone: 'emerald' } },
  { id: 'vm2',   position: { x:  760, y: 160 }, data: { label: 'vm "fra1"',  sub: 'tmux + claude×N', tone: 'emerald' } },
]
const archEdges: Edge[] = [
  { id: 'e1', source: 'you',   target: 'relay', sourceHandle: 'r', targetHandle: 'l', label: 'browser' },
  { id: 'e2', source: 'relay', target: 'orch',  sourceHandle: 'r', targetHandle: 'l', label: 'wss agent tunnel' },
  { id: 'e3', source: 'orch',  target: 'vm1',   sourceHandle: 'r', targetHandle: 'l', label: 'ssh' },
  { id: 'e4', source: 'orch',  target: 'vm2',   sourceHandle: 'r', targetHandle: 'l', label: 'ssh' },
]
export function ArchitectureDiagram() {
  return <Diagram nodes={archNodes} edges={archEdges} height={320} />
}

// ─── issue lifecycle: linear top row + "Ignored" branch below the gate ───
const issueNodes: Node<BoxData>[] = [
  { id: 'open',   position: { x:    0, y:  60 }, data: { label: 'Issue opened', sub: 'inbox repo', tone: 'muted' } },
  { id: 'route',  position: { x:  220, y:  60 }, data: { label: 'Label match?', tone: 'amber', shape: 'pill' } },
  { id: 'spawn',  position: { x:  430, y:  60 }, data: { label: 'tmux + claude', sub: 'clone target', tone: 'ink' } },
  { id: 'pr',     position: { x:  660, y:  60 }, data: { label: 'PR opened',    sub: 'in target repo', tone: 'purple' } },
  { id: 'merge',  position: { x:  890, y:  60 }, data: { label: 'Merge',        sub: 'orch tears down', tone: 'emerald' } },
  { id: 'ignore', position: { x:  220, y: 220 }, data: { label: 'Ignored',      sub: 'no matching label', tone: 'rose' } },
]
const issueEdges: Edge[] = [
  { id: 'e1', source: 'open',  target: 'route',  sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e2', source: 'route', target: 'spawn',  sourceHandle: 'r', targetHandle: 'l', label: 'yes' },
  { id: 'e3', source: 'spawn', target: 'pr',     sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e4', source: 'pr',    target: 'merge',  sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e5', source: 'route', target: 'ignore', sourceHandle: 'b', targetHandle: 't', label: 'no',
    style: { stroke: '#9f1239' }, markerEnd: { type: MarkerType.ArrowClosed, color: '#9f1239', width: 18, height: 18 } },
]
export function IssueLifecycleDiagram() {
  return <Diagram nodes={issueNodes} edges={issueEdges} height={320} />
}

// ─── VM join: linear 4 steps (clean horizontal flow) ───
const vmJoinNodes: Node<BoxData>[] = [
  { id: 'central', position: { x:   0, y:  80 }, data: { label: 'Central orch', sub: 'issues invite token', tone: 'ink' } },
  { id: 'newvm',   position: { x: 260, y:  80 }, data: { label: 'New VM',       sub: 'runs WORKER=1 installer', tone: 'muted' } },
  { id: 'key',     position: { x: 520, y:  80 }, data: { label: 'Generate SSH key', sub: 'POST /api/vm/join', tone: 'purple', shape: 'pill' } },
  { id: 'merged',  position: { x: 780, y:  80 }, data: { label: 'Pubkey accepted',  sub: 'swarm.hcl gets a new vm{}', tone: 'emerald' } },
]
const vmJoinEdges: Edge[] = [
  { id: 'e1', source: 'central', target: 'newvm',  sourceHandle: 'r', targetHandle: 'l', label: '1. invite token' },
  { id: 'e2', source: 'newvm',   target: 'key',    sourceHandle: 'r', targetHandle: 'l', label: '2. orch join vm' },
  { id: 'e3', source: 'key',     target: 'merged', sourceHandle: 'r', targetHandle: 'l', label: '3. ssh ready' },
]
export function VMJoinDiagram() {
  return <Diagram nodes={vmJoinNodes} edges={vmJoinEdges} height={240} />
}

// ─── supervision: phone → agent → (orch, GitHub) fan-out from the right ───
const supNodes: Node<BoxData>[] = [
  { id: 'phone',  position: { x:   0, y: 100 }, data: { label: 'Telegram', sub: '"check orchid"', tone: 'muted' } },
  { id: 'agent',  position: { x: 260, y: 100 }, data: { label: 'OpenClaw / Hermes', sub: 'reads SKILL.md', tone: 'purple', shape: 'pill' } },
  { id: 'orch',   position: { x: 560, y:   0 }, data: { label: 'orch host', sub: 'ssh + tmux', tone: 'ink' } },
  { id: 'gh',     position: { x: 560, y: 200 }, data: { label: 'GitHub',    sub: 'gh issue / pr', tone: 'emerald' } },
]
const supEdges: Edge[] = [
  { id: 'e1', source: 'phone', target: 'agent', sourceHandle: 'r', targetHandle: 'l', label: 'chat' },
  { id: 'e2', source: 'agent', target: 'orch',  sourceHandle: 'r', targetHandle: 'l', label: 'ssh' },
  { id: 'e3', source: 'agent', target: 'gh',    sourceHandle: 'r', targetHandle: 'l', label: 'gh CLI' },
]
export function SupervisionDiagram() {
  return <Diagram nodes={supNodes} edges={supEdges} height={340} />
}

// ─── capture: 3 client devices → /api/drafts → gh issue ───
const capNodes: Node<BoxData>[] = [
  { id: 'mac',    position: { x:   0, y:   0 }, data: { label: 'macOS app',   sub: 'menu-bar composer',  tone: 'muted' } },
  { id: 'ios',    position: { x:   0, y: 110 }, data: { label: 'iOS app',     sub: 'voice + share ext',  tone: 'muted' } },
  { id: 'watch',  position: { x:   0, y: 220 }, data: { label: 'watchOS',     sub: 'hold to capture',    tone: 'muted' } },
  { id: 'drafts', position: { x: 320, y: 110 }, data: { label: '/api/drafts', sub: 'orch on your host',  tone: 'purple', shape: 'pill' } },
  { id: 'gh',     position: { x: 620, y: 110 }, data: { label: 'gh issue',    sub: 'in inbox repo',      tone: 'emerald' } },
]
const capEdges: Edge[] = [
  { id: 'e1', source: 'mac',    target: 'drafts', sourceHandle: 'r', targetHandle: 'l', label: 'POST' },
  { id: 'e2', source: 'ios',    target: 'drafts', sourceHandle: 'r', targetHandle: 'l', label: 'POST' },
  { id: 'e3', source: 'watch',  target: 'drafts', sourceHandle: 'r', targetHandle: 'l', label: 'POST' },
  { id: 'e4', source: 'drafts', target: 'gh',     sourceHandle: 'r', targetHandle: 'l', label: 'create issue' },
]
export function CaptureDiagram() {
  return <Diagram nodes={capNodes} edges={capEdges} height={360} />
}

// ─── signup journey: sign in → install → join → first issue ───
const journeyNodes: Node<BoxData>[] = [
  { id: 'signin', position: { x:    0, y:  60 }, data: { label: 'Sign in',      sub: 'GitHub OAuth', tone: 'muted' } },
  { id: 'install',position: { x:  220, y:  60 }, data: { label: 'install.sh',   sub: 'on your machine', tone: 'ink' } },
  { id: 'join',   position: { x:  450, y:  60 }, data: { label: 'orch join',    sub: 'paste agent token', tone: 'purple', shape: 'pill' } },
  { id: 'issue',  position: { x:  690, y:  60 }, data: { label: 'File issue',   sub: 'labeled with target', tone: 'amber' } },
  { id: 'pr',     position: { x:  920, y:  60 }, data: { label: 'PR appears',   sub: 'on your dashboard', tone: 'emerald' } },
]
const journeyEdges: Edge[] = [
  { id: 'e1', source: 'signin', target: 'install', sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e2', source: 'install',target: 'join',    sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e3', source: 'join',   target: 'issue',   sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e4', source: 'issue',  target: 'pr',      sourceHandle: 'r', targetHandle: 'l' },
]
export function SignupJourneyDiagram() {
  return <Diagram nodes={journeyNodes} edges={journeyEdges} height={220} />
}

// ─── card states (dashboard) — clean state-machine row ───
const stateNodes: Node<BoxData>[] = [
  { id: 'spawn',  position: { x:    0, y:  60 }, data: { label: 'Spawning', sub: 'tmux booting', tone: 'muted' } },
  { id: 'work',   position: { x:  200, y:  60 }, data: { label: 'Working',  sub: 'claude busy',   tone: 'amber' } },
  { id: 'pr',     position: { x:  400, y:  60 }, data: { label: 'PR open',  sub: 'CI running',    tone: 'purple' } },
  { id: 'needs',  position: { x:  600, y:  60 }, data: { label: 'Needs you', sub: 'permission dialog', tone: 'rose' } },
  { id: 'quiet',  position: { x:  600, y: 180 }, data: { label: 'Quiet',    sub: 'awaiting review', tone: 'muted' } },
  { id: 'merge',  position: { x:  830, y: 120 }, data: { label: 'Merged',   sub: 'session ends',  tone: 'emerald' } },
]
const stateEdges: Edge[] = [
  { id: 'e1', source: 'spawn', target: 'work',  sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e2', source: 'work',  target: 'pr',    sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e3', source: 'pr',    target: 'needs', sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e4', source: 'pr',    target: 'quiet', sourceHandle: 'b', targetHandle: 't' },
  { id: 'e5', source: 'needs', target: 'merge', sourceHandle: 'r', targetHandle: 'l' },
  { id: 'e6', source: 'quiet', target: 'merge', sourceHandle: 'r', targetHandle: 'l' },
]
export function CardStatesDiagram() {
  return <Diagram nodes={stateNodes} edges={stateEdges} height={300} />
}

// ─── registry: marker name → component ───
export const DIAGRAMS: Record<string, React.FC> = {
  architecture: ArchitectureDiagram,
  issue:        IssueLifecycleDiagram,
  'vm-join':    VMJoinDiagram,
  supervision:  SupervisionDiagram,
  capture:      CaptureDiagram,
  journey:      SignupJourneyDiagram,
  'card-states': CardStatesDiagram,
}
