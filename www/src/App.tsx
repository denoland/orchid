import { useEffect, useState } from 'react'
import { Dashboard } from './Dashboard'
import { InstallModal } from './InstallModal'
import type { State } from './types'

export default function App() {
  const [state, setState] = useState<State>({ jobs: [], vms: [], inbox: '', operator: '' })

  useEffect(() => {
    let cancelled = false
    async function poll() {
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
    const id = setInterval(poll, 1000)
    return () => { cancelled = true; clearInterval(id) }
  }, [])

  return (
    <>
      <Dashboard state={state} />
      <InstallModal />
    </>
  )
}
