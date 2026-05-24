# orchid-relay

Cloudflare Worker that hosts orchid.littledivy.com:

- **Landing page** at `https://orchid.littledivy.com/`
- **GitHub OAuth** at `/login` → `/oauth/callback`
- **Per-user subdomain**: `<gh-login>.orchid.littledivy.com` serves the dashboard SPA + tunnels `/api/*` to the user's self-hosted `orch` instance
- **Agent tunnel**: orch dials `wss://<sub>.orchid.littledivy.com/agent?token=…` outbound; relay multiplexes HTTP/SSE/WS over that single WebSocket via a per-user Durable Object

## Dev

```bash
cd relay
npm install
cp ../www/dist public/dash       # bundle the React dashboard into the worker
npx wrangler kv namespace create OAUTH     # update id in wrangler.toml
npx wrangler secret put GH_CLIENT_ID
npx wrangler secret put GH_CLIENT_SECRET
npx wrangler secret put SESSION_KEY
npm run dev
```

## Deploy

```bash
npm run deploy
# point DNS for orchid.littledivy.com + *.orchid.littledivy.com at the worker (Cloudflare for SaaS)
```

## Local end-to-end test

```bash
# terminal 1 — relay
cd relay && npm run dev      # http://127.0.0.1:8787

# terminal 2 — orch dialing the local relay
ORCHID_AGENT_TOKEN=$(curl -s http://127.0.0.1:8787/_dev/mint?sub=local)  # TODO _dev endpoint
./orch -config swarm.hcl \
       -relay ws://local.orchid.littledivy.com:8787/agent \
       -relay-token "$ORCHID_AGENT_TOKEN"

# terminal 3 — browser
open http://local.orchid.littledivy.com:8787/
```

(Add `local.orchid.littledivy.com 127.0.0.1` to `/etc/hosts` for the subdomain to resolve locally.)
