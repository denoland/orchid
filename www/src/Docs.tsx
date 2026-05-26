import { useEffect, useState } from 'react'
import { marked } from 'marked'

interface Page { slug: string; title: string; file: string; lede?: string; section: 'start' | 'configure' | 'integrate' | 'deep' }
const PAGES: Page[] = [
  {
    slug: 'getting-started',
    title: 'Getting started',
    file: 'getting-started.md',
    section: 'start',
    lede: 'Sign in, install on a machine, file your first inbox issue.',
  },
  {
    slug: 'dashboard',
    title: 'Dashboard',
    file: 'dashboard.md',
    section: 'start',
    lede: 'Tour the canvas, list view, panes, and settings.',
  },
  {
    slug: 'configuration',
    title: 'Configuration',
    file: 'configuration.md',
    section: 'configure',
    lede: 'Every field in swarm.hcl, what it does, when to change it.',
  },
  {
    slug: 'targets',
    title: 'Targets',
    file: 'targets.md',
    section: 'configure',
    lede: 'Route labels in the inbox to different work repos.',
  },
  {
    slug: 'workers',
    title: 'Workers',
    file: 'workers.md',
    section: 'configure',
    lede: 'Scale the swarm to multiple VMs.',
  },
  {
    slug: 'capture',
    title: 'Capture',
    file: 'capture.md',
    section: 'integrate',
    lede: 'macOS, iOS, and watchOS draft intake apps.',
  },
  {
    slug: 'supervision',
    title: 'Supervision',
    file: 'SUPERVISION.md',
    section: 'integrate',
    lede: 'Chat with your orchid on Telegram, Slack, Discord via OpenClaw or Hermes.',
  },
  {
    slug: 'architecture',
    title: 'Architecture',
    file: 'architecture.md',
    section: 'deep',
    lede: 'How orch, the relay, and Claude sessions fit together.',
  },
]

const SECTIONS: { id: Page['section']; title: string }[] = [
  { id: 'start',     title: 'Start here' },
  { id: 'configure', title: 'Configure' },
  { id: 'integrate', title: 'Integrate' },
  { id: 'deep',      title: 'Under the hood' },
]

function slugFromPath(): string | null {
  const m = location.pathname.match(/^\/docs\/?([^/]*)/)
  if (!m) return null
  return m[1] || null
}

export function Docs() {
  const [slug, setSlug] = useState(slugFromPath())
  const [body, setBody] = useState<string>('')

  useEffect(() => {
    const onPop = () => setSlug(slugFromPath())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  useEffect(() => {
    if (!slug) { setBody(''); return }
    const page = PAGES.find((p) => p.slug === slug)
    if (!page) { setBody(''); return }
    let cancelled = false
    fetch(`/docs/${page.file}`)
      .then((r) => r.text())
      .then((md) => { if (!cancelled) setBody(marked.parse(md) as string) })
    return () => { cancelled = true }
  }, [slug])

  const go = (to: string | null, e: React.MouseEvent) => {
    e.preventDefault()
    history.pushState({}, '', to ? `/docs/${to}` : '/docs')
    setSlug(to)
    window.scrollTo(0, 0)
  }

  const page = slug ? PAGES.find((p) => p.slug === slug) ?? null : null

  return (
    <div className="docs-page">
      <header className="docs-nav">
        <a href="/" className="docs-brand"><em>Orchid</em></a>
        <a className="docs-signin" href="/login">Sign in</a>
      </header>
      <div className="docs-layout">
        <aside className="docs-sidebar">
          <a
            href="/docs"
            onClick={(e) => go(null, e)}
            className={'docs-side-link' + (slug === null ? ' is-active' : '')}
          >
            Overview
          </a>
          {SECTIONS.map((s) => {
            const items = PAGES.filter((p) => p.section === s.id)
            if (items.length === 0) return null
            return (
              <div key={s.id}>
                <div className="docs-side-section">{s.title}</div>
                {items.map((p) => (
                  <a
                    key={p.slug}
                    href={`/docs/${p.slug}`}
                    onClick={(e) => go(p.slug, e)}
                    className={'docs-side-link' + (slug === p.slug ? ' is-active' : '')}
                  >
                    {p.title}
                  </a>
                ))}
              </div>
            )
          })}
        </aside>
        <main className="docs-main">
          {!slug && (
            <>
              <h1 className="docs-hero-title"><em>Docs</em></h1>
              <p className="docs-hero-lede">Everything orchid knows how to do — written down.</p>
              {SECTIONS.map((s) => {
                const items = PAGES.filter((p) => p.section === s.id)
                if (items.length === 0) return null
                return (
                  <section key={s.id} className="docs-section">
                    <h2 className="docs-section-title">{s.title}</h2>
                    <ul className="docs-index">
                      {items.map((p) => (
                        <li key={p.slug}>
                          <a href={`/docs/${p.slug}`} onClick={(e) => go(p.slug, e)} className="docs-card">
                            <span className="docs-card-title">{p.title}</span>
                            {p.lede && <span className="docs-card-lede">{p.lede}</span>}
                          </a>
                        </li>
                      ))}
                    </ul>
                  </section>
                )
              })}
            </>
          )}
          {slug && !page && <p>Page not found.</p>}
          {slug && page && <article className="docs-prose" dangerouslySetInnerHTML={{ __html: body }} />}
        </main>
      </div>
      <footer className="docs-footer">
        <span>orchid · <a href="https://github.com/denoland/orchid">github</a></span>
      </footer>
    </div>
  )
}
