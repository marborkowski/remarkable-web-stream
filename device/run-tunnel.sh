#!/bin/sh
# Starts the tunnel in one of these modes (checked in this order):
#  0. Tailscale Funnel             -> tunnel.env contains TUNNEL=tailscale (optionally TS_AUTHKEY,
#                                     TS_HOSTNAME); stable URL https://<host>.<tailnet>.ts.net
#  1. named tunnel with a token    -> /home/root/rmshare/tunnel.env contains TUNNEL_TOKEN=...
#  2. named tunnel with config.yml -> /home/root/rmshare/cloudflared/config.yml + credentials
#  3. quick tunnel (no account)    -> random *.trycloudflare.com URL, written to tunnel-url.txt
# In modes 1-2 you can set PUBLIC_URL=https://rm.example.com in tunnel.env so the URL
# also ends up in tunnel-url.txt (the /url page and the document on the tablet).
#
# Watchdog: after a long network outage (e.g. the tablet was suspended) cloudflared
# may keep trying to register a tunnel Cloudflare has already deleted ("Tunnel not found")
# and never create a new one. This script detects that (log + /ready endpoint) and exits
# with an error; systemd (Restart=always) then starts a fresh tunnel. The old URL is removed.
DIR=/home/root/rmshare
CF=$DIR/cloudflared
URLFILE=$DIR/tunnel-url.txt
ORIGIN=http://127.0.0.1:8080
METRICS=127.0.0.1:20241

[ -f "$DIR/tunnel.env" ] && . "$DIR/tunnel.env"
DEAD_AFTER=${TUNNEL_DEAD_AFTER:-120}   # seconds of continuous /ready=503 while the internet is reachable

# cloudflared needs trusted CA certificates; point it at the system bundle if present.
for f in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem; do
  [ -f "$f" ] && export SSL_CERT_FILE="$f" && break
done

write_public_url() {
  if [ -n "$PUBLIC_URL" ]; then echo "$PUBLIC_URL" > "$URLFILE"; else rm -f "$URLFILE"; fi
}

die_and_restart() {
  echo "watchdog: $1 - restarting the tunnel"
  [ "$MODE" = quick ] && rm -f "$URLFILE"
  kill $(pidof cloudflared) $(pidof tailscaled) 2>/dev/null
  sleep 1
  kill -9 $(pidof cloudflared) $(pidof tailscaled) 2>/dev/null
  exit 1
}

# Log filter: passes lines through to the journal, extracts the quick tunnel URL,
# and kills cloudflared on "Tunnel not found" (the watchdog below then exits).
log_filter() {
  while IFS= read -r line; do
    echo "$line"
    case "$line" in
      *https://*.trycloudflare.com*)
        url=$(echo "$line" | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com')
        [ -n "$url" ] && echo "$url" > "$URLFILE" && echo "URL: $url"
        ;;
      *"Tunnel not found"*)
        echo "watchdog: Cloudflare no longer knows this tunnel"
        kill $(pidof cloudflared) 2>/dev/null
        ;;
    esac
  done
}

# ---------------------------------------------------------------------------
# Tailscale Funnel
# ---------------------------------------------------------------------------
# tailscaled/tailscale logs go to the journal with a prefix.
prefix_log() {
  while IFS= read -r line; do echo "$1 $line"; done
}

