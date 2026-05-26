# Tailscale

{{illust:vine-mesh}}

{{diagram:tailnet}}

The relay (`orchid.littledivy.com`) is one way to reach your
dashboard without a public IP. **Tailscale** is the other: spin up
your own private mesh, run orch on a node, and hit it from any
device on the tailnet. No relay involvement, no third-party domain,
no inbound port forwarding.

This setup also lets the central orch reach VMs behind NAT — useful
when your "worker" is a laptop, a beefy desktop at home, or a node
in a private cloud.

## Why Tailscale

- **No public IPs anywhere.** All nodes get a `100.x.y.z` address on
  your tailnet. Connections traverse NAT via STUN; if that fails,
  Tailscale's DERP relays handle it transparently.
- **MagicDNS.** Each node becomes `<host>.<tail-net>.ts.net`. No
  bookkeeping `/etc/hosts` entries.
- **ACLs.** Lock the dashboard to specific Tailscale users / tags
  without any auth wiring on orch's side.

## Setup

**1. Install Tailscale** on the central host and each VM:

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
```

Follow the auth URL. The default policy auto-joins; restrict via
ACLs in the Tailscale admin console.

**2. Point orch at the tailnet host names** for each VM:

```hcl
vm "homedesktop" {
  host         = "orchid@homedesktop.tail-abc.ts.net"
  capacity     = 8
  session_home = "/home/orchid"
}

vm "vps-fra1" {
  host         = "orchid@vps-fra1.tail-abc.ts.net"
  capacity     = 16
  session_home = "/home/orchid"
}
```

Orch's SSH calls now traverse the tailnet — VMs can live anywhere.

**3. Bind the orch HTTP listener to the tailnet interface only:**

```hcl
orchestrator {
  http_addr   = "100.x.y.z:8000"   # the central's Tailscale IP
  http_secret = "<32-hex>"
}
```

Or use `127.0.0.1:8000` and reach the dashboard through Tailscale's
own port-forward / ssh-tunnel. Either way the open internet sees
nothing.

**4. Skip the relay if you want.** Drop the `orch join` step
entirely. The dashboard is reachable at
`http://<central>.tail-abc.ts.net:8000/?token=<http_secret>` — phone,
laptop, anywhere on the tailnet.

## Picking your topology

| What you have | Suggested setup |
|---------------|-----------------|
| One beefy box at home | Central + sessions on that box; tailscale for remote access only. |
| Home box + VPS | VPS as central (always-on); home as VM via tailscale (free compute). |
| Multiple workstations | Pick one as central; tag others with `vm` role in Tailscale ACL. |
| Pure cloud | Central + workers on cloud nodes; tailscale optional, relay does the same job. |

## Tailscale vs the relay

Both solve the "no public IP" problem. Pick by what bothers you
more:

| | Relay | Tailscale |
|---|---|---|
| **Setup** | One signup, one `orch join`. | Per-node install + login. |
| **Cost** | Free for now. | Free for up to 3 users / 100 devices. |
| **Trust boundary** | `orchid.littledivy.com` operator. | Tailscale + your ACL config. |
| **Cross-device chat** | Tunnels everything over CF Workers. | Direct WireGuard mesh. |
| **VM ↔ central** | Still goes over SSH (you pick reachability). | Same SSH, but tailnet eliminates NAT. |

You can run both. The relay handles the dashboard; the tailnet
handles VM connectivity.

## SSH key plumbing

`orch join vm` adds the worker's pubkey to the central's
authorized_keys. With tailscale + tailscale-ssh you can skip the
key dance entirely: `host = "orchid@homedesktop"` and let
tailscale-ssh issue short-lived certs. See Tailscale's SSH docs.

## See also

- [VMs](/docs/vms) — adding worker VMs.
- [Security](/docs/security) — clawpatrol + auth posture.
