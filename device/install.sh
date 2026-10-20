#!/bin/sh
# Runs ON the reMarkable (as root), from /home/root/rmshare.
# Installs and enables three systemd services: stream, proxy, tunnel.
# Run it again after a reMarkable OS update (the files in /home survive it).
set -e
DIR=/home/root/rmshare
cd "$DIR"
. ./rootfs.sh

for f in gomarkablestream rmshare cloudflared run-tunnel.sh; do
  [ -f "$f" ] || { echo "missing file $DIR/$f"; exit 1; }
  chmod +x "$f"
done
[ -f tunnel.env ] && chmod 600 tunnel.env

# Make the services start on boot even where /etc is a RAM-backed overlay.
if etc_is_volatile; then
  if with_rootfs_etc persist_units; then
    echo "Services installed on the root partition (start on boot until the next OS update)."
  else
    echo "WARNING: could not write to the root partition; the services will NOT start after a reboot."
  fi
fi

# Install into the live /etc as well, so they are active right away.
for u in $UNITS; do
  cp "$u.service" /etc/systemd/system/
done
systemctl daemon-reload
for u in $UNITS; do
  systemctl enable "$u.service" >/dev/null 2>&1
  systemctl restart "$u.service"
done

sleep 3
echo
for u in $UNITS; do
  printf "%-16s %s\n" "$u" "$(systemctl is-active $u.service)"
done
echo
if grep -q "^TUNNEL=tailscale" tunnel.env 2>/dev/null; then
  echo "Tailscale Funnel: waiting for status (up to 60 s)..."
  i=0
  while [ $i -lt 60 ] && [ ! -s tunnel-url.txt ] && [ ! -s tailscale-login-url.txt ] && [ ! -s tailscale-funnel-url.txt ]; do sleep 2; i=$((i+2)); done
  [ -s tailscale-login-url.txt ] && echo "LOG THE DEVICE INTO TAILSCALE: $(cat tailscale-login-url.txt)"
  [ -s tailscale-funnel-url.txt ] && echo "ENABLE FUNNEL IN TAILSCALE: $(cat tailscale-funnel-url.txt)"
  [ -s tunnel-url.txt ] && echo "URL: $(cat tunnel-url.txt)"
elif grep -q "^TUNNEL_TOKEN=" tunnel.env 2>/dev/null || [ -f cloudflared/config.yml ]; then
  echo "Named tunnel: the URL is the hostname you configured in Cloudflare."
else
  echo "Quick tunnel: waiting for the URL (up to 30 s)..."
  i=0
  while [ $i -lt 30 ] && [ ! -s tunnel-url.txt ]; do sleep 1; i=$((i+1)); done
  if [ -s tunnel-url.txt ]; then echo "URL: $(cat tunnel-url.txt)"; else echo "No URL yet - check: journalctl -u rmshare-tunnel -n 50"; fi
fi
echo
echo "Local (USB/Wi-Fi): http://$(ip -4 addr show 2>/dev/null | grep -o 'inet 10\.11\.99\.[0-9]*' | head -n 1 | cut -d' ' -f2):8080"
echo "Logs: journalctl -u rmshare-stream -u rmshare-proxy -u rmshare-tunnel -f"
