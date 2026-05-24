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
