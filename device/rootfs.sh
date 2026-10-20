#!/bin/sh
# Helpers for installing systemd units so they survive a reboot.
# Sourced by install.sh and uninstall.sh; runs ON the reMarkable.
#
# On the reMarkable Paper Pro, /etc is an overlay whose writable layer lives in
# RAM (/var/volatile/etc), so units copied to /etc/systemd/system are gone after
# a reboot. To persist they have to be written to the read-only root partition
# underneath the overlay. They are still lost on an OS update, which switches to
# the other root partition: run install.sh again afterwards.

UNITS="rmshare-stream rmshare-proxy rmshare-tunnel"

# etc_is_volatile succeeds when /etc is an overlay (writes there do not survive a reboot).
etc_is_volatile() {
  awk '$2 == "/etc" && $3 == "overlay" { found = 1 } END { exit !found }' /proc/mounts
}

# with_rootfs_etc CMD... runs CMD with $ROOTFS_ETC pointing at the root
# partition's own /etc, remounting / read-write only for the duration.
with_rootfs_etc() {
  mnt=$(mktemp -d)
  if ! mount -o remount,rw /; then
    rmdir "$mnt"
    return 1
  fi
  rc=1
  # A non-recursive bind mount of / shows the root partition without the /etc overlay.
  if mount -o bind / "$mnt"; then
    ROOTFS_ETC=$mnt/etc "$@"
    rc=$?
    umount "$mnt"
  fi
  rmdir "$mnt"
  mount -o remount,ro / || echo "warning: could not remount / read-only"
  return $rc
}

# persist_units copies the units into the root partition's /etc and enables them there.
persist_units() {
  for u in $UNITS; do
    cp "$u.service" "$ROOTFS_ETC/systemd/system/" || return 1
    ln -sf "/etc/systemd/system/$u.service" "$ROOTFS_ETC/systemd/system/multi-user.target.wants/$u.service" || return 1
  done
}

# unpersist_units removes the units from the root partition's /etc.
unpersist_units() {
  for u in $UNITS; do
    rm -f "$ROOTFS_ETC/systemd/system/$u.service" "$ROOTFS_ETC/systemd/system/multi-user.target.wants/$u.service"
  done
}
