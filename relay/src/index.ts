/// Orchid relay — Cloudflare Worker.
///
/// Routing:
///   GET  orchid.littledivy.com/                        → landing page (public/index.html)
///   GET  orchid.littledivy.com/login                   → start GitHub OAuth
///   GET  orchid.littledivy.com/oauth/callback          → finish OAuth, set session, redirect to <sub>.orchid.littledivy.com
///   GET  orchid.littledivy.com/dashboard               → personal dashboard hub (links to <sub>)
///   *    <sub>.orchid.littledivy.com/agent             → WS endpoint for the user's orch instance
///   *    <sub>.orchid.littledivy.com/api/*             → proxied to the user's connected orch via DO
///   GET  <sub>.orchid.littledivy.com/* (rest)          → embedded dashboard SPA (public/dash/*)
///
/// Each user owns a Durable Object whose name is the lowercased GitHub
/// login. The DO holds the agent's WebSocket. HTTP/SSE proxying multiplexes
/// frames over that socket — see frames.ts.
import { Hono } from 'hono'
import { handleLogin, handleOAuthCallback, currentUser } from './oauth'
import { proxyToAgent } from './proxy'
export { } /* hush ts about empty deps */

export { UserSession } from './userSession'

interface Env {
  ASSETS: Fetcher
  USER: DurableObjectNamespace
  OAUTH: KVNamespace
  GH_CLIENT_ID: string
  GH_CLIENT_SECRET: string
  SESSION_KEY: string
  ROOT_DOMAIN: string
}

const app = new Hono<{ Bindings: Env }>()

// Subdomain detection. Anything that isn't the apex is a user subdomain.
function subOf(host: string, root: string): string | null {
  const h = host.toLowerCase()
  const r = root.toLowerCase()
  if (h === r) return null
  if (h.endsWith('.' + r)) {
    const sub = h.slice(0, -1 - r.length)
    if (sub && !sub.includes('.')) return sub // single-label subdomain only
  }
  return null
}

// Subdomain routes must run BEFORE apex path routes — Hono matches by path
// only, so without this middleware a request like `divy.orchid.littledivy.com/`
// would fall into the apex `app.get('/')` and serve the landing page.
app.use('*', async (c, next) => {
  const url = new URL(c.req.url)
  const sub = subOf(url.host, c.env.ROOT_DOMAIN)
  if (!sub) return next()

  // /logout from any subdomain → bounce to apex /logout where the
  // cookie clear actually lives.
  if (url.pathname === '/logout') {
    return Response.redirect(`https://${c.env.ROOT_DOMAIN}/logout`, 302)
  }

  // /agent WS — orch instances connect here. Auth happens inside the DO
  // against the agent token; no session cookie required.
  if (url.pathname === '/agent') {
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    return do_.fetch(c.req.raw)
  }

  // Everything else on a subdomain is private. Two ways in:
  //   1. Owner (cookie.subdomain === host's subdomain).
  //   2. Allowed login (operator-defined list pushed by the agent from
  //      swarm.hcl). Lets the owner share their dashboard with specific
  //      GitHub users without granting full account access.
  // The relay injects an Authorization header onto tunneled requests so
  // orch's own auth would let them through unconditionally — gate here.
  const user = await currentUser(c.env, c.req.raw)
  let allowed = false
  if (user) {
    if (user.subdomain === sub) {
      allowed = true
    } else {
      const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
      try {
        const r = await do_.fetch(new Request(
          'https://internal/_check_access?login=' + encodeURIComponent(user.login),
        ))
        const j = (await r.json()) as { ok: boolean }
        allowed = !!j.ok
      } catch { /* deny on failure */ }
    }
  }
  if (!allowed) {
    const loginURL = `https://${c.env.ROOT_DOMAIN}/login?next=` +
      encodeURIComponent(`https://${sub}.${c.env.ROOT_DOMAIN}${url.pathname}${url.search}`)
    const wantsHTML = (c.req.raw.headers.get('accept') ?? '').includes('text/html')
    if (wantsHTML) return Response.redirect(loginURL, 302)
    return new Response('unauthorized', { status: 401 })
  }

  // Relay-side meta endpoint the dashboard polls to learn if the user's
  // orch is online + grab the agent token for the install modal.
  if (url.pathname === '/api/_relay/info') {
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    return do_.fetch(new Request('https://internal/_info'))
  }

  // Proxy /api/* and /captures/ to the agent. WS upgrades go through a
  // dedicated DO path that pairs a WebSocketPair and multiplexes frames.
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/captures/')) {
    if (c.req.raw.headers.get('upgrade')?.toLowerCase() === 'websocket') {
      const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
      const innerHeaders: [string, string][] = []
      c.req.raw.headers.forEach((v, k) => innerHeaders.push([k, v]))
      const proxyReq = new Request('https://internal/_proxy_ws', {
        method: 'GET',
        headers: {
          'upgrade': 'websocket',
          'x-orchid-inner-url': url.pathname + url.search,
          'x-orchid-inner-headers': btoa(JSON.stringify(innerHeaders)),
        },
      })
      return do_.fetch(proxyReq)
    }
    return proxyToAgent(c.env, sub, c.req.raw)
  }

  // Anything else → SPA from ASSETS root. SPA-style fallback to
  // index.html so client-side routes survive a refresh.
  const res = await c.env.ASSETS.fetch(c.req.raw)
  if (res.status === 404) {
    const idx = new URL(c.req.url)
    idx.pathname = '/index.html'
    return c.env.ASSETS.fetch(new Request(idx, c.req.raw))
  }
  return res
})

// ─── apex routes (orchid.littledivy.com) ───
// Apex always serves the landing page — even for logged-in users. The
// "go to dashboard" hop lives behind the nav's sign-in button so the
// marketing page stays addressable.
app.get('/', async (c) => {
  const u = new URL(c.req.url)
  u.pathname = '/landing.html'
  return c.env.ASSETS.fetch(new Request(u, c.req.raw))
})

app.get('/login', (c) => handleLogin(c.env, c.req.raw))

app.get('/oauth/callback', (c) => handleOAuthCallback(c.env, c.req.raw))

app.get('/dashboard', async (c) => {
  const user = await currentUser(c.env, c.req.raw)
  if (!user) return Response.redirect(new URL('/login', c.req.url).toString(), 302)
  return Response.redirect(`https://${user.subdomain}.${c.env.ROOT_DOMAIN}/`, 302)
})

// Clear the session cookie and bounce to the landing page. Cookie is
// HttpOnly + domain-scoped so the JS side can't expire it directly — the
// dashboard's logout button posts here from any subdomain.
app.get('/logout', (c) => {
  const headers = new Headers()
  headers.set('set-cookie',
    `orchid_session=; Domain=.${c.env.ROOT_DOMAIN}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0`)
  headers.set('location', `https://${c.env.ROOT_DOMAIN}/`)
  return new Response(null, { status: 302, headers })
})

app.get('/healthz', (c) => c.text('ok'))

app.all('*', (c) => c.env.ASSETS.fetch(c.req.raw))

export default app
