import { useEffect, useState } from 'react'
import { Dashboard } from './Dashboard'
import { Pane } from './Pane'
import type { State } from './types'

type Route =
  | { name: 'dashboard' }
  | { name: 'pane'; session: string }

function parseRoute(): Route {
  const h = window.location.hash
  const m = h.match(/^#\/pane\/(.+)$/)
  if (m) return { name: 'pane', session: decodeURIComponent(m[1]) }
  return { name: 'dashboard' }
}

export default function App() {
  const [route, setRoute] = useState<Route>(parseRoute)
  const [state, setState] = useState<State>({ jobs: [], vms: [], inbox: '' })
  const [tick, setTick] = useState(3)

  useEffect(() => {
    const onHash = () => setRoute(parseRoute())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  useEffect(() => {
    let cancelled = false

    async function poll() {
      try {
        const res = await fetch('/api/state')
        if (res.ok) {
          const data: State = await res.json()
          if (!cancelled) setState(data)
        }
      } catch {
        /* swallow */
      }
    }

    poll()
    const pollInterval = setInterval(poll, 3000)

    // countdown tick
    setTick(3)
    const tickInterval = setInterval(() => {
      setTick((t) => (t <= 1 ? 3 : t - 1))
    }, 1000)

    return () => {
      cancelled = true
      clearInterval(pollInterval)
      clearInterval(tickInterval)
    }
  }, [])

  if (route.name === 'pane') {
    return <Pane session={route.session} />
  }

  return <Dashboard state={state} tick={tick} />
}
