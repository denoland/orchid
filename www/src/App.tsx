import { useEffect, useState } from 'react'
import { Dashboard } from './Dashboard'
import { InstallModal } from './InstallModal'
import type { State } from './types'

export default function App() {
  const [state, setState] = useState<State>({ jobs: [], vms: [], inbox: '', operator: '' })

  useEffect(() => {
    let cancelled = false
    let id: ReturnType<typeof setInterval> | undefined
    async function poll() {
      // Skip the round-trip when the tab is hidden — backgrounded tabs
      // were our biggest unnecessary DO bill. The visibilitychange
      // listener below kicks an immediate poll on return so the user
      // never sees stale state.
      if (document.hidden) return
      try {
        const res = await fetch('/api/state')
        // Session cookie missing / expired — kick to OAuth so the
        // browser doesn't sit on an empty dashboard forever.
        if (res.status === 401 || res.status === 403) {
          // /login lives on the apex. Strip the leftmost subdomain
          // segment to reach it, then come back here after OAuth.
          const apex = location.host.split('.').slice(1).join('.')
          const next = encodeURIComponent(location.href)
          location.href = `https://${apex}/login?next=${next}`
          return
        }
        if (!res.ok) return
        const data: State = await res.json()
        if (!cancelled) setState(data)
      } catch { /* swallow */ }
    }
    poll()
    // 3s instead of 1s — orchid dispatch is multi-second anyway,
    // so sub-second polling just burned DO requests.
    id = setInterval(poll, 3000)
    const onVis = () => { if (!document.hidden) poll() }
    document.addEventListener('visibilitychange', onVis)
    return () => {
      cancelled = true
      if (id) clearInterval(id)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [])

  return (
    <>
      <Dashboard state={state} />
      <InstallModal />
    </>
  )
}
