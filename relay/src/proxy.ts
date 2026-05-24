/// HTTP proxy: take an inbound browser request on <sub>.orchid.littledivy.com/api/*
/// and forward it to that user's Durable Object, which sends it across the
/// agent's WebSocket. Response streams back end-to-end.

interface Env {
  USER: DurableObjectNamespace
}

export async function proxyToAgent(env: Env, subdomain: string, req: Request): Promise<Response> {
  const do_ = env.USER.get(env.USER.idFromName(subdomain))
  const url = new URL(req.url)
  const innerHeaders: [string, string][] = []
  req.headers.forEach((v, k) => {
    if (k.toLowerCase().startsWith('x-orchid-inner-')) return
    innerHeaders.push([k, v])
  })
  // Buffer the body before crossing the DO boundary. Streaming bodies
  // across `do_.fetch` is unreliable — the inner request would race the
  // outer Request constructor on the same stream. POST payloads to orch
  // (tmux keystrokes, capture uploads) are bounded so buffering is fine.
  let body: ArrayBuffer | null = null
  if (!['GET', 'HEAD'].includes(req.method)) {
    body = await req.arrayBuffer()
  }
  const proxyReq = new Request(`https://internal/_proxy`, {
    method: req.method,
    headers: {
      'x-orchid-inner-url': url.pathname + url.search,
      'x-orchid-inner-method': req.method,
      'x-orchid-inner-headers': btoa(JSON.stringify(innerHeaders)),
    },
    body,
  })
  return do_.fetch(proxyReq)
}
