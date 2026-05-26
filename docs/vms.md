# {{illust:vm-cluster}} VMs

A **VM** in orchid is anything that runs `tmux` + `claude` sessions
on orch's behalf. The central host counts as one (its localhost
block), and you can add as many remote VMs as you have capacity for.
Orch drives all of them over plain SSH — no agent, no daemon, no
inbound ports on the worker.

## When to add VMs

- **Capacity.** One VM holds ~30 concurrent Claude sessions at the
  default 16-core / 32 GB tier. Past that, RAM pressure starts
  flapping panes.
- **Isolation.** Per-project VMs let you give each target a clean
  filesystem + git identity without sharing state.
- **Locality.** Run a VM near a slow CI runner so git clone, build,
  and test loops feel snappy.

## Adding a VM

The central orch generates an invite token; the new VM runs the
installer + `orch join vm`. The pair handshakes over the relay:

**Central** — copy the join command from Settings → VMs → "Add VM",
or generate one by hand:

```bash
orch invite-vm
# prints:
# orch join vm wss://<sub>.orchid.littledivy.com/agent <invite-token>
```

**VM (fresh Linux box, your user)** — paste both lines:

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | WORKER=1 bash
orch join vm wss://<sub>.orchid.littledivy.com/agent <invite-token>
```

What happens under the hood:

1. The VM generates a fresh `ed25519` keypair for the orchid bot
   user.
2. It posts `{ pubkey, hostname, user }` to the central's
   `/api/vm/join`.
3. Central appends `vm "<name>" {}` to its `swarm.hcl`, adds the
   pubkey to the bot user's `~/.ssh/authorized_keys`, and the next
   poll tick starts dispatching to the VM.

No reverse tunnel. No public IP on the VM. Only the central needs
network presence.

## VM block reference

```hcl
vm "fra1" {
  host         = "orchid@worker.fra1.example.com"
  capacity     = 10
  session_cmd  = "runuser -u orchid -- claude --dangerously-skip-permissions"
  session_home = "/home/orchid"
  workdir_root = "/home/orchid/orch-work"

  # optional per-VM overrides:
  # bot_login       = "fra1bot"
  # bot_email       = "fra1bot@users.noreply.github.com"
  # bot_github_key  = "/home/orchid/.ssh/id_ed25519"
  # bootstrap_prompt = "..."
}
```

| Field | Default | Meaning |
|-------|---------|---------|
| `host` | (required) | `user@host` for SSH. `localhost` runs in-process. |
| `capacity` | 0 (unlimited) | Max concurrent sessions on this VM. |
| `session_cmd` | claude default | Command tmux runs per pane. |
| `session_home` | `/home/orchid` | Working dir + `$HOME` for the session user. |
| `workdir_root` | orch global | Where per-issue clones land. |
| `bot_login` | orch global | GitHub login Claude commits as. |
| `bot_email` | derived | Commit email. |
| `bot_github_key` | inherited | SSH key the bot uses to push. |
| `bootstrap_prompt` | orch global | Per-VM system prompt override. |

## Bootstrap

On first dispatch, orch SSHs in once and runs `bootstrapVM`:

- ensures `tmux` and `claude` are on `$PATH`
- ensures the `orchid` user has its onboarding flags pre-set so
  Claude doesn't stall on the trust dialog
- checks `gh auth status` works (HTTPS or SSH)

If bootstrap fails, the VM stays in the swarm as "unhealthy" and
orch keeps retrying every poll tick. The dashboard shows the last
error.

## Removing a VM

Delete its block from `swarm.hcl` (or click **Remove** in Settings).
On the next config reload, orch stops dispatching. Sessions already
in-flight on that VM finish on their own; orch won't kill them.

## See also

- [Security](/docs/security) — sandboxing what Claude can run.
- [Tailscale](/docs/tailscale) — VMs with no public IP at all.
