#!/bin/sh
# Buduje goMarkableStream ze zrodel (wersja GMS_VERSION) z nasza latka na
# wykrywanie bufora ekranu w reMarkable Paper Pro (pointer_arm64.patch) i
# wielu widzow naraz (stream_preempt.patch), usypianie i auto-obrot w kliencie
# (client_sleep.patch, client_orientation.patch). Kolejnosc: gms/series.
#
#   gms/build-gms.sh <arm|arm64> <plik-wyjsciowy>
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
  echo "    pobieram zrodla goMarkableStream $GMS_VERSION"
  curl -fsSL --retry 3 "https://github.com/owulveryck/goMarkableStream/archive/refs/tags/$GMS_VERSION.tar.gz" \
    | tar xz -C "$SRC" --strip-components=1
  # Kolejnosc z gms/series (latki klienta naklada sie jedna na drugiej).
  while read -r name; do
    case "$name" in ''|'#'*) continue ;; esac
    echo "    nakladam latke $name"
    ( cd "$SRC" && patch -p1 --no-backup-if-mismatch < "$ROOT/gms/$name" )
  done < "$ROOT/gms/series"
  touch "$SRC/.patched"
fi

echo "    go build (GOARCH=$GOARCH, bez tailscale)"
( cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH GOARM=7 "$GO" build -trimpath -ldflags="-s -w" -o "$OUT" . )
