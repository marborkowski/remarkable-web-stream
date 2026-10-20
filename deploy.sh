#!/bin/sh
# Buduje proxy, pobiera goMarkableStream i cloudflared dla architektury Twojego
# reMarkable, kopiuje wszystko na urzadzenie i instaluje uslugi.
#
#   ./deploy.sh                 # domyslnie root@10.11.99.1 (USB)
#   ./deploy.sh root@192.168.1.50
#
# Wymaga: go, curl, patch, ssh z kluczem na urzadzeniu (ssh-copy-id root@10.11.99.1).
set -eu
HOST=${1:-root@10.11.99.1}
GMS_VERSION=${GMS_VERSION:-v1.4.0}
CF_VERSION=${CF_VERSION:-2026.10.1}
TS_VERSION=${TS_VERSION:-1.104.1}
GO=${GO:-$(command -v go || echo /usr/local/opt/go/bin/go)}
ROOT=$(cd "$(dirname "$0")" && pwd)
DIST=$ROOT/dist
REMOTE_DIR=/home/root/rmshare
mkdir -p "$DIST"

echo "==> Sprawdzam urzadzenie $HOST"
ARCH=$(ssh -o ConnectTimeout=8 "$HOST" uname -m)
case "$ARCH" in
  armv7l)  GOARCH=arm;   CF_ASSET=cloudflared-linux-armhf ;;
  aarch64) GOARCH=arm64; CF_ASSET=cloudflared-linux-arm64 ;;
  *) echo "nieznana architektura: $ARCH"; exit 1 ;;
esac
echo "    arch=$ARCH -> GOARCH=$GOARCH, $CF_ASSET"

echo "==> Buduje proxy (GOARCH=$GOARCH)"
( cd "$ROOT/proxy" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH GOARM=7 "$GO" build -trimpath -ldflags="-s -w" -o "$DIST/rmshare-$GOARCH" . )

fetch() { # url dest
  if [ -s "$2" ]; then echo "    (cache) $(basename "$2")"; return; fi
  echo "    pobieram $1"
  curl -fsSL --retry 3 -o "$2.part" "$1" && mv "$2.part" "$2"
}
echo "==> Buduje goMarkableStream $GMS_VERSION ze zrodel (z latka dla Paper Pro)"
GMS_BIN=$DIST/gomarkablestream-$GOARCH-$GMS_VERSION-patched
[ -s "$GMS_BIN" ] && echo "    (cache) $(basename "$GMS_BIN")" || GO="$GO" GMS_VERSION="$GMS_VERSION" "$ROOT/gms/build-gms.sh" "$GOARCH" "$GMS_BIN"

echo "==> Pobieram cloudflared $CF_VERSION"
CF_BIN=$DIST/$CF_ASSET-$CF_VERSION
fetch "https://github.com/cloudflare/cloudflared/releases/download/$CF_VERSION/$CF_ASSET" "$CF_BIN"

TS_MODE=0
if [ -f "$ROOT/tunnel.env" ] && grep -q "^TUNNEL=tailscale" "$ROOT/tunnel.env"; then
  TS_MODE=1
  echo "==> Pobieram Tailscale $TS_VERSION (tryb Funnel)"
  TS_TGZ=$DIST/tailscale_${TS_VERSION}_$GOARCH.tgz
  fetch "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_$GOARCH.tgz" "$TS_TGZ"
  mkdir -p "$DIST/tailscale-$GOARCH"
  tar xzf "$TS_TGZ" -C "$DIST/tailscale-$GOARCH" --strip-components=1 "tailscale_${TS_VERSION}_$GOARCH/tailscale" "tailscale_${TS_VERSION}_$GOARCH/tailscaled"
fi

echo "==> Pakuje i wysylam na $HOST:$REMOTE_DIR"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
cp "$DIST/rmshare-$GOARCH" "$STAGE/rmshare"
cp "$GMS_BIN" "$STAGE/gomarkablestream"
cp "$CF_BIN" "$STAGE/cloudflared"
cp "$ROOT"/device/*.service "$ROOT"/device/*.sh "$STAGE/"
[ -f "$ROOT/tunnel.env" ] && cp "$ROOT/tunnel.env" "$STAGE/tunnel.env"
[ $TS_MODE -eq 1 ] && cp "$DIST/tailscale-$GOARCH/tailscale" "$DIST/tailscale-$GOARCH/tailscaled" "$STAGE/" && chmod +x "$STAGE/tailscale" "$STAGE/tailscaled"
if [ -d "$ROOT/cloudflared" ]; then mkdir -p "$STAGE/cloudflared" && cp "$ROOT"/cloudflared/* "$STAGE/cloudflared/"; fi
chmod +x "$STAGE"/rmshare "$STAGE"/gomarkablestream "$STAGE"/cloudflared "$STAGE"/*.sh

# Zatrzymaj uslugi, zeby nadpisac dzialajace binarki, potem wgraj i zainstaluj.
ssh "$HOST" "systemctl stop rmshare-tunnel rmshare-proxy rmshare-stream 2>/dev/null; mkdir -p $REMOTE_DIR"
( cd "$STAGE" && COPYFILE_DISABLE=1 tar cf - . ) | ssh "$HOST" "tar xf - -C $REMOTE_DIR"

echo "==> Instaluje uslugi na urzadzeniu"
ssh "$HOST" "cd $REMOTE_DIR && sh ./install.sh"
