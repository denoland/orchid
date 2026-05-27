// Frame protocol shared by the Worker DO and the agent client. JSON text
// frames carry control; binary frames carry stream bodies with a 4-byte
// big-endian stream-id prefix.
export type Frame =
  | { t: 'req'; id: number; method: string; path: string; headers: [string, string][]; hasBody: boolean }
  | { t: 'req-end'; id: number }
  | { t: 'res-head'; id: number; status: number; headers: [string, string][]; streaming: boolean }
  | { t: 'res-end'; id: number }
  | { t: 'cancel'; id: number }
  | { t: 'hello'; server: string }
  | { t: 'pong' }
  | { t: 'ws-open'; id: number; path: string; headers: [string, string][] }
  | { t: 'ws-text'; id: number; data: string }
  | { t: 'ws-close'; id: number; code?: number; reason?: string }

export function encodeBinary(streamId: number, chunk: Uint8Array): Uint8Array {
  const out = new Uint8Array(4 + chunk.byteLength)
  new DataView(out.buffer).setUint32(0, streamId, false)
  out.set(chunk, 4)
  return out
}

export function decodeBinary(buf: ArrayBuffer): { streamId: number; chunk: Uint8Array } {
  return { streamId: new DataView(buf).getUint32(0, false), chunk: new Uint8Array(buf, 4) }
}

// RoutingMode describes how the worker maps an incoming public URL to a
// tunnel name and an inner path.
//
//   subdomain: `<tunnel>.<rootDomain>/...path`
//                                       ^ inner path stays as-is
//
//   path-prefix: `<host>/<tunnel>/...path`
//                                  ^ stripped from inner path
export type RoutingMode =
  | { mode: 'subdomain'; rootDomain: string }
  | { mode: 'path-prefix' }

export interface RelayOptions<E = any> {
  // Routing mode.
  routing: RoutingMode
  // Returns the static agent token for a given tunnel name. The token is
  // matched against ?token= / X-Agent-Token on /agent WS upgrades. Return
  // null to reject the connection.
  agentToken: (env: E, tunnel: string) => string | null | Promise<string | null>
  // Optional gate on public-side requests. Return a Response to short-
  // circuit (e.g. send a 401 / redirect). Return undefined to allow.
  publicAuth?: (req: Request, env: E, tunnel: string) => undefined | Response | Promise<undefined | Response>
  // Optional handler invoked on inbound control frames whose `t` doesn't
  // match a built-in type. Use to implement extension frames.
  onExtraFrame?: (frame: any, ctx: SessionContext) => void | Promise<void>
  // Name of the Durable Object namespace binding (default 'TUNNEL').
  doBinding?: string
}

export type Env = any

// SessionContext is handed to onExtraFrame so extensions can talk back
// to the agent and to currently-connected subscriber WSs.
export interface SessionContext {
  // Send a control frame back to the agent.
  sendToAgent: (frame: any) => void
  // Get the agent WebSocket if connected.
  agentWS: WebSocket | null
}

export interface ParsedRoute {
  tunnel: string
  innerPath: string
}

export function parseRoute(opts: RelayOptions<any>, req: Request): ParsedRoute | null {
  const url = new URL(req.url)
  if (opts.routing.mode === 'subdomain') {
    const host = url.host.toLowerCase()
    const root = opts.routing.rootDomain.toLowerCase()
    if (host === root) return null
    if (!host.endsWith('.' + root)) return null
    const sub = host.slice(0, -1 - root.length)
    if (!sub || sub.includes('.')) return null
    return { tunnel: sub, innerPath: url.pathname + url.search }
  }
  // path-prefix
  const seg = url.pathname.match(/^\/([^\/]+)(\/.*)?$/)
  if (!seg) return null
  const tunnel = decodeURIComponent(seg[1] ?? '')
  if (!tunnel) return null
  const inner = (seg[2] ?? '/') + url.search
  return { tunnel, innerPath: inner }
}
