// TunnelSession Durable Object. Holds the agent's outbound WebSocket and
// routes HTTP/WS requests across it using the frame protocol defined in
// types.ts.
import {
  type Env,
  type Frame,
  type RelayOptions,
  type SessionContext,
  encodeBinary,
  decodeBinary,
} from './types'

interface PendingStream {
  res: { resolve: (r: Response) => void; reject: (e: Error) => void }
  status?: number
  headers?: Headers
  body?: { controller: ReadableStreamDefaultController<Uint8Array>; stream: ReadableStream<Uint8Array> }
  streaming?: boolean
  ws?: WebSocket
}

// Internal endpoints the makeRelayApp router uses to talk to its DO.
export const INTERNAL_PATHS = {
  agent: '/__cfrelaytun/agent',
  proxy: '/__cfrelaytun/proxy',
  proxyWS: '/__cfrelaytun/proxy_ws',
  info: '/__cfrelaytun/info',
} as const

// Header name carrying the tunnel identity into the DO. The DO doesn't
// know its own name via the platform; the router stamps it on every
// internal request.
export const TUNNEL_HEADER = 'x-cfrt-tunnel'
export const INNER_URL_HEADER = 'x-cfrt-inner-url'
export const INNER_METHOD_HEADER = 'x-cfrt-inner-method'
export const INNER_HEADERS_HEADER = 'x-cfrt-inner-headers'

// defineTunnelSession returns a Durable Object class bound to the given
// options. Export the result from your Worker module so wrangler can
// instantiate it.
export function defineTunnelSession<E = any>(options: RelayOptions<E>) {
  return class TunnelSession {
    state: DurableObjectState
    env: Env
    agentWS: WebSocket | null = null
    nextStreamId = 1
    streams = new Map<number, PendingStream>()

    constructor(state: DurableObjectState, env: Env) {
      this.state = state
      this.env = env
      // Resurrect the agent WS that Cloudflare held open across DO
      // eviction so the first cold-start proxy doesn't see a stale
      // "agent offline".
      const existing = state.getWebSockets('agent')
      if (existing.length > 0) {
        this.agentWS = existing[0] as unknown as WebSocket
      }
    }

    async fetch(req: Request): Promise<Response> {
      const url = new URL(req.url)
      const tunnel = req.headers.get(TUNNEL_HEADER) ?? ''
      if (url.pathname === INTERNAL_PATHS.agent) return this.handleAgent(req, tunnel)
      if (url.pathname === INTERNAL_PATHS.proxy) return this.handleProxy(req)
      if (url.pathname === INTERNAL_PATHS.proxyWS) return this.handleProxyWS(req)
      if (url.pathname === INTERNAL_PATHS.info) {
        return Response.json({ connected: !!this.agentWS })
      }
      return new Response('not found', { status: 404 })
    }

    async handleAgent(req: Request, tunnel: string): Promise<Response> {
      if (req.headers.get('upgrade') !== 'websocket') {
        return new Response('expected websocket', { status: 426 })
      }
      const url = new URL(req.url)
      const token = url.searchParams.get('token') ?? req.headers.get('x-agent-token')
      const expected = await options.agentToken(this.env, tunnel)
      if (!expected || token !== expected) {
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
      server.send(JSON.stringify({ t: 'hello', server: 'cfrelaytun/0.1' } satisfies Frame))
      return new Response(null, { status: 101, webSocket: client })
    }

    async webSocketMessage(ws: WebSocket, data: string | ArrayBuffer) {
      try {
        if (this.agentWS == null) this.agentWS = ws
        this.onAgentMessage(ws, data)
      } catch {}
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

    async handleProxy(req: Request): Promise<Response> {
      if (!this.agentWS) await this.waitForAgent(3000)
      if (!this.agentWS) return new Response('agent offline', { status: 503 })
      try {
        const innerReq = await innerRequestFromProxyBody(req)
        return await this.send(innerReq)
      } catch (e) {
        return new Response('proxy error: ' + (e as Error).message, { status: 502 })
      }
    }

    async waitForAgent(ms: number): Promise<void> {
      const deadline = Date.now() + ms
      while (!this.agentWS && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 100))
      }
    }

    async handleProxyWS(req: Request): Promise<Response> {
      if (!this.agentWS) return new Response('agent offline', { status: 503 })
      const innerPath = req.headers.get(INNER_URL_HEADER) ?? '/'
      const innerHeaders = parseHeaderBlockList(req.headers.get(INNER_HEADERS_HEADER) ?? '')

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

    async send(req: Request): Promise<Response> {
      const id = this.nextStreamId++
      const headerList: [string, string][] = []
      req.headers.forEach((v, k) => headerList.push([k, v]))
      const body = req.body
      const hasBody = !!body

      const promise = new Promise<Response>((resolve, reject) => {
        this.streams.set(id, { res: { resolve, reject } })
      })

      const url = new URL(req.url)
      this.agentWS!.send(JSON.stringify({
        t: 'req', id, method: req.method, path: url.pathname + url.search,
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

    onAgentMessage(_ws: WebSocket, data: string | ArrayBuffer) {
      if (typeof data === 'string') {
        let f: any
        try { f = JSON.parse(data) } catch { return }
        const id = (f?.id) as number | undefined
        const s = id !== undefined ? this.streams.get(id) : undefined
        if (f?.t === 'res-head' && s) {
          const status = f.status as number
          s.status = status
          s.headers = new Headers(f.headers)
          s.streaming = f.streaming
          let bodyController!: ReadableStreamDefaultController<Uint8Array>
          const stream = new ReadableStream<Uint8Array>({
            start: (controller) => { bodyController = controller },
          })
          s.body = { controller: bodyController, stream }
          const nullBody = status === 101 || status === 204 ||
            status === 205 || status === 304 || (status >= 100 && status < 200)
          s.res.resolve(new Response(nullBody ? null : stream, { status, headers: s.headers }))
        } else if (f?.t === 'res-end' && s) {
          try { s.body?.controller.close() } catch {}
          this.streams.delete(f.id)
        } else if (f?.t === 'cancel' && s) {
          try { s.body?.controller.error(new Error('cancelled')) } catch {}
          this.streams.delete(f.id)
        } else if (f?.t === 'ws-text' && s?.ws) {
          try { s.ws.send(f.data) } catch {}
        } else if (f?.t === 'ws-close' && s?.ws) {
          try { s.ws.close(f.code ?? 1000, f.reason ?? '') } catch {}
          this.streams.delete(f.id)
        } else if (f?.t && options.onExtraFrame) {
          const ctx: SessionContext = {
            sendToAgent: (frame: any) => {
              try { this.agentWS?.send(JSON.stringify(frame)) } catch {}
            },
            agentWS: this.agentWS,
          }
          void options.onExtraFrame(f, ctx)
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
}

async function innerRequestFromProxyBody(req: Request): Promise<Request> {
  const inner = req.headers.get(INNER_URL_HEADER)
  if (!inner) throw new Error('missing inner url')
  return new Request('https://agent.internal' + inner, {
    method: req.headers.get(INNER_METHOD_HEADER) ?? 'GET',
    headers: parseHeaderBlock(req.headers.get(INNER_HEADERS_HEADER) ?? ''),
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
