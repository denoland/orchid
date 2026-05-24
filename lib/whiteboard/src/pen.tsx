import { useRef, useState } from 'react'
import { useViewport } from '@xyflow/react'
import type { Stroke } from './types'

/// Overlay that captures free-hand strokes anywhere on the React Flow
/// surface. Emits a finished `Stroke` (with a path string + bounding
/// box) once the pointer is released — the caller decides what to do
/// with it (file as a node, send over WS, etc.).
export function PenLayer({
  active,
  containerRef,
  onStroke,
  newId,
  width = 1.8,
}: {
  active: boolean
  containerRef: React.RefObject<HTMLDivElement | null>
  onStroke: (s: Stroke) => void
  newId: () => string
  width?: number
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
      const xs = current.map((p) => p.x), ys = current.map((p) => p.y)
      const minX = Math.min(...xs), minY = Math.min(...ys)
      const maxX = Math.max(...xs), maxY = Math.max(...ys)
      const local = current.map((p) => ({ x: p.x - minX, y: p.y - minY }))
      onStroke({ id: newId(), x: minX, y: minY, w: maxX - minX, h: maxY - minY, d: pointsToPath(local), width })
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
          <path d={pointsToPath(current)} strokeWidth={width} fill="none" strokeLinejoin="round" strokeLinecap="round" style={{ stroke: 'var(--ink)' }} />
        )}
      </g>
    </svg>
  )
}

export function pointsToPath(points: { x: number; y: number }[]): string {
  if (points.length === 0) return ''
  let d = `M ${points[0].x.toFixed(1)} ${points[0].y.toFixed(1)}`
  for (let i = 1; i < points.length; i++) d += ` L ${points[i].x.toFixed(1)} ${points[i].y.toFixed(1)}`
  return d
}
