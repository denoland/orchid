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
}

export interface VM {
  name: string
  host: string
  capacity: number
  used: number
}

export interface State {
  jobs: Job[]
  vms: VM[]
  inbox: string
  operator: string
}
