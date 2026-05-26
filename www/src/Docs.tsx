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
          <div className="docs-side-section">Guides</div>
          {PAGES.map((p) => (
            <a
              key={p.slug}
              href={`/docs/${p.slug}`}
              onClick={(e) => go(p.slug, e)}
              className={'docs-side-link' + (slug === p.slug ? ' is-active' : '')}
            >
              {p.title}
            </a>
          ))}
        </aside>
        <main className="docs-main">
          {!slug && (
            <>
              <h1 className="docs-hero-title"><em>Docs</em></h1>
              <p className="docs-hero-lede">Everything orchid knows how to do — written down.</p>
              <ul className="docs-index">
                {PAGES.map((p) => (
                  <li key={p.slug}>
                    <a href={`/docs/${p.slug}`} onClick={(e) => go(p.slug, e)} className="docs-card">
                      <span className="docs-card-title">{p.title}</span>
                      {p.lede && <span className="docs-card-lede">{p.lede}</span>}
                    </a>
                  </li>
                ))}
              </ul>
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
