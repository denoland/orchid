# Workers

The central orch host runs the orchestrator daemon plus, by default,
one or more Claude sessions in tmux. To scale, add **worker VMs** —
extra machines that run tmux + claude only and accept SSH from the
central host.

## How a worker joins

The central orch generates an invite token, prints the join command,
and waits. On the worker, you run the worker installer + `orch join
vm`, which:

1. Posts a public SSH key + worker hostname to the central's
   `/api/vm/join` endpoint.
2. Central appends a fresh `vm "<name>" {}` block to its `swarm.hcl`,
   adds the worker's pubkey to its authorized_keys, and starts using
   the worker on the next poll tick.

No reverse tunnels, no public IP on the worker — only the central
needs an address. Connectivity goes central → worker over SSH.

## Step-by-step

**On the central** (run from the dashboard or by hand):

```bash
orch invite-vm
# → orch join vm wss://<sub>.orchid.littledivy.com/agent <invite-token>
```

**On the worker VM** (a fresh Linux box, your user account):

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | WORKER=1 bash
orch join vm wss://<sub>.orchid.littledivy.com/agent <invite-token>
```

The worker installer skips the swarm.hcl + service-unit steps. It just
puts the `orch` binary on the worker and prepares it to receive
sessions from the central.

## VM block fields

After `join`, the central's `swarm.hcl` has:

```hcl
vm "worker-fra1" {
  host         = "orchid@worker.fra1.example.com"
  capacity     = 10
  session_cmd  = "runuser -u orchid -- claude --dangerously-skip-permissions"
  session_home = "/home/orchid"
  workdir_root = "/home/orchid/orch-work"
}
```

| Field | Meaning |
|-------|---------|
| `host` | `user@host` for SSH. `localhost` for in-process VMs. |
| `capacity` | Max concurrent claude sessions on this VM. |
| `session_cmd` | Command tmux runs per session. |
| `session_home` | Working directory for the session user. |
| `workdir_root` | Where issue clones land. Defaults to the orchestrator's. |
| `bot_login`, `bot_email` | Per-VM git identity override. |
| `bot_github_key` | Path to an SSH key the bot uses to push. |

Per-VM `bootstrap_prompt` is allowed if you want a worker to receive a
slightly different system prompt (e.g. specialised codebase).

## Removing a worker

Delete the `vm` block in `swarm.hcl` (or via Settings → Workers in the
dashboard). On next config reload, orch stops dispatching to it. Tmux
sessions already running on that worker survive until they finish or
you kill them manually.
