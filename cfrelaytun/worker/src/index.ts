// cfrelaytun — Worker-side router + Durable Object glue. Public entry.
import { Hono } from 'hono'
import type { RelayOptions } from './types'
import { parseRoute } from './types'
import {
  defineTunnelSession,
  INTERNAL_PATHS,
  TUNNEL_HEADER,
  INNER_URL_HEADER,
  INNER_METHOD_HEADER,
  INNER_HEADERS_HEADER,
} from './session'

export { defineTunnelSession } from './session'
export {
  INNER_URL_HEADER,
  INNER_METHOD_HEADER,
  INNER_HEADERS_HEADER,
} from './session'
export type { Env, RelayOptions, RoutingMode, SessionContext, Frame } from './types'

// makeRelayApp returns a Hono app that handles agent connects, public
// HTTP/WS proxying, and a small status endpoint. Mount it from your
// Worker's default export.
export function makeRelayApp<E extends Record<string, any> = Record<string, any>>(
  options: RelayOptions<E>,
): Hono<{ Bindings: E }> {
  const app = new Hono<{ Bindings: E }>()
  const doBinding = options.doBinding ?? 'TUNNEL'

  const getDO = (env: E, tunnel: string): DurableObjectStub => {
    const ns = (env as any)[doBinding] as DurableObjectNamespace
    if (!ns) throw new Error(`cfrelaytun: missing DO binding '${doBinding}' on env`)
    return ns.get(ns.idFromName(tunnel))
  }

  // Catch-all that dispatches public requests to the right tunnel.
  app.all('*', async (c) => {
    const route = parseRoute(options, c.req.raw)
    if (!route) return c.text('not found', 404)
    const { tunnel, innerPath } = route

    const url = new URL(c.req.url)

    // Agent WS connect.
    if (innerPath.split('?')[0] === '/agent' || url.pathname === '/agent') {
      const do_ = getDO(c.env, tunnel)
      const internal = new Request(
        new URL(INTERNAL_PATHS.agent + url.search, url),
        c.req.raw,
      )
      internal.headers.set(TUNNEL_HEADER, tunnel)
      return do_.fetch(internal)
    }

    // Optional public-side auth gate.
    if (options.publicAuth) {
      const gate = await options.publicAuth(c.req.raw, c.env, tunnel)
      if (gate) return gate
    }

    // WS upgrade → proxy_ws DO endpoint.
    if (c.req.raw.headers.get('upgrade')?.toLowerCase() === 'websocket') {
      const do_ = getDO(c.env, tunnel)
      const innerHeaders: [string, string][] = []
      c.req.raw.headers.forEach((v, k) => innerHeaders.push([k, v]))
      const proxyReq = new Request(new URL(INTERNAL_PATHS.proxyWS, url), {
        method: 'GET',
        headers: {
          'upgrade': 'websocket',
          [TUNNEL_HEADER]: tunnel,
          [INNER_URL_HEADER]: innerPath,
          [INNER_HEADERS_HEADER]: btoa(JSON.stringify(innerHeaders)),
        },
      })
      return do_.fetch(proxyReq)
    }

    // HTTP → proxy DO endpoint.
    const do_ = getDO(c.env, tunnel)
    const innerHeaders: [string, string][] = []
    c.req.raw.headers.forEach((v, k) => {
      if (!k.toLowerCase().startsWith('x-cfrt-')) innerHeaders.push([k, v])
    })
    const body = ['GET', 'HEAD'].includes(c.req.raw.method) ? null : await c.req.raw.arrayBuffer()
    return do_.fetch(new Request(new URL(INTERNAL_PATHS.proxy, url), {
      method: c.req.raw.method,
      headers: {
        [TUNNEL_HEADER]: tunnel,
        [INNER_URL_HEADER]: innerPath,
        [INNER_METHOD_HEADER]: c.req.raw.method,
        [INNER_HEADERS_HEADER]: btoa(JSON.stringify(innerHeaders)),
      },
      body,
    }))
  })

  return app
}
