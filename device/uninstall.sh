#!/bin/sh
# Runs ON the reMarkable. Stops and removes the services; leaves the files in /home/root/rmshare.
cd /home/root/rmshare && . ./rootfs.sh
for u in rmshare-tunnel rmshare-proxy rmshare-stream; do
  systemctl disable --now "$u.service" 2>/dev/null
  rm -f "/etc/systemd/system/$u.service"
done
if etc_is_volatile; then
  with_rootfs_etc unpersist_units || echo "WARNING: could not remove the services from the root partition"
fi
systemctl daemon-reload
echo "Services removed. To delete the files as well: rm -rf /home/root/rmshare"
