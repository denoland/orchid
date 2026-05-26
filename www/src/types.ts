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

export interface Quota {
  five_hour_pct: number
  five_hour_resets_at: number
  seven_day_pct: number
  seven_day_resets_at: number
}

export interface State {
  jobs: Job[]
  vms: VM[]
  inbox: string
  quota?: Quota
}
