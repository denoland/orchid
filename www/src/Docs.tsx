import { useEffect, useState } from 'react'
import { marked } from 'marked'

const PAGES: { slug: string; title: string; file: string; lede?: string }[] = [
  {
    slug: 'supervision',
    title: 'Chat with your orchid',
    file: 'SUPERVISION.md',
    lede: 'Point an OpenClaw or Hermes agent at orchid and talk to it on Telegram or Slack.',
  },
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

  const nav = (to: string | null, e: React.MouseEvent) => {
    e.preventDefault()
    history.pushState({}, '', to ? `/docs/${to}` : '/docs')
    setSlug(to)
  }

  if (!slug) {
    return (
      <Shell>
        <header className="docs-hero">
          <h1 className="docs-hero-title"><em>Docs</em></h1>
          <p className="docs-hero-lede">Everything orchid knows how to do — written down.</p>
        </header>
        <ul className="docs-index">
          {PAGES.map((p) => (
            <li key={p.slug}>
              <a href={`/docs/${p.slug}`} onClick={(e) => nav(p.slug, e)} className="docs-card">
                <span className="docs-card-title">{p.title}</span>
                {p.lede && <span className="docs-card-lede">{p.lede}</span>}
              </a>
            </li>
          ))}
        </ul>
      </Shell>
    )
  }

  const page = PAGES.find((p) => p.slug === slug)
  if (!page) {
    return (
      <Shell>
        <p className="docs-back-wrap">
          <a href="/docs" onClick={(e) => nav(null, e)} className="docs-back">← All docs</a>
        </p>
        <p>Page not found.</p>
      </Shell>
    )
  }

  return (
    <Shell>
      <p className="docs-back-wrap">
        <a href="/docs" onClick={(e) => nav(null, e)} className="docs-back">← All docs</a>
      </p>
      <article className="docs-prose" dangerouslySetInnerHTML={{ __html: body }} />
    </Shell>
  )
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="docs-page">
      <header className="docs-nav">
        <a href="/" className="docs-brand"><em>Orchid</em></a>
        <a className="docs-nav-link" href="/docs" onClick={(e) => {
          e.preventDefault()
          history.pushState({}, '', '/docs')
          window.dispatchEvent(new PopStateEvent('popstate'))
        }}>Docs</a>
        <a className="docs-signin" href="/login">Sign in</a>
      </header>
      <main className="docs-main">{children}</main>
      <footer className="docs-footer">
        <span>orchid · <a href="https://github.com/denoland/orchid">github</a></span>
      </footer>
    </div>
  )
}
