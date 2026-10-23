# How it works

This document describes the moving parts in more detail than the [README](../README.md).

## Components

- **goMarkableStream** ([upstream](https://github.com/owulveryck/goMarkableStream)) reads the screen from the memory of `xochitl` (the reMarkable UI process) and serves a page with a WebGL preview, plus a delta-encoded frame stream at `/stream`. It runs only on `127.0.0.1:2011`, with its own login disabled (`-unsafe`, `RK_JWT_ENABLED=false`). It is built from source with the patches described [below](#gomarkablestream-patches).
- **rmshare** ([`proxy/`](../proxy/)) sits in front of it on `:8080`. Without a valid session cookie it shows the login form, or the join page for an invite link. After login it forwards everything, including the long-lived `/stream` response and the `/events` SSE feed (`FlushInterval: -1`). It also implements the features below.
- **cloudflared / tailscaled** publish the proxy (see [tunnels.md](tunnels.md)).

## Proxy endpoints

| Path | Auth | Description |
| --- | --- | --- |
| `/login` | — | Login form (GET) and password check (POST): opens an **owner** session. |
| `/join/<token>` | — | Join page for an invite link (GET); `POST /join` opens a **guest** session. |
| `/logout` | — | Revokes the session. |
| `/invites` | owner | Invitations page: create and revoke links, list and end sessions. POST actions need the per-session CSRF token. |
| `/healthz` | — | Returns `ok`. |
| `/state` | ✔ | JSON `{"awake", "since", "orientation", "version", "viewers", "role", "canDraw"}`. With `?wait=<version>` it long-polls for up to 25 s until the state changes. |
| `/pen` | owner | WebSocket for remote drawing (same-origin only). |
| `/url`, `/url/qr.png` | ✔ | Current public URL and its QR code. `/url?format=txt` returns plain text. |
| everything else | ✔ | Proxied to goMarkableStream. `/stream`, `/screenshot`, `/events`, `/gestures` and `/raw` return **503** while the tablet sleeps. |

## Roles and invite links

- **Owner:** signs in with `RMSHARE_PASSWORD`. Watches, draws and manages `/invites`. Sessions last 30 days.
- **Guest:** opens an invite link. Watches only. The session lasts exactly as long as the invitation.

An invitation is a 192-bit random token in a link (`/join/<token>`). The proxy stores only its SHA-256 hash, a label, the expiry and the number of sessions it opened, so the link can be shown only once, at creation time. Opening the link shows a page with a *Join* button; only the POST creates the session, so link previews and prefetchers do not open sessions. An invalid or expired link counts as a failed attempt for the lockout, like a wrong password.

Sessions are 256-bit random tokens in the `rmshare_session` cookie (`HttpOnly`, `SameSite=Lax`, `Secure` behind HTTPS). The proxy keeps their hashes with the role, the invitation, the client IP and the last activity in `state.json` (written atomically, mode `600`) and reloads it at start, so a restart does not log anyone out. Revoking an invitation deletes every session opened through it. The forms on `/invites` carry a per-session CSRF token and must come from the same origin.

Lockout: 10 failures from one client IP within 15 minutes block that IP; 100 failures from all IPs together block everyone. The IP comes from `CF-Connecting-IP` / `X-Forwarded-For`, which only the tunnel sets reliably.

The page learns its role from `/state` (`role`, `canDraw`). Guests do not see the *Draw* button, and the owner gets an *Invite* button (`client_roles.patch`). A `401` from `/state` reloads the page, which lands on the login form once a session has ended.

## Sleep detection

A Paper Pro connected to USB never really suspends, because the charger holds a wake lock. After the screen goes to sleep, processes and Wi-Fi keep running. To stop anyone from watching a "sleeping" tablet, the proxy follows xochitl's journal (`journalctl -u xochitl -f`) for this line:

```
Changing display state from Normal to DeepSleep      (and back: ... from DeepSleep to Normal)
```

- When the tablet falls asleep, the proxy cancels all active stream and pen requests. Later requests to the streaming paths get **503**.
- The page (`client_sleep.patch`) covers the last frame with an overlay saying *"The tablet is asleep"* and polls every 5 s. The picture comes back on its own after wake-up.
- When the connection is lost (the tablet suspended on battery, or no Wi-Fi), the overlay says *"Cannot reach the tablet"* and the page retries forever, instead of giving up after a few attempts as upstream does.

You can disable this with `-sleep-watch=false`.

## Auto-rotate

The framebuffer is always native portrait, and xochitl rotates its UI inside it. The proxy reads lines like `Setting new orientation Landscape` from the same journal (also `Portrait`, `InvertedLandscape` and `InvertedPortrait`). It exposes the current orientation on `/state`.

The page (`client_orientation.patch`) long-polls `/state?wait=<version>`. A request waits up to 25 s and returns as soon as something changes. A rotation therefore shows up within a fraction of a second, while an idle page costs only one small request every 25 s.

- The orientation-to-view mapping is the `ORIENTATION_VIEWS` table in `client_orientation.patch`. On the Paper Pro, `Landscape` is a 90° turn plus a flip, and `InvertedLandscape` is a 90° turn only.
- A new rotation is applied together with the first frame in which the picture actually changed (`stream_idle_refresh.patch`), with a 2 s fallback. The old picture is therefore never shown rotated.
- xochitl rotates the screen by itself when its sensor detects a tilt, even while you write with the tablet on your lap. The preview reproduces that faithfully. If it bothers you, lock rotation on the tablet.

## Refreshing without pen input

Upstream goMarkableStream sends frames only while you write or touch the screen, and pauses 2 s after the last input. Changes that xochitl makes on its own therefore reached viewers only at the next pen stroke. Examples are a document finishing loading, a rotation, or a page transition.

`stream_idle_refresh.patch` keeps checking the screen every 0.5 s while idle. An unchanged frame costs a hash and an empty delta of a few bytes. The cost is about 15–20 % of one of the four CPU cores while someone is watching and nobody writes. Without an open preview, the stream does not run at all.

## Remote pen

The **Draw** button (`D`) turns the preview into a drawing surface. Pointer positions are converted to the tablet's native coordinates, taking the view rotation into account. They are sent over the `/pen` WebSocket as text frames:

```
d <x> <y>    pen down   (x, y normalised to 0..1, native portrait)
m <x> <y>    move
u            pen up
```

The proxy injects them as evdev events (`BTN_TOOL_PEN`, `BTN_TOUCH`, `ABS_X/Y`, `ABS_PRESSURE`, `ABS_DISTANCE`) into `/dev/input/event2`. xochitl reads that device directly, so it draws with **whatever tool is selected on the tablet**.

- Pressure is fixed at 50 % (`penPressureFraction` in `proxy/pen.go`). Axis ranges are read with `EVIOCGABS`, and Paper Pro values are used as a fallback.
- Only one session can draw at a time. A dropped connection, or 1.5 s with the pen down and no movement, always lifts the pen.
- Events are dropped while the tablet sleeps.
- The WebSocket accepts only same-origin connections (`Origin` check) and requires a session.
- You can disable it with `-pen-device=`.
- Diagnostics: [`tools/pentool`](../tools/pentool/main.go) has `info` (axis ranges), `record` (dump real pen events) and `line` (inject a test stroke). Build it with `GOOS=linux GOARCH=arm64 go build` and copy it to the tablet.

## "Someone is watching" signal

xochitl has no local notification API (its notification bar is fed only by the reMarkable cloud), and e-ink will not refresh pixels drawn from outside. The proxy therefore signals viewers with the **front light**, and always restores your brightness level:

- **Two short blinks:** the first viewer has connected. This fires once the stream has been open for 2 s, so an accidental page reload does not blink.
- **One long blink:** the last viewer has left. This fires after 15 s without viewers, so brief connection drops do not blink.

With a bright light, a blink dims it; with a dim or off light, a blink brightens it. Nothing blinks while the tablet sleeps. The number of active viewers is the `viewers` field of `/state`. You can disable the signal with `-notify-light=`.

## URL document on the tablet

When the URL in `tunnel-url.txt` changes and `-doc` is on (`PUBLISH_DOC=1`), the proxy generates a one-page PDF with the URL and a QR code. It does this without a PDF library, using Helvetica text and a Flate-compressed greyscale image. It writes the PDF to the xochitl library as **"reMarkable Live – URL"** (pinned).

The document UUID is stored in `~/.local/share/remarkable/rmshare-doc-id`, so each new URL overwrites the same document. The last published URL is stored in `tunnel-url.txt.published`, so a proxy restart does not republish it.

xochitl only notices new documents on startup, so the proxy restarts `xochitl`. After 8 s it also restarts `rmshare-stream`, because goMarkableStream locates the framebuffer at startup.

## Multiple viewers

Upstream goMarkableStream serves **one viewer at a time**: the second gets a 429, and its client shows "Rate limited" for good. With a local tab and a tunnel tab open, the two kept stealing the stream from each other every 1–2 s.

`stream_preempt.patch` gives every connection its own delta encoder and admits up to **3 concurrent viewers**. A fourth viewer preempts the existing streams instead of being rejected. That matters because a tab that vanished behind a tunnel may not be noticed as disconnected for a long time.

Each admitted viewer gets its own framebuffer reads and delta encoding on the tablet's CPU, and its own upload over the tablet's Wi-Fi. That is why the limit is deliberately small: this is not meant for broadcasting to many viewers (see [Intended use and limitations](../README.md#intended-use-and-limitations)).

## goMarkableStream patches

The patches live in [`gms/`](../gms/) and are applied by [`gms/build-gms.sh`](../gms/build-gms.sh) in the order listed in [`gms/series`](../gms/series). They target goMarkableStream **v1.4.0**. All of them are required: without them you get plain upstream behaviour, which does not work reliably on the Paper Pro.

| Patch | Changes |
| --- | --- |
| `pointer_arm64.patch` | **Paper Pro framebuffer detection.** Upstream finds the framebuffer by walking from the last `/dev/dri/card0` mapping. That works only after a cold boot; after a `xochitl` restart it got stuck in an endless loop. The patch scans xochitl's anonymous mappings for an mmapped glibc malloc chunk of frame size whose content looks like an opaque BGRA image. The old method remains as a fallback, now guarded against looping. |
| `stream_preempt.patch` | Per-connection delta encoder; up to 3 concurrent viewers; the newest viewer preempts instead of getting a 429. |
| `client_sleep.patch` | "The tablet is asleep" / "Cannot reach the tablet" overlay, handling of the proxy's 503, and endless reconnect with backoff capped at 10 s. |
| `client_orientation.patch` | **Auto-rotate** button and shortcut, plus the `/state` long-poll. |
| `stream_idle_refresh.patch` | Keeps refreshing at 0.5 s without pen input, and applies a pending rotation with the first changed frame. |
| `client_pen.patch` | **Draw** button and shortcut, pointer to native coordinate mapping, and the `/pen` WebSocket client. |
| `client_roles.patch` | Owner/guest roles from `/state`: hides **Draw** for guests, adds the **Invite** button for the owner, reloads the page when the session ends. |

To update the patches for a new upstream version, see [CONTRIBUTING.md](../CONTRIBUTING.md#updating-the-gomarkablestream-patches).

## Resource usage

On the device, goMarkableStream uses roughly 20–40 MB of RAM and cloudflared 30–50 MB. The proxy is negligible.
