export interface UsageHistoryRow {
  date: string
  session_id: string
  model: string
  issue?: number
  input_tokens: number
  cache_creation: number
  cache_read: number
  output_tokens: number
  cost_usd: number
}

export interface PaneUsage {
  model?: string
  cost_usd?: number
  context_pct?: number
}

export interface Job {
  issue: number
  vm: string
  tmux: string
  target: string
  target_repo: string
  branch: string
  issue_title: string
  lifecycle: string
  schedule: string
  pr: number
  next_fire_at: string
  last_check_conclusions: Record<string, string>
  activity?: number[]
  needs_input?: boolean
  vm_online?: boolean
  usage?: PaneUsage
  closed_state?: string
  closed_at?: number
  priority?: number // governor priority; higher = more important, hidden when 0
  paused?: boolean // persisted duty-cycle pause flag (omitempty in the blob)
  paused_state?: boolean // explicit pause flag from /api/state, always present
  paused_at?: string
}

export interface VM {
  name: string
  host: string
  capacity: number
  used: number
  bot?: string
  agent?: string
  online?: boolean
  last_err?: string
}

export interface Throttle {
  mode: 'allow' | 'throttle' | 'pause_5h' | 'pause_week'
  reason?: string
  until?: number // unix secs; 0/absent unless a pause mode
  target_pct?: number // linear pace target for the 7d bar marker
  projected_exhaust_at?: number // unix secs ETA the 7d bucket hits 100%
}

export interface Quota {
  five_hour_pct: number
  five_hour_resets_at: number
  seven_day_pct: number
  seven_day_resets_at: number
  throttle?: Throttle
}

export interface ConnectStatus {
  github: { connected: boolean; login?: string }
}

export interface Governor {
  enabled: boolean
  effective_cap: number // -1 == uncapped
  active: number
  paused: number
  burn_weekly: number // %/h
  target_weekly: number // %/h
  burn_five: number // %/h
  target_five: number // %/h
  projected_end_pct: number
  binding: 'weekly' | '5h' | ''
}

export interface State {
  jobs: Job[]
  vms: VM[]
  inbox: string
  quota?: Quota
  connect?: ConnectStatus
  governor?: Governor
}
