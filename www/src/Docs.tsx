import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { marked } from 'marked'
import { ILLUSTRATIONS } from './DocsIllustrations'
import { MOCKUPS } from './DocsMockups'

interface Page { slug: string; title: string; file: string; lede?: string; section: 'start' | 'configure' | 'integrate' | 'deep' }
const PAGES: Page[] = [
  {
    slug: 'getting-started',
    title: 'Getting started',
    file: 'getting-started.md',
    section: 'start',
    lede: 'Install on a machine, connect GitHub, file your first inbox issue.',
  },
  {
    slug: 'dashboard',
    title: 'Dashboard',
    file: 'dashboard.md',
    section: 'start',
    lede: 'Tour the session list, PR/CI status, panes, and settings.',
  },
  {
    slug: 'configuration',
    title: 'Configuration',
    file: 'configuration.md',
    section: 'configure',
    lede: 'Every field in swarm.hcl, what it does, when to change it.',
  },
  {
    slug: 'throttling',
    title: 'Throttling & pacing',
    file: 'throttling.md',
    section: 'configure',
    lede: 'Spend the weekly quota evenly — hard gate, adaptive governor, duty-cycling, priority.',
  },
  {
    slug: 'targets',
    title: 'Targets',
    file: 'targets.md',
    section: 'configure',
    lede: 'Route labels in the inbox to different work repos.',
  },
  {
    slug: 'vms',
    title: 'VMs',
    file: 'vms.md',
    section: 'configure',
    lede: 'Scale the swarm across multiple machines.',
  },
  {
    slug: 'security',
    title: 'Security',
    file: 'security.md',
    section: 'configure',
    lede: 'Sandbox sessions with clawpatrol; rotate tokens.',
  },
  {
    slug: 'tailscale',
    title: 'Tailscale',
    file: 'tailscale.md',
    section: 'configure',
    lede: 'Run orch without public IPs using your own tailnet.',
  },
  {
    slug: 'memory',
    title: 'Memory',
    file: 'memory.md',
    section: 'integrate',
    lede: 'Shared, git-backed knowledge base the swarm accumulates across sessions.',
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

// Mirrors the dashboard ThemeToggle so /docs honors the same `orchid.theme`
// and toggles html.dark — docs is a standalone route, nothing else applies it.
function DocsThemeToggle() {
  const [dark, setDark] = useState(() => {
    if (typeof window === 'undefined') return false
    const saved = localStorage.getItem('orchid.theme')
    if (saved) return saved === 'dark'
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false
  })
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('orchid.theme', dark ? 'dark' : 'light')
  }, [dark])
  return (
    <button className="docs-icon-btn" onClick={() => setDark((d) => !d)} title={dark ? 'switch to light' : 'switch to dark'}>
      {dark ? (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
        </svg>
      ) : (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
        </svg>
      )}
    </button>
  )
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
      .then((md) => {
        if (cancelled) return
        // Replace {{illust:name}} / {{mockup:name}} markers with
        // placeholder divs the React effect mounts components into.
        const transformed = md
          .replace(/\{\{illust:([a-z0-9-]+)\}\}/g, (_, n) => `<span data-illust="${n}"></span>`)
          .replace(/\{\{mockup:([a-z0-9-]+)\}\}/g, (_, n) => `<div data-mockup="${n}"></div>`)
        setBody(marked.parse(transformed) as string)
      })
    return () => { cancelled = true }
  }, [slug])

  // After marked renders, hunt for diagram placeholders and portal the
  // matching React component into each. New body resets the list.
  type MountKind = 'i' | 'm'
  const articleRef = useRef<HTMLElement | null>(null)
  const [mounts, setMounts] = useState<{ kind: MountKind; name: string; el: HTMLElement }[]>([])
  useEffect(() => {
    if (!body || !articleRef.current) { setMounts([]); return }
    const next: { kind: MountKind; name: string; el: HTMLElement }[] = []
    const collect = (sel: string, kind: MountKind, dict: Record<string, React.FC>, attr: string) => {
      articleRef.current!.querySelectorAll<HTMLElement>(sel).forEach((el) => {
        const name = el.dataset[attr as keyof DOMStringMap] as string | undefined
        if (name && dict[name]) next.push({ kind, name, el })
      })
    }
    collect('[data-illust]', 'i', ILLUSTRATIONS, 'illust')
    collect('[data-mockup]', 'm', MOCKUPS,       'mockup')
    setMounts(next)
  }, [body])

  const [navOpen, setNavOpen] = useState(false)

  const go = (to: string | null, e: React.MouseEvent) => {
    e.preventDefault()
    history.pushState({}, '', to ? `/docs/${to}` : '/docs')
    setSlug(to)
    setNavOpen(false)
    window.scrollTo(0, 0)
  }

  const page = slug ? PAGES.find((p) => p.slug === slug) ?? null : null

  return (
    <div className={'docs-page' + (navOpen ? ' nav-open' : '')}>
      <header className="docs-nav">
        <button className="docs-burger" onClick={() => setNavOpen((v) => !v)} aria-label="Toggle sidebar">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
            <line x1="4" y1="7"  x2="20" y2="7"  />
            <line x1="4" y1="12" x2="20" y2="12" />
            <line x1="4" y1="17" x2="20" y2="17" />
          </svg>
        </button>
        <a href="/docs" className="docs-brand" onClick={(e) => go(null, e)}>
          <img src="/favicon.svg" alt="" />Orchid<span className="docs-crumb">Docs</span>
        </a>
        <div className="docs-nav-right">
          <DocsThemeToggle />
          <a className="docs-signin" href="https://github.com/denoland/orchid" target="_blank" rel="noopener">
            <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M8 0a8 8 0 0 0-2.53 15.59c.4.07.55-.17.55-.38v-1.49c-2.23.48-2.7-.95-2.7-.95-.36-.93-.9-1.18-.9-1.18-.73-.5.06-.49.06-.49.81.06 1.24.83 1.24.83.72 1.24 1.9.88 2.36.67.07-.52.28-.88.5-1.08-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.66 7.66 0 0 1 4 0c1.53-1.03 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.28.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.74.54 1.5v2.22c0 .21.15.46.55.38A8 8 0 0 0 8 0z"/></svg>
            GitHub
          </a>
        </div>
      </header>
      <div className="docs-scrim" onClick={() => setNavOpen(false)} />
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
              <h1 className="docs-hero-title">Documentation</h1>
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
          {slug && page && (
            <>
              <article ref={articleRef} className="docs-prose" dangerouslySetInnerHTML={{ __html: body }} />
              {mounts.map(({ kind, name, el }, i) => {
                const C = kind === 'i' ? ILLUSTRATIONS[name] : MOCKUPS[name]
                return createPortal(<C />, el, `${kind}-${name}-${i}`)
              })}
            </>
          )}
        </main>
      </div>
      <footer className="docs-footer">
        <span>orchid · <a href="https://github.com/denoland/orchid">github</a></span>
      </footer>
    </div>
  )
}
