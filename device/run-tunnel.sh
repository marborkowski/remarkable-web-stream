#!/bin/sh
# Uruchamia tunel w jednym z trybow (sprawdzane w tej kolejnosci):
#  0. Tailscale Funnel             -> tunnel.env zawiera TUNNEL=tailscale (opcjonalnie TS_AUTHKEY,
#                                     TS_HOSTNAME); staly adres https://<host>.<tailnet>.ts.net
#  1. tunel nazwany z tokenem      -> /home/root/rmshare/tunnel.env zawiera TUNNEL_TOKEN=...
#  2. tunel nazwany z config.yml   -> /home/root/rmshare/cloudflared/config.yml + credentials
#  3. quick tunnel (bez konta)     -> losowy adres *.trycloudflare.com, zapisywany do tunnel-url.txt
# W trybach 1-2 mozesz podac w tunnel.env PUBLIC_URL=https://rm.twojadomena.pl, zeby adres
# tez trafil do tunnel-url.txt (strona /url i dokument na tablecie).
#
# Watchdog: cloudflared po dluzszym braku sieci (np. tablet w suspendzie) potrafi w kolo
# probowac rejestrowac tunel, ktory Cloudflare juz skasowal ("Tunnel not found"), i nigdy
# nie zalozyc nowego. Skrypt wykrywa to (log + endpoint /ready) i konczy sie z bledem,
# a systemd (Restart=always) uruchamia swiezy tunel. Stary adres jest wtedy kasowany.
DIR=/home/root/rmshare
CF=$DIR/cloudflared
URLFILE=$DIR/tunnel-url.txt
ORIGIN=http://127.0.0.1:8080
METRICS=127.0.0.1:20241
DEAD_AFTER=${TUNNEL_DEAD_AFTER:-120}   # sekund ciaglego /ready=503 przy dzialajacym internecie

[ -f "$DIR/tunnel.env" ] && . "$DIR/tunnel.env"

# cloudflared potrzebuje zaufanych certyfikatow CA; wskaz systemowy plik, jesli istnieje.
for f in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem; do
  [ -f "$f" ] && export SSL_CERT_FILE="$f" && break
done

write_public_url() {
  if [ -n "$PUBLIC_URL" ]; then echo "$PUBLIC_URL" > "$URLFILE"; else rm -f "$URLFILE"; fi
}

die_and_restart() {
  echo "watchdog: $1 - restartuje tunel"
  [ "$MODE" = quick ] && rm -f "$URLFILE"
  kill $(pidof cloudflared) $(pidof tailscaled) 2>/dev/null
  sleep 1
  kill -9 $(pidof cloudflared) $(pidof tailscaled) 2>/dev/null
  exit 1
}

# Filtr logu: przepuszcza linie do journala, wyciaga adres quick tunnelu,
# a na "Tunnel not found" ubija cloudflared (watchdog ponizej konczy skrypt).
log_filter() {
  while IFS= read -r line; do
    echo "$line"
    case "$line" in
      *https://*.trycloudflare.com*)
        url=$(echo "$line" | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com')
        [ -n "$url" ] && echo "$url" > "$URLFILE" && echo "ADRES: $url"
        ;;
      *"Tunnel not found"*)
        echo "watchdog: Cloudflare nie zna juz tego tunelu"
        kill $(pidof cloudflared) 2>/dev/null
        ;;
    esac
  done
}

# ---------------------------------------------------------------------------
# Tailscale Funnel
# ---------------------------------------------------------------------------
# Logi tailscaled/tailscale trafiaja do journala z prefiksem.
prefix_log() {
  while IFS= read -r line; do echo "$1 $line"; done
}

