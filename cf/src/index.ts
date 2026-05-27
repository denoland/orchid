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
/// frames over that socket — see userSession.ts for the frame protocol.
import { Hono } from 'hono'
import { handleLogin, handleOAuthCallback, currentUser } from './oauth'
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

// Per-isolate cache for non-owner access checks. Cuts the DO round-trip
// to one per (sub, login) per 60s — the operator-defined allowlist
// changes rarely (when the agent reconnects with a fresh
// `allowed_logins` frame) and a brief lag is fine.
const accessCache = new Map<string, { ok: boolean; expires: number }>()
async function checkAllowed(env: Env, sub: string, login: string): Promise<boolean> {
  const key = sub + '|' + login.toLowerCase()
  const hit = accessCache.get(key)
  if (hit && hit.expires > Date.now()) return hit.ok
  try {
    const do_ = env.USER.get(env.USER.idFromName(sub))
    const r = await do_.fetch(new Request(
      'https://internal/_check_access?login=' + encodeURIComponent(login),
    ))
    const j = (await r.json()) as { ok: boolean }
    accessCache.set(key, { ok: !!j.ok, expires: Date.now() + 60_000 })
    return !!j.ok
  } catch {
    return false
  }
}

// HTTP proxy → user's Durable Object → out across the agent WS. The DO
// is the only thing that knows the shape of the agent connection; here
// we just buffer the request body (streaming across `do_.fetch` is
// unreliable in Workers) and pack the original URL + headers into
// sidecar headers so the DO can reconstruct an inner Request.
async function proxyToAgent(env: Env, subdomain: string, req: Request): Promise<Response> {
  const do_ = env.USER.get(env.USER.idFromName(subdomain))
  const url = new URL(req.url)
  const innerHeaders: [string, string][] = []
  req.headers.forEach((v, k) => {
    if (!k.toLowerCase().startsWith('x-orchid-inner-')) innerHeaders.push([k, v])
  })
  const body = ['GET', 'HEAD'].includes(req.method) ? null : await req.arrayBuffer()
  return do_.fetch(new Request('https://internal/_proxy', {
    method: req.method,
    headers: {
      'x-orchid-inner-url': url.pathname + url.search,
      'x-orchid-inner-method': req.method,
      'x-orchid-inner-headers': btoa(JSON.stringify(innerHeaders)),
    },
    body,
  }))
}

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
  // cookie clear actually lives. Same for /login so the landing's
  // "Get started" button works even when it's served on a subdomain.
  if (url.pathname === '/logout' || url.pathname === '/login') {
    const next = url.searchParams.get('next') ?? `https://${url.host}/`
    const target = url.pathname === '/login'
      ? `https://${c.env.ROOT_DOMAIN}/login?next=${encodeURIComponent(next)}`
      : `https://${c.env.ROOT_DOMAIN}/logout`
    return Response.redirect(target, 302)
  }

  // /agent WS — orch instances connect here. Auth happens inside the DO
  // against the agent token; no session cookie required.
  if (url.pathname === '/agent') {
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    return do_.fetch(c.req.raw)
  }

  // Capture intake bypasses the session-cookie gate — orch's own
  // X-Capture-Token header is the actual auth for these requests, and
  // the macOS / iOS apps don't carry a relay session cookie. Anything
  // unauthenticated still has to satisfy orch's per-endpoint check.
  //
  // /api/vm/join (worker joining a swarm) gets the same bypass: the
  // worker only has the bearer invite token (= orch's http_secret), not
  // a relay user session — orch's auth() still requires the bearer
  // match, so this just lets the request reach the agent.
  const isCapture = c.req.raw.method === 'POST' &&
    (url.pathname === '/api/drafts' || url.pathname.startsWith('/captures/'))
  const isVMJoin = c.req.raw.method === 'POST' && url.pathname === '/api/vm/join'
  // Any /api/* request that already carries a Bearer token gets to skip
  // the session-cookie gate — orch will reject the bearer itself if wrong.
  // This is what the iOS app + scripted callers use: paste http_secret
  // into Authorization and hit /api/state directly.
  const isApiBearer = url.pathname.startsWith('/api/') &&
    (c.req.raw.headers.get('authorization') ?? '').toLowerCase().startsWith('bearer ')

  // currentUser() runs an HMAC verify on the session cookie; skip it for
  // capture POSTs and /api/vm/join which never carry a session cookie
  // (they carry their own bearer tokens checked inside orch). user stays
  // null in those cases — DO-side auth is the real gate.
  let user: Awaited<ReturnType<typeof currentUser>> = null
  if (!isCapture && !isVMJoin && !isApiBearer) {
    // Everything else on a subdomain is private. Two ways in:
    //   1. Owner (cookie.subdomain === host's subdomain).
    //   2. Allowed login (operator-defined list pushed by the agent from
    //      swarm.hcl). Lets the owner share their dashboard with specific
    //      GitHub users without granting full account access.
    // The relay injects an Authorization header onto tunneled requests so
    // orch's own auth would let them through unconditionally — gate here.
    user = await currentUser(c.env, c.req.raw)
    let allowed = false
    if (user) {
      allowed = user.subdomain === sub || await checkAllowed(c.env, sub, user.login)
    }
    if (!allowed) {
      const loginURL = `https://${c.env.ROOT_DOMAIN}/login?next=` +
        encodeURIComponent(`https://${sub}.${c.env.ROOT_DOMAIN}${url.pathname}${url.search}`)
      const wantsHTML = (c.req.raw.headers.get('accept') ?? '').includes('text/html')
      if (wantsHTML) return Response.redirect(loginURL, 302)
      return new Response('unauthorized', { status: 401 })
    }
  }

  // Relay-side meta endpoint the dashboard polls to learn if the user's
  // orch is online + grab the agent token for the install modal. The
  // agent token is the secret that lets ANY holder impersonate the
  // owner's orch on the relay, so /info is owner-only — non-owner
  // collaborators in allowed_logins get a connectivity-only view.
  if (url.pathname === '/api/_relay/info') {
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    if (!user || user.subdomain !== sub) {
      // Hand collaborators the bare connectivity bit they need to render
      // the "online/offline" pill, never the token.
      const r = await do_.fetch(new Request('https://internal/_info'))
      const j = (await r.json()) as { connected?: boolean }
      return Response.json({ connected: !!j.connected, root: c.env.ROOT_DOMAIN })
    }
    const r = await do_.fetch(new Request('https://internal/_info'))
    const j = (await r.json()) as Record<string, any>
    return Response.json({ ...j, root: c.env.ROOT_DOMAIN })
  }

  // Owner-initiated agent-token reset. Wipes the DO-side token and
  // boots the live agent — operator gets a fresh token to wire into
  // `orch join` on the next sign-in.
  if (url.pathname === '/api/_relay/revoke' && c.req.raw.method === 'POST') {
    if (!user || user.subdomain !== sub) {
      return new Response('owner only', { status: 403 })
    }
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    return do_.fetch(new Request('https://internal/_revoke', { method: 'POST' }))
  }

  // Owner-only repo list, sourced from the user's GitHub OAuth token.
  // Drives the inbox + target repo pickers in Settings.
  if (url.pathname === '/api/_relay/repos') {
    if (!user || user.subdomain !== sub) {
      return new Response('owner only', { status: 403 })
    }
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    return do_.fetch(new Request('https://internal/_gh_repos'))
  }

  // Push channel for /api/state + relay-info. The DO holds a hibernated
  // WS per browser tab; orch + relay broadcast frames as conditions
  // change. Bypasses the agent-tunnel path entirely so an idle
  // subscriber costs zero DO CPU/requests after the initial open.
  if (url.pathname === '/api/events/ws') {
    if (c.req.raw.headers.get('upgrade')?.toLowerCase() !== 'websocket') {
      return new Response('expected websocket', { status: 426 })
    }
    const do_ = c.env.USER.get(c.env.USER.idFromName(sub))
    // The owner flag controls whether relay-info frames sent on this WS
    // include the agent token. checked here against the same
    // user.subdomain test the rest of the middleware uses.
    const isOwner = !!user && user.subdomain === sub
    return do_.fetch(new Request('https://internal/_events_ws', {
      method: 'GET',
      headers: {
        'upgrade': 'websocket',
        'x-orchid-owner': isOwner ? '1' : '0',
      },
    }))
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

// /docs[/<slug>] on the apex serves the SPA shell so the React Docs
// route can mount and render bundled markdown. Raw .md files under
// /docs/*.md still hit ASSETS directly via the catch-all below.
app.get('/docs', (c) => {
  const u = new URL(c.req.url); u.pathname = '/index.html'
  return c.env.ASSETS.fetch(new Request(u, c.req.raw))
})
app.get('/docs/:slug', async (c) => {
  const u = new URL(c.req.url)
  if (u.pathname.endsWith('.md')) return c.env.ASSETS.fetch(c.req.raw)
  u.pathname = '/index.html'
  return c.env.ASSETS.fetch(new Request(u, c.req.raw))
})

app.all('*', (c) => c.env.ASSETS.fetch(c.req.raw))

export default app
