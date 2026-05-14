import type { State, Job } from './types'

interface Props {
  state: State
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

function Sparkline({ data, width = 80, height = 18 }: { data?: number[]; width?: number; height?: number }) {
  const buckets = 30
  const raw = data ?? []
  const padded: number[] = raw.length >= buckets
    ? raw.slice(-buckets)
    : Array.from({ length: buckets - raw.length }, () => 0).concat(raw)
  const max = Math.max(1, ...padded)
  const step = padded.length === 1 ? 0 : width / (padded.length - 1)
  const pts = padded.map((v, i) => [i * step, height - (v / max) * (height - 1) - 0.5] as const)
  const line = 'M ' + pts.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' L ')
  const fill = line + ` L ${width},${height} L 0,${height} Z`
  const active = padded.some((v) => v > 0)
  const color = active ? '#22c55e' : '#d4d4d4'
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="block" preserveAspectRatio="none">
      <path d={fill} fill={color} opacity={0.15} />
      <path d={line} fill="none" stroke={color} strokeWidth={1.25} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}

function ActivityDot({ job }: { job: Job | null }) {
  if (!job) {
    return (
      <span className="relative inline-flex w-2 h-2 flex-shrink-0">
        <span className="w-2 h-2 rounded-full bg-[#e5e5e5]" />
      </span>
    )
  }
  if (job.lifecycle === 'cron') {
    const active = job.tmux !== ''
    return (
      <span className="relative inline-flex w-2 h-2 flex-shrink-0">
        {active && <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-[#a855f7] opacity-50" />}
        <span className={`relative w-2 h-2 rounded-full ${active ? 'bg-[#a855f7]' : 'bg-[#d4d4d4]'}`} />
      </span>
    )
  }
  return (
    <span className="relative inline-flex w-2 h-2 flex-shrink-0">
      <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-[#22c55e] opacity-40" />
      <span className="relative w-2 h-2 rounded-full bg-[#22c55e]" />
    </span>
  )
}

function CIBadge({ conclusions }: { conclusions: Record<string, string> }) {
  const s = ciStatus(conclusions)
  if (s === 'fail') return (
    <span className="inline-flex text-[10px] font-medium text-[#dc2626] bg-[#fef2f2] border border-[#fecaca] rounded px-1.5 py-0.5 flex-shrink-0">
      CI fail
    </span>
  )
  if (s === 'pass') return (
    <span className="inline-flex text-[10px] font-medium text-[#16a34a] bg-[#f0fdf4] border border-[#bbf7d0] rounded px-1.5 py-0.5 flex-shrink-0">
      CI pass
    </span>
  )
  return null
}

function JobCard({ job, onClick }: { job: Job; onClick: () => void }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || ''
  const issueUrl = job.issue ? `https://github.com/denoland/orchid/issues/${job.issue}` : null
  return (
    <div
      className="px-4 py-3 border-b border-[#f5f5f5] cursor-pointer hover:bg-[#fafafa] transition-colors active:bg-[#f5f5f5]"
      onClick={onClick}
    >
      <div className="flex items-start gap-2.5 min-w-0">
        <div className="mt-[5px]"><ActivityDot job={job} /></div>
        <div className="flex-1 min-w-0">
          <div className="text-[13px] font-medium text-[#171717] leading-snug truncate">
            {issueUrl ? (
              <a href={issueUrl} target="_blank" rel="noopener noreferrer"
                className="hover:underline" onClick={(e) => e.stopPropagation()}>
                {job.issue_title || job.tmux || '—'}
              </a>
            ) : (job.issue_title || job.tmux || '—')}
          </div>
          <div className="flex items-center gap-2 mt-1 flex-wrap">
            {repo && <span className="text-[11px] text-[#a3a3a3]">{repo}</span>}
            {job.tmux && <code className="text-[11px] text-[#c4c4c4]">{job.tmux}</code>}
            {job.pr && (
              <a
                href={`https://github.com/${job.target_repo}/pull/${job.pr}`}
                target="_blank" rel="noopener noreferrer"
                className="text-[11px] text-[#525252] font-mono hover:underline"
                onClick={(e) => e.stopPropagation()}
              >
                PR #{job.pr}
              </a>
            )}
            {job.pr && <CIBadge conclusions={job.last_check_conclusions ?? {}} />}
          </div>
          {job.activity && job.activity.length > 0 && (
            <div className="mt-1.5">
              <Sparkline data={job.activity} width={100} height={14} />
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function FreeCard() {
  return (
    <div className="px-4 py-3 border-b border-[#f5f5f5]">
      <div className="flex items-center gap-2.5">
        <ActivityDot job={null} />
        <span className="text-[13px] text-[#d4d4d4]">free</span>
      </div>
    </div>
  )
}

// Desktop-only table row variants
function JobRow({ job, onClick }: { job: Job; onClick: () => void }) {
  const repo = job.target_repo ? job.target_repo.split('/')[1] : job.target || '—'
  const issueUrl = job.issue ? `https://github.com/denoland/orchid/issues/${job.issue}` : null
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
              {issueUrl ? (
                <a href={issueUrl} target="_blank" rel="noopener noreferrer"
                  className="hover:underline" onClick={(e) => e.stopPropagation()}>
                  {job.issue_title || job.tmux || '—'}
                </a>
              ) : (job.issue_title || job.tmux || '—')}
            </span>
            {job.issue_title && job.tmux && (
              <code className="text-[11px] text-[#a3a3a3] leading-tight">{job.tmux}</code>
            )}
          </span>
        </span>
      </td>
      <td className="px-4 py-2.5 align-middle">
        <Sparkline data={job.activity} width={80} height={18} />
      </td>
      <td className="px-4 py-2.5 align-middle text-[13px] text-[#737373]">
        {job.target_repo ? (
          <a href={`https://github.com/${job.target_repo}`} target="_blank" rel="noopener noreferrer"
            className="hover:underline" onClick={(e) => e.stopPropagation()}>
            {repo}
          </a>
        ) : '—'}
      </td>
      <td className="px-4 py-2.5 align-middle text-[13px]">
        {job.pr ? (
          <span className="flex items-center gap-2">
            <a href={`https://github.com/${job.target_repo}/pull/${job.pr}`} target="_blank" rel="noopener noreferrer"
              className="hover:underline font-mono text-[#525252]" onClick={(e) => e.stopPropagation()}>
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

export function Dashboard({ state }: Props) {
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

  const handleClick = (tmux: string) => {
    if (tmux) window.location.hash = `/pane/${encodeURIComponent(tmux)}`
  }

  return (
    <div className="min-h-screen bg-white px-4 sm:px-6 pt-4 sm:pt-6 pb-8">
      <div className="mb-5 sm:mb-6">
        <h1 className="font-mono text-[26px] sm:text-[28px] font-bold text-[#171717] tracking-tight leading-none mb-1">
          orchid
        </h1>
        <div className="text-[12px] text-[#a3a3a3] flex items-center gap-2 flex-wrap">
          {cap > 0 && <span>{busy}/{cap} active</span>}
          {cap > 0 && inbox && <span>·</span>}
          {inbox && (
            <a href={`https://github.com/${inbox}/issues`} target="_blank" rel="noopener noreferrer"
              className="hover:text-[#404040] transition-colors">
              {inbox}
            </a>
          )}
        </div>
      </div>

      <div className="border border-[#ebebeb] rounded-lg overflow-hidden">
        {/* Mobile: card list */}
        <div className="sm:hidden">
          {rows.length === 0 ? (
            <div className="px-4 py-10 text-center text-[#a3a3a3] text-[13px]">no sessions</div>
          ) : (
            rows.map((row, i) =>
              row.type === 'job' ? (
                <JobCard key={`job-${row.job.issue}-${row.job.tmux}-${i}`} job={row.job}
                  onClick={() => handleClick(row.job.tmux)} />
              ) : (
                <FreeCard key={`free-${i}`} />
              )
            )
          )}
        </div>

        {/* Desktop: table */}
        <table className="hidden sm:table w-full table-fixed border-collapse text-[13px]">
          <thead>
            <tr className="border-b border-[#ebebeb] bg-[#fafafa]">
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[44%]">Session</th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[14%]">Activity</th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[18%]">Repo</th>
              <th className="px-4 py-2 text-left text-[10px] uppercase tracking-[.1em] text-[#a3a3a3] font-medium w-[24%]">PR</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td colSpan={4} className="px-4 py-10 text-center text-[#a3a3a3] text-[13px]">no sessions</td>
              </tr>
            ) : (
              rows.map((row, i) =>
                row.type === 'job' ? (
                  <JobRow key={`job-${row.job.issue}-${row.job.tmux}-${i}`} job={row.job}
                    onClick={() => handleClick(row.job.tmux)} />
                ) : (
                  <FreeRow key={`free-${i}`} />
                )
              )
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
