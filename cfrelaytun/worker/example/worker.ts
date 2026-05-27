// Minimal cfrelaytun Worker. Replace AGENT_TOKEN with a per-tunnel
// secret (e.g. derived from KV) for multi-tenant deploys.
import { defineTunnelSession, makeRelayApp } from '../src'

interface Env {
  TUNNEL: DurableObjectNamespace
  AGENT_TOKEN: string
  ROOT_DOMAIN: string
}

const options = {
  // Switch to { mode: 'path-prefix' } for workers.dev / no-DNS deploys.
  routing: { mode: 'subdomain' as const, rootDomain: '' /* set per-env from ROOT_DOMAIN */ },
  agentToken: (env: Env, _tunnel: string) => env.AGENT_TOKEN,
}

export const TunnelSession = defineTunnelSession(options)

export default {
  fetch(req: Request, env: Env, ctx: ExecutionContext) {
    // ROOT_DOMAIN comes from wrangler vars; pin it onto the routing
    // config at request time since options is captured at module load.
    if (options.routing.mode === 'subdomain') {
      options.routing.rootDomain = env.ROOT_DOMAIN
    }
    return makeRelayApp(options).fetch(req, env, ctx)
  },
} satisfies ExportedHandler<Env>
