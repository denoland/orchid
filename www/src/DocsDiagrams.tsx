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

// ─── styled node primitives ───
// Tiny pill / box nodes in the orchid palette. Edges between them use the
// dashboard's accent purple so diagrams feel like the dashboard, not docs.

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
        minWidth: 120, textAlign: 'center',
        boxShadow: data.tone === 'muted' ? '0 1px 2px rgba(0,0,0,.04)' : 'none',
      }}
    >
      <div style={{ fontWeight: 600 }}>{data.label}</div>
      {data.sub && (
        <div style={{ fontSize: 11, opacity: .75, marginTop: 2 }}>{data.sub}</div>
      )}
      <Handle type="target" position={Position.Left}  style={{ opacity: 0, width: 1, height: 1 }} />
      <Handle type="source" position={Position.Right} style={{ opacity: 0, width: 1, height: 1 }} />
      <Handle type="target" position={Position.Top}    style={{ opacity: 0, width: 1, height: 1 }} id="t" />
      <Handle type="source" position={Position.Bottom} style={{ opacity: 0, width: 1, height: 1 }} id="b" />
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
    ...e,
    type: 'smoothstep',
    animated: e.animated ?? true,
    style: { stroke: '#7c3aed', strokeWidth: 1.6, ...(e.style || {}) },
    markerEnd: e.markerEnd ?? { type: MarkerType.ArrowClosed, color: '#7c3aed' },
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

// ─── architecture: user → relay → orch → VMs ───
const archNodes: Node<BoxData>[] = [
  { id: 'you',    position: { x:   0, y:  60 }, data: { label: 'You',         sub: 'phone · laptop',  tone: 'muted' } },
  { id: 'relay',  position: { x: 200, y:  60 }, data: { label: 'CF Worker',   sub: 'orchid.littledivy.com', tone: 'purple', shape: 'pill' } },
  { id: 'orch',   position: { x: 440, y:  60 }, data: { label: 'orch',        sub: 'your machine',   tone: 'ink' } },
  { id: 'vm1',    position: { x: 660, y:   0 }, data: { label: 'vm "local"',  sub: 'tmux + claude×N',tone: 'emerald' } },
  { id: 'vm2',    position: { x: 660, y: 120 }, data: { label: 'vm "fra1"',   sub: 'tmux + claude×N',tone: 'emerald' } },
]
const archEdges: Edge[] = [
  { id: 'e1', source: 'you',   target: 'relay', label: 'browser' },
  { id: 'e2', source: 'relay', target: 'orch',  label: 'wss agent tunnel' },
  { id: 'e3', source: 'orch',  target: 'vm1',   label: 'ssh' },
  { id: 'e4', source: 'orch',  target: 'vm2',   label: 'ssh' },
]
export function ArchitectureDiagram() {
  return <Diagram nodes={archNodes} edges={archEdges} height={300} />
}

// ─── issue lifecycle ───
const issueNodes: Node<BoxData>[] = [
  { id: 'open',   position: { x:   0, y: 70 }, data: { label: 'Issue opened', sub: 'inbox repo', tone: 'muted' } },
  { id: 'route',  position: { x: 200, y: 70 }, data: { label: 'Label match?', tone: 'amber', shape: 'pill' } },
  { id: 'spawn',  position: { x: 400, y: 70 }, data: { label: 'tmux + claude', sub: 'clone target', tone: 'ink' } },
  { id: 'pr',     position: { x: 600, y: 70 }, data: { label: 'PR opened',  sub: 'in target repo', tone: 'purple' } },
  { id: 'merge',  position: { x: 800, y: 70 }, data: { label: 'Merge',      sub: 'orch tears down', tone: 'emerald' } },
  { id: 'ignore', position: { x: 200, y: 200 }, data: { label: 'Ignored',   sub: 'no matching label', tone: 'rose' } },
]
const issueEdges: Edge[] = [
  { id: 'e1', source: 'open',  target: 'route' },
  { id: 'e2', source: 'route', target: 'spawn', label: 'yes' },
  { id: 'e3', source: 'route', target: 'ignore', label: 'no', style: { stroke: '#9f1239' }, markerEnd: { type: MarkerType.ArrowClosed, color: '#9f1239' } },
  { id: 'e4', source: 'spawn', target: 'pr' },
  { id: 'e5', source: 'pr',    target: 'merge' },
]
export function IssueLifecycleDiagram() {
  return <Diagram nodes={issueNodes} edges={issueEdges} height={320} />
}

