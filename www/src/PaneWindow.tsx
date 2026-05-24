import { useEffect, useRef, useState } from 'react'
import type { Job } from './types'
import { Pane } from './Pane'
import { ciStatus } from './attention'

interface Props {
  tmux: string
  job?: Job
  x: number
  y: number
  w?: number
  h?: number
  onClose: () => void
  onMove: (x: number, y: number) => void
  onResize?: (w: number, h: number) => void
}

const MIN_W = 480
const MIN_H = 320

/// Detached floating "terminal window" rendered above the canvas. Multiple
/// can be open at once. Dragging the title bar repositions, dragging the
/// bottom-right corner resizes.
export function PaneWindow({ tmux, job, x, y, w = 720, h = 480, onClose, onMove, onResize }: Props) {
  const [pos, setPos] = useState({ x, y, w, h })
  const dragRef = useRef<{ kind: 'move' | 'resize'; sx: number; sy: number; bx: number; by: number; bw: number; bh: number } | null>(null)

  useEffect(() => { setPos({ x, y, w, h }) }, [x, y, w, h])

  const startDrag = (kind: 'move' | 'resize') => (e: React.PointerEvent) => {
    if (e.button !== 0) return
    e.preventDefault()
    ;(e.currentTarget as Element).setPointerCapture(e.pointerId)
    dragRef.current = {
      kind,
      sx: e.clientX, sy: e.clientY,
      bx: pos.x, by: pos.y, bw: pos.w, bh: pos.h,
    }
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const d = dragRef.current
    if (!d) return
    const dx = e.clientX - d.sx
    const dy = e.clientY - d.sy
    if (d.kind === 'move') {
      setPos((p) => ({ ...p, x: d.bx + dx, y: d.by + dy }))
    } else {
      setPos((p) => ({
        ...p,
        w: Math.max(MIN_W, d.bw + dx),
        h: Math.max(MIN_H, d.bh + dy),
      }))
    }
  }
  const onPointerUp = (e: React.PointerEvent) => {
    const d = dragRef.current
    if (!d) return
    ;(e.currentTarget as Element).releasePointerCapture(e.pointerId)
    dragRef.current = null
    if (d.kind === 'move') onMove(pos.x, pos.y)
    else onResize?.(pos.w, pos.h)
  }

  const ci = job ? ciStatus(job.last_check_conclusions ?? {}) : 'pending'
  const title = job?.issue_title || tmux

  return (
    <div
      className="fixed z-50 rounded-lg overflow-hidden shadow-2xl ring-1 ring-black/40 flex flex-col bg-[#0b0b0e]"
      style={{ left: pos.x, top: pos.y, width: pos.w, height: pos.h }}
    >
      <div
        className="h-8 bg-zinc-800/95 flex items-center px-3 gap-3 cursor-grab active:cursor-grabbing select-none flex-shrink-0"
        onPointerDown={startDrag('move')}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
      >
        <div className="flex gap-1.5 flex-shrink-0">
          <button
            onPointerDown={(e) => e.stopPropagation()}
            onClick={onClose}
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
      <div className="flex-1 min-h-0">
        <Pane session={tmux} />
      </div>
      <button
        onPointerDown={startDrag('resize')}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        className="absolute bottom-0 right-0 w-4 h-4 cursor-nwse-resize"
        style={{
          background: 'linear-gradient(135deg, transparent 50%, rgba(161,161,170,0.5) 50%)',
        }}
        title="resize"
      />
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
    // GitHub closed-PR icon (octicon git-pull-request-closed)
    return (
      <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
        <path d="M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 14a2.25 2.25 0 1 1 0-4.5 2.25 2.25 0 0 1 0 4.5ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm-2.03-9.53a.75.75 0 0 1 1.06 0L13 3.69l1.22-1.22a.75.75 0 1 1 1.06 1.06L14.06 4.75l1.22 1.22a.75.75 0 1 1-1.06 1.06L13 5.81l-1.22 1.22a.75.75 0 1 1-1.06-1.06l1.22-1.22-1.22-1.22a.75.75 0 0 1 0-1.06Z"/>
      </svg>
    )
  }
  // Open / pending — octicon git-pull-request
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="currentColor">
      <path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z"/>
    </svg>
  )
}
