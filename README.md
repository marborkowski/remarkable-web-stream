# reMarkable Live

**Watch your reMarkable screen live in any web browser, from anywhere, behind a password. Everything runs on the tablet itself.**

Open a URL on any computer or phone, type the password, and see what you write and draw on the tablet in real time. Viewers can even draw back onto the tablet. You need a computer only to deploy it. After that, the tablet runs the whole thing as three systemd services.

> [!WARNING]
> **This is an experimental hobby project.** It modifies a device you rely on, runs as `root`, and makes your tablet's screen reachable from the public internet. Read [Security considerations](#security-considerations) before you use it, and use it at your own risk. It is provided **as is, without any warranty**. The author is not liable for any damage, data loss or security incident caused by using it (see [Disclaimer](#disclaimer) and [LICENSE](LICENSE)).

> [!IMPORTANT]
> **This is not a broadcasting or mass-sharing tool.** The server runs *on the tablet itself*, so its CPU, Wi-Fi and battery serve every viewer. At most **3 viewers** can watch at once, and a 4th one disconnects the others. It is meant for showing your screen to yourself or a few people, for example in a call, a tutoring session or a small meeting. See [Intended use and limitations](#intended-use-and-limitations).

---

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Choosing a tunnel](#choosing-a-tunnel)
- [Finding the current URL](#finding-the-current-url)
- [Using the viewer](#using-the-viewer)
- [Configuration](#configuration)
- [Operating the services](#operating-the-services)
- [Troubleshooting](#troubleshooting)
- [Intended use and limitations](#intended-use-and-limitations)
- [Security considerations](#security-considerations)
- [Project layout](#project-layout)
- [Development](#development)
- [Credits](#credits)
- [Disclaimer](#disclaimer)
- [License](#license)

## Features

- **Live screen in the browser.** The tablet's framebuffer is streamed with WebGL rendering and delta-encoded frames, courtesy of [goMarkableStream](https://github.com/owulveryck/goMarkableStream).
- **Password protection.** A small Go reverse proxy (`rmshare`) handles the login form, session cookies and brute-force throttling.
- **Reachable from anywhere.** You can use [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) (a stable URL, free, no domain needed), a Cloudflare quick tunnel (no account at all), or a Cloudflare named tunnel (your own domain).
- **Draw from the browser.** You can draw on the tablet with a mouse, finger or stylus. Strokes are injected as real pen events, so they use the tool currently selected on the tablet.
- **Auto-rotate.** The preview follows the tablet's orientation within a fraction of a second.
- **Sleep-aware.** When the tablet's display goes to sleep, streams are cut off and the page shows "The tablet is asleep". It recovers by itself on wake-up.
- **"Someone is watching" signal.** The front light blinks twice when a viewer connects and once, longer, when the last viewer leaves.
- **URL on the tablet.** With a quick tunnel the URL changes on restart. The proxy can put a pinned PDF with the current URL and a QR code into your library.
- **Several viewers at once** (up to 3 concurrent streams).
- **Self-healing tunnel.** A watchdog replaces a Cloudflare quick tunnel that died during a long offline period.

## How it works

```
browser ──HTTPS──▶ tunnel (Tailscale Funnel / Cloudflare) ──▶ tunnel client on the reMarkable
                                                                   │ http://127.0.0.1:8080
                                                                   ▼
                                     rmshare (Go proxy: login form, session cookie,
                                              sleep/orientation watcher, remote pen,
                                              viewer signal, URL document)
                                                                   │ http://127.0.0.1:2011
                                                                   ▼
                                     goMarkableStream (patched; reads the framebuffer
                                                       from xochitl's memory, serves the
                                                       frame stream and the web UI)
```

| Service | Binary | Role |
| --- | --- | --- |
| `rmshare-stream` | `gomarkablestream` | Upstream [goMarkableStream](https://github.com/owulveryck/goMarkableStream) built from source with [our patches](gms/). Bound to `127.0.0.1` only, with its own authentication disabled, so it can be reached only through the proxy. |
| `rmshare-proxy` | `rmshare` ([`proxy/`](proxy/)) | Password gate and reverse proxy on `:8080`, plus all the extras: sleep and orientation detection from the xochitl journal, the remote pen WebSocket, the front-light signal and the URL document. |
| `rmshare-tunnel` | `cloudflared` or `tailscaled` | Publishes the proxy to the internet ([`device/run-tunnel.sh`](device/run-tunnel.sh)). |

For the details of each feature and of each patch, see [docs/how-it-works.md](docs/how-it-works.md).

## Requirements

**Tablet**
- A **reMarkable Paper Pro** (tested on firmware 6.0.106). The **reMarkable 2** should work (goMarkableStream supports it and the build targets 32-bit ARM), but it has **not been tested**. Remote drawing and the front-light signal in particular assume Paper Pro devices and value ranges.
- **Developer mode** enabled, with SSH access as `root` (over USB at `10.11.99.1`, or over Wi-Fi).

**Computer used for deployment** (macOS or Linux)
- [Go](https://go.dev/dl/) 1.26 or newer (required to build goMarkableStream v1.4.0), plus `curl`, `patch`, `tar` and `ssh`.
- An SSH key installed on the tablet (one-time setup). The root password is shown on the tablet under *Settings → Help → Copyrights and licenses* (GPLv3 section):
  ```bash
  ssh-copy-id root@10.11.99.1
  ```

## Quick start

1. **Clone this repository** and create your configuration:
   ```bash
   cp tunnel.env.example tunnel.env
   ```
2. **Set a strong password** in `tunnel.env`:
   ```bash
   RMSHARE_PASSWORD=<long random string, e.g. from: openssl rand -base64 18>
   ```
   Optionally pick a tunnel mode (see [Choosing a tunnel](#choosing-a-tunnel)). If you set nothing else, a Cloudflare quick tunnel is used.
3. **Connect the tablet over USB and deploy:**
   ```bash
   ./deploy.sh                    # or: ./deploy.sh root@<tablet-ip>
   ```
   The script detects the tablet's architecture and builds the proxy. It builds goMarkableStream from source with the patches, then downloads cloudflared (and Tailscale in Funnel mode). Finally it uploads everything to `/home/root/rmshare/` and enables the services. At the end it prints the URL:
   ```
   URL: https://something-random.trycloudflare.com
   ```
4. **Open the URL, enter the password** and watch.

Downloaded and built artifacts are cached in `dist/`. Delete `dist/` (or the relevant files in it) to force a fresh build or download.

## Choosing a tunnel

| Mode | URL | Account needed | Notes |
| --- | --- | --- | --- |
| **Tailscale Funnel** (recommended) | stable `https://<name>.<tailnet>.ts.net` | free Tailscale account | No domain needed. TLS terminates on the tablet. |
| **Cloudflare quick tunnel** (default) | random `*.trycloudflare.com`, changes on every restart | none | Easiest way to try it out. The URL is published to the tablet (see below). |
| **Cloudflare named tunnel** | your own hostname | Cloudflare account and a domain | Stable. You can add Cloudflare Access in front of it as a second login layer. |

You can find setup steps for each mode, and a comparison with alternatives such as ngrok or plain Tailscale, in **[docs/tunnels.md](docs/tunnels.md)**.

## Finding the current URL

A quick-tunnel URL changes whenever the tunnel restarts. You can always find the current one in any of these places:

1. **On the tablet.** A pinned document called **"reMarkable Live – URL"** (in Favourites) shows the URL and a QR code. To make the tablet notice the new document, the proxy restarts `xochitl` (about 10 s). Set `PUBLISH_DOC=0` in `tunnel.env` to disable this. The password is never printed in that document.
2. **In a browser on the local network.** Open `http://10.11.99.1:8080/url` (USB) or `http://<tablet-ip>:8080/url`, log in, and you get the URL and a QR code. Add `?format=txt` for plain text.
3. **Over SSH:**
   ```bash
   ssh root@10.11.99.1 cat /home/root/rmshare/tunnel-url.txt
   ```

## Using the viewer

The viewer is goMarkableStream's web UI with a few additions in the left-hand menu:

| Button | Key | What it does |
| --- | --- | --- |
| **Auto-rotate** | `A` | Follow the tablet's orientation (on by default; the choice is remembered). A manual **Rotate** (`R`) turns auto-rotate off. |
| **Draw** | `D` | Turn the preview into a drawing surface. Strokes go to the tablet with a fixed 50% pressure and the tool currently selected on the tablet (pen, marker, eraser, …). Only one person can draw at a time. |

Other behaviour to know about:
- **Sleep.** While the tablet sleeps, the page shows *"The tablet is asleep"* and checks every 5 s. When the tablet cannot be reached, it shows *"Cannot reach the tablet"* and keeps retrying indefinitely.
- **Viewer signal.** The front light blinks **twice** about 2 s after the first viewer connects. It blinks **once, longer** about 15 s after the last viewer leaves. With a bright light the blink dims it; with a dim or off light the blink brightens it. Your brightness is always restored.
- **Log out** by visiting `/logout`.

## Configuration

Most settings live in **`tunnel.env`** (see [`tunnel.env.example`](tunnel.env.example)):

| Variable | Default | Meaning |
| --- | --- | --- |
| `RMSHARE_PASSWORD` | *(required)* | Viewer password, at least 8 characters. |
| `TUNNEL` | *(unset)* | Set to `tailscale` for Tailscale Funnel. |
| `TS_HOSTNAME` | `remarkable` | Tailscale machine name, which becomes part of the URL. |
| `TS_AUTHKEY` | *(unset)* | Optional Tailscale auth key for unattended login. |
| `TUNNEL_TOKEN` | *(unset)* | Cloudflare named tunnel token. |
| `PUBLIC_URL` | *(unset)* | URL shown on `/url` and in the tablet document for named tunnels. |
| `PUBLISH_DOC` | `1` | Publish the URL document to the tablet's library (restarts xochitl). |
| `TUNNEL_DEAD_AFTER` | `120` | Seconds before the watchdog restarts a stuck Cloudflare tunnel. |

The proxy itself has command-line flags. Change them in [`device/rmshare-proxy.service`](device/rmshare-proxy.service):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `:8080` | Listen address. |
| `-upstream` | `http://127.0.0.1:2011` | goMarkableStream address. |
| `-password` | `$RMSHARE_PASSWORD` | Viewer password. |
| `-urlfile` | `/home/root/rmshare/tunnel-url.txt` | File holding the current public URL. |
| `-doc` | `false` | Publish the URL document (the unit passes `${PUBLISH_DOC}`). |
| `-xochitl-dir` | `/home/root/.local/share/remarkable/xochitl` | xochitl document directory. |
| `-sleep-watch` | `true` | Follow sleep and orientation in the xochitl journal. |
| `-pen-device` | `/dev/input/event2` | Pen input device for remote drawing (`""` disables it). |
| `-notify-light` | `/sys/class/backlight/rm_frontlight` | Front light for the viewer signal (`""` disables it). |

The component versions are pinned in [`deploy.sh`](deploy.sh). You can override them with environment variables: `GMS_VERSION` (goMarkableStream, default `v1.4.0`), `CF_VERSION` (cloudflared) and `TS_VERSION` (Tailscale). The patches target goMarkableStream `v1.4.0`, so other versions may need patch updates.

## Operating the services

```bash
ssh root@10.11.99.1
systemctl status rmshare-stream rmshare-proxy rmshare-tunnel
journalctl -u rmshare-stream -u rmshare-proxy -u rmshare-tunnel -f
wget -qO- http://127.0.0.1:8080/healthz      # "ok" when the proxy is up
sh /home/root/rmshare/uninstall.sh            # stop and remove the services
rm -rf /home/root/rmshare                     # ...and delete all files
```

The viewer also works locally without any tunnel, still behind the password: `http://10.11.99.1:8080` (USB) or `http://<tablet-ip>:8080` (Wi-Fi).

**Starting on boot.** On the Paper Pro, `/etc` is an overlay whose writable layer lives in RAM, so anything written to `/etc/systemd/system` disappears on reboot. `install.sh` therefore also writes the three unit files to the root partition itself. It remounts `/` read-write just for that and switches it back to read-only afterwards. `uninstall.sh` removes them again.

**After a reMarkable software update.** An update switches to the other root partition, so the services are gone. The files in `/home/root` survive. Re-install the services with:

```bash
ssh root@10.11.99.1 'sh /home/root/rmshare/install.sh'     # or simply re-run ./deploy.sh
```

## Troubleshooting

| Symptom | Likely cause and fix |
| --- | --- |
| `deploy.sh` stops with *"set RMSHARE_PASSWORD"* | Add `RMSHARE_PASSWORD=...` (8+ characters) to `tunnel.env`. |
| **502 Bad Gateway** after logging in | goMarkableStream is not running. Check `journalctl -u rmshare-stream -n 30`. |
| No URL printed after deploy | Check `journalctl -u rmshare-tunnel -n 50`. In Tailscale mode, open the printed login and Funnel links. |
| Tailscale URL gives *"server not found"* | The public DNS record appears only after Funnel is enabled, and it can take several minutes to propagate. The first request can also take a few seconds while the certificate is issued. |
| The page says *"The tablet is asleep"* | It is. Wake the tablet up and the picture comes back by itself. |
| Choppy picture through the tunnel | Compare it with the local `http://10.11.99.1:8080`. If local is smooth, the tunnel is the bottleneck; if local is just as choppy, the tablet's Wi-Fi or frame encoding is. |
| Login says *"Too many failed attempts"* | 20 failed logins (from anyone) within 15 minutes lock logging in until the window passes. Restarting `rmshare-proxy` also clears the lock. |

## Intended use and limitations

reMarkable Live is a **personal screen-sharing tool for a handful of viewers**. It is not a streaming service. There is no server or CDN in the middle that could fan the stream out to many people: every viewer gets a separate stream straight from the tablet.

- **Hard limit of 3 concurrent viewers.** The patched goMarkableStream admits at most 3 streams at once (`maxWriters` in [`gms/stream_preempt.patch`](gms/stream_preempt.patch)). A 4th viewer does not wait in a queue. It **disconnects the existing viewers** and takes over, so sharing the link widely makes the stream unusable for everyone.
- **Every viewer costs the tablet resources.** Frames are read from xochitl's memory and delta-encoded **separately for each connection**, on the tablet's CPU, right next to the UI you are writing in. More viewers mean more CPU load, a less responsive tablet and faster battery drain. Even with a single viewer, idle refresh alone takes about 15–20 % of one CPU core.
- **Bandwidth comes from the tablet's Wi-Fi.** Each viewer's stream is uploaded separately over the tablet's wireless connection, so total traffic grows with the number of viewers. The tunnel provider only relays it.
- **Availability depends on the tablet.** When the tablet sleeps, loses Wi-Fi, runs out of battery or restarts xochitl, every viewer loses the picture. There is no recording, buffering or replay.
- **No per-viewer access control.** Everyone shares one password, and anyone who has it can also draw on the tablet (see [Security considerations](#security-considerations)).

If you need to present to a larger audience, share your screen to a single viewer (for example your own computer) and broadcast that computer's screen with a video-conferencing or streaming tool.

## Security considerations

This project puts a live view of your tablet on the internet. Please understand what that means:

- **The password is the only barrier.** Anyone who has it can watch **everything** on your screen, including any document you open, and can **draw into your notes** through the remote pen. Use a long random password and do not reuse it. Change it whenever you have shared it with someone who should no longer have access. (Remote drawing can be disabled with `-pen-device=`.)
- **There is a single shared password,** with no user accounts. Sessions are kept in memory for 30 days and cannot be revoked one by one. Restarting `rmshare-proxy` logs everyone out.
- **Brute-force protection is global.** After 20 failed attempts in 15 minutes, logins are blocked for **everyone**, including you. This slows down password guessing, but it also lets anyone who knows the URL lock legitimate viewers out for a while.
- **The URL is not a secret.** Tailscale Funnel hostnames get public TLS certificates, which are recorded in public Certificate Transparency logs, so the address can be discovered. Quick-tunnel URLs are random but not secret either. Do not rely on obscurity.
- **The local listener uses plain HTTP.** The proxy listens on `:8080` on every interface of the tablet (USB and Wi-Fi) without TLS. On a shared or untrusted Wi-Fi network, someone else on that network can intercept the password and session cookie. Only the tunnels provide HTTPS.
- **Everything runs as `root`.** This includes the proxy, goMarkableStream, cloudflared and tailscaled. The proxy needs root to read xochitl's memory, inject pen events, control the front light and restart xochitl. A bug in any of these components could therefore compromise the whole device.
- **goMarkableStream runs with its own authentication disabled** (`-unsafe`, `RK_JWT_ENABLED=false`). It is bound to `127.0.0.1`, so only processes on the tablet can reach it directly.
- **The proxy changes your tablet.** With `PUBLISH_DOC=1`, it writes a document into your xochitl library and **restarts xochitl**, which could interrupt whatever you are doing. Remote drawing writes real strokes into the open document.
- **Secrets are stored in plain text** in `/home/root/rmshare/tunnel.env` on the device (mode `600`). That includes the password, Tailscale auth keys and Cloudflare tokens. They are also on your computer in `tunnel.env`, which is git-ignored. Never commit it.
- **Downloads are not checksum-verified.** `deploy.sh` downloads cloudflared, Tailscale and the goMarkableStream source over HTTPS from their official locations, but it does not verify signatures or checksums.
- **Client IPs in the logs can be spoofed.** They come from `CF-Connecting-IP` / `X-Forwarded-For` and are used only for logging.
- **The system partition is modified.** To start on boot, `install.sh` remounts the read-only root partition read-write and adds three systemd unit files to it (nothing else is changed). `uninstall.sh` removes them.
- **Developer mode** and root SSH access lower the tablet's own security. That is a prerequisite of this project, not something it adds.

If you find a vulnerability, please report it as described in [SECURITY.md](SECURITY.md).

## Project layout

```
proxy/                    rmshare, the password-protected proxy (Go)
  main.go, login.html       login form, sessions, reverse proxy
  sleep.go                  xochitl journal watcher: sleep state, orientation, /state long-poll
  pen.go                    remote pen: /pen WebSocket -> evdev events
  viewers.go                viewer counting and front-light signal
  urldoc.go                 /url page and PDF with URL + QR code for the tablet library
gms/                      goMarkableStream build script and patches
  build-gms.sh              downloads the upstream source, applies the patches, builds
  series                    patch order
  *.patch                   see docs/how-it-works.md#gomarkablestream-patches
device/                   files installed on the tablet
  rmshare-*.service         systemd units: stream, proxy, tunnel
  run-tunnel.sh             tunnel mode selection (Tailscale / Cloudflare) and watchdog
  install.sh, uninstall.sh  service installation and removal (run on the tablet)
  rootfs.sh                 helpers to persist the units on the root partition
tools/pentool/            pen diagnostics: axis ranges, event dump, test stroke
docs/                     detailed documentation
deploy.sh                 build + download + upload + install (run on your computer)
tunnel.env.example        configuration template
dist/                     build and download cache (git-ignored)
```

## Development

```bash
make test        # go vet + unit tests for the proxy
make build       # cross-compile the proxy for arm and arm64 into dist/
make gms         # build patched goMarkableStream for arm64 into dist/
make deploy      # same as ./deploy.sh
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines, including how to update the goMarkableStream patches.

## Credits

- [goMarkableStream](https://github.com/owulveryck/goMarkableStream) by Olivier Wulveryck (MIT) does all the heavy lifting of reading and streaming the screen. This project only patches it and wraps it.
- [cloudflared](https://github.com/cloudflare/cloudflared), [Tailscale](https://tailscale.com) and [go-qrcode](https://github.com/skip2/go-qrcode).

*reMarkable is a trademark of reMarkable AS. This project is not affiliated with, endorsed by or supported by reMarkable AS. Using developer mode and third-party software on your tablet may affect your warranty and support options.*

## Disclaimer

This software is experimental and provided **"as is", without warranty of any kind**, express or implied. That includes, without limitation, any warranties of merchantability, fitness for a particular purpose and non-infringement. **You use it entirely at your own risk.** In no event shall the author or contributors be liable for any claim, damages or other liability arising from the software or its use. That includes, among other things, a bricked or malfunctioning device, lost or corrupted notes, exposure of your screen content, or any other security incident. Read the code and the [Security considerations](#security-considerations) before you install it.

## License

[MIT](LICENSE). The patches in [`gms/`](gms/) modify goMarkableStream, which is also MIT-licensed (Copyright © 2023 Olivier Wulveryck).
