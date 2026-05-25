/// Per-user Durable Object. Holds the agent's outbound WebSocket and routes
/// requests/responses across it. Frame protocol is inlined below — every
/// frame either carries JSON (control) or a 4-byte big-endian stream-id
/// prefix followed by a raw body chunk (binary).
type Frame =
  | { t: 'req'; id: number; method: string; path: string; headers: [string, string][]; hasBody: boolean }
  | { t: 'req-end'; id: number }
  | { t: 'res-head'; id: number; status: number; headers: [string, string][]; streaming: boolean }
  | { t: 'res-end'; id: number }
  | { t: 'cancel'; id: number }
  | { t: 'hello'; userId: number; login: string }
  | { t: 'pong' }
  | { t: 'ws-open'; id: number; path: string; headers: [string, string][] }
  | { t: 'ws-text'; id: number; data: string }
  | { t: 'ws-close'; id: number; code?: number; reason?: string }

function encodeBinary(streamId: number, chunk: Uint8Array): Uint8Array {
  const out = new Uint8Array(4 + chunk.byteLength)
  new DataView(out.buffer).setUint32(0, streamId, false)
  out.set(chunk, 4)
  return out
}
function decodeBinary(buf: ArrayBuffer): { streamId: number; chunk: Uint8Array } {
  return { streamId: new DataView(buf).getUint32(0, false), chunk: new Uint8Array(buf, 4) }
}

interface Env {
  USER: DurableObjectNamespace
}

interface PendingStream {
  res: { resolve: (r: Response) => void; reject: (e: Error) => void }
  status?: number
  headers?: Headers
  body?: { controller: ReadableStreamDefaultController<Uint8Array>; stream: ReadableStream<Uint8Array> }
  streaming?: boolean
  ws?: WebSocket // browser-side WS for ws-multiplexed streams
}

export function userSubdomain(login: string): string {
  return login.toLowerCase().replace(/[^a-z0-9-]/g, '')
}

export class UserSession {
  state: DurableObjectState
  env: Env
  agentWS: WebSocket | null = null
  agentToken: string | null = null
  uid: number | null = null
  login: string | null = null
  ghToken: string | null = null  // user's GitHub OAuth access token; lets the dashboard fetch their repo list
  // Lowercased GitHub logins the orch operator allows in addition to the
  // owner. Pushed by the agent on connect via the "config" frame so the
  // source of truth stays in the operator's swarm.hcl. Not persisted —
  // if the agent reconnects with a new list, that's what counts.
  allowedLogins: string[] = []
  nextStreamId = 1
  streams = new Map<number, PendingStream>()
  // In-memory snapshot of the user's GitHub repo list. Cached for the DO
  // isolate's lifetime up to TTL — the Settings page is opened rarely,
  // but when it is it pulls 3 pages × 100 repos from api.github.com every
  // open. A 5-minute TTL keeps the dropdown fresh without re-fanning out.
  reposCache: { body: string; expires: number } | null = null

