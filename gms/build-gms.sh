#!/bin/sh
# Builds goMarkableStream (version GMS_VERSION) from source with this project's
# patches applied in the order listed in gms/series:
#   pointer_arm64.patch        framebuffer detection on the reMarkable Paper Pro
#   stream_preempt.patch       several concurrent viewers
#   client_sleep.patch         "tablet asleep" / "cannot reach" overlay, endless reconnect
#   client_orientation.patch   auto-rotate the preview with the tablet
#   stream_idle_refresh.patch  keep refreshing the picture without pen input
#   client_pen.patch           draw on the tablet from the browser (remote pen)
#   client_roles.patch         owner/guest roles: hide Draw for guests, Invite button
#
#   gms/build-gms.sh <arm|arm64> <output-file>
set -eu
GOARCH=$1
OUT=$2
case "$OUT" in /*) ;; *) OUT=$(pwd)/$OUT ;; esac
GMS_VERSION=${GMS_VERSION:-v1.4.0}
GO=${GO:-go}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
SRC=$ROOT/dist/gms-src-$GMS_VERSION

if [ ! -f "$SRC/.patched" ]; then
  rm -rf "$SRC" && mkdir -p "$SRC"
  echo "    downloading goMarkableStream $GMS_VERSION source"
  curl -fsSL --retry 3 "https://github.com/owulveryck/goMarkableStream/archive/refs/tags/$GMS_VERSION.tar.gz" \
    | tar xz -C "$SRC" --strip-components=1
  # Order from gms/series (the client patches build on top of each other).
  while read -r name; do
    case "$name" in ''|'#'*) continue ;; esac
    echo "    applying patch $name"
    ( cd "$SRC" && patch -p1 --no-backup-if-mismatch < "$ROOT/gms/$name" )
  done < "$ROOT/gms/series"
  touch "$SRC/.patched"
fi

echo "    go build (GOARCH=$GOARCH)"
( cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH GOARM=7 "$GO" build -trimpath -ldflags="-s -w" -o "$OUT" . )