run_tailscale() {
  TSDIR=$DIR/tailscale-state   # (nie "tailscale": tak nazywa sie binarka CLI)
  mkdir -p "$TSDIR"
  SOCK=$TSDIR/tailscaled.sock
  TS="$DIR/tailscale --socket=$SOCK"
  HOST=${TS_HOSTNAME:-remarkable}
  LOGIN_FILE=$DIR/tailscale-login-url.txt
  FUNNEL_FILE=$DIR/tailscale-funnel-url.txt
  echo "tryb: Tailscale Funnel (hostname $HOST)"
  rm -f "$LOGIN_FILE" "$FUNNEL_FILE"
  # Adres Funnel jest staly, wiec zostaje miedzy restartami; adres z innego trybu kasujemy.
  grep -q "\.ts\.net" "$URLFILE" 2>/dev/null || rm -f "$URLFILE"

  # Brak /dev/net/tun na reMarkable -> siec w przestrzeni uzytkownika (Funnel tego nie potrzebuje).
  # --statedir: tu tailscaled trzyma m.in. certyfikaty HTTPS dla Funnel (bez tego "no TailscaleVarRoot").
  ( "$DIR/tailscaled" --state="$TSDIR/tailscaled.state" --statedir="$TSDIR" --socket="$SOCK" \
      --tun=userspace-networking --port=41641 2>&1 | prefix_log "tailscaled:" ) &
  TSD=$!
  i=0; while [ ! -S "$SOCK" ] && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done
  [ -S "$SOCK" ] || die_and_restart "tailscaled nie wystartowal"

  # Logowanie: 'tailscale up' z kluczem loguje od razu; bez klucza czeka na zatwierdzenie
  # w przegladarce. Uruchamiamy je tylko raz (kazde nowe 'up' uniewaznia poprzedni link!),
  # a link do zalogowania czytamy ze 'tailscale status' i zapisujemy do tailscale-login-url.txt.
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
        # Funnel: publiczny HTTPS 443 -> proxy. Pierwszy raz trzeba wlaczyc Funnel w konsoli
        # Tailscale (link w komunikacie bledu) i certyfikaty HTTPS (DNS -> HTTPS Certificates).
        # 'tailscale funnel' czeka interaktywnie, dopoki Funnel nie jest wlaczony w tailnecie -> limit czasu
        out=$(timeout 20 $TS funnel --bg --https=443 http://127.0.0.1:8080 2>&1)
        if echo "$out" | grep -q "Available on the internet\|Funnel on"; then
          funnel_ok=1
          rm -f "$FUNNEL_FILE"
          dns=$($TS status --json 2>/dev/null | grep -o '"DNSName": *"[^"]*"' | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/; s/\.$//')
          [ -n "$dns" ] && echo "https://$dns" > "$URLFILE" && echo "ADRES: https://$dns"
        else
          echo "tailscale funnel: $out" | head -n 8
          echo "$out" | grep -o 'https://login.tailscale.com/[A-Za-z0-9/._?=&-]*' | head -n 1 > "$FUNNEL_FILE"
          [ -s "$FUNNEL_FILE" ] && echo "WLACZ FUNNEL: $(cat "$FUNNEL_FILE")"
        fi
      fi
    elif [ "$state" = "NeedsLogin" ]; then
      funnel_ok=0
      url=$($TS status 2>&1 | grep -o 'https://login.tailscale.com/[A-Za-z0-9/._-]*' | head -n 1)
      if [ -n "$url" ] && [ "$url" != "$(cat "$LOGIN_FILE" 2>/dev/null)" ]; then
        echo "$url" > "$LOGIN_FILE"
        echo "ZALOGUJ URZADZENIE: $url"
      fi
      # sesja wygasla / urzadzenie usuniete z tailnetu: 'up' juz nie dziala -> uruchom ponownie
      kill -0 "$UP_PID" 2>/dev/null || start_up
    fi
    sleep 10
  done
  die_and_restart "tailscaled zakonczyl sie"
}

if [ "$TUNNEL" = "tailscale" ]; then
  MODE=tailscale
  run_tailscale
  exit 1
fi

if [ -n "$TUNNEL_TOKEN" ]; then
  MODE=token; echo "tryb: tunel nazwany (token)"; write_public_url
  set -- tunnel --no-autoupdate --metrics "$METRICS" run --token "$TUNNEL_TOKEN"
elif [ -f "$DIR/cloudflared/config.yml" ]; then
  MODE=config; echo "tryb: tunel nazwany (config.yml)"; write_public_url
  set -- tunnel --no-autoupdate --metrics "$METRICS" --config "$DIR/cloudflared/config.yml" run
else
  MODE=quick; echo "tryb: quick tunnel (adres losowy, zapisze go do $URLFILE)"; rm -f "$URLFILE"
  set -- tunnel --no-autoupdate --metrics "$METRICS" --url "$ORIGIN"
fi

( "$CF" "$@" 2>&1 | log_filter ) &
CHILD=$!

# Watchdog: co 10 s sprawdza /ready cloudflared. Brak gotowosci przez DEAD_AFTER sekund
# przy osiagalnym internecie oznacza martwy tunel.
down=0
while kill -0 "$CHILD" 2>/dev/null; do
  sleep 10
  if wget -qO- --timeout=3 "http://$METRICS/ready" >/dev/null 2>&1; then
    down=0
  else
    if ping -c1 -W2 1.1.1.1 >/dev/null 2>&1; then
      down=$((down + 10))
      [ $down -ge "$DEAD_AFTER" ] && die_and_restart "tunel niegotowy od ${down}s mimo dzialajacej sieci"
    else
      down=0   # brak internetu: to nie wina tunelu, czekaj
    fi
  fi
done
wait "$CHILD"
die_and_restart "cloudflared zakonczyl sie"