  constructor(state: DurableObjectState, env: Env) {
    this.state = state
    this.env = env
    state.blockConcurrencyWhile(async () => {
      this.agentToken = (await state.storage.get('token')) ?? null
      this.uid = (await state.storage.get('uid')) ?? null
      this.login = (await state.storage.get('login')) ?? null
      this.ghToken = (await state.storage.get('ghToken')) ?? null
      // Resurrect the agent WS that Cloudflare held open across DO
      // eviction. Without this, every cold start reports
      // `connected: false` until the agent's next reconnect — which
      // can be minutes away if the original socket is still alive.
      const existing = state.getWebSockets('agent')
      if (existing.length > 0) {
        this.agentWS = existing[0] as unknown as WebSocket
      }
    })
  }

  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url)
    if (url.pathname === '/agent') return this.handleAgent(req)
    if (url.pathname === '/_mint') return this.handleMint(req)
    if (url.pathname === '/_proxy') return this.handleProxy(req)
    if (url.pathname === '/_proxy_ws') return this.handleProxyWS(req)
    if (url.pathname === '/_info') return this.handleInfo()
    if (url.pathname === '/_check_access') return this.handleCheckAccess(req)
    if (url.pathname === '/_revoke') return this.handleRevoke()
    if (url.pathname === '/_set_gh_token') return this.handleSetGhToken(req)
    if (url.pathname === '/_gh_repos') return this.handleGhRepos(req)
    return new Response('not found', { status: 404 })
  }

  // Stash the just-completed-OAuth user's access token. Used later by
  // the Settings page to list their repos for the inbox / target
  // pickers without re-running the OAuth dance.
  private async handleSetGhToken(req: Request): Promise<Response> {
    const body = (await req.json()) as { token?: string }
    if (!body.token) return new Response('missing token', { status: 400 })
    this.ghToken = body.token
    // Different token may see a different repo set (private repos in
    // particular). Drop the cache so the next /repos call re-fetches.
    this.reposCache = null
    await this.state.storage.put('ghToken', body.token)
    return Response.json({ ok: true })
  }

  // Owner-only repo browser. Cached in-memory for the DO isolate's
  // lifetime (TTL below) so the Settings dropdown doesn't fan out to
  // api.github.com on every open. The response also carries a private
  // Cache-Control so the browser doesn't refetch on tab switches.
  // `q` filters client-side; full list is paginated up to ~300 repos.
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
        // Don't poison the cache with partial / error pages.
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

  // Owner-only: wipe the current agent token and force a fresh mint on
  // the next OAuth sign-in. Closes any live agent connection so the old
  // token can't reconnect even if it stayed in someone's clipboard.
  private async handleRevoke(): Promise<Response> {
    this.agentToken = null
    await this.state.storage.delete('token')
    if (this.agentWS) {
      try { this.agentWS.close(4001, 'token revoked') } catch {}
      this.agentWS = null
    }
    return Response.json({ ok: true })
  }

  // Owner-or-allowed check. Called from the relay middleware before
  // tunnelling subdomain requests. The allowedLogins list comes from the
  // operator's swarm.hcl via the agent connect frame.
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

  private async handleAgent(req: Request): Promise<Response> {
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
    // Tag the WS so the constructor can find it again via
    // state.getWebSockets('agent') after CF evicts this DO instance.
    // The class-level webSocketMessage/webSocketClose hooks below
    // replace the per-instance addEventListener pattern.
    this.state.acceptWebSocket(server, ['agent'])
    this.agentWS = server
    server.send(JSON.stringify({ t: 'hello', userId: this.uid ?? 0, login: this.login ?? '' } satisfies Frame))
    return new Response(null, { status: 101, webSocket: client })
  }

  // Class-level hibernation hooks. CF calls these for any WS the DO
  // accepted via state.acceptWebSocket. Routes everything through the
  // same handler the old addEventListener path used.
  async webSocketMessage(ws: WebSocket, data: string | ArrayBuffer) {
    if (this.agentWS == null) this.agentWS = ws
    this.onAgentMessage({ data } as MessageEvent)
  }
  async webSocketClose(ws: WebSocket, _code: number, _reason: string, _wasClean: boolean) {
    if (this.agentWS === ws) this.agentWS = null
    for (const s of this.streams.values()) {
      s.res.reject(new Error('agent disconnected'))
      try { s.body?.controller.close() } catch {}
      try { s.ws?.close(1011, 'agent disconnected') } catch {}
    }
    this.streams.clear()
  }
  async webSocketError(ws: WebSocket, _err: unknown) {
    if (this.agentWS === ws) this.agentWS = null
  }

  private async handleProxy(req: Request): Promise<Response> {
    // Brief grace period for the agent to (re)connect — the most common
    // 503 happens right after an orch deploy. Wait up to ~3s instead of
    // failing immediately so the dashboard doesn't flash error toasts on
    // every restart.
    if (!this.agentWS) {
      await this.waitForAgent(3000)
    }
    if (!this.agentWS) return new Response('agent offline', { status: 503 })
    try {
      const innerReq = await innerRequestFromProxyBody(req)
      return await this.send(innerReq)
    } catch (e) {
      return new Response('proxy error: ' + (e as Error).message, { status: 502 })
    }
  }

  private async waitForAgent(ms: number): Promise<void> {
    const deadline = Date.now() + ms
    while (!this.agentWS && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 100))
    }
  }

  private async handleProxyWS(req: Request): Promise<Response> {
    if (!this.agentWS) return new Response('agent offline', { status: 503 })
    const innerPath = req.headers.get('x-orchid-inner-url') ?? '/'
    const innerHeaders = parseHeaderBlockList(req.headers.get('x-orchid-inner-headers') ?? '')

    const pair = new WebSocketPair()
    const client = pair[0]
    const server = pair[1]
    server.accept()

    const id = this.nextStreamId++
    this.streams.set(id, { res: { resolve: () => {}, reject: () => {} }, ws: server })

    this.agentWS.send(JSON.stringify({
      t: 'ws-open', id, path: innerPath, headers: innerHeaders,
    } satisfies Frame))

    server.addEventListener('message', (ev) => {
      if (!this.agentWS) return
      if (typeof ev.data === 'string') {
        this.agentWS.send(JSON.stringify({ t: 'ws-text', id, data: ev.data } satisfies Frame))
      } else {
        this.agentWS.send(encodeBinary(id, new Uint8Array(ev.data as ArrayBuffer)))
      }
    })
    server.addEventListener('close', (ev) => {
      if (this.agentWS) {
        try {
          this.agentWS.send(JSON.stringify({ t: 'ws-close', id, code: ev.code, reason: ev.reason } satisfies Frame))
        } catch {}
      }
      this.streams.delete(id)
    })

    return new Response(null, { status: 101, webSocket: client })
  }

  private async send(req: Request): Promise<Response> {
    const id = this.nextStreamId++
    const headerList: [string, string][] = []
    req.headers.forEach((v, k) => headerList.push([k, v]))
    const body = req.body
    const hasBody = !!body

    const promise = new Promise<Response>((resolve, reject) => {
      this.streams.set(id, { res: { resolve, reject } })
    })

    this.agentWS!.send(JSON.stringify({
      t: 'req', id, method: req.method, path: new URL(req.url).pathname + new URL(req.url).search,
      headers: headerList, hasBody,
    } satisfies Frame))

    if (hasBody && body) {
      const reader = body.getReader()
      while (true) {
        const { value, done } = await reader.read()
        if (done) break
        if (value) this.agentWS!.send(encodeBinary(id, value))
      }
    }
    this.agentWS!.send(JSON.stringify({ t: 'req-end', id } satisfies Frame))

    return promise
  }

  private onAgentMessage(ev: MessageEvent) {
    const data = ev.data
    if (typeof data === 'string') {
      let f: any
      try { f = JSON.parse(data) } catch { return }
      // Operator-defined allowlist pushed at connect time. Source of
      // truth lives in swarm.hcl — we just cache the latest snapshot.
      if (f.t === 'config') {
        const list = Array.isArray(f.allowed_logins) ? f.allowed_logins : []
        this.allowedLogins = list
          .map((s: any) => String(s).toLowerCase().trim())
          .filter(Boolean)
        return
      }
      const id = (f as any).id as number | undefined
      const s = id !== undefined ? this.streams.get(id) : undefined
      if (f.t === 'res-head' && s) {
        s.status = f.status
        s.headers = new Headers(f.headers)
        s.streaming = f.streaming
        let bodyController!: ReadableStreamDefaultController<Uint8Array>
        const stream = new ReadableStream<Uint8Array>({
          start: (controller) => { bodyController = controller },
        })
        s.body = { controller: bodyController, stream }
        // Statuses 1xx, 204, 205, 304 must have null body — passing a
        // stream into the Response constructor with these statuses throws.
        const nullBodyStatus = s.status === 101 || s.status === 204 ||
          s.status === 205 || s.status === 304 || (s.status >= 100 && s.status < 200)
        s.res.resolve(new Response(nullBodyStatus ? null : stream, { status: s.status, headers: s.headers }))
      } else if (f.t === 'res-end' && s) {
        try { s.body?.controller.close() } catch {}
        this.streams.delete(f.id)
      } else if (f.t === 'cancel' && s) {
        try { s.body?.controller.error(new Error('cancelled')) } catch {}
        this.streams.delete(f.id)
      } else if (f.t === 'ws-text' && s?.ws) {
        try { s.ws.send(f.data) } catch {}
      } else if (f.t === 'ws-close' && s?.ws) {
        try { s.ws.close(f.code ?? 1000, f.reason ?? '') } catch {}
        this.streams.delete(f.id)
      }
    } else if (data instanceof ArrayBuffer) {
      const { streamId, chunk } = decodeBinary(data)
      const s = this.streams.get(streamId)
      if (s?.ws) {
        try { s.ws.send(chunk) } catch {}
      } else if (s?.body) {
        try { s.body.controller.enqueue(chunk) } catch {}
      }
    }
  }
}

/// Mint or fetch an agent token for a user. Called from the OAuth callback.
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

// Reconstructs the original Request from the sidecar headers added by the
// outer proxy worker before forwarding to the DO.
async function innerRequestFromProxyBody(req: Request): Promise<Request> {
  const inner = req.headers.get('x-orchid-inner-url')
  if (!inner) throw new Error('missing inner url')
  // Workers Request constructor requires an absolute URL. Use a sentinel
  // host; only the pathname+search is forwarded across the tunnel.
  return new Request('https://agent.internal' + inner, {
    method: req.headers.get('x-orchid-inner-method') ?? 'GET',
    headers: parseHeaderBlock(req.headers.get('x-orchid-inner-headers') ?? ''),
    body: req.body,
  })
}
function parseHeaderBlock(b: string): Headers {
  const h = new Headers()
  try {
    const arr = JSON.parse(atob(b)) as [string, string][]
    for (const [k, v] of arr) h.append(k, v)
  } catch {}
  return h
}
function parseHeaderBlockList(b: string): [string, string][] {
  try {
    return JSON.parse(atob(b)) as [string, string][]
  } catch {
    return []
  }
}
