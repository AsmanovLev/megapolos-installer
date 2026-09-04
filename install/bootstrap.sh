#!/usr/bin/env bash
# =============================================================================
# bootstrap.sh — однострочник установки megapolos из squashfs-бандла.
#
#   curl -fsSL http://<host>:8000/install/bootstrap.sh | sudo bash
#   sudo bash bootstrap.sh                              # из файла
#   sudo bash bootstrap.sh http://<host>:8000/bundle/megapolos-bundle.sqfs
#
# Качает megapolos-bundle.sqfs → монтирует (без распаковки) → запускает
# Go-установщик из смонтированного образа. Аргументы пробрасываются
# в installer (например: --no-tui --yes).
# =============================================================================
set -euo pipefail

HOST_IP="${MEGAPOLOS_HOST_IP:-10.0.2.2}"
SQFS_URL="${MEGAPOLOS_BUNDLE_URL:-http://$HOST_IP:8000/bundle/megapolos-bundle.sqfs}"
SQFS=/tmp/megapolos-bundle.sqfs
MNT=/mnt/megapolos-bundle

[ "$(id -u)" -eq 0 ] || { echo "Запусти от root: sudo bash bootstrap.sh" >&2; exit 1; }

if [ ! -f "$SQFS" ]; then
  echo "==> качаю $SQFS_URL"
  curl -fSL --retry 5 --retry-delay 5 -C - -o "$SQFS" "$SQFS_URL"
fi

mkdir -p "$MNT"
if ! mountpoint -q "$MNT"; then
  if ! mount -o ro,loop "$SQFS" "$MNT"; then
    echo "mount не удался (нет squashfs в ядре?) — распаковываю через unsquashfs" >&2
    command -v unsquashfs >/dev/null || { apt-get update && apt-get install -y squashfs-tools; }
    unsquashfs -d /opt/megapolos-bundle "$SQFS"
    exec /opt/megapolos-bundle/installer "$@"
  fi
fi

echo "==> бандл смонтирован в $MNT, запускаю installer"
exec "$MNT/installer" "$@"
