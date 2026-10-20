#!/bin/sh
# Uruchamiane NA reMarkable (jako root), z katalogu /home/root/rmshare.
# Instaluje i wlacza trzy uslugi systemd: stream, proxy, tunnel.
set -e
DIR=/home/root/rmshare
cd "$DIR"

for f in gomarkablestream rmshare cloudflared run-tunnel.sh; do
  [ -f "$f" ] || { echo "brak pliku $DIR/$f"; exit 1; }
  chmod +x "$f"
done

for u in rmshare-stream rmshare-proxy rmshare-tunnel; do
  cp "$u.service" /etc/systemd/system/
done
systemctl daemon-reload
for u in rmshare-stream rmshare-proxy rmshare-tunnel; do
  systemctl enable "$u.service" >/dev/null 2>&1
  systemctl restart "$u.service"
done

sleep 3
echo
for u in rmshare-stream rmshare-proxy rmshare-tunnel; do
  printf "%-16s %s\n" "$u" "$(systemctl is-active $u.service)"
done
echo
if grep -q "^TUNNEL=tailscale" tunnel.env 2>/dev/null; then
  echo "Tailscale Funnel: czekam na stan (max 60 s)..."
  i=0
  while [ $i -lt 60 ] && [ ! -s tunnel-url.txt ] && [ ! -s tailscale-login-url.txt ] && [ ! -s tailscale-funnel-url.txt ]; do sleep 2; i=$((i+2)); done
  [ -s tailscale-login-url.txt ] && echo "ZALOGUJ URZADZENIE W TAILSCALE: $(cat tailscale-login-url.txt)"
  [ -s tailscale-funnel-url.txt ] && echo "WLACZ FUNNEL W TAILSCALE: $(cat tailscale-funnel-url.txt)"
  [ -s tunnel-url.txt ] && echo "ADRES: $(cat tunnel-url.txt)"
elif [ -f tunnel.env ] || [ -f cloudflared/config.yml ]; then
  echo "Tunel nazwany: adres to Twoj hostname skonfigurowany w Cloudflare."
else
  echo "Quick tunnel: czekam na adres (max 30 s)..."
  i=0
  while [ $i -lt 30 ] && [ ! -s tunnel-url.txt ]; do sleep 1; i=$((i+1)); done
  if [ -s tunnel-url.txt ]; then echo "ADRES: $(cat tunnel-url.txt)"; else echo "Brak adresu - sprawdz: journalctl -u rmshare-tunnel -n 50"; fi
fi
echo
echo "Lokalnie (USB/Wi-Fi): http://$(ip -4 addr show 2>/dev/null | grep -o 'inet 10\.11\.99\.[0-9]*' | head -n 1 | cut -d' ' -f2):8080"
echo "Logi: journalctl -u rmshare-stream -u rmshare-proxy -u rmshare-tunnel -f"
