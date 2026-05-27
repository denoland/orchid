/// Per-user Durable Object. Subclasses cfrelaytun's TunnelSession for the
/// HTTP/WS proxy protocol (req/res/ws-* frames, stream multiplex, agent
/// hibernation) and layers orchid-specific behavior on top:
///   - GitHub OAuth-derived owner + allowedLogins gating
///   - /api/state push fanout to dashboard tabs (state-update frame)
///   - snap.json layout sync (snap, snap-put frames)
///   - tmux pane stream fanout (pane frame)
///   - per-tab cursor + presence broadcast on the events WS
import { defineTunnelSession } from 'cfrelaytun'

interface Env {
  USER: DurableObjectNamespace
  ROOT_DOMAIN: string
}

export function userSubdomain(login: string): string {
  return login.toLowerCase().replace(/[^a-z0-9-]/g, '')
}

// Token validation lives in the subclass (per-DO state) so the agentToken
// option here returns null — handleAgent is overridden below.
const Base = defineTunnelSession<Env>({
  // routing/rootDomain isn't used: the orchid Worker (cf/src/index.ts)
  // handles routing and calls into the DO directly.
  routing: { mode: 'subdomain', rootDomain: '' },
  agentToken: () => null,
})

export class UserSession extends Base {
  agentToken: string | null = null
  uid: number | null = null
  login: string | null = null
  ghToken: string | null = null  // user's GitHub OAuth access token; lets the dashboard fetch their repo list

  // Lowercased GitHub logins the orch operator allows in addition to the
  // owner. Pushed by the agent on connect via the "config" frame so the
  // source of truth stays in the operator's swarm.hcl. Not persisted —
  // if the agent reconnects with a new list, that's what counts.
  allowedLogins: string[] = []

  reposCache: { body: string; expires: number } | null = null
  lastState: string | null = null
  lastSnap: string | null = null
  lastSubCount = -1

