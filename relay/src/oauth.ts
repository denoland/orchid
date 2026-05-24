/// GitHub OAuth flow. Stores the resulting session in a signed cookie
/// (HS256 with SESSION_KEY). On first login a Durable Object is provisioned
/// for the user's subdomain and an agent token minted.
import { userSubdomain, mintAgentToken } from './userSession'

interface Env {
  OAUTH: KVNamespace
  USER: DurableObjectNamespace
  GH_CLIENT_ID: string
  GH_CLIENT_SECRET: string
  SESSION_KEY: string
  ROOT_DOMAIN: string
}

interface Session {
  login: string
  subdomain: string
  uid: number
}

const COOKIE = 'orchid_session'

function callbackURL(env: Env, req: Request): string {
  // Use the request's origin so localhost dev round-trips through the
  // same host; prod requests come in on ROOT_DOMAIN already.
  const u = new URL(req.url)
  return `${u.protocol}//${u.host}/oauth/callback`
}

export async function handleLogin(env: Env, req: Request): Promise<Response> {
  const state = crypto.randomUUID()
  const next = new URL(req.url).searchParams.get('next') ?? '/dashboard'
  await env.OAUTH.put(`state:${state}`, JSON.stringify({ next }), { expirationTtl: 600 })

  const callback = callbackURL(env, req)
  const url = new URL('https://github.com/login/oauth/authorize')
  url.searchParams.set('client_id', env.GH_CLIENT_ID)
  url.searchParams.set('redirect_uri', callback)
  url.searchParams.set('scope', 'read:user user:email')
  url.searchParams.set('state', state)
  return Response.redirect(url.toString(), 302)
}

export async function handleOAuthCallback(env: Env, req: Request): Promise<Response> {
  const url = new URL(req.url)
  const code = url.searchParams.get('code')
  const state = url.searchParams.get('state')
  if (!code || !state) return new Response('missing code/state', { status: 400 })

  const stateRow = await env.OAUTH.get(`state:${state}`)
  if (!stateRow) return new Response('invalid state', { status: 400 })
  await env.OAUTH.delete(`state:${state}`)
  const { next } = JSON.parse(stateRow) as { next: string }

  // Exchange code → access token.
  const tokenRes = await fetch('https://github.com/login/oauth/access_token', {
    method: 'POST',
    headers: { 'accept': 'application/json', 'content-type': 'application/json' },
    body: JSON.stringify({
      client_id: env.GH_CLIENT_ID,
      client_secret: env.GH_CLIENT_SECRET,
      code,
      redirect_uri: callbackURL(env, req),
    }),
  })
  if (!tokenRes.ok) return new Response('github token exchange failed', { status: 502 })
  const token = (await tokenRes.json()) as { access_token?: string }
  if (!token.access_token) return new Response('no access token', { status: 502 })

  const userRes = await fetch('https://api.github.com/user', {
    headers: {
      'authorization': `Bearer ${token.access_token}`,
      'user-agent': 'orchid-relay/1.0',
      'accept': 'application/vnd.github+json',
    },
  })
  if (!userRes.ok) return new Response('github user fetch failed', { status: 502 })
  const gh = (await userRes.json()) as { login: string; id: number }
  const session: Session = {
    login: gh.login,
    uid: gh.id,
    subdomain: userSubdomain(gh.login),
  }

  // Ensure the user's DO has an agent token minted on first login,
  // and persist the just-issued GH access token so settings can fetch
  // the user's repo list later without re-running OAuth.
  const do_ = env.USER.get(env.USER.idFromName(session.subdomain))
  await mintAgentToken(do_, session.uid, session.login)
  if (token.access_token) {
    await do_.fetch(new Request('https://internal/_set_gh_token', {
      method: 'POST',
      body: JSON.stringify({ token: token.access_token }),
    }))
  }

  const cookie = await signSession(env.SESSION_KEY, session)
  const headers = new Headers()
  headers.set('set-cookie',
    `${COOKIE}=${cookie}; Domain=.${env.ROOT_DOMAIN}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=2592000`)
  headers.set('location', next)
  return new Response(null, { status: 302, headers })
}

export async function currentUser(env: Env, req: Request): Promise<Session | null> {
  // The cookie value is base64 + '.' + sig — base64 padding contains '='
  // characters, so we can't naively split on '='. Take everything after
  // the first '=' as the value.
  const raw = (req.headers.get('cookie') ?? '')
    .split(/;\s*/)
    .map((p) => {
      const i = p.indexOf('=')
      return i < 0 ? [p, ''] : [p.slice(0, i), p.slice(i + 1)]
    })
    .find((p) => p[0] === COOKIE)?.[1]
  if (!raw) return null
  try {
    return await verifySession(env.SESSION_KEY, raw)
  } catch {
    return null
  }
}

// ─── tiny HMAC cookie ───

async function signSession(secret: string, s: Session): Promise<string> {
  const body = btoa(JSON.stringify(s))
  const sig = await hmac(secret, body)
  return `${body}.${sig}`
}
async function verifySession(secret: string, cookie: string): Promise<Session> {
  const [body, sig] = cookie.split('.')
  if (!body || !sig) throw new Error('bad cookie')
  const expect = await hmac(secret, body)
  if (sig !== expect) throw new Error('bad sig')
  return JSON.parse(atob(body)) as Session
}
async function hmac(secret: string, data: string): Promise<string> {
  const enc = new TextEncoder()
  const key = await crypto.subtle.importKey(
    'raw', enc.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'],
  )
  const sig = await crypto.subtle.sign('HMAC', key, enc.encode(data))
  return btoa(String.fromCharCode(...new Uint8Array(sig)))
    .replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_')
}
