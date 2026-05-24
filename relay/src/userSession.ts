/// Per-user Durable Object. Holds the agent's outbound WebSocket and routes
/// requests/responses across it.
import type { Frame } from './frames'
import { decodeBinary, encodeBinary } from './frames'

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
  // Lowercased GitHub logins the orch operator allows in addition to the
  // owner. Pushed by the agent on connect via the "config" frame so the
  // source of truth stays in the operator's swarm.hcl. Not persisted —
  // if the agent reconnects with a new list, that's what counts.
  allowedLogins: string[] = []
  nextStreamId = 1
  streams = new Map<number, PendingStream>()

  constructor(state: DurableObjectState, env: Env) {
    this.state = state
    this.env = env
    state.blockConcurrencyWhile(async () => {
      this.agentToken = (await state.storage.get('token')) ?? null
      this.uid = (await state.storage.get('uid')) ?? null
      this.login = (await state.storage.get('login')) ?? null
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
    return new Response('not found', { status: 404 })
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
    server.accept()
    if (this.agentWS) {
      try { this.agentWS.close(4000, 'replaced by new agent') } catch {}
    }
    this.agentWS = server
    server.addEventListener('message', (ev) => this.onAgentMessage(ev))
    server.addEventListener('close', () => {
      if (this.agentWS === server) this.agentWS = null
      for (const s of this.streams.values()) {
        s.res.reject(new Error('agent disconnected'))
        try { s.body?.controller.close() } catch {}
        try { s.ws?.close(1011, 'agent disconnected') } catch {}
      }
      this.streams.clear()
    })
    server.send(JSON.stringify({ t: 'hello', userId: this.uid ?? 0, login: this.login ?? '' } satisfies Frame))
    return new Response(null, { status: 101, webSocket: client })
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

/// Helper: the proxy worker turns the inbound Request into a stripped-down
/// inner Request that the DO sends across the tunnel. Used by proxy.ts.
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
