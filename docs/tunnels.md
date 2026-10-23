# Tunnels

The proxy listens on `:8080` on the tablet. To reach it from outside your local network, `rmshare-tunnel` runs [`device/run-tunnel.sh`](../device/run-tunnel.sh). That script picks one of the modes below, checked in this order:

1. **Tailscale Funnel**: `TUNNEL=tailscale` in `tunnel.env`.
2. **Cloudflare named tunnel (token)**: `TUNNEL_TOKEN=...` in `tunnel.env`.
3. **Cloudflare named tunnel (config file)**: `cloudflared/config.yml` plus a credentials file in the project root.
4. **Cloudflare quick tunnel**: the fallback when nothing above is configured.

Whatever the mode, the current public URL is written to `/home/root/rmshare/tunnel-url.txt`. The proxy picks it up from there for the `/url` page and the URL document on the tablet.

## Tailscale Funnel (recommended)

This gives you a stable `https://<name>.<tailnet>.ts.net` URL for free, with no domain needed. Traffic goes through Tailscale's relays, and TLS terminates on the tablet.

1. Create a free account at [tailscale.com](https://tailscale.com) (you can sign in with Google, GitHub or e-mail).
2. In `tunnel.env`:
   ```
   TUNNEL=tailscale
   TS_HOSTNAME=remarkable
   ```
   Optionally add `TS_AUTHKEY=tskey-auth-...` (admin console → *Settings → Keys → Generate auth key*, tick *Reusable*). The tablet then logs in by itself. Without a key, the deploy prints a login link.
3. Run `./deploy.sh`. It downloads the Tailscale binaries and uploads them. `tailscaled` runs in userspace-networking mode, because the reMarkable has no `/dev/net/tun`. At the end it prints:
   - `LOG THE DEVICE INTO TAILSCALE: https://login.tailscale.com/a/...`: open it and approve the device.
   - `ENABLE FUNNEL IN TAILSCALE: https://login.tailscale.com/f/funnel?...`: open it and approve. This adds the `funnel` attribute to your tailnet policy (one-time). Also enable **DNS → HTTPS Certificates** in the admin console if it is not on yet.
4. After a moment you get `URL: https://remarkable.<tailnet>.ts.net`. It is also published to the tablet document.
   Tailscale creates the public DNS record only once Funnel is enabled, and propagation takes from a few minutes to a quarter of an hour. Until then, browsers report "server not found". The first request can also take a few seconds while the Let's Encrypt certificate is issued.

**Implementation notes** (`run_tailscale` in `run-tunnel.sh`):
- `tailscaled` gets `--statedir`. Without it, it has nowhere to keep certificates, and Funnel fails with "no TailscaleVarRoot".
- `tailscale up` is run only once, because every new `up` invalidates the previous login link. The login link is read from `tailscale status` instead.
- `tailscale funnel` is called with a timeout, because it waits forever when Funnel is not enabled in the tailnet.

**Checking the state over SSH:**
```bash
/home/root/rmshare/tailscale --socket=/home/root/rmshare/tailscale-state/tailscaled.sock status
```
The login and Funnel links are also kept in `/home/root/rmshare/tailscale-login-url.txt` and `tailscale-funnel-url.txt` for as long as they are needed.

## Cloudflare quick tunnel (default, no account)

If nothing is configured, cloudflared opens a quick tunnel with a random `https://*.trycloudflare.com` URL. The URL changes every time the tunnel restarts, so see [Finding the current URL](../README.md#finding-the-current-url).

### Watchdog

After a long time without a network connection (for example, the tablet suspended on battery for several hours), Cloudflare deletes the quick tunnel. cloudflared then keeps trying to register it forever (`Unauthorized: Tunnel not found`) instead of creating a new one.

`run-tunnel.sh` detects this in two ways. It watches the log, and it polls cloudflared's `/ready` endpoint: a 503 for `TUNNEL_DEAD_AFTER` seconds (default 120) while the internet is reachable counts as a dead tunnel. In that case it deletes the old URL, kills cloudflared and exits. systemd then starts a fresh tunnel, and the new URL reaches the tablet document.

## Cloudflare named tunnel (account + domain)

This gives you a stable URL on your own domain.

1. In [Cloudflare Zero Trust](https://one.dash.cloudflare.com), go to *Networks → Tunnels → Create a tunnel* (type *Cloudflared*) and name it, for example `remarkable`.
2. Copy the token from the install command (the part after `--token`).
3. Under **Public Hostname**, add for example `rm.example.com` with service `HTTP` and URL `localhost:8080`.
4. In `tunnel.env`:
   ```
   TUNNEL_TOKEN=eyJhIjoi...
   PUBLIC_URL=https://rm.example.com     # optional: shown on /url and in the tablet document
   ```
5. Run `./deploy.sh`.

Alternatively, put your own `config.yml` and credentials file (from `cloudflared tunnel create`) into a `cloudflared/` directory in the project root. `run-tunnel.sh` uses them when no token is set.

**Tip:** you can put [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/) (for example, e-mail one-time PINs) in front of the hostname. You then have two layers: Cloudflare Access and the proxy's own login (password or invite link).

## Comparison with alternatives

| Option | Verdict |
| --- | --- |
| **Tailscale Funnel** | Implemented. Free, stable `*.ts.net` URL, no domain needed. |
| **Cloudflare quick tunnel** | Implemented. No account needed, but the URL changes on restart and the tunnel can be deleted after long offline periods (handled by the watchdog). |
| **Cloudflare named tunnel** | Implemented. Same infrastructure and performance as the quick tunnel, but a stable URL and no deletion problem. Needs an account and a domain. |
| **Tailscale without Funnel** (viewer's computer joins the tailnet) | Best performance (direct WireGuard connection, nothing in between) and a stable address. But viewers need Tailscale installed, so you cannot open the preview from just any computer. Not wired up as a separate mode; untested. |
| **ngrok (free plan)** | Unsuitable for screen streaming. There is a 1 GB/month outbound transfer limit (a few hours of watching), and the free plan shows an interstitial page before each browser session. |

**Before switching tunnels to fix choppiness,** compare the local preview (`http://10.11.99.1:8080` over USB) with the tunnel. If local is smooth and the tunnel is not, the intermediary is to blame. If both are similar, the tablet's Wi-Fi or the frame encoding is the limit.

The frame stream goes through every tunnel as a plain chunked HTTP response, so it works on free plans. On long idle periods, browsers may drop the SSE connection used for pen events, and the client reconnects by itself.