run_tailscale() {
  TSDIR=$DIR/tailscale-state   # (not "tailscale": that is the CLI binary name)
  mkdir -p "$TSDIR"
  SOCK=$TSDIR/tailscaled.sock
  TS="$DIR/tailscale --socket=$SOCK"
  HOST=${TS_HOSTNAME:-remarkable}
  LOGIN_FILE=$DIR/tailscale-login-url.txt
  FUNNEL_FILE=$DIR/tailscale-funnel-url.txt
  echo "mode: Tailscale Funnel (hostname $HOST)"
  rm -f "$LOGIN_FILE" "$FUNNEL_FILE"
  # The Funnel URL is stable, so keep it across restarts; drop a URL left by another mode.
  grep -q "\.ts\.net" "$URLFILE" 2>/dev/null || rm -f "$URLFILE"

  # No /dev/net/tun on the reMarkable -> userspace networking (Funnel does not need a TUN device).
  # --statedir: where tailscaled keeps e.g. the Funnel HTTPS certificates (without it: "no TailscaleVarRoot").
  ( "$DIR/tailscaled" --state="$TSDIR/tailscaled.state" --statedir="$TSDIR" --socket="$SOCK" \
      --tun=userspace-networking --port=41641 2>&1 | prefix_log "tailscaled:" ) &
  TSD=$!
  i=0; while [ ! -S "$SOCK" ] && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done
  [ -S "$SOCK" ] || die_and_restart "tailscaled did not start"

  # Login: 'tailscale up' with an auth key logs in at once; without one it waits for approval
  # in a browser. Run it only once (every new 'up' invalidates the previous login link!),
  # and read the login link from 'tailscale status' into tailscale-login-url.txt.
  up_args="--hostname=$HOST --accept-dns=false --accept-routes=false"
  UP_PID=""
  start_up() {
    if [ -n "$TS_AUTHKEY" ]; then
      ( $TS up $up_args --authkey="$TS_AUTHKEY" 2>&1 | prefix_log "tailscale up:" ) &
    else
      ( $TS up $up_args 2>&1 | prefix_log "tailscale up:" ) &
    fi
    UP_PID=$!
  }
  start_up

  funnel_ok=0
  while kill -0 "$TSD" 2>/dev/null; do
    state=$($TS status --json 2>/dev/null | grep -o '"BackendState": *"[^"]*"' | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')
    if [ "$state" = "Running" ]; then
      rm -f "$LOGIN_FILE"
      if [ $funnel_ok -eq 0 ]; then
        # Funnel: public HTTPS 443 -> proxy. The first time, Funnel must be enabled in the Tailscale
        # admin console (link in the error message), as must HTTPS certificates (DNS -> HTTPS Certificates).
        # 'tailscale funnel' waits interactively until Funnel is enabled in the tailnet -> timeout
        out=$(timeout 20 $TS funnel --bg --https=443 http://127.0.0.1:8080 2>&1)
        if echo "$out" | grep -q "Available on the internet\|Funnel on"; then
          funnel_ok=1
          rm -f "$FUNNEL_FILE"
          dns=$($TS status --json 2>/dev/null | grep -o '"DNSName": *"[^"]*"' | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/; s/\.$//')
          [ -n "$dns" ] && echo "https://$dns" > "$URLFILE" && echo "URL: https://$dns"
        else
          echo "tailscale funnel: $out" | head -n 8
          echo "$out" | grep -o 'https://login.tailscale.com/[A-Za-z0-9/._?=&-]*' | head -n 1 > "$FUNNEL_FILE"
          [ -s "$FUNNEL_FILE" ] && echo "ENABLE FUNNEL: $(cat "$FUNNEL_FILE")"
        fi
      fi
    elif [ "$state" = "NeedsLogin" ]; then
      funnel_ok=0
      url=$($TS status 2>&1 | grep -o 'https://login.tailscale.com/[A-Za-z0-9/._-]*' | head -n 1)
      if [ -n "$url" ] && [ "$url" != "$(cat "$LOGIN_FILE" 2>/dev/null)" ]; then
        echo "$url" > "$LOGIN_FILE"
        echo "LOG THE DEVICE IN: $url"
      fi
      # session expired / device removed from the tailnet: 'up' has exited -> start it again
      kill -0 "$UP_PID" 2>/dev/null || start_up
    fi
    sleep 10
  done
  die_and_restart "tailscaled exited"
}

if [ "$TUNNEL" = "tailscale" ]; then
  MODE=tailscale
  run_tailscale
  exit 1
fi

if [ -n "$TUNNEL_TOKEN" ]; then
  MODE=token; echo "mode: named tunnel (token)"; write_public_url
  set -- tunnel --no-autoupdate --metrics "$METRICS" run --token "$TUNNEL_TOKEN"
elif [ -f "$DIR/cloudflared/config.yml" ]; then
  MODE=config; echo "mode: named tunnel (config.yml)"; write_public_url
  set -- tunnel --no-autoupdate --metrics "$METRICS" --config "$DIR/cloudflared/config.yml" run
else
  MODE=quick; echo "mode: quick tunnel (random URL, written to $URLFILE)"; rm -f "$URLFILE"
  set -- tunnel --no-autoupdate --metrics "$METRICS" --url "$ORIGIN"
fi

( "$CF" "$@" 2>&1 | log_filter ) &
CHILD=$!

# Watchdog: checks cloudflared's /ready every 10 s. Not ready for DEAD_AFTER seconds
# while the internet is reachable means the tunnel is dead.
down=0
while kill -0 "$CHILD" 2>/dev/null; do
  sleep 10
  if wget -qO- --timeout=3 "http://$METRICS/ready" >/dev/null 2>&1; then
    down=0
  else
    if ping -c1 -W2 1.1.1.1 >/dev/null 2>&1; then
      down=$((down + 10))
      [ $down -ge "$DEAD_AFTER" ] && die_and_restart "tunnel not ready for ${down}s although the network works"
    else
      down=0   # no internet: not the tunnel's fault, keep waiting
    fi
  fi
done
wait "$CHILD"
die_and_restart "cloudflared exited"
