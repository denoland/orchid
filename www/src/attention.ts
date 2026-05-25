import type { Job } from './types'

export type AttentionLevel = 'needs-you' | 'watching' | 'working' | 'quiet'

export interface Attention {
  level: AttentionLevel
  reason: string
  score: number
}

export function ciStatus(conclusions: Record<string, string>): 'fail' | 'pass' | 'pending' {
  const vals = Object.values(conclusions ?? {})
  if (vals.length === 0) return 'pending'
  if (vals.some((v) => /fail/i.test(v))) return 'fail'
  if (vals.every((v) => /success|completed/i.test(v))) return 'pass'
  return 'pending'
}

/// Heuristics from data already in state.json. Higher score = redder bar.
export function attention(job: Job): Attention {
  const ci = ciStatus(job.last_check_conclusions ?? {})
  const activity = job.activity ?? []
  const recent = activity.slice(-10).reduce((a, b) => a + b, 0)
  const overall = activity.reduce((a, b) => a + b, 0)
  const hasPR = job.pr > 0
  const hasTmux = job.tmux !== ''

  // Positive signal from the pane sampler: the agent is showing a modal
  // dialog (Yes/No, plan approval, …) and is blocked on a human. Outranks
  // CI-failing because the dialog is right now, on screen, waiting.
  if (hasTmux && job.needs_input) {
    return { level: 'needs-you', reason: 'awaiting your answer', score: 110 }
  }
  if (hasPR && ci === 'fail') {
    return { level: 'needs-you', reason: 'CI failing', score: 100 }
  }
  if (hasTmux && overall > 0 && recent === 0 && !hasPR) {
    return { level: 'needs-you', reason: 'idle — likely awaiting prompt', score: 90 }
  }
  if (hasPR && ci === 'pass' && recent === 0) {
    return { level: 'watching', reason: 'PR ready — review needed', score: 70 }
  }
  if (hasPR && ci === 'pending') {
    return { level: 'watching', reason: 'PR open — CI pending', score: 50 }
  }
  if (hasTmux && recent > 0) {
    return { level: 'working', reason: 'active', score: 30 }
  }
  if (hasTmux) {
    return { level: 'quiet', reason: 'no recent activity', score: 20 }
  }
  return { level: 'quiet', reason: '', score: 0 }
}

export const LEVEL_COLOR: Record<AttentionLevel, { bar: string }> = {
  'needs-you': { bar: 'bg-rose-500' },
  'watching':  { bar: 'bg-amber-500' },
  'working':   { bar: 'bg-emerald-500' },
  'quiet':     { bar: 'bg-zinc-300' },
}
