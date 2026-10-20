#!/bin/sh
# Runs ON the reMarkable. Stops and removes the services; leaves the files in /home/root/rmshare.
for u in rmshare-tunnel rmshare-proxy rmshare-stream; do
  systemctl disable --now "$u.service" 2>/dev/null
  rm -f "/etc/systemd/system/$u.service"
done
systemctl daemon-reload
echo "Services removed. To delete the files as well: rm -rf /home/root/rmshare"
