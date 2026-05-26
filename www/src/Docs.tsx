import { useEffect, useState } from 'react'
import { marked } from 'marked'

const PAGES: { slug: string; title: string; file: string }[] = [
  { slug: 'supervision', title: 'Chat with your orchid', file: 'SUPERVISION.md' },
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
    if (!page) { setBody('') ; return }
    let cancelled = false
    fetch(`/docs/${page.file}`)
      .then((r) => r.text())
      .then((md) => { if (!cancelled) setBody(marked.parse(md) as string) })
    return () => { cancelled = true }
  }, [slug])

  const nav = (to: string | null, e: React.MouseEvent) => {
    e.preventDefault()
    const url = to ? `/docs/${to}` : '/docs'
    history.pushState({}, '', url)
    setSlug(to)
  }

  if (!slug) {
    return (
      <Shell>
        <h1 className="text-3xl font-semibold mb-6">Orchid Docs</h1>
        <ul className="space-y-2">
          {PAGES.map((p) => (
            <li key={p.slug}>
              <a
                href={`/docs/${p.slug}`}
                onClick={(e) => nav(p.slug, e)}
                className="text-violet-600 hover:underline"
              >
                {p.title}
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
        <p className="mb-4">Page not found.</p>
        <a href="/docs" onClick={(e) => nav(null, e)} className="text-violet-600 hover:underline">← All docs</a>
      </Shell>
    )
  }

  return (
    <Shell>
      <a href="/docs" onClick={(e) => nav(null, e)} className="text-sm text-violet-600 hover:underline">← All docs</a>
      <article className="docs-prose mt-4" dangerouslySetInnerHTML={{ __html: body }} />
    </Shell>
  )
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen bg-white text-zinc-900">
      <header className="border-b border-zinc-200">
        <div className="max-w-3xl mx-auto px-6 py-4 flex items-center justify-between">
          <a href="/" className="font-semibold">orchid</a>
          <nav className="text-sm text-zinc-500"><a href="/docs" className="hover:text-zinc-900">docs</a></nav>
        </div>
      </header>
      <main className="max-w-3xl mx-auto px-6 py-10">{children}</main>
    </div>
  )
}