// ─── VM join handshake ───
const vmJoinNodes: Node<BoxData>[] = [
  { id: 'central', position: { x:   0, y:  80 }, data: { label: 'Central orch', sub: 'has invite token', tone: 'ink' } },
  { id: 'newvm',   position: { x: 380, y:  80 }, data: { label: 'New VM',       sub: 'fresh Linux box',  tone: 'muted' } },
  { id: 'pubkey',  position: { x: 380, y: 220 }, data: { label: 'Generates SSH key', tone: 'purple', shape: 'pill' } },
  { id: 'auth',    position: { x:   0, y: 220 }, data: { label: 'authorized_keys += pubkey', sub: 'vm {} block written', tone: 'emerald' } },
]
const vmJoinEdges: Edge[] = [
  { id: 'e1', source: 'central', target: 'newvm',   label: '1. invite token', sourceHandle: 'b' },
  { id: 'e2', source: 'newvm',   target: 'pubkey',  label: '2. orch join vm', sourceHandle: 'b', targetHandle: 't' },
  { id: 'e3', source: 'pubkey',  target: 'auth',    label: '3. POST /api/vm/join' },
  { id: 'e4', source: 'auth',    target: 'central', label: '4. dispatch ready', targetHandle: 't' },
]
export function VMJoinDiagram() {
  return <Diagram nodes={vmJoinNodes} edges={vmJoinEdges} height={360} />
}

// ─── supervision flow ───
const supNodes: Node<BoxData>[] = [
  { id: 'phone',   position: { x:   0, y:  80 }, data: { label: 'Telegram',  sub: '"check orchid"', tone: 'muted' } },
  { id: 'agent',   position: { x: 240, y:  80 }, data: { label: 'OpenClaw / Hermes', sub: 'reads SKILL.md', tone: 'purple', shape: 'pill' } },
  { id: 'orch',    position: { x: 500, y:   0 }, data: { label: 'orch host',  sub: 'ssh + tmux', tone: 'ink' } },
  { id: 'github',  position: { x: 500, y: 160 }, data: { label: 'GitHub',     sub: 'gh issue / pr', tone: 'emerald' } },
]
const supEdges: Edge[] = [
  { id: 'e1', source: 'phone', target: 'agent', label: 'chat' },
  { id: 'e2', source: 'agent', target: 'orch', label: 'ssh', sourceHandle: 'b', targetHandle: 't' },
  { id: 'e3', source: 'agent', target: 'github', label: 'gh CLI', sourceHandle: 'b', targetHandle: 't' },
]
export function SupervisionDiagram() {
  return <Diagram nodes={supNodes} edges={supEdges} height={340} />
}

// ─── capture pipeline ───
const capNodes: Node<BoxData>[] = [
  { id: 'mac',    position: { x:   0, y:  20 }, data: { label: 'macOS app',   sub: 'menu-bar composer',  tone: 'muted' } },
  { id: 'ios',    position: { x:   0, y: 110 }, data: { label: 'iOS app',     sub: 'voice + share ext',  tone: 'muted' } },
  { id: 'watch',  position: { x:   0, y: 200 }, data: { label: 'watchOS',     sub: 'hold to capture',    tone: 'muted' } },
  { id: 'drafts', position: { x: 320, y: 110 }, data: { label: '/api/drafts', sub: 'orch on your host',  tone: 'purple', shape: 'pill' } },
  { id: 'gh',     position: { x: 600, y: 110 }, data: { label: 'gh issue',    sub: 'in inbox repo',      tone: 'emerald' } },
]
const capEdges: Edge[] = [
  { id: 'e1', source: 'mac',    target: 'drafts', label: 'POST json' },
  { id: 'e2', source: 'ios',    target: 'drafts', label: 'POST json' },
  { id: 'e3', source: 'watch',  target: 'drafts', label: 'POST json' },
  { id: 'e4', source: 'drafts', target: 'gh',     label: 'create labeled issue' },
]
export function CaptureDiagram() {
  return <Diagram nodes={capNodes} edges={capEdges} height={340} />
}

// ─── registry: marker name → component ───
export const DIAGRAMS: Record<string, React.FC> = {
  architecture: ArchitectureDiagram,
  issue:        IssueLifecycleDiagram,
  'vm-join':    VMJoinDiagram,
  supervision:  SupervisionDiagram,
  capture:      CaptureDiagram,
}