  constructor(state: DurableObjectState, env: Env) {
    super(state, env)
    state.blockConcurrencyWhile(async () => {
      this.agentToken = (await state.storage.get('token')) ?? null
      this.uid = (await state.storage.get('uid')) ?? null
      this.login = (await state.storage.get('login')) ?? null
      this.ghToken = (await state.storage.get('ghToken')) ?? null
      this.lastSnap = (await state.storage.get('lastSnap')) ?? null
    })
  }

  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url)
    if (url.pathname === '/agent') return this.handleAgentOrchid(req)
    if (url.pathname === '/_mint') return this.handleMint(req)
    if (url.pathname === '/_proxy') return this.handleProxy(req)
    if (url.pathname === '/_proxy_ws') return this.handleProxyWS(req)
    if (url.pathname === '/_events_ws') return this.handleEventsWS(req)
    if (url.pathname === '/_info') return this.handleInfo()
    if (url.pathname === '/_check_access') return this.handleCheckAccess(req)
    if (url.pathname === '/_revoke') return this.handleRevoke()
    if (url.pathname === '/_set_gh_token') return this.handleSetGhToken(req)
    if (url.pathname === '/_gh_repos') return this.handleGhRepos(req)
    return new Response('not found', { status: 404 })
  }

  // Overridden agent handshake: validates against the per-DO stored token
  // (the lib's options.agentToken hook can't reach DO state) and sends an
  // orchid-flavoured hello with the resolved uid/login.
  private async handleAgentOrchid(req: Request): Promise<Response> {
    if (req.headers.get('upgrade') !== 'websocket') {
      return new Response('expected websocket', { status: 426 })
    }
    const url = new URL(req.url)
    const token = url.searchParams.get('token') ?? req.headers.get('x-agent-token')
    if (!this.agentToken || token !== this.agentToken) {
      return new Response('auth', { status: 401 })
    }

    const pair = new WebSocketPair()
    const client = pair[0]
    const server = pair[1]
    if (this.agentWS) {
      try { this.agentWS.close(4000, 'replaced by new agent') } catch {}
    }
    this.state.acceptWebSocket(server, ['agent'])
    this.agentWS = server
    server.send(JSON.stringify({ t: 'hello', userId: this.uid ?? 0, login: this.login ?? '' }))
    this.broadcastRelayInfo()
    this.lastSubCount = -1
    this.notifySubs(true)
    return new Response(null, { status: 101, webSocket: client })
  }

  // Class-level hibernation hook. Intercepts events-tagged subscriber
  // frames and orchid-specific agent extension frames before delegating
  // the HTTP/WS protocol frames to the base class.
  async webSocketMessage(ws: WebSocket, data: string | ArrayBuffer) {
    try {
      const tags = this.state.getTags(ws) ?? []
      if (tags.includes('events')) {
        return this.handleEventsFrame(ws, data)
      }
      // Agent WS: try orchid extension frames first.
      if (typeof data === 'string') {
        let f: any
        try { f = JSON.parse(data) } catch { return }
        if (this.handleAgentExtension(f)) return
      }
      // Fall through to the base lib (req/res/cancel/ws-* + binary stream).
      return super.webSocketMessage(ws, data)
    } catch {}
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string, wasClean: boolean) {
    const wasAgent = this.agentWS === ws
    await super.webSocketClose(ws, code, reason, wasClean)
    if (wasAgent) {
      this.broadcastRelayInfo()
      return
    }
    this.notifySubs()
    // events-WS departure → tell remaining peers so they can drop the
    // stale cursor from their map without waiting for GC, and tell orch
    // to refcount-down any pane captures this WS was holding open
    // (browser closes don't always send pane-unsub).
    try {
      const a = ws.deserializeAttachment() as { id?: string; panes?: string[] } | null
      const id = a?.id
      if (id) {
        const out = JSON.stringify({ t: 'leave', userId: id })
        for (const peer of this.state.getWebSockets('events')) {
          if (peer === ws) continue
          try { peer.send(out) } catch {}
        }
      }
      const panes = a?.panes ?? []
      if (panes.length > 0 && this.agentWS) {
        for (const paneId of panes) {
          try { this.agentWS.send(JSON.stringify({ t: 'pane-unsub', paneId })) } catch {}
        }
      }
    } catch {}
  }

  // ─── orchid agent extension frames ─────────────────────────────────

  // handleAgentExtension returns true if it consumed the frame.
  private handleAgentExtension(f: any): boolean {
    if (!f || typeof f.t !== 'string') return false
    switch (f.t) {
      case 'config': {
        const list = Array.isArray(f.allowed_logins) ? f.allowed_logins : []
        this.allowedLogins = list
          .map((s: any) => String(s).toLowerCase().trim())
          .filter(Boolean)
        return true
      }
      case 'pane': {
        const wrapped = `{"t":"pane","paneId":${JSON.stringify(f.paneId ?? '')},"data":${JSON.stringify(f.data ?? '')}}`
        for (const sub of this.state.getWebSockets('events')) {
          try { sub.send(wrapped) } catch {}
        }
        return true
      }
      case 'snap': {
        const body = JSON.stringify(f.snap ?? {})
        if (body !== this.lastSnap) {
          this.lastSnap = body
          this.state.storage.put('lastSnap', body).catch(() => {})
          const wrapped = `{"t":"snap","snap":${body}}`
          for (const sub of this.state.getWebSockets('events')) {
            try { sub.send(wrapped) } catch {}
          }
        }
        return true
      }
      case 'state-update': {
        const body = JSON.stringify(f.state ?? {})
        if (body === this.lastState) return true
        this.lastState = body
        const wrapped = `{"t":"state","state":${body}}`
        for (const sub of this.state.getWebSockets('events')) {
          try { sub.send(wrapped) } catch {}
        }
        return true
      }
    }
    return false
  }

  // ─── events WS (per-tab subscriber) ────────────────────────────────

  // handleEventsFrame consumes a browser-tab WS message. Frames recognized:
  //   cursor — broadcast peer cursor to other tabs
  //   pane-sub/pane-unsub — refcount pane captures via the agent
  //   snap-put — write a fresh canvas snap, fan out + forward to agent
  private handleEventsFrame(ws: WebSocket, data: string | ArrayBuffer) {
    if (typeof data !== 'string') return
    let f: any
    try { f = JSON.parse(data) } catch { return }
    if (!f) return
    if (f.t === 'cursor') {
      const id = this.wsId(ws)
      const out = JSON.stringify({ t: 'cursor', userId: id, x: f.x, y: f.y })
      for (const peer of this.state.getWebSockets('events')) {
        if (peer === ws) continue
        try { peer.send(out) } catch {}
      }
      return
    }
    if ((f.t === 'pane-sub' || f.t === 'pane-unsub') && typeof f.paneId === 'string') {
      const a = (ws.deserializeAttachment() as { id?: string; panes?: string[] }) || {}
      const set = new Set<string>(a.panes ?? [])
      if (f.t === 'pane-sub') set.add(f.paneId)
      else set.delete(f.paneId)
      try { ws.serializeAttachment({ ...a, panes: Array.from(set) }) } catch {}
      if (this.agentWS) {
        try {
          this.agentWS.send(JSON.stringify({
            t: f.t, paneId: f.paneId,
            cols: typeof f.cols === 'number' ? f.cols : undefined,
            rows: typeof f.rows === 'number' ? f.rows : undefined,
          }))
        } catch {}
      }
      return
    }
    if (f.t === 'snap-put' && f.snap !== undefined) {
      const body = JSON.stringify(f.snap ?? {})
      if (body === this.lastSnap) return
      this.lastSnap = body
      this.state.storage.put('lastSnap', body).catch(() => {})
      const wrapped = `{"t":"snap","snap":${body}}`
      for (const peer of this.state.getWebSockets('events')) {
        if (peer === ws) continue
        try { peer.send(wrapped) } catch {}
      }
      if (this.agentWS) {
        try { this.agentWS.send(JSON.stringify({ t: 'snap-put', snap: f.snap })) } catch {}
      }
      return
    }
  }

  // handleEventsWS accepts a dashboard tab WS. Receives the current
  // relay-info + last cached state + snap on open; gets state/snap/pane
  // pushes thereafter.
  private async handleEventsWS(req: Request): Promise<Response> {
    if (req.headers.get('upgrade')?.toLowerCase() !== 'websocket') {
      return new Response('expected websocket', { status: 426 })
    }
    const isOwner = req.headers.get('x-orchid-owner') === '1'
    const pair = new WebSocketPair()
    const client = pair[0]
    const server = pair[1]
    const tags = isOwner ? ['events', 'owner'] : ['events', 'collab']
    this.state.acceptWebSocket(server, tags)
    const id = crypto.randomUUID().replaceAll('-', '').slice(0, 12)
    try { server.serializeAttachment({ id }) } catch {}
    try { server.send(JSON.stringify({ t: 'hello', userId: id })) } catch {}
    this.notifySubs()
    try { server.send(JSON.stringify(this.relayInfoFrame(isOwner))) } catch {}
    if (this.lastState) {
      try { server.send(JSON.stringify({ t: 'state', state: JSON.parse(this.lastState) })) } catch {}
    }
    if (this.lastSnap) {
      try { server.send(`{"t":"snap","snap":${this.lastSnap}}`) } catch {}
    }
    return new Response(null, { status: 101, webSocket: client })
  }

  // Stable per-WS identity so peer cursors line up across messages.
  private wsId(ws: WebSocket): string {
    const a = ws.deserializeAttachment() as { id?: string } | null
    if (a?.id) return a.id
    const id = crypto.randomUUID().replaceAll('-', '').slice(0, 12)
    try { ws.serializeAttachment({ id }) } catch {}
    return id
  }

  private relayInfoFrame(isOwner: boolean) {
    return {
      t: 'relay-info',
      connected: !!this.agentWS,
      root: this.env.ROOT_DOMAIN,
      login: this.login,
      // The token grants full agent impersonation. Only the subdomain
      // owner ever gets it — collab WSs get it as null.
      token: isOwner ? this.agentToken : null,
    }
  }

  // Tell the agent how many dashboard tabs are currently subscribed
  // to the events channel. Orch uses this to skip emitting state-update
  // frames when nobody's looking — saves the WS write + the DO wake
  // on the relay side. Only sent when the count crosses 0 or > 0 so
  // we don't spam the agent on every reload.
  private notifySubs(force = false) {
    if (!this.agentWS) return
    const count = this.state.getWebSockets('events').length
    const transitioned =
      (this.lastSubCount <= 0 && count > 0) ||
      (this.lastSubCount > 0 && count === 0)
    const send = force || transitioned
    this.lastSubCount = count
    if (!send) return
    try {
      this.agentWS.send(JSON.stringify({ t: 'subs', count }))
    } catch {}
  }

  // Broadcast a relay-info frame to every subscriber. Owner and collab
  // groups get separate payloads because the token field is privileged.
  private broadcastRelayInfo() {
    const ownerFrame = JSON.stringify(this.relayInfoFrame(true))
    const collabFrame = JSON.stringify(this.relayInfoFrame(false))
    for (const ws of this.state.getWebSockets('owner')) {
      try { ws.send(ownerFrame) } catch {}
    }
    for (const ws of this.state.getWebSockets('collab')) {
      try { ws.send(collabFrame) } catch {}
    }
  }

  // ─── orchid-specific HTTP endpoints ────────────────────────────────

  private async handleSetGhToken(req: Request): Promise<Response> {
    const body = (await req.json()) as { token?: string }
    if (!body.token) return new Response('missing token', { status: 400 })
    this.ghToken = body.token
    this.reposCache = null
    await this.state.storage.put('ghToken', body.token)
    return Response.json({ ok: true })
  }

  // Owner-only repo browser. Cached in-memory for the DO isolate's
  // lifetime so the Settings dropdown doesn't fan out to api.github.com
  // on every open.
  private async handleGhRepos(_req: Request): Promise<Response> {
    if (!this.ghToken) return new Response('no gh token', { status: 412 })
    const now = Date.now()
    const ttl = 300_000 // 5 minutes
    if (this.reposCache && this.reposCache.expires > now) {
      return new Response(this.reposCache.body, {
        headers: {
          'content-type': 'application/json',
          'cache-control': 'private, max-age=60',
          'x-orchid-cache': 'hit',
        },
      })
    }
    type Repo = {
      full_name: string
      private: boolean
      description: string | null
      pushed_at: string | null
      owner: { avatar_url: string }
    }
    const out: Array<{ full_name: string; private: boolean; description: string | null; pushed_at: string | null; avatar: string }> = []
    for (let page = 1; page <= 3; page++) {
      const r = await fetch(`https://api.github.com/user/repos?per_page=100&sort=pushed&page=${page}`, {
        headers: {
          authorization: `Bearer ${this.ghToken}`,
          accept: 'application/vnd.github+json',
          'user-agent': 'orchid-relay/1.0',
        },
      })
      if (!r.ok) {
        return Response.json({ error: 'gh ' + r.status, repos: out }, { status: 200 })
      }
      const page_data = (await r.json()) as Repo[]
      for (const repo of page_data) {
        out.push({
          full_name: repo.full_name,
          private: repo.private,
          description: repo.description,
          pushed_at: repo.pushed_at,
          avatar: repo.owner.avatar_url,
        })
      }
      if (page_data.length < 100) break
    }
    const body = JSON.stringify({ repos: out })
    this.reposCache = { body, expires: now + ttl }
    return new Response(body, {
      headers: {
        'content-type': 'application/json',
        'cache-control': 'private, max-age=60',
        'x-orchid-cache': 'miss',
      },
    })
  }

  private async handleRevoke(): Promise<Response> {
    this.agentToken = null
    await this.state.storage.delete('token')
    if (this.agentWS) {
      try { this.agentWS.close(4001, 'token revoked') } catch {}
      this.agentWS = null
    }
    return Response.json({ ok: true })
  }

  private async handleCheckAccess(req: Request): Promise<Response> {
    const url = new URL(req.url)
    const login = (url.searchParams.get('login') ?? '').toLowerCase()
    const ok = login !== '' && (
      login === (this.login ?? '').toLowerCase() ||
      this.allowedLogins.includes(login)
    )
    return Response.json({ ok })
  }

  private handleInfo(): Response {
    return Response.json({
      connected: !!this.agentWS,
      token: this.agentToken,
      login: this.login,
    })
  }

  private async handleMint(req: Request): Promise<Response> {
    const body = (await req.json()) as { uid: number; login: string }
    this.uid = body.uid
    this.login = body.login
    await this.state.storage.put('uid', body.uid)
    await this.state.storage.put('login', body.login)
    if (!this.agentToken) {
      this.agentToken = crypto.randomUUID().replaceAll('-', '')
      await this.state.storage.put('token', this.agentToken)
    }
    return Response.json({ token: this.agentToken })
  }
}

// Re-exported for the OAuth callback in oauth.ts.
export async function mintAgentToken(
  do_: DurableObjectStub,
  uid: number,
  login: string,
): Promise<string> {
  const res = await do_.fetch(new Request('https://internal/_mint', {
    method: 'POST',
    body: JSON.stringify({ uid, login }),
  }))
  const j = (await res.json()) as { token: string }
  return j.token
}

