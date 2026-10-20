#!/bin/sh
# Uruchamiane NA reMarkable. Zatrzymuje i usuwa uslugi; pliki w /home/root/rmshare zostawia.
for u in rmshare-tunnel rmshare-proxy rmshare-stream; do
  systemctl disable --now "$u.service" 2>/dev/null
  rm -f "/etc/systemd/system/$u.service"
done
systemctl daemon-reload
echo "Uslugi usuniete. Pliki: rm -rf /home/root/rmshare"
