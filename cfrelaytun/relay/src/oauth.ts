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
// Session cookie lifetime. 30 days matches the GitHub OAuth refresh
// window — re-prompting the user before then offers no real benefit.
const SESSION_MAX_AGE_SECONDS = 30 * 24 * 60 * 60
// Short-lived cookie that binds an in-flight OAuth state to the browser
// that started the flow. Defends against login-CSRF: an attacker who
// initiates /login can obtain a state value, but without the matching
// `orchid_oauth_state` cookie on the victim's browser, the callback will
// not accept a stolen authorization code submitted from the victim's
// session. Cleared by handleOAuthCallback on completion.
const STATE_COOKIE = 'orchid_oauth_state'

function callbackURL(req: Request): string {
  // Use the request's origin so localhost dev round-trips through the
  // same host; prod requests come in on ROOT_DOMAIN already.
  const u = new URL(req.url)
  return `${u.protocol}//${u.host}/oauth/callback`
}

// Reject `next=` redirects that aren't same-origin paths. Without this,
// attackers can craft /login?next=//evil.com/ to bounce a freshly
// authenticated user off to a phishing site that mimics the relay.
function safeNext(raw: string | null): string {
  if (!raw || !raw.startsWith('/') || raw.startsWith('//')) return '/dashboard'
  for (let i = 0; i < raw.length; i++) {
    const c = raw.charCodeAt(i)
    if (c < 0x20 || c === 0x7f) return '/dashboard'
  }
  return raw
}

// Constant-time equality for two equal-length strings. Returns false when
// the lengths differ so length-based timing leaks are absorbed by the
// caller (we compare equal-length hex/base64 tokens in practice).
function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false
  let diff = 0
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i)
  return diff === 0
}

export async function handleLogin(env: Env, req: Request): Promise<Response> {
  const state = crypto.randomUUID()
  const next = safeNext(new URL(req.url).searchParams.get('next'))
  await env.OAUTH.put(`state:${state}`, JSON.stringify({ next }), { expirationTtl: 600 })

  const callback = callbackURL(req)
  const url = new URL('https://github.com/login/oauth/authorize')
  url.searchParams.set('client_id', env.GH_CLIENT_ID)
  url.searchParams.set('redirect_uri', callback)
  url.searchParams.set('scope', 'read:user user:email')
  url.searchParams.set('state', state)
  const headers = new Headers()
  headers.set('set-cookie',
    `${STATE_COOKIE}=${state}; Domain=.${env.ROOT_DOMAIN}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=600`)
  headers.set('location', url.toString())
  return new Response(null, { status: 302, headers })
}

export async function handleOAuthCallback(env: Env, req: Request): Promise<Response> {
  const url = new URL(req.url)
  const code = url.searchParams.get('code')
  const state = url.searchParams.get('state')
  if (!code || !state) return new Response('missing code/state', { status: 400 })

  // Bind the OAuth state to the browser that began the flow. Without
  // this, an attacker can prime KV with a state and trick the victim
  // into completing the callback, ending up signed in as the attacker's
  // GitHub account (login-CSRF). With the cookie binding, the callback
  // only completes for the original requester.
  const cookieState = readCookie(req, STATE_COOKIE)
  if (!cookieState || !constantTimeEqual(cookieState, state)) {
    return new Response('state mismatch', { status: 400 })
  }

  const stateRow = await env.OAUTH.get(`state:${state}`)
  if (!stateRow) return new Response('invalid state', { status: 400 })
  await env.OAUTH.delete(`state:${state}`)
  const { next: rawNext } = JSON.parse(stateRow) as { next: string }
  const next = safeNext(rawNext)

  // Exchange code → access token.
  const tokenRes = await fetch('https://github.com/login/oauth/access_token', {
    method: 'POST',
    headers: { 'accept': 'application/json', 'content-type': 'application/json' },
    body: JSON.stringify({
      client_id: env.GH_CLIENT_ID,
      client_secret: env.GH_CLIENT_SECRET,
      code,
      redirect_uri: callbackURL(req),
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
  headers.append('set-cookie',
    `${COOKIE}=${cookie}; Domain=.${env.ROOT_DOMAIN}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=${SESSION_MAX_AGE_SECONDS}`)
  // Burn the one-shot state cookie — it's no longer useful and would
  // linger as an idle target for the next CSRF attempt.
  headers.append('set-cookie',
    `${STATE_COOKIE}=; Domain=.${env.ROOT_DOMAIN}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0`)
  headers.set('location', next)
  return new Response(null, { status: 302, headers })
}

function readCookie(req: Request, name: string): string | null {
  // The cookie value is base64 + '.' + sig — base64 padding contains '='
  // characters, so we can't naively split on '='. Take everything after
  // the first '=' as the value.
  const raw = (req.headers.get('cookie') ?? '')
    .split(/;\s*/)
    .map((p) => {
      const i = p.indexOf('=')
      return i < 0 ? [p, ''] : [p.slice(0, i), p.slice(i + 1)]
    })
    .find((p) => p[0] === name)?.[1]
  return raw ?? null
}

export async function currentUser(env: Env, req: Request): Promise<Session | null> {
  const raw = readCookie(req, COOKIE)
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
  // Constant-time compare to defeat signature-forgery probes that rely
  // on the early-exit timing of `!==` to learn the expected sig
  // byte-by-byte.
  if (!constantTimeEqual(sig, expect)) throw new Error('bad sig')
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
