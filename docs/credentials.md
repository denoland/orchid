# {{illust:config-knot}} Credentials

Each worker runs a coding agent (`claude` or `codex`) that needs to be logged in.
Doing that by hand on every box — and re-doing it whenever you add a VM — is the
tax orchid removes. Agent auth comes from a **pluggable credential provider**;
the default keeps the creds in orch and writes them onto each VM at spawn, so
adding a machine never means logging in again.

## Per-account model

Creds are keyed by **account**, not by agent. A swarm runs several accounts of
the same provider (e.g. a `codex` and a `codex-mini` login), and different
machines can hold different creds of the same provider. Each VM's `account`
field (defaults to its `agent`) selects which cred it gets; the agent type
decides the file layout and where on the box it's written:

| Agent | Auth lives under | Files |
|-------|------------------|-------|
| `claude` | the session home | `.claude/.credentials.json`, `.claude.json` |
| `codex`  | `CODEX_HOME` (per VM) | `auth.json`, `config.toml` |

So two machines with different claude logins are just two accounts
(`account = "claude"` and `account = "claude-2"`), each with its own stored cred.

## Providers

Selected by `orchestrator { credentials { provider = "…" } }` (default `local`).

### `local` (default)

orch keeps each account's auth files in its own store (sqlite) and writes them
onto a VM at spawn — **only when absent**, so a CLI's freshly-refreshed token is
never clobbered. A new VM is auto-provisioned; existing VMs are untouched.

### `clawpatrol` (provider plugin)

[clawpatrol](https://clawpatrol.dev) is a firewall gateway that holds the OAuth
centrally and **injects it into the agent's traffic at the wire** — the secret
never touches the worker. It never exports a token, so it can't be a
"copy-the-token" backend; instead its provider would ensure the VM is joined to
the gateway. Added as a credential-provider plugin (one `registerCredProvider`
call) — no scheduler changes.

## Adding an account

Connecting is done out-of-band (the dashboard is status-only). On the
orchestrator host:

```bash
# claude account — read ~/.claude
orch creds import <account> --agent claude --from /home/orchid

# codex account — read its CODEX_HOME
orch creds import <account> --agent codex --from /home/orchid/.codex
```

`--from` is the agent's auth base (the home dir for claude, the `CODEX_HOME` dir
for codex). Then point a VM at it with `account = "<account>"` in its `vm` block.

## Dashboard

The **Connections** tab shows each account and whether orch has creds for it
(`connected` / `not connected`), alongside the GitHub connection. It's
status-only by design — credentials are managed by the provider, not pasted into
a browser.

## Configuration

```hcl
orchestrator {
  # …
  credentials {
    provider = "local"   # default; or a provider plugin like "clawpatrol"
  }
}
```
