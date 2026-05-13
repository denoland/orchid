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

function ActivityDot({ job }: { job: Job | null }) {
  if (!job) {
    return (
      <span className="relative inline-flex w-2 h-2">
        <span className="w-2 h-2 rounded-full bg-[#e5e5e5]" />
      </span>
    )
  }
  if (job.lifecycle === 'cron') {
    const active = job.tmux !== ''
    return (
      <span className="relative inline-flex w-2 h-2">
        {active && (
          <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-[#a855f7] opacity-50" />
        )}
        <span className={`relative w-2 h-2 rounded-full ${active ? 'bg-[#a855f7]' : 'bg-[#d4d4d4]'}`} />
      </span>
    )
  }
  return (
    <span className="relative inline-flex w-2 h-2">
      <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-[#22c55e] opacity-40" />
      <span className="relative w-2 h-2 rounded-full bg-[#22c55e]" />
    </span>
  )
}

function CIBadge({ conclusions }: { conclusions: Record<string, string> }) {
  const s = ciStatus(conclusions)
  if (s === 'fail') return (
    <span className="inline-flex items-center gap-1 text-[10px] font-medium text-[#dc2626] bg-[#fef2f2] border border-[#fecaca] rounded px-1.5 py-0.5">
      CI fail
    </span>
  )
  if (s === 'pass') return (
    <span className="inline-flex items-center gap-1 text-[10px] font-medium text-[#16a34a] bg-[#f0fdf4] border border-[#bbf7d0] rounded px-1.5 py-0.5">
      CI pass
    </span>
  )
  return null
}

function JobRow({ job, onClick }: { job: Job; onClick: () => void }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  return (
    <tr
      className="border-b border-[#f5f5f5] cursor-pointer hover:bg-[#fafafa] transition-colors"
      onClick={onClick}
    >
      <td className="px-4 py-2.5 align-middle">
        <span className="flex items-center gap-2.5 min-w-0">
          <ActivityDot job={job} />
          <span className="flex flex-col min-w-0">
            <span className="text-[13px] text-[#171717] truncate leading-snug font-medium">
              {job.issue_title || job.tmux || '—'}
            </span>
            {job.issue_title && job.tmux && (
              <code className="text-[11px] text-[#a3a3a3] leading-tight">{job.tmux}</code>
            )}
          </span>
        </span>
      </td>
      <td className="px-4 py-2.5 align-middle text-[13px] text-[#525252]">
        {job.issue ? (
          <a
            href={`https://github.com/denoland/orchid/issues/${job.issue}`}
            target="_blank"
            rel="noopener noreferrer"
            className="hover:underline font-mono"
            onClick={(e) => e.stopPropagation()}
          >
            #{job.issue}
          </a>
        ) : '—'}
      </td>
      <td className="px-4 py-2.5 align-middle text-[13px] text-[#737373]">
        {job.target_repo ? (
          <a
            href={`https://github.com/${job.target_repo}`}
            target="_blank"
            rel="noopener noreferrer"
            className="hover:underline"
            onClick={(e) => e.stopPropagation()}
          >
            {repo}
          </a>
        ) : '—'}
      </td>
      <td className="px-4 py-2.5 align-middle text-[13px]">
        {job.pr ? (
          <span className="flex items-center gap-2">
            <a
              href={`https://github.com/${job.target_repo}/pull/${job.pr}`}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:underline font-mono text-[#525252]"
              onClick={(e) => e.stopPropagation()}
            >
              #{job.pr}
            </a>
            <CIBadge conclusions={job.last_check_conclusions ?? {}} />
          </span>
        ) : '—'}
      </td>
    </tr>
  )
}

function FreeRow() {
  return (
    <tr className="border-b border-[#f5f5f5]">
      <td className="px-4 py-2.5 align-middle">
        <span className="flex items-center gap-2.5">
          <ActivityDot job={null} />
          <span className="text-[13px] text-[#d4d4d4]">free</span>
        </span>
      </td>
      <td className="px-4 py-2.5 text-[#d4d4d4] text-[13px]">—</td>
      <td className="px-4 py-2.5 text-[#d4d4d4] text-[13px]">—</td>
      <td className="px-4 py-2.5 text-[#d4d4d4] text-[13px]">—</td>
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
    <div className="min-h-screen bg-white px-6 py-8">
      <div className="mb-4 flex items-baseline justify-between">
        <span className="font-mono text-[13px] font-semibold text-[#171717]">orchid</span>
        <span className="text-[12px] text-[#a3a3a3]">
          {cap > 0 && <>{busy}/{cap} · </>}
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
          {inbox && <> · </>}↻{tick}s
        </span>
      </div>

      <div className="border border-[#ebebeb] rounded-lg overflow-hidden">
        <table className="w-full table-fixed border-collapse text-[13px]">
          <thead>
            <tr className="border-b border-[#ebebeb] bg-[#fafafa]">
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[42%]">
                Session
              </th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[12%]">
                Issue
              </th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[20%]">
                Repo
              </th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[26%]">
                PR
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td colSpan={4} className="px-4 py-10 text-center text-[#a3a3a3] text-[13px]">
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
    </div>
  )
}
