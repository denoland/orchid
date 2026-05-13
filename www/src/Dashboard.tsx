import type { State, Job, VM } from './types'

interface Props {
  state: State
  tick: number
}

function ciStatus(conclusions: Record<string, string>): 'fail' | 'pass' | 'pending' {
  const vals = Object.values(conclusions ?? {})
  if (vals.length === 0) return 'pending'
  if (vals.some((v) => v === 'FAILURE' || v === 'failure' || v === 'FAILED' || v === 'failed'))
    return 'fail'
  if (vals.every((v) => v === 'SUCCESS' || v === 'success' || v === 'COMPLETED'))
    return 'pass'
  return 'pending'
}

function StatusDot({ job }: { job: Job | null }) {
  if (!job) return <span className="w-[5px] h-[5px] rounded-full bg-[#e5e5e5] inline-block" />
  if (job.lifecycle === 'cron') {
    const active = job.tmux !== ''
    return (
      <span
        className={`w-[5px] h-[5px] rounded-full inline-block ${active ? 'bg-[#a855f7]' : 'bg-[#d4d4d4]'}`}
      />
    )
  }
  return <span className="w-[5px] h-[5px] rounded-full bg-[#22c55e] inline-block" />
}

function CIDot({ conclusions }: { conclusions: Record<string, string> }) {
  const s = ciStatus(conclusions)
  if (s === 'fail') return <span className="w-[5px] h-[5px] rounded-full bg-[#ef4444] inline-block ml-1.5" title="CI failing" />
  if (s === 'pass') return <span className="w-[5px] h-[5px] rounded-full bg-[#22c55e] inline-block ml-1.5" title="CI passing" />
  return null
}

function JobRow({ job, onClick }: { job: Job; onClick: () => void }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  return (
    <tr
      className="border-b border-[#f5f5f5] cursor-pointer hover:bg-[#f9f9f9] transition-colors"
      onClick={onClick}
    >
      <td className="px-3 sm:px-[14px] py-[9px] align-middle">
        <span className="flex items-center gap-2 min-w-0">
          <StatusDot job={job} />
          <span className="flex flex-col min-w-0">
            <span className="text-[12px] text-[#171717] truncate leading-tight">
              {job.issue_title || job.tmux || '—'}
            </span>
            {job.issue_title && job.tmux && (
              <code className="text-[10px] text-[#a3a3a3] leading-tight">{job.tmux}</code>
            )}
          </span>
        </span>
      </td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[13px] text-[#404040]">
        {job.issue ? (
          <a
            href={`https://github.com/denoland/orchid/issues/${job.issue}`}
            target="_blank"
            rel="noopener noreferrer"
            className="hover:underline"
            onClick={(e) => e.stopPropagation()}
          >
            #{job.issue}
          </a>
        ) : '—'}
      </td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[13px] text-[#404040]">
        {job.target_repo ? (
          <a
            href={`https://github.com/${job.target_repo}`}
            target="_blank"
            rel="noopener noreferrer"
            className="hover:underline text-[#737373]"
            onClick={(e) => e.stopPropagation()}
          >
            {repo}
          </a>
        ) : '—'}
      </td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[13px] text-[#404040]">
        {job.pr ? (
          <span className="flex items-center">
            <a
              href={`https://github.com/${job.target_repo}/pull/${job.pr}`}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              #{job.pr}
            </a>
            <CIDot conclusions={job.last_check_conclusions ?? {}} />
          </span>
        ) : '—'}
      </td>
    </tr>
  )
}

function FreeRow() {
  return (
    <tr className="border-b border-[#f5f5f5]">
      <td className="px-3 sm:px-[14px] py-[9px] align-middle">
        <span className="flex items-center gap-2">
          <StatusDot job={null} />
          <span className="text-[12px] text-[#d4d4d4]">free</span>
        </span>
      </td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[#d4d4d4] text-[13px]">—</td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[#d4d4d4] text-[13px]">—</td>
      <td className="px-3 sm:px-[14px] py-[9px] align-middle text-[#d4d4d4] text-[13px]">—</td>
    </tr>
  )
}

export function Dashboard({ state, tick }: Props) {
  const { jobs = [], vms = [], inbox = '' } = state

  const rows: Array<{ type: 'job'; job: Job } | { type: 'free' }> = []

  for (const vm of vms) {
    const vmJobs = jobs.filter((j) => j.vm === vm.name)
    for (const job of vmJobs) rows.push({ type: 'job', job })
    const free = vm.capacity - vmJobs.length
    for (let i = 0; i < free; i++) rows.push({ type: 'free' })
  }

  const knownVMs = new Set(vms.map((v) => v.name))
  for (const job of jobs) {
    if (!knownVMs.has(job.vm)) rows.push({ type: 'job', job })
  }

  if (vms.length === 0) {
    for (const job of jobs) rows.push({ type: 'job', job })
  }

  const busy = jobs.length
  const cap = vms.reduce((s, v) => s + v.capacity, 0)

  return (
    <div className="flex flex-col min-h-screen bg-white">
      <header className="border-b border-[#f5f5f5] px-[14px] sm:px-6 h-10 flex items-center justify-between flex-shrink-0">
        <a href="#/" className="font-mono text-[14px] font-semibold text-[#171717] hover:opacity-60 transition-opacity">
          orchid
        </a>
        <div className="flex items-center gap-4 text-[12px] text-[#a3a3a3]">
          {cap > 0 && <span>{busy}/{cap}</span>}
          {inbox && (
            <a
              href={`https://github.com/${inbox}/issues`}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:text-[#404040] transition-colors"
            >
              {inbox}
            </a>
          )}
          <span>↻ {tick}s</span>
        </div>
      </header>

      <main className="flex-1 mx-auto w-full max-w-[860px] px-4 sm:px-6 py-6">
        <div className="border border-[#f0f0f0] rounded overflow-hidden">
          <table className="w-full table-fixed border-collapse text-[13px]">
            <thead>
              <tr className="border-b border-[#f5f5f5]">
                <th className="px-3 sm:px-[14px] py-[9px] text-left text-[10px] uppercase tracking-[.12em] text-[#a3a3a3] font-medium w-[38%]">
                  Session
                </th>
                <th className="px-3 sm:px-[14px] py-[9px] text-left text-[10px] uppercase tracking-[.12em] text-[#a3a3a3] font-medium w-[15%]">
                  Issue
                </th>
                <th className="px-3 sm:px-[14px] py-[9px] text-left text-[10px] uppercase tracking-[.12em] text-[#a3a3a3] font-medium w-[27%]">
                  Repo
                </th>
                <th className="px-3 sm:px-[14px] py-[9px] text-left text-[10px] uppercase tracking-[.12em] text-[#a3a3a3] font-medium w-[20%]">
                  PR
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <td colSpan={4} className="px-[14px] py-8 text-center text-[#a3a3a3] text-[13px]">
                    no sessions
                  </td>
                </tr>
              ) : (
                rows.map((row, i) =>
                  row.type === 'job' ? (
                    <JobRow
                      key={`job-${row.job.issue}-${row.job.tmux}-${i}`}
                      job={row.job}
                      onClick={() => {
                        if (row.job.tmux) window.location.hash = `/pane/${encodeURIComponent(row.job.tmux)}`
                      }}
                    />
                  ) : (
                    <FreeRow key={`free-${i}`} />
                  ),
                )
              )}
            </tbody>
          </table>
        </div>
      </main>
    </div>
  )
}
